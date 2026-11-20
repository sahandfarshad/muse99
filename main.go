package main

// Muse99 — a production personal agent in Go.
// Give it a task and it works till the end:
//   muse99 --task "Build a todo app in ./todo-app with tests" --allow-shell
// Chat, web UI, SSH menu, remote servers, scheduler, memory, goals.
// Any OpenAI-compatible API works. Spark = Muse mode:
//   MODEL_API_BASE_URL=https://api.meta.ai/v1 MODEL_ID=muse-spark-1.3 (or 1.4)
// Config: ~/.muse99/config.json, .env, env vars, then flags.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	BaseURL    string
	APIKey     string
	Model      string
	Provider   string
	Providers  map[string]Provider
	Domain     string
	TLSEnabled bool
	CertFile   string
	KeyFile    string
	UserName   string
	// NameSet is true once the user told us their name (first-run
	// question in the web UI / SSH menu, Settings, or --user /
	// MUSE_USER_NAME). Until then the UI asks for it.
	NameSet    bool
	MemoryPath string
	Workdir    string
	AllowShell bool
	Prompt     string
	Web        bool
	WebPort    int
	WebAddr    string
	Menu       bool
	Task       string
	Schedule   bool
	// v5: uninstall. --uninstall removes the binary + service files
	// (data only with --yes). `muse99 uninstall` is the same thing.
	Uninstall    bool
	UninstallYes bool
}

// switchProvider activates a provider by name, updating BaseURL/Model/APIKey
// from its stored settings. It returns false if the name is unknown.
func (cfg *Config) switchProvider(name string) bool {
	if cfg.Providers == nil {
		return false
	}
	p, ok := cfg.Providers[name]
	if !ok {
		return false
	}
	if p.BaseURL != "" {
		cfg.BaseURL = p.BaseURL
	}
	if p.Model != "" {
		cfg.Model = p.Model
	}
	cfg.APIKey = p.APIKey
	cfg.Provider = name
	return true
}

func loadConfig() Config {
	var cfg Config
	var flagAllowShell bool
	flag.BoolVar(&flagAllowShell, "allow-shell", false, "Allow run_shell tool")
	flag.StringVar(&cfg.BaseURL, "base-url", envOr("MODEL_API_BASE_URL", "https://api.meta.ai/v1"), "API base URL (OpenAI-compatible)")
	flag.StringVar(&cfg.APIKey, "api-key", envOr("MODEL_API_KEY", os.Getenv("OPENAI_API_KEY")), "API key (or MODEL_API_KEY env)")
	flag.StringVar(&cfg.Model, "model", envOr("MODEL_ID", "muse-spark-1.3"), "Model ID")
	flag.StringVar(&cfg.UserName, "user", envOr("MUSE_USER_NAME", ""), "User's name (asked on first run when not set)")
	flag.StringVar(&cfg.MemoryPath, "memory", os.Getenv("MUSE_MEMORY"), "Path to MEMORY.md")
	flag.StringVar(&cfg.Workdir, "workdir", envOr("MUSE_WORKDIR", mustCwd()), "Working directory for file/shell tools")
	flag.StringVar(&cfg.Prompt, "prompt", "", "Single prompt, then exit (non-interactive)")
	flag.BoolVar(&cfg.Web, "web", false, "Start web UI instead of terminal chat")
	flag.IntVar(&cfg.WebPort, "port", DefaultWebPort, "Web UI port (with --web)")
	flag.StringVar(&cfg.WebAddr, "addr", "0.0.0.0", "Web UI address (with --web)")
	flag.BoolVar(&cfg.Menu, "menu", true, "Show SSH menu on top (web address + options). Use --menu=false for plain terminal chat.")
	flag.StringVar(&cfg.Task, "task", "", "Autonomous task: work till DONE, up to 100 steps")
	flag.BoolVar(&cfg.Schedule, "schedule", false, "Run scheduler (scheduled.json, every minute)")
	flag.BoolVar(&cfg.Uninstall, "uninstall", false, "Uninstall Muse99 (binary + service files; data only with --yes)")
	flag.BoolVar(&cfg.UninstallYes, "yes", false, "With --uninstall: skip prompts and remove ~/.muse99 data too")
	flag.Parse()
	// Positional subcommand: `muse99 uninstall [--yes]`.
	if !cfg.Uninstall && flag.NArg() > 0 && flag.Arg(0) == "uninstall" {
		cfg.Uninstall = true
	}
	// Config file + .env fill gaps left by env/flags.
	loadDotEnv(cfg.Workdir)
	fc := loadFileConfig()
	// v6 root-cause fix: only fold env/flag values into the providers
	// map when the user ACTUALLY provided them — see foldEnvIntoConfig.
	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	envSet := os.Getenv("MODEL_API_KEY") != "" || os.Getenv("MODEL_API_BASE_URL") != "" ||
		os.Getenv("MODEL_ID") != "" || os.Getenv("OPENAI_API_KEY") != ""
	foldEnvIntoConfig(&cfg, &fc, explicit, envSet)
	applyFileConfig(&cfg, fc)
	// A name given on the command line or in MUSE_USER_NAME wins.
	if explicit["user"] || strings.TrimSpace(os.Getenv("MUSE_USER_NAME")) != "" {
		if n := cleanUserName(envOr("MUSE_USER_NAME", "")); n != "" && !isLegacyName(n) && !explicit["user"] {
			cfg.UserName = n
		}
		if explicit["user"] {
			cfg.UserName = cleanUserName(cfg.UserName)
		}
		if isLegacyName(cfg.UserName) {
			cfg.UserName = ""
		}
		cfg.NameSet = cfg.UserName != ""
	}
	// --allow-shell on the command line wins over the saved setting;
	// otherwise the saved checkbox value (from the web UI / menu) applies.
	if flagAllowShell {
		cfg.AllowShell = true
	}
	// A provider switch must also refresh the stored key if the file had
	// a key for the active provider and no env key overrode it.
	if cfg.APIKey == "" {
		if p, ok := fc.Providers[fc.ActiveProvider]; ok {
			cfg.APIKey = p.APIKey
		}
	}

	if cfg.MemoryPath == "" {
		cfg.MemoryPath = defaultMemoryPath(cfg.Workdir)
	}
	// Also honour a positional prompt: ./muse "do X"
	// (but not the `uninstall` subcommand — that is handled above.)
	if cfg.Prompt == "" && flag.NArg() > 0 && flag.Arg(0) != "uninstall" {
		cfg.Prompt = strings.Join(flag.Args(), " ")
	}
	return cfg
}

// foldEnvIntoConfig folds explicitly-provided env/flag values into the
// providers map. The flag defaults (Spark base URL / muse-spark-1.3)
// are always non-empty, so the old code folded them on every startup
// — and setProvider sets ActiveProvider unconditionally, silently
// switching the active provider back to Spark and dropping a saved
// Gemini (or other) provider after every restart. That is why Gemini
// "could not connect": the live client was Spark with no key, even
// though config.json still said gemini. Now the saved provider
// survives restarts; env/flags only win when explicitly given.
func foldEnvIntoConfig(cfg *Config, fc *FileConfig, explicit map[string]bool, envSet bool) {
	if !envSet && !explicit["api-key"] && !explicit["base-url"] && !explicit["model"] {
		return
	}
	envProvider := matchProvider(cfg.BaseURL, cfg.Model)
	fc.setProvider(Provider{Name: envProvider, BaseURL: cfg.BaseURL, Model: cfg.Model, APIKey: cfg.APIKey})
	fc.ActiveProvider = envProvider
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "--version" || a == "-version" || a == "version" {
			fmt.Println("muse99", versionLabel())
			return
		}
	}
	cfg := loadConfig()
	// Uninstall runs before anything else — no API key, no DB needed.
	if cfg.Uninstall {
		if err := runUninstallCLI(cfg.UninstallYes); err != nil {
			fmt.Println("Uninstall failed:", err)
			os.Exit(1)
		}
		return
	}
	// Open the embedded database (chats, memories, usage). If it
	// cannot open, keep going with the old file-only behaviour so a
	// DB problem never stops the agent from starting.
	//
	// v7: when the web UI is already running in another process
	// (the installer starts it detached, then opens this menu),
	// that process owns the database — bbolt takes an exclusive
	// lock, so opening it here would stall ~5s on the lock and
	// then fall back to files anyway. Skip it: the menu works
	// file-only, and the running web UI keeps the chats.
	if externalWebRunning(cfg) {
		fmt.Println("Note: web UI is already running in another process — it owns the database, so this menu runs without it.")
	} else if db, err := openAppDB(); err != nil {
		fmt.Println("Note: database could not be opened, using files only:", err)
	} else {
		defer db.Close()
		scrubLegacyName(cfg.UserName)
	}
	// v5: the API key is set inside the web UI (Settings → Provider &
	// model), not on the command line. So the menu / web UI may start
	// with no key at all — chat just shows a friendly notice until
	// the key is added. Only non-interactive modes still need a key.
	if cfg.APIKey == "" && !cfg.Menu && !cfg.Web {
		fmt.Println("No API key set yet.")
		fmt.Println("Start the web UI and add it in Settings → Provider & model:")
		fmt.Println("  muse99            # SSH menu — web UI link is on top")
		fmt.Println("Or set it for this run only: MODEL_API_KEY=your-key muse99 --menu=false")
		os.Exit(1)
	}
	if cfg.APIKey == "" {
		fmt.Println("No API key yet — open the web UI and add one in Settings → Provider & model.")
	}

	client := NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	runner := &toolRunner{workdir: cfg.Workdir, memoryPath: cfg.MemoryPath, allowShell: cfg.AllowShell, client: client}
	tools := toolDefinitions()
	runner.tools = tools
	// Sub-agent delegate: a smaller autonomous turn with the same model.
	runner.delegate = func(task string) string {
		sub := []Message{
			{Role: "system", Content: systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model)},
			{Role: "user", Content: "Sub-task (work it, verify it, return a short summary): " + task},
		}
		// Limit sub-agent to 10 tool steps by reusing runTurnDetailed with a cap via messages — it stops at 20, acceptable for a sub-task.
		reply, used := runTurnDetailed(client, runner, tools, &sub, task)
		return "Sub-agent used " + strings.Join(used, ", ") + ". Result: " + reply
	}

	memory := memoryContextFor("", cfg.MemoryPath)
	system := systemPrompt(cfg.UserName, memory, cfg.BaseURL, cfg.Model)

	mode := "generic"
	if isSpark(cfg.BaseURL, cfg.Model) {
		mode = "Muse Spark — Muse mode"
	}
	fmt.Printf("Muse99 · %s · provider %s · model %s\n", mode, cfg.Provider, cfg.Model)
	fmt.Printf("Workdir: %s · Memory: %s\n", cfg.Workdir, cfg.MemoryPath)
	if !cfg.AllowShell {
		fmt.Println("Shell tool is off — run with --allow-shell to let me run commands.")
	}
	if cfg.Schedule {
		runScheduler(client, runner, tools, system)
		return
	}
	if cfg.Task != "" {
		fmt.Println(runAutonomous(client, runner, tools, system, cfg.Task, cfg.Workdir, ""))
		return
	}
	messages := []Message{{Role: "system", Content: system}}

	if cfg.Prompt != "" {
		reply := runTurn(client, runner, tools, &messages, cfg.Prompt)
		fmt.Println(reply)
		return
	}

	// SSH menu is the default interactive mode: web address on top.
	if cfg.Menu {
		ctrl := newWebController(cfg, client, runner, tools, system)
		if cfg.Web {
			if err := ctrl.Start(cfg); err != nil {
				fmt.Println("Web server failed to start:", err)
			}
		}
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		showMenu(&cfg, client, runner, tools, system, ctrl, scanner)
		return
	}

	// Plain terminal chat (--menu=false, for scripts / CI).
	if cfg.Web {
		if err := startWeb(cfg, client, runner, tools, system); err != nil {
			fmt.Println("Web server failed:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println("Type /help for commands, /exit to quit.")
	fmt.Println()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	runREPL(&cfg, client, runner, tools, &messages, scanner)
}

// loadChatInto loads a DB chat's last 100 messages into *messages,
// with the system prompt first. Returns the chat id used.
func loadChatInto(chatID string, messages *[]Message, system string) string {
	if appDB == nil {
		*messages = []Message{{Role: "system", Content: system}}
		return ""
	}
	if chatID == "" {
		if c, err := appDB.DefaultChat(); err == nil {
			chatID = c.ID
		}
	}
	loaded, err := appDB.LoadMessages(chatID, 100)
	if err != nil {
		loaded = nil
	}
	*messages = []Message{{Role: "system", Content: system}}
	*messages = append(*messages, loaded...)
	return chatID
}

// persistTurn saves the messages added during one turn to the chat.
func persistTurn(chatID string, beforeLen int, after []Message) {
	if appDB == nil || chatID == "" || len(after) <= beforeLen {
		return
	}
	for _, m := range after[beforeLen:] {
		if m.Role == "system" {
			continue
		}
		_ = appDB.AddMessage(chatID, m)
	}
}

// refreshSystemFor injects the memories relevant to this user message
// into the system prompt, so each turn sees the right memories
// instead of only the ones from startup.
func refreshSystemFor(cfg *Config, messages *[]Message, input string) {
	if len(*messages) == 0 || (*messages)[0].Role != "system" {
		return
	}
	syncUserName(cfg)
	ctx := memoryContextFor(input, cfg.MemoryPath)
	(*messages)[0].Content = systemPrompt(cfg.UserName, ctx, cfg.BaseURL, cfg.Model)
}

// runTurn sends one user message and loops through tool calls
// until the model gives a final text answer (max 20 steps).
func runTurn(client *Client, runner *toolRunner, tools []ToolDef, messages *[]Message, input string) string {
	reply, _ := runTurnDetailed(client, runner, tools, messages, input)
	return reply
}

func runTurnDetailed(client *Client, runner *toolRunner, tools []ToolDef, messages *[]Message, input string) (string, []string) {
	return runTurnFallback(client, nil, runner, tools, messages, input)
}

// runTurnFallback is runTurnDetailed with a backup model: when the
// main model still fails after its retries, the same step is sent to
// the backup and the rest of the turn stays on it.
func runTurnFallback(client, fallback *Client, runner *toolRunner, tools []ToolDef, messages *[]Message, input string) (string, []string) {
	switchedFrom := ""
	*messages = append(*messages, Message{Role: "user", Content: input})
	var used []string
	activity.add("chat", "user", "New message", input, false)

	stopped := func() (string, []string) {
		activity.add("chat", "error", "Stopped by you", "", false)
		return fmt.Sprintf("⏹ Stopped by you after %d tool steps. What was done so far is saved in this chat; send \"continue\" to go on.", len(used)), used
	}
	for step := 0; step < 20; step++ {
		if ctl.checkpoint("chat") != nil {
			return stopped()
		}
		aid := activity.add("chat", "model", fmt.Sprintf("Waiting for %s (step %d, %d messages)…", client.Model, step+1, len(*messages)), "", true)
		msg, err := client.Chat(*messages, tools)
		if errors.Is(err, errStopped) {
			activity.finish(aid, false, "Model call cancelled", "")
			return stopped()
		}
		if err != nil {
			activity.finish(aid, false, "Model call failed", err.Error())
			if fallback != nil && switchedFrom == "" {
				activity.add("chat", "retry", "Switching to backup model "+fallback.Model, client.Model+" failed: "+err.Error(), false)
				switchedFrom = client.Model
				client = fallback
				step--
				continue
			}
			if len(used) > 0 {
				return "Sorry — I lost the API after " + fmt.Sprint(len(used)) + " tool steps: " + err.Error() + "\n\nNothing is lost: the files written so far are on disk and the steps are saved in this chat. Send \"continue\" and I'll pick up where I stopped.", used
			}
			return "Sorry — I couldn't reach the API: " + err.Error(), used
		}
		t, d := describeReply(msg)
		activity.finish(aid, true, t, d)
		*messages = append(*messages, msg)

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Content) == "" {
				return "(no reply)", used
			}
			activity.add("chat", "done", "Answer sent to chat", "", false)
			if switchedFrom != "" {
				return "↪ " + switchedFrom + " kept failing, so I finished with the backup model " + client.Model + ".\n\n" + msg.Content, used
			}
			return msg.Content, used
		}

		for i, tc := range msg.ToolCalls {
			if i > 0 && ctl.checkpoint("chat") != nil {
				// keep every tool call answered, or the next request 400s
				for _, rest := range msg.ToolCalls[i:] {
					*messages = append(*messages, Message{Role: "tool", Content: "not run: stopped by the user", ToolCallID: rest.ID})
				}
				return stopped()
			}
			used = append(used, tc.Function.Name)
			fmt.Printf("  [tool] %s %s\n", tc.Function.Name, truncate(tc.Function.Arguments, 200))
			tid := activity.add("chat", "tool", "Running "+tc.Function.Name+"…", prettyToolArgs(tc.Function.Arguments), true)
			result := runner.run(tc.Function.Name, tc.Function.Arguments)
			activity.finish(tid, true, "Ran "+tc.Function.Name, prettyToolArgs(tc.Function.Arguments)+"\n\n→ "+truncate(result, 2500))
			*messages = append(*messages, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}
	activity.add("chat", "error", "Stopped after 20 tool steps", "", false)
	return "I did a lot of tool steps and stopped — try breaking the task into smaller pieces.", used
}

var _ = filepath.Join
