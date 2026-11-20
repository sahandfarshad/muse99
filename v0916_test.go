package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInterimTextsAreLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer ctl.begin()
	ctl.begin()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Let me check the DNS","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x.txt\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"All done"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", "m")
	client.NoRetry = true
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	msgs := []Message{{Role: "system", Content: "s"}}
	liveBegin("chat1")
	reply, _ := runTurnFallback(client, nil, runner, toolDefinitions(), &msgs, "hi")
	if reply != "All done" {
		t.Fatalf("reply %q", reply)
	}
	chat, texts := liveTexts()
	if chat != "chat1" || len(texts) != 1 || texts[0] != "Let me check the DNS" {
		t.Fatalf("live %q %v", chat, texts)
	}
	s := &webServer{}
	rec := httptest.NewRecorder()
	s.handleLive(rec, httptest.NewRequest("GET", "/api/chat/live?from=1", nil))
	if !strings.Contains(rec.Body.String(), `"total":1`) || strings.Contains(rec.Body.String(), "DNS") {
		t.Fatalf("from=1 should return nothing new: %s", rec.Body.String())
	}
}

func TestWebHTMLUniqueIDs(t *testing.T) {
	seen := map[string]bool{}
	rest := webHTML
	for {
		i := strings.Index(rest, ` id="`)
		if i < 0 {
			break
		}
		rest = rest[i+5:]
		j := strings.Index(rest, `"`)
		id := rest[:j]
		if seen[id] {
			t.Fatalf("duplicate id %q in the page", id)
		}
		seen[id] = true
	}
}
