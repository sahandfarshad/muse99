package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactChatSummarisesOlderPart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MUSE99_COMPACT_AT", "5000")
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []Message }
		_ = json.NewDecoder(r.Body).Decode(&req)
		asked = req.Messages[len(req.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "- installed x-ui on US1, port 2053"}}},
		})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", "m")
	client.NoRetry = true

	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ch, _ := d.CreateChat("long")
	big := strings.Repeat("output line\n", 300)
	for i := 0; i < 20; i++ {
		_ = d.AddMessage(ch.ID, Message{Role: "user", Content: "step please"})
		_ = d.AddMessage(ch.ID, Message{Role: "assistant", Content: "working " + big})
	}
	s := &webServer{db: d, system: "sys", client: client}
	s.loadCurrentChat(ch.ID)
	if len(s.messages) != 41 {
		t.Fatalf("loaded %d", len(s.messages))
	}
	if !maybeCompactChat(client, d, ch.ID, s.messages) {
		t.Fatal("expected a summary")
	}
	if !strings.Contains(asked, "step please") {
		t.Fatal("summary prompt missed the chat")
	}
	got, _ := d.GetChat(ch.ID)
	if got.SummaryUpTo != 30 || !strings.Contains(got.Summary, "x-ui") {
		t.Fatalf("chat after compact: up_to=%d summary=%q", got.SummaryUpTo, got.Summary)
	}
	s.loadCurrentChat(ch.ID)
	// system + summary pair + last 10 messages, starting with a user message
	if len(s.messages) != 13 || !isSummaryMessage(s.messages[1]) || s.messages[3].Role != "user" {
		t.Fatalf("after reload: %d messages", len(s.messages))
	}
	if estimateTokens(s.messages) >= 5000 {
		t.Fatal("still too long")
	}
	// all messages are still stored for the screen
	if all, _ := d.LoadMessages(ch.ID, 1000); len(all) != 40 {
		t.Fatalf("stored %d", len(all))
	}
	// short chat: nothing to do
	if maybeCompactChat(client, d, ch.ID, s.messages) {
		t.Fatal("compacted a short chat")
	}
	_ = d.ClearChat(ch.ID)
	if c, _ := d.GetChat(ch.ID); c.Summary != "" || c.SummaryUpTo != 0 {
		t.Fatal("clear must drop the summary")
	}
}

func TestCompactCutKeepsToolPairs(t *testing.T) {
	var msgs []StoredMessage
	roles := []string{"user", "assistant", "tool", "assistant", "tool", "assistant"}
	for i := 0; i < 4; i++ {
		for _, r := range roles {
			msgs = append(msgs, StoredMessage{Seq: len(msgs) + 1, Role: r})
		}
	}
	cut := compactCut(msgs)
	if cut <= 0 || msgs[cut].Role != "user" || len(msgs)-cut < compactKeep {
		t.Fatalf("cut=%d", cut)
	}
}

func TestCompactSetting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if compactAt() != defaultCompactTokens {
		t.Fatal("default")
	}
	s := &webServer{}
	rec := httptest.NewRecorder()
	s.handleCompact(rec, httptest.NewRequest("POST", "/api/compact", strings.NewReader(`{"tokens":0}`)))
	if compactAt() != 0 {
		t.Fatal("off not saved: " + rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleCompact(rec, httptest.NewRequest("POST", "/api/compact", strings.NewReader(`{"tokens":80000}`)))
	if compactAt() != 80000 {
		t.Fatal(rec.Body.String())
	}
}
