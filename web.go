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
//   muse99 --web --port 8080 --allow-shell
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

// Start launches the web UI in the background (non-blocking).
func (c *webController) Start(cfg Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
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
		// Only update fields the user sent; keep the existing API key if blank.
		old := loadFileConfig()
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
		_ = saveFileConfig(old)
	}
	w.Header().Set("Content-Type", "application/json")
	fc := loadFileConfig()
	masked := maskKey(fc.APIKey)
	providers := []map[string]string{}
	for _, name := range providerPresetNames() {
		p := fc.Providers[name]
		key := maskKey(p.APIKey)
		providers = append(providers, map[string]string{
			"name": name, "base_url": p.BaseURL, "model": p.Model, "api_key_masked": key,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"base_url": fc.BaseURL, "model": fc.Model, "user_name": fc.UserName,
		"api_key_masked": masked, "config_path": configPath(),
		"provider": fc.ActiveProvider, "providers": providers,
		"port": fc.Port, "domain": fc.Domain, "tls_enabled": fc.TLSEnabled,
		"allow_shell": fc.AllowShell, "usage": usageTotalsJSON(),
	})
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
<style>*{box-sizing:border-box}html,body{margin:0;padding:0}body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f7f7f5;color:#1a1a1a;font-size:15px;line-height:1.55}header{position:sticky;top:0;background:#fff;border-bottom:1px solid #e5e5e0;z-index:10}header .bar{max-width:860px;margin:0 auto;padding:12px 16px;display:flex;gap:10px;align-items:center;flex-wrap:wrap}h1{font-size:17px;margin:0;font-weight:650;letter-spacing:-.01em}.badge{background:#f0f0ec;color:#555550;padding:3px 10px;border-radius:999px;font-size:12px}.badge.spark{background:#eef2ff;color:#3730a3}.muted{color:#8a8a84;font-size:13px}.addr{color:#8a8a84;font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:280px}header button{background:none;border:1px solid #e5e5e0;color:#555550;border-radius:999px;padding:6px 12px;cursor:pointer;font-size:13px}nav{max-width:860px;margin:0 auto;padding:0 16px;display:flex;gap:2px;overflow-x:auto}nav button{background:none;border:0;border-bottom:2px solid transparent;color:#6b6b66;padding:10px 12px;cursor:pointer;font-size:14px;white-space:nowrap}nav button.active{color:#1a1a1a;border-bottom-color:#1a1a1a;font-weight:600}main{max-width:860px;margin:0 auto;padding:20px 16px 160px}.pane{display:none}.pane.active{display:block}.msg{padding:11px 14px;border-radius:18px;line-height:1.55;white-space:pre-wrap;max-width:82%;margin-bottom:12px;font-size:15px}.user{margin-left:auto;background:#1a1a1a;color:#fff;border-bottom-right-radius:6px}.muse{background:#fff;border:1px solid #e5e5e0;border-bottom-left-radius:6px}.tools{font-size:12px;color:#8a8a84;margin-top:6px}form{position:fixed;left:0;right:0;bottom:0;background:#f7f7f5;border-top:1px solid #e5e5e0;padding:12px 16px}.row{max-width:860px;margin:0 auto;display:flex;gap:10px}.row input{flex:1;padding:12px 14px;border-radius:999px;border:1px solid #d9d9d3;background:#fff;font-size:15px;outline:none}.row input:focus{border-color:#1a1a1a}.row button{padding:12px 20px;border-radius:999px;border:0;background:#1a1a1a;color:#fff;font-weight:600;cursor:pointer}button.action{padding:10px 18px;border-radius:999px;border:0;background:#1a1a1a;color:#fff;font-weight:600;cursor:pointer}input,textarea,select{padding:10px 12px;border-radius:10px;border:1px solid #d9d9d3;background:#fff;font-size:14px;width:100%}label{display:block;font-size:13px;color:#6b6b66;margin:14px 0 5px}pre{background:#fff;border:1px solid #e5e5e0;padding:14px;border-radius:12px;overflow:auto;white-space:pre-wrap;font-size:13px}.card{background:#fff;border:1px solid #e5e5e0;border-radius:12px;padding:14px;margin-bottom:12px}h3{font-size:16px;margin:4px 0 12px}.note{font-size:13px;color:#8a8a84;margin-top:12px}
.layout{display:flex;max-width:1120px;margin:0 auto}aside{width:240px;flex-shrink:0;border-right:1px solid #e5e5e0;padding:16px 12px;background:#fff;position:sticky;top:96px;height:calc(100vh - 96px);overflow-y:auto}aside h3{font-size:13px;text-transform:uppercase;letter-spacing:.04em;color:#8a8a84}aside input{margin-bottom:10px}.chatitem{display:flex;gap:6px;align-items:center;padding:8px 10px;border-radius:10px;cursor:pointer;font-size:14px}.chatitem:hover{background:#f0f0ec}.chatitem.active{background:#eef2ff;font-weight:600}.chatitem span{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.chatitem button{background:none;border:0;color:#8a8a84;cursor:pointer;padding:2px 4px;font-size:13px}.shellbox{display:flex;gap:8px;align-items:center;background:#fff;border:1px solid #e5e5e0;border-radius:999px;padding:6px 12px;font-size:13px;color:#555550;cursor:pointer}.shellbox input{width:auto;margin:0}.shellbox.on{border-color:#1a1a1a;color:#1a1a1a;font-weight:600}@media (max-width:760px){.layout{flex-direction:column}aside{width:100%;position:static;height:auto;border-right:0;border-bottom:1px solid #e5e5e0}main{max-width:860px}}</style></head><body>
<header><div class="bar"><h1>✦ Muse99</h1><span class="badge" id="mode">…</span><span class="muted" id="model"></span><span class="addr" id="addr"></span><span class="muted" id="usage"></span><span style="flex:1"></span><label class="shellbox" id="shellbox" title="Lets Muse99 run commands on this server. Only turn on in a folder you trust."><input type="checkbox" id="shell-toggle" onchange="toggleShell()"> Allow shell</label><button onclick="clearChat()">Clear chat</button></div>
<nav><button class="active" onclick="tab('chat',this)">Chat</button><button onclick="tab('files',this)">Files</button><button onclick="tab('tasks',this)">Tasks</button><button onclick="tab('memory',this)">Memory</button><button onclick="tab('servers',this)">Servers</button><button onclick="tab('settings',this)">Settings</button></nav></header>
<div class="layout"><aside id="sidebar"><h3>Chats</h3><input id="chat-search" placeholder="Search chats…" oninput="loadChats()"><button class="action" style="width:100%;margin-bottom:10px" onclick="newChat()">+ New chat</button><div id="chatlist"></div></aside><main>
<div id="pane-chat" class="pane active"><div id="chat"></div><div class="muted" id="thinking" style="display:none">Muse99 is working…</div></div>
<div id="pane-files" class="pane"><h3>Files</h3><div id="filelist"></div><pre id="fileview">Click a file to view it.</pre></div>
<div id="pane-tasks" class="pane"><h3>Autonomous tasks</h3><div class="row" style="max-width:none;margin-bottom:12px"><input id="taskgoal" placeholder="Build a todo app in ./todo-app with tests…"><button onclick="startTask()">Start task</button></div><div id="tasklist"></div></div>
<div id="pane-memory" class="pane"><h3>Memory</h3><p class="note">Facts Muse99 remembers about you, with kind and source. They are used in every chat. Delete one here to forget it.</p><div id="memlist"></div><button class="action" onclick="summariseChat()">Summarise current chat into memory</button><h3 style="margin-top:20px">Memory file</h3><textarea id="mem" rows="12"></textarea><br><br><button class="action" onclick="saveMemory()">Save memory file</button></div>
<div id="pane-servers" class="pane"><h3>Servers — give Muse99 access only if you want it to</h3><div class="row" style="max-width:none;flex-wrap:wrap"><input id="srv-name" placeholder="Name"><input id="srv-user" placeholder="user"><input id="srv-host" placeholder="host / IP"><input id="srv-port" placeholder="22"></div><br><button class="action" onclick="addServer()">Add server</button><div id="srvlist" style="margin-top:14px"></div><p class="note">Remote commands use your system ssh key. No server is used unless you add it and ask for it.</p></div>
<div id="pane-settings" class="pane"><h3>Settings</h3><div id="settings"></div><div class="card" id="usagebox"><b>Token usage</b><br><span id="usage-detail">Loading…</span></div>
<label>Provider</label><select id="set-provider"></select>
<label>API base URL</label><input id="set-base" placeholder="API base URL">
<label>Model</label><input id="set-model" placeholder="Model, e.g. muse-spark-1.3">
<label>API key (leave blank to keep)</label><input id="set-key" placeholder="API key" type="password">
<label>Your name</label><input id="set-user" placeholder="Your name">
<label>Web port</label><input id="set-port" placeholder="8080" type="number">
<label>Domain (optional, for the web link)</label><input id="set-domain" placeholder="muse99.example.com">
<label><input id="set-tls" type="checkbox" style="width:auto"> SSL / HTTPS on</label>
<label><input id="set-shell" type="checkbox" style="width:auto"> Allow shell commands</label>
<p class="note">Lets Muse99 run commands on this server. Only turn on in a folder you trust. Takes effect immediately — no restart needed.</p>
<br><br><button class="action" onclick="saveSettings()">Save settings</button>
<p class="note">Saved to ~/.muse99/config.json (600 permissions). Changing the port or SSL needs a restart of the web UI — do it from the SSH menu (Restart web UI).</p></div>
</main></div>
<form id="f"><div class="row"><input id="in" autocomplete="off" placeholder="Ask Muse99 to do something…"><button>Send</button></div></form>
<script>
const chat=document.getElementById('chat'),inp=document.getElementById('in'),think=document.getElementById('thinking');
document.getElementById('addr').textContent=window.location.href;
function tab(name,btn){document.querySelectorAll('.pane').forEach(p=>p.classList.remove('active'));document.getElementById('pane-'+name).classList.add('active');document.querySelectorAll('nav button').forEach(b=>b.classList.remove('active'));btn.classList.add('active');if(name==='files')loadFiles('');if(name==='tasks')loadTasks();if(name==='memory'){loadMemory();loadMemories();}if(name==='servers')loadServers();if(name==='settings')loadSettings();}
function add(t,cls,tools){const d=document.createElement('div');d.className='msg '+cls;d.textContent=t;if(tools&&tools.length){const x=document.createElement('div');x.className='tools';x.textContent='Tools: '+tools.join(' · ');d.appendChild(x);}chat.appendChild(d);window.scrollTo(0,document.body.scrollHeight);}
add("Hey Takahashi, I'm Muse99 in your browser. Give me a task and I'll work it till the end — files, shell, tests, and verification.","muse");
function fmtUsage(u){if(!u||!u.calls)return 'No tokens used yet';return u.prompt_tokens.toLocaleString()+' in / '+u.completion_tokens.toLocaleString()+' out · est. $'+Number(u.estimated_cost_usd).toFixed(4)+' (Spark rates)';}
loadStatus();
let currentChatId=null;
async function loadChats(){const q=document.getElementById('chat-search').value.trim();const r=await fetch('/api/chats'+(q?'?q='+encodeURIComponent(q):''));const j=await r.json();const list=document.getElementById('chatlist');list.innerHTML=(j||[]).map(c=>'<div class="chatitem'+(c.id===currentChatId?' active':'')+'" onclick="openChat(\''+c.id+'\')"><span>'+escapeHtml(c.title||'Chat')+'</span><button title="Rename" onclick="event.stopPropagation();renameChat(\''+c.id+'\',\''+escapeHtml(c.title||'').replace(/'/g,'')+'\')">✎</button><button title="Delete" onclick="event.stopPropagation();deleteChat(\''+c.id+'\')">✕</button></div>').join('')||'(no chats yet)';}
function escapeHtml(t){return String(t).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
async function openChat(id){currentChatId=id;const r=await fetch('/api/chats/'+id+'/messages');const j=await r.json();chat.innerHTML='';(j||[]).forEach(m=>{if(m.role==='user')add(m.content,'user');else if(m.role==='assistant')add(m.content,'muse');});loadChats();}
async function newChat(){const r=await fetch('/api/chats',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:'New chat'})});const j=await r.json();currentChatId=j.id;chat.innerHTML='';add("New chat — what should we do?","muse");loadChats();}
async function renameChat(id,title){const t=prompt('Rename chat',title);if(!t)return;await fetch('/api/chats/'+id,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({title:t})});loadChats();}
async function deleteChat(id){if(!confirm('Delete this chat and all its messages?'))return;await fetch('/api/chats/'+id,{method:'DELETE'});if(currentChatId===id){currentChatId=null;chat.innerHTML='';}loadChats();}
async function loadMemories(){const r=await fetch('/api/memories');const j=await r.json();document.getElementById('memlist').innerHTML=(j||[]).map(m=>'<div class="card"><b>['+escapeHtml(m.kind||'fact')+']</b> '+escapeHtml(m.text)+' <span class="muted">'+(m.created_at||'').slice(0,10)+'</span> <button onclick="deleteMemory(\''+m.id+'\')">Delete</button></div>').join('')||'(no memories saved yet)';}
async function deleteMemory(id){await fetch('/api/memories/'+id,{method:'DELETE'});loadMemories();}
async function summariseChat(){const r=await fetch('/api/chats/'+(currentChatId||'')+'/summarise',{method:'POST'});const j=await r.json();alert('Saved '+(j.saved||0)+' new fact(s) to memory.');loadMemories();}
async function toggleShell(){const on=document.getElementById('shell-toggle').checked;await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({allow_shell:on})});document.getElementById('shellbox').classList.toggle('on',on);loadStatus();}
async function loadStatus(){const r=await fetch('/api/status');const s=await r.json();document.getElementById('mode').textContent=s.mode;document.getElementById('model').textContent=s.model+' · '+s.user;if(s.address)document.getElementById('addr').textContent=s.address;document.getElementById('usage').textContent=fmtUsage(s.usage);const t=document.getElementById('shell-toggle');if(t){t.checked=!!s.shell;document.getElementById('shellbox').classList.toggle('on',!!s.shell);}}
loadChats();
document.getElementById('f').addEventListener('submit',async e=>{e.preventDefault();const v=inp.value.trim();if(!v)return;add(v,'user');inp.value='';think.style.display='block';try{const r=await fetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:v,chat_id:currentChatId})});const j=await r.json();if(j.chat_id)currentChatId=j.chat_id;think.style.display='none';add(j.reply||j.error||'(no reply)','muse',j.tools);}catch(err){think.style.display='none';add('Could not reach server: '+err,'muse');}});
async function clearChat(){await fetch('/api/clear',{method:'POST'});chat.innerHTML='';add('Chat cleared. Memory is kept.','muse');}
async function loadFiles(p){const r=await fetch('/api/files?path='+encodeURIComponent(p||''));const j=await r.json();document.getElementById('filelist').innerHTML=j.files.map(f=>'<div class="card" style="cursor:pointer" onclick="viewFile(\''+f.path.replace(/'/g,'')+'\','+f.dir+')">'+(f.dir?'📁 ':'📄 ')+f.name+'</div>').join('')||'(empty)';}
async function viewFile(p,isDir){if(isDir){loadFiles(p);return;}const r=await fetch('/api/file?path='+encodeURIComponent(p));const j=await r.json();document.getElementById('fileview').textContent=j.content||'(empty)';}
async function loadTasks(){const r=await fetch('/api/task');const j=await r.json();document.getElementById('tasklist').innerHTML=j.map(t=>'<div class="card"><b>'+t.goal+'</b><br>Status: '+t.status+' · '+t.id+'<br>'+(t.summary||'')+'</div>').join('')||'(no tasks yet)';}
async function startTask(){const g=document.getElementById('taskgoal').value.trim();if(!g)return;await fetch('/api/task',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({goal:g})});document.getElementById('taskgoal').value='';setTimeout(loadTasks,1000);add('Started autonomous task: '+g,'muse');}
async function loadMemory(){const r=await fetch('/api/memory');const j=await r.json();document.getElementById('mem').value=j.memory||'';}
async function saveMemory(){await fetch('/api/memory',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({memory:document.getElementById('mem').value})});alert('Saved');}
async function loadServers(){const r=await fetch('/api/servers');const j=await r.json();document.getElementById('srvlist').innerHTML=j.map(s=>'<div class="card">'+s.name+' — '+s.user+'@'+s.host+':'+s.port+'</div>').join('')||'(no servers — Muse99 only uses servers you add here)';}
async function addServer(){const b={name:document.getElementById('srv-name').value,user:document.getElementById('srv-user').value,host:document.getElementById('srv-host').value,port:parseInt(document.getElementById('srv-port').value||'22')};await fetch('/api/servers',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});loadServers();}
let settingsData=null;
async function loadSettings(){const r=await fetch('/api/settings');const j=await r.json();settingsData=j;document.getElementById('settings').innerHTML='Base: '+(j.base_url||'')+'<br>Model: '+(j.model||'')+'<br>User: '+(j.user_name||'')+'<br>Key: '+(j.api_key_masked||'(not set)')+'<br>Config: '+j.config_path;document.getElementById('usage-detail').textContent=fmtUsage(j.usage)+(j.usage&&j.usage.last_label?' — last: '+j.usage.last_prompt_tokens.toLocaleString()+' in / '+j.usage.last_completion_tokens.toLocaleString()+' out ('+j.usage.last_label+')':'')+' · '+((j.usage&&j.usage.calls)||0)+' API calls';const sel=document.getElementById('set-provider');sel.innerHTML=(j.providers||[]).map(p=>'<option value="'+p.name+'">'+p.name+' — '+p.model+'</option>').join('');sel.value=j.provider||'spark';document.getElementById('set-base').value=j.base_url||'';document.getElementById('set-model').value=j.model||'';document.getElementById('set-user').value=j.user_name||'';document.getElementById('set-port').value=j.port||8080;document.getElementById('set-domain').value=j.domain||'';document.getElementById('set-tls').checked=!!j.tls_enabled;document.getElementById('set-shell').checked=!!j.allow_shell;sel.onchange=()=>{const p=(j.providers||[]).find(x=>x.name===sel.value);if(p){document.getElementById('set-base').value=p.base_url||'';document.getElementById('set-model').value=p.model||'';}};}
async function saveSettings(){const b={active_provider:document.getElementById('set-provider').value,base_url:document.getElementById('set-base').value,model:document.getElementById('set-model').value,api_key:document.getElementById('set-key').value,user_name:document.getElementById('set-user').value,port:parseInt(document.getElementById('set-port').value||'8080'),domain:document.getElementById('set-domain').value,tls_enabled:document.getElementById('set-tls').checked,allow_shell:document.getElementById('set-shell').checked};await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});alert('Saved — restart the web UI from the SSH menu if you changed the port or SSL.');loadSettings();}
</script></body></html>`
