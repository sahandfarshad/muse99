package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsSpark(t *testing.T) {
	if !isSpark("https://api.meta.ai/v1", "muse-spark-1.3") {
		t.Fatal("Meta API should be Spark")
	}
	if !isSpark("https://example.com/v1", "muse-spark-1.4") {
		t.Fatal("muse-spark model should be Spark")
	}
	if isSpark("https://api.openai.com/v1", "gpt-5") {
		t.Fatal("OpenAI GPT should not be Spark")
	}
}

func TestSystemPrompt(t *testing.T) {
	p := systemPrompt("Takahashi", "likes Go", "https://api.meta.ai/v1", "muse-spark-1.3")
	if !strings.Contains(p, "Muse") || !strings.Contains(p, "Takahashi") {
		t.Fatalf("prompt missing Muse/Takahashi: %s", p)
	}
	if !strings.Contains(p, "Muse Spark") {
		t.Fatal("Spark prompt should mention Muse Spark")
	}
	generic := systemPrompt("Takahashi", "", "https://api.openai.com/v1", "gpt-5")
	if strings.Contains(generic, "powered by Muse Spark") {
		t.Fatal("generic prompt should not claim Spark")
	}
}

func TestSafePathAndTruncate(t *testing.T) {
	if got := safePath("/tmp/work", "a/b.txt"); got != "/tmp/work/a/b.txt" {
		t.Fatalf("safePath = %q", got)
	}
	if got := truncate("hello world", 5); !strings.HasPrefix(got, "hello") {
		t.Fatalf("truncate = %q", got)
	}
	if len(toolDefinitions()) < 5 {
		t.Fatal("expected at least 5 tools")
	}
}

// Mock OpenAI-compatible server: first turn asks to read a file,
// second turn (after tool result) gives a final answer.
// This is what CI runs — no real API key needed.
func TestAgentLoopWithMockServer(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var resp map[string]any
		if calls == 1 {
			resp = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"README.md"}`},
				}},
			}}}}
		} else {
			resp = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "Mock reply: read it fine.",
			}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test", "muse-spark-1.3")
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	messages := []Message{{Role: "system", Content: systemPrompt("Takahashi", "", srv.URL, "muse-spark-1.3")}}
	reply := runTurn(client, runner, toolDefinitions(), &messages, "hello")
	if !strings.Contains(reply, "Mock reply") {
		t.Fatalf("unexpected reply: %q (calls=%d)", reply, calls)
	}
	if calls != 2 {
		t.Fatalf("expected 2 API calls (tool + final), got %d", calls)
	}
}
