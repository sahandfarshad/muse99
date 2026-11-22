package main

// v0.10.1: choose the work folder (the folder Muse99's file and shell
// tools may use) from Settings, and remember it. In the desktop app the
// default is ~/Muse99 instead of wherever the .exe happens to sit.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// defaultDesktopWorkdir is ~/Muse99 (created when missing).
func defaultDesktopWorkdir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return mustCwd()
	}
	d := filepath.Join(home, "Muse99")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// cleanWorkdir turns user input into an absolute folder that exists.
func cleanWorkdir(p string) (string, string) {
	p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'`))
	if p == "" {
		return "", "type a folder path"
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err.Error()
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", "folder not found: " + abs
	}
	if !st.IsDir() {
		return "", "that is a file, not a folder: " + abs
	}
	return abs, ""
}

func (r *toolRunner) setWorkdir(d string) {
	r.wdMu.Lock()
	r.workdir = d
	r.wdMu.Unlock()
}

// handleWorkdir: GET → current folder; POST {path} → use it (live, and saved).
func (s *webServer) handleWorkdir(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d, msg := cleanWorkdir(body.Path)
		if msg != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
			return
		}
		s.mu.Lock()
		s.cfg.Workdir = d
		s.mu.Unlock()
		s.runner.setWorkdir(d)
		fc := loadFileConfig()
		fc.Workdir = d
		if s.cfg.Desktop {
			fc.DesktopWorkdir = d
		}
		if err := saveFileConfig(fc); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "workdir": d})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"workdir": s.cfg.Workdir, "can_pick": canPickFolder() && s.cfg.Desktop})
}

// handlePickFolder opens the system's folder dialog (desktop app only).
func (s *webServer) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.cfg.Desktop || !canPickFolder() {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "type the folder path instead"})
		return
	}
	p, err := pickFolder(s.cfg.Workdir)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": p, "cancelled": p == ""})
}
