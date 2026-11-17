package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Muse99 Web UI — full dashboard, no build step, single binary.
//   muse99 --web --port 8080 --allow-shell
// Tabs: Chat | Files | Tasks | Memory | Servers | Settings
// The address is shown at the top of the page AND in the SSH menu.

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
		if fc.BaseURL != "" {
			old.BaseURL = fc.BaseURL
			s.cfg.BaseURL = fc.BaseURL
		}
		if fc.APIKey != "" {
			old.APIKey = fc.APIKey
			s.cfg.APIKey = fc.APIKey
			*s.client = *NewClient(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model)
		}
		if fc.Model != "" {
			old.Model = fc.Model
			s.cfg.Model = fc.Model
			*s.client = *NewClient(s.cfg.BaseURL, s.cfg.APIKey, s.cfg.Model)
		}
		if fc.UserName != "" {
			old.UserName = fc.UserName
			s.cfg.UserName = fc.UserName
		}
		_ = saveFileConfig(old)
	}
	w.Header().Set("Content-Type", "application/json")
	fc := loadFileConfig()
	masked := ""
	if fc.APIKey != "" {
		masked = "••••••••" + fc.APIKey[len(fc.APIKey)-4:]
		if len(fc.APIKey) < 4 {
			masked = "••••"
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]string{
		"base_url": fc.BaseURL, "model": fc.Model, "user_name": fc.UserName,
		"api_key_masked": masked, "config_path": configPath(),
	})
}

func startWeb(cfg Config, client *Client, runner *toolRunner, tools []ToolDef, system string) error {
	s := &webServer{client: client, runner: runner, tools: tools, cfg: cfg, messages: []Message{{Role: "system", Content: system}}, system: system}
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
	addr := fmt.Sprintf("%s:%d", cfg.WebAddr, cfg.WebPort)
	fmt.Printf("Muse99 web UI: http://%s\n", addr)
	fmt.Printf("Model: %s · Workdir: %s\n", cfg.Model, cfg.Workdir)
	return http.ListenAndServe(addr, mux)
}

var _ = html.EscapeString

const webHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Muse99 — Takahashi's agent</title>
<style>*{box-sizing:border-box}body{margin:0;font-family:system-ui,sans-serif;background:#0f1115;color:#e8eaf0}header{padding:12px 20px;border-bottom:1px solid #222633;display:flex;gap:10px;align-items:center;flex-wrap:wrap}h1{font-size:18px;margin:0}.badge{background:#6c5ce7;color:#fff;padding:4px 10px;border-radius:999px;font-size:12px}.muted{color:#9aa0b4;font-size:13px}nav{display:flex;gap:6px;padding:10px 20px;border-bottom:1px solid #222633;flex-wrap:wrap}nav button{background:#1c2030;color:#e8eaf0;border:1px solid #2a3042;padding:8px 14px;border-radius:999px;cursor:pointer}nav button.active{background:#6c5ce7;border-color:#6c5ce7}main{max-width:900px;margin:0 auto;padding:20px 16px 140px}.pane{display:none}.pane.active{display:block}.msg{padding:12px 14px;border-radius:14px;line-height:1.55;white-space:pre-wrap;max-width:88%;margin-bottom:12px}.user{margin-left:auto;background:#2563eb;color:#fff}.muse{background:#1c2030;border:1px solid #2a3042}.tools{font-size:12px;color:#9aa0b4;margin-top:6px}form{position:fixed;left:0;right:0;bottom:0;background:#0f1115;border-top:1px solid #222633;padding:14px}.row{max-width:900px;margin:0 auto;display:flex;gap:10px}input,textarea,select{flex:1;padding:10px 12px;border-radius:10px;border:1px solid #333a4d;background:#1c2030;color:#fff;font-size:14px}button{padding:10px 18px;border-radius:999px;border:0;background:#6c5ce7;color:#fff;font-weight:600;cursor:pointer}pre{background:#1c2030;padding:14px;border-radius:10px;overflow:auto;white-space:pre-wrap}.card{background:#1c2030;border:1px solid #2a3042;border-radius:12px;padding:14px;margin-bottom:12px}</style></head><body>
<header><h1>✦ Muse99</h1><span class="badge" id="mode">…</span><span class="muted" id="model"></span><span class="muted" id="addr"></span><span style="flex:1"></span><button onclick="clearChat()">Clear chat</button></header>
<nav><button class="active" onclick="tab('chat',this)">Chat</button><button onclick="tab('files',this)">Files</button><button onclick="tab('tasks',this)">Tasks</button><button onclick="tab('memory',this)">Memory</button><button onclick="tab('servers',this)">Servers</button><button onclick="tab('settings',this)">Settings</button></nav>
<main>
<div id="pane-chat" class="pane active"><div id="chat"></div><div class="muted" id="thinking" style="display:none">Muse99 is working…</div></div>
<div id="pane-files" class="pane"><h3>Files</h3><div id="filelist"></div><pre id="fileview">Click a file to view it.</pre></div>
<div id="pane-tasks" class="pane"><h3>Autonomous tasks</h3><div class="row" style="max-width:none;margin-bottom:12px"><input id="taskgoal" placeholder="Build a todo app in ./todo-app with tests…"><button onclick="startTask()">Start task</button></div><div id="tasklist"></div></div>
<div id="pane-memory" class="pane"><h3>Memory</h3><textarea id="mem" rows="12" style="width:100%"></textarea><br><br><button onclick="saveMemory()">Save memory</button></div>
<div id="pane-servers" class="pane"><h3>Servers — give Muse99 access only if you want it to</h3><div class="row" style="max-width:none;flex-wrap:wrap"><input id="srv-name" placeholder="Name"><input id="srv-user" placeholder="user"><input id="srv-host" placeholder="host / IP"><input id="srv-port" placeholder="22"></div><br><button onclick="addServer()">Add server</button><div id="srvlist" style="margin-top:14px"></div><p class="muted">Remote commands use your system ssh key. No server is used unless you add it and ask for it.</p></div>
<div id="pane-settings" class="pane"><h3>Settings</h3><div id="settings"></div><br><input id="set-base" placeholder="API base URL"><br><br><input id="set-model" placeholder="Model, e.g. muse-spark-1.4"><br><br><input id="set-key" placeholder="API key (leave blank to keep)"><br><br><input id="set-user" placeholder="Your name"><br><br><button onclick="saveSettings()">Save settings</button><p class="muted">Saved to ~/.muse99/config.json (600 permissions). Restart to fully apply model changes in the menu.</p></div>
</main>
<form id="f"><div class="row"><input id="in" autocomplete="off" placeholder="Ask Muse99 to do something…"><button>Send</button></div></form>
<script>
const chat=document.getElementById('chat'),inp=document.getElementById('in'),think=document.getElementById('thinking');
document.getElementById('addr').textContent=window.location.href;
function tab(name,btn){document.querySelectorAll('.pane').forEach(p=>p.classList.remove('active'));document.getElementById('pane-'+name).classList.add('active');document.querySelectorAll('nav button').forEach(b=>b.classList.remove('active'));btn.classList.add('active');if(name==='files')loadFiles('');if(name==='tasks')loadTasks();if(name==='memory')loadMemory();if(name==='servers')loadServers();if(name==='settings')loadSettings();}
function add(t,cls,tools){const d=document.createElement('div');d.className='msg '+cls;d.textContent=t;if(tools&&tools.length){const x=document.createElement('div');x.className='tools';x.textContent='🛠 '+tools.join(' · ');d.appendChild(x);}chat.appendChild(d);window.scrollTo(0,document.body.scrollHeight);}
add("Hey Takahashi, I'm Muse99 in your browser. Give me a task and I'll work it till the end — files, shell, tests, and verification.","muse");
fetch('/api/status').then(r=>r.json()).then(s=>{document.getElementById('mode').textContent=s.mode;document.getElementById('model').textContent=s.model+' · '+s.user;});
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
async function loadSettings(){const r=await fetch('/api/settings');const j=await r.json();document.getElementById('settings').innerHTML='Base: '+(j.base_url||'')+'<br>Model: '+(j.model||'')+'<br>User: '+(j.user_name||'')+'<br>Key: '+(j.api_key_masked||'(not set)')+'<br>Config: '+j.config_path;}
async function saveSettings(){const b={base_url:document.getElementById('set-base').value,model:document.getElementById('set-model').value,api_key:document.getElementById('set-key').value,user_name:document.getElementById('set-user').value};await fetch('/api/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});alert('Saved');loadSettings();}
</script></body></html>`
