package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestStablePrefixMemoriesOnUserMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := appDB
	appDB = d
	defer func() { appDB = old }()
	_, _, _ = d.AddMemory("user likes x-ui panels on port 2053", "preference", "")
	_, _, _ = d.AddMemory("user server US1 runs ubuntu", "fact", "")

	var reqs [][]Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Messages []Message }
		_ = json.NewDecoder(r.Body).Decode(&req)
		reqs = append(reqs, req.Messages)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 5, "prompt_tokens_details": map[string]any{"cached_tokens": 64}},
		})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", "m")
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	cfg := Config{BaseURL: srv.URL, Model: "m", MemoryPath: t.TempDir() + "/MEMORY.md"}
	msgs := []Message{{Role: "system", Content: "startup"}}

	before := loadUsage()
	ctx1 := refreshSystemFor(&cfg, &msgs, "x-ui panel port")
	runTurnWithContext(client, nil, runner, nil, &msgs, "x-ui panel port", ctx1)
	ctx2 := refreshSystemFor(&cfg, &msgs, "x-ui panel port again")
	runTurnWithContext(client, nil, runner, nil, &msgs, "x-ui panel port again", ctx2)

	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	if strings.Contains(reqs[0][0].Content, "2053") || reqs[0][0].Content != reqs[1][0].Content {
		t.Fatal("system prompt must be stable and carry no memories")
	}
	if !strings.Contains(reqs[0][1].Content, "2053") || !strings.HasSuffix(reqs[0][1].Content, "x-ui panel port") {
		t.Fatalf("memory not sent with the user message: %q", reqs[0][1].Content)
	}
	// second request starts exactly like the first (cacheable prefix)
	for i := range reqs[0] {
		if reqs[1][i].Content != reqs[0][i].Content {
			t.Fatalf("prefix changed at %d", i)
		}
	}
	if strings.Contains(reqs[1][3].Content, "2053") {
		t.Fatal("memory sent twice in one chat")
	}
	if strings.Contains(msgs[1].Content, "2053") {
		t.Fatal("memory leaked into the stored message")
	}
	if d := diffUsage(before, loadUsage()); d.CachedTokens != 128 {
		t.Fatalf("cached tokens: %+v", d)
	}
}
