package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func toolDefinitions() []ToolDef {
	def := func(name, desc string, props map[string]any, required []string) ToolDef {
		var t ToolDef
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = map[string]any{
			"type": "object", "properties": props, "required": required,
		}
		return t
	}
	str := func() map[string]any { return map[string]any{"type": "string"} }
	boolean := func() map[string]any { return map[string]any{"type": "boolean"} }
	_ = boolean

	return []ToolDef{
		def("read_file", "Read a file as text.", map[string]any{"path": str()}, []string{"path"}),
		def("write_file", "Write (create or overwrite) a file. Create parent folders as needed.", map[string]any{"path": str(), "content": str()}, []string{"path", "content"}),
		def("edit_file", "Replace old_text with new_text in a file. Use for precise edits, not full rewrites.", map[string]any{"path": str(), "old_text": str(), "new_text": str()}, []string{"path", "old_text", "new_text"}),
		def("list_files", "List files in a directory.", map[string]any{"path": str()}, []string{"path"}),
		def("search_files", "Search file contents for text (like grep -r). Returns file:line matches.", map[string]any{"query": str(), "path": str()}, []string{"query"}),
		def("run_shell", "Run a shell command ("+shellName()+" on this computer), return stdout/stderr. Use for build, test, git, docker.", map[string]any{"command": str()}, []string{"command"}),
		def("remote_shell", "Run a command on one of the user's servers over SSH. server = the server's name from the Servers list in your instructions.", map[string]any{"server": str(), "command": str()}, []string{"server", "command"}),
		def("git_run", "Run a safe git command in the workdir, e.g. status, log --oneline -10, diff --stat, add, commit -m.", map[string]any{"command": str()}, []string{"command"}),
		def("fetch_url", "Fetch a URL as clean text (title + main text). Lightweight HTTP fetch by default.", map[string]any{"url": str()}, []string{"url"}),
		def("browse_url", "Browse a URL and return its title and clean text. Set js=true only when the page needs JavaScript — that uses headless Chromium (~300-500 MB RAM) if installed, and returns a clear install message if it is not.", map[string]any{"url": str(), "js": boolean()}, []string{"url"}),
		def("web_search", "Search the internet. Returns up to 8 results (title, link, snippet). Then use fetch_url to read a page.", map[string]any{"query": str()}, []string{"query"}),
		def("scaffold_app", "Create a starter app in a folder: kind is one of go-cli, web-html, python-cli, node-express. Creates real files, README and a run script.", map[string]any{"kind": str(), "name": str(), "path": str()}, []string{"kind", "name"}),
		def("spawn_subtask", "Split off an independent sub-task to a sub-agent. It runs its own tool loop (max 10 steps) and returns a summary. Use for parallel pieces of a big task.", map[string]any{"task": str()}, []string{"task"}),
		def("get_time", "Get the current date and time.", map[string]any{}, []string{}),
		def("remember", "Save a durable fact about the user to MEMORY.md.", map[string]any{"fact": str()}, []string{"fact"}),
		def("set_goal", "Save a user goal (title + status).", map[string]any{"title": str()}, []string{"title"}),
	}
}

type toolRunner struct {
	workdir    string
	memoryPath string
	allowShell bool
	// confine (desktop app): file tools may only touch the work folder.
	confine bool
	wdMu    sync.RWMutex
	shellMu sync.Mutex
	// delegate is set by main for spawn_subtask — a sub-agent turn.
	delegate func(task string) string
	client   *Client
	tools    []ToolDef
}

// SetAllowShell changes the shell setting live (web checkbox / menu
// toggle). No restart needed — the next tool call sees the new value.
func (r *toolRunner) SetAllowShell(v bool) {
	r.shellMu.Lock()
	defer r.shellMu.Unlock()
	r.allowShell = v
}

func (r *toolRunner) shellAllowed() bool {
	r.shellMu.Lock()
	defer r.shellMu.Unlock()
	return r.allowShell
}

func (r *toolRunner) run(name, argsJSON string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)
	get := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}

	switch name {
	case "read_file":
		p, perr := r.safe(get("path"))
		if perr != nil {
			return "error: " + perr.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		return truncate(string(data), 20000)

	case "write_file":
		p, perr := r.safe(get("path"))
		if perr != nil {
			return "error: " + perr.Error()
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "error: " + err.Error()
		}
		if err := os.WriteFile(p, []byte(get("content")), 0o644); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("wrote %s (%d bytes)", p, len(get("content")))

	case "edit_file":
		p, perr := r.safe(get("path"))
		if perr != nil {
			return "error: " + perr.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		oldText, newText := get("old_text"), get("new_text")
		if !strings.Contains(string(data), oldText) {
			return "error: old_text not found in file — read the file first and copy it exactly"
		}
		out := strings.Replace(string(data), oldText, newText, 1)
		if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
			return "error: " + err.Error()
		}
		return "edited " + p

	case "list_files":
		p, perr := r.safe(get("path"))
		if perr != nil {
			return "error: " + perr.Error()
		}
		if p == "" {
			p = r.workdir
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, e.Name()+"/")
			} else {
				out = append(out, e.Name())
			}
		}
		return strings.Join(out, "\n")

	case "search_files":
		q := get("query")
		root, perr := r.safe(get("path"))
		if perr != nil {
			return "error: " + perr.Error()
		}
		if root == "" {
			root = r.workdir
		}
		var out []string
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Size() > 500000 {
				return nil
			}
			if strings.Contains(p, "/.git/") || strings.Contains(p, "/node_modules/") {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(strings.ToLower(line), strings.ToLower(q)) {
					out = append(out, fmt.Sprintf("%s:%d: %s", p, i+1, strings.TrimSpace(line)))
					if len(out) > 100 {
						return filepath.SkipAll
					}
				}
			}
			return nil
		})
		if len(out) == 0 {
			return "no matches for " + q
		}
		return strings.Join(out, "\n")

	case "run_shell":
		if !r.shellAllowed() {
			return "error: shell is disabled. Turn on 'Allow shell commands' in the web UI (Settings) or the SSH menu, or run with --allow-shell."
		}
		return r.shell(get("command"), r.workdir)

	case "remote_shell":
		srv, ok := findServer(get("server"))
		if !ok {
			return "error: unknown server " + strconvQuote(get("server")) + ". Only servers the user added in the Servers tab can be used."
		}
		out, err := runRemote(srv, get("command"), shellTimeout())
		if err != nil {
			return "error: " + err.Error() + "\n" + truncate(out, 20000)
		}
		return truncate(out, 20000)

	case "git_run":
		if !r.shellAllowed() {
			return "error: shell is disabled."
		}
		// Block destructive / credential operations.
		lower := strings.ToLower(get("command"))
		for _, bad := range []string{"push --force", "reset --hard", "clean -f", "credential"} {
			if strings.Contains(lower, bad) {
				return "error: blocked git command: " + bad
			}
		}
		return r.shell("git "+get("command"), r.workdir)

	case "fetch_url":
		return browsePage(get("url"), false)

	case "browse_url":
		useJS := false
		if v, ok := args["js"].(bool); ok {
			useJS = v
		}
		return browsePage(get("url"), useJS)

	case "web_search":
		return webSearch(get("query"))

	case "scaffold_app":
		return r.scaffold(get("kind"), get("name"), get("path"))

	case "spawn_subtask":
		if r.delegate == nil {
			return "error: subagents not available in this mode"
		}
		return r.delegate(get("task"))

	case "get_time":
		return time.Now().Format("Monday 2006-01-02 15:04:05 MST")

	case "remember":
		if err := rememberFact(r.memoryPath, get("fact")); err != nil {
			return "error: " + err.Error()
		}
		return "saved to memory"

	case "set_goal":
		goals := loadGoals()
		goals = append(goals, Goal{Title: get("title"), Status: "active", CreatedAt: time.Now().Format("2006-01-02")})
		_ = saveGoals(goals)
		return "goal saved: " + get("title")

	default:
		return "error: unknown tool " + name
	}
}

// shellTimeout is how long one shell command may run before it is
// killed. Default 120s; override with MUSE99_SHELL_TIMEOUT (seconds,
// 5..3600). A stuck command (a server that never exits, an interactive
// prompt, an endless loop) no longer hangs the agent forever.
func shellTimeout() time.Duration {
	d := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("MUSE99_SHELL_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 3600 {
			d = time.Duration(n) * time.Second
		}
	}
	return d
}

func (r *toolRunner) shell(command, dir string) string {
	return runShellWithTimeout(command, dir, shellTimeout())
}

func runShellWithTimeout(command, dir string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(ctl.context(), timeout)
	defer cancel()
	cmd := shellCommand(ctx, command)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.WaitDelay = 3 * time.Second
	out, err := cmd.CombinedOutput()
	s := truncate(string(out), 10000)
	if ctx.Err() == context.Canceled {
		return "error: stopped by the user (Stop button); the command was killed.\n" + s
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("error: command timed out after %s and was killed. For a server or long job, run it in the background (%s) and check its log.\n%s", timeout, backgroundHint(), s)
	}
	if err != nil {
		return fmt.Sprintf("exit error: %v\n%s", err, s)
	}
	return s
}

func (r *toolRunner) scaffold(kind, name, path string) string {
	if name == "" {
		name = "app"
	}
	dir, perr := r.safe(path)
	if perr != nil {
		return "error: " + perr.Error()
	}
	if dir == r.workdir || path == "" {
		dir = filepath.Join(r.workdir, name)
	}
	_ = os.MkdirAll(dir, 0o755)
	write := func(rel, content string) {
		_ = os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644)
	}
	switch kind {
	case "web-html":
		write("index.html", "<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>"+name+"</title><link rel=\"stylesheet\" href=\"style.css\"></head><body><h1>"+name+"</h1><p>Built by Muse99.</p><script src=\"app.js\"></script></body></html>\n")
		write("style.css", "body{font-family:system-ui,sans-serif;max-width:720px;margin:40px auto;padding:0 16px}\n")
		write("app.js", "console.log('"+name+" ready');\n")
	case "python-cli":
		write("main.py", "def main():\n    print('Hello from "+name+"')\n\nif __name__ == '__main__':\n    main()\n")
		write("requirements.txt", "")
	case "node-express":
		write("package.json", "{\"name\":\""+name+"\",\"version\":\"1.0.0\",\"main\":\"index.js\",\"scripts\":{\"start\":\"node index.js\"},\"dependencies\":{\"express\":\"^4.19.0\"}}\n")
		write("index.js", "const express=require('express');const app=express();app.get('/',(req,res)=>res.send('Hello from "+name+"'));app.listen(3000,()=>console.log('http://localhost:3000'));\n")
	default: // go-cli
		write("go.mod", "module "+name+"\n\ngo 1.23\n")
		write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"Hello from "+name+"\") }\n")
	}
	write("README.md", "# "+name+"\n\nBuilt by Muse99.\n\nRun: see the project files.\n")
	return "scaffolded " + kind + " app at " + dir
}

// safe resolves a tool path. In the desktop app (confine) the result
// must stay inside the work folder: absolute paths elsewhere, "..", and
// symlinks that lead out are refused.
func (r *toolRunner) safe(p string) (string, error) {
	r.wdMu.RLock()
	wd := r.workdir
	r.wdMu.RUnlock()
	full := safePath(wd, p)
	if !r.confine {
		return full, nil
	}
	if !insideDir(wd, full) {
		return "", fmt.Errorf("%s is outside the work folder %s. Only files in the work folder can be used (change it in Settings > Work folder)", full, wd)
	}
	return full, nil
}

// insideDir reports whether path is dir or below it, after following
// symlinks of the parts that exist.
func insideDir(dir, path string) bool {
	real := func(p string) string {
		p = filepath.Clean(p)
		rest := ""
		for {
			if rp, err := filepath.EvalSymlinks(p); err == nil {
				return filepath.Join(rp, rest)
			}
			parent := filepath.Dir(p)
			if parent == p {
				return filepath.Join(p, rest)
			}
			rest = filepath.Join(filepath.Base(p), rest)
			p = parent
		}
	}
	d, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	pa, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(real(d), real(pa))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func safePath(workdir, p string) string {
	if p == "" {
		return workdir
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(workdir, p)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n...[truncated]"
	}
	return s
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }
