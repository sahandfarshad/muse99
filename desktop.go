package main

// v0.10.0: the desktop app. On Windows, double-clicking Muse99.exe
// starts the same web UI as the server version, but only on
// 127.0.0.1 (nothing outside this computer can reach it), opens it in
// its own app window, and connects to a local model server (LM Studio
// first) when no model is set up yet. Closing the window quits.
//
// No password is needed: the app opens the window with a one-time
// token that logs that window in. Other programs or web pages on the
// computer do not have the token, so they still cannot use the API.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// desktopQuit is closed by /api/desktop/quit (the Quit button).
var desktopQuit = make(chan struct{})

type desktopState struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func desktopStatePath() string { return filepath.Join(configDir(), "desktop.json") }

func desktopURL(port int, token string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/desktop?t=%s", port, token)
}

// desktopHandOff: when a desktop app is already running, just open
// its window again and report true (this process then exits).
func desktopHandOff() bool {
	b, err := os.ReadFile(desktopStatePath())
	if err != nil {
		return false
	}
	var st desktopState
	if json.Unmarshal(b, &st) != nil || st.Port == 0 || st.Token == "" {
		return false
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", st.Port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	_, _ = openAppWindow(desktopURL(st.Port, st.Token), filepath.Join(configDir(), "window"), false)
	return true
}

// desktopLog sends output to ~/.muse99/desktop.log (a windowed app has
// no console).
func desktopLog() {
	_ = os.MkdirAll(configDir(), 0o700)
	if f, err := os.OpenFile(filepath.Join(configDir(), "desktop.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		os.Stdout, os.Stderr = f, f
		fmt.Printf("\n--- Muse99 desktop %s started %s ---\n", versionLabel(), time.Now().Format(time.RFC3339))
	}
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freeLocalPort keeps the usual port when it is free, else asks the
// system for any free one.
func freeLocalPort(want int) int {
	if want > 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", want)); err == nil {
			ln.Close()
			return want
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return want
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// runDesktop starts the local web UI and the app window, and returns
// when the window is closed or Quit is pressed.
func runDesktop(cfg Config, client *Client, runner *toolRunner, tools []ToolDef, system string) error {
	cfg.WebAddr = "127.0.0.1"
	cfg.TLSEnabled = false
	cfg.WebPort = freeLocalPort(cfg.WebPort)
	cfg.DesktopToken = randomToken()
	ctrl := newWebController(cfg, client, runner, tools, system)
	if err := ctrl.Start(cfg); err != nil {
		return err
	}
	st, _ := json.Marshal(desktopState{Port: cfg.WebPort, Token: cfg.DesktopToken, PID: os.Getpid()})
	_ = os.WriteFile(desktopStatePath(), st, 0o600)
	defer os.Remove(desktopStatePath())

	url := desktopURL(cfg.WebPort, cfg.DesktopToken)
	windowDone := make(chan bool, 1)
	go func() {
		start := time.Now()
		waited, err := openAppWindow(url, filepath.Join(configDir(), "window"), true)
		if err != nil {
			fmt.Println("Could not open the app window:", err)
		}
		// A window that "closed" within a few seconds was handed to
		// another browser process: keep running until Quit instead.
		windowDone <- waited && time.Since(start) > 4*time.Second
	}()
	select {
	case <-desktopQuit:
	case closed := <-windowDone:
		if !closed {
			select {
			case <-desktopQuit:
			case <-ctrl.done:
			}
		}
	case <-ctrl.done:
	}
	_ = ctrl.Stop()
	return nil
}

// desktopGate runs before the login check. It logs the app window in
// from its one-time token, and refuses requests whose Host is not this
// computer (stops DNS-rebinding pages from talking to the API).
// Returns true when it already answered the request.
func (s *webServer) desktopGate(w http.ResponseWriter, r *http.Request) bool {
	if s.desktopToken == "" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		http.Error(w, "Muse99 desktop only answers on 127.0.0.1", http.StatusForbidden)
		return true
	}
	switch r.URL.Path {
	case "/desktop":
		t := r.URL.Query().Get("t")
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.desktopToken)) != 1 {
			http.Error(w, "wrong or old link — open Muse99 from its icon", http.StatusForbidden)
			return true
		}
		s.setSessionCookie(w, s.auth.createSession(), 30*24*3600)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return true
	case "/login":
		if !loginConfigured() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Muse99</title><body style="font-family:Segoe UI,sans-serif;padding:40px">This window lost its sign-in. Close it and open Muse99 again from its icon.</body>`)
			return true
		}
	}
	return false
}

// handleDesktopQuit serves POST /api/desktop/quit.
func (s *webServer) handleDesktopQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.desktopToken == "" {
		http.Error(w, "desktop app only", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
	go func() {
		time.Sleep(300 * time.Millisecond)
		select {
		case <-desktopQuit:
		default:
			close(desktopQuit)
		}
	}()
}

// isDesktopDefault: on Windows, starting with no arguments (a
// double-click) means the desktop app.
func isDesktopDefault(args []string, goos string) bool {
	return goos == "windows" && len(args) == 0
}
