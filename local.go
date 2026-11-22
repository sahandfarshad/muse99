package main

// v0.10.0: local model servers (LM Studio, Ollama, llama.cpp, Jan,
// vLLM, KoboldCpp). All of them speak the OpenAI-compatible API, so
// connecting one is just a base URL + model. This file finds which of
// them are running on this computer, lists their models, and (in the
// Windows desktop app) connects to LM Studio on its own at start.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// localServer is one local model server Muse99 knows how to find.
type localServer struct {
	Name  string `json:"name"`  // provider name (matches a preset)
	Label string `json:"label"` // shown in the UI
	Base  string `json:"base_url"`
	Key   string `json:"api_key"` // placeholder key the server ignores
}

func localServers() []localServer {
	return []localServer{
		{"lmstudio", "LM Studio", "http://127.0.0.1:1234/v1", "lm-studio"},
		{"ollama", "Ollama", "http://127.0.0.1:11434/v1", "ollama"},
		{"llamacpp", "llama.cpp server / LocalAI", "http://127.0.0.1:8080/v1", "local"},
		{"jan", "Jan", "http://127.0.0.1:1337/v1", "local"},
		{"vllm", "vLLM", "http://127.0.0.1:8000/v1", "local"},
		{"koboldcpp", "KoboldCpp", "http://127.0.0.1:5001/v1", "local"},
	}
}

// isLocalURL reports whether a base URL points at this computer.
func isLocalURL(b string) bool {
	b = strings.ToLower(b)
	return strings.Contains(b, "://localhost") || strings.Contains(b, "://127.0.0.1") || strings.Contains(b, "://[::1]")
}

type localModel struct {
	ID     string `json:"id"`
	Loaded bool   `json:"loaded,omitempty"`
}

type localFound struct {
	localServer
	Up     bool         `json:"up"`
	Models []localModel `json:"models"`
	Error  string       `json:"error,omitempty"`
}

var localHTTP = &http.Client{Timeout: 1500 * time.Millisecond}

// probeLocal asks one server for its models. LM Studio also has a
// native endpoint (/api/v0/models) that says which models are loaded
// and which are embedding models; we use it when it answers.
func probeLocal(ls localServer) localFound {
	f := localFound{localServer: ls, Models: []localModel{}}
	base := strings.TrimRight(ls.Base, "/")
	if ls.Name == "lmstudio" {
		if ms, ok := lmStudioNative(base); ok {
			f.Up, f.Models = true, ms
			return f
		}
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+ls.Key)
	resp, err := localHTTP.Do(req)
	if err != nil {
		f.Error = "not running"
		return f
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.Error = fmt.Sprintf("answered HTTP %d", resp.StatusCode)
		return f
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct { // Ollama-style fallback
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.Error = "not an OpenAI-compatible server"
		return f
	}
	f.Up = true
	for _, d := range body.Data {
		if d.ID != "" && !looksEmbedding(d.ID) {
			f.Models = append(f.Models, localModel{ID: d.ID})
		}
	}
	for _, d := range body.Models {
		if d.Name != "" && !looksEmbedding(d.Name) {
			f.Models = append(f.Models, localModel{ID: d.Name})
		}
	}
	return f
}

func lmStudioNative(base string) ([]localModel, bool) {
	root := strings.TrimSuffix(base, "/v1")
	resp, err := localHTTP.Get(root + "/api/v0/models")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Data []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil, false
	}
	out := []localModel{}
	for _, d := range body.Data {
		if d.ID == "" || d.Type == "embeddings" || looksEmbedding(d.ID) {
			continue
		}
		out = append(out, localModel{ID: d.ID, Loaded: d.State == "loaded"})
	}
	// Loaded models first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Loaded && !out[j].Loaded })
	return out, true
}

func looksEmbedding(id string) bool {
	l := strings.ToLower(id)
	return strings.Contains(l, "embed") || strings.Contains(l, "bge-") || strings.Contains(l, "rerank")
}

// detectLocal probes every known local server at once (about 1.5 s
// at most) and returns them in the fixed order of localServers().
func detectLocal(extra string) []localFound {
	list := localServers()
	if extra = strings.TrimSpace(extra); extra != "" {
		list = append(list, localServer{Name: "local", Label: "Your address", Base: normalizeLocalBase(extra), Key: "local"})
	}
	out := make([]localFound, len(list))
	var wg sync.WaitGroup
	for i, ls := range list {
		wg.Add(1)
		go func(i int, ls localServer) {
			defer wg.Done()
			out[i] = probeLocal(ls)
		}(i, ls)
	}
	wg.Wait()
	return out
}

// normalizeLocalBase turns "127.0.0.1:5000" or "http://pc:5000" into
// a base URL ending in /v1.
func normalizeLocalBase(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	if u, err := url.Parse(s); err == nil && (u.Path == "" || u.Path == "/") {
		s += "/v1"
	}
	return s
}

// pickLocalModel chooses the model to connect to: a loaded one first,
// an OxCoder one next, else the first.
func pickLocalModel(ms []localModel) string {
	if len(ms) == 0 {
		return ""
	}
	for _, m := range ms {
		if m.Loaded {
			return m.ID
		}
	}
	for _, m := range ms {
		if strings.Contains(strings.ToLower(m.ID), "oxcoder") {
			return m.ID
		}
	}
	return ms[0].ID
}

// autoConnectLocal is run by the desktop app at start. When no model is
// usable yet (no API key on the active provider) it looks for LM Studio,
// then the other local servers, and makes the first one it finds with
// a model the active provider. Returns a line for the log ("" = no change).
func autoConnectLocal(cfg *Config) string {
	fc := loadFileConfig()
	fc.ensureProviders()
	active := fc.activeProvider()
	if strings.TrimSpace(cfg.APIKey) != "" && !(isLocalURL(active.BaseURL) && active.Model == "") {
		return ""
	}
	for _, f := range detectLocal("") {
		if !f.Up {
			continue
		}
		model := pickLocalModel(f.Models)
		if model == "" {
			continue
		}
		p := Provider{Name: f.Name, BaseURL: f.Base, Model: model, APIKey: f.Key}
		fc.setProvider(p)
		fc.ActiveProvider = f.Name
		fc.noteRecentModel(f.Name, model)
		if err := saveFileConfig(fc); err != nil {
			return "auto-connect: could not save config: " + err.Error()
		}
		cfg.Provider, cfg.BaseURL, cfg.Model, cfg.APIKey = f.Name, f.Base, model, f.Key
		if cfg.Providers == nil {
			cfg.Providers = map[string]Provider{}
		}
		cfg.Providers[f.Name] = p
		return fmt.Sprintf("auto-connected to %s (%s), model %s", f.Label, f.Base, model)
	}
	return ""
}

// handleLocalDetect serves GET /api/local/detect[?url=host:port].
func (s *webServer) handleLocalDetect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"servers": detectLocal(r.URL.Query().Get("url")), "active": s.cfg.Provider})
}
