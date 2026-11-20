package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// v0.9: the activity log behind the web UI's "Behind the scenes"
// panel. Every step of a chat turn or task is an event: the request
// to the model (pending while we wait), what came back (thinking,
// tool calls, text), each tool run with its result, retries, errors.
// It lives in memory only (last 400 events) and never holds API keys.

type ActivityEvent struct {
	ID      int64  `json:"id"`
	Seq     int64  `json:"seq"`
	Source  string `json:"source"` // chat | task <id>
	Kind    string `json:"kind"`   // user | model | think | tool | retry | error | done | info
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Start   int64  `json:"start"` // unix ms
	End     int64  `json:"end,omitempty"`
	Pending bool   `json:"pending"`
	OK      bool   `json:"ok"`
	Chat    string `json:"chat,omitempty"` // v0.9.6: chat id the event belongs to
	key     string // DB key once saved
}

type activityLog struct {
	mu     sync.Mutex
	events []*ActivityEvent
	nextID int64
	seq    int64
}

var activity = &activityLog{}

const activityMax = 400
const activityDetailMax = 4000

func nowMs() int64 { return time.Now().UnixMilli() }

func (a *activityLog) add(source, kind, title, detail string, pending bool) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.nextID == 0 {
		// Unique across restarts, so saved and live events never clash.
		a.nextID = nowMs() * 1000
	}
	a.nextID++
	a.seq++
	ev := &ActivityEvent{ID: a.nextID, Seq: a.seq, Source: source, Kind: kind, Title: title,
		Detail: redactActivity(truncate(detail, activityDetailMax)), Start: nowMs(), Pending: pending, OK: true}
	if !pending {
		ev.End = ev.Start
	}
	if source == "chat" || source == "" || source == "servers" {
		ev.Chat = currentActivityChat()
	}
	saveActivityEvent(ev)
	a.events = append(a.events, ev)
	if len(a.events) > activityMax {
		a.events = a.events[len(a.events)-activityMax:]
	}
	return ev.ID
}

// finish closes a pending event, optionally replacing its title.
func (a *activityLog) finish(id int64, ok bool, title, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.events) - 1; i >= 0; i-- {
		ev := a.events[i]
		if ev.ID != id {
			continue
		}
		a.seq++
		ev.Seq = a.seq
		ev.Pending = false
		ev.OK = ok
		ev.End = nowMs()
		if title != "" {
			ev.Title = title
		}
		if detail != "" {
			ev.Detail = redactActivity(truncate(detail, activityDetailMax))
		}
		saveActivityEvent(ev)
		return
	}
}

// since returns events changed after seq (new or finished).
func (a *activityLog) since(seq int64) ([]ActivityEvent, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ActivityEvent
	for _, ev := range a.events {
		if ev.Seq > seq {
			out = append(out, *ev)
		}
	}
	return out, a.seq
}

func (a *activityLog) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = nil
	a.seq++
}

// redactActivity hides anything that looks like a bearer token or
// API key in commands and results before they reach the browser.
func redactActivity(s string) string {
	for _, p := range []string{"Bearer ", "sk-", "sk_", "AIza", "ghp_", "github_pat_"} {
		for off := 0; ; {
			i := strings.Index(s[off:], p)
			if i < 0 {
				break
			}
			i += off
			j := i + len(p)
			for j < len(s) && !strings.ContainsRune(" \t\n\"'`,;)", rune(s[j])) {
				j++
			}
			if j-i-len(p) >= 12 {
				s = s[:i] + p + "•••" + s[j:]
				off = i + len(p) + 3
			} else {
				off = j
			}
		}
	}
	return s
}

// clearChat drops one chat's live events.
func (a *activityLog) clearChat(chat string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := a.events[:0]
	for _, ev := range a.events {
		if ev.Chat != chat {
			keep = append(keep, ev)
		}
	}
	a.events = keep
	a.seq++
}

// GET /api/activity?since=<seq>  -> {events, seq, now}
// GET /api/activity?chat=<id>    -> that chat's saved events
// DELETE /api/activity[?chat=id] -> clear the panel (one chat or all)
func (s *webServer) handleActivity(w http.ResponseWriter, r *http.Request) {
	chat := r.URL.Query().Get("chat")
	if r.Method == http.MethodDelete {
		if chat != "" {
			activity.clearChat(chat)
			if appDB != nil {
				_ = appDB.DeleteActivity(chat)
			}
		} else {
			activity.clear()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if chat != "" {
		var evs []ActivityEvent
		if appDB != nil {
			evs = appDB.LoadActivity(chat, activityPerChat)
		}
		if evs == nil {
			evs = []ActivityEvent{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": evs, "now": nowMs()})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	evs, seq := activity.since(since)
	if evs == nil {
		evs = []ActivityEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": evs, "seq": seq, "now": nowMs()})
}

// describeReply turns a model message into a short title + detail.
func describeReply(msg Message) (string, string) {
	var b strings.Builder
	if strings.TrimSpace(msg.Reasoning) != "" {
		b.WriteString("💭 " + strings.TrimSpace(msg.Reasoning) + "\n\n")
	}
	if strings.TrimSpace(msg.Content) != "" {
		b.WriteString(strings.TrimSpace(msg.Content))
	}
	if len(msg.ToolCalls) > 0 {
		names := make([]string, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		return "Model wants to run: " + strings.Join(names, ", "), b.String()
	}
	if strings.TrimSpace(msg.Content) == "" {
		return "Model replied with nothing", b.String()
	}
	return "Model wrote the answer", b.String()
}

// prettyToolArgs shows a tool's arguments readably (the command for
// shell tools, otherwise compact JSON).
func prettyToolArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	for _, k := range []string{"command", "cmd"} {
		if v, ok := m[k].(string); ok {
			rest := ""
			for kk, vv := range m {
				if kk != k {
					rest += "\n" + kk + ": " + truncate(strings.TrimSpace(toString(vv)), 300)
				}
			}
			_ = sort.Strings
			return "$ " + v + rest
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+truncate(toString(m[k]), 600))
	}
	return strings.Join(parts, "\n")
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
