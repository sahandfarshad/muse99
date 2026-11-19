package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authTestServer(t *testing.T) (*webServer, http.Handler) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MUSE99_WEB_USER", "")
	t.Setenv("MUSE99_WEB_PASSWORD", "")
	s := &webServer{cfg: Config{Workdir: t.TempDir()}}
	return s, buildHandler(s)
}

func login(t *testing.T, h http.Handler, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAPILockedWithoutLogin(t *testing.T) {
	_, h := authTestServer(t)
	for _, p := range []string{"/api/settings", "/api/file?path=/etc/passwd", "/api/chat"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s without login = %d, want 401", p, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("/ without login should redirect to /login, got %d", rr.Code)
	}
	// Liveness probe still answers 200, without settings.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "model") {
		t.Fatalf("/api/status without login = %d %q", rr.Code, rr.Body.String())
	}
}

func TestLoginFlow(t *testing.T) {
	_, h := authTestServer(t)
	if rr := login(t, h, "admin", "whatever1"); rr.Code == http.StatusSeeOther {
		t.Fatal("login must fail when no login is configured")
	}
	if err := setWebLogin("admin", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := setWebLogin("admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	if fc := loadFileConfig(); fc.WebPassHash == "" || strings.Contains(fc.WebPassHash, "correct-horse") {
		t.Fatal("password must be stored only as a hash")
	}
	if rr := login(t, h, "admin", "wrong-pass"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", rr.Code)
	}
	rr := login(t, h, "admin", "correct-horse")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("good login = %d", rr.Code)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie not hardened: %+v", cookies)
	}
	req := httptest.NewRequest("GET", "/api/files", nil)
	req.AddCookie(cookies[0])
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("/api/files with session = %d", rr.Code)
	}
	// Cross-site write is blocked even with the cookie.
	req = httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"allow_shell":true}`))
	req.AddCookie(cookies[0])
	req.Header.Set("Origin", "https://evil.example")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", rr.Code)
	}
}

func TestLoginLockout(t *testing.T) {
	_, h := authTestServer(t)
	_ = setWebLogin("admin", "correct-horse")
	for i := 0; i < loginMaxFails; i++ {
		login(t, h, "admin", "nope-nope")
	}
	if rr := login(t, h, "admin", "correct-horse"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d fails, login = %d, want 429", loginMaxFails, rr.Code)
	}
}

func TestPathInWorkdir(t *testing.T) {
	wd := t.TempDir()
	_ = os.WriteFile(filepath.Join(wd, "a.txt"), []byte("x"), 0o644)
	if _, ok := pathInWorkdir(wd, "a.txt"); !ok {
		t.Fatal("relative path inside workdir rejected")
	}
	for _, bad := range []string{"/etc/passwd", "../x", "sub/../../x"} {
		if _, ok := pathInWorkdir(wd, bad); ok {
			t.Fatalf("%q should be rejected", bad)
		}
	}
	_ = os.Symlink("/etc", filepath.Join(wd, "link"))
	if _, ok := pathInWorkdir(wd, "link/passwd"); ok {
		t.Fatal("symlink escape accepted")
	}
}
