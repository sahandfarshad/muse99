package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupModelTakesOver(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer good.Close()
	t.Setenv("HOME", t.TempDir())
	pri := NewClientForProvider(bad.URL, "k", "main-m", "custom")
	pri.NoRetry = true
	backup := NewClientForProvider(good.URL, "k", "backup-m", "custom")
	msgs := []Message{{Role: "system", Content: "s"}}
	reply, _ := runTurnFallback(pri, backup, &toolRunner{}, nil, &msgs, "hi")
	if !strings.Contains(reply, "done") || !strings.Contains(reply, "backup-m") {
		t.Fatalf("reply %q", reply)
	}
	msgs = []Message{{Role: "system", Content: "s"}}
	if reply, _ = runTurnFallback(pri, nil, &toolRunner{}, nil, &msgs, "hi"); !strings.Contains(reply, "Sorry") {
		t.Fatalf("no-backup reply %q", reply)
	}
}

func TestRecentModels(t *testing.T) {
	var fc FileConfig
	for i := 0; i < 15; i++ {
		fc.noteRecentModel("p", "m"+string(rune('a'+i)))
	}
	fc.noteRecentModel("p", "mc")
	if len(fc.RecentModels) != maxRecentModels || fc.RecentModels[0].Model != "mc" {
		t.Fatalf("%+v", fc.RecentModels)
	}
	seen := map[string]bool{}
	for _, m := range fc.RecentModels {
		if seen[m.Model] {
			t.Fatal("duplicate")
		}
		seen[m.Model] = true
	}
}

func TestModelMenuAndBackupAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	mock := mockProviderServer(t, []string{"glm-5.3-flash"}, "OK", false)
	defer mock.Close()
	cfg := Config{UserName: "S", NameSet: true, Workdir: t.TempDir(), Provider: "spark"}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient("https://api.meta.ai/v1", "", "muse-spark-1.3"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)
	call := func(method, path, body string) string {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rr, req)
		return rr.Body.String()
	}
	call("POST", "/api/models/active", `{"provider":"glm","base_url":"`+mock.URL+`","api_key":"g","model":"glm-5.3-flash"}`)
	call("POST", "/api/models/active", `{"provider":"openrouter","api_key":"o","model":"openai/gpt-5"}`)
	if out := call("POST", "/api/models/fallback", `{"provider":"glm","model":"glm-5.3-flash"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatal(out)
	}
	st := call("GET", "/api/status", "")
	if !strings.Contains(st, `"recent":[{"provider":"openrouter","model":"openai/gpt-5"},{"provider":"glm"`) || !strings.Contains(st, `"fallback":{"provider":"glm"`) {
		t.Fatalf("status %s", st)
	}
	fb := s.fallbackClient()
	if fb == nil || fb.Model != "glm-5.3-flash" || fb.APIKey != "g" || fb.BaseURL != mock.URL {
		t.Fatalf("fallback client %+v", fb)
	}
	call("POST", "/api/models/fallback", `{"provider":"","model":""}`)
	if s.fallbackClient() != nil {
		t.Fatal("backup not cleared")
	}
}
