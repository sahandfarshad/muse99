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
	client   *Client
	runner   *toolRunner
	tools    []ToolDef
	cfg      Config
	mu       sync.Mutex
	messages []Message
	system   string
}

type chatReq struct {
	Message string `json:"message"`
}
type chatResp struct {
	Reply string   `json:"reply"`
	Tools []string `json:"tools"`
	Error string   `json:"error,omitempty"`
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
	s := &webServer{client: c.client, runner: c.runner, tools: c.tools, cfg: cfg, messages: []Message{{Role: "system", Content: c.system}}, system: c.system}
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
		"workdir": s.cfg.Workdir, "shell": fmt.Sprint(s.cfg.AllowShell),
		"base_url": s.cfg.BaseURL, "tasks": len(loadTasks()),
		"servers": len(loadServers()), "goals": len(loadGoals()),
		"provider": s.cfg.Provider, "address": webAddressFor(s.cfg),
		"port": s.cfg.WebPort, "domain": s.cfg.Domain, "tls": s.cfg.TLSEnabled,
		"usage": usageTotalsJSON(),
	})
}

func (s *webServer) handleMemory(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct{ Memory string `json:"memory"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = os.WriteFile(s.cfg.MemoryPath, []byte(body.Memory), 0o644)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"memory": loadMemory(s.cfg.MemoryPath)})
}

func (s *webServer) handleClear(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	reply, tools := runTurnDetailed(s.client, s.runner, s.tools, &s.messages, req.Message)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(chatResp{Reply: reply, Tools: tools})
}

func (s *webServer) handleTask(w http.ResponseWriter, r *http.Request) {
	// POST /api/task {goal} starts an autonomous task in the background.
	if r.Method == http.MethodPost {
		var body struct{ Goal string `json:"goal"` }
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
		var fc FileConfig
		_ = json.NewDecoder(r.Body).Decode(&fc)
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
		// TLS is a bool: only change it when the field was sent. The web
		// UI always sends it, so a missing field means "leave it alone"
		// is not distinguishable here — the UI sends tls_enabled explicitly.
		old.TLSEnabled = fc.TLSEnabled
		s.cfg.TLSEnabled = fc.TLSEnabled
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
		"usage": usageTotalsJSON(),
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
<style>*{box-sizing:border-box}html,body{margin:0;padding:0}body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f7f7f5;color:#1a1a1a;font-size:15px;line-height:1.55}header{position:sticky;top:0;background:#fff;border-bottom:1px solid #e5e5e0;z-index:10}header .bar{max-width:860px;margin:0 auto;padding:12px 16px;display:flex;gap:10px;align-items:center;flex-wrap:wrap}h1{font-size:17px;margin:0;font-weight:650;letter-spacing:-.01em}.badge{background:#f0f0ec;color:#555550;padding:3px 10px;border-radius:999px;font-size:12px}.badge.spark{background:#eef2ff;color:#3730a3}.muted{color:#8a8a84;font-size:13px}.addr{color:#8a8a84;font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:280px}header button{background:none;border:1px solid #e5e5e0;color:#555550;border-radius:999px;padding:6px 12px;cursor:pointer;font-size:13px}nav{max-width:860px;margin:0 auto;padding:0 16px;display:flex;gap:2px;overflow-x:auto}nav button{background:none;border:0;border-bottom:2px solid transparent;color:#6b6b66;padding:10px 12px;cursor:pointer;font-size:14px;white-space:nowrap}nav button.active{color:#1a1a1a;border-bottom-color:#1a1a1a;font-weight:600}main{max-width:860px;margin:0 auto;padding:20px 16px 160px}.pane{display:none}.pane.active{display:block}.msg{padding:11px 14px;border-radius:18px;line-height:1.55;white-space:pre-wrap;max-width:82%;margin-bottom:12px;font-size:15px}.user{margin-left:auto;background:#1a1a1a;color:#fff;border-bottom-right-radius:6px}.muse{background:#fff;border:1px solid #e5e5e0;border-bottom-left-radius:6px}.tools{font-size:12px;color:#8a8a84;margin-top:6px}form{position:fixed;left:0;right:0;bottom:0;background:#f7f7f5;border-top:1px solid #e5e5e0;padding:12px 16px}.row{max-width:860px;margin:0 auto;display:flex;gap:10px}.row input{flex:1;padding:12px 14px;border-radius:999px;border:1px solid #d9d9d3;background:#fff;font-size:15px;outline:none}.row input:focus{border-color:#1a1a1a}.row button{padding:12px 20px;border-radius:999px;border:0;background:#1a1a1a;color:#fff;font-weight:600;cursor:pointer}button.action{padding:10px 18px;border-radius:999px;border:0;background:#1a1a1a;color:#fff;font-weight:600;cursor:pointer}input,textarea,select{padding:10px 12px;border-radius:10px;border:1px solid #d9d9d3;background:#fff;font-size:14px;width:100%}label{display:block;font-size:13px;color:#6b6b66;margin:14px 0 5px}pre{background:#fff;border:1px solid #e5e5e0;padding:14px;border-radius:12px;overflow:auto;white-space:pre-wrap;font-size:13px}.card{background:#fff;border:1px solid #e5e5e0;border-radius:12px;padding:14px;margin-bottom:12px}h3{font-size:16px;margin:4px 0 12px}.note{font-size:13px;color:#8a8a84;margin-top:12px}</style></head><body>
<header><div class="bar"><h1>✦ Muse99</h1><span class="badge" id="mode">…</span><span class="muted" id="model"></span><span class="addr" id="addr"></span><span class="muted" id="usage"></span><span style="flex:1"></span><button onclick="clearChat()">Clear chat</button></div>
<nav><button class="active" onclick="tab('chat',this)">Chat</button><button onclick="tab('files',this)">Files</button><button onclick="tab('tasks',this)">Tasks</button><button onclick="tab('memory',this)">Memory</button><button onclick="tab('servers',this)">Servers</button><button onclick="tab('settings',this)">Settings</button></nav></header>
<main>
<div id="pane-chat" class="pane active"><div id="chat"></div><div class="muted" id="thinking" style="display:none">Muse99 is working…</div></div>
<div id="pane-files" class="pane"><h3>Files</h3><div id="filelist"></div><pre id="fileview">Click a file to view it.</pre></div>
<div id="pane-tasks" class="pane"><h3>Autonomous tasks</h3><div class="row" style="max-width:none;margin-bottom:12px"><input id="taskgoal" placeholder="Build a todo app in ./todo-app with tests…"><button onclick="startTask()">Start task</button></div><div id="tasklist"></div></div>
<div id="pane-memory" class="pane"><h3>Memory</h3><textarea id="mem" rows="12"></textarea><br><br><button class="action" onclick="saveMemory()">Save memory</button></div>
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
<br><br><button class="action" onclick="saveSettings()">Save settings</button>
<p class="note">Saved to ~/.muse99/config.json (600 permissions). Changing the port or SSL needs a restart of the web UI — do it from the SSH menu (Restart web UI).</p></div>
</main>
<form id="f"><div class="row"><input id="in" autocomplete="off" placeholder="Ask Muse99 to do something…"><button>Send</button></div></form>
<script>
const chat=document.getElementById('chat'),inp=document.getElementById('in'),think=document.getElementById('thinking');
document.getElementById('addr').textContent=window.location.href;
function tab(name,btn){document.querySelectorAll('.pane').forEach(p=>p.classList.remove('active'));document.getElementById('pane-'+name).classList.add('active');document.querySelectorAll('nav button').forEach(b=>b.classList.remove('active'));btn.classList.add('active');if(name==='files')loadFiles('');if(name==='tasks')loadTasks();if(name==='memory')loadMemory();if(name==='servers')loadServers();if(name==='settings')loadSettings();}
function add(t,cls,tools){const d=document.createElement('div');d.className='msg '+cls;d.textContent=t;if(tools&&tools.length){const x=document.createElement('div');x.className='tools';x.textContent='Tools: '+tools.join(' · ');d.appendChild(x);}chat.appendChild(d);window.scrollTo(0,document.body.scrollHeight);}
add("Hey Takahashi, I'm Muse99 in your browser. Give me a task and I'll work it till the end — files, shell, tests, and verification.","muse");
function fmtUsage(u){if(!u||!u.calls)return 'No tokens used yet';return u.prompt_tokens.toLocaleString()+' in / '+u.completion_tokens.toLocaleString()+' out · est. $'+Number(u.estimated_cost_usd).toFixed(4)+' (Spark rates)';}
fetch('/api/status').then(r=>r.json()).then(s=>{document.getElementById('mode').textContent=s.mode;document.getElementById('model').textContent=s.model+' · '+s.user;if(s.address)document.getElementById('addr').textContent=s.address;document.getElementById('usage').textContent=fmtUsage(s.usage);});
document.getElementById('f').addEventListener('submit',async e=>{e.preventDefault();const v=inp.value.trim();if(!v)return;add(v,'user');inp.value='';think.style.display='block';try{const r=await fetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({message:v})});const j=await r.json();think.style.display='none';add(j.reply||j.error||'(no reply)','muse',j.tools);}catch(err){think.style.display='none';add('Could not reach server: '+err,'muse');}});
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
async function loadSettings(){const r=await fetch('/api/settings');const j=await r.json();settingsData=j;document.getElementById('settings').innerHTML='Base: '+(j.base_url||'')+'<br>Model: '+(j.model||'')+'<br>User: '+(j.user_name||'')+'<br>Key: '+(j.api_key_masked||'(not set)')+'<br>Config: '+j.config_path;document.getElementById('usage-detail').textContent=fmtUsage(j.usage)+(j.usage&&j.usage.last_label?' — last: '+j.usage.last_prompt_tokens.toLocaleString()+' in / '+j.usage.last_completion_tokens.toLocaleString()+' out ('+j.usage.last_label+')':'')+' · '+((j.usage&&j.usage.calls)||0)+' API calls';const sel=document.getElementById('set-provider');sel.innerHTML=(j.providers||[]).map(p=>'<option value="'+p.name+'">'+p.name+' — '+p.model+'</option>').join('');sel.value=j.provider||'spark';document.getElementById('set-base').value=j.base_url||'';document.getElementById('set-model').value=j.model||'';document.getElementById('set-user').value=j.user_name||'';document.getElementById('set-port').value=j.port||8080;document.getElementById('set-domain').value=j.domain||'';document.getElementById('set-tls').checked=!!j.tls_enabled;sel.onchange=()=>{const p=(j.providers||[]).find(x=>x.name===sel.value);if(p){document.getElementById('set-base').value=p.base_url||'';document.getElementById('set-model').value=p.model||'';}};}
async function saveSettings(){const b={active_provider:document.getElementById('set-provider').value,base_url:document.getElementById('set-base').value,model:document.getElementById('set-model').value,api_key:document.getElementById('set-key').value,user_name:document.getElementById('set-user').value,port:parseInt(document.getElementById('set-port').value||'8080'),domain:document.getElementById('set-domain').value,tls_enabled:document.getElementById('set-tls').checked};await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});alert('Saved — restart the web UI from the SSH menu if you changed the port or SSL.');loadSettings();}
</script></body></html>`
