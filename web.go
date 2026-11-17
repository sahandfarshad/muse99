package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Muse99 Web UI — clean, minimal chat dashboard, no build step.
//   muse99 --web --port 3759 --allow-shell
// Tabs: Chat | Files | Tasks | Memory | Servers | Settings
// The address is shown at the top of the page AND in the SSH menu.
//
// v2: the web server is a real controllable service — Start / Stop /
// Restart with graceful Shutdown, optional TLS (Let's Encrypt certs),
// and provider settings in /api/settings. All /api/* endpoints keep
// their original JSON shapes; new fields are added, none removed.

type webServer struct {
	client        *Client
	runner        *toolRunner
	tools         []ToolDef
	cfg           Config
	mu            sync.Mutex
	messages      []Message
	system        string
	db            *DB
	currentChatID string
}

type chatReq struct {
	Message string `json:"message"`
	ChatID  string `json:"chat_id,omitempty"`
}
type chatResp struct {
	Reply  string   `json:"reply"`
	Tools  []string `json:"tools"`
	Error  string   `json:"error,omitempty"`
	ChatID string   `json:"chat_id,omitempty"`
}

// webController owns one HTTP server so the SSH menu can start, stop
// and restart it without leaking goroutines.
type webController struct {
	mu      sync.Mutex
	srv     *http.Server
	running bool
	done    chan struct{}
	cfg     Config
	client  *Client
	runner  *toolRunner
	tools   []ToolDef
	system  string
}

func newWebController(cfg Config, client *Client, runner *toolRunner, tools []ToolDef, system string) *webController {
	return &webController{cfg: cfg, client: client, runner: runner, tools: tools, system: system, done: make(chan struct{})}
}

func (c *webController) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *webController) Address() string { return webAddressFor(c.cfg) }

// webPIDPath / webLogPath: the installer starts the web UI detached,
// so it leaves a PID file (and its log) in ~/.muse99. The menu and
// uninstall use the PID file to find and stop that process.
func webPIDPath() string { return filepath.Join(configDir(), "web.pid") }
func webLogPath() string { return filepath.Join(configDir(), "web.log") }

func writeWebPID() {
	_ = os.MkdirAll(configDir(), 0o700)
	_ = os.WriteFile(webPIDPath(), []byte(fmt.Sprint(os.Getpid())), 0o644)
}

func removeWebPID() { _ = os.Remove(webPIDPath()) }

// externalWebRunning reports whether a web UI is serving on this
// config's port in ANOTHER process (e.g. started by install.sh).
// It probes the status endpoint, so a stale PID file alone is not
// enough to claim the UI is up.
func externalWebRunning(cfg Config) bool {
	if pid := readWebPID(); pid > 0 && !processAlive(pid) {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", cfg.WebPort))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// anyWebRunning is true when this process runs the UI or another one does.
func (c *webController) anyWebRunning(cfg Config) bool {
	return c.IsRunning() || externalWebRunning(cfg)
}

// Start launches the web UI in the background (non-blocking).
// NOTE: this function unlocks c.mu part-way through (before the
// bind wait) — do NOT add a deferred Unlock here, it would unlock
// an already-unlocked mutex and panic.
func (c *webController) Start(cfg Config) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("web UI is already running at %s", webAddressFor(cfg))
	}
	s := &webServer{client: c.client, runner: c.runner, tools: c.tools, cfg: cfg, messages: []Message{{Role: "system", Content: c.system}}, system: c.system, db: appDB}
	if s.db != nil {
		if ch, err := s.db.DefaultChat(); err == nil {
			s.currentChatID = ch.ID
			if loaded, err := s.db.LoadMessages(ch.ID, 100); err == nil {
				s.messages = append(s.messages, loaded...)
			}
		}
	}
	mux := buildMux(s)
	addr := fmt.Sprintf("%s:%d", cfg.WebAddr, cfg.WebPort)
	c.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	c.cfg = cfg
	c.running = true
	certFile, keyFile, hasCerts := "", "", false
	if cfg.TLSEnabled {
		if cfg.CertFile != "" && cfg.KeyFile != "" {
			certFile, keyFile, hasCerts = cfg.CertFile, cfg.KeyFile, true
		} else {
			certFile, keyFile, hasCerts = findCerts(cfg.Domain)
		}
	}
	if cfg.TLSEnabled && !hasCerts {
		fmt.Println("Warning: TLS is on but no certificate was found — serving plain HTTP. Set up SSL from the menu first.")
	}
	// Unlock before waiting: the serve goroutine needs the lock to
	// report a bind failure, and Start must see that failure instead
	// of reading a stale `running=true` while holding the lock.
	c.mu.Unlock()
	go func() {
		var err error
		if cfg.TLSEnabled && hasCerts {
			fmt.Printf("Muse99 web UI: %s (TLS)\n", webAddressFor(cfg))
			err = c.srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			fmt.Printf("Muse99 web UI: %s\n", webAddressFor(cfg))
			err = c.srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			fmt.Println("Web server error:", err)
		}
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	// Give the listener a moment to bind so a bad port fails fast.
	time.Sleep(150 * time.Millisecond)
	// Leave a PID file so the menu / installer / uninstall can find
	// this process even after the parent shell exits. Only write it
	// when this server actually bound the port — if the port was
	// already taken, this process is exiting and must not claim it.
	if !c.IsRunning() {
		// The listener failed (usually "address already in use") —
		// report it instead of returning nil and hanging forever in
		// startWeb's <-done wait with no server running.
		return fmt.Errorf("could not listen on port %d (is another web UI already running there?)", cfg.WebPort)
	}
	writeWebPID()
	return nil
}

// Stop shuts the web UI down gracefully.
func (c *webController) Stop() error {
	c.mu.Lock()
	srv := c.srv
	wasRunning := c.running
	c.mu.Unlock()
	if !wasRunning || srv == nil {
		return fmt.Errorf("web UI is not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	removeWebPID()
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return err
}

// Restart stops the server (if running) and starts it again with the
// new config / system prompt.
func (c *webController) Restart(cfg Config, system string) error {
	c.system = system
	if c.IsRunning() {
		if err := c.Stop(); err != nil {
			return err
		}
		// Stop closed the done channel; make a fresh one for the next run.
		c.mu.Lock()
		c.done = make(chan struct{})
		c.mu.Unlock()
	}
	return c.Start(cfg)
}

func (s *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, webHTML)
}

func (s *webServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := "generic"
	if isSpark(s.cfg.BaseURL, s.cfg.Model) {
		mode = "Muse Spark — Muse mode"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": s.cfg.Model, "mode": mode, "user": s.cfg.UserName,
		"workdir": s.cfg.Workdir, "shell": s.runner.shellAllowed(),
		"base_url": s.cfg.BaseURL, "tasks": len(loadTasks()), "chats": s.chatCount(),
		"servers": len(loadServers()), "goals": len(loadGoals()),
		"provider": s.cfg.Provider, "address": webAddressFor(s.cfg),
		"port": s.cfg.WebPort, "domain": s.cfg.Domain, "tls": s.cfg.TLSEnabled,
		"api_key_set": s.cfg.APIKey != "" || s.client.APIKey != "",
		"usage":       usageTotalsJSON(),
	})
}

func (s *webServer) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Memory string `json:"memory"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = os.WriteFile(s.cfg.MemoryPath, []byte(body.Memory), 0o644)
		if s.db != nil {
			// Re-import the edited file so the DB and MEMORY.md agree.
			_, _ = s.db.ImportMemoriesFromFile(s.cfg.MemoryPath)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"memory": loadMemory(s.cfg.MemoryPath)}
	if s.db != nil {
		if mems, err := s.db.ListMemories(); err == nil {
			resp["memories"] = mems
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// chatCount is the number of persistent chats (0 without a DB).
func (s *webServer) chatCount() int {
	if s.db == nil {
		return 0
	}
	chats, _ := s.db.ListChats()
	return len(chats)
}

// loadCurrentChat switches the in-RAM messages to a chat's stored
// history (last 100 messages). It is a no-op without a DB.
func (s *webServer) loadCurrentChat(chatID string) {
	if s.db == nil {
		return
	}
	if chatID == "" {
		return
	}
	if _, err := s.db.GetChat(chatID); err != nil {
		return
	}
	loaded, err := s.db.LoadMessages(chatID, 100)
	if err != nil {
		return
	}
	s.currentChatID = chatID
	s.messages = []Message{{Role: "system", Content: s.system}}
	s.messages = append(s.messages, loaded...)
}

// handleChats lists / creates / renames / deletes chats.
// GET /api/chats[?q=...]  POST /api/chats {title}
// PUT /api/chats/<id> {title}  DELETE /api/chats/<id>
func (s *webServer) handleChats(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database is not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/chats/")
	id = strings.TrimSuffix(id, "/")
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case "GET":
		if q := r.URL.Query().Get("q"); q != "" {
			chats, _ := s.db.SearchChats(q)
			_ = json.NewEncoder(w).Encode(chats)
			return
		}
		chats, _ := s.db.ListChats()
		if chats == nil {
			chats = []Chat{}
		}
		_ = json.NewEncoder(w).Encode(chats)
	case "POST":
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		c, err := s.db.CreateChat(body.Title)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	case "PUT":
		if id == "" {
			http.Error(w, "need a chat id", http.StatusBadRequest)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := s.db.RenameChat(id, body.Title); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c, _ := s.db.GetChat(id)
		_ = json.NewEncoder(w).Encode(c)
	case "DELETE":
		if id == "" {
			http.Error(w, "need a chat id", http.StatusBadRequest)
			return
		}
		if err := s.db.DeleteChat(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.currentChatID == id {
			s.messages = []Message{{Role: "system", Content: s.system}}
			s.currentChatID = ""
			if ch, err := s.db.DefaultChat(); err == nil {
				s.loadCurrentChat(ch.ID)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleChatMessages routes /api/chats/<id>/... : a single chat's
// messages (GET), its summary (POST .../summarise), and the chat
// itself (GET / PUT rename / DELETE).
func (s *webServer) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database is not available", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/chats/")
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	w.Header().Set("Content-Type", "application/json")
	if len(parts) == 1 {
		switch r.Method {
		case "GET":
			c, err := s.db.GetChat(parts[0])
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(c)
			return
		case "PUT":
			var body struct {
				Title string `json:"title"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := s.db.RenameChat(parts[0], body.Title); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			c, _ := s.db.GetChat(parts[0])
			_ = json.NewEncoder(w).Encode(c)
			return
		case "DELETE":
			if err := s.db.DeleteChat(parts[0]); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if s.currentChatID == parts[0] {
				s.messages = []Message{{Role: "system", Content: s.system}}
				s.currentChatID = ""
				if ch, err := s.db.DefaultChat(); err == nil {
					s.loadCurrentChat(ch.ID)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
			return
		}
	}
	if len(parts) == 2 && parts[1] == "summarise" && r.Method == "POST" {
		n, err := summariseChat(s.client, s.db, parts[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"saved": n})
		return
	}
	if len(parts) == 2 && parts[1] == "messages" && r.Method == "GET" {
		msgs, _ := s.db.LoadMessages(parts[0], 100)
		if msgs == nil {
			msgs = []Message{}
		}
		_ = json.NewEncoder(w).Encode(msgs)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ---------- v4: model discovery ----------

// modelListResp is the JSON shape of /api/models.
type modelListResp struct {
	Models    []ModelInfo `json:"models"`
	Source    string      `json:"source"` // "live" | "cached" | "suggested"
	FetchedAt string      `json:"fetched_at,omitempty"`
	Provider  string      `json:"provider,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// handleModels serves GET /api/models (list models for a provider)
// and POST /api/models/refresh (force a fresh fetch). Query/body:
// provider (saved name), or base_url + api_key for an unsaved custom
// provider. The API key is never returned in the response.
func (s *webServer) handleModels(w http.ResponseWriter, r *http.Request) {
	force := r.Method == http.MethodPost
	var provider, baseURL, apiKey string
	if r.Method == http.MethodPost {
		var body struct {
			Provider string `json:"provider"`
			BaseURL  string `json:"base_url"`
			APIKey   string `json:"api_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		provider, baseURL, apiKey = body.Provider, body.BaseURL, body.APIKey
	} else if r.Method == http.MethodGet {
		q := r.URL.Query()
		provider, baseURL, apiKey = q.Get("provider"), q.Get("base_url"), q.Get("api_key")
		force = q.Get("refresh") == "1"
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base, key, name := resolveModelTarget(&s.cfg, provider, baseURL, apiKey)
	list, err := getModels(base, key, name, force)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// Still answer 200 with an error field (and any cached list
		// the fallback found) so the UI can show it inline.
		_ = json.NewEncoder(w).Encode(modelListResp{Models: []ModelInfo{}, Source: "cached", Provider: name, Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(modelListResp{Models: list.Models, Source: list.Source, FetchedAt: list.FetchedAt.Format(time.RFC3339), Provider: name})
}

// handleModelTest serves POST /api/models/test: send one tiny chat
// completion to a model and report whether it answers. Body:
// {provider|base_url, api_key, model}. The key is never echoed back.
func (s *webServer) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "need JSON body", http.StatusBadRequest)
		return
	}
	body.Model = normalizeModelID(body.Model)
	base, key, name := resolveModelTarget(&s.cfg, body.Provider, body.BaseURL, body.APIKey)
	reply, latency, err := testModel(base, key, body.Model, name)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "model": body.Model, "latency_ms": latency, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": body.Model, "latency_ms": latency, "reply": reply})
}

// handleModelActive serves POST /api/models/active: make a model the
// active model for a provider. Body: {provider|base_url, api_key,
// model}. The model id is normalised (a "models/..." prefix from
// Gemini's native list is stripped) before it is saved, and the live
// client is rebuilt so /api/status shows the new model at once.
func (s *webServer) handleModelActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "need JSON body", http.StatusBadRequest)
		return
	}
	model := normalizeModelID(body.Model)
	if model == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no model given"})
		return
	}
	name := strings.TrimSpace(body.Provider)
	if name == "" {
		name = s.cfg.Provider
	}
	// Save the model for this provider (keeping its existing key and
	// base URL when the request does not send new ones), then make
	// it the live model. Start from the LIVE provider entry, not
	// just the disk one: a custom provider (or one set up via env)
	// may not be in config.json yet, and starting from disk would
	// wipe its base URL and key.
	fc := loadFileConfig()
	p := Provider{Name: name}
	if live, ok := s.cfg.Providers[name]; ok {
		p = live
	}
	if saved, ok := fc.Providers[name]; ok {
		if p.BaseURL == "" {
			p.BaseURL = saved.BaseURL
		}
		if p.APIKey == "" {
			p.APIKey = saved.APIKey
		}
	}
	if body.BaseURL != "" {
		p.BaseURL = body.BaseURL
	}
	if p.BaseURL == "" {
		if preset, ok := providerPreset(name); ok {
			p.BaseURL = preset.BaseURL
		}
	}
	if body.APIKey != "" {
		p.APIKey = body.APIKey
	}
	p.Name = name
	p.Model = model
	fc.setProvider(p)
	fc.ActiveProvider = name
	_ = saveFileConfig(fc)
	active := fc.activeProvider()
	s.cfg.Provider = active.Name
	s.cfg.BaseURL = active.BaseURL
	s.cfg.Model = active.Model
	s.cfg.APIKey = active.APIKey
	if s.cfg.Providers == nil {
		s.cfg.Providers = map[string]Provider{}
	}
	s.cfg.Providers[name] = active
	*s.client = *NewClient(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": model, "provider": name})
}

// handleMemories lists / adds / edits / deletes individual memories.
func (s *webServer) handleMemories(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database is not available", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/memories/")
	id = strings.TrimSuffix(id, "/")
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case "GET":
		mems, _ := s.db.ListMemories()
		if mems == nil {
			mems = []Memory{}
		}
		_ = json.NewEncoder(w).Encode(mems)
	case "POST":
		var body struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, added, err := s.db.AddMemory(body.Text, body.Kind, s.currentChatID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"memory": m, "added": added})
	case "PUT":
		var body struct {
			Text string `json:"text"`
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := s.db.UpdateMemory(id, body.Text, body.Kind); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	case "DELETE":
		if err := s.db.DeleteMemory(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = s.db.ExportMemories()
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *webServer) handleClear(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil && s.currentChatID != "" {
		_ = s.db.ClearChat(s.currentChatID)
	}
	s.messages = []Message{{Role: "system", Content: s.system}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
}

func (s *webServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, "need {\"message\": \"...\"}", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// v5: no API key yet — the key is set in Settings, not on the
	// command line, so say so plainly instead of a raw API error.
	// (The live client is rebuilt from Settings, so check both.)
	if strings.TrimSpace(s.cfg.APIKey) == "" && strings.TrimSpace(s.client.APIKey) == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResp{Reply: noAPIKeyNotice, Tools: nil, ChatID: s.currentChatID})
		return
	}
	// Switch chats when the caller asks for one; otherwise keep the
	// current chat. Without a DB this is the old single-chat behaviour.
	if req.ChatID != "" && req.ChatID != s.currentChatID {
		s.loadCurrentChat(req.ChatID)
	}
	if s.db != nil && s.currentChatID == "" {
		if ch, err := s.db.DefaultChat(); err == nil {
			s.loadCurrentChat(ch.ID)
		}
	}
	before := len(s.messages)
	refreshSystemFor(&s.cfg, &s.messages, req.Message)
	reply, tools := runTurnDetailed(s.client, s.runner, s.tools, &s.messages, req.Message)
	persistTurn(s.currentChatID, before, s.messages)
	if s.db != nil && s.currentChatID != "" {
		chatID := s.currentChatID
		go func() {
			if n := maybeAutoSummarise(s.client, s.db, chatID); n > 0 {
				fmt.Printf("Auto-summary: saved %d new fact(s) to memory (chat %s)\n", n, chatID)
			}
		}()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(chatResp{Reply: reply, Tools: tools, ChatID: s.currentChatID})
}

// noAPIKeyNotice is what chat returns (and the UI shows) when no
// provider API key has been set yet. It points at Settings, where
// the key is added — there is no terminal setup step any more.
const noAPIKeyNotice = "No API key set yet — I can't chat until you add one.\n\nOpen Settings → Provider & model (the Settings tab above), pick your provider, paste your API key, press Fetch models, click a model to test it, then press Set active. Then come back here and chat."

func (s *webServer) handleTask(w http.ResponseWriter, r *http.Request) {
	// POST /api/task {goal} starts an autonomous task in the background.
	if r.Method == http.MethodPost {
		var body struct {
			Goal string `json:"goal"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.Goal) == "" {
			http.Error(w, "need goal", http.StatusBadRequest)
			return
		}
		go func() {
			_ = runAutonomous(s.client, s.runner, s.tools, s.system, body.Goal, s.cfg.Workdir)
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "started"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(loadTasks())
}

func (s *webServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = s.cfg.Workdir
	}
	full := safePath(s.cfg.Workdir, p)
	// Allow absolute workdir paths too.
	if filepath.IsAbs(p) {
		full = p
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type item struct {
		Name string `json:"name"`
		Dir  bool   `json:"dir"`
		Path string `json:"path"`
	}
	var out []item
	for _, e := range entries {
		out = append(out, item{Name: e.Name(), Dir: e.IsDir(), Path: filepath.Join(p, e.Name())})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"path": full, "files": out})
}

func (s *webServer) handleFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	data, err := os.ReadFile(safePath(s.cfg.Workdir, p))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": p, "content": truncate(string(data), 50000)})
}

func (s *webServer) handleServers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var srv Server
		_ = json.NewDecoder(r.Body).Decode(&srv)
		if srv.Host == "" || srv.User == "" {
			http.Error(w, "need host and user", http.StatusBadRequest)
			return
		}
		if srv.Port == 0 {
			srv.Port = 22
		}
		srv.Enabled = true
		servers := loadServers()
		servers = append(servers, srv)
		_ = saveServers(servers)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(loadServers())
}

func (s *webServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			FileConfig
			AllowShell *bool `json:"allow_shell"`
			TLSEnabled *bool `json:"tls_enabled"`
			Port       *int  `json:"port"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fc := body.FileConfig
		if body.Port != nil {
			fc.Port = *body.Port
		}
		// v6: a blank key field means "keep the key in use", not
		// "clear it". The key in use may come from env vars (folded
		// into the live config at startup, never written to
		// config.json) — saving Settings used to wipe it, which broke
		// providers set up that way. Keep the live key, and persist
		// it so it survives a restart.
		target := fc.ActiveProvider
		if target == "" {
			target = s.cfg.Provider
		}
		if fc.APIKey == "" {
			if p, ok := s.cfg.Providers[target]; ok && p.APIKey != "" {
				fc.APIKey = p.APIKey
			}
		}
		fc.Model = normalizeModelID(fc.Model)
		// Only update fields the user sent; keep the existing API key if blank.
		old := loadFileConfig()
		// Merge the live providers in: a custom provider (or one set
		// up via env) may not be in config.json yet, and saving
		// Settings must not wipe its base URL or key.
		for name, p := range s.cfg.Providers {
			if _, ok := old.Providers[name]; !ok {
				old.Providers[name] = p
			}
		}
		if fc.ActiveProvider != "" {
			// Switching provider: save any base/model/key sent with it,
			// then activate it so the live client uses the right key.
			if fc.BaseURL != "" || fc.Model != "" || fc.APIKey != "" {
				old.setProvider(Provider{Name: fc.ActiveProvider, BaseURL: fc.BaseURL, Model: fc.Model, APIKey: fc.APIKey})
			}
			old.ActiveProvider = fc.ActiveProvider
			p := old.activeProvider()
			s.cfg.Provider = p.Name
			s.cfg.BaseURL = p.BaseURL
			s.cfg.Model = p.Model
			s.cfg.APIKey = p.APIKey
			*s.client = *NewClient(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model)
		} else {
			if fc.BaseURL != "" {
				old.BaseURL = fc.BaseURL
				s.cfg.BaseURL = fc.BaseURL
			}
			if fc.APIKey != "" {
				old.APIKey = fc.APIKey
				s.cfg.APIKey = fc.APIKey
			}
			if fc.Model != "" {
				old.Model = fc.Model
				s.cfg.Model = fc.Model
			}
			if fc.BaseURL != "" || fc.APIKey != "" || fc.Model != "" {
				*s.client = *NewClient(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model)
			}
			// Keep the active provider's stored copy in sync.
			if s.cfg.Provider != "" {
				old.setProvider(Provider{Name: s.cfg.Provider, BaseURL: s.cfg.BaseURL, Model: s.cfg.Model, APIKey: s.cfg.APIKey})
			}
		}
		if body.AllowShell != nil {
			old.AllowShell = *body.AllowShell
			s.cfg.AllowShell = *body.AllowShell
			s.runner.SetAllowShell(*body.AllowShell)
		}
		if fc.UserName != "" {
			old.UserName = fc.UserName
			s.cfg.UserName = fc.UserName
		}
		if fc.Port != 0 {
			old.Port = fc.Port
			s.cfg.WebPort = fc.Port
		}
		if fc.Domain != "" {
			old.Domain = fc.Domain
			s.cfg.Domain = fc.Domain
		}
		// TLS is a bool: only change it when the field was sent.
		if body.TLSEnabled != nil {
			old.TLSEnabled = *body.TLSEnabled
			s.cfg.TLSEnabled = *body.TLSEnabled
		}
		// Keep the legacy top-level fields in sync with the active
		// provider, so GET /api/settings (and older readers) show
		// the same base URL / model / key that is actually in use.
		// (v7 fix: saving Settings used to update only
		// providers[active], leaving the top-level fields empty —
		// so the next GET showed a blank base URL and no model,
		// as if no API settings had ever been saved.)
		_ = saveFileConfig(syncTopLevel(old))
	}
	w.Header().Set("Content-Type", "application/json")
	fc := loadFileConfig()
	// v6: say where the key in use actually comes from. A key set via
	// env vars is folded into the live config at startup but is NOT
	// in config.json — Settings used to show "(not set)" for it, and
	// saving Settings wiped it. "saved" = in config.json, "env" =
	// from the environment / command line (press Save to keep it in
	// config.json), "none" = no key.
	keySource := func(name string) (masked, source string) {
		if k := fc.Providers[name].APIKey; k != "" {
			return maskKey(k), "saved"
		}
		if p, ok := s.cfg.Providers[name]; ok && p.APIKey != "" {
			return maskKey(p.APIKey), "env"
		}
		return "(not set)", "none"
	}
	active := fc.ActiveProvider
	activeMasked, activeSource := keySource(active)
	providers := []map[string]string{}
	for _, name := range providerPresetNames() {
		p := fc.Providers[name]
		masked, source := keySource(name)
		providers = append(providers, map[string]string{
			"name": name, "base_url": p.BaseURL, "model": p.Model,
			"api_key_masked": masked, "api_key_source": source,
		})
	}
	resp := map[string]any{
		// v7: base_url / model come from the ACTIVE provider, not
		// the legacy top-level fields — those are empty in any
		// config written by the providers flow, so Settings used
		// to show a blank base URL and "(none yet)" for the model
		// even when a provider and key were saved.
		"base_url": fc.activeProvider().BaseURL, "model": fc.activeProvider().Model, "user_name": fc.UserName,
		"api_key_masked": activeMasked, "api_key_source": activeSource,
		"config_path": configPath(),
		"provider":    fc.ActiveProvider, "providers": providers,
		"port": fc.Port, "domain": fc.Domain, "tls_enabled": fc.TLSEnabled,
		"allow_shell": fc.AllowShell, "usage": usageTotalsJSON(),
	}
	// Last-known model list for the active provider (from the DB
	// cache only — loading Settings never makes a network call).
	if appDB != nil {
		if saved, ok := appDB.LoadModelList(fc.activeProvider().BaseURL); ok {
			resp["models"] = saved.Models
			resp["models_source"] = saved.Source
			resp["models_fetched_at"] = saved.FetchedAt.Format(time.RFC3339)
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func buildMux(s *webServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/memory", s.handleMemory)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/task", s.handleTask)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/file", s.handleFile)
	mux.HandleFunc("/api/servers", s.handleServers)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/chats", s.handleChats)
	mux.HandleFunc("/api/chats/", s.handleChatMessages)
	mux.HandleFunc("/api/memories", s.handleMemories)
	mux.HandleFunc("/api/memories/", s.handleMemories)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/models/refresh", s.handleModels)
	mux.HandleFunc("/api/models/test", s.handleModelTest)
	mux.HandleFunc("/api/models/active", s.handleModelActive)
	return mux
}

// startWeb starts the web UI and blocks until it is stopped.
// (The SSH menu uses newWebController + Start/Stop/Restart instead.)
func startWeb(cfg Config, client *Client, runner *toolRunner, tools []ToolDef, system string) error {
	ctrl := newWebController(cfg, client, runner, tools, system)
	if err := ctrl.Start(cfg); err != nil {
		return err
	}
	<-ctrl.done
	return nil
}

const webHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Muse99</title>
<style>
:root{--bg:#fafaf9;--surface:#ffffff;--ink:#171717;--ink2:#525252;--muted:#a3a3a3;--line:#e7e5e4;--line2:#f0efed;--accent:#4f46e5;--accent-soft:#eef2ff;--accent-ink:#3730a3;--ok:#15803d;--bad:#b91c1c;--radius:16px;--shadow:0 1px 2px rgba(0,0,0,.04),0 4px 16px rgba(0,0,0,.04);--font:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{font-family:var(--font);background:var(--bg);color:var(--ink);font-size:15px;line-height:1.55;-webkit-font-smoothing:antialiased}
header{position:sticky;top:0;background:rgba(255,255,255,.9);backdrop-filter:blur(10px);border-bottom:1px solid var(--line);z-index:20}
.topbar{max-width:1180px;margin:0 auto;padding:10px 20px;display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.brand{display:flex;align-items:center;gap:8px;font-size:16px;font-weight:700;letter-spacing:-.02em}
.brand .mark{width:26px;height:26px;border-radius:8px;background:var(--ink);color:#fff;display:inline-flex;align-items:center;justify-content:center;font-size:14px}
.pill{display:inline-flex;align-items:center;gap:6px;background:var(--line2);color:var(--ink2);padding:4px 11px;border-radius:999px;font-size:12.5px;font-weight:500;max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pill.spark{background:var(--accent-soft);color:var(--accent-ink)}
.pill.model{background:#fff;border:1px solid var(--line);color:var(--ink)}
.spacer{flex:1}
.tabs{max-width:1180px;margin:0 auto;padding:0 20px;display:flex;gap:4px;overflow-x:auto}
.tabs button{background:none;border:0;color:var(--ink2);padding:11px 13px;cursor:pointer;font-size:14px;font-weight:500;white-space:nowrap;border-radius:10px 10px 0 0;border-bottom:2px solid transparent}
.tabs button:hover{color:var(--ink)}
.tabs button.active{color:var(--ink);border-bottom-color:var(--ink);font-weight:650}
.layout{display:flex;max-width:1180px;margin:0 auto;align-items:flex-start}
aside{width:264px;flex-shrink:0;padding:20px 12px 20px 20px;position:sticky;top:104px;height:calc(100vh - 104px);overflow-y:auto}
aside h3{font-size:11px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin:2px 4px 10px;font-weight:650}
aside input{margin-bottom:10px}
main{flex:1;min-width:0;max-width:820px;padding:22px 20px 170px}
.pane{display:none;animation:fade .18s ease}
.pane.active{display:block}
@keyframes fade{from{opacity:0;transform:translateY(3px)}to{opacity:1;transform:none}}
.msg{padding:11px 15px;border-radius:18px;line-height:1.6;white-space:pre-wrap;max-width:85%;margin-bottom:12px;font-size:15px;box-shadow:var(--shadow)}
.user{margin-left:auto;background:var(--ink);color:#fff;border-bottom-right-radius:6px}
.muse{background:var(--surface);border:1px solid var(--line);border-bottom-left-radius:6px}
.tools{font-size:12px;color:var(--muted);margin-top:6px}
.composer{position:fixed;left:0;right:0;bottom:0;background:linear-gradient(transparent,var(--bg) 30%);padding:22px 20px 16px;z-index:10}
.composer .row{max-width:820px;margin:0 auto;display:flex;gap:10px;background:var(--surface);border:1px solid var(--line);border-radius:999px;padding:5px 5px 5px 18px;box-shadow:var(--shadow)}
.composer input{flex:1;border:0;background:none;padding:10px 0;font-size:15px;outline:none}
.composer button{padding:10px 20px;border-radius:999px;border:0;background:var(--ink);color:#fff;font-weight:600;cursor:pointer;font-size:14px}
.composer button:hover{background:#000}
button{font-family:var(--font)}
.btn{display:inline-flex;align-items:center;gap:6px;padding:9px 16px;border-radius:999px;border:1px solid var(--line);background:var(--surface);color:var(--ink);font-weight:600;font-size:13.5px;cursor:pointer}
.btn:hover{border-color:var(--ink)}
.btn.primary{background:var(--ink);color:#fff;border-color:var(--ink)}
.btn.primary:hover{background:#000}
.btn.ghost{background:none;border:1px solid var(--line);color:var(--ink2);font-weight:500}
.btn:disabled{opacity:.5;cursor:wait}
input,textarea,select{padding:10px 13px;border-radius:12px;border:1px solid var(--line);background:var(--surface);font-size:14px;width:100%;font-family:var(--font);color:var(--ink);outline:none}
input:focus,textarea:focus,select:focus{border-color:var(--accent);box-shadow:0 0 0 3px var(--accent-soft)}
label{display:block;font-size:13px;color:var(--ink2);margin:16px 0 6px;font-weight:550}
label .hint{font-weight:400;color:var(--muted)}
pre{background:var(--surface);border:1px solid var(--line);padding:15px;border-radius:var(--radius);overflow:auto;white-space:pre-wrap;font-size:13px;box-shadow:var(--shadow)}
.card{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);padding:18px;margin-bottom:14px;box-shadow:var(--shadow)}
.card h3{margin:0 0 4px;font-size:16px;letter-spacing:-.01em}
.card p.sub{margin:0 0 6px;color:var(--ink2);font-size:13.5px}
h3.sec{font-size:17px;margin:6px 0 14px;letter-spacing:-.01em}
.note{font-size:13px;color:var(--muted);margin-top:10px;line-height:1.5}
.chatitem{display:flex;gap:6px;align-items:center;padding:9px 11px;border-radius:11px;cursor:pointer;font-size:14px;color:var(--ink2)}
.chatitem:hover{background:var(--line2);color:var(--ink)}
.chatitem.active{background:var(--accent-soft);color:var(--accent-ink);font-weight:600}
.chatitem span{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.chatitem button{background:none;border:0;color:var(--muted);cursor:pointer;padding:2px 5px;font-size:13px;border-radius:6px}
.chatitem button:hover{color:var(--ink)}
.shellbox{display:inline-flex;gap:7px;align-items:center;background:var(--surface);border:1px solid var(--line);border-radius:999px;padding:6px 13px;font-size:13px;color:var(--ink2);cursor:pointer;font-weight:500}
.shellbox input{width:auto;margin:0;accent-color:var(--accent)}
.shellbox.on{border-color:var(--ink);color:var(--ink);font-weight:600}
.iconbtn{background:none;border:1px solid var(--line);color:var(--ink2);border-radius:999px;padding:7px 13px;cursor:pointer;font-size:13px;font-weight:500}
.iconbtn:hover{color:var(--ink);border-color:var(--ink)}
.modelrow{display:flex;gap:8px;align-items:flex-start}
.modelrow input{flex:1}
.modelrow .btn{flex-shrink:0;margin-top:1px}
.modelitem{display:flex;gap:10px;align-items:center;padding:11px 13px;border:1px solid var(--line);border-radius:12px;margin-bottom:8px;cursor:pointer;background:var(--surface)}
.modelitem:hover{border-color:var(--ink)}
.modelitem.active{border-color:var(--accent);background:var(--accent-soft)}
.modelitem .mid{flex:1;min-width:0}
.modelitem .mid b{font-size:14px;word-break:break-all}
.modelitem .mid .sub{font-size:12.5px;color:var(--muted)}
.modelitem .res{font-size:12.5px;white-space:nowrap}
.modelitem .res.ok{color:var(--ok)}
.modelitem .res.bad{color:var(--bad)}
.modelitem .btn{padding:6px 12px;font-size:12.5px;flex-shrink:0}
.status{font-size:13px;margin-top:9px;min-height:19px;color:var(--ink2)}
.status.ok{color:var(--ok)}
.status.bad{color:var(--bad)}
.kv{font-size:13.5px;color:var(--ink2);line-height:1.9}
.kv b{color:var(--ink);font-weight:600}
.tag{display:inline-block;background:var(--accent-soft);color:var(--accent-ink);border-radius:6px;padding:1px 7px;font-size:12px;font-weight:600}
.empty{color:var(--muted);font-size:14px;padding:8px 2px}
@media (max-width:820px){.layout{flex-direction:column}aside{width:100%;position:static;height:auto;padding:14px 20px 4px;border-bottom:1px solid var(--line)}main{padding:16px 16px 170px}.msg{max-width:92%}.topbar{padding:10px 14px}.tabs{padding:0 14px}.pill.addr{display:none}}
</style></head><body>
<header><div class="topbar"><span class="brand"><span class="mark">✦</span>Muse99</span><span class="pill spark" id="mode">…</span><span class="pill model" id="model"></span><span class="pill addr" id="addr"></span><span class="pill" id="usage"></span><span class="spacer"></span><label class="shellbox" id="shellbox" title="Lets Muse99 run commands on this server. Only turn on in a folder you trust."><input type="checkbox" id="shell-toggle" onchange="toggleShell()"> Shell</label><button class="iconbtn" onclick="clearChat()">Clear chat</button></div>
<nav class="tabs"><button class="active" onclick="tab('chat',this)">Chat</button><button onclick="tab('files',this)">Files</button><button onclick="tab('tasks',this)">Tasks</button><button onclick="tab('memory',this)">Memory</button><button onclick="tab('servers',this)">Servers</button><button onclick="tab('settings',this)">Settings</button></nav></header>
<div class="layout"><aside id="sidebar"><h3>Chats</h3><input id="chat-search" placeholder="Search chats…" oninput="loadChats()"><button class="btn primary" style="width:100%;justify-content:center;margin-bottom:10px" onclick="newChat()">+ New chat</button><div id="chatlist"></div></aside><main>
<div id="pane-chat" class="pane active"><div id="nokey" class="card" style="display:none"><h3>No API key yet</h3><p class="sub">Muse99 can't chat until you add a provider API key. It only takes a minute, and it happens right here in the web UI — no terminal commands.</p><button class="btn primary" onclick="showSettingsTab()">Open Settings → Provider &amp; model</button></div><div id="chat"></div><div class="muted" id="thinking" style="display:none">Muse99 is working…</div></div>
<div id="pane-files" class="pane"><h3 class="sec">Files</h3><div id="filelist"></div><pre id="fileview">Click a file to view it.</pre></div>
<div id="pane-tasks" class="pane"><h3 class="sec">Autonomous tasks</h3><div class="card"><div class="modelrow"><input id="taskgoal" placeholder="Build a todo app in ./todo-app with tests…"><button class="btn primary" onclick="startTask()">Start task</button></div><p class="note">Muse99 plans the task, builds it, runs the tests, fixes failures, and only stops when it can say DONE.</p></div><div id="tasklist"></div></div>
<div id="pane-memory" class="pane"><h3 class="sec">Memory</h3><p class="note">Facts Muse99 remembers about you, with kind and source. They are used in every chat. Delete one here to forget it.</p><div id="memlist"></div><button class="btn" onclick="summariseChat()">Summarise current chat into memory</button><h3 class="sec" style="margin-top:24px">Memory file</h3><textarea id="mem" rows="12"></textarea><br><br><button class="btn primary" onclick="saveMemory()">Save memory file</button></div>
<div id="pane-servers" class="pane"><h3 class="sec">Servers</h3><p class="note">Give Muse99 access only if you want it to. Remote commands use your system ssh key.</p><div class="card"><div style="display:flex;gap:8px;flex-wrap:wrap"><input id="srv-name" placeholder="Name" style="flex:1;min-width:120px"><input id="srv-user" placeholder="user" style="flex:1;min-width:120px"><input id="srv-host" placeholder="host / IP" style="flex:1;min-width:140px"><input id="srv-port" placeholder="22" style="width:80px"></div><br><button class="btn primary" onclick="addServer()">Add server</button></div><div id="srvlist" style="margin-top:14px"></div></div>
<div id="pane-settings" class="pane"><h3 class="sec">Settings</h3>
<div class="card"><h3>Provider &amp; model</h3><p class="sub">Enter a provider and its API key, fetch its models, then click a model to test it and set it active — no typing model names.</p>
<label>Provider</label><select id="set-provider"></select>
<label>API base URL</label><input id="set-base" placeholder="API base URL">
<label>API key <span class="hint">— leave blank to keep the saved one</span></label><input id="set-key" placeholder="API key" type="password">
<div class="status" id="key-source" style="margin-top:6px"></div>
<div class="kv" style="margin-top:10px">Active model: <b id="active-model">…</b></div>
<div style="display:flex;gap:8px;margin-top:12px;flex-wrap:wrap"><button class="btn primary" id="fetch-models-btn" onclick="fetchModels()">Fetch models</button><button class="btn" id="test-all-btn" onclick="testAllModels()" disabled>Test all</button><button class="btn ghost" id="test-all-cancel" onclick="cancelTestAll()" style="display:none">Cancel</button></div>
<input id="model-filter" placeholder="Filter the model list…" oninput="renderModelList()" style="margin-top:10px">
<div id="model-list" style="margin-top:10px"></div>
<div class="status" id="model-status"></div>
<p class="note"><a href="#" onclick="showManualModel(event)">Fetch didn't return any models? Enter one manually</a></p>
<div id="manual-model-box" style="display:none;margin-top:8px"><div class="modelrow"><input id="manual-model" placeholder="e.g. gemini-2.5-flash" autocomplete="off"><button class="btn" onclick="saveManualModel()">Use this model</button></div></div>
<p class="note">Fetching asks the provider's <span style="font-family:monospace">/v1/models</span> endpoint with your key — the key is never shown or logged. Click a model (or its <b>Test</b> button) to send one tiny message ("Reply with the single word OK") and see whether it answers and how fast. <b>Set active</b> makes that model the one Muse99 chats with. <b>Test all</b> tests the first 20 models one by one. For Gemini, if the list endpoint is empty or refuses the key, Muse99 also tries Gemini's native model list — and strips the "models/" prefix, so the model it saves always works for chat.</p>
</div>
<div class="card"><h3>Web &amp; access</h3>
<label>Your name</label><input id="set-user" placeholder="Your name">
<label>Web port</label><input id="set-port" placeholder="3759" type="number">
<label>Domain <span class="hint">— optional, for the web link</span></label><input id="set-domain" placeholder="muse99.example.com">
<label style="display:flex;gap:8px;align-items:center"><input id="set-tls" type="checkbox" style="width:auto"> SSL / HTTPS on</label>
<label style="display:flex;gap:8px;align-items:center"><input id="set-shell" type="checkbox" style="width:auto"> Allow shell commands</label>
<p class="note">Lets Muse99 run commands on this server. Only turn on in a folder you trust. Takes effect immediately — no restart needed.</p>
</div>
<div class="card" id="usagebox"><h3>Token usage</h3><span id="usage-detail">Loading…</span><div class="kv" id="settings" style="margin-top:10px"></div></div>
<br><button class="btn primary" onclick="saveSettings()">Save settings</button>
<p class="note">Saved to ~/.muse99/config.json (600 permissions). Changing the port or SSL needs a restart of the web UI — do it from the SSH menu (Restart web UI).</p></div>
</main></div>
<form id="f" class="composer"><div class="row"><input id="in" autocomplete="off" placeholder="Ask Muse99 to do something…"><button>Send</button></div></form>
<script>
const chat=document.getElementById('chat'),inp=document.getElementById('in'),think=document.getElementById('thinking');
document.getElementById('addr').textContent=window.location.href;
function showSettingsTab(){document.querySelectorAll('.tabs button')[5].click();}
function tab(name,btn){document.querySelectorAll('.pane').forEach(p=>p.classList.remove('active'));document.getElementById('pane-'+name).classList.add('active');document.querySelectorAll('.tabs button').forEach(b=>b.classList.remove('active'));btn.classList.add('active');if(name==='files')loadFiles('');if(name==='tasks')loadTasks();if(name==='memory'){loadMemory();loadMemories();}if(name==='servers')loadServers();if(name==='settings')loadSettings();}
function add(t,cls,tools){const d=document.createElement('div');d.className='msg '+cls;d.textContent=t;if(tools&&tools.length){const x=document.createElement('div');x.className='tools';x.textContent='Tools: '+tools.join(' · ');d.appendChild(x);}chat.appendChild(d);window.scrollTo(0,document.body.scrollHeight);}
add("Hey Takahashi, I'm Muse99 in your browser. Give me a task and I'll work it till the end — files, shell, tests, and verification.","muse");
function fmtUsage(u){if(!u||!u.calls)return 'No tokens used yet';return u.prompt_tokens.toLocaleString()+' in / '+u.completion_tokens.toLocaleString()+' out · est. $'+Number(u.estimated_cost_usd).toFixed(4)+' (Spark rates)';}
loadStatus();
let currentChatId=null;
async function loadChats(){const q=document.getElementById('chat-search').value.trim();const r=await fetch('/api/chats'+(q?'?q='+encodeURIComponent(q):''));const j=await r.json();const list=document.getElementById('chatlist');list.innerHTML=(j||[]).map(c=>'<div class="chatitem'+(c.id===currentChatId?' active':'')+'" onclick="openChat(\''+c.id+'\')"><span>'+escapeHtml(c.title||'Chat')+'</span><button title="Rename" onclick="event.stopPropagation();renameChat(\''+c.id+'\',\''+escapeHtml(c.title||'').replace(/'/g,'')+'\')">✎</button><button title="Delete" onclick="event.stopPropagation();deleteChat(\''+c.id+'\')">✕</button></div>').join('')||'<div class="empty">(no chats yet)</div>';}
function escapeHtml(t){return String(t).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
async function openChat(id){currentChatId=id;const r=await fetch('/api/chats/'+id+'/messages');const j=await r.json();chat.innerHTML='';(j||[]).forEach(m=>{if(m.role==='user')add(m.content,'user');else if(m.role==='assistant')add(m.content,'muse');});loadChats();}
async function newChat(){const r=await fetch('/api/chats',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:'New chat'})});const j=await r.json();currentChatId=j.id;chat.innerHTML='';add("New chat — what should we do?","muse");loadChats();}
async function renameChat(id,title){const t=prompt('Rename chat',title);if(!t)return;await fetch('/api/chats/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:t})});loadChats();}
async function deleteChat(id){if(!confirm('Delete this chat and all its messages?'))return;await fetch('/api/chats/'+id,{method:'DELETE'});if(currentChatId===id){currentChatId=null;chat.innerHTML='';}loadChats();}
async function loadMemories(){const r=await fetch('/api/memories');const j=await r.json();document.getElementById('memlist').innerHTML=(j||[]).map(m=>'<div class="card"><b>['+escapeHtml(m.kind||'fact')+']</b> '+escapeHtml(m.text)+' <span class="muted">'+(m.created_at||'').slice(0,10)+'</span> <button class="btn ghost" onclick="deleteMemory(\''+m.id+'\')">Delete</button></div>').join('')||'<div class="empty">(no memories saved yet)</div>';}
async function deleteMemory(id){await fetch('/api/memories/'+id,{method:'DELETE'});loadMemories();}
async function summariseChat(){const r=await fetch('/api/chats/'+(currentChatId||'')+'/summarise',{method:'POST'});const j=await r.json();alert('Saved '+(j.saved||0)+' new fact(s) to memory.');loadMemories();}
async function toggleShell(){const on=document.getElementById('shell-toggle').checked;await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({allow_shell:on})});document.getElementById('shellbox').classList.toggle('on',on);loadStatus();}
async function loadStatus(){const r=await fetch('/api/status');const s=await r.json();document.getElementById('mode').textContent=s.mode;document.getElementById('model').textContent=s.model+' · '+s.user;if(s.address)document.getElementById('addr').textContent=s.address;document.getElementById('usage').textContent=fmtUsage(s.usage);const nk=document.getElementById('nokey');if(nk){nk.style.display=s.api_key_set?'none':'block';}const t=document.getElementById('shell-toggle');if(t){t.checked=!!s.shell;document.getElementById('shellbox').classList.toggle('on',!!s.shell);}}
loadChats();
document.getElementById('f').addEventListener('submit',async e=>{e.preventDefault();const v=inp.value.trim();if(!v)return;add(v,'user');inp.value='';think.style.display='block';try{const r=await fetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:v,chat_id:currentChatId})});const j=await r.json();if(j.chat_id)currentChatId=j.chat_id;think.style.display='none';add(j.reply||j.error||'(no reply)','muse',j.tools);}catch(err){think.style.display='none';add('Could not reach server: '+err,'muse');}});
async function clearChat(){await fetch('/api/clear',{method:'POST'});chat.innerHTML='';add('Chat cleared. Memory is kept.','muse');}
async function loadFiles(p){const r=await fetch('/api/files?path='+encodeURIComponent(p||''));const j=await r.json();document.getElementById('filelist').innerHTML=j.files.map(f=>'<div class="card" style="cursor:pointer;padding:12px 16px" onclick="viewFile(\''+f.path.replace(/'/g,'')+'\','+f.dir+')">'+(f.dir?'📁 ':'📄 ')+f.name+'</div>').join('')||'<div class="empty">(empty)</div>';}
async function viewFile(p,isDir){if(isDir){loadFiles(p);return;}const r=await fetch('/api/file?path='+encodeURIComponent(p));const j=await r.json();document.getElementById('fileview').textContent=j.content||'(empty)';}
async function loadTasks(){const r=await fetch('/api/task');const j=await r.json();document.getElementById('tasklist').innerHTML=j.map(t=>'<div class="card"><b>'+t.goal+'</b><br>Status: '+t.status+' · '+t.id+'<br>'+(t.summary||'')+'</div>').join('')||'<div class="empty">(no tasks yet)</div>';}
async function startTask(){const g=document.getElementById('taskgoal').value.trim();if(!g)return;await fetch('/api/task',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({goal:g})});document.getElementById('taskgoal').value='';setTimeout(loadTasks,1000);add('Started autonomous task: '+g,'muse');}
async function loadMemory(){const r=await fetch('/api/memory');const j=await r.json();document.getElementById('mem').value=j.memory||'';}
async function saveMemory(){await fetch('/api/memory',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({memory:document.getElementById('mem').value})});alert('Saved');}
async function loadServers(){const r=await fetch('/api/servers');const j=await r.json();document.getElementById('srvlist').innerHTML=j.map(s=>'<div class="card">'+s.name+' — '+s.user+'@'+s.host+':'+s.port+'</div>').join('')||'<div class="empty">(no servers — Muse99 only uses servers you add here)</div>';}
async function addServer(){const b={name:document.getElementById('srv-name').value,user:document.getElementById('srv-user').value,host:document.getElementById('srv-host').value,port:parseInt(document.getElementById('srv-port').value||'22')};await fetch('/api/servers',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});loadServers();}
// --- model discovery (v6): clickable list, click-to-test, set active ---
// No model text input any more: Fetch models, click a model to test
// it, press Set active. The only typing is the hidden manual fallback.
let allModels=[];
let modelResults={}; // id -> {ok, latency_ms, reply, error, testing}
let activeModel='';
let testAllRunning=false;
let testAllCancel=false;
const TEST_ALL_LIMIT=20;
function modelStatus(msg,cls){const el=document.getElementById('model-status');el.textContent=msg;el.className='status'+(cls?' '+cls:'');}
function providerFields(){return {provider:document.getElementById('set-provider').value,base_url:document.getElementById('set-base').value,api_key:document.getElementById('set-key').value};}
function fillModelList(models,source,fetchedAt){allModels=(models||[]).map(m=>({id:m.id,owned_by:m.owned_by||''}));modelResults={};renderModelList();document.getElementById('test-all-btn').disabled=!allModels.length;if(source){const when=fetchedAt?new Date(fetchedAt).toLocaleString():'';modelStatus(allModels.length+' model'+(allModels.length===1?'':'s')+' · '+(source==='live'?'live from provider':source==='suggested'?'suggested models (provider list was empty)':'cached'+(when?' · last fetched '+when:''))+(source==='cached'?' — press Fetch models to refresh':''),'');}}
function renderModelList(){const q=document.getElementById('model-filter').value.trim().toLowerCase();const shown=allModels.filter(m=>!q||m.id.toLowerCase().includes(q)||(m.owned_by||'').toLowerCase().includes(q));const list=document.getElementById('model-list');if(!shown.length){list.innerHTML='<div class="empty">'+(allModels.length?'No models match "'+escapeHtml(q)+'".':'No models yet — press Fetch models.')+'</div>';return;}list.innerHTML=shown.map(m=>{const r=modelResults[m.id]||{};const isActive=m.id===activeModel;let res='';if(r.testing)res='<span class="res">Testing…</span>';else if(r.ok)res='<span class="res ok">✓ '+r.latency_ms+' ms · "'+escapeHtml(r.reply||'')+'"</span>';else if(r.error)res='<span class="res bad">✗ '+escapeHtml(r.error)+'</span>';return '<div class="modelitem'+(isActive?' active':'')+'" data-id="'+escapeHtml(m.id)+'"><div class="mid"><b>'+escapeHtml(m.id)+'</b>'+(isActive?' <span class="tag">ACTIVE</span>':'')+'<div class="sub">'+escapeHtml(m.owned_by||'')+'</div></div>'+res+'<button class="btn" data-act="test" data-id="'+escapeHtml(m.id)+'">Test</button><button class="btn primary" data-act="active" data-id="'+escapeHtml(m.id)+'">Set active</button></div>';}).join('');if(q)modelStatus(shown.length+' of '+allModels.length+' models match "'+q+'"','');}
document.getElementById('model-list').addEventListener('click',e=>{const btn=e.target.closest('[data-act]');const row=e.target.closest('.modelitem');const id=btn?btn.dataset.id:(row?row.dataset.id:null);if(!id)return;if(btn&&btn.dataset.act==='active'){setActiveModel(id);return;}testOneModel(id);});
async function refreshModelList(){const p=document.getElementById('set-provider').value;try{const r=await fetch('/api/models?provider='+encodeURIComponent(p));const j=await r.json();if(j.error){modelStatus(j.error,'bad');if(j.models&&j.models.length)fillModelList(j.models,'cached','');return;}fillModelList(j.models,j.source,j.fetched_at);}catch(e){modelStatus('Could not load models: '+e,'bad');}}
async function fetchModels(){const btn=document.getElementById('fetch-models-btn');btn.disabled=true;modelStatus('Fetching models from provider…','');try{const b=providerFields();const r=await fetch('/api/models/refresh',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});const j=await r.json();if(j.error){modelStatus(j.error,'bad');}else{fillModelList(j.models,j.source,j.fetched_at);if(!j.models||!j.models.length)modelStatus('Provider returned no models — use "Enter one manually" below, or try another provider.','bad');}}catch(e){modelStatus('Fetch failed: '+e,'bad');}btn.disabled=false;}
async function testOneModel(id){if(modelResults[id]&&modelResults[id].testing)return;modelResults[id]={testing:true};renderModelList();try{const b=Object.assign(providerFields(),{model:id});const r=await fetch('/api/models/test',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});const j=await r.json();if(j.ok){modelResults[id]={ok:true,latency_ms:j.latency_ms,reply:j.reply};}else{modelResults[id]={ok:false,error:j.error||'failed'};}}catch(e){modelResults[id]={ok:false,error:String(e)};}renderModelList();}
async function setActiveModel(id){try{const b=Object.assign(providerFields(),{model:id});const r=await fetch('/api/models/active',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});const j=await r.json();if(j.ok){activeModel=j.model;document.getElementById('active-model').textContent=j.model;modelStatus('✓ '+j.model+' is now the active model for '+j.provider+'.','ok');renderModelList();loadStatus();}else{modelStatus('Could not set active model: '+(j.error||'unknown error'),'bad');}}catch(e){modelStatus('Set active failed: '+e,'bad');}}
async function testAllModels(){if(testAllRunning)return;const list=allModels.slice(0,TEST_ALL_LIMIT);if(!list.length){modelStatus('Fetch models first.','bad');return;}testAllRunning=true;testAllCancel=false;document.getElementById('test-all-btn').disabled=true;document.getElementById('test-all-cancel').style.display='';let done=0,ok=0;for(const m of list){if(testAllCancel)break;await testOneModel(m.id);done++;if(modelResults[m.id]&&modelResults[m.id].ok)ok++;await new Promise(r=>setTimeout(r,150));}testAllRunning=false;document.getElementById('test-all-btn').disabled=false;document.getElementById('test-all-cancel').style.display='none';modelStatus('Tested '+done+' model'+(done===1?'':'s')+' (first '+TEST_ALL_LIMIT+') — '+ok+' work'+(testAllCancel?' (cancelled)':''),ok?'ok':'');}
function cancelTestAll(){testAllCancel=true;}
function showManualModel(e){e.preventDefault();document.getElementById('manual-model-box').style.display='block';document.getElementById('manual-model').focus();}
async function saveManualModel(){const m=document.getElementById('manual-model').value.trim().replace(/^models\//,'');if(!m){modelStatus('Type a model name first.','bad');return;}await setActiveModel(m);document.getElementById('manual-model-box').style.display='none';}
let settingsData=null;
async function loadSettings(){const r=await fetch('/api/settings');const j=await r.json();settingsData=j;document.getElementById('settings').innerHTML='Base: '+(j.base_url||'')+'<br>Model: '+(j.model||'')+'<br>User: '+(j.user_name||'')+'<br>Key: '+(j.api_key_masked||'(not set)')+' ('+(j.api_key_source||'none')+')<br>Config: '+j.config_path;document.getElementById('usage-detail').textContent=fmtUsage(j.usage)+(j.usage&&j.usage.last_label?' — last: '+j.usage.last_prompt_tokens.toLocaleString()+' in / '+j.usage.last_completion_tokens.toLocaleString()+' out ('+j.usage.last_label+')':'')+' · '+((j.usage&&j.usage.calls)||0)+' API calls';const sel=document.getElementById('set-provider');sel.innerHTML=(j.providers||[]).map(p=>'<option value="'+p.name+'">'+p.name+' — '+p.model+'</option>').join('');sel.value=j.provider||'spark';document.getElementById('set-base').value=j.base_url||'';activeModel=j.model||'';document.getElementById('active-model').textContent=j.model||'(none yet — fetch models and set one active)';document.getElementById('key-source').textContent='Key in use: '+(j.api_key_masked||'(not set)')+(j.api_key_source==='env'?' — from the environment, press Save settings to keep it in config.json':j.api_key_source==='saved'?' — saved in config.json':'');document.getElementById('set-user').value=j.user_name||'';document.getElementById('set-port').value=j.port||3759;document.getElementById('set-domain').value=j.domain||'';document.getElementById('set-tls').checked=!!j.tls_enabled;document.getElementById('set-shell').checked=!!j.allow_shell;sel.onchange=()=>{const p=(j.providers||[]).find(x=>x.name===sel.value);if(p){document.getElementById('set-base').value=p.base_url||'';activeModel=p.model||'';document.getElementById('active-model').textContent=p.model||'(none yet — fetch models and set one active)';document.getElementById('key-source').textContent='Key in use: '+(p.api_key_masked||'(not set)')+(p.api_key_source==='env'?' — from the environment, press Save settings to keep it in config.json':p.api_key_source==='saved'?' — saved in config.json':'');}refreshModelList();};if(j.models&&j.models.length){fillModelList(j.models,j.models_source||'cached',j.models_fetched_at);}else{modelStatus('No models fetched yet for this provider — press Fetch models.','');}}
async function saveSettings(){const b={active_provider:document.getElementById('set-provider').value,base_url:document.getElementById('set-base').value,api_key:document.getElementById('set-key').value,user_name:document.getElementById('set-user').value,port:parseInt(document.getElementById('set-port').value||'3759'),domain:document.getElementById('set-domain').value,tls_enabled:document.getElementById('set-tls').checked,allow_shell:document.getElementById('set-shell').checked};await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});alert('Saved — restart the web UI from the SSH menu if you changed the port or SSL.');loadSettings();loadStatus();}
</script></body></html>
`
