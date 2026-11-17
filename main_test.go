package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// ---------- v5: default port 3759, uninstall, no-key notice ----------

func TestDefaultWebPort(t *testing.T) {
	if DefaultWebPort != 3759 {
		t.Fatalf("DefaultWebPort = %d, want 3759", DefaultWebPort)
	}
	// A saved port is preserved; no saved port keeps the default.
	cfg := Config{WebPort: DefaultWebPort}
	applyFileConfig(&cfg, FileConfig{Port: 8081})
	if cfg.WebPort != 8081 {
		t.Fatalf("saved port 8081 was overridden: got %d", cfg.WebPort)
	}
	cfg = Config{WebPort: DefaultWebPort}
	applyFileConfig(&cfg, FileConfig{})
	if cfg.WebPort != DefaultWebPort {
		t.Fatalf("no saved port should keep the default: got %d", cfg.WebPort)
	}
	// The default address shows 3759.
	if got := webAddressFor(Config{WebPort: DefaultWebPort}); !strings.Contains(got, ":3759") {
		t.Fatalf("default address should contain :3759: %q", got)
	}
}

func TestUninstallKeepsDataByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	dataDir := filepath.Join(home, ".muse99")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(binDir, "muse99")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svcDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	svcPath := filepath.Join(svcDir, "muse99.service")
	if err := os.WriteFile(svcPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := uninstallTarget{ExePath: binPath, InstallDir: binDir, DataDir: dataDir, HomeDir: home}
	removed, kept, err := runUninstall(tgt, false)
	if err != nil {
		t.Fatalf("uninstall (keep data) failed: %v", err)
	}
	if _, err := os.Stat(binPath); !os.IsNotExist(err) {
		t.Fatal("binary should be removed")
	}
	if _, err := os.Stat(svcPath); !os.IsNotExist(err) {
		t.Fatal("service file should be removed")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Fatal("data should be KEPT by default, but config.json is gone")
	}
	joined := strings.Join(append(removed, kept...), "\n")
	if !strings.Contains(joined, "data kept") {
		t.Fatalf("result should say data was kept: %s", joined)
	}
}

func TestUninstallRemovesDataWithOptIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := t.TempDir()
	dataDir := filepath.Join(home, ".muse99")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(binDir, "muse99")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := uninstallTarget{ExePath: binPath, InstallDir: binDir, DataDir: dataDir, HomeDir: home}
	if _, _, err := runUninstall(tgt, true); err != nil {
		t.Fatalf("uninstall (remove data) failed: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("data dir should be removed with the opt-in")
	}
	// The install dir was emptied by the uninstall, so it goes too.
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatal("empty install dir should be removed")
	}
}

func TestNoAPIKeyChatNotice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), APIKey: ""}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	// The client points nowhere — the notice must come back without
	// any API call being attempted.
	s := &webServer{client: NewClient("http://127.0.0.1:9", "", "m"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"message":"hello"}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("chat with no key = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No API key set yet") || !strings.Contains(body, "Settings") {
		t.Fatalf("chat should return the friendly Settings notice, got %s", body)
	}
	if strings.Contains(body, "401") || strings.Contains(body, "unauthorized") {
		t.Fatalf("chat should not leak a raw API error, got %s", body)
	}
	// And /api/status must say the key is not set, so the UI banner shows.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"api_key_set":false`) {
		t.Fatalf("status should report api_key_set false, got %s", rr.Body.String())
	}
}

func TestInstallScriptHasNoExportBlock(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "export MODEL_API_KEY") {
		t.Fatal("install.sh must not print export MODEL_API_KEY setup lines any more")
	}
	if strings.Contains(s, "--allow-shell") {
		t.Fatal("install.sh must not tell the user to run --allow-shell any more")
	}
	if !strings.Contains(s, "3759") {
		t.Fatal("install.sh should mention the default port 3759")
	}
	if !strings.Contains(s, "/dev/tty") {
		t.Fatal("install.sh should reopen the terminal (/dev/tty) for the menu after a curl|bash run")
	}
	if !strings.Contains(s, "Settings") || !strings.Contains(s, "Provider") {
		t.Fatal("install.sh should point at Settings → Provider & model for the API key")
	}
}

// ---------- v6: no model text input, click-to-test, Gemini fixes ----------

func TestGeminiModelPrefixStripping(t *testing.T) {
	// Gemini's native list returns "name": "models/gemini-2.5-flash".
	m, err := parseModelsResponse([]byte(`{"models":[{"name":"models/gemini-2.5-flash","displayName":"Gemini 2.5 Flash"},{"name":"models/gemini-2.5-pro"}]}`))
	if err != nil || len(m) != 2 || m[0].ID != "gemini-2.5-flash" || m[1].ID != "gemini-2.5-pro" {
		t.Fatalf("native name shape: models=%+v err=%v", m, err)
	}
	// The OpenAI-compat list can also carry the prefix in "id".
	m, err = parseModelsResponse([]byte(`{"data":[{"id":"models/gemini-2.0-flash","owned_by":"google"}]}`))
	if err != nil || len(m) != 1 || m[0].ID != "gemini-2.0-flash" {
		t.Fatalf("prefixed id shape: models=%+v err=%v", m, err)
	}
	// Plain strings with the prefix are stripped too.
	m, err = parseModelsResponse([]byte(`["models/gemini-2.5-flash"]`))
	if err != nil || len(m) != 1 || m[0].ID != "gemini-2.5-flash" {
		t.Fatalf("prefixed string shape: models=%+v err=%v", m, err)
	}
	if normalizeModelID("models/gemini-2.5-flash") != "gemini-2.5-flash" {
		t.Fatal("normalizeModelID must strip the models/ prefix")
	}
	if normalizeModelID("gemini-2.5-flash") != "gemini-2.5-flash" {
		t.Fatal("normalizeModelID must leave bare ids alone")
	}
	// A prefixed model must never reach a chat request either.
	c := NewClient("https://generativelanguage.googleapis.com/v1beta/openai/", "k", "models/gemini-2.5-flash")
	if c.Model != "gemini-2.5-flash" {
		t.Fatalf("NewClient must strip the prefix, got %q", c.Model)
	}
	// …and never be saved to config.
	fc := FileConfig{}
	fc.setProvider(Provider{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "models/gemini-2.5-flash", APIKey: "k"})
	if fc.Providers["gemini"].Model != "gemini-2.5-flash" {
		t.Fatalf("setProvider must strip the prefix, got %q", fc.Providers["gemini"].Model)
	}
}

// geminiMockServer mimics Gemini's quirks: the OpenAI-compat /models
// endpoint rejects Bearer (401) but accepts x-goog-api-key, the native
// /v1beta/models endpoint answers with "models/..." names, and chat
// completions reject Bearer but accept x-goog-api-key — and reject a
// prefixed model id outright.
func geminiMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The native list endpoint, reached via fetchGeminiNativeModels
		// when the compat endpoint is empty.
		if r.URL.Path == "/v1beta/models" {
			if r.Header.Get("x-goog-api-key") != "gem-key-1" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
				return
			}
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.5-pro"}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			// Compat endpoint: Bearer is refused, x-goog-api-key works.
			if r.Header.Get("x-goog-api-key") == "gem-key-1" {
				fmt.Fprint(w, `{"data":[{"id":"gemini-2.5-flash"},{"id":"gemini-2.5-pro"}]}`)
				return
			}
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"Bearer not accepted here"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.HasPrefix(body.Model, "models/") {
				w.WriteHeader(404)
				fmt.Fprint(w, `{"error":{"message":"model not found"}}`)
				return
			}
			if r.Header.Get("x-goog-api-key") == "gem-key-1" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "OK"}}},
					"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
				})
				return
			}
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
			return
		}
		http.NotFound(w, r)
	}))
}

func TestGeminiAuthFallback(t *testing.T) {
	mock := geminiMockServer(t)
	defer mock.Close()
	// Compat list: Bearer 401 -> x-goog-api-key retry -> live list.
	models, source, err := fetchModelsLive(mock.URL+"/v1beta/openai", "gem-key-1", "gemini")
	if err != nil || source != "live" || len(models) != 2 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("gemini list with x-goog-api-key fallback: models=%+v source=%s err=%v", models, source, err)
	}
	// Chat completions: Bearer 401 -> x-goog-api-key retry -> OK.
	reply, _, err := testModel(mock.URL+"/v1beta/openai", "gem-key-1", "gemini-2.5-flash", "gemini")
	if err != nil || reply != "OK" {
		t.Fatalf("gemini chat with x-goog-api-key fallback: reply=%q err=%v", reply, err)
	}
	// A prefixed model id is stripped before the request, so the
	// mock's "model not found" 404 never fires.
	reply, _, err = testModel(mock.URL+"/v1beta/openai", "gem-key-1", "models/gemini-2.5-flash", "gemini")
	if err != nil || reply != "OK" {
		t.Fatalf("prefixed model should be stripped before testing, reply=%q err=%v", reply, err)
	}
}

func TestGeminiNativeListFallback(t *testing.T) {
	// Compat endpoint answers empty; the native list saves the day.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta/openai/models":
			fmt.Fprint(w, `{"data":[]}`)
		case "/v1beta/models":
			if r.Header.Get("x-goog-api-key") != "gem-key-1" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
				return
			}
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-2.5-flash"},{"name":"models/gemini-2.0-flash"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()
	models, source, err := fetchModelsLive(mock.URL+"/v1beta/openai", "gem-key-1", "gemini")
	if err != nil || source != "live" || len(models) != 2 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("gemini native fallback: models=%+v source=%s err=%v", models, source, err)
	}
}

func TestNoModelTextInputInSettingsHTML(t *testing.T) {
	// v6: the Settings page must not have a model text input in the
	// normal flow — only the clickable list and the hidden manual
	// fallback (which appears only when Fetch returns nothing).
	rr := httptest.NewRecorder()
	s := &webServer{client: NewClient("http://127.0.0.1:9", "", "m"), cfg: Config{UserName: "T"},
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	buildMux(s).ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	html := rr.Body.String()
	if strings.Contains(html, `id="set-model"`) {
		t.Fatal("Settings must not contain a model text input (id=set-model) any more")
	}
	if strings.Contains(html, "model-datalist") {
		t.Fatal("Settings must not contain the model datalist dropdown any more")
	}
	for _, want := range []string{`id="model-list"`, `id="fetch-models-btn"`, `id="test-all-btn"`, `id="manual-model-box"`, `id="manual-model"`, "Set active", "Test all", "first 20"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Settings HTML should contain %q", want)
		}
	}
}

func TestModelActiveEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearModelCache()
	mock := mockProviderServer(t, []string{"model-a", "model-b"}, "OK", false)
	defer mock.Close()
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "mock",
		Providers: map[string]Provider{"mock": {Name: "mock", BaseURL: mock.URL, Model: "model-a", APIKey: "test-key-123"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient(mock.URL, "test-key-123", "model-a"), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	// Clicking "Set active" on a model — even one whose id arrived
	// with a "models/" prefix — saves the bare id and makes it live.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/models/active", strings.NewReader(`{"provider":"mock","model":"models/model-b"}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("set active = %d %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true || resp["model"] != "model-b" {
		t.Fatalf("set active should return the bare id, got %s", rr.Body.String())
	}
	if fc := loadFileConfig(); fc.Providers["mock"].Model != "model-b" {
		t.Fatalf("set active should persist the bare id, got %q", fc.Providers["mock"].Model)
	}
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if !strings.Contains(rr.Body.String(), `"model":"model-b"`) {
		t.Fatalf("status should show the new active model, got %s", rr.Body.String())
	}
	// And testing a prefixed id sends the bare id to the provider.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/models/test", strings.NewReader(`{"provider":"mock","model":"models/model-a"}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr, req)
	testBody := rr.Body.String()
	var testResp map[string]any
	if err := json.Unmarshal([]byte(testBody), &testResp); err != nil {
		t.Fatal(err)
	}
	if testResp["ok"] != true || testResp["model"] != "model-a" {
		t.Fatalf("test of a prefixed id should use the bare id, got %s", testBody)
	}
}

// TestSavedProviderSurvivesRestart is the regression test for the
// v6 Gemini root cause: loadConfig used to fold the Spark flag
// defaults into the config on every startup, and setProvider
// unconditionally made Spark the active provider — so a saved Gemini
// provider was silently dropped after every restart and the live
// client was Spark with no key ("couldn't connect to Gemini").
func TestSavedProviderSurvivesRestart(t *testing.T) {
	fc := FileConfig{}
	fc.setProvider(Provider{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "gemini-2.5-flash", APIKey: "gem-key-1"})
	fc.ActiveProvider = "gemini"
	// No env vars, no flags: the Spark defaults in cfg must NOT
	// switch the active provider.
	cfg := Config{BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: ""}
	foldEnvIntoConfig(&cfg, &fc, map[string]bool{}, false)
	if fc.ActiveProvider != "gemini" {
		t.Fatalf("saved provider must survive a restart with no env/flags, active=%q", fc.ActiveProvider)
	}
	if fc.Providers["gemini"].APIKey != "gem-key-1" {
		t.Fatalf("saved gemini key must survive, got %q", fc.Providers["gemini"].APIKey)
	}
	// But an explicitly given env key DOES win, as before.
	cfg2 := Config{BaseURL: "https://api.openai.com/v1", Model: "gpt-5", APIKey: "sk-test"}
	foldEnvIntoConfig(&cfg2, &fc, map[string]bool{}, true)
	if fc.ActiveProvider != "openai" || fc.Providers["openai"].APIKey != "sk-test" {
		t.Fatalf("explicit env values should switch provider, active=%q", fc.ActiveProvider)
	}
	// And an explicit --model flag does too.
	fc3 := FileConfig{}
	fc3.setProvider(Provider{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "gemini-2.5-flash", APIKey: "gem-key-1"})
	fc3.ActiveProvider = "gemini"
	cfg3 := Config{BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: ""}
	foldEnvIntoConfig(&cfg3, &fc3, map[string]bool{"model": true}, false)
	if fc3.ActiveProvider != "spark" {
		t.Fatalf("explicit --model flag should switch provider, active=%q", fc3.ActiveProvider)
	}
}

func TestKeySourceAndSavePreservesEnvKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// On disk, the gemini provider has NO key. At runtime (as if the
	// key came from env vars, folded in at startup) it has one.
	fc := loadFileConfig()
	fc.setProvider(Provider{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "gemini-2.5-flash", APIKey: ""})
	_ = saveFileConfig(fc)
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "gemini", APIKey: "env-key-xyz",
		Providers: map[string]Provider{"gemini": {Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "gemini-2.5-flash", APIKey: "env-key-xyz"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	// Settings must say where the key came from, not "(not set)".
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `"api_key_source":"env"`) {
		t.Fatalf("settings should report the key source as env, got %s", body)
	}
	if strings.Contains(body, "env-key-xyz") {
		t.Fatal("settings must never return the raw key")
	}
	// Saving Settings with a blank key field must NOT wipe the env key —
	// it keeps it and persists it to config.json.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"active_provider":"gemini"}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("settings POST = %d %s", rr.Code, rr.Body.String())
	}
	if saved := loadFileConfig(); saved.Providers["gemini"].APIKey != "env-key-xyz" {
		t.Fatalf("saving settings must preserve (and persist) the env key, got %q", saved.Providers["gemini"].APIKey)
	}
	// And now the source is "saved".
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	if !strings.Contains(rr.Body.String(), `"api_key_source":"saved"`) {
		t.Fatalf("after saving, the key source should be saved, got %s", rr.Body.String())
	}
}

// TestSettingsShowsActiveProvider is the regression test for the v7
// "doesn't show me API settings" bug: GET /api/settings used to
// return the legacy top-level base_url / model fields, which are
// empty in any config written through the providers flow — so the
// Settings page showed a blank base URL and no model even when a
// provider and key were saved. It must return the ACTIVE provider's
// values, and saving Settings must keep the top-level fields in sync.
func TestSettingsShowsActiveProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A config as the providers flow writes it: everything under
	// providers[spark], top-level fields empty.
	fc := FileConfig{}
	fc.setProvider(Provider{Name: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: "spark-key-9999"})
	fc.ActiveProvider = "spark"
	fc.BaseURL, fc.Model, fc.APIKey = "", "", ""
	_ = saveFileConfig(fc)
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: "spark-key-9999",
		Providers: map[string]Provider{"spark": {Name: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", APIKey: "spark-key-9999"}}}
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	s := &webServer{client: NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model), runner: runner, cfg: cfg,
		messages: []Message{{Role: "system", Content: "s"}}, system: "s"}
	mux := buildMux(s)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/settings", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `"base_url":"https://api.meta.ai/v1"`) {
		t.Fatalf("settings should show the active provider's base URL, got %s", body)
	}
	if !strings.Contains(body, `"model":"muse-spark-1.3"`) {
		t.Fatalf("settings should show the active provider's model, got %s", body)
	}
	if !strings.Contains(body, `"api_key_source":"saved"`) || !strings.Contains(body, "••••••••9999") {
		t.Fatalf("settings should show the saved key (masked), got %s", body)
	}
	if strings.Contains(body, "spark-key-9999") {
		t.Fatal("settings must never return the raw key")
	}
	// Saving Settings with a blank key must also sync the legacy
	// top-level fields, so the next GET shows the same values.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"active_provider":"spark","base_url":"https://api.meta.ai/v1","api_key":""}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rr, req)
	if saved := loadFileConfig(); saved.BaseURL != "https://api.meta.ai/v1" || saved.Model != "muse-spark-1.3" {
		t.Fatalf("saving settings must sync the top-level fields, got base=%q model=%q", saved.BaseURL, saved.Model)
	}
}

// TestSettingsLoadDoesNotAutoFetchModels is the regression test for
// the v7 Settings hang: loadSettings() used to end with a live
// /api/models fetch, so opening Settings could stall up to ~45s on
// a slow provider (compat list + fallbacks) before the page settled.
func TestSettingsLoadDoesNotAutoFetchModels(t *testing.T) {
	// The loadSettings() ending that used to fire a live fetch on
	// every Settings open must be gone (the provider-change
	// handler still refreshes — that one is an explicit action).
	if strings.Contains(webHTML, "press Fetch models.','');}refreshModelList();}") {
		t.Fatal("loadSettings must not auto-fetch the model list when Settings opens — Fetch models is an explicit button")
	}
	if !strings.Contains(webHTML, "press Fetch models.','');}}") {
		t.Fatal("loadSettings should end after showing the cached-list note")
	}
}

// TestPromptZeroCancels: typing 0 at a menu prompt cancels out of
// it instead of being taken as the value; empty keeps the default.
func TestPromptZeroCancels(t *testing.T) {
	if v, ok := prompt(bufio.NewScanner(strings.NewReader("0\n")), "Web port", "3759"); ok || v != "" {
		t.Fatalf("typing 0 should cancel the prompt, got (%q, %v)", v, ok)
	}
	if v, ok := prompt(bufio.NewScanner(strings.NewReader("\n")), "Web port", "3759"); !ok || v != "3759" {
		t.Fatalf("empty answer should keep the default, got (%q, %v)", v, ok)
	}
	if v, ok := prompt(bufio.NewScanner(strings.NewReader("8080\n")), "Web port", "3759"); !ok || v != "8080" {
		t.Fatalf("a typed value should be returned, got (%q, %v)", v, ok)
	}
}

// TestMenuExitZero: 0 exits the SSH menu at once.
func TestMenuExitZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := Config{UserName: "Takahashi", Workdir: t.TempDir(), Provider: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3", WebPort: 3759, WebAddr: "127.0.0.1"}
	client := NewClient(cfg.BaseURL, "", cfg.Model)
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: t.TempDir() + "/MEMORY.md"}
	ctrl := newWebController(cfg, client, runner, toolDefinitions(), "s")
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(strings.NewReader("0\n"))
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		showMenu(&cfg, client, runner, toolDefinitions(), "s", ctrl, scanner)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("menu did not exit on 0 within 5s")
	}
	// And 17 still exits, for muscle memory.
	cfg2 := cfg
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		scanner := bufio.NewScanner(strings.NewReader("17\n"))
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		showMenu(&cfg2, client, runner, toolDefinitions(), "s", ctrl, scanner)
	}()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("menu did not exit on 17 within 5s")
	}
}
