package main

// Web UI login.
//
// The web UI can run shell commands, read files and hold API keys, so
// it is never served without a login. The username and a bcrypt hash
// of the password live in ~/.muse99/config.json (web_user /
// web_pass_hash) and are set from the SSH menu (option 15) or, for
// Docker / systemd, from MUSE99_WEB_USER + MUSE99_WEB_PASSWORD.
//
// Sessions are random 32-byte tokens kept in memory (a restart logs
// everyone out), sent as an HttpOnly, SameSite=Strict cookie, Secure
// when TLS is on. Every non-GET request must also come from the same
// origin (Origin / Referer check) so another site cannot drive the
// API through the user's browser. Failed logins are rate limited per
// client IP.

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	sessionCookie   = "muse99_session"
	sessionTTL      = 7 * 24 * time.Hour
	loginMaxFails   = 5
	loginLockout    = 15 * time.Minute
	minPasswordLen  = 8
	bcryptCost      = 12
	maxLoginBodyLen = 4096
)

// dummyHash is compared against when the username is wrong, so a
// wrong username takes as long as a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("muse99-dummy-password"), bcryptCost)

type authStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	fails    map[string]*loginFail
}

type loginFail struct {
	count int
	until time.Time
	last  time.Time
}

func newAuthStore() *authStore {
	return &authStore{sessions: map[string]time.Time{}, fails: map[string]*loginFail{}}
}

// webCredentials returns the configured login. Env vars win, so a
// container can be configured without the menu.
func webCredentials() (user, hash string) {
	if u, p := os.Getenv("MUSE99_WEB_USER"), os.Getenv("MUSE99_WEB_PASSWORD"); u != "" && p != "" {
		envHashOnce.Do(func() {
			h, err := bcrypt.GenerateFromPassword([]byte(p), bcryptCost)
			if err == nil {
				envHash = string(h)
			}
		})
		return u, envHash
	}
	fc := loadFileConfig()
	return fc.WebUser, fc.WebPassHash
}

var (
	envHashOnce sync.Once
	envHash     string
)

func loginConfigured() bool {
	u, h := webCredentials()
	return u != "" && h != ""
}

func checkLogin(user, pass string) bool {
	wantUser, hash := webCredentials()
	if wantUser == "" || hash == "" {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) == 1
	if !userOK {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}

// setWebLogin hashes and saves a new login. It also logs out every
// running session the next time the web UI restarts.
func setWebLogin(user, pass string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return fmt.Errorf("username cannot be empty")
	}
	if len(pass) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcryptCost)
	if err != nil {
		return err
	}
	fc := loadFileConfig()
	fc.WebUser = user
	fc.WebPassHash = string(h)
	return saveFileConfig(fc)
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *authStore) createSession() string {
	t := newToken()
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[t] = now.Add(sessionTTL)
	return t
}

func (a *authStore) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *authStore) drop(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
}

func (a *authStore) locked(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.fails[ip]
	if !ok {
		return false, 0
	}
	if left := time.Until(f.until); left > 0 {
		return true, left
	}
	return false, 0
}

func (a *authStore) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.fails[ip]
	if !ok || time.Since(f.last) > loginLockout {
		f = &loginFail{}
		a.fails[ip] = f
	}
	f.count++
	f.last = time.Now()
	if f.count >= loginMaxFails {
		f.until = time.Now().Add(loginLockout)
		f.count = 0
	}
}

func (a *authStore) clearFails(ip string) {
	a.mu.Lock()
	delete(a.fails, ip)
	a.mu.Unlock()
}

// clientIP is the TCP peer. X-Forwarded-For is deliberately ignored:
// it is set by the client and would let anyone dodge the rate limit.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOrigin rejects cross-site writes. Browsers always send Origin on
// POST/DELETE from fetch; a missing Origin and Referer is allowed only
// for non-browser clients (curl), which cannot carry the cookie
// anyway unless the user gave it to them.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *webServer) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.tlsActive,
		SameSite: http.SameSiteStrictMode,
	})
}

// withAuth wraps the whole mux. Only /login, /logout and a minimal
// /api/status (so the menu and installer can see the UI is up) are
// reachable without a session.
func (s *webServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if s.tlsActive {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if s.desktopGate(w, r) {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/login":
			s.handleLogin(w, r)
			return
		case "/logout":
			s.auth.drop(r)
			s.setSessionCookie(w, "", -1)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if s.auth.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/status" {
			// Liveness only — no settings, no usage, no address.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"auth":"required"}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *webServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !loginConfigured() {
		writeLoginPage(w, "", "No web login is set yet. On the server, run muse99 and choose option 15 (Web login) to set a username and password.", false)
		return
	}
	if r.Method == http.MethodGet {
		if s.auth.valid(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		writeLoginPage(w, "", "", true)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if locked, left := s.auth.locked(ip); locked {
		w.WriteHeader(http.StatusTooManyRequests)
		writeLoginPage(w, "", fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(left.Minutes())+1), true)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyLen)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user, pass := r.PostFormValue("username"), r.PostFormValue("password")
	if !checkLogin(user, pass) {
		s.auth.recordFail(ip)
		w.WriteHeader(http.StatusUnauthorized)
		writeLoginPage(w, user, "Wrong username or password.", true)
		return
	}
	s.auth.clearFails(ip)
	s.setSessionCookie(w, s.auth.createSession(), int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func writeLoginPage(w http.ResponseWriter, user, msg string, form bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body := ""
	if msg != "" {
		body += `<p class="msg">` + html.EscapeString(msg) + `</p>`
	}
	if form {
		body += `<form method="post" action="/login">
<label>Username<input name="username" autocomplete="username" autofocus required value="` + html.EscapeString(user) + `"></label>
<label>Password<input name="password" type="password" autocomplete="current-password" required></label>
<button type="submit">Sign in</button></form>`
	}
	fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Muse99 — Sign in</title>
<style>
:root{--bg:#f7f5f1;--surface:#fffdfa;--ink:#1d1b18;--ink2:#5d5850;--muted:#9a948a;--line:#e9e4dc;--line2:#f0ece5;--glow:#e8dccb}
@media (prefers-color-scheme:dark){:root{--bg:#161513;--surface:#1f1d1a;--ink:#f1ece4;--ink2:#b9b2a7;--muted:#7d776d;--line:#2e2b27;--line2:#27241f;--glow:#4a3f30}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:radial-gradient(1200px 600px at 50% -10%,var(--surface),var(--bg));font:15px/1.5 ui-sans-serif,-apple-system,"SF Pro Text","Segoe UI",Roboto,sans-serif;color:var(--ink);-webkit-font-smoothing:antialiased}
.card{background:var(--surface);border:1px solid var(--line);border-radius:22px;box-shadow:0 2px 4px rgba(40,30,20,.04),0 20px 50px rgba(40,30,20,.10);padding:34px 30px 28px;width:min(380px,92vw)}
h1{margin:0 0 4px;font-size:22px;letter-spacing:-.02em;display:flex;align-items:center;gap:10px}
.orb{width:28px;height:28px;border-radius:50%;background:radial-gradient(circle at 30% 30%,#fff 0,#f1e6d6 35%,#c9b49a 100%);box-shadow:inset 0 0 0 1px rgba(0,0,0,.06),0 2px 8px rgba(150,120,80,.25)}
.sub{color:var(--muted);font-size:13.5px;margin:0 0 22px}
label{display:block;font-size:13px;color:var(--ink2);margin-bottom:14px;font-weight:550}
input{display:block;width:100%;margin-top:6px;padding:11px 13px;border:1px solid var(--line);border-radius:12px;background:var(--line2);color:var(--ink);font-size:15px;outline:none;transition:all .15s}
input:focus{border-color:var(--ink2);background:var(--surface);box-shadow:0 0 0 3px var(--glow)}
button{width:100%;margin-top:8px;padding:12px;border:0;border-radius:999px;background:var(--ink);color:var(--bg);font-size:15px;font-weight:600;cursor:pointer}
button:hover{opacity:.9}
.msg{background:#fbefe9;border:1px solid #efcfc2;color:#8a3a20;border-radius:12px;padding:10px 13px;font-size:13.5px;margin:0 0 16px}
</style></head><body><div class="card"><h1><span class="orb"></span>Muse99</h1><p class="sub">Sign in to your agent.</p>`+body+`</div></body></html>`)
}

// webLoginMenu is SSH menu option 15.
func webLoginMenu(scanner *bufio.Scanner) {
	user, hash := webCredentials()
	if os.Getenv("MUSE99_WEB_USER") != "" && os.Getenv("MUSE99_WEB_PASSWORD") != "" {
		fmt.Println("The web login comes from MUSE99_WEB_USER / MUSE99_WEB_PASSWORD in the environment.")
		fmt.Println("Change it there; a login saved here is used only when those are not set.")
	}
	if user != "" && hash != "" {
		fmt.Println("Current web login user:", user)
	} else {
		fmt.Println("No web login is set yet — the web UI shows a locked page until you set one.")
	}
	newUser, ok := prompt(scanner, "Username (0 to cancel)", user)
	if !ok {
		fmt.Println("Cancelled — nothing changed.")
		return
	}
	pass, ok := readPassword(scanner, fmt.Sprintf("New password (min %d chars, 0 to cancel): ", minPasswordLen))
	if !ok {
		fmt.Println("Cancelled — nothing changed.")
		return
	}
	again, ok := readPassword(scanner, "Repeat password: ")
	if !ok {
		fmt.Println("Cancelled — nothing changed.")
		return
	}
	if pass != again {
		fmt.Println("Passwords do not match. Nothing changed.")
		return
	}
	if err := setWebLogin(newUser, pass); err != nil {
		fmt.Println("Not saved:", err)
		return
	}
	fmt.Println("Web login saved. Restart the web UI (option 4) to sign everyone out of old sessions.")
}

// readPassword reads without echo when stdin is a terminal, and falls
// back to the line scanner otherwise (tests, pipes).
func readPassword(scanner *bufio.Scanner, label string) (string, bool) {
	fmt.Print("  " + label)
	var v string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", false
		}
		v = string(b)
	} else {
		if !scanner.Scan() {
			return "", false
		}
		v = scanner.Text()
	}
	if strings.TrimSpace(v) == "0" {
		return "", false
	}
	return v, true
}
