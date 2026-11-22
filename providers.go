package main

// v0.11.0: provider tabs and favourite models.
//
// Settings shows one tab per provider: Gemini, OpenAI and Anthropic
// are always there, and any provider the user saved a key for or
// added under their own name gets a tab too. Inside a tab the user
// saves the key, sees every model the provider lists, tests one or
// all of them, and stars the ones they like. Starred models
// (FileConfig.Favorites) make up the model menu above the chat.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// defaultTabs are the provider tabs that are always shown.
var defaultTabs = []string{"gemini", "openai", "anthropic"}

// localPresets are local servers; they live in the "Local model" card.
var localPresets = map[string]bool{"lmstudio": true, "ollama": true, "llamacpp": true, "jan": true, "vllm": true, "koboldcpp": true, "custom": true}

// providerLabel is the name a tab shows.
func providerLabel(name string) string {
	switch name {
	case "gemini":
		return "Gemini"
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "spark":
		return "Muse Spark"
	case "openrouter":
		return "OpenRouter"
	}
	return name
}

// isPresetName reports whether name is a built-in provider.
func isPresetName(name string) bool {
	_, ok := providerPreset(name)
	return ok
}

// providerTab is one tab in Settings. The key itself is never sent.
type providerTab struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	BaseURL   string `json:"base_url"`
	KeyMasked string `json:"key_masked"`
	HasKey    bool   `json:"has_key"`
	Model     string `json:"model"`
	Preset    bool   `json:"preset"`
	Removable bool   `json:"removable"`
	Active    bool   `json:"active"`
}

// tabNames lists the providers that get a tab, in order: the
// defaults, then the active one, then every other saved provider
// with a key or a name of the user's own (sorted).
func tabNames(fc FileConfig, cfg *Config) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(n string) {
		if n != "" && !seen[n] && validProviderName(n) {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range defaultTabs {
		add(n)
	}
	if cfg != nil && cfg.Provider != "" && !localPresets[cfg.Provider] {
		add(cfg.Provider)
	}
	names := map[string]bool{}
	for n := range fc.Providers {
		names[n] = true
	}
	if cfg != nil {
		for n := range cfg.Providers {
			names[n] = true
		}
	}
	extra := []string{}
	for n := range names {
		if seen[n] || localPresets[n] {
			continue
		}
		if !isPresetName(n) || mergedProvider(cfg, n).APIKey != "" {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	for _, n := range extra {
		add(n)
	}
	return out
}

func (s *webServer) providerTabs() []providerTab {
	fc := loadFileConfig()
	tabs := []providerTab{}
	for _, n := range tabNames(fc, &s.cfg) {
		p := mergedProvider(&s.cfg, n)
		preset := isPresetName(n)
		tabs = append(tabs, providerTab{
			Name: n, Label: providerLabel(n), BaseURL: p.BaseURL,
			KeyMasked: maskKey(p.APIKey), HasKey: p.APIKey != "",
			Model: p.Model, Preset: preset,
			Removable: !preset && n != s.cfg.Provider,
			Active:    n == s.cfg.Provider,
		})
	}
	return tabs
}

// saveProviderEntry stores a provider's base URL and key without
// making it active (blank fields keep what is saved).
func (s *webServer) saveProviderEntry(name, baseURL, apiKey string) Provider {
	fc := loadFileConfig()
	fc.ensureProviders()
	p := mergedProvider(&s.cfg, name)
	if b := strings.TrimSpace(baseURL); b != "" {
		p.BaseURL = b
	}
	if k := strings.TrimSpace(apiKey); k != "" {
		p.APIKey = k
	}
	p.Name = name
	fc.Providers[name] = p
	_ = saveFileConfig(fc)
	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]Provider{}
	}
	s.cfg.Providers[name] = p
	if name == s.cfg.Provider {
		s.cfg.BaseURL, s.cfg.APIKey = p.BaseURL, p.APIKey
		*s.client = *NewClientForProvider(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider)
	}
	return p
}

// handleProviders serves the provider tabs:
//
//	GET    /api/providers               tabs + favourites
//	POST   /api/providers {name, base_url, api_key}  add / save one
//	DELETE /api/providers?name=x        remove a provider you added
func (s *webServer) handleProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(msg string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var body struct {
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			APIKey  string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail("need JSON body")
			return
		}
		name := strings.ToLower(strings.TrimSpace(body.Name))
		if !validProviderName(name) {
			fail("name: letters, digits, - _ . only (max 40)")
			return
		}
		if localPresets[name] {
			fail("that name is for a local server — use the Local model card")
			return
		}
		base := strings.TrimSpace(body.BaseURL)
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			fail("the API address must start with https:// or http://")
			return
		}
		if base == "" && mergedProvider(&s.cfg, name).BaseURL == "" {
			fail("add the API address (base URL), for example https://api.deepseek.com/v1")
			return
		}
		s.saveProviderEntry(name, base, body.APIKey)
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if !validProviderName(name) || isPresetName(name) {
			fail("only a provider you added can be removed")
			return
		}
		if name == s.cfg.Provider {
			fail("this provider is in use — pick a model from another provider first")
			return
		}
		fc := loadFileConfig()
		delete(fc.Providers, name)
		fc.Favorites = dropProvider(fc.Favorites, name)
		fc.RecentModels = dropProvider(fc.RecentModels, name)
		if fc.Fallback != nil && fc.Fallback.Provider == name {
			fc.Fallback = nil
		}
		if err := saveFileConfig(fc); err != nil {
			fail(err.Error())
			return
		}
		delete(s.cfg.Providers, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fc := loadFileConfig()
	favs := fc.Favorites
	if favs == nil {
		favs = []ModelRef{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "providers": s.providerTabs(), "active": s.cfg.Provider, "model": s.cfg.Model, "favorites": favs})
}

func dropProvider(list []ModelRef, name string) []ModelRef {
	out := []ModelRef{}
	for _, m := range list {
		if m.Provider != name {
			out = append(out, m)
		}
	}
	return out
}

// setFavorite stars (on) or un-stars a model.
func (fc *FileConfig) setFavorite(provider, model string, on bool) {
	model = normalizeModelID(model)
	out := []ModelRef{}
	for _, m := range fc.Favorites {
		if m.Provider == provider && m.Model == model {
			continue
		}
		out = append(out, m)
	}
	if on {
		out = append(out, ModelRef{Provider: provider, Model: model})
	}
	fc.Favorites = out
}

// handleModelFavorite serves POST /api/models/favorite {provider,
// model, on}.
func (s *webServer) handleModelFavorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		On       bool   `json:"on"`
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "need JSON body"})
		return
	}
	name := strings.TrimSpace(body.Provider)
	if !validProviderName(name) || normalizeModelID(body.Model) == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "need provider and model"})
		return
	}
	fc := loadFileConfig()
	fc.setFavorite(name, body.Model, body.On)
	if err := saveFileConfig(fc); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "favorites": fc.Favorites})
}
