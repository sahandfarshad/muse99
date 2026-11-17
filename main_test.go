package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSpark(t *testing.T) {
	if !isSpark("https://api.meta.ai/v1", "muse-spark-1.3") {
		t.Fatal("Meta API should be Spark")
	}
	if !isSpark("https://example.com/v1", "muse-spark-1.4") {
		t.Fatal("muse-spark model should be Spark")
	}
	if isSpark("https://api.openai.com/v1", "gpt-5") {
		t.Fatal("OpenAI GPT should not be Spark")
	}
}

func TestSystemPrompt(t *testing.T) {
	p := systemPrompt("Takahashi", "likes Go", "https://api.meta.ai/v1", "muse-spark-1.3")
	if !strings.Contains(p, "Muse") || !strings.Contains(p, "Takahashi") {
		t.Fatalf("prompt missing Muse/Takahashi: %s", p)
	}
	if !strings.Contains(p, "Muse Spark") {
		t.Fatal("Spark prompt should mention Muse Spark")
	}
	generic := systemPrompt("Takahashi", "", "https://api.openai.com/v1", "gpt-5")
	if strings.Contains(generic, "powered by Muse Spark") {
		t.Fatal("generic prompt should not claim Spark")
	}
}

func TestProviderPresets(t *testing.T) {
	byName := map[string]Provider{}
	for _, p := range providerPresets() {
		byName[p.Name] = p
	}
	if byName["spark"].BaseURL != "https://api.meta.ai/v1" || byName["spark"].Model != "muse-spark-1.3" {
		t.Fatalf("spark preset wrong: %+v", byName["spark"])
	}
	if byName["openai"].BaseURL != "https://api.openai.com/v1" || byName["openai"].Model != "gpt-5" {
		t.Fatalf("openai preset wrong: %+v", byName["openai"])
	}
	if byName["gemini"].BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai/" || byName["gemini"].Model != "gemini-2.5-flash" {
		t.Fatalf("gemini preset wrong: %+v", byName["gemini"])
	}
	if byName["openrouter"].BaseURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("openrouter preset wrong: %+v", byName["openrouter"])
	}
	if byName["ollama"].BaseURL != "http://localhost:11434/v1" || byName["ollama"].Model != "llama3.1" {
		t.Fatalf("ollama preset wrong: %+v", byName["ollama"])
	}
	if _, ok := byName["custom"]; !ok {
		t.Fatal("custom preset missing")
	}
	// Switching restores the right key per provider.
	fc := FileConfig{}
	fc.setProvider(Provider{Name: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: "sk-spark"})
	fc.setProvider(Provider{Name: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "sk-openai"})
	cfg := Config{Providers: fc.Providers}
	if !cfg.switchProvider("openai") || cfg.APIKey != "sk-openai" || cfg.Model != "gpt-5" {
		t.Fatalf("switch to openai failed: %+v", cfg)
	}
	if !cfg.switchProvider("spark") || cfg.APIKey != "sk-spark" || cfg.Model != "muse-spark-1.3" {
		t.Fatalf("switch back to spark failed: %+v", cfg)
	}
	if cfg.switchProvider("nope") {
		t.Fatal("unknown provider should not switch")
	}
	// Legacy single-provider file folds into the matching provider.
	legacy := FileConfig{BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "sk-legacy"}
	// Simulate loadFileConfig's backward-compat fold.
	if len(legacy.Providers) == 0 {
		name := matchProvider(legacy.BaseURL, legacy.Model)
		legacy.Providers = map[string]Provider{name: {Name: name, BaseURL: legacy.BaseURL, Model: legacy.Model, APIKey: legacy.APIKey}}
		legacy.ActiveProvider = name
	}
	if legacy.ActiveProvider != "openai" || legacy.Providers["openai"].APIKey != "sk-legacy" {
		t.Fatalf("legacy fold failed: %+v", legacy)
	}
}

func TestWebAddressFor(t *testing.T) {
	if got := webAddressFor(Config{Domain: "muse99.example.com", TLSEnabled: true, WebPort: 8080}); got != "https://muse99.example.com" {
		t.Fatalf("TLS domain address = %q", got)
	}
	if got := webAddressFor(Config{Domain: "muse99.example.com", WebPort: 8080}); got != "http://muse99.example.com:8080" {
		t.Fatalf("plain domain address = %q", got)
	}
	if got := webAddressFor(Config{WebPort: 8080}); !strings.Contains(got, ":8080") {
		t.Fatalf("IP address should contain :8080: %q", got)
	}
}

func TestUsageRecording(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := estimatedCost(1_000_000, 1_000_000); got != 5.50 {
		t.Fatalf("estimatedCost(1M, 1M) = %v, want 5.50", got)
	}
	u := recordUsage(100, 50)
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.TotalTokens != 150 || u.Calls != 1 {
		t.Fatalf("recordUsage totals wrong: %+v", u)
	}
	u = recordUsage(200, 100)
	if u.PromptTokens != 300 || u.CompletionTokens != 150 || u.Calls != 2 {
		t.Fatalf("recordUsage did not accumulate: %+v", u)
	}
	if got := loadUsage(); got.PromptTokens != 300 {
		t.Fatalf("usage was not persisted: %+v", got)
	}
	if got := usageLine(u); !strings.Contains(got, "300 in") || !strings.Contains(got, "150 out") {
		t.Fatalf("usageLine wrong: %q", got)
	}
	// No usage reported -> no change.
	if got := recordUsage(0, 0); got.Calls != 2 {
		t.Fatalf("zero usage should not add a call: %+v", got)
	}
}

func TestClientRecordsUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
		})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "test", "muse-spark-1.3")
	msgs := []Message{{Role: "system", Content: "test"}}
	if _, err := client.Chat(msgs, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadUsage(); got.PromptTokens != 120 || got.CompletionTokens != 30 || got.Calls != 1 {
		t.Fatalf("client did not record usage: %+v", got)
	}
}

func TestSafePathAndTruncate(t *testing.T) {
	if got := safePath("/tmp/work", "a/b.txt"); got != "/tmp/work/a/b.txt" {
		t.Fatalf("safePath = %q", got)
	}
	if got := truncate("hello world", 5); !strings.HasPrefix(got, "hello") {
		t.Fatalf("truncate = %q", got)
	}
	if len(toolDefinitions()) < 5 {
		t.Fatal("expected at least 5 tools")
	}
}

// Mock OpenAI-compatible server: first turn asks to read a file,
// second turn (after tool result) gives a final answer.
// This is what CI runs — no real API key needed.
func TestAgentLoopWithMockServer(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var resp map[string]any
		if calls == 1 {
			resp = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"README.md"}`},
				}},
			}}}}
		} else {
			resp = map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": "Mock reply: read it fine.",
			}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test", "muse-spark-1.3")
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	messages := []Message{{Role: "system", Content: systemPrompt("Takahashi", "", srv.URL, "muse-spark-1.3")}}
	reply := runTurn(client, runner, toolDefinitions(), &messages, "hello")
	if !strings.Contains(reply, "Mock reply") {
		t.Fatalf("unexpected reply: %q (calls=%d)", reply, calls)
	}
	if calls != 2 {
		t.Fatalf("expected 2 API calls (tool + final), got %d", calls)
	}
}

// ---------- v3: persistent chats + advanced memory ----------

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenDB(filepath.Join(t.TempDir(), "muse99.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDBChatPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muse99.db")
	d1, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d1.CreateChat("My project")
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.AddMessage(c.ID, Message{Role: "user", Content: "hello, build a todo app"}); err != nil {
		t.Fatal(err)
	}
	if err := d1.AddMessage(c.ID, Message{Role: "assistant", Content: "sure, here it is"}); err != nil {
		t.Fatal(err)
	}
	// Rename + create a second chat, then close (restart) and reopen.
	if err := d1.RenameChat(c.ID, "Todo app"); err != nil {
		t.Fatal(err)
	}
	c2, _ := d1.CreateChat("Second chat")
	_ = d1.AddMessage(c2.ID, Message{Role: "user", Content: "second chat message"})
	if err := d1.Close(); err != nil {
		t.Fatal(err)
	}
	d2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	chats, err := d2.ListChats()
	if err != nil || len(chats) != 2 {
		t.Fatalf("expected 2 chats after reopen, got %d (err %v)", len(chats), err)
	}
	msgs, err := d2.LoadMessages(c.ID, 100)
	if err != nil || len(msgs) != 2 || msgs[0].Content != "hello, build a todo app" || msgs[1].Content != "sure, here it is" {
		t.Fatalf("messages did not survive restart: %+v (err %v)", msgs, err)
	}
	got, _ := d2.GetChat(c.ID)
	if got.Title != "Todo app" {
		t.Fatalf("rename did not survive: %q", got.Title)
	}
	msgs2, _ := d2.LoadMessages(c2.ID, 100)
	if len(msgs2) != 1 || msgs2[0].Content != "second chat message" {
		t.Fatalf("second chat messages wrong: %+v", msgs2)
	}
	// Search finds a chat by its message text.
	found, _ := d2.SearchChats("todo app")
	if len(found) != 1 {
		t.Fatalf("search for 'todo app' found %d chats, want 1", len(found))
	}
	// Delete removes the chat and its messages.
	if err := d2.DeleteChat(c2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d2.GetChat(c2.ID); err == nil {
		t.Fatal("deleted chat should be gone")
	}
}

func TestMemoryScoringAndRetrieval(t *testing.T) {
	d := openTestDB(t)
	_, _, _ = d.AddMemory("Takahashi prefers Go over Python for servers", "preference", "")
	_, _, _ = d.AddMemory("The Muse99 project lives on GitHub as sahfar363/muse99", "project", "")
	_, _, _ = d.AddMemory("He likes his coffee black", "preference", "")
	// A question about the project should surface the project memory first.
	top := d.RelevantMemories("tell me about the Muse99 project on GitHub", 10, 4000)
	if len(top) == 0 || !strings.Contains(top[0].Text, "Muse99") {
		t.Fatalf("project memory should rank first, got %+v", top)
	}
	// A question about Go should surface the Go preference, not the coffee one.
	top = d.RelevantMemories("should I use Go for the server?", 10, 4000)
	if len(top) == 0 || !strings.Contains(top[0].Text, "Go") {
		t.Fatalf("Go memory should rank first, got %+v", top)
	}
	// Char bound: a tiny budget keeps only the first memory.
	top = d.RelevantMemories("Go", 10, 30)
	if len(top) != 1 {
		t.Fatalf("maxChars should bound results, got %d", len(top))
	}
}

func TestMemoryDedupe(t *testing.T) {
	d := openTestDB(t)
	m1, added1, err := d.AddMemory("Takahashi prefers Go over Python", "", "")
	if err != nil || !added1 {
		t.Fatalf("first add should succeed: added=%v err=%v", added1, err)
	}
	_, added2, _ := d.AddMemory("takahashi prefers go over python!", "", "")
	if added2 {
		t.Fatal("near-duplicate memory should not be added twice")
	}
	mems, _ := d.ListMemories()
	if len(mems) != 1 || mems[0].ID != m1.ID {
		t.Fatalf("expected exactly 1 memory, got %d", len(mems))
	}
	// Edit and delete work.
	if err := d.UpdateMemory(m1.ID, "Takahashi prefers Go", "preference"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteMemory(m1.ID); err != nil {
		t.Fatal(err)
	}
	if mems, _ := d.ListMemories(); len(mems) != 0 {
		t.Fatalf("expected 0 memories after delete, got %d", len(mems))
	}
}

func TestMemoryImportExport(t *testing.T) {
	d := openTestDB(t)
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	content := "# Memory\n\n- [preference] 2026-11-17 — Takahashi prefers Go\n- [fact] 2026-11-17 — The server is Ubuntu 22.04\n- plain line without a kind\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := d.ImportMemoriesFromFile(path)
	if err != nil || n != 3 {
		t.Fatalf("import = %d, err %v, want 3", n, err)
	}
	// Regression: importing must NOT rewrite the source file — the
	// user's hand-written MEMORY.md (headings and all) stays untouched.
	after, _ := os.ReadFile(path)
	if string(after) != content {
		t.Fatalf("import rewrote the source MEMORY.md:\n%s", after)
	}
	// Importing again dedupes: nothing new.
	n2, _ := d.ImportMemoriesFromFile(path)
	if n2 != 0 {
		t.Fatalf("second import should add 0, got %d", n2)
	}
	out := filepath.Join(t.TempDir(), "OUT.md")
	if err := d.ExportMemoriesTo(out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "prefers Go") || !strings.Contains(string(data), "Ubuntu 22.04") {
		t.Fatalf("export lost facts: %s", data)
	}
}

func TestAutoSummaryDedupe(t *testing.T) {
	d := openTestDB(t)
	// Mock model that extracts two facts from any chat.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
				"content": "- Takahashi prefers Go\n- The project is Muse99\n"}}},
		})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "test", "muse-spark-1.3")
	c, _ := d.CreateChat("test")
	for i := 0; i < 10; i++ {
		_ = d.AddMessage(c.ID, Message{Role: "user", Content: "message number here"})
	}
	n, err := summariseChat(client, d, c.ID)
	if err != nil || n != 2 {
		t.Fatalf("first summary: saved=%d err=%v, want 2", n, err)
	}
	// A second summary of the same messages saves nothing (dedupe +
	// LastSummaryMsg means the same messages are not re-scanned).
	n2, err := summariseChat(client, d, c.ID)
	if err != nil || n2 != 0 {
		t.Fatalf("second summary: saved=%d err=%v, want 0", n2, err)
	}
	mems, _ := d.ListMemories()
	if len(mems) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(mems))
	}
	// maybeAutoSummarise only fires at 10 new messages.
	if got := maybeAutoSummarise(client, d, c.ID); got != 0 {
		t.Fatalf("auto-summary should not fire below 10 new messages, got %d", got)
	}
	for i := 0; i < 10; i++ {
		_ = d.AddMessage(c.ID, Message{Role: "user", Content: "another message"})
	}
	if got := maybeAutoSummarise(client, d, c.ID); got != 0 {
		t.Fatalf("auto-summary of duplicate facts should save 0, got %d", got)
	}
}

// ---------- v3: shell checkbox ----------

func TestShellCheckboxToggle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), AllowShell: false}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md", allowShell: false}
	s := &webServer{client: NewClient("http://localhost:9", "x", "m"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	// Shell off: status says false and run_shell is refused.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"shell":false`) {
		t.Fatalf("status should report shell false, got %s", rr.Body.String())
	}
	if got := runner.run("run_shell", `{"command":"echo hi"}`); !strings.Contains(got, "disabled") {
		t.Fatalf("run_shell should be refused while off, got %q", got)
	}

	// Toggle on via /api/settings, like the web checkbox does.
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"allow_shell":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("settings POST = %d, body %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"shell":true`) {
		t.Fatalf("status should report shell true after toggle, got %s", rr.Body.String())
	}
	if got := runner.run("run_shell", `{"command":"echo hi"}`); got != "hi\n" {
		t.Fatalf("run_shell should work while on, got %q", got)
	}

	// Toggle off again — and make sure the TLS flag was not clobbered.
	req = httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"allow_shell":false}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if runner.shellAllowed() {
		t.Fatal("shell should be off after toggling it off")
	}
}

// ---------- v3: browse ----------

func TestBrowseURLStaticPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Example Domain</title></head>
<body><h1>Example Domain</h1><p>This domain is for use in documentation.</p>
<script>var x = 1;</script><a href="https://example.com/more">More information</a></body></html>`)
	}))
	defer srv.Close()
	out := browsePage(srv.URL, false)
	if !strings.Contains(out, "Example Domain") || !strings.Contains(out, "for use in documentation") {
		t.Fatalf("browse should return title + text, got:\n%s", out)
	}
	if strings.Contains(out, "var x = 1") {
		t.Fatal("browse should strip <script> content")
	}
	if !strings.Contains(out, "More information") {
		t.Fatal("browse should keep link text")
	}
	// fetch_url is the backward-compatible alias.
	if got := browsePage(srv.URL, false); !strings.Contains(got, "Example Domain") {
		t.Fatalf("alias browse failed: %s", got)
	}
	// Layer 2 without Chromium installed returns a clear install message.
	if _, err := exec.LookPath("chromium"); err != nil {
		if _, err := exec.LookPath("chromium-browser"); err != nil {
			out2 := browsePage(srv.URL, true)
			if !strings.Contains(out2, "chromium-browser") || !strings.Contains(out2, "300-500 MB") {
				t.Fatalf("missing-chromium message should explain install + RAM cost, got:\n%s", out2)
			}
		}
	}
}

func TestHTMLToText(t *testing.T) {
	title, text := htmlToText("<html><head><title>My Page</title></head><body><h1>Head</h1><p>Hello <b>world</b></p><style>.x{}</style></body></html>")
	if title != "My Page" || !strings.Contains(text, "Head") || !strings.Contains(text, "Hello world") {
		t.Fatalf("htmlToText = title %q text %q", title, text)
	}
	if strings.Contains(text, ".x{}") {
		t.Fatal("htmlToText should strip <style>")
	}
}

// ---------- v3: web chats API, persistence across restart ----------

func TestWebChatsPersistAcrossRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db := openTestDB(t)
	oldDB := appDB
	appDB = db
	defer func() { appDB = oldDB }()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "mock reply"}}},
		})
	}))
	defer mock.Close()

	newServer := func() *webServer {
		cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), MemoryPath: t.TempDir() + "/MEMORY.md"}
		runner := &toolRunner{workdir: cfg.Workdir, memoryPath: cfg.MemoryPath}
		s := &webServer{client: NewClient(mock.URL, "x", "m"), runner: runner, cfg: cfg,
			messages: []Message{{Role: "system", Content: "s"}}, system: "s", db: db}
		if ch, err := db.DefaultChat(); err == nil {
			s.currentChatID = ch.ID
			if loaded, err := db.LoadMessages(ch.ID, 100); err == nil {
				s.messages = append(s.messages, loaded...)
			}
		}
		return s
	}

	s1 := newServer()
	mux1 := buildMux(s1)
	// Create two chats via the API.
	var chat1, chat2 Chat
	for _, dst := range []*Chat{&chat1, &chat2} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/chats", strings.NewReader(`{"title":""}`))
		mux1.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("create chat = %d %s", rr.Code, rr.Body.String())
		}
		if err := json.NewDecoder(rr.Body).Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	// Send a message to each chat.
	for _, c := range []Chat{chat1, chat2} {
		rr := httptest.NewRecorder()
		body := fmt.Sprintf(`{"message":"hello from %s","chat_id":%q}`, c.ID[:8], c.ID)
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
		mux1.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("chat = %d %s", rr.Code, rr.Body.String())
		}
		var resp chatResp
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		if resp.Reply != "mock reply" || resp.ChatID != c.ID {
			t.Fatalf("chat reply wrong: %+v", resp)
		}
	}
	// Old behaviour: no chat_id still works (default chat).
	rr := httptest.NewRecorder()
	mux1.ServeHTTP(rr, httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":"no id"}`)))
	if rr.Code != 200 {
		t.Fatalf("chat without id = %d %s", rr.Code, rr.Body.String())
	}

	// "Restart": a fresh webServer on the same DB.
	s2 := newServer()
	mux2 := buildMux(s2)
	rr = httptest.NewRecorder()
	mux2.ServeHTTP(rr, httptest.NewRequest("GET", "/api/chats", nil))
	var chats []Chat
	if err := json.NewDecoder(rr.Body).Decode(&chats); err != nil {
		t.Fatal(err)
	}
	if len(chats) < 2 {
		t.Fatalf("expected at least 2 chats after restart, got %d", len(chats))
	}
	for _, c := range []Chat{chat1, chat2} {
		rr = httptest.NewRecorder()
		mux2.ServeHTTP(rr, httptest.NewRequest("GET", "/api/chats/"+c.ID+"/messages", nil))
		var msgs []Message
		if err := json.NewDecoder(rr.Body).Decode(&msgs); err != nil {
			t.Fatal(err)
		}
		if len(msgs) < 2 {
			t.Fatalf("chat %s should have 2 messages after restart, got %d", c.ID, len(msgs))
		}
		if msgs[0].Role != "user" || msgs[1].Role != "assistant" || msgs[1].Content != "mock reply" {
			t.Fatalf("chat %s messages wrong: %+v", c.ID, msgs)
		}
	}
	// Rename + delete via the API.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/chats/"+chat1.ID, strings.NewReader(`{"title":"Renamed"}`))
	mux2.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("rename = %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux2.ServeHTTP(rr, httptest.NewRequest("DELETE", "/api/chats/"+chat2.ID, nil))
	if rr.Code != 200 {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	mux2.ServeHTTP(rr, httptest.NewRequest("GET", "/api/chats", nil))
	if err := json.NewDecoder(rr.Body).Decode(&chats); err != nil {
		t.Fatal(err)
	}
	for _, c := range chats {
		if c.ID == chat2.ID {
			t.Fatal("deleted chat still listed")
		}
	}
}

// ---------- v4: model discovery ----------

func TestParseModelsResponse(t *testing.T) {
	// OpenAI shape.
	m, err := parseModelsResponse([]byte(`{"data":[{"id":"gpt-5","owned_by":"openai"},{"id":"gpt-4o","owned_by":"openai"}],"object":"list"}`))
	if err != nil || len(m) != 2 || m[0].ID != "gpt-5" || m[0].OwnedBy != "openai" {
		t.Fatalf("OpenAI shape: models=%+v err=%v", m, err)
	}
	// Ollama native /api/tags shape.
	m, err = parseModelsResponse([]byte(`{"models":[{"name":"llama3.1:latest","model":"llama3.1:latest"},{"name":"mistral:7b"}]}`))
	if err != nil || len(m) != 2 || m[0].ID != "llama3.1:latest" {
		t.Fatalf("Ollama tags shape: models=%+v err=%v", m, err)
	}
	// Plain array of strings.
	m, err = parseModelsResponse([]byte(`["a","b","a"]`))
	if err != nil || len(m) != 2 {
		t.Fatalf("string array: models=%+v err=%v", m, err)
	}
	// Empty data is not an error — the caller falls back to suggestions.
	m, err = parseModelsResponse([]byte(`{"data":[]}`))
	if err != nil || len(m) != 0 {
		t.Fatalf("empty data: models=%+v err=%v", m, err)
	}
	// Malformed JSON is an error.
	if _, err := parseModelsResponse([]byte(`{not json`)); err == nil {
		t.Fatal("malformed JSON should be an error")
	}
	if _, err := parseModelsResponse([]byte(``)); err == nil {
		t.Fatal("empty body should be an error")
	}
	// Duplicates are dropped.
	m, err = parseModelsResponse([]byte(`{"data":[{"id":"x"},{"id":"x"},{"id":"y"}]}`))
	if err != nil || len(m) != 2 {
		t.Fatalf("duplicates: models=%+v err=%v", m, err)
	}
}

func TestFetchModelsLive(t *testing.T) {
	// Happy path: /v1/models returns a list.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k1" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"bad key"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"muse-spark-1.3"},{"id":"muse-spark-1.3-contributor"}]}`)
	}))
	defer ok.Close()
	models, source, err := fetchModelsLive(ok.URL, "k1", "spark")
	if err != nil || source != "live" || len(models) != 2 || models[0].ID != "muse-spark-1.3" {
		t.Fatalf("live fetch: models=%+v source=%s err=%v", models, source, err)
	}
	// Wrong key: a clear error, not an empty list.
	if _, _, err := fetchModelsLive(ok.URL, "wrong", "spark"); err == nil {
		t.Fatal("401 from /models should be an error")
	}
	// No key for a cloud provider: clear error before any request.
	if _, _, err := fetchModelsLive(ok.URL, "", "spark"); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key should say so, got %v", err)
	}
	// Ollama fallback: /v1/models 404s, /api/tags answers.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"models":[{"name":"llama3.1:latest"},{"name":"mistral:7b"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ollama.Close()
	models, source, err = fetchModelsLive(ollama.URL+"/v1", "", "ollama")
	if err != nil || source != "live" || len(models) != 2 || models[0].ID != "llama3.1:latest" {
		t.Fatalf("ollama fallback: models=%+v source=%s err=%v", models, source, err)
	}
	// Gemini: live endpoint answers but lists nothing -> suggested models.
	gem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer gem.Close()
	models, source, err = fetchModelsLive(gem.URL, "k1", "gemini")
	if err != nil || source != "suggested" || len(models) == 0 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("gemini suggested: models=%+v source=%s err=%v", models, source, err)
	}
}

// mockProviderServer serves /models and /chat/completions for tests.
func mockProviderServer(t *testing.T, models []string, reply string, failModels bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if failModels {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
				return
			}
			data := []any{}
			for _, id := range models {
				data = append(data, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if r.Header.Get("Authorization") != "Bearer test-key-123" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}},
				"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestModelsHandler(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	db := openTestDB(t)
	oldDB := appDB
	appDB = db
	defer func() { appDB = oldDB }()

	mock := mockProviderServer(t, []string{"model-a", "model-b"}, "OK", false)
	// Keep the URL around; the server is closed later to test fallback.
	mockURL := mock.URL

	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "mock",
		Providers: map[string]Provider{"mock": {Name: "mock", BaseURL: mockURL, Model: "model-a", APIKey: "test-key-123"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient(mockURL, "test-key-123", "model-a"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s", db: db}
	mux := buildMux(s)

	getModels := func() modelListResp {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/models?provider=mock", nil))
		if rr.Code != 200 {
			t.Fatalf("GET /api/models = %d %s", rr.Code, rr.Body.String())
		}
		var resp modelListResp
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Live list.
	resp := getModels()
	if resp.Error != "" || resp.Source != "live" || len(resp.Models) != 2 || resp.Models[0].ID != "model-a" {
		t.Fatalf("live list wrong: %+v", resp)
	}
	// Second call comes from the in-memory cache.
	resp = getModels()
	if resp.Source != "cached" || len(resp.Models) != 2 {
		t.Fatalf("cached list wrong: %+v", resp)
	}
	// Provider is now down: in-memory cache still answers…
	mock.Close()
	resp = getModels()
	if len(resp.Models) != 2 {
		t.Fatalf("cache should survive the provider going down: %+v", resp)
	}
	// …and so does the persisted list, once the memory cache is cleared.
	clearModelCache()
	resp = getModels()
	if resp.Source != "cached" || len(resp.Models) != 2 {
		t.Fatalf("persisted list should survive restart: %+v", resp)
	}
	// Force refresh while down: still falls back to the persisted list.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/models/refresh", strings.NewReader(`{"provider":"mock"}`))
	mux.ServeHTTP(rr, req)
	var fres modelListResp
	if err := json.NewDecoder(rr.Body).Decode(&fres); err != nil {
		t.Fatal(err)
	}
	if len(fres.Models) != 2 {
		t.Fatalf("refresh fallback should return the persisted list: %+v", fres)
	}
	// Unknown provider with no key: a clear error, no models.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/models?provider=spark", nil))
	var errResp modelListResp
	if err := json.NewDecoder(rr.Body).Decode(&errResp); err != nil {
		t.Fatal(err)
	}
	if errResp.Error == "" || !strings.Contains(errResp.Error, "API key") {
		t.Fatalf("missing key should be a clear error: %+v", errResp)
	}
}

func TestModelTestHandler(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	mock := mockProviderServer(t, []string{"model-a"}, "OK", false)
	defer mock.Close()

	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "mock",
		Providers: map[string]Provider{"mock": {Name: "mock", BaseURL: mock.URL, Model: "model-a", APIKey: "test-key-123"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient(mock.URL, "test-key-123", "model-a"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	post := func(body string) (int, string) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/models/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}
	// Success: ok, reply OK, latency present, key never echoed.
	code, body := post(`{"provider":"mock","model":"model-a"}`)
	if code != 200 {
		t.Fatalf("test = %d %s", code, body)
	}
	var okResp map[string]any
	if err := json.Unmarshal([]byte(body), &okResp); err != nil {
		t.Fatal(err)
	}
	if okResp["ok"] != true || okResp["reply"] != "OK" || okResp["model"] != "model-a" {
		t.Fatalf("test success wrong: %s", body)
	}
	if _, has := okResp["latency_ms"]; !has {
		t.Fatalf("test should report latency_ms: %s", body)
	}
	if strings.Contains(body, "test-key-123") {
		t.Fatal("test response must never echo the API key")
	}
	// Failure: wrong key -> ok false with a clear error.
	code, body = post(`{"provider":"mock","api_key":"wrong-key","model":"model-a"}`)
	if code != 200 {
		t.Fatalf("test failure = %d %s", code, body)
	}
	var badResp map[string]any
	if err := json.Unmarshal([]byte(body), &badResp); err != nil {
		t.Fatal(err)
	}
	if badResp["ok"] != false || badResp["error"] == nil || badResp["error"] == "" {
		t.Fatalf("test failure wrong: %s", body)
	}
	if strings.Contains(body, "wrong-key") {
		t.Fatal("test error must never echo the API key")
	}
	// No model: clear error.
	_, body = post(`{"provider":"mock","model":""}`)
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("empty model should fail: %s", body)
	}
}

func TestModelSelectSaves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "mock",
		Providers: map[string]Provider{"mock": {Name: "mock", BaseURL: "http://localhost:9", Model: "model-a", APIKey: "test-key-123"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient("http://localhost:9", "test-key-123", "model-a"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	// Pick a model from the dropdown and save, as the Settings UI does.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"active_provider":"mock","model":"model-b"}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("settings POST = %d %s", rr.Code, rr.Body.String())
	}
	fc := loadFileConfig()
	if fc.Providers["mock"].Model != "model-b" {
		t.Fatalf("choosing a model should save it, got %q", fc.Providers["mock"].Model)
	}
	// /api/status shows the new model right away.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"model":"model-b"`) {
		t.Fatalf("status should show the new model, got %s", rr.Body.String())
	}
}
