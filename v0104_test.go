package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestThinkingBudgetRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setReplyTokens(0)
	defer setReplyTokens(0)
	var seen []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Max int `json:"max_completion_tokens"`
		}
		_ = json.Unmarshal(b, &req)
		seen = append(seen, req.Max)
		w.Header().Set("Content-Type", "application/json")
		if req.Max < 32768 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"hmm..."},"finish_reason":"length"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", "glm")
	c.NoRetry = true
	m, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil || m.Content != "answer" {
		t.Fatalf("got %q %v (sent %v)", m.Content, err, seen)
	}
	if len(seen) != 2 || seen[0] != 8192 || seen[1] != 32768 {
		t.Fatalf("budgets %v", seen)
	}
	// The bigger budget sticks for this client.
	seen = nil
	if _, err := c.Chat([]Message{{Role: "user", Content: "again"}}, nil); err != nil || len(seen) != 1 || seen[0] != 32768 {
		t.Fatalf("sticky %v %v", seen, err)
	}
}

func TestThinkingBudgetGivesUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setReplyTokens(65536)
	defer setReplyTokens(0)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", "glm")
	c.NoRetry = true
	if _, err := c.Chat([]Message{{Role: "user", Content: "hi"}}, nil); err == nil || n != 1 {
		t.Fatalf("err %v calls %d", err, n)
	}
}

func TestReplyBudgetSetting(t *testing.T) {
	t.Setenv("MUSE99_MAX_TOKENS", "")
	setReplyTokens(0)
	if maxTokens() != 8192 {
		t.Fatal(maxTokens())
	}
	setReplyTokens(20000)
	if maxTokens() != 20000 {
		t.Fatal(maxTokens())
	}
	setReplyTokens(5)
	if maxTokens() != 1024 {
		t.Fatal(maxTokens())
	}
	setReplyTokens(0)
	if biggerBudget(8192) != 32768 || biggerBudget(32768) != 65536 || biggerBudget(65536) != 0 {
		t.Fatal("biggerBudget")
	}
}
