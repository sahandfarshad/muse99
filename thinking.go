package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// v0.9.15: which of the model's earlier thoughts (reasoning_content)
// are sent back with the history.
//
//	"turn" (default): only the steps since the latest user message, so
//	         a model with interleaved thinking (GLM, DeepSeek, Kimi)
//	         keeps its thread between tool steps. Older thinking is
//	         dropped: it is resent on every call and only costs tokens.
//	"all":   every stored thought (preserved thinking; costs more).
//	"off":   never (the behaviour before v0.9.15).
//
// Set in Options (Thinking select), FileConfig.ThinkingKeep, or env
// MUSE99_KEEP_THINKING.
func thinkingKeep() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MUSE99_KEEP_THINKING")))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(loadFileConfig().ThinkingKeep))
	}
	switch v {
	case "all", "off":
		return v
	}
	return "turn"
}

// keepThinking returns a copy of msgs with reasoning_content kept only
// where thinkingKeep() says. It never changes msgs.
func keepThinking(msgs []Message, never bool) []Message {
	mode := thinkingKeep()
	if never {
		mode = "off"
	}
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lastUser = i
		}
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].ReasoningContent == "" {
			continue
		}
		keep := out[i].Role == "assistant" && (mode == "all" || (mode == "turn" && i > lastUser))
		if !keep {
			out[i].ReasoningContent = ""
		}
	}
	return out
}

func hasReasoning(msgs []Message) bool {
	for _, m := range msgs {
		if m.ReasoningContent != "" {
			return true
		}
	}
	return false
}

func stripReasoning(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].ReasoningContent = ""
	}
	return out
}

// handleThinking serves GET/POST /api/thinking {mode}.
func (s *webServer) handleThinking(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		_ = json.NewEncoder(w).Encode(map[string]any{"mode": thinkingKeep()})
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	m := strings.ToLower(strings.TrimSpace(body.Mode))
	if m != "turn" && m != "all" && m != "off" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "mode is turn, all or off"})
		return
	}
	fc := loadFileConfig()
	fc.ThinkingKeep = m
	if m == "turn" {
		fc.ThinkingKeep = ""
	}
	if err := saveFileConfig(fc); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "mode": m})
}
