package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBudgetRetryAndActivity(t *testing.T) {
	var seen []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Max int `json:"max_completion_tokens"`
		}
		_ = json.Unmarshal(b, &req)
		seen = append(seen, req.Max)
		if req.Max > 2048 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"No seller can serve this model at an affordable price right now."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK","reasoning_content":"thinking"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	c := NewClientForProvider(srv.URL, "k", "m", "custom")
	msg, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || msg.Content != "OK" || msg.Reasoning != "thinking" {
		t.Fatalf("got %+v %v", msg, err)
	}
	if c.MaxTokens != 2048 || len(seen) != 2 {
		t.Fatalf("budget not lowered: %v %d", seen, c.MaxTokens)
	}
	if _, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err != nil || seen[len(seen)-1] != 2048 {
		t.Fatalf("not sticky: %v", seen)
	}
	evs, _ := activity.since(0)
	found := false
	for _, e := range evs {
		if e.Kind == "retry" && strings.Contains(e.Title, "2048") {
			found = true
		}
	}
	if !found {
		t.Fatal("no retry event")
	}
	if msg.ReasoningContent != "thinking" {
		t.Fatal("reasoning_content should be kept on the message (v0.9.15)")
	}
}

func TestRedactActivity(t *testing.T) {
	s := redactActivity(`curl -H "Authorization: Bearer abcdefghijklmnop123" key=sk-abcdefghijklmnopqrstu`)
	if strings.Contains(s, "abcdefghijklmnop123") || strings.Contains(s, "sk-abcdefghijklmnopqrstu") {
		t.Fatal(s)
	}
}

func TestScrubLegacyName(t *testing.T) {
	d, err := OpenDB(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := appDB
	appDB = d
	defer func() { appDB = old }()
	_, _, _ = d.AddMemory(legacyDefaultName+" prefers Go", "preference", "")
	scrubLegacyName("Sahand")
	mems, _ := d.ListMemories()
	if len(mems) != 1 || mems[0].Text != "Sahand prefers Go" {
		t.Fatalf("got %+v", mems)
	}
	if !isLegacyName(strings.ToLower(legacyDefaultName)) {
		t.Fatal("case")
	}
}

func TestChatWaitsThroughOutage(t *testing.T) {
	old := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryDelays = old }()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= 2 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded, try again"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	c := NewClientForProvider(srv.URL, "k", "m", "custom")
	msg, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || msg.Content != "OK" || n != 3 {
		t.Fatalf("n=%d %v %+v", n, err, msg)
	}
	c2 := NewClientForProvider(srv.URL, "k", "m", "custom")
	n = 0
	c2.NoRetry = true
	if _, err := c2.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err == nil {
		t.Fatal("NoRetry should fail fast")
	}
}

func TestClipOldToolResults(t *testing.T) {
	var msgs []Message
	big := strings.Repeat("x", 5000)
	for i := 0; i < 12; i++ {
		msgs = append(msgs, Message{Role: "tool", Content: big, ToolCallID: "c"})
	}
	out := clipOldToolResults(msgs)
	if len(out[0].Content) >= 5000 || len(out[11].Content) != 5000 || len(msgs[0].Content) != 5000 {
		t.Fatal("clip wrong")
	}
}
