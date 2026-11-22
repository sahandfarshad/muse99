package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	auth          *authStore
	tlsActive     bool
	desktopToken  string // v0.10.0: set in the desktop app only
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
	// v0.8: tokens this reply used and the model that answered.
	Usage *usageDelta `json:"usage,omitempty"`
	// v0.9.13: this chat's tokens since it began.
	ChatTokens *usageDelta `json:"chat_tokens,omitempty"`
	// v0.9.16: what the model said between tool steps this turn.
	Interim []string `json:"interim,omitempty"`
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
// v9: the listener is bound FIRST (net.Listen) and the "Muse99 web
// UI: ..." line is printed only after the bind succeeds. Before
// this, the line was printed inside the serve goroutine before
// ListenAndServe ran, so a process that failed to bind (port already
// taken) still left a "Muse99 web UI: ..." line in the log — and
// install.sh greps for exactly that line, so it reported
// "Web UI restarted" for a web UI that never started, while the old
// process kept serving the old version.
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
	s.auth = newAuthStore()
	s.desktopToken = cfg.DesktopToken
	addr := fmt.Sprintf("%s:%d", cfg.WebAddr, cfg.WebPort)
	c.srv = &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	c.cfg = cfg
	certFile, keyFile, hasCerts := "", "", false
	if cfg.TLSEnabled {
		if cfg.CertFile != "" && cfg.KeyFile != "" {
			certFile, keyFile, hasCerts = cfg.CertFile, cfg.KeyFile, true
		} else {
			certFile, keyFile, hasCerts = findCerts(cfg.Domain)
		}
	}
	var reloader *certReloader
	if cfg.TLSEnabled && hasCerts {
		r, err := newCertReloader(certFile, keyFile)
		if err != nil {
			fmt.Println("Warning: TLS certificate could not be loaded (" + err.Error() + ") — serving plain HTTP. Run SSL setup (menu option 7) again.")
			hasCerts = false
		} else {
			reloader = r
			c.srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
		}
	}
	s.tlsActive = cfg.TLSEnabled && hasCerts
	c.srv.Handler = buildHandler(s)
	if !loginConfigured() && cfg.DesktopToken == "" {
		fmt.Println("Warning: no web login is set — the web UI stays locked. Set one with menu option 15.")
	}
	if cfg.TLSEnabled && !hasCerts {
		fmt.Println("Warning: TLS is on but no certificate was found — serving plain HTTP. Set up SSL from the menu first.")
	}
	// Bind now, while holding the lock: a failure here is immediate
	// and synchronous, so Start can report it instead of a goroutine
	// failing later with the "started" line already printed.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("could not listen on port %d (is another web UI already running there?)", cfg.WebPort)
	}
	c.running = true
	c.mu.Unlock()
	if cfg.TLSEnabled && hasCerts {
		fmt.Printf("Muse99 web UI: %s (TLS)\n", webAddressFor(cfg))
	} else {
		fmt.Printf("Muse99 web UI: %s\n", webAddressFor(cfg))
	}
	go func() {
		var err error
		if cfg.TLSEnabled && hasCerts {
			_ = reloader
			err = c.srv.ServeTLS(ln, "", "")
		} else {
			err = c.srv.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			fmt.Println("Web server error:", err)
		}
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()
	// Leave a PID file so the menu / installer / uninstall can find
	// this process even after the parent shell exits. Only written
	// after a successful bind — a process that could not listen
	// must not claim the port.
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
	syncUserName(&s.cfg)
	recent, fallback := s.modelMenuJSON()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"recent": recent, "fallback": fallback, "pace": int(callPace() / time.Second), "compact": compactAt(), "thinking": thinkingKeep(),
		"model": s.cfg.Model, "mode": mode, "user": s.cfg.UserName,
		"name_needed": !s.cfg.NameSet, "version": versionLabel(),
		"workdir": s.cfg.Workdir, "shell": s.runner.shellAllowed(),
		"base_url": s.cfg.BaseURL, "tasks": len(loadTasks()), "chats": s.chatCount(),
		"servers": len(loadServers()), "goals": len(loadGoals()),
		"provider": s.cfg.Provider, "address": webAddressFor(s.cfg),
		"port": s.cfg.WebPort, "domain": s.cfg.Domain, "tls": s.cfg.TLSEnabled,
		"api_key_set": s.cfg.APIKey != "" || s.client.APIKey != "",
		"desktop":     s.desktopToken != "", "os": runtime.GOOS,
		"usage": usageTotalsJSON(),
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
	chat, err := s.db.GetChat(chatID)
	if err != nil {
		return
	}
	var loaded []Message
	if chat.SummaryUpTo > 0 {
		stored, err := s.db.loadMessagesAfter(chatID, chat.SummaryUpTo)
		if err != nil {
			return
		}
		if len(stored) > 100 {
			stored = stored[len(stored)-100:]
		}
		loaded = summaryMessages(chat.Summary)
		for _, sm := range stored {
			loaded = append(loaded, Message{Role: sm.Role, Content: sm.Content, ToolCalls: sm.ToolCalls})
		}
	} else if loaded, err = s.db.LoadMessages(chatID, 100); err != nil {
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
	// v0.8: a key / base URL that just fetched models successfully is
	// saved under that provider's name at once (without switching the
	// active provider), so it is still there after switching away.
	if err == nil && list.Source == "live" && strings.TrimSpace(provider) != "" && validProviderName(strings.TrimSpace(provider)) && (apiKey != "" || baseURL != "") {
		fc := loadFileConfig()
		fc.ensureProviders()
		n := strings.TrimSpace(provider)
		p := fc.Providers[n]
		p.Name = n
		if baseURL != "" {
			p.BaseURL = strings.TrimSpace(baseURL)
		}
		if apiKey != "" {
			p.APIKey = apiKey
		}
		fc.Providers[n] = p
		_ = saveFileConfig(fc)
		if s.cfg.Providers == nil {
			s.cfg.Providers = map[string]Provider{}
		}
		s.cfg.Providers[n] = mergedProvider(&s.cfg, n)
	}
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
	if !validProviderName(name) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "provider name: letters, digits, - _ . only (max 40)"})
		return
	}
	// Save the model for this provider (keeping its existing key and
	// base URL when the request does not send new ones), then make
	// it the live model. Start from the LIVE provider entry, not
	// just the disk one: a custom provider (or one set up via env)
	// may not be in config.json yet, and starting from disk would
	// wipe its base URL and key.
	fc := loadFileConfig()
	p := mergedProvider(&s.cfg, name)
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
	fc.noteRecentModel(name, model)
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
	*s.client = *NewClientForProvider(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider)
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
	setActivityChat(s.currentChatID)
	defer setActivityChat("")
	ctl.begin()
	liveBegin(s.currentChatID)
	usageBefore := loadUsage()
	if s.db != nil && s.currentChatID != "" {
		probe := append(append([]Message{}, s.messages...), Message{Role: "user", Content: req.Message})
		if maybeCompactChat(s.client, s.db, s.currentChatID, probe) {
			s.loadCurrentChat(s.currentChatID)
		}
	}
	before := len(s.messages)
	memCtx := refreshSystemFor(&s.cfg, &s.messages, req.Message)
	reply, tools := runTurnWithContext(s.client, s.fallbackClient(), s.runner, s.tools, &s.messages, req.Message, memCtx)
	var turnUsage *usageDelta
	if d := diffUsage(usageBefore, loadUsage()); d.Calls > 0 {
		if d.Model == "" {
			d.Model = s.client.Model
		}
		turnUsage = &d
	}
	persistTurn(s.currentChatID, before, s.messages)
	var chatTokens *usageDelta
	if s.db != nil && s.currentChatID != "" && turnUsage != nil {
		if ch, err := s.db.AddChatUsage(s.currentChatID, *turnUsage); err == nil {
			chatTokens = &ch.Tokens
		}
	}
	if s.db != nil && s.currentChatID != "" {
		chatID := s.currentChatID
		go func() {
			if n := maybeAutoSummarise(s.client, s.db, chatID); n > 0 {
				fmt.Printf("Auto-summary: saved %d new fact(s) to memory (chat %s)\n", n, chatID)
			}
		}()
	}
	w.Header().Set("Content-Type", "application/json")
	_, interim := liveTexts()
	_ = json.NewEncoder(w).Encode(chatResp{Reply: reply, Tools: tools, ChatID: s.currentChatID, Usage: turnUsage, ChatTokens: chatTokens, Interim: interim})
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
		// The task runs in the background; create it here so the id
		// can be returned to the browser, which shows live progress.
		task := addTask(body.Goal)
		ctl.begin()
		go func() {
			_ = runAutonomous(s.client, s.runner, s.tools, s.system, body.Goal, s.cfg.Workdir, task.ID)
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "started", "task_id": task.ID})
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
	full, ok := pathInWorkdir(s.cfg.Workdir, p)
	if !ok {
		http.Error(w, "path is outside the workdir", http.StatusForbidden)
		return
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
	full, ok := pathInWorkdir(s.cfg.Workdir, p)
	if !ok {
		http.Error(w, "path is outside the workdir", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": p, "content": truncate(string(data), 50000)})
}

func (s *webServer) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var srv Server
		_ = json.NewDecoder(r.Body).Decode(&srv)
		srv.Name = strings.TrimSpace(srv.Name)
		srv.Host = strings.TrimSpace(srv.Host)
		srv.User = strings.TrimSpace(srv.User)
		if srv.Host == "" || srv.User == "" {
			http.Error(w, "need host and user", http.StatusBadRequest)
			return
		}
		if srv.Name == "" {
			srv.Name = srv.Host
		}
		if srv.Port == 0 {
			srv.Port = 22
		}
		srv.Enabled = true
		servers := loadServers()
		replaced := false
		for i, old := range servers {
			if old.Name == srv.Name {
				if srv.Password == "" {
					srv.Password = old.Password
				}
				servers[i] = srv
				replaced = true
			}
		}
		if !replaced {
			servers = append(servers, srv)
		}
		if err := saveServers(servers); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		var keep []Server
		for _, sv := range loadServers() {
			if sv.Name != name {
				keep = append(keep, sv)
			}
		}
		_ = saveServers(keep)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(publicServers())
}

// publicServers is the server list without passwords.
func publicServers() []Server {
	out := []Server{}
	for _, sv := range loadServers() {
		sv.HasPassword = sv.Password != ""
		sv.Password = ""
		out = append(out, sv)
	}
	return out
}

// handleServerKey serves GET /api/servers/key: Muse99's public key and
// the one-line command that installs it on a server.
func (s *webServer) handleServerKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, pub, err := museKey()
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "public_key": pub, "command": keyInstallCommand(pub)})
}

// handleServerAction serves POST /api/servers/test and
// /api/servers/installkey {name}.
func (s *webServer) handleServerAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var srv Server
	found := false
	for _, sv := range loadServers() {
		if sv.Name == body.Name {
			srv, found = sv, true
		}
	}
	if !found {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "no such server"})
		return
	}
	cmd := "echo connected as $(whoami) on $(hostname); uname -sr"
	if strings.HasSuffix(r.URL.Path, "/installkey") {
		if srv.Password == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "add the password first, or run the key command on the server yourself"})
			return
		}
		_, pub, err := museKey()
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		cmd = keyInstallCommand(pub)
	}
	activity.add("servers", "tool", "SSH "+srv.Name, cmd, false)
	out, err := runRemote(srv, cmd, 30*time.Second)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error(), "output": out})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "output": strings.TrimSpace(out)})
}

func (s *webServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			FileConfig
			AllowShell *bool `json:"allow_shell"`
			ChatSteps  *int  `json:"chat_steps"`
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
		if fc.ActiveProvider != "" && !validProviderName(fc.ActiveProvider) {
			http.Error(w, "provider name: letters, digits, - _ . only (max 40)", http.StatusBadRequest)
			return
		}
		if fc.APIKey == "" && target != "" {
			if k := mergedProvider(&s.cfg, target).APIKey; k != "" {
				fc.APIKey = k
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
			*s.client = *NewClientForProvider(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider)
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
				*s.client = *NewClientForProvider(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model, s.cfg.Provider)
			}
			// Keep the active provider's stored copy in sync.
			if s.cfg.Provider != "" {
				old.setProvider(Provider{Name: s.cfg.Provider, BaseURL: s.cfg.BaseURL, Model: s.cfg.Model, APIKey: s.cfg.APIKey})
			}
		}
		if body.ChatSteps != nil && *body.ChatSteps > 0 {
			old.ChatSteps = clampSteps(*body.ChatSteps)
			setChatStepLimit(old.ChatSteps)
		}
		if body.AllowShell != nil {
			old.AllowShell = *body.AllowShell
			s.cfg.AllowShell = *body.AllowShell
			s.runner.SetAllowShell(*body.AllowShell)
		}
		if n := cleanUserName(fc.UserName); n != "" {
			old.UserName = n
			old.NameSet = true
			s.cfg.UserName = n
			s.cfg.NameSet = true
			scrubLegacyName(n)
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
		// v0.8: keep the live providers in step with what was saved.
		if s.cfg.Providers == nil {
			s.cfg.Providers = map[string]Provider{}
		}
		for n := range loadFileConfig().Providers {
			s.cfg.Providers[n] = mergedProvider(&s.cfg, n)
		}
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
	for _, name := range providerNamesFor(fc) {
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
		"base_url": fc.activeProvider().BaseURL, "model": fc.activeProvider().Model, "user_name": func() string { n, _ := fc.savedUserName(); return n }(),
		"version":        versionLabel(),
		"api_key_masked": activeMasked, "api_key_source": activeSource,
		"config_path": configPath(),
		"provider":    fc.ActiveProvider, "providers": providers,
		"port": fc.Port, "domain": fc.Domain, "tls_enabled": fc.TLSEnabled,
		"allow_shell": fc.AllowShell, "chat_steps": chatStepLimit(), "usage": usageTotalsJSON(),
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

// buildHandler is the production handler: the API mux behind the
// login (auth.go). Tests that exercise the API directly use buildMux.
func buildHandler(s *webServer) http.Handler {
	if s.auth == nil {
		s.auth = newAuthStore()
	}
	return s.withAuth(buildMux(s))
}

func buildMux(s *webServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/models/fallback", s.handleModelFallback)
	mux.HandleFunc("/api/pace", s.handlePace)
	mux.HandleFunc("/api/compact", s.handleCompact)
	mux.HandleFunc("/api/thinking", s.handleThinking)
	mux.HandleFunc("/api/prices", s.handlePrices)
	mux.HandleFunc("/api/chat/live", s.handleLive)
	mux.HandleFunc("/api/control", s.handleControl)
	mux.HandleFunc("/api/db", s.handleDB)
	mux.HandleFunc("/api/servers/key", s.handleServerKey)
	mux.HandleFunc("/api/servers/test", s.handleServerAction)
	mux.HandleFunc("/api/servers/installkey", s.handleServerAction)
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
	mux.HandleFunc("/api/local/detect", s.handleLocalDetect)
	mux.HandleFunc("/api/workdir", s.handleWorkdir)
	mux.HandleFunc("/api/workdir/pick", s.handlePickFolder)
	mux.HandleFunc("/api/desktop/quit", s.handleDesktopQuit)
	mux.HandleFunc("/api/activity", s.handleActivity)
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
:root{--bg:#f7f5f1;--surface:#fffdfa;--ink:#1d1b18;--ink2:#5d5850;--muted:#9a948a;--line:#e9e4dc;--line2:#f0ece5;--glow:#e8dccb;--accent:#1d1b18;--accent-soft:#f0ece5;--accent-ink:#1d1b18;--ok:#3d7a52;--bad:#b0452a;--warn:#b0602a;--radius:14px;--r-lg:22px;--shadow:0 1px 2px rgba(40,30,20,.04),0 8px 28px rgba(40,30,20,.06);--font:ui-sans-serif,-apple-system,BlinkMacSystemFont,"SF Pro Text","Segoe UI",Roboto,Helvetica,Arial,sans-serif;--mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;--side:264px}
@media (prefers-color-scheme:dark){:root{--bg:#161513;--surface:#1f1d1a;--ink:#f1ece4;--ink2:#b9b2a7;--muted:#7d776d;--line:#2e2b27;--line2:#27241f;--glow:#4a3f30;--accent:#f1ece4;--accent-soft:#2a2722;--accent-ink:#f1ece4;--shadow:0 1px 2px rgba(0,0,0,.3),0 8px 28px rgba(0,0,0,.25)}}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{font-family:var(--font);background:var(--bg);color:var(--ink);font-size:15px;line-height:1.55;-webkit-font-smoothing:antialiased}
::selection{background:var(--glow)}
/* sidebar */
aside{position:fixed;left:0;top:0;bottom:0;width:var(--side);padding:18px 14px 14px;display:flex;flex-direction:column;gap:6px;border-right:1px solid var(--line);background:linear-gradient(180deg,var(--surface),var(--bg));z-index:30}
.brand{display:flex;align-items:center;gap:10px;padding:4px 8px 14px;font-weight:650;font-size:17px;letter-spacing:-.01em}
.orb{width:26px;height:26px;border-radius:50%;flex:none;background:radial-gradient(circle at 30% 30%,#fff 0,#f1e6d6 35%,#c9b49a 100%);box-shadow:inset 0 0 0 1px rgba(0,0,0,.06),0 2px 8px rgba(150,120,80,.25)}
.newchat{display:flex;align-items:center;gap:8px;width:100%;padding:10px 12px;border-radius:var(--radius);background:var(--surface);border:1px solid var(--line);box-shadow:var(--shadow);font-weight:560;color:var(--ink);font-size:14px;cursor:pointer;text-align:left}
.newchat:hover{border-color:var(--ink2)}
#chat-search{margin:8px 0 2px;background:var(--line2);border-color:transparent}
.sect{font-size:11.5px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);padding:12px 10px 4px}
#chatlist{flex:1;overflow-y:auto;min-height:60px;margin:0 -4px;padding:0 4px}
.chatitem{display:flex;gap:4px;align-items:center;padding:8px 10px;border-radius:10px;cursor:pointer;font-size:14px;color:var(--ink2)}
.chatitem:hover{background:var(--line2);color:var(--ink)}
.chatitem.active{background:var(--surface);color:var(--ink);box-shadow:var(--shadow)}
.chatitem span{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.chatitem button{background:none;border:0;color:var(--muted);cursor:pointer;padding:2px 5px;font-size:12px;border-radius:6px;opacity:0;transition:opacity .15s}
.chatitem:hover button,.chatitem.active button{opacity:1}
.chatitem button:hover{color:var(--ink)}
.tabs{display:flex;flex-direction:column;gap:2px;border-top:1px solid var(--line);padding-top:10px;margin-top:6px}
.tabs button{display:flex;gap:10px;align-items:center;padding:8px 10px;border-radius:10px;background:none;border:0;color:var(--ink2);font-size:14px;cursor:pointer;text-align:left;font-family:var(--font)}
.tabs button i{font-style:normal;width:18px;text-align:center;opacity:.7}
.tabs button:hover{background:var(--line2);color:var(--ink)}
.tabs button.active{background:var(--line2);color:var(--ink);font-weight:560}
.me{display:flex;align-items:center;gap:10px;padding:10px 8px 0;font-size:13.5px;color:var(--ink2)}
.av{width:28px;height:28px;border-radius:50%;background:var(--ink);color:var(--bg);display:grid;place-items:center;font-size:12px;font-weight:600}
.me a{margin-left:auto;color:var(--muted);text-decoration:none;font-size:13px}.me a:hover{color:var(--ink)}
/* top bar */
.page{margin-left:var(--side);min-height:100vh}
header{position:sticky;top:0;z-index:20;background:color-mix(in srgb,var(--bg) 86%,transparent);backdrop-filter:blur(12px)}
.topbar{display:flex;gap:10px;align-items:center;padding:14px 28px;flex-wrap:wrap}
#pagetitle{font-weight:600;font-size:15px;margin-right:6px}
select.pill{appearance:auto;cursor:pointer;font:inherit;font-size:12.5px;color:var(--ink2);background:var(--surface);border:1px solid var(--line);padding:4px 8px;max-width:240px}
.pill{display:inline-flex;align-items:center;gap:6px;padding:5px 11px;border-radius:999px;background:var(--surface);border:1px solid var(--line);font-size:12.5px;color:var(--ink2);max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.pill.model::before{content:"";width:7px;height:7px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 3px rgba(61,122,82,.15);flex:none}
.pill.spark{background:none;border-color:transparent;color:var(--muted)}
.spacer{flex:1}
.shellbox{display:inline-flex;gap:8px;align-items:center;font-size:12.5px;color:var(--ink2);cursor:pointer;user-select:none}
.shellbox input{appearance:none;-webkit-appearance:none;width:34px;height:20px;margin:0;padding:0;border:0;border-radius:999px;background:#d9d3c9;position:relative;cursor:pointer;transition:background .2s}
.shellbox input::after{content:"";position:absolute;top:2px;left:2px;width:16px;height:16px;border-radius:50%;background:#fff;box-shadow:0 1px 3px rgba(0,0,0,.2);transition:left .2s}
.shellbox input:checked{background:var(--warn)}.shellbox input:checked::after{left:16px}
.shellbox input:focus{box-shadow:0 0 0 3px var(--glow)}
.iconbtn{background:none;border:1px solid transparent;color:var(--ink2);border-radius:999px;padding:6px 12px;cursor:pointer;font-size:12.5px;font-family:var(--font)}
.iconbtn:hover{border-color:var(--line);color:var(--ink)}
/* content */
main{max-width:1180px;margin:0 auto;padding:10px 40px 200px}
.opts{display:contents}.optbtn{display:none}
.pane{display:none;animation:fade .2s ease}
.pane.active{display:block}
@keyframes fade{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:none}}
#chat{display:flex;flex-direction:column}
.msg .ln{unicode-bidi:plaintext;text-align:start;min-height:1em}
.msg .tools,.msg .tok{direction:ltr;unicode-bidi:isolate}
.msg{line-height:1.65;white-space:pre-wrap;margin:14px 0;font-size:15px;overflow-wrap:anywhere}
.msg.user{align-self:flex-end;max-width:min(78%,760px);background:var(--ink);color:var(--bg);padding:11px 16px;border-radius:20px 20px 6px 20px}
.msg.muse{position:relative;padding-left:38px;max-width:min(100%,860px);align-self:flex-start}
.msg.muse::before{content:"";position:absolute;left:0;top:1px;width:24px;height:24px;border-radius:50%;background:radial-gradient(circle at 30% 30%,#fff 0,#f1e6d6 35%,#c9b49a 100%);box-shadow:inset 0 0 0 1px rgba(0,0,0,.06),0 2px 8px rgba(150,120,80,.25)}
.ver{font-size:11px;font-weight:500;color:var(--muted);margin-left:6px;letter-spacing:.02em}
.tok{display:block;font-size:11.5px;color:var(--muted);margin-top:6px}
.modal{position:fixed;inset:0;background:rgba(0,0,0,.35);display:flex;align-items:center;justify-content:center;z-index:50;padding:16px}
.modal .card{max-width:380px;width:100%;margin:0}
.utable{width:100%;border-collapse:collapse;font-size:13px;margin-top:10px}
.utable th,.utable td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line)}
.utable th{color:var(--muted);font-weight:560}
.utable td.n{text-align:right;font-variant-numeric:tabular-nums}
.tools{display:inline-block;font-size:12px;color:var(--muted);margin-top:8px;background:var(--line2);padding:3px 10px;border-radius:999px;white-space:normal}
.chatuse{font-size:12px;color:var(--ink2);white-space:nowrap}.chatuse:empty{display:none}.runctl{display:none;justify-content:center;gap:10px;margin:0 auto 10px}.runctl.show{display:flex}.runctl .btn{padding:6px 16px;border-radius:999px}.composer .runctl button{width:auto;height:auto;font-size:13.5px;font-weight:560;background:var(--surface);color:var(--ink);border:1px solid var(--line);padding:7px 18px;border-radius:999px;box-shadow:var(--shadow)}.composer .runctl button::after{content:none}.runctl .btn.stop{color:var(--bad);border-color:var(--bad)}
#thinking{display:none;align-items:center;gap:10px;margin:14px 0 14px 38px;padding:12px 16px;background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);box-shadow:var(--shadow);font-size:13.5px;color:var(--ink2);width:fit-content}
#thinking::before{content:"";width:12px;height:12px;border-radius:50%;border:2px solid var(--glow);border-top-color:var(--ink);animation:spin .8s linear infinite;display:inline-block;margin-right:8px;vertical-align:-1px}
@keyframes spin{to{transform:rotate(360deg)}}
code,pre{font-family:var(--mono)}
/* composer */
.composer{position:fixed;left:var(--side);right:0;bottom:0;padding:24px 28px 22px;background:linear-gradient(180deg,transparent,var(--bg) 42%);z-index:10;margin:0}
.composer .row{max-width:1100px;margin:0 auto;display:flex;align-items:flex-end;gap:10px;background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);padding:10px 10px 10px 18px;box-shadow:0 2px 4px rgba(40,30,20,.04),0 16px 40px rgba(40,30,20,.10);transition:border-color .2s}
.composer .row:focus-within{border-color:var(--ink2)}
.composer input{flex:1;border:0;background:none;padding:10px 0;font-size:15px;outline:none;box-shadow:none!important}
.composer button{width:38px;height:38px;flex:none;border-radius:50%;border:0;background:var(--ink);color:var(--bg);font-size:0;cursor:pointer;position:relative}
.composer button::after{content:"↑";font-size:17px;font-weight:600;position:absolute;inset:0;display:grid;place-items:center}
.composer button:hover{opacity:.88}
.composer .hint{text-align:center;font-size:11.5px;color:var(--muted);margin-top:9px}
.composer.hide{display:none}
/* controls */
button{font-family:var(--font)}
.btn{display:inline-flex;align-items:center;gap:6px;padding:8px 15px;border-radius:999px;border:1px solid var(--line);background:var(--surface);color:var(--ink);font-weight:560;font-size:13.5px;cursor:pointer;transition:border-color .15s,opacity .15s}
.btn:hover{border-color:var(--ink2)}
.btn.primary{background:var(--ink);color:var(--bg);border-color:var(--ink)}
.btn.primary:hover{opacity:.88}
.btn.ghost{background:none;color:var(--ink2);font-weight:500}
.btn:disabled{opacity:.45;cursor:wait}
input,textarea,select{padding:10px 13px;border-radius:10px;border:1px solid var(--line);background:var(--line2);font-size:14px;width:100%;font-family:var(--font);color:var(--ink);outline:none;transition:border-color .15s,box-shadow .15s,background .15s}
input:focus,textarea:focus,select:focus{border-color:var(--ink2);background:var(--surface);box-shadow:0 0 0 3px var(--glow)}
textarea{font-family:var(--mono);font-size:13px;line-height:1.55}
label{display:block;font-size:13px;color:var(--ink2);margin:16px 0 6px;font-weight:550}
label .hint{font-weight:400;color:var(--muted)}
pre{background:var(--surface);border:1px solid var(--line);padding:16px;border-radius:var(--radius);overflow:auto;white-space:pre-wrap;font-size:12.5px;box-shadow:var(--shadow)}
a{color:var(--ink)}
.card{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);padding:18px 20px;margin-bottom:14px;box-shadow:var(--shadow)}
.card h3{margin:0 0 4px;font-size:14.5px;letter-spacing:-.005em}
.card p.sub{margin:0 0 8px;color:var(--muted);font-size:13px}
h3.sec{font-size:26px;letter-spacing:-.02em;font-weight:650;margin:14px 0 6px}
h3.sec+.note{margin:0 0 20px;font-size:14px;color:var(--ink2)}
.note{font-size:13px;color:var(--muted);margin-top:10px;line-height:1.55}
.modelrow{display:flex;gap:8px;align-items:flex-start}
.modelrow input{flex:1}
.modelrow .btn{flex-shrink:0;margin-top:2px}
.modelitem{display:flex;gap:10px;align-items:center;padding:10px 13px;border:1px solid var(--line);border-radius:10px;margin-bottom:6px;cursor:pointer;background:var(--surface);transition:border-color .15s}
.modelitem:hover{border-color:var(--ink2)}
.modelitem.active{border-color:var(--ink);box-shadow:0 0 0 3px var(--glow)}
.modelitem .mid{flex:1;min-width:0}
.modelitem .mid b{font-size:13.5px;font-weight:600;word-break:break-all}
.modelitem .mid .sub{font-size:12px;color:var(--muted)}
.modelitem .res{font-size:12px;overflow-wrap:anywhere;max-width:300px;text-align:right}
.modelitem .res.ok{color:var(--ok)}
.modelitem .res.bad{color:var(--bad)}
.modelitem .btn{padding:5px 11px;font-size:12px;flex-shrink:0}
.taskbar{max-width:704px;margin:4px auto 0;padding:10px 16px;border:1px solid var(--line);border-radius:var(--radius);background:var(--surface);box-shadow:var(--shadow);cursor:pointer;font-size:13.5px}
.taskbar.bad{border-color:var(--bad);color:var(--bad)}
.status{font-size:13px;margin-top:9px;min-height:19px;color:var(--ink2)}
.status.ok{color:var(--ok)}
.status.bad{color:var(--bad)}
.kv{font-size:13.5px;color:var(--ink2);line-height:1.9}
.kv b{color:var(--ink);font-weight:600}
.tag{display:inline-block;background:var(--ink);color:var(--bg);border-radius:999px;padding:1px 8px;font-size:11px;font-weight:650;letter-spacing:.02em}
.empty{color:var(--muted);font-size:14px;padding:8px 2px}
#filelist .card,#tasklist .card,#memlist .card,#srvlist .card{padding:12px 16px;margin-bottom:8px;font-size:14px}
#filelist .card:hover{border-color:var(--ink2)}
.check{display:flex!important;gap:10px;align-items:center;font-weight:500}
.check input{width:auto;margin:0;accent-color:var(--ink)}
.savebar{position:sticky;top:58px;z-index:15;display:flex;align-items:center;gap:14px;margin:0 0 14px;padding:10px 14px;border:1px solid var(--line);border-radius:var(--radius);background:color-mix(in srgb,var(--surface) 92%,transparent);backdrop-filter:blur(10px);box-shadow:var(--shadow)}
#toast{position:fixed;left:50%;bottom:110px;transform:translateX(-50%) translateY(10px);background:var(--ink);color:var(--bg);padding:10px 18px;border-radius:999px;font-size:13.5px;box-shadow:var(--shadow);opacity:0;pointer-events:none;transition:opacity .25s,transform .25s;z-index:60;max-width:80vw;text-align:center}
#toast.show{opacity:1;transform:translateX(-50%)}
#toast.bad{background:var(--bad);color:#fff}
#actpanel{position:fixed;top:0;right:0;bottom:0;width:400px;max-width:92vw;background:var(--surface);border-left:1px solid var(--line);box-shadow:-8px 0 30px rgba(0,0,0,.08);z-index:40;display:flex;flex-direction:column;transform:translateX(100%);transition:transform .22s ease}
#actpanel.open{transform:none}
#actpanel .ahead{display:flex;align-items:center;gap:8px;padding:14px 16px;border-bottom:1px solid var(--line)}
#actpanel .ahead b{flex:1;font-size:14px}
#actnow{padding:10px 16px;font-size:12.5px;color:var(--ink2);border-bottom:1px solid var(--line2);min-height:18px}
#actlist{flex:1;overflow-y:auto;padding:8px 10px 20px}
.aev{border:1px solid var(--line2);border-radius:10px;padding:8px 10px;margin:6px 0;font-size:12.5px;background:var(--bg)}
.aev .at{display:flex;gap:6px;align-items:baseline}
.aev .at span.t{flex:1;font-weight:600;color:var(--ink)}
.aev .at span.m{color:var(--muted);font-size:11.5px;white-space:nowrap}
.aev pre{white-space:pre-wrap;word-break:break-word;margin:6px 0 0;font-size:11.5px;max-height:220px;overflow:auto;color:var(--ink2);font-family:ui-monospace,Menlo,monospace}
.aev.pending{border-color:var(--glow)}
.aev.pending .at span.t::after{content:"";display:inline-block;width:9px;height:9px;margin-left:7px;border-radius:50%;border:2px solid var(--glow);border-top-color:var(--ink);animation:spin .8s linear infinite;vertical-align:-1px}
.aev.bad{border-color:var(--bad)}.aev.bad .at span.t{color:var(--bad)}
.aev.retry .at span.t{color:var(--warn)}
.aev .src{font-size:10.5px;color:var(--muted);text-transform:uppercase;letter-spacing:.04em}
@media (min-width:1200px){body.actopen main{margin-right:400px;margin-left:0}body.actopen .composer{right:400px}}
@media (max-width:860px){:root{--side:0px}aside{position:static;width:auto;border-right:0;border-bottom:1px solid var(--line);padding:12px 14px}#chatlist{max-height:150px}.tabs{flex-direction:row;overflow-x:auto;border-top:0;padding-top:4px}.tabs button{white-space:nowrap}.me{display:none}.topbar{padding:10px 14px}.pill.addr,.pill.spark{display:none}main{padding:8px 16px 190px}.composer{left:0;padding:20px 14px 14px}.msg.user{max-width:90%}.msg.muse{max-width:100%}.optbtn{display:inline-block;border-color:var(--line)}.opts{display:none;width:100%;flex-wrap:wrap;gap:8px}.opts.show{display:flex}.opts select.pill{max-width:100%;flex:1 1 140px}#pagetitle{margin-right:0}}
</style></head><body><div id="toast"></div><div id="actpanel"><div class="ahead"><b>Behind the scenes</b><button class="iconbtn" onclick="clearActivity()" title="Clear the list">Clear</button><button class="iconbtn" onclick="toggleActivity()" title="Close">✕</button></div><div id="actnow">Idle.</div><div id="actlist"><div class="empty">Nothing yet. Send a message and every step shows up here: waiting for the model, its thinking, tool runs and their output.</div></div></div>
<div id="namebox" class="modal" style="display:none"><div class="card"><h3>What should I call you?</h3><p class="sub">I'll use this name from now on. You can change it any time in Settings.</p><input id="name-in" dir="auto" placeholder="Your name" autocomplete="given-name" maxlength="60"><div class="savebar" style="margin-top:12px"><button class="btn primary" onclick="saveFirstName()">Continue</button></div></div></div>
<aside id="sidebar">
<div class="brand"><span class="orb"></span>Muse99<span class="ver" id="ver"></span></div>
<button class="newchat" onclick="newChat()">✎&nbsp; New chat</button>
<input id="chat-search" dir="auto" placeholder="Search chats" oninput="loadChats()">
<div class="sect">Chats</div>
<div id="chatlist"></div>
<nav class="tabs"><button class="active" id="tab-chat" onclick="tab('chat',this)"><i>◌</i>Chat</button><button onclick="tab('files',this)"><i>▤</i>Files</button><button onclick="tab('tasks',this)"><i>◎</i>Tasks</button><button onclick="tab('memory',this)"><i>✦</i>Memory</button><button onclick="tab('db',this)"><i>▤</i>Database</button><button onclick="tab('servers',this)"><i>⌁</i>Servers</button><button onclick="tab('settings',this)"><i>⚙</i>Settings</button></nav>
<div class="me"><span class="av">✦</span><span id="me-name">Muse99</span><a href="/logout">Log out</a></div>
</aside>
<div class="page">
<header><div class="topbar"><span id="pagetitle">Chat</span><span class="pill chatuse" id="chatuse" style="cursor:pointer" onclick="openPrices()" title="Tokens (and cost) this chat used since it began, and what the last message used. Click to set model prices."></span><span class="pill spark" id="mode">…</span><span class="spacer"></span><button class="iconbtn optbtn" id="optbtn" onclick="toggleOpts()" title="Show model, backup, pace, summary and other options">⚙ Options</button><div class="opts" id="opts"><select class="pill model" id="model" onchange="pickModel(this.value)" title="Model for this chat. Switch any time: the chat and its steps stay, the next message goes to the new model."></select><select class="pill" id="backup" onchange="pickBackup(this.value)" title="Backup model: used automatically when the main model still fails after retries."></select><select class="pill" id="pace" onchange="pickPace(this.value)" title="Wait this long between model calls. Use 3-10 s for free models with a per-minute limit (e.g. OpenRouter :free)."><option value="0">Pace: off</option><option value="1">Pace: 1s</option><option value="2">Pace: 2s</option><option value="3">Pace: 3s</option><option value="5">Pace: 5s</option><option value="10">Pace: 10s</option><option value="15">Pace: 15s</option><option value="20">Pace: 20s</option><option value="30">Pace: 30s</option><option value="60">Pace: 60s</option></select><select class="pill" id="compact" onchange="pickCompact(this.value)" title="When the chat gets this long, Muse99 summarises the older part and sends only the summary plus the latest messages. Saves tokens on long tasks. The whole chat stays on screen."><option value="0">Summary: off</option><option value="20000">Summary at 20k</option><option value="40000">Summary at 40k</option><option value="80000">Summary at 80k</option><option value="150000">Summary at 150k</option></select><select class="pill" id="thinkkeep" onchange="pickThinking(this.value)" title="Send the model's own earlier thinking back to it. This message (default): only while it works on your latest message, so models like GLM keep their thread between tool steps. All: every earlier thought (more tokens). Off: never."><option value="turn">Thinking: this message</option><option value="all">Thinking: all</option><option value="off">Thinking: off</option></select><span class="pill addr" id="addr"></span><span class="pill" id="usage"></span><label class="shellbox" id="shellbox" title="Lets Muse99 run commands on this server. Only turn on in a folder you trust.">Shell <input type="checkbox" id="shell-toggle" onchange="toggleShell()"></label></div><button class="iconbtn" id="actbtn" onclick="toggleActivity()" title="See what Muse99 is doing step by step">◧ Behind the scenes</button><button class="iconbtn" onclick="clearChat()">Clear</button></div></header>
<div id="taskbar" class="taskbar" style="display:none" onclick="showTasksTab()"></div>
<main>
<div id="pane-chat" class="pane active"><div id="nokey" class="card" style="display:none;margin-top:18px"><h3>Add an API key to start</h3><p class="sub">Muse99 needs a provider key before it can chat. It takes a minute, right here — no terminal.</p><button class="btn primary" onclick="showSettingsTab()">Open Settings</button></div><div id="chat"></div><div id="thinking">Muse99 is working…</div></div>
<div id="pane-files" class="pane"><h3 class="sec">Files</h3><p class="note">Everything in your work folder. Click a file to read it. Change the folder in Settings &gt; Work folder.</p><div id="filelist"></div><pre id="fileview">Click a file to view it.</pre></div>
<div id="pane-tasks" class="pane"><h3 class="sec">Tasks</h3><p class="note">Give it a goal. It plans, builds, tests, fixes, and only stops when it can say DONE.</p><div class="card"><div class="modelrow"><input id="taskgoal" dir="auto" placeholder="Build a todo app in ./todo-app with tests…"><button class="btn primary" onclick="startTask()">Start task</button></div></div><div id="tasklist"></div></div>
<div id="pane-db" class="pane"><h3 class="sec">Database</h3><p class="note">Everything Muse99 keeps in its database. Open a section to see its entries, click one to read it, and delete what you don't want. Deleting a chat also deletes its messages and its Behind the scenes history.</p><div id="dbpath" class="sub"></div><div id="dbbuckets"></div><div id="dbbrowse" style="display:none;margin-top:18px"><div class="savebar" style="margin-bottom:10px"><b id="dbname"></b><input id="dbq" dir="auto" placeholder="Search in this section…" style="flex:1;margin:0 10px" onkeydown="if(event.key==='Enter')dbOpen(dbCur)"><button class="btn" onclick="dbOpen(dbCur)">Search</button><button class="btn" id="dbclearall" onclick="dbClearAll()" style="display:none">Delete all</button></div><div id="dbentries"></div></div><div id="dbview" class="card" style="display:none;margin-top:14px"><div class="savebar"><b id="dbviewkey" style="overflow-wrap:anywhere"></b><button class="btn" onclick="document.getElementById('dbview').style.display='none'">Close</button></div><pre id="dbviewval" style="white-space:pre-wrap;max-height:420px;overflow:auto;unicode-bidi:plaintext"></pre></div></div><div id="pane-memory" class="pane"><h3 class="sec">Memory</h3><p class="note">What Muse99 remembers about you, used in every chat. Delete one to forget it.</p><div id="memlist"></div><button class="btn" onclick="summariseChat()">Summarise this chat into memory</button><div class="card" style="margin-top:22px"><h3>Memory file</h3><p class="sub">MEMORY.md, exported from the database.</p><textarea id="mem" dir="auto" rows="12"></textarea><div class="savebar" style="margin-top:12px"><button class="btn primary" onclick="saveMemory()">Save memory file</button></div></div></div>
<div id="pane-servers" class="pane"><h3 class="sec">Servers</h3><p class="note">Add a server and Muse99 can run commands there for you (just say "on myserver, do …"). Login with a password, or better with Muse99's key (below).</p><div class="card"><div style="display:flex;gap:8px;flex-wrap:wrap"><input id="srv-name" placeholder="Name (e.g. myserver)" style="flex:1;min-width:120px"><input id="srv-user" placeholder="user (e.g. root)" style="flex:1;min-width:110px"><input id="srv-host" placeholder="host / IP" style="flex:1;min-width:140px"><input id="srv-port" placeholder="22" style="width:70px"><input id="srv-pass" type="password" autocomplete="new-password" placeholder="password (optional)" style="flex:1;min-width:150px"></div><div class="savebar" style="margin-top:12px"><button class="btn primary" onclick="addServer()">Save server</button></div></div><div class="card" style="margin-top:14px"><b>Muse99's key (safer than a password)</b><p class="note">Log in to your new server once (e.g. with PuTTY or ssh) and paste this one line there. Nothing to copy back. After that Muse99 can log in without the password. Or press "Install key" on a server that has a password.</p><pre id="srv-keycmd" style="white-space:pre-wrap;word-break:break-all;font-size:12px;background:var(--surface);padding:10px;border-radius:8px;border:1px solid var(--line)">…</pre><button class="btn" onclick="copyKeyCmd()">Copy command</button></div><div id="srvlist" style="margin-top:14px"></div></div>
<div id="pane-settings" class="pane"><h3 class="sec">Settings</h3><p class="note">Your model, your server, your rules.</p><div class="savebar"><button class="btn primary" onclick="saveSettings()">Save settings</button><span class="note" style="margin:0">Saved to ~/.muse99/config.json (600).</span></div>
<div class="card" id="workdir-card"><h3>Work folder</h3><p class="sub">The folder Muse99 may read, create and edit files in (and where shell commands run). Nothing outside it is touched.</p><div style="display:flex;gap:8px;flex-wrap:wrap"><input id="wd-path" placeholder="C:\Users\you\Documents\project" style="flex:1;min-width:240px" onkeydown="if(event.key==='Enter')setWorkdir()"><button class="btn" id="wd-pick" style="display:none" onclick="pickWorkdir()">Choose folder…</button><button class="btn primary" onclick="setWorkdir()">Use this folder</button></div><div class="note" id="wd-now"></div></div><div class="card" id="local-card"><h3>Local model (on this computer)</h3><p class="sub">LM Studio, Ollama, llama.cpp, Jan, vLLM or KoboldCpp. Start its server, press Find, then click a model to use it. Any other program: type its address, or pick a provider below and enter it by hand.</p><div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn primary" onclick="findLocal(false)">Find local servers</button><input id="local-url" placeholder="Other address, e.g. 127.0.0.1:5000" style="flex:1;min-width:200px" onkeydown="if(event.key==='Enter')findLocal(true)"><button class="btn" onclick="findLocal(true)">Check address</button></div><div id="local-list" style="margin-top:12px"></div></div>
<div class="card"><h3>Provider &amp; model</h3><p class="sub">Pick a provider, fetch its models, click one to test it and set it active.</p>
<label>Provider</label><select id="set-provider"></select>
<label>API base URL</label><input id="set-base" placeholder="API base URL">
<label>API key <span class="hint">— leave blank to keep the saved one</span></label><input id="set-key" placeholder="API key" type="password">
<div class="status" id="key-source" style="margin-top:6px"></div>
<div class="kv" style="margin-top:6px">Active model: <b id="active-model">…</b></div>
<div style="display:flex;gap:8px;margin-top:12px;flex-wrap:wrap"><button class="btn primary" id="fetch-models-btn" onclick="fetchModels()">Fetch models</button><button class="btn" id="test-all-btn" onclick="testAllModels()" disabled>Test all</button><button class="btn ghost" id="test-all-cancel" onclick="cancelTestAll()" style="display:none">Cancel</button></div>
<input id="model-filter" placeholder="Filter models…" oninput="renderModelList()" style="margin-top:12px">
<div id="model-list" style="margin-top:10px"></div>
<div class="status" id="model-status"></div>
<p class="note"><a href="#" onclick="showManualModel(event)">Fetch didn't return any models? Enter one manually</a></p>
<div id="manual-model-box" style="display:none;margin-top:8px"><div class="modelrow"><input id="manual-model" placeholder="e.g. gemini-2.5-flash" autocomplete="off"><button class="btn" onclick="saveManualModel()">Use this model</button></div></div>
<p class="note">Fetch asks the provider's <code>/v1/models</code> with your key; the key is never shown or logged. Test sends one tiny message and shows if the model answers and how fast. Test all checks the first 20. For Gemini, the native list is tried too and the "models/" prefix is stripped.</p>
</div>
<div class="card"><h3>Web &amp; access</h3><p class="sub">The web login is set from the SSH menu, option 15.</p>
<label>Your name</label><input id="set-user" dir="auto" placeholder="Your name">
<label>Web port</label><input id="set-port" placeholder="3759" type="number">
<label>Domain <span class="hint">— optional, for the web link</span></label><input id="set-domain" placeholder="muse99.example.com">
<label class="check"><input id="set-tls" type="checkbox"> SSL / HTTPS on</label>
<label>Steps per message <span class="note" style="margin:0">(tool steps one message may take before it pauses and reports; 5 to 300)</span></label><input id="set-steps" type="number" min="5" max="300" style="max-width:120px">
<label class="check"><input id="set-shell" type="checkbox"> Allow shell commands <span class="note" style="margin:0">(commands start in the work folder but can reach the whole computer)</span></label>
<p class="note">Shell lets Muse99 run commands on this server. Only turn it on in a folder you trust. It applies at once; port and SSL need a web UI restart from the SSH menu.</p>
</div>
<div class="card" id="pricebox"><h3>Model prices</h3><p class="sub">Type what each model costs per 1 million tokens (input and output), in dollars or Toman. The chat header then shows what the chat and the last message cost. Leave both at 0 to remove a price. Only messages sent after you set a price are counted.</p><div id="price-list">Loading…</div><div class="modelrow" style="margin-top:10px;flex-wrap:wrap;gap:6px"><input id="price-new" placeholder="Another model id (e.g. glm-5.3-flash)" style="flex:1;min-width:200px"><button class="btn" onclick="addPriceRow()">Add model</button></div></div>
<div class="card" id="usagebox"><h3>Usage</h3><p class="sub">Tokens per model. Cost is a rough estimate at Spark rates.</p><div id="usage-detail">Loading…</div><div class="kv" id="settings" style="margin-top:10px"></div></div>
</div>
</main>
<form id="f" class="composer"><div class="runctl" id="runctl"><button type="button" class="btn" id="pausebtn" onclick="ctlPause()" title="Finish the step it is on, then wait">⏸ Pause</button><button type="button" class="btn stop" onclick="ctlStop()" title="Cancel the model call and any running command now">⏹ Stop</button></div><div class="row"><input id="in" dir="auto" autocomplete="off" placeholder="Give Muse99 a task…"><button aria-label="Send">Send</button></div><div class="hint" id="composer-hint">Muse99 can run commands on this server while Shell is on.</div></form>
</div>
<script>
if(window.fetch){const _fetch=window.fetch.bind(window);window.fetch=async(...a)=>{const r=await _fetch(...a);if(r.status===401){location.href='/login';}return r;};}

const chat=document.getElementById('chat'),inp=document.getElementById('in'),think=document.getElementById('thinking');
document.getElementById('addr').textContent=window.location.href;
function showSettingsTab(){document.querySelectorAll('.tabs button')[5].click();}
function showTasksTab(){document.querySelectorAll('.tabs button')[2].click();}
let tasksTimer=null;
function toggleOpts(){const o=document.getElementById('opts');o.classList.toggle('show');document.getElementById('optbtn').textContent=o.classList.contains('show')?'✕ Options':'⚙ Options';}
function tab(name,btn){document.querySelectorAll('.pane').forEach(p=>p.classList.remove('active'));document.getElementById('pane-'+name).classList.add('active');document.querySelectorAll('.tabs button').forEach(b=>b.classList.remove('active'));btn.classList.add('active');{const pt=document.getElementById('pagetitle');if(pt)pt.textContent=btn.textContent.replace(/^[^A-Za-z]+/,'');const fm=document.getElementById('f');if(fm&&fm.classList)fm.classList.toggle('hide',name!=='chat');}if(name==='files')loadFiles('');if(name==='tasks'){loadTasks();if(!tasksTimer)tasksTimer=setInterval(loadTasks,3000);}else if(tasksTimer){clearInterval(tasksTimer);tasksTimer=null;}if(name==='memory'){loadMemory();loadMemories();}if(name==='servers')loadServers();if(name==='db')loadDB();if(name==='settings')loadSettings();}
// refreshTaskBar is the background-activity indicator: while a task
// runs (or one just failed) it shows what is happening on every
// pane, including Chat — a task started from the Tasks tab is no
// longer invisible until you go back and check.
async function refreshTaskBar(){try{const r=await fetch('/api/task');const j=await r.json();const bar=document.getElementById('taskbar');if(!j||!j.length){bar.style.display='none';return;}const t=j[j.length-1];if(t.status==='running'){bar.style.display='';bar.className='taskbar';bar.innerHTML='● Working in the background: <b>'+escapeHtml(t.goal)+'</b> — '+escapeHtml(t.summary||'started')+' <span class="muted">(tap to open Tasks)</span>';}else if(t.status==='failed'&&t.updated_at&&Date.now()-new Date(t.updated_at).getTime()<10*60*1000){bar.style.display='';bar.className='taskbar bad';bar.innerHTML='✗ Background task failed: <b>'+escapeHtml(t.goal)+'</b> — '+escapeHtml(t.summary||'')+' <span class="muted">(tap to open Tasks)</span>';}else{bar.style.display='none';}}catch(e){}}
setInterval(refreshTaskBar,5000);refreshTaskBar();
async function pickThinking(v){try{const j=await (await fetch('/api/thinking',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({mode:v})})).json();toast(j.ok?(v==='turn'?'Earlier thinking is sent back only while working on your latest message':v==='all'?'All earlier thinking is sent back (uses more tokens)':'Earlier thinking is never sent back'):'Could not save: '+j.error,j.ok?'':'err');}catch(e){toast('Could not save: '+e,'err');}document.getElementById('thinkkeep').blur();}
async function pickCompact(v){try{const j=await (await fetch('/api/compact',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({tokens:parseInt(v)||0})})).json();toast(j.ok?(+v?'Long chats are summarised at '+(v/1000)+'k tokens':'Summaries off: the whole chat is always sent'):'Could not save: '+j.error,j.ok?'':'err');}catch(e){toast('Could not save: '+e,'err');}document.getElementById('compact').blur();}
async function pickPace(v){try{const j=await (await fetch('/api/pace',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({seconds:parseInt(v)||0})})).json();toast(j.ok?(+v?'Waiting '+v+' s between model calls':'Pacing off'):'Could not save: '+j.error,j.ok?'':'err');}catch(e){toast('Could not save: '+e,'err');}document.getElementById('pace').blur();}
function textDir(t){const r=(t.match(/[\u0590-\u08FF\uFB1D-\uFDFF\uFE70-\uFEFF]/g)||[]).length;const l=(t.match(/[A-Za-z]/g)||[]).length;if(!r&&!l)return '';return r>=l*0.5&&r>0?'rtl':'ltr';}
function fillText(d,t){const lines=String(t==null?'':t).split('\n');const all=textDir(String(t||''));if(all)d.dir=all;let inCode=false;lines.forEach(line=>{const p=document.createElement('div');p.className='ln';if(/^\s*\x60\x60\x60/.test(line))inCode=!inCode;const dd=(inCode||/^\s*\x60\x60\x60/.test(line))?'ltr':(textDir(line)||all||'auto');p.dir=dd;p.textContent=line===''?'\u00a0':line;d.appendChild(p);});}
let liveShown=0,liveTimer=null,liveChat=null;
function liveShow(texts,from){for(let i=liveShown-from;i<texts.length;i++){if(i<0)continue;add(texts[i],'muse');liveShown++;}}
async function livePoll(){try{const j=await (await fetch('/api/chat/live?from='+liveShown)).json();if(j&&(!currentChatId||!j.chat_id||j.chat_id===currentChatId))liveShow(j.texts||[],j.total-(j.texts||[]).length);}catch(e){}}
function liveStart(){liveShown=0;liveChat=currentChatId;clearInterval(liveTimer);liveTimer=setInterval(livePoll,2000);}
function liveStop(){clearInterval(liveTimer);liveTimer=null;}
function add(t,cls,tools,usage){const d=document.createElement('div');d.className='msg '+cls;fillText(d,t);if(tools&&tools.length){const x=document.createElement('div');x.className='tools';x.textContent='Tools: '+tools.join(' · ');d.appendChild(x);}const ut=fmtTok(usage);if(ut){const y=document.createElement('span');y.className='tok';y.textContent=ut;d.appendChild(y);}chat.appendChild(d);window.scrollTo(0,document.body.scrollHeight);}
let welcomed=false;
function welcome(name){if(welcomed||(chat.children&&chat.children.length))return;welcomed=true;add((name?'Hey '+name+', ':'Hey, ')+"I'm Muse99 in your browser. Give me a task and I'll work it till the end — files, shell, tests, and verification.","muse");}
async function saveFirstName(){const n=document.getElementById('name-in').value.trim();if(!n){document.getElementById('name-in').focus();return;}await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({user_name:n})});document.getElementById('namebox').style.display='none';await loadStatus();}
document.getElementById('name-in').addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();saveFirstName();}});
function fmtTok(u){if(!u||!u.calls)return '';return u.total_tokens.toLocaleString()+' tokens ('+u.prompt_tokens.toLocaleString()+' in / '+u.completion_tokens.toLocaleString()+' out'+(u.cached_tokens?', '+u.cached_tokens.toLocaleString()+' from cache':'')+')'+(u.model?' · '+u.model:'')+(fmtMoney(u)?' · '+fmtMoney(u):'');}
function fmtUsage(u){if(!u||!u.calls)return 'No tokens used yet';return u.total_tokens.toLocaleString()+' tokens'+(u.last_model?' · '+u.last_model:'');}
function usageTable(u){const rows=(u&&u.by_model)||[];if(!rows.length)return '';return '<table class="utable"><tr><th>Model</th><th>Provider</th><th class="n">In</th><th class="n">Out</th><th class="n">Total</th><th class="n">Calls</th></tr>'+rows.map(m=>'<tr><td>'+escapeHtml(m.model)+'</td><td>'+escapeHtml(m.provider||'')+'</td><td class="n">'+m.prompt_tokens.toLocaleString()+'</td><td class="n">'+m.completion_tokens.toLocaleString()+'</td><td class="n">'+m.total_tokens.toLocaleString()+'</td><td class="n">'+m.calls.toLocaleString()+'</td></tr>').join('')+'</table>';}
loadStatus();
let currentChatId=null;
let toastTimer=null;function toast(msg,kind){const t=document.getElementById('toast');t.textContent=msg;t.className='show'+(kind==='bad'?' bad':'');clearTimeout(toastTimer);toastTimer=setTimeout(()=>{t.className=kind==='bad'?'bad':'';},kind==='bad'?5000:2500);}
const actEvents=new Map();let actSeq=0,actTimer=null,actBusy=false,actSkew=0;
function actOpen(){return document.getElementById('actpanel').classList.contains('open');}
function toggleActivity(){const p=document.getElementById('actpanel');const o=!p.classList.contains('open');p.classList.toggle('open',o);document.body.classList.toggle('actopen',o);try{localStorage.setItem('muse99_act',o?'1':'0');}catch(e){}if(o){pollActivity();}actSchedule();}
async function clearActivity(){await fetch('/api/activity'+(currentChatId?'?chat='+encodeURIComponent(currentChatId):''),{method:'DELETE'});actEvents.clear();actNodes.clear();document.getElementById('actlist').innerHTML='';renderActivity();}
function fmtDur(ms){if(ms<1000)return ms+' ms';const s=Math.round(ms/100)/10;if(s<60)return s+' s';return Math.floor(s/60)+'m '+Math.round(s%60)+'s';}
function actSchedule(){clearTimeout(actTimer);if(actOpen()||actBusy){actTimer=setTimeout(pollActivity,1000);}}
async function pollActivity(){try{const r=await fetch('/api/activity?since='+actSeq);if(r.ok){const j=await r.json();actSkew=Date.now()-j.now;(j.events||[]).forEach(e=>actEvents.set(e.id,e));if(j.seq<actSeq){actEvents.clear();actNodes.clear();document.getElementById('actlist').innerHTML='';}actSeq=j.seq;while(actEvents.size>900){actEvents.delete(actEvents.keys().next().value);}renderActivity();}}catch(e){}actSchedule();}
const actNodes=new Map();
function actMeta(e,now){const d=e.pending?fmtDur(now-e.start):(e.end&&e.end>e.start?fmtDur(e.end-e.start):'');const tm=new Date(e.start+actSkew).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit',second:'2-digit'});return tm+(d?' · '+d:'');}
function actNodeHTML(e,now){const icon={user:'🗨',model:'🧠',tool:'⚙',retry:'↻',error:'✗',done:'✓',info:'·'};return (e.source&&e.source!=='chat'?'<div class="src">'+escapeHtml(e.source)+'</div>':'')+'<div class="at"><span>'+(icon[e.kind]||'·')+'</span><span class="t" dir="auto">'+escapeHtml(e.title)+'</span><span class="m">'+actMeta(e,now)+'</span></div>'+(e.detail?'<pre dir="auto">'+escapeHtml(e.detail)+'</pre>':'');}
let dbCur='';
function fmtSize(n){return n<1024?n+' B':n<1048576?(n/1024).toFixed(1)+' KB':(n/1048576).toFixed(1)+' MB';}
async function loadDB(){const el=document.getElementById('dbbuckets');try{const j=await (await fetch('/api/db')).json();if(!j.ok){el.textContent=j.error||'No database';return;}document.getElementById('dbpath').textContent='File: '+j.path;el.innerHTML='';(j.buckets||[]).forEach(b=>{const d=document.createElement('div');d.className='card';d.style.cssText='margin:8px 0;cursor:pointer;display:flex;justify-content:space-between;gap:10px';d.innerHTML='<div><b>'+escapeHtml(b.name)+'</b><div class="sub">'+escapeHtml(b.about)+'</div></div><div class="sub" style="white-space:nowrap">'+b.count+' entries · '+fmtSize(b.bytes)+'</div>';d.onclick=()=>{document.getElementById('dbq').value='';dbOpen(b.name);};el.appendChild(d);});}catch(e){el.textContent='Could not read the database: '+e;}}
async function dbOpen(name){dbCur=name;document.getElementById('dbbrowse').style.display='';document.getElementById('dbname').textContent=name;document.getElementById('dbclearall').style.display=name==='activity'?'':'none';const q=document.getElementById('dbq').value;const box=document.getElementById('dbentries');box.textContent='Loading…';const j=await (await fetch('/api/db?bucket='+encodeURIComponent(name)+'&q='+encodeURIComponent(q))).json();box.innerHTML='';if(!j.entries||!j.entries.length){box.innerHTML='<div class="empty">Nothing here.</div>';return;}if(j.total>j.entries.length){const n=document.createElement('div');n.className='sub';n.textContent='Showing '+j.entries.length+' of '+j.total+'. Search to narrow it down.';box.appendChild(n);}j.entries.forEach(e=>{const r=document.createElement('div');r.style.cssText='display:flex;gap:10px;align-items:center;padding:8px 0;border-bottom:1px solid rgba(0,0,0,.07)';const t=document.createElement('div');t.style.cssText='flex:1;min-width:0;cursor:pointer';t.innerHTML='<div style="font-family:monospace;font-size:12px;overflow-wrap:anywhere">'+escapeHtml(e.key)+' <span class="sub">'+fmtSize(e.size)+'</span></div>';const pv=document.createElement('div');pv.className='sub';pv.dir='auto';pv.style.cssText='white-space:nowrap;overflow:hidden;text-overflow:ellipsis';pv.textContent=e.preview;t.appendChild(pv);t.onclick=()=>dbView(e.key);const b=document.createElement('button');b.className='btn';b.textContent='Delete';b.onclick=()=>dbDel(e.key);r.appendChild(t);r.appendChild(b);box.appendChild(r);});}
async function dbView(key){const j=await (await fetch('/api/db?bucket='+encodeURIComponent(dbCur)+'&key='+encodeURIComponent(key))).json();document.getElementById('dbviewkey').textContent=dbCur+' / '+key;document.getElementById('dbviewval').textContent=j.value||'';const v=document.getElementById('dbview');v.style.display='';v.scrollIntoView({behavior:'smooth'});}
async function dbDel(key){const warn=dbCur==='chats'?'Delete this chat, all its messages and its Behind the scenes history?':dbCur==='meta'?'Delete this setting/total? (usage totals start again from zero)':'Delete this entry?';if(!confirm(warn+'\n\n'+key))return;const j=await (await fetch('/api/db?bucket='+encodeURIComponent(dbCur)+'&key='+encodeURIComponent(key),{method:'DELETE'})).json();toast(j.ok?'Deleted':'Could not delete: '+j.error,j.ok?'':'err');if(dbCur==='chats'){if(currentChatId===key){currentChatId=null;chat.innerHTML='';}loadChats();}dbOpen(dbCur);loadDB();}
async function dbClearAll(){if(!confirm('Delete the whole Behind the scenes history of every chat?'))return;const j=await (await fetch('/api/db?bucket=activity',{method:'DELETE'})).json();toast(j.ok?'Deleted '+j.deleted+' events':'Could not delete: '+j.error,j.ok?'':'err');actEvents.clear();actNodes.clear();document.getElementById('actlist').innerHTML='';renderActivity();dbOpen(dbCur);loadDB();}
async function loadChatActivity(id){for(const [k,e] of actEvents){if(e.chat&&!e.pending)actEvents.delete(k);}if(!id){renderActivity();return;}try{const j=await (await fetch('/api/activity?chat='+encodeURIComponent(id))).json();(j.events||[]).forEach(e=>{if(!actEvents.has(e.id))actEvents.set(e.id,e);});}catch(e){}renderActivity();}
function renderActivity(){const evs=[...actEvents.values()].filter(e=>!currentChatId||!e.chat||e.chat===currentChatId).sort((a,b)=>a.id-b.id);const now=Date.now()-actSkew;const pend=evs.filter(e=>e.pending);const nowEl=document.getElementById('actnow');let line='Idle.';if(pend.length){const e=pend[pend.length-1];line='● '+e.title+' — '+fmtDur(now-e.start);}nowEl.textContent=line;if(actBusy){think.textContent=pend.length?(pend[pend.length-1].title.replace(/…$/,'')+' · '+fmtDur(now-pend[pend.length-1].start)):'Muse99 is working…';}if(!actOpen())return;const list=document.getElementById('actlist');if(!evs.length){actNodes.clear();list.innerHTML='<div class="empty">Nothing yet. Send a message and every step shows up here.</div>';return;}const emp=list.querySelector('.empty');if(emp)emp.remove();const atBottom=list.scrollHeight-list.scrollTop-list.clientHeight<40;let added=false;const keep=new Set(evs.map(e=>e.id));for(const [id,n] of actNodes){if(!keep.has(id)){n.el.remove();actNodes.delete(id);}}for(const e of evs){let n=actNodes.get(e.id);const cls='aev'+(e.pending?' pending':'')+(e.ok?'':' bad')+(e.kind==='retry'?' retry':'');if(!n){const el=document.createElement('div');el.className=cls;el.innerHTML=actNodeHTML(e,now);list.appendChild(el);n={el:el,seq:e.seq};actNodes.set(e.id,n);added=true;continue;}if(n.seq!==e.seq){const pre=n.el.querySelector('pre');const st=pre?pre.scrollTop:0;n.el.className=cls;n.el.innerHTML=actNodeHTML(e,now);const p2=n.el.querySelector('pre');if(p2&&st)p2.scrollTop=st;n.seq=e.seq;continue;}if(e.pending){const m=n.el.querySelector('.m');if(m)m.textContent=actMeta(e,now);}}if(added&&atBottom)list.scrollTop=list.scrollHeight;}
try{if(localStorage.getItem('muse99_act')==='1'){setTimeout(toggleActivity,0);}}catch(e){}
async function loadChats(){const q=document.getElementById('chat-search').value.trim();const r=await fetch('/api/chats'+(q?'?q='+encodeURIComponent(q):''));const j=await r.json();const list=document.getElementById('chatlist');list.innerHTML=(j||[]).map(c=>'<div class="chatitem'+(c.id===currentChatId?' active':'')+'" onclick="openChat(\''+c.id+'\')"><span>'+escapeHtml(c.title||'Chat')+'</span><button title="Rename" onclick="event.stopPropagation();renameChat(\''+c.id+'\',\''+escapeHtml(c.title||'').replace(/'/g,'')+'\')">✎</button><button title="Delete" onclick="event.stopPropagation();deleteChat(\''+c.id+'\')">✕</button></div>').join('')||'<div class="empty">(no chats yet)</div>';}
function escapeHtml(t){return String(t).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
async function goChat(){const p=document.getElementById('pane-chat');const b=document.getElementById('tab-chat');if(p&&b&&!p.classList.contains('active'))tab('chat',b);}
async function openChat(id){goChat();currentChatId=id;loadChatActivity(id);refreshChatUse();const r=await fetch('/api/chats/'+id+'/messages');const j=await r.json();chat.innerHTML='';(j||[]).forEach(m=>{if(m.role==='user')add(m.content,'user');else if(m.role==='assistant'&&(m.content||'').trim())add(m.content,'muse');});loadChats();}
async function newChat(){goChat();const r=await fetch('/api/chats',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:'New chat'})});const j=await r.json();currentChatId=j.id;loadChatActivity(j.id);showChatUse(null,null);chat.innerHTML='';add("New chat — what should we do?","muse");loadChats();}
async function renameChat(id,title){const t=prompt('Rename chat',title);if(!t)return;await fetch('/api/chats/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:t})});loadChats();}
async function deleteChat(id){if(!confirm('Delete this chat and all its messages?'))return;await fetch('/api/chats/'+id,{method:'DELETE'});if(currentChatId===id){currentChatId=null;chat.innerHTML='';}loadChats();}
async function loadMemories(){const r=await fetch('/api/memories');const j=await r.json();document.getElementById('memlist').innerHTML=(j||[]).map(m=>'<div class="card"><b>['+escapeHtml(m.kind||'fact')+']</b> '+escapeHtml(m.text)+' <span class="muted">'+(m.created_at||'').slice(0,10)+'</span> <button class="btn ghost" onclick="deleteMemory(\''+m.id+'\')">Delete</button></div>').join('')||'<div class="empty">(no memories saved yet)</div>';}
async function deleteMemory(id){await fetch('/api/memories/'+id,{method:'DELETE'});loadMemories();}
async function summariseChat(){const r=await fetch('/api/chats/'+(currentChatId||'')+'/summarise',{method:'POST'});const j=await r.json();toast('Saved '+(j.saved||0)+' new fact(s) to memory');loadMemories();}
async function toggleShell(){const on=document.getElementById('shell-toggle').checked;await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({allow_shell:on})});document.getElementById('shellbox').classList.toggle('on',on);loadStatus();}
function fillModels(s){const sel=document.getElementById('model'),bk=document.getElementById('backup');if(!sel||!bk||sel.tagName!=='SELECT')return;if(document.activeElement===sel||document.activeElement===bk)return;const list=(s.recent||[]).slice();if(s.model&&!list.some(m=>m.provider===s.provider&&m.model===s.model))list.unshift({provider:s.provider,model:s.model});window._models=list;sel.innerHTML='';bk.innerHTML='';const n=document.createElement('option');n.value='-1';n.textContent='Backup: none';bk.appendChild(n);list.forEach((m,i)=>{const lab=m.model+' · '+m.provider;const o=document.createElement('option');o.value=String(i);o.textContent=lab;if(m.provider===s.provider&&m.model===s.model)o.selected=true;sel.appendChild(o);const b=document.createElement('option');b.value=String(i);b.textContent='Backup: '+lab;if(s.fallback&&s.fallback.provider===m.provider&&s.fallback.model===m.model)b.selected=true;bk.appendChild(b);});const more=document.createElement('option');more.value='more';more.textContent='+ Other model… (Settings)';sel.appendChild(more);}
async function pickModel(v){if(v==='more'){showSettingsTab();loadStatus();return;}const m=(window._models||[])[+v];if(!m)return;try{const r=await fetch('/api/models/active',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({provider:m.provider,model:m.model})});const j=await r.json();if(j.ok){toast('Now using '+m.model+'. Send "continue" to carry on in this chat.');}else{toast('Could not switch: '+(j.error||'error'),'err');}}catch(e){toast('Could not switch: '+e,'err');}document.getElementById('model').blur();loadStatus();}
async function pickBackup(v){const m=(window._models||[])[+v];const body=m?{provider:m.provider,model:m.model}:{provider:'',model:''};try{const r=await fetch('/api/models/fallback',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});const j=await r.json();toast(j.ok?(m?'Backup model: '+m.model:'Backup model off'):'Could not save: '+(j.error||'error'),j.ok?'':'err');}catch(e){toast('Could not save: '+e,'err');}document.getElementById('backup').blur();loadStatus();}
async function loadStatus(){const r=await fetch('/api/status');const s=await r.json();document.getElementById('mode').textContent=s.mode;fillModels(s);{const pc=document.getElementById('pace');if(pc&&document.activeElement!==pc&&typeof s.pace==='number')pc.value=String(s.pace);}{const th=document.getElementById('thinkkeep');if(th&&document.activeElement!==th&&s.thinking)th.value=s.thinking;}{const cp=document.getElementById('compact');if(cp&&document.activeElement!==cp&&typeof s.compact==='number'){const v=String(s.compact);if(![...cp.options].some(o=>o.value===v)){const o=document.createElement('option');o.value=v;o.textContent='Summary at '+Math.round(s.compact/1000)+'k';cp.appendChild(o);}cp.value=v;}}{const mn=document.getElementById('me-name');if(mn&&s.user)mn.textContent=s.user;const av=document.querySelector?document.querySelector('.me .av'):null;if(av&&s.user)av.textContent=s.user.trim().charAt(0).toUpperCase();}if(s.address)document.getElementById('addr').textContent=s.address;document.getElementById('usage').textContent=fmtUsage(s.usage);{const v=document.getElementById('ver');if(v)v.textContent=s.version||'';}if(s.name_needed){const nb=document.getElementById('namebox');if(nb.style.display==='none'){nb.style.display='flex';setTimeout(()=>document.getElementById('name-in').focus(),50);}}else{welcome(s.user);}const nk=document.getElementById('nokey');if(nk){nk.style.display=s.api_key_set?'none':'block';}const t=document.getElementById('shell-toggle');if(t){t.checked=!!s.shell;document.getElementById('shellbox').classList.toggle('on',!!s.shell);}}
loadChats();
let runPaused=false;function runCtl(on){const c=document.getElementById('runctl');if(c)c.classList.toggle('show',!!on);if(!on){runPaused=false;const b=document.getElementById('pausebtn');if(b)b.textContent='⏸ Pause';}}
async function ctlSend(a){try{return await (await fetch('/api/control',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:a})})).json();}catch(e){toast('Could not reach server: '+e,'err');return null;}}
async function ctlPause(){const j=await ctlSend(runPaused?'play':'pause');if(!j)return;runPaused=!!j.paused;document.getElementById('pausebtn').textContent=runPaused?'▶ Play':'⏸ Pause';think.textContent=runPaused?'Paused after this step — press Play to go on':'Muse99 is working…';toast(runPaused?'Pausing after the current step':'Going on');}
async function ctlStop(){const j=await ctlSend('stop');if(j){think.textContent='Stopping…';toast('Stopped');}}
document.getElementById('f').addEventListener('submit',async e=>{e.preventDefault();const v=inp.value.trim();if(!v)return;if(document.getElementById('runctl').classList.contains('show')){toast('Muse99 is still on the last message. Press Stop first, or wait.');return;}add(v,'user');inp.value='';think.textContent='Muse99 is working…';think.style.display='block';actBusy=true;runCtl(true);pollActivity();liveStart();try{const r=await fetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:v,chat_id:currentChatId})});const j=await r.json();if(j.chat_id)currentChatId=j.chat_id;if(j.chat_tokens)showChatUse(j.chat_tokens,j.usage);liveStop();liveShow(j.interim||[],0);think.style.display='none';actBusy=false;runCtl(false);pollActivity();add(j.reply||j.error||'(no reply)','muse',j.tools,j.usage);loadStatus();}catch(err){liveStop();think.style.display='none';actBusy=false;runCtl(false);add('Could not reach server: '+err,'muse');}});
function fmtMoney(u){if(!u)return '';const p=[];if(u.cost_usd>0){const v=u.cost_usd;p.push('$'+(v>=1?v.toFixed(2):v>=0.01?v.toFixed(3):v.toFixed(4)));}if(u.cost_toman>0){const v=u.cost_toman;p.push((v>=10?Math.round(v).toLocaleString():v.toFixed(2))+' Toman');}return p.join(' + ');}
function showChatUse(t,l){const e=document.getElementById('chatuse');if(!e)return;if(!t||!t.total_tokens){e.textContent='';return;}const tc=fmtMoney(t),lc=fmtMoney(l);e.textContent='This chat: '+t.total_tokens.toLocaleString()+' tokens'+(tc?' · '+tc:'')+(l&&l.total_tokens?' · Last: '+l.total_tokens.toLocaleString()+(lc?' · '+lc:''):'');e.title=(tc?'':'No price set for this model yet. ')+'Click to set model prices (per 1M tokens, $ or Toman).'+(t.unpriced?' '+t.unpriced+' call(s) had no price.':'');}
function openPrices(){showSettingsTab();setTimeout(()=>{const b=document.getElementById('pricebox');if(b)b.scrollIntoView({behavior:'smooth',block:'start'});},150);}
async function loadPrices(){try{const j=await (await fetch('/api/prices')).json();window._prices=j.prices||[];renderPrices(j.active||'');}catch(e){document.getElementById('price-list').textContent='Could not load prices: '+e;}}
function renderPrices(active){const rows=window._prices||[];const el=document.getElementById('price-list');if(!rows.length){el.innerHTML='<div class="empty">No models yet. Add one below.</div>';return;}el.innerHTML='<table class="utable"><tr><th>Model</th><th class="n">Input / 1M</th><th class="n">Output / 1M</th><th>Currency</th><th></th></tr>'+rows.map((r,i)=>'<tr><td>'+escapeHtml(r.model)+(r.model===active?' <span class="tag">ACTIVE</span>':'')+'</td><td class="n"><input id="pin-'+i+'" type="number" min="0" step="any" value="'+(r.set?r.in:'')+'" placeholder="0" style="width:110px"></td><td class="n"><input id="pout-'+i+'" type="number" min="0" step="any" value="'+(r.set?r.out:'')+'" placeholder="0" style="width:110px"></td><td><select id="pcur-'+i+'"><option value="usd"'+(r.currency!=='toman'?' selected':'')+'>$ Dollar</option><option value="toman"'+(r.currency==='toman'?' selected':'')+'>Toman</option></select></td><td><button class="btn primary" onclick="savePrice('+i+')">Save</button></td></tr>').join('')+'</table>';}
async function savePrice(i){const r=(window._prices||[])[i];if(!r)return;const a=parseFloat(document.getElementById('pin-'+i).value||'0'),b=parseFloat(document.getElementById('pout-'+i).value||'0');if(isNaN(a)||isNaN(b)||a<0||b<0){toast('Prices must be numbers, 0 or more','err');return;}try{const j=await (await fetch('/api/prices',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model:r.model,in:a,out:b,currency:document.getElementById('pcur-'+i).value})})).json();toast(j.ok?(a||b?'Price saved for '+r.model:'Price removed for '+r.model):'Could not save: '+(j.error||'error'),j.ok?'':'err');if(j.ok)loadPrices();}catch(e){toast('Could not save: '+e,'err');}}
function addPriceRow(){const el=document.getElementById('price-new');const m=el.value.trim();if(!m)return;window._prices=window._prices||[];if(!window._prices.some(r=>r.model===m))window._prices.push({model:m,in:0,out:0,currency:'usd',set:false});el.value='';renderPrices('');}
async function refreshChatUse(){if(!currentChatId){showChatUse(null,null);return;}try{const c=await (await fetch('/api/chats/'+currentChatId)).json();if(c&&c.id===currentChatId)showChatUse(c.tokens,c.last_tokens);}catch(e){}}
async function clearChat(){await fetch('/api/clear',{method:'POST'});showChatUse(null,null);chat.innerHTML='';add('Chat cleared. Memory is kept.','muse');}
async function loadFiles(p){const r=await fetch('/api/files?path='+encodeURIComponent(p||''));const j=await r.json();document.getElementById('filelist').innerHTML=j.files.map(f=>'<div class="card" style="cursor:pointer;padding:12px 16px" onclick="viewFile(\''+f.path.replace(/'/g,'')+'\','+f.dir+')">'+(f.dir?'📁 ':'📄 ')+f.name+'</div>').join('')||'<div class="empty">(empty)</div>';}
async function viewFile(p,isDir){if(isDir){loadFiles(p);return;}const r=await fetch('/api/file?path='+encodeURIComponent(p));const j=await r.json();document.getElementById('fileview').textContent=j.content||'(empty)';}
async function loadTasks(){const r=await fetch('/api/task');const j=await r.json();document.getElementById('tasklist').innerHTML=j.map(t=>'<div class="card"><b>'+escapeHtml(t.goal)+'</b><br>Status: <b>'+escapeHtml(t.status)+'</b> · '+escapeHtml(t.id)+(t.steps&&t.steps.length?'<br>Steps: '+escapeHtml(t.steps.slice(-8).join(' → ')):'')+'<br>'+escapeHtml(t.summary||'')+'</div>').join('')||'<div class="empty">(no tasks yet)</div>';}
async function startTask(){const g=document.getElementById('taskgoal').value.trim();if(!g)return;const r=await fetch('/api/task',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({goal:g})});const j=await r.json().catch(()=>({}));document.getElementById('taskgoal').value='';setTimeout(loadTasks,1000);add('Started autonomous task: '+g+(j.task_id?' ('+j.task_id+')':''),'muse');refreshTaskBar();}
async function loadMemory(){const r=await fetch('/api/memory');const j=await r.json();document.getElementById('mem').value=j.memory||'';}
async function saveMemory(){await fetch('/api/memory',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({memory:document.getElementById('mem').value})});toast('Saved ✓');}
async function loadServers(){const r=await fetch('/api/servers');const j=await r.json();window._servers=j;document.getElementById('srvlist').innerHTML=j.map((s,i)=>'<div class="card"><b>'+escapeHtml(s.name||'')+'</b> — '+escapeHtml(s.user||'')+'@'+escapeHtml(s.host||'')+':'+escapeHtml(String(s.port||22))+(s.has_password?' · 🔑 password saved':'')+'<div style="margin-top:8px;display:flex;gap:6px;flex-wrap:wrap"><button class="btn" onclick="srvAct('+i+',\'test\')">Test connection</button>'+(s.has_password?'<button class="btn" onclick="srvAct('+i+',\'installkey\')">Install key</button>':'')+'<button class="btn" onclick="srvDel('+i+')">Delete</button></div><div class="note" id="srvout-'+i+'"></div></div>').join('')||'<div class="empty">(no servers yet)</div>';try{const k=await (await fetch('/api/servers/key')).json();document.getElementById('srv-keycmd').textContent=k.ok?k.command:('error: '+k.error);}catch(e){}}
async function addServer(){const b={name:document.getElementById('srv-name').value,user:document.getElementById('srv-user').value,host:document.getElementById('srv-host').value,port:parseInt(document.getElementById('srv-port').value||'22'),password:document.getElementById('srv-pass').value};const r=await fetch('/api/servers',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});if(!r.ok){toast('Need user and host','err');return;}document.getElementById('srv-pass').value='';toast('Server saved');loadServers();}
async function srvAct(i,act){const s=(window._servers||[])[i];if(!s)return;const o=document.getElementById('srvout-'+i);o.textContent=act==='test'?'Connecting…':'Installing key…';try{const j=await (await fetch('/api/servers/'+act,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:s.name})})).json();o.textContent=(j.ok?'✓ ':'✗ ')+(j.ok?j.output:j.error);}catch(e){o.textContent='✗ '+e;}}
async function srvDel(i){const s=(window._servers||[])[i];if(!s||!confirm('Delete server '+s.name+'?'))return;await fetch('/api/servers?name='+encodeURIComponent(s.name),{method:'DELETE'});loadServers();}
function copyKeyCmd(){const t=document.getElementById('srv-keycmd').textContent;if(navigator.clipboard&&window.isSecureContext){navigator.clipboard.writeText(t).then(()=>toast('Copied')).catch(()=>selKeyCmd());}else{selKeyCmd();}}
function selKeyCmd(){const el=document.getElementById('srv-keycmd');const rg=document.createRange();rg.selectNodeContents(el);const sel=window.getSelection();sel.removeAllRanges();sel.addRange(rg);try{document.execCommand('copy');toast('Copied');}catch(e){toast('Selected — press Ctrl+C');}}
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
function renderModelList(){const q=document.getElementById('model-filter').value.trim().toLowerCase();const shown=allModels.filter(m=>!q||m.id.toLowerCase().includes(q)||(m.owned_by||'').toLowerCase().includes(q));const list=document.getElementById('model-list');if(!shown.length){list.innerHTML='<div class="empty">'+(allModels.length?'No models match "'+escapeHtml(q)+'".':'No models yet — press Fetch models.')+'</div>';return;}list.innerHTML=shown.map(m=>{const r=modelResults[m.id]||{};const isActive=m.id===activeModel;let res='';if(r.testing)res='<span class="res">Testing…</span>';else if(r.ok)res='<span class="res ok">✓ '+r.latency_ms+' ms · "'+escapeHtml(r.reply||'')+'"</span>';else if(r.error){const short=r.error.length>180?r.error.slice(0,180)+'…':r.error;res='<span class="res bad" title="'+escapeHtml(r.error)+'">✗ '+escapeHtml(short)+'</span>';}return '<div class="modelitem'+(isActive?' active':'')+'" data-id="'+escapeHtml(m.id)+'"><div class="mid"><b>'+escapeHtml(m.id)+'</b>'+(isActive?' <span class="tag">ACTIVE</span>':'')+'<div class="sub">'+escapeHtml(m.owned_by||'')+'</div></div>'+res+'<button class="btn" data-act="test" data-id="'+escapeHtml(m.id)+'">Test</button><button class="btn primary" data-act="active" data-id="'+escapeHtml(m.id)+'">Set active</button></div>';}).join('');if(q)modelStatus(shown.length+' of '+allModels.length+' models match "'+q+'"','');}
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
// pendingKeys holds a key the user typed for a provider but has not
// saved yet, keyed by provider name. The key box is shared by all
// providers, so without this a Gemini key typed in the box would
// still be sitting there after switching to ChatGPT — and pressing
// Save would store the Gemini key under ChatGPT too. On switch the
// typed value is stashed under the old provider and the new
// provider's own value (or blank) is shown instead.
let pendingKeys={};let currentSettingsProvider='';
// setKeyField shows the right key state for one provider: a typed
// (unsaved) key for it, or an empty box whose placeholder shows the
// saved key's masked ending for THIS provider only.
function setKeyField(provider,masked){const k=document.getElementById('set-key');k.value=pendingKeys[provider]||'';k.placeholder=masked?('Key saved ('+masked+') — leave blank to keep it'):'API key';}
async function loadSettings(){loadPrices();loadWorkdir();const r=await fetch('/api/settings');const j=await r.json();settingsData=j;document.getElementById('settings').innerHTML='Base: '+(j.base_url||'')+'<br>Model: '+(j.model||'')+'<br>User: '+(j.user_name||'')+'<br>Key: '+(j.api_key_masked||'(not set)')+' ('+(j.api_key_source||'none')+')<br>Config: '+j.config_path+'<br>Version: '+escapeHtml(j.version||'');{const u=j.usage||{};document.getElementById('usage-detail').innerHTML=(u.calls?escapeHtml(u.prompt_tokens.toLocaleString()+' in / '+u.completion_tokens.toLocaleString()+' out · '+u.calls.toLocaleString()+' API calls · est. $'+Number(u.estimated_cost_usd).toFixed(4)+' (Spark rates)')+(u.last_total_tokens?'<br>Last call: '+escapeHtml(u.last_total_tokens.toLocaleString()+' tokens'+(u.last_model?' · '+u.last_model:'')+(u.last_label?' ('+u.last_label+')':'')):''):'No tokens used yet')+usageTable(u);}const sel=document.getElementById('set-provider');sel.innerHTML=(j.providers||[]).map(p=>'<option value="'+escapeHtml(p.name)+'">'+escapeHtml(p.name)+(p.model?' — '+escapeHtml(p.model):'')+(p.api_key_source==='none'?'':' · key saved')+'</option>').join('')+'<option value="__new">+ Add a provider (your own name)…</option>';sel.value=j.provider||'spark';currentSettingsProvider=sel.value;const ap=(j.providers||[]).find(x=>x.name===sel.value);setKeyField(sel.value,ap?ap.api_key_masked:'');document.getElementById('set-base').value=j.base_url||'';activeModel=j.model||'';document.getElementById('active-model').textContent=j.model||'(none yet — fetch models and set one active)';document.getElementById('key-source').textContent='Key in use: '+(j.api_key_masked||'(not set)')+(j.api_key_source==='env'?' — from the environment, press Save settings to keep it in config.json':j.api_key_source==='saved'?' — saved in config.json':'');document.getElementById('set-user').value=j.user_name||'';document.getElementById('set-port').value=j.port||3759;document.getElementById('set-domain').value=j.domain||'';document.getElementById('set-tls').checked=!!j.tls_enabled;document.getElementById('set-shell').checked=!!j.allow_shell;document.getElementById('set-steps').value=j.chat_steps||60;sel.onchange=()=>{pendingKeys[currentSettingsProvider]=document.getElementById('set-key').value;if(sel.value==='__new'){const nm=(prompt('Name for this API (for example glm, deepseek, my-openrouter). Letters, digits, - _ . only.')||'').trim();if(!/^[A-Za-z0-9._-]{1,40}$/.test(nm)){sel.value=currentSettingsProvider;if(nm)toast('Use letters, digits, - _ . only (max 40)','bad');return;}if(!(j.providers||[]).find(x=>x.name===nm)){(j.providers=j.providers||[]).push({name:nm,base_url:'',model:'',api_key_masked:'(not set)',api_key_source:'none'});const o=document.createElement('option');o.value=nm;o.textContent=nm;sel.insertBefore(o,sel.lastElementChild);}sel.value=nm;}const p=(j.providers||[]).find(x=>x.name===sel.value);if(p){document.getElementById('set-base').value=p.base_url||'';activeModel=p.model||'';document.getElementById('active-model').textContent=p.model||'(none yet — fetch models and set one active)';document.getElementById('key-source').textContent='Key in use: '+(p.api_key_masked||'(not set)')+(p.api_key_source==='env'?' — from the environment, press Save settings to keep it in config.json':p.api_key_source==='saved'?' — saved in config.json':'');currentSettingsProvider=sel.value;setKeyField(sel.value,p.api_key_masked||'');}refreshModelList();};if(j.models&&j.models.length){fillModelList(j.models,j.models_source||'cached',j.models_fetched_at);}else{modelStatus('No models fetched yet for this provider — press Fetch models.','');}}
async function saveSettings(){const b={active_provider:document.getElementById('set-provider').value,base_url:document.getElementById('set-base').value,api_key:document.getElementById('set-key').value,user_name:document.getElementById('set-user').value,port:parseInt(document.getElementById('set-port').value||'3759'),domain:document.getElementById('set-domain').value,tls_enabled:document.getElementById('set-tls').checked,allow_shell:document.getElementById('set-shell').checked,chat_steps:parseInt(document.getElementById('set-steps').value||'60')};const sr=await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});if(!sr.ok){toast('Could not save: '+(await sr.text()).slice(0,160),'bad');return;}delete pendingKeys[b.active_provider];document.getElementById('set-key').value='';toast((b.port!==(settingsData&&settingsData.port)||b.tls_enabled!==!!(settingsData&&settingsData.tls_enabled))?'Saved — restart the web UI from the SSH menu for the port or SSL change':'Saved ✓');loadSettings();loadStatus();}
async function loadWorkdir(){try{const j=await (await fetch('/api/workdir')).json();document.getElementById('wd-path').value=j.workdir||'';document.getElementById('wd-now').textContent='Now: '+(j.workdir||'');document.getElementById('wd-pick').style.display=j.can_pick?'':'none';}catch(e){}}
async function setWorkdir(p){const path=p||document.getElementById('wd-path').value.trim();try{const j=await (await fetch('/api/workdir',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path})})).json();if(j.ok){toast('Work folder: '+j.workdir,'ok');loadWorkdir();loadStatus();}else toast(j.error||'Could not use that folder','bad');}catch(e){toast(String(e),'bad');}}
async function pickWorkdir(){const b=document.getElementById('wd-pick');b.disabled=true;try{const j=await (await fetch('/api/workdir/pick',{method:'POST'})).json();if(j.ok&&j.path){await setWorkdir(j.path);}else if(!j.ok)toast(j.error||'Could not open the folder dialog','bad');}catch(e){toast(String(e),'bad');}b.disabled=false;}
async function findLocal(withUrl){const el=document.getElementById('local-list');el.innerHTML='<div class="status">Looking for local servers…</div>';const u=withUrl?document.getElementById('local-url').value.trim():'';try{const r=await fetch('/api/local/detect'+(u?'?url='+encodeURIComponent(u):''));const j=await r.json();const all=j.servers||[];const up=all.filter(x=>x.up);const down=all.filter(x=>!x.up);let h='';up.forEach(x=>{h+='<div class="card" style="margin:8px 0;padding:12px 14px"><b>'+escapeHtml(x.label)+'</b> <span class="note" style="margin:0">'+escapeHtml(x.base_url)+'</span><div style="display:flex;gap:6px;flex-wrap:wrap;margin-top:8px">'+(x.models.length?x.models.map(m=>'<button class="btn" data-p="'+escapeHtml(x.name)+'" data-b="'+escapeHtml(x.base_url)+'" data-k="'+escapeHtml(x.api_key)+'" data-m="'+escapeHtml(m.id)+'" onclick="useLocal(this)">'+escapeHtml(m.id)+(m.loaded?' · loaded':'')+(j.active===x.name&&activeModel===m.id?' ✓':'')+'</button>').join(''):'<span class="note" style="margin:0">Running, but it has no models. Download or load one in that app.</span>')+'</div></div>';});h+='<div class="note">'+(up.length?'':'No local model server is running. In LM Studio: Developer tab, then Start Server. ')+(down.length?'Not running: '+escapeHtml(down.map(x=>x.label+(x.name==='local'?' ('+x.base_url+')':'')).join(', ')):'')+'</div>';el.innerHTML=h;}catch(e){el.innerHTML='<div class="status bad">'+escapeHtml(String(e))+'</div>';}}
async function useLocal(b){const body={provider:b.dataset.p,base_url:b.dataset.b,api_key:b.dataset.k,model:b.dataset.m};b.disabled=true;try{const r=await fetch('/api/models/active',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});const j=await r.json();if(j.ok){toast('Connected to '+body.model,'ok');await loadSettings();loadStatus();findLocal(false);}else toast(j.error||'Could not connect','bad');}catch(e){toast(String(e),'bad');}b.disabled=false;}
async function quitDesktop(e){e.preventDefault();if(!confirm('Quit Muse99?'))return;try{await fetch('/api/desktop/quit',{method:'POST'});}catch(x){}document.body.innerHTML='<p style="font-family:Segoe UI,sans-serif;padding:40px">Muse99 has quit. You can close this window.</p>';setTimeout(()=>window.close(),400);}
fetch('/api/status').then(r=>r.json()).then(j=>{if(!j.desktop)return;{const h=document.getElementById('composer-hint');if(h)h.textContent='Muse99 works in your work folder. With Shell on it can run commands on this computer.';}const a=document.querySelector('.me a');if(a){a.textContent='Quit';a.href='#';a.onclick=quitDesktop;}}).catch(()=>{});
</script></body></html>
`
