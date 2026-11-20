package main

// Chat compaction (v0.9.8): when the history sent to the model grows
// past a size, the older part of the chat is summarised once and only
// the summary plus the latest messages are sent from then on. The full
// chat stays in the database and on screen; only what goes to the
// model gets shorter.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.etcd.io/bbolt"
)

const (
	defaultCompactTokens = 40000
	compactKeep          = 10 // latest messages always sent whole
	compactMarker        = "[Summary of the earlier part of this chat. The older messages were removed from what you see to save tokens; the user still sees them.]"
)

// compactAt is the size (estimated tokens) that triggers a summary.
// 0 means off. MUSE99_COMPACT_AT wins over the saved setting.
func compactAt() int {
	if v := strings.TrimSpace(os.Getenv("MUSE99_COMPACT_AT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	switch n := loadFileConfig().CompactTokens; {
	case n < 0:
		return 0
	case n == 0:
		return defaultCompactTokens
	default:
		return n
	}
}

// estimateTokens is a rough count (about 4 characters a token) of what
// a request would carry, after old tool outputs are clipped.
func estimateTokens(msgs []Message) int {
	n := 0
	for _, m := range clipOldToolResults(msgs) {
		n += len(m.Content) + len(m.Reasoning)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		n += 16
	}
	return n / 4
}

// summaryMessages are the two messages that stand in for the older part.
func summaryMessages(summary string) []Message {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	return []Message{
		{Role: "user", Content: compactMarker + "\n\n" + summary},
		{Role: "assistant", Content: "Understood. I'll continue from this summary."},
	}
}

func isSummaryMessage(m Message) bool {
	return m.Role == "user" && strings.HasPrefix(m.Content, compactMarker)
}

// loadMessagesAfter returns the stored messages of a chat whose seq is
// greater than after, in order (empty ones were never stored).
func (d *DB) loadMessagesAfter(chatID string, after int) ([]StoredMessage, error) {
	var out []StoredMessage
	err := d.bolt.View(func(tx *bbolt.Tx) error {
		prefix := []byte(chatID + "/")
		c := tx.Bucket(bucketMessages).Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var sm StoredMessage
			if json.Unmarshal(v, &sm) == nil && sm.Seq > after {
				out = append(out, sm)
			}
		}
		return nil
	})
	return out, err
}

// setChatSummary saves the summary and the last seq it covers.
func (d *DB) setChatSummary(chatID, summary string, upTo int) error {
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		cb := tx.Bucket(bucketChats)
		v := cb.Get([]byte(chatID))
		if v == nil {
			return fmt.Errorf("chat not found: %s", chatID)
		}
		var c Chat
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		c.Summary, c.SummaryUpTo = summary, upTo
		data, _ := json.Marshal(c)
		return cb.Put([]byte(chatID), data)
	})
}

// compactCut picks how many of msgs to fold into the summary: all but
// about the last compactKeep, moved back so the kept part starts at a
// user message (a tool result never loses the call it answers).
func compactCut(msgs []StoredMessage) int {
	cut := len(msgs) - compactKeep
	for cut > 0 && msgs[cut].Role != "user" {
		cut--
	}
	return cut
}

func storedText(sm StoredMessage, limit int) string {
	var b strings.Builder
	b.WriteString(truncate(sm.Content, limit))
	for _, tc := range sm.ToolCalls {
		fmt.Fprintf(&b, "\n[calls %s %s]", tc.Function.Name, truncate(tc.Function.Arguments, 400))
	}
	return b.String()
}

// buildSummaryPrompt asks for a summary that keeps what the next steps need.
func buildSummaryPrompt(prev string, part []StoredMessage) string {
	var b strings.Builder
	b.WriteString("Summarise this conversation between a user and an AI agent that runs tools (shell, files, remote servers), so the agent can continue the work without the old messages.\n")
	b.WriteString("Keep: the user's goals and requests, facts learned (servers, paths, ports, versions, settings, names), what was done and what worked, what failed and why, decisions, and what is still open or next.\n")
	b.WriteString("Drop: small talk, repeated attempts, long command output. Write short bullet points in the user's language. At most 400 words.\n\n")
	if strings.TrimSpace(prev) != "" {
		b.WriteString("Earlier summary:\n" + prev + "\n\n")
	}
	b.WriteString("Conversation:\n")
	budget := 120000
	for _, sm := range part {
		lim := 1500
		if sm.Role == "tool" {
			lim = 600
		}
		line := sm.Role + ": " + storedText(sm, lim) + "\n"
		if budget-len(line) < 0 {
			b.WriteString("…\n")
			break
		}
		budget -= len(line)
		b.WriteString(line)
	}
	return b.String()
}

// maybeCompactChat folds the older part of a chat into its summary when
// what would be sent is over the limit. It returns true when it did.
func maybeCompactChat(client *Client, d *DB, chatID string, current []Message) bool {
	limit := compactAt()
	if limit <= 0 || d == nil || client == nil || chatID == "" {
		return false
	}
	before := estimateTokens(current)
	if before < limit {
		return false
	}
	chat, err := d.GetChat(chatID)
	if err != nil {
		return false
	}
	msgs, err := d.loadMessagesAfter(chatID, chat.SummaryUpTo)
	if err != nil || len(msgs) <= compactKeep {
		return false
	}
	cut := compactCut(msgs)
	if cut <= 0 {
		return false
	}
	aid := activity.add("chat", "model", fmt.Sprintf("Chat is long (~%dk tokens): summarising %d older messages…", before/1000, cut), "Set in the Summary menu at the top of the chat.", true)
	resp, err := client.Chat([]Message{
		{Role: "system", Content: "You write compact working summaries of agent conversations."},
		{Role: "user", Content: buildSummaryPrompt(chat.Summary, msgs[:cut])},
	}, nil)
	summary := strings.TrimSpace(resp.Content)
	if err != nil || summary == "" {
		msg := "empty summary"
		if err != nil {
			msg = err.Error()
		}
		activity.finish(aid, false, "Summary failed, the full chat is sent as before", msg)
		return false
	}
	if err := d.setChatSummary(chatID, summary, msgs[cut-1].Seq); err != nil {
		activity.finish(aid, false, "Could not save the summary", err.Error())
		return false
	}
	activity.finish(aid, true, fmt.Sprintf("Summarised %d older messages", cut), summary)
	return true
}

// handleCompact serves GET/POST /api/compact {tokens} (0 = off).
func (s *webServer) handleCompact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": compactAt()})
		return
	}
	var body struct {
		Tokens int `json:"tokens"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Tokens < 0 || (body.Tokens > 0 && body.Tokens < 5000) || body.Tokens > 1000000 {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "0 (off) or 5000 to 1000000"})
		return
	}
	fc := loadFileConfig()
	fc.CompactTokens = body.Tokens
	if body.Tokens == 0 {
		fc.CompactTokens = -1
	}
	if err := saveFileConfig(fc); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tokens": body.Tokens})
}
