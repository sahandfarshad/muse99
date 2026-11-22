package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderTabsAndFavorites(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	mock := mockProviderServer(t, []string{"deepseek-chat", "deepseek-reasoner"}, "OK", false)
	defer mock.Close()
	cfg := Config{UserName: "S", NameSet: true, Workdir: t.TempDir(), Provider: "spark", Model: "muse-spark-1.3"}
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
	out := call("GET", "/api/providers", "")
	for _, want := range []string{`"name":"gemini"`, `"name":"openai"`, `"name":"anthropic"`, `"name":"spark"`, `"label":"Anthropic"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `"name":"ollama"`) || strings.Contains(out, `"name":"openrouter"`) {
		t.Fatalf("local / keyless presets should not get a tab: %s", out)
	}
	// Add a custom provider: needs a base URL.
	if out = call("POST", "/api/providers", `{"name":"deepseek","api_key":"sk-d"}`); !strings.Contains(out, `"ok":false`) {
		t.Fatal(out)
	}
	out = call("POST", "/api/providers", `{"name":"DeepSeek","base_url":"`+mock.URL+`","api_key":"sk-secret-1234"}`)
	if !strings.Contains(out, `"name":"deepseek"`) || !strings.Contains(out, `"removable":true`) || strings.Contains(out, "sk-secret") {
		t.Fatal(out)
	}
	if s.cfg.Provider != "spark" {
		t.Fatal("adding a provider must not switch the active one")
	}
	if out = call("GET", "/api/models?provider=deepseek", ""); !strings.Contains(out, "deepseek-reasoner") {
		t.Fatal(out)
	}
	// Star two models: they become the chat menu.
	call("POST", "/api/models/favorite", `{"provider":"deepseek","model":"deepseek-chat","on":true}`)
	call("POST", "/api/models/favorite", `{"provider":"deepseek","model":"deepseek-reasoner","on":true}`)
	call("POST", "/api/models/favorite", `{"provider":"deepseek","model":"deepseek-chat","on":false}`)
	st := call("GET", "/api/status", "")
	if !strings.Contains(st, `"recent":[{"provider":"spark","model":"muse-spark-1.3"},{"provider":"deepseek","model":"deepseek-reasoner"}]`) {
		t.Fatalf("menu %s", st)
	}
	// Removing: not a preset, not the one in use; drops its stars.
	if out = call("DELETE", "/api/providers?name=gemini", ""); !strings.Contains(out, `"ok":false`) {
		t.Fatal(out)
	}
	if out = call("DELETE", "/api/providers?name=deepseek", ""); !strings.Contains(out, `"ok":true`) || strings.Contains(out, `"name":"deepseek"`) {
		t.Fatal(out)
	}
	if fc := loadFileConfig(); len(fc.Favorites) != 0 {
		t.Fatalf("stars left %+v", fc.Favorites)
	}
}

func TestAnthropicModelHeaders(t *testing.T) {
	var gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotVer = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5","type":"model"}]}`))
	}))
	defer srv.Close()
	ms, err := fetchModelsFromURL(srv.URL+"/models", "ak", "anthropic")
	if err != nil || len(ms) != 1 || gotKey != "ak" || gotVer == "" {
		t.Fatalf("%v %v %q %q", ms, err, gotKey, gotVer)
	}
	if !needsAPIKey("x", "anthropic") {
		t.Fatal("anthropic needs a key")
	}
}
