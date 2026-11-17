package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestThoughtSignatureRoundTrip: a tool call carrying Gemini's
// thought signature must survive a JSON round-trip through the
// ToolCall struct. Before the fix the struct had no field for it,
// so the signature was silently dropped and the next turn 400d.
func TestThoughtSignatureRoundTrip(t *testing.T) {
	raw := `{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"},"extra_content":{"google":{"thought_signature":"Ep4BCpsB"}}}`
	var tc ToolCall
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		t.Fatal(err)
	}
	if got := tc.thoughtSignature(); got != "Ep4BCpsB" {
		t.Fatalf("signature not read, got %q", got)
	}
	out, err := json.Marshal(tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Ep4BCpsB") {
		t.Fatalf("signature lost in round-trip, got %s", out)
	}
}

// geminiValidationServer mimics a Gemini 3 OpenAI-compatible
// endpoint: it returns a tool call with a thought signature on the
// first request, and on later requests it 400s with the exact error
// the real API returns when the replayed tool call lacks it.
func geminiValidationServer(t *testing.T, bodies *[]string) *httptest.Server {
	t.Helper()
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{}"},"extra_content":{"google":{"thought_signature":"Ep4BCpsB"}}}]}}]}`)
			return
		}
		if !strings.Contains(string(b), "Ep4BCpsB") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts. This is required for tools to work correctly, and missing thought_signature may lead to degraded model performance. Additional data, function call `+"`default_api:get_time`"+`, position 2.","status":"INVALID_ARGUMENT"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
}

func testToolDefs() []ToolDef {
	var d ToolDef
	d.Type = "function"
	d.Function.Name = "get_time"
	d.Function.Description = "Get the current date and time."
	d.Function.Parameters = map[string]any{}
	return []ToolDef{d}
}

func testToolCall(id string) ToolCall {
	var tc ToolCall
	tc.ID = id
	tc.Type = "function"
	tc.Function.Name = "get_time"
	tc.Function.Arguments = "{}"
	return tc
}

// TestChatPreservesThoughtSignature reproduces the user's Gemini 400:
// a tool turn works only when the signature from the first response
// is sent back on the second request.
func TestChatPreservesThoughtSignature(t *testing.T) {
	var bodies []string
	srv := geminiValidationServer(t, &bodies)
	defer srv.Close()
	c := NewClientForProvider(srv.URL, "key", "gemini-3-flash", "gemini")
	msgs := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "what time is it?"}}
	msg, err := c.Chat(msgs, testToolDefs())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].thoughtSignature() != "Ep4BCpsB" {
		t.Fatalf("first reply should carry the signature, got %+v", msg.ToolCalls)
	}
	msgs = append(msgs, msg, Message{Role: "tool", Content: "12:00", ToolCallID: "call_1"})
	if _, err := c.Chat(msgs, testToolDefs()); err != nil {
		t.Fatalf("second call should succeed once the signature is replayed, got: %v", err)
	}
}

// TestChatSentinelForUnsignedGemini3: history whose first tool call
// has no signature (another model, or a session saved before
// signatures were preserved) must get Google's documented bypass
// sentinel — on the FIRST call only, never on parallel siblings.
func TestChatSentinelForUnsignedGemini3(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	c := NewClientForProvider(srv.URL, "key", "gemini-3-flash", "gemini")
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{testToolCall("c1"), testToolCall("c2")}},
		{Role: "tool", Content: "12:00", ToolCallID: "c1"},
		{Role: "tool", Content: "12:00", ToolCallID: "c2"},
	}
	if _, err := c.Chat(msgs, nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(bodies))
	}
	if n := strings.Count(bodies[0], skipThoughtSignatureValidator); n != 1 {
		t.Fatalf("sentinel should appear exactly once (first call only), got %d in %s", n, bodies[0])
	}
	if n := strings.Count(bodies[0], "extra_content"); n != 1 {
		t.Fatalf("extra_content should appear exactly once (first call only), got %d in %s", n, bodies[0])
	}
}

// TestChatNoSentinelForOtherProviders: the sentinel is a Gemini-only
// concern — other providers must never see it.
func TestChatNoSentinelForOtherProviders(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	c := NewClientForProvider(srv.URL, "key", "gpt-5", "openai")
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{testToolCall("c1")}},
		{Role: "tool", Content: "12:00", ToolCallID: "c1"},
	}
	if _, err := c.Chat(msgs, nil); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if strings.Contains(bodies[0], skipThoughtSignatureValidator) || strings.Contains(bodies[0], "extra_content") {
		t.Fatalf("non-Gemini request must not carry a thought signature, got %s", bodies[0])
	}
}
