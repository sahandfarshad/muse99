package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func thinkHistory() []Message {
	tc := ToolCall{ID: "c1", Type: "function"}
	tc.Function.Name, tc.Function.Arguments = "shell", "{}"
	return []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer", ReasoningContent: "OLDTHOUGHT"},
		{Role: "user", Content: "new question"},
		{Role: "assistant", ToolCalls: []ToolCall{tc}, ReasoningContent: "NEWTHOUGHT"},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	}
}

func TestKeepThinkingModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := thinkHistory()
	for mode, want := range map[string][2]bool{"": {false, true}, "turn": {false, true}, "all": {true, true}, "off": {false, false}} {
		t.Setenv("MUSE99_KEEP_THINKING", mode)
		out := keepThinking(h, false)
		got := [2]bool{out[2].ReasoningContent != "", out[4].ReasoningContent != ""}
		if got != want {
			t.Fatalf("mode %q: got %v want %v", mode, got, want)
		}
	}
	if h[2].ReasoningContent == "" {
		t.Fatal("keepThinking changed the caller's history")
	}
	t.Setenv("MUSE99_KEEP_THINKING", "all")
	if hasReasoning(keepThinking(h, true)) {
		t.Fatal("never=true must strip all")
	}
}

func TestThinkingSentAndRejectRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MUSE99_KEEP_THINKING", "")
	var bodies []string
	reject := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if reject && strings.Contains(string(b), "reasoning_content") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field reasoning_content"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	// Accepting provider: only this turn's thought is sent.
	reject = false
	c := NewClientForProvider(srv.URL, "k", "m", "custom")
	c.NoRetry = true
	if _, err := c.Chat(thinkHistory(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[0], "NEWTHOUGHT") || strings.Contains(bodies[0], "OLDTHOUGHT") {
		t.Fatalf("turn mode body wrong: %s", bodies[0])
	}

	// Rejecting provider: one retry without thinking, then sticky off.
	reject = true
	bodies = nil
	c2 := NewClientForProvider(srv.URL, "k", "m", "custom")
	c2.NoRetry = true
	msg, err := c2.Chat(thinkHistory(), nil)
	if err != nil || msg.Content != "done" || len(bodies) != 2 || !c2.noReasoning {
		t.Fatalf("retry failed: %v %v %d %v", msg, err, len(bodies), c2.noReasoning)
	}
	bodies = nil
	if _, err := c2.Chat(thinkHistory(), nil); err != nil || len(bodies) != 1 || strings.Contains(bodies[0], "reasoning_content") {
		t.Fatalf("not sticky: %v %d", err, len(bodies))
	}
}

func TestReasoningStoredInJSON(t *testing.T) {
	b, _ := json.Marshal(Message{Role: "assistant", Content: "x", ReasoningContent: "T"})
	var m Message
	_ = json.Unmarshal(b, &m)
	if m.ReasoningContent != "T" {
		t.Fatal("reasoning_content not kept in stored JSON")
	}
}
