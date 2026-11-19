package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsagePerModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	before := loadUsage()
	recordUsageFor("custom", "glm-5.3-flash", 1000, 200)
	recordUsageFor("custom", "glm-5.3-flash", 500, 100)
	recordUsageFor("openai", "gpt-5", 10, 5)
	u := loadUsage()
	if u.LastModel != "gpt-5" {
		t.Fatalf("last model = %q", u.LastModel)
	}
	list := modelUsageList(u)
	if len(list) != 2 || list[0].Model != "glm-5.3-flash" || list[0].TotalTokens != 1800 || list[0].Calls != 2 || list[0].Provider != "custom" {
		t.Fatalf("per-model list wrong: %+v", list)
	}
	d := diffUsage(before, u)
	if d.TotalTokens != 1815 || d.Calls != 3 || d.Model != "glm-5.3-flash, gpt-5" {
		t.Fatalf("diff wrong: %+v", d)
	}
	mid := loadUsage()
	recordUsageFor("custom", "glm-5.3-flash", 7, 3)
	if d := diffUsage(mid, loadUsage()); d.Model != "glm-5.3-flash" || d.TotalTokens != 10 {
		t.Fatalf("single-reply diff wrong: %+v", d)
	}
}

func TestSavedUserNameLegacy(t *testing.T) {
	if _, ok := (FileConfig{UserName: "Takahashi"}).savedUserName(); ok {
		t.Fatal("legacy default name must not count as set")
	}
	if n, ok := (FileConfig{UserName: "Takahashi", NameSet: true}).savedUserName(); !ok || n != "Takahashi" {
		t.Fatal("a confirmed Takahashi is a real name")
	}
	if n, ok := (FileConfig{UserName: "  Sahand  "}).savedUserName(); !ok || n != "Sahand" {
		t.Fatalf("custom name from an older version should count: %q %v", n, ok)
	}
	if _, ok := (FileConfig{}).savedUserName(); ok {
		t.Fatal("empty name is not set")
	}
}

func TestSetUserNamePersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{}
	if err := setUserName(&cfg, "Sahand"); err != nil {
		t.Fatal(err)
	}
	other := Config{}
	syncUserName(&other)
	if !other.NameSet || other.UserName != "Sahand" {
		t.Fatalf("name not shared via config: %+v", other)
	}
	// Saving the runtime config must keep it.
	_ = saveFileConfig(configFromRuntime(other))
	if n, ok := loadFileConfig().savedUserName(); !ok || n != "Sahand" {
		t.Fatalf("name lost on save: %q", n)
	}
}

func TestNormalizeTagAndURL(t *testing.T) {
	if normalizeTag("0.7.2") != "v0.7.2" || normalizeTag("v0.7.2") != "v0.7.2" || normalizeTag("latest") != "" {
		t.Fatal("normalizeTag wrong")
	}
	if got := releaseURL("v0.7.2", "arm64"); got != "https://github.com/sahandfarshad/muse99/releases/download/v0.7.2/muse99-linux-arm64" {
		t.Fatal(got)
	}
}

func TestPrevVersionFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "muse99")
	if prevVersion(exe) != "" {
		t.Fatal("no prev expected")
	}
	_ = os.WriteFile(prevPath(exe), []byte("#!/bin/sh\nexit 2\n"), 0o755)
	_ = os.WriteFile(prevPath(exe)+".version", []byte("v0.7.2\n"), 0o644)
	if got := prevVersion(exe); got != "v0.7.2" {
		t.Fatalf("prevVersion = %q", got)
	}
}

func TestToolHistoryRepair(t *testing.T) {
	call := func(id, name string) ToolCall {
		var tc ToolCall
		tc.ID = id
		tc.Function.Name = name
		return tc
	}
	msgs := []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{call("", "read_file"), call("", "list_dir")}},
		{Role: "tool", Content: "a"},
		{Role: "tool", Content: "b"},
		{Role: "assistant", Content: "done"},
		{Role: "tool", Content: "orphan"},
		{Role: "user", Content: "next"},
		{Role: "assistant", ToolCalls: []ToolCall{call("x1", "run_shell")}},
		{Role: "user", Content: "interrupted"},
	}
	fillToolCallIDs(msgs)
	a := msgs[2].ToolCalls
	if a[0].ID == "" || a[1].ID == "" || msgs[3].ToolCallID != a[0].ID || msgs[4].ToolCallID != a[1].ID {
		t.Fatalf("ids not filled/paired: %+v %q %q", a, msgs[3].ToolCallID, msgs[4].ToolCallID)
	}
	out := sanitizeToolHistory(msgs)
	// Every tool message must answer a call in the assistant message before it.
	var calls map[string]bool
	for i, m := range out {
		switch m.Role {
		case "assistant":
			calls = map[string]bool{}
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = true
			}
		case "tool":
			if m.ToolCallID == "" || !calls[m.ToolCallID] {
				t.Fatalf("message %d: bad tool result %+v", i, m)
			}
			delete(calls, m.ToolCallID)
		default:
			if len(calls) > 0 {
				t.Fatalf("message %d: calls %v left without results", i, calls)
			}
			calls = nil
		}
	}
	found := false
	for _, m := range out {
		if m.Role == "user" && m.Content == "[earlier tool result]\norphan" {
			found = true
		}
	}
	if !found {
		t.Fatal("orphan tool result should become a user note")
	}
}

func TestProviderKeysSurviveSwitching(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	mock := mockProviderServer(t, []string{"glm-5.3-flash"}, "OK", false)
	defer mock.Close()
	cfg := Config{UserName: "S", NameSet: true, Workdir: t.TempDir(), Provider: "spark"}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient("https://api.meta.ai/v1", "", "muse-spark-1.3"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)
	post := func(path, body string) string {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rr, req)
		return rr.Body.String()
	}
	// A new named provider: fetch models with a typed key saves it.
	if out := post("/api/models/refresh", `{"provider":"glm","base_url":"`+mock.URL+`","api_key":"glm-key"}`); !strings.Contains(out, "glm-5.3-flash") {
		t.Fatalf("fetch: %s", out)
	}
	if fc := loadFileConfig(); fc.Providers["glm"].APIKey != "glm-key" || fc.ActiveProvider == "glm" {
		t.Fatalf("fetch should save the key without switching: %+v", fc.Providers["glm"])
	}
	post("/api/models/active", `{"provider":"glm","model":"glm-5.3-flash"}`)
	// Another provider set up and made active.
	post("/api/models/active", `{"provider":"openrouter","api_key":"or-key","model":"openai/gpt-5"}`)
	// Switch back to glm from Settings with a blank key field.
	post("/api/settings", `{"active_provider":"glm","base_url":"`+mock.URL+`","api_key":""}`)
	fc := loadFileConfig()
	if fc.Providers["glm"].APIKey != "glm-key" || fc.Providers["openrouter"].APIKey != "or-key" {
		t.Fatalf("keys lost: glm=%q or=%q", fc.Providers["glm"].APIKey, fc.Providers["openrouter"].APIKey)
	}
	if s.client.APIKey != "glm-key" || s.client.Model != "glm-5.3-flash" {
		t.Fatalf("live client wrong: %q %q", s.client.APIKey, s.client.Model)
	}
	// The model list comes back without fetching again.
	clearModelCache()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/models?provider=glm", nil))
	if !strings.Contains(rr.Body.String(), "glm-5.3-flash") {
		t.Fatalf("models list: %s", rr.Body.String())
	}
	// Settings lists the named provider.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	if !strings.Contains(rr.Body.String(), `"name":"glm"`) {
		t.Fatalf("settings should list glm: %s", rr.Body.String())
	}
}

func TestToolHistory400FallsBackToText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "tool" || len(m.ToolCalls) > 0 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}]`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer srv.Close()
	c := NewClientForProvider(srv.URL, "k", "gemini-3.5-flash-lite", "gemini")
	var tc ToolCall
	tc.ID, tc.Function.Name, tc.Function.Arguments = "a1", "run_shell", `{"command":"ls"}`
	msgs := []Message{{Role: "user", Content: "hi"}, {Role: "assistant", ToolCalls: []ToolCall{tc}}, {Role: "tool", ToolCallID: "a1", Content: "x"}}
	m, err := c.Chat(msgs, nil)
	if err != nil || m.Content != "done" {
		t.Fatalf("fallback failed: %v %+v", err, m)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
	if _, err := os.Stat(filepath.Join(configDir(), "data", "last_bad_request.json")); err != nil {
		t.Fatal("bad request not saved")
	}
	// Next call goes straight to text mode.
	if _, err := c.Chat(msgs, nil); err != nil || calls != 3 {
		t.Fatalf("second call: %v calls=%d", err, calls)
	}
}
