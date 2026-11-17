package main

// Model discovery + testing (v4).
//
// Instead of typing a model name by hand, Muse99 asks the provider
// itself what models it has: GET {base_url}/models, the standard
// OpenAI-compatible endpoint. The list is cached in memory for 10
// minutes and the last-known list is persisted in the DB, so the
// chooser still works offline after a restart.
//
// Provider quirks (documented in README too):
//   - Spark / OpenAI / OpenRouter: /v1/models works with the key.
//   - Gemini (v6 fixes): the OpenAI-compat list can come back empty
//     or 401 — then we retry with the native x-goog-api-key header
//     and fall back to the native GET /v1beta/models endpoint, which
//     answers {"models": [{"name": "models/gemini-2.5-flash"}]}.
//     The "models/" prefix is stripped everywhere (list, test, save,
//     chat), because chat completions need the bare id. Only if all
//     of that fails do we show suggested models.
//   - Ollama: /v1/models works, but if it fails we fall back to the
//     native GET /api/tags and map tag names to model ids.
//   - Any provider whose /models endpoint is blocked/empty: the UI
//     shows a hidden manual-entry fallback, not a visible input.
//
// Security: API keys are never logged and never returned in any
// endpoint response. The test endpoint only returns ok/model/
// latency/reply/error.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

const modelCacheTTL = 10 * time.Minute

// ModelInfo is one model a provider offers.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// cachedModelList is one fetched list, kept in memory and in the DB.
type cachedModelList struct {
	Models    []ModelInfo `json:"models"`
	FetchedAt time.Time   `json:"fetched_at"`
	Source    string      `json:"source"` // "live" | "cached" | "suggested"
	Provider  string      `json:"provider,omitempty"`
}

var bucketModelLists = []byte("modelLists")

var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]cachedModelList{}
)

// clearModelCache resets the in-memory cache (used by tests).
func clearModelCache() {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	modelCache = map[string]cachedModelList{}
}

// modelCacheKey is the cache key for a provider endpoint.
func modelCacheKey(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// ---------- parsing ----------

// normalizeModelID cleans a model id for use in chat completions.
// Gemini's native API returns ids as "models/gemini-2.5-flash", but
// chat completions (OpenAI-compatible or native) need the bare id
// "gemini-2.5-flash" — sending the prefixed id fails with "model not
// found". The same strip is applied everywhere a model id is saved
// or sent, so a prefixed id can never leak into a request.
func normalizeModelID(id string) string {
	id = strings.TrimSpace(id)
	for strings.HasPrefix(id, "models/") {
		id = strings.TrimPrefix(id, "models/")
	}
	return id
}

// isGeminiBase reports whether a base URL / provider is Google Gemini.
func isGeminiBase(baseURL, provider string) bool {
	return strings.ToLower(strings.TrimSpace(provider)) == "gemini" ||
		strings.Contains(strings.ToLower(baseURL), "generativelanguage.googleapis.com")
}

// parseModelsResponse parses a provider's model list, accepting the
// shapes providers actually return:
//   - OpenAI: {"data": [{"id": "...", "owned_by": "..."}]}
//   - Ollama native: {"models": [{"name": "..."}]}
//   - {"models": [{"id": ...}]} / {"models": ["..."]}
//   - a bare JSON array of objects or strings.
//
// An empty list is not an error — the caller decides what to do
// (e.g. Gemini suggested models). Malformed JSON is an error.
func parseModelsResponse(body []byte) ([]ModelInfo, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response from models endpoint")
	}
	// Bare array: [{"id": "..."}] or ["model-a", ...]
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("bad models response: %w", err)
		}
		return parseModelItems(arr)
	}
	var obj struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, fmt.Errorf("bad models response: %w", err)
	}
	if obj.Data != nil {
		return parseModelItems(obj.Data)
	}
	if obj.Models != nil {
		return parseModelItems(obj.Models)
	}
	return []ModelInfo{}, nil
}

// parseModelItems turns one array of model entries into ModelInfos.
// Entries may be {"id": "...", "owned_by": "..."}, {"name": "..."}
// (Ollama tags), or plain strings.
func parseModelItems(items []json.RawMessage) ([]ModelInfo, error) {
	out := []ModelInfo{}
	seen := map[string]bool{}
	for _, raw := range items {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			s = normalizeModelID(s)
			if s != "" && !seen[s] {
				seen[s] = true
				out = append(out, ModelInfo{ID: s})
			}
			continue
		}
		var m struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Model   string `json:"model"`
			OwnedBy string `json:"owned_by"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		// Gemini's native list uses "name": "models/gemini-2.5-flash";
		// the OpenAI-compat list uses "id". Either way, strip the
		// "models/" prefix — see normalizeModelID.
		id := normalizeModelID(m.ID)
		if id == "" {
			id = normalizeModelID(m.Name)
		}
		if id == "" {
			id = normalizeModelID(m.Model)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ModelInfo{ID: id, OwnedBy: m.OwnedBy})
	}
	return out, nil
}

// suggestedModelsFor returns well-known models for providers whose
// /models endpoint is limited, shown in the UI as "suggested".
func suggestedModelsFor(provider string) []ModelInfo {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini":
		return []ModelInfo{
			{ID: "gemini-2.5-flash", OwnedBy: "google"},
			{ID: "gemini-2.5-pro", OwnedBy: "google"},
			{ID: "gemini-2.0-flash", OwnedBy: "google"},
		}
	case "spark":
		return []ModelInfo{
			{ID: "muse-spark-1.3", OwnedBy: "meta"},
			{ID: "muse-spark-1.3-contributor", OwnedBy: "meta"},
		}
	}
	return nil
}

// ---------- DB persistence of the last-known list ----------

// SaveModelList persists a fetched list so the chooser works offline
// after a restart. It is a no-op without a DB.
func (d *DB) SaveModelList(baseURL string, list cachedModelList) error {
	if d == nil {
		return nil
	}
	key := modelCacheKey(baseURL)
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketModelLists)
		if b == nil {
			return fmt.Errorf("modelLists bucket is missing")
		}
		return b.Put([]byte(key), data)
	})
}

// LoadModelList returns the last persisted list for a base URL.
func (d *DB) LoadModelList(baseURL string) (cachedModelList, bool) {
	var out cachedModelList
	if d == nil {
		return out, false
	}
	_ = d.bolt.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketModelLists)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(modelCacheKey(baseURL)))
		if v == nil {
			return nil
		}
		_ = json.Unmarshal(v, &out)
		return nil
	})
	return out, len(out.Models) > 0
}

// ---------- live fetching ----------

var modelsHTTP = &http.Client{Timeout: 15 * time.Second}

// fetchModelsFromURL does one GET to an endpoint that should return a
// model list. It never sends a key it was not given, and never logs one.
//
// Gemini auth quirk (v6): Google's OpenAI-compatible endpoint
// accepts a Bearer key, but some Gemini keys / projects are only
// accepted with the native `x-goog-api-key` header. So on a 401/403
// from a Gemini endpoint, the request is retried once with that
// header instead of Bearer. Other providers are unaffected.
func fetchModelsFromURL(url, apiKey, provider string) ([]ModelInfo, error) {
	doGet := func(useGoogKey bool) ([]ModelInfo, int, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Accept", "application/json")
		// Local providers (Ollama) work without a key; the stored key is
		// "ollama" there, which is harmless to send. Skip obviously
		// placeholder keys so a local server is not confused by them.
		if apiKey != "" && apiKey != "ollama" {
			if useGoogKey {
				req.Header.Set("x-goog-api-key", apiKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+apiKey)
			}
		}
		resp, err := modelsHTTP.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("could not reach %s: %w", url, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode >= 400 {
			return nil, resp.StatusCode, fmt.Errorf("models endpoint returned %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		models, err := parseModelsResponse(body)
		return models, resp.StatusCode, err
	}
	models, status, err := doGet(false)
	if err != nil && (status == 401 || status == 403) && isGeminiBase(url, provider) {
		if retry, _, rerr := doGet(true); rerr == nil {
			return retry, nil
		}
	}
	return models, err
}

// fetchGeminiNativeModels asks Gemini's native models.list endpoint
// (GET /v1beta/models), which is the reliable list when the
// OpenAI-compatible /models endpoint is empty or errors. It answers
// {"models": [{"name": "models/gemini-2.5-flash", ...}]} — the
// "models/" prefix is stripped by parseModelsResponse. Auth is the
// native x-goog-api-key header. The native root is derived from the
// provider base URL, so a test server can stand in for Google.
func fetchGeminiNativeModels(apiKey, base string) ([]ModelInfo, error) {
	root := "https://generativelanguage.googleapis.com/v1beta"
	if !strings.Contains(base, "generativelanguage.googleapis.com") {
		root = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(base), "/"), "/openai")
	}
	req, err := http.NewRequest("GET", root+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("x-goog-api-key", apiKey)
	}
	resp, err := modelsHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Gemini models list: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Gemini models list returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseModelsResponse(body)
}

// needsAPIKey reports whether a provider needs a real API key.
// Local servers (Ollama, LM Studio, …) work without one. A named
// cloud provider (spark, openai, …) always needs one, even when it
// points at a local address in tests.
func needsAPIKey(baseURL, provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ollama", "custom":
		return false
	case "spark", "openai", "gemini", "openrouter":
		return true
	}
	b := strings.ToLower(strings.TrimSpace(baseURL))
	if strings.Contains(b, "localhost") || strings.Contains(b, "127.0.0.1") || strings.Contains(b, ":11434") {
		return false
	}
	return true
}

// fetchModelsLive asks a provider for its models, with provider
// fallbacks. Returns the models and the source ("live" or
// "suggested"). An error means the provider could not be asked at
// all; an empty live list falls back to suggested models where we
// have them, and to an empty list otherwise.
func fetchModelsLive(baseURL, apiKey, provider string) ([]ModelInfo, string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, "", fmt.Errorf("no API base URL set for this provider")
	}
	if strings.TrimSpace(apiKey) == "" && needsAPIKey(base, provider) {
		return nil, "", fmt.Errorf("this provider needs an API key — add one in Settings first")
	}
	models, err := fetchModelsFromURL(base+"/models", apiKey, provider)
	if err == nil && len(models) > 0 {
		return models, "live", nil
	}
	// Ollama fallback: native /api/tags (base is .../v1, strip it).
	if strings.Contains(base, ":11434") || strings.ToLower(provider) == "ollama" {
		root := strings.TrimSuffix(base, "/v1")
		if tagged, terr := fetchModelsFromURL(root+"/api/tags", apiKey, provider); terr == nil && len(tagged) > 0 {
			return tagged, "live", nil
		}
	}
	// Gemini fallback: the OpenAI-compatible /models endpoint often
	// answers empty or errors for a key that works fine on the native
	// API — ask the native models.list endpoint instead.
	if isGeminiBase(base, provider) {
		if native, nerr := fetchGeminiNativeModels(apiKey, base); nerr == nil && len(native) > 0 {
			return native, "live", nil
		}
	}
	if err != nil {
		return nil, "", err
	}
	// Live endpoint answered but listed nothing (Gemini's
	// OpenAI-compat endpoint often does this).
	if sugg := suggestedModelsFor(provider); len(sugg) > 0 {
		return sugg, "suggested", nil
	}
	return []ModelInfo{}, "live", nil
}

// getModels returns a provider's model list, using the 10-minute
// in-memory cache, then a live fetch, then the persisted last-known
// list, then suggested models. force skips the cache and goes
// straight to a live fetch. The returned source is "live",
// "cached", or "suggested". It never returns the API key.
func getModels(baseURL, apiKey, provider string, force bool) (cachedModelList, error) {
	key := modelCacheKey(baseURL)
	if !force {
		modelCacheMu.Lock()
		if c, ok := modelCache[key]; ok && time.Since(c.FetchedAt) < modelCacheTTL {
			modelCacheMu.Unlock()
			c.Source = "cached"
			return c, nil
		}
		modelCacheMu.Unlock()
	}
	models, source, err := fetchModelsLive(baseURL, apiKey, provider)
	if err != nil {
		// Live fetch failed: fall back to the persisted list so the
		// chooser still works offline, then to the cache, then error.
		if appDB != nil {
			if saved, ok := appDB.LoadModelList(baseURL); ok {
				saved.Source = "cached"
				return saved, nil
			}
		}
		modelCacheMu.Lock()
		if c, ok := modelCache[key]; ok {
			modelCacheMu.Unlock()
			c.Source = "cached"
			return c, nil
		}
		modelCacheMu.Unlock()
		return cachedModelList{}, err
	}
	list := cachedModelList{Models: models, FetchedAt: time.Now(), Source: source, Provider: provider}
	modelCacheMu.Lock()
	modelCache[key] = list
	modelCacheMu.Unlock()
	if appDB != nil {
		_ = appDB.SaveModelList(baseURL, list)
	}
	return list, nil
}

// ---------- testing a model ----------

// testModel sends one tiny chat completion to a model and reports
// whether it actually answers. Returns the reply, latency in ms,
// and an error. The API key is never included in any return value.
func testModel(baseURL, apiKey, model, provider string) (reply string, latencyMs int64, err error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	model = normalizeModelID(model)
	if base == "" {
		return "", 0, fmt.Errorf("no API base URL set")
	}
	if model == "" {
		return "", 0, fmt.Errorf("no model to test")
	}
	if provider == "" {
		provider = matchProvider(base, model)
	}
	if strings.TrimSpace(apiKey) == "" && needsAPIKey(base, provider) {
		return "", 0, fmt.Errorf("this provider needs an API key — add one in Settings first")
	}
	c := NewClientForProvider(base, apiKey, model, provider)
	c.HTTP.Timeout = 60 * time.Second
	start := time.Now()
	msg, err := c.Chat([]Message{{Role: "user", Content: "Reply with the single word OK"}}, nil)
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return "", latencyMs, err
	}
	reply = strings.TrimSpace(msg.Content)
	if reply == "" {
		return "", latencyMs, fmt.Errorf("model answered but returned empty text")
	}
	return reply, latencyMs, nil
}

// ---------- resolving which provider a request means ----------

// resolveModelTarget works out the base URL / key / provider name for
// a model-list or model-test request. Priority: an explicit provider
// name (from the saved providers map, falling back to presets), then
// an explicit base URL + key for an unsaved custom provider, then
// the active provider in cfg.
func resolveModelTarget(cfg *Config, provider, baseURL, apiKey string) (base, key, name string) {
	name = strings.TrimSpace(provider)
	if name != "" {
		if cfg != nil && cfg.Providers != nil {
			if p, ok := cfg.Providers[name]; ok {
				base, key = p.BaseURL, p.APIKey
			}
		}
		if base == "" {
			if preset, ok := providerPreset(name); ok {
				base = preset.BaseURL
			}
		}
		if baseURL != "" {
			base = baseURL
		}
		if apiKey != "" {
			key = apiKey
		}
		return base, key, name
	}
	if strings.TrimSpace(baseURL) != "" {
		return strings.TrimSpace(baseURL), apiKey, matchProvider(baseURL, "")
	}
	if cfg != nil {
		return cfg.BaseURL, cfg.APIKey, cfg.Provider
	}
	return "", "", ""
}
