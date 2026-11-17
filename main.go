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
	flag.StringVar(&cfg.UserName, "user", envOr("MUSE_USER_NAME", "Takahashi"), "User's name")
	flag.StringVar(&cfg.MemoryPath, "memory", os.Getenv("MUSE_MEMORY"), "Path to MEMORY.md")
	flag.StringVar(&cfg.Workdir, "workdir", envOr("MUSE_WORKDIR", mustCwd()), "Working directory for file/shell tools")
	flag.StringVar(&cfg.Prompt, "prompt", "", "Single prompt, then exit (non-interactive)")
	flag.BoolVar(&cfg.Web, "web", false, "Start web UI instead of terminal chat")
	flag.IntVar(&cfg.WebPort, "port", 8080, "Web UI port (with --web)")
	flag.StringVar(&cfg.WebAddr, "addr", "0.0.0.0", "Web UI address (with --web)")
	flag.BoolVar(&cfg.Menu, "menu", true, "Show SSH menu on top (web address + options). Use --menu=false for plain terminal chat.")
	flag.StringVar(&cfg.Task, "task", "", "Autonomous task: work till DONE, up to 100 steps")
	flag.BoolVar(&cfg.Schedule, "schedule", false, "Run scheduler (scheduled.json, every minute)")
	flag.Parse()
	// Config file + .env fill gaps left by env/flags.
	loadDotEnv(cfg.Workdir)
	fc := loadFileConfig()
	// Env/flags are the default provider (usually Spark). Fold them into
	// the providers map so switching away and back keeps the right key.
	envProvider := matchProvider(cfg.BaseURL, cfg.Model)
	if cfg.APIKey != "" || cfg.BaseURL != "" || cfg.Model != "" {
		fc.setProvider(Provider{Name: envProvider, BaseURL: cfg.BaseURL, Model: cfg.Model, APIKey: cfg.APIKey})
		// Only treat the env values as the active provider when the user
		// actually set them (or no file config existed yet).
		if os.Getenv("MODEL_API_KEY") != "" || os.Getenv("MODEL_API_BASE_URL") != "" || os.Getenv("MODEL_ID") != "" {
			fc.ActiveProvider = envProvider
		}
	}
	applyFileConfig(&cfg, fc)
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
	if cfg.Prompt == "" && flag.NArg() > 0 {
		cfg.Prompt = strings.Join(flag.Args(), " ")
	}
	return cfg
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
	cfg := loadConfig()
	// Open the embedded database (chats, memories, usage). If it
	// cannot open, keep going with the old file-only behaviour so a
	// DB problem never stops the agent from starting.
	if db, err := openAppDB(); err != nil {
		fmt.Println("Note: database could not be opened, using files only:", err)
	} else {
		defer db.Close()
	}
	if cfg.APIKey == "" {
		fmt.Println("Missing API key. Set MODEL_API_KEY, e.g.:")
		fmt.Println("  export MODEL_API_KEY=your-key")
		fmt.Println("  export MODEL_API_BASE_URL=https://api.meta.ai/v1")
		fmt.Println("  export MODEL_ID=muse-spark-1.3")
		fmt.Println("Or for any other provider, change MODEL_API_BASE_URL / MODEL_ID.")
		os.Exit(1)
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
		fmt.Println(runAutonomous(client, runner, tools, system, cfg.Task, cfg.Workdir))
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
		showMenu(&cfg, client, runner, tools, system, ctrl)
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
	*messages = append(*messages, Message{Role: "user", Content: input})
	var used []string

	for step := 0; step < 20; step++ {
		msg, err := client.Chat(*messages, tools)
		if err != nil {
			return "Sorry — I couldn't reach the API: " + err.Error(), used
		}
		*messages = append(*messages, msg)

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Content) == "" {
				return "(no reply)", used
			}
			return msg.Content, used
		}

		for _, tc := range msg.ToolCalls {
			used = append(used, tc.Function.Name)
			fmt.Printf("  [tool] %s %s\n", tc.Function.Name, truncate(tc.Function.Arguments, 200))
			result := runner.run(tc.Function.Name, tc.Function.Arguments)
			*messages = append(*messages, Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}
	return "I did a lot of tool steps and stopped — try breaking the task into smaller pieces.", used
}

var _ = filepath.Join
