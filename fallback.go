package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ModelRef names one model on one saved provider.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

const maxRecentModels = 10

// noteRecentModel puts a model at the front of the recent list used by
// the chat's model menu (deduplicated, at most maxRecentModels).
func (fc *FileConfig) noteRecentModel(provider, model string) {
	model = normalizeModelID(model)
	provider = strings.TrimSpace(provider)
	if provider == "" || model == "" {
		return
	}
	out := []ModelRef{{Provider: provider, Model: model}}
	for _, m := range fc.RecentModels {
		if m.Provider == provider && m.Model == model {
			continue
		}
		if len(out) >= maxRecentModels {
			break
		}
		out = append(out, m)
	}
	fc.RecentModels = out
}

// fallbackClient builds the backup model's client, or nil when no
// backup is set or it is the model already in use.
func (s *webServer) fallbackClient() *Client {
	fc := loadFileConfig()
	if fc.Fallback == nil || strings.TrimSpace(fc.Fallback.Model) == "" || !validProviderName(fc.Fallback.Provider) {
		return nil
	}
	model := normalizeModelID(fc.Fallback.Model)
	if fc.Fallback.Provider == s.cfg.Provider && model == s.client.Model {
		return nil
	}
	p := mergedProvider(&s.cfg, fc.Fallback.Provider)
	if p.BaseURL == "" {
		return nil
	}
	return NewClientForProvider(p.BaseURL, p.APIKey, model, fc.Fallback.Provider)
}

// handleModelFallback serves POST /api/models/fallback {provider,
// model}: set the backup model (an empty model clears it).
func (s *webServer) handleModelFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body ModelRef
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "need JSON body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fc := loadFileConfig()
	model := normalizeModelID(body.Model)
	if model == "" {
		fc.Fallback = nil
	} else {
		name := strings.TrimSpace(body.Provider)
		if !validProviderName(name) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown provider"})
			return
		}
		fc.Fallback = &ModelRef{Provider: name, Model: model}
	}
	if err := saveFileConfig(fc); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "fallback": fc.Fallback})
}

// modelMenuJSON is what /api/status sends for the chat's model menu.
func (s *webServer) modelMenuJSON() (recent []ModelRef, fallback *ModelRef) {
	fc := loadFileConfig()
	recent = append([]ModelRef{}, fc.RecentModels...)
	has := func(p, m string) bool {
		for _, r := range recent {
			if r.Provider == p && r.Model == m {
				return true
			}
		}
		return false
	}
	if s.cfg.Model != "" && !has(s.cfg.Provider, s.cfg.Model) {
		recent = append([]ModelRef{{Provider: s.cfg.Provider, Model: s.cfg.Model}}, recent...)
	}
	if fc.Fallback != nil && !has(fc.Fallback.Provider, fc.Fallback.Model) {
		recent = append(recent, *fc.Fallback)
	}
	return recent, fc.Fallback
}
