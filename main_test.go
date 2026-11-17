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

func TestProviderPresets(t *testing.T) {
	byName := map[string]Provider{}
	for _, p := range providerPresets() {
		byName[p.Name] = p
	}
	if byName["spark"].BaseURL != "https://api.meta.ai/v1" || byName["spark"].Model != "muse-spark-1.3" {
		t.Fatalf("spark preset wrong: %+v", byName["spark"])
	}
	if byName["openai"].BaseURL != "https://api.openai.com/v1" || byName["openai"].Model != "gpt-5" {
		t.Fatalf("openai preset wrong: %+v", byName["openai"])
	}
	if byName["gemini"].BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai/" || byName["gemini"].Model != "gemini-2.5-flash" {
		t.Fatalf("gemini preset wrong: %+v", byName["gemini"])
	}
	if byName["openrouter"].BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("openrouter preset wrong: %+v", byName["openrouter"])
	}
	if byName["ollama"].BaseURL != "http://localhost:11434/v1" || byName["ollama"].Model != "llama3.1" {
		t.Fatalf("ollama preset wrong: %+v", byName["ollama"])
	}
	if _, ok := byName["custom"]; !ok {
		t.Fatal("custom preset missing")
	}
	// Switching restores the right key per provider.
	fc := FileConfig{}
	fc.setProvider(Provider{Name: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: "sk-spark"})
	fc.setProvider(Provider{Name: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "sk-openai"})
	cfg := Config{Providers: fc.Providers}
	if !cfg.switchProvider("openai") || cfg.APIKey != "sk-openai" || cfg.Model != "gpt-5" {
		t.Fatalf("switch to openai failed: %+v", cfg)
	}
	if !cfg.switchProvider("spark") || cfg.APIKey != "sk-spark" || cfg.Model != "muse-spark-1.3" {
		t.Fatalf("switch back to spark failed: %+v", cfg)
	}
	if cfg.switchProvider("nope") {
		t.Fatal("unknown provider should not switch")
	}
	// Legacy single-provider file folds into the matching provider.
	legacy := FileConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "sk-legacy"}
	// Simulate loadFileConfig's backward-compat fold.
	if len(legacy.Providers) == 0 {
		name := matchProvider(legacy.BaseURL, legacy.Model)
		legacy.Providers = map[string]Provider{name: {Name: name, BaseURL: legacy.BaseURL, Model: legacy.Model, APIKey: legacy.APIKey}}
		legacy.ActiveProvider = name
	}
	if legacy.ActiveProvider != "openai" || legacy.Providers["openai"].APIKey != "sk-legacy" {
		t.Fatalf("legacy fold failed: %+v", legacy)
	}
}

func TestWebAddressFor(t *testing.T) {
	if got := webAddressFor(Config{Domain: "muse99.example.com", TLSEnabled: true, WebPort: 8080}); got != "https://muse99.example.com" {
		t.Fatalf("TLS domain address = %q", got)
	}
	if got := webAddressFor(Config{Domain: "muse99.example.com", WebPort: 8080}); got != "http://muse99.example.com:8080" {
		t.Fatalf("plain domain address = %q", got)
	}
	if got := webAddressFor(Config{WebPort: 8080}); !strings.Contains(got, ":8080") {
		t.Fatalf("IP address should contain :8080: %q", got)
	}
}

func TestUsageRecording(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := estimatedCost(1_000_000, 1_000_000); got != 5.50 {
		t.Fatalf("estimatedCost(1M, 1M) = %v, want 5.50", got)
	}
	u := recordUsage(100, 50)
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 || u.Calls != 1 {
		t.Fatalf("recordUsage totals wrong: %+v", u)
	}
	u = recordUsage(200, 100)
	if u.PromptTokens != 300 || u.CompletionTokens != 150 || u.Calls != 2 {
		t.Fatalf("recordUsage did not accumulate: %+v", u)
	}
	if got := loadUsage(); got.PromptTokens != 300 {
		t.Fatalf("usage was not persisted: %+v", got)
	}
	if got := usageLine(u); !strings.Contains(got, "300 in") || !strings.Contains(got, "150 out") {
		t.Fatalf("usageLine wrong: %q", got)
	}
	// No usage reported -> no change.
	if got := recordUsage(0, 0); got.Calls != 2 {
		t.Fatalf("zero usage should not add a call: %+v", got)
	}
}

func TestClientRecordsUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
		})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "test", "muse-spark-1.3")
	msgs := []Message{{Role: "system", Content: "test"}}
	if _, err := client.Chat(msgs, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadUsage(); got.PromptTokens != 120 || got.CompletionTokens != 30 || got.Calls != 1 {
		t.Fatalf("client did not record usage: %+v", got)
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
