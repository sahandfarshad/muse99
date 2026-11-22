package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func httpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	return client.Get(url)
}

// SSH menu — what you see when you SSH into your Ubuntu server and run `muse99`.
//
// The web address is always on top, so you can copy it into your browser.
// With a domain + SSL it shows https://your-domain, otherwise
// http://<lan-ip>:<port>.
//
// Menu (v7):
//   0 Exit   1 Start web UI   2 Terminal chat   3 Stop web UI
//   4 Restart web UI   5 Choose web port   6 Set domain
//   7 Set up SSL / Let's Encrypt   8 Update Muse99
//   9 Providers / API keys   10 Show memory   11 Show usage
//   12 Show web address / status   13 Chats
//   14 Toggle shell commands   16 Uninstall Muse99
//   (17 still exits, for muscle memory. 0 also cancels out of
//   any prompt inside the menu.)
//
// The web UI started from the menu runs as its own process, so it
// keeps serving after the menu quits. The menu hands its database
// handle to that process first (one owner at a time).

func webAddressFor(cfg Config) string {
	if cfg.WebAddr == "127.0.0.1" || cfg.Desktop {
		return fmt.Sprintf("http://127.0.0.1:%d", cfg.WebPort)
	}
	if cfg.Domain != "" {
		if cfg.TLSEnabled {
			if cfg.WebPort == 443 || cfg.WebPort == 0 {
				return "https://" + cfg.Domain
			}
			return fmt.Sprintf("https://%s:%d", cfg.Domain, cfg.WebPort)
		}
		if cfg.WebPort == 80 {
			return "http://" + cfg.Domain
		}
		return fmt.Sprintf("http://%s:%d", cfg.Domain, cfg.WebPort)
	}
	if ip := outboundIP(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, cfg.WebPort)
	}
	hostname, _ := os.Hostname()
	if hostname != "" {
		return fmt.Sprintf("http://%s:%d  (or http://localhost:%d)", hostname, cfg.WebPort, cfg.WebPort)
	}
	return fmt.Sprintf("http://localhost:%d", cfg.WebPort)
}

// webAddress is kept for compatibility with older callers.
func webAddress(port int) string { return webAddressFor(Config{WebPort: port}) }

func outboundIP() string {
	// UDP dial does not send traffic, it just picks the local interface.
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

func printMenuHeader(cfg Config, ctrl *webController) {
	webRunning := ctrl != nil && ctrl.anyWebRunning(cfg)
	fmt.Print("\033[H\033[2J") // clear screen, keep it tidy over SSH
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("  ✦ Muse99 %s  —  Web UI: %s\n", versionLabel(), webAddressFor(cfg))
	if webRunning {
		fmt.Println("  Status: web UI is RUNNING  ·  open the address above in your browser")
	} else {
		fmt.Println("  Status: web UI is stopped  ·  choose 1 to start it")
	}
	if cfg.TLSEnabled {
		if _, _, ok := findCerts(cfg.Domain); ok {
			fmt.Println("  SSL: on (certificate found)")
		} else {
			fmt.Println("  SSL: on, but NO certificate found — web UI will fall back to plain HTTP")
		}
	} else if cfg.Domain != "" {
		fmt.Println("  SSL: off  ·  domain:", cfg.Domain)
	}
	mode := "generic"
	if isSpark(cfg.BaseURL, cfg.Model) {
		mode = "Muse Spark — Muse mode"
	}
	fmt.Printf("  Provider: %s  ·  Model: %s  ·  %s\n", cfg.Provider, cfg.Model, mode)
	if strings.TrimSpace(cfg.APIKey) == "" {
		fmt.Println("  No API key yet — open the web UI and add one in Settings → Provider & model.")
	}
	shell := "OFF"
	if cfg.AllowShell {
		shell = "ON"
	}
	fmt.Printf("  Shell commands: %s  ·  %s\n", shell, usageShort())
	if loginConfigured() {
		u, _ := webCredentials()
		fmt.Printf("  Web login: %s\n", u)
	} else {
		fmt.Println("  Web login: NOT SET — the web UI is locked until you set one (option 15)")
	}
	name := cfg.UserName
	if n, ok := loadFileConfig().savedUserName(); ok {
		name = n
	}
	if name == "" {
		name = "(not set — option 17)"
	}
	fmt.Printf("  User: %s  ·  Workdir: %s\n", name, cfg.Workdir)
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println()
}

func pause(scanner *bufio.Scanner) {
	fmt.Print("\nPress Enter to return to the menu… ")
	scanner.Scan()
}

func prompt(scanner *bufio.Scanner, label, def string) (string, bool) {
	// Ask for one value. An empty answer keeps the default.
	// Typing 0 cancels out of the prompt (ok=false) instead of
	// being taken as a value — so 0 never becomes a port, a
	// domain, or a password by accident.
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	if !scanner.Scan() {
		return def, false
	}
	v := strings.TrimSpace(scanner.Text())
	if v == "0" {
		return "", false
	}
	if v == "" {
		return def, true
	}
	return v, true
}

// saveRuntimeConfig persists the live config (providers, port, domain, SSL).
func saveRuntimeConfig(cfg *Config) {
	_ = saveFileConfig(configFromRuntime(*cfg))
}

// refreshClient rebuilds the API client and system prompt after a
// provider / key change, and restarts the web UI if it was running
// so the new provider actually applies.
func refreshClient(cfg *Config, client *Client, ctrl *webController, system *string, messages *[]Message) {
	*client = *NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
	*system = systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model)
	*messages = []Message{{Role: "system", Content: *system}}
	if ctrl != nil && ctrl.IsRunning() {
		if err := ctrl.Restart(*cfg, *system); err != nil {
			fmt.Println("Web UI restart failed:", err)
		}
	} else if externalWebRunning(*cfg) {
		// The web UI is a separate process: restart it from the
		// new binary/config, so the provider change applies there
		// too instead of only in this menu.
		handOverDB()
		if err := restartDetachedWeb(*cfg); err != nil {
			fmt.Println("Web UI restart failed:", err)
		} else {
			fmt.Println("Web UI restarted with the new provider.")
		}
	}
}

// startDetachedWeb launches the web UI as its own process, so it
// keeps running after the menu quits. The child owns the database
// — call handOverDB first so it can open it.
//
// v9: two guards against reporting a false success. (1) Before
// starting, the port must NOT already answer — if the old process
// survived the stop (stale PID file, installed from another folder),
// the new child cannot bind and the old one would keep answering,
// which used to look like a successful start. (2) After the status
// poll succeeds, the child itself must still be alive: the poll can
// succeed against a leftover old process while the new child has
// already died on a bind failure.
func startDetachedWeb(cfg Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if externalWebRunning(cfg) {
		return fmt.Errorf("a web UI is still answering on port %d — stop it first (menu option 3), then try again", cfg.WebPort)
	}
	_ = os.MkdirAll(configDir(), 0o700)
	logFile, err := os.OpenFile(webLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "--menu=false", "--web", "--port", strconv.Itoa(cfg.WebPort), "--addr", cfg.WebAddr)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child when it exits, so it never lingers as a zombie.
	go func() { _ = cmd.Wait() }()
	_ = os.WriteFile(webPIDPath(), []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	// Wait until it answers (up to ~10s).
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 20; i++ {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", cfg.WebPort))
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				// Give a just-started child a moment to die on a
				// bind failure, then prove the child itself is
				// alive — the status answer alone may come from
				// a leftover old process.
				time.Sleep(300 * time.Millisecond)
				if !processAlive(cmd.Process.Pid) {
					return fmt.Errorf("web UI process (PID %d) exited — it probably could not bind port %d; see %s", cmd.Process.Pid, cfg.WebPort, webLogPath())
				}
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("web UI did not answer on port %d — see %s", cfg.WebPort, webLogPath())
}

// restartDetachedWeb stops a detached web UI (if any) and starts it
// again from the current binary, so an install or update takes effect.
func restartDetachedWeb(cfg Config) error {
	if externalWebRunning(cfg) {
		if err := stopExternalWeb(); err != nil {
			return err
		}
	}
	return startDetachedWeb(cfg)
}

// handOverDB closes the menu's database handle so a detached web UI
// process can own it. The menu keeps working afterwards in
// file-only mode (chat still works; chats are just not persisted
// from the menu while the web UI owns the database).
func handOverDB() {
	if appDB != nil {
		_ = appDB.Close()
		appDB = nil
	}
}

func showMenu(cfg *Config, client *Client, runner *toolRunner, tools []ToolDef, system string, ctrl *webController, scanner *bufio.Scanner) {
	// First run: ask the user's name once, then use it everywhere.
	syncUserName(cfg)
	if !cfg.NameSet {
		printMenuHeader(*cfg, ctrl)
		if askUserName(cfg, scanner, true) {
			system = systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model)
		}
	}
	messages := []Message{{Role: "system", Content: system}}

	for {
		printMenuHeader(*cfg, ctrl)
		fmt.Println("  1  Start web UI")
		fmt.Println("  2  Terminal chat")
		fmt.Println("  3  Stop web UI")
		fmt.Println("  4  Restart web UI")
		fmt.Println("  5  Choose web port")
		fmt.Println("  6  Set domain")
		fmt.Println("  7  Set up SSL / Let's Encrypt")
		fmt.Println("  8  Update / roll back (now " + versionLabel() + ")")
		fmt.Println("  9  Providers / API keys")
		fmt.Println("  10 Show memory")
		fmt.Println("  11 Show usage")
		fmt.Println("  12 Show web address / status")
		fmt.Println("  13 Chats")
		fmt.Println("  14 Toggle shell commands")
		fmt.Println("  15 Web login (username / password)")
		fmt.Println("  17 Your name")
		fmt.Println("  0  Exit")
		fmt.Println("  16 Uninstall Muse99")
		fmt.Println()
		fmt.Print("  Choose [0-17]: ")
		if !scanner.Scan() {
			return
		}
		choice := strings.TrimSpace(scanner.Text())
		fmt.Println()

		switch choice {
		case "1":
			if ctrl.anyWebRunning(*cfg) {
				fmt.Println("Web UI is already running at", webAddressFor(*cfg))
			} else {
				// v7: start it as its own process, so it keeps
				// running after you quit this menu. An in-process
				// web UI dies with the menu — that is why the UI
				// used to vanish after a Restart + quit.
				handOverDB()
				if err := startDetachedWeb(*cfg); err != nil {
					fmt.Println("Could not start web UI:", err)
					if appDB == nil {
						_, _ = openAppDB()
					}
				} else {
					fmt.Println("Web UI started at", webAddressFor(*cfg))
					fmt.Println("It keeps running after you quit this menu.")
					fmt.Println("Also try: http://localhost:" + fmt.Sprint(cfg.WebPort))
				}
			}
			pause(scanner)

		case "2":
			fmt.Println("Terminal chat — type /exit to return to the menu.")
			fmt.Println()
			runREPL(cfg, client, runner, tools, &messages, scanner)
			messages = []Message{{Role: "system", Content: system}}

		case "3":
			if ctrl.IsRunning() {
				if err := ctrl.Stop(); err != nil {
					fmt.Println(err)
				} else {
					fmt.Println("Web UI stopped.")
				}
			} else if externalWebRunning(*cfg) {
				if err := stopExternalWeb(); err != nil {
					fmt.Println("Could not stop the web UI:", err)
				} else {
					fmt.Println("Web UI stopped.")
				}
			} else {
				fmt.Println("Web UI is not running")
			}
			pause(scanner)

		case "4":
			if ctrl.IsRunning() {
				// Started with `muse99 --web`: the web UI lives
				// in this process — restart it in place.
				if err := ctrl.Restart(*cfg, system); err != nil {
					fmt.Println("Could not restart web UI:", err)
				} else {
					fmt.Println("Web UI restarted at", webAddressFor(*cfg))
				}
			} else {
				handOverDB()
				if err := restartDetachedWeb(*cfg); err != nil {
					fmt.Println("Could not restart web UI:", err)
					if appDB == nil {
						_, _ = openAppDB()
					}
				} else {
					fmt.Println("Web UI restarted at", webAddressFor(*cfg))
					fmt.Println("It keeps running after you quit this menu.")
				}
			}
			pause(scanner)

		case "5":
			portStr, ok := prompt(scanner, "Web port (0 to cancel)", fmt.Sprint(cfg.WebPort))
			if !ok {
				fmt.Println("Cancelled — nothing changed.")
				pause(scanner)
				continue
			}
			port, err := strconv.Atoi(portStr)
			if err != nil || port < 1 || port > 65535 {
				fmt.Println("That is not a valid port (1-65535). Nothing changed.")
			} else {
				cfg.WebPort = port
				saveRuntimeConfig(cfg)
				fmt.Printf("Web port is now %d.\n", port)
				if ctrl.IsRunning() {
					fmt.Println("Restarting web UI on the new port…")
					if err := ctrl.Restart(*cfg, system); err != nil {
						fmt.Println("Restart failed:", err)
					} else {
						fmt.Println("Web UI is now at", webAddressFor(*cfg))
					}
				} else {
					fmt.Println("Start the web UI (option 1) to use the new port.")
				}
			}
			pause(scanner)

		case "6":
			domain, ok := prompt(scanner, "Domain (example: muse99.example.com, blank to clear, 0 to cancel)", cfg.Domain)
			if !ok {
				fmt.Println("Cancelled — nothing changed.")
				pause(scanner)
				continue
			}
			cfg.Domain = strings.TrimSpace(domain)
			saveRuntimeConfig(cfg)
			if cfg.Domain == "" {
				fmt.Println("Domain cleared. The web link now shows your server IP.")
			} else {
				fmt.Println("Domain set to", cfg.Domain)
				fmt.Println("Web link is now:", webAddressFor(*cfg))
				fmt.Println("For https, set up SSL with option 7.")
			}
			pause(scanner)

		case "7":
			setupSSL(cfg, ctrl, scanner, system)
			pause(scanner)

		case "8":
			if versionsMenu(scanner) {
				restartWebAfterUpdate(cfg, ctrl, system)
			}
			pause(scanner)

		case "9":
			providerMenu(cfg, client, ctrl, scanner, &system, &messages)
			pause(scanner)

		case "10":
			if appDB != nil {
				if mems, err := appDB.ListMemories(); err == nil && len(mems) > 0 {
					for _, m := range mems {
						fmt.Printf("- [%s] %s\n", m.Kind, m.Text)
					}
					pause(scanner)
					continue
				}
			}
			mem := loadMemory(cfg.MemoryPath)
			if strings.TrimSpace(mem) == "" {
				fmt.Println("(no memory saved yet)")
			} else {
				fmt.Println(mem)
			}
			pause(scanner)

		case "11":
			u := loadUsage()
			fmt.Println("Token usage — all time (persisted in ~/.muse99/data/usage.json):")
			fmt.Println(" ", usageLine(u))
			fmt.Printf("  API calls: %d\n", u.Calls)
			if u.LastTotal > 0 {
				last := fmt.Sprintf("  Last call: %s in / %s out", formatInt(u.LastPromptTokens), formatInt(u.LastCompletion))
				if u.LastModel != "" {
					last += " · " + u.LastModel
				}
				if u.LastLabel != "" {
					last += " (" + u.LastLabel + ")"
				}
				fmt.Println(last)
			}
			if list := modelUsageList(u); len(list) > 0 {
				fmt.Println()
				fmt.Printf("  %-34s %12s %12s %12s %7s\n", "Model", "In", "Out", "Total", "Calls")
				for _, m := range list {
					name := m.Model
					if m.Provider != "" {
						name = m.Provider + " · " + m.Model
					}
					if len([]rune(name)) > 34 {
						name = string([]rune(name)[:33]) + "…"
					}
					fmt.Printf("  %-34s %12s %12s %12s %7d\n", name, formatInt(m.PromptTokens), formatInt(m.CompletionTokens), formatInt(m.TotalTokens), m.Calls)
				}
				fmt.Println("  (Per-model counts start from v0.8; older calls are only in the totals.)")
			}
			fmt.Println("  Cost is an estimate at Spark rates ($1.25 / 1M in, $4.25 / 1M out).")
			fmt.Println("  Ollama (local) costs $0 in API fees — its tokens are still counted.")
			pause(scanner)

		case "12":
			fmt.Println("Web UI address:", webAddressFor(*cfg))
			fmt.Println("Local address:  http://localhost:" + fmt.Sprint(cfg.WebPort))
			fmt.Println("Running:", ctrl.IsRunning())
			fmt.Println("Provider:", cfg.Provider)
			fmt.Println("Model:", cfg.Model)
			fmt.Println("API base:", cfg.BaseURL)
			fmt.Println("API key:", maskKey(cfg.APIKey))
			fmt.Println("Domain:", cfg.Domain)
			fmt.Println("SSL:", cfg.TLSEnabled)
			if cert, key, ok := findCerts(cfg.Domain); ok {
				fmt.Println("Certificate:", cert)
				fmt.Println("Key:", key)
			} else {
				fmt.Println("Certificate: none found")
			}
			pause(scanner)

		case "13":
			if appDB == nil {
				fmt.Println("No database — chats are not persistent in this run.")
				pause(scanner)
				continue
			}
			chats, _ := appDB.ListChats()
			if len(chats) == 0 {
				fmt.Println("(no chats yet — start one from Terminal chat or the web UI)")
			}
			for i, c := range chats {
				fmt.Printf("  %d  %s  (%d messages, %s)\n", i+1, c.Title, c.MsgCount, c.UpdatedAt.Format("2006-01-02 15:04"))
			}
			fmt.Println("Switch chats in Terminal chat with /open <number>, or in the web UI sidebar.")
			pause(scanner)

		case "14":
			runner.SetAllowShell(!runner.shellAllowed())
			cfg.AllowShell = runner.shellAllowed()
			saveRuntimeConfig(cfg)
			if cfg.AllowShell {
				fmt.Println("Shell commands are now ON — the agent can run commands on this server.")
				fmt.Println("Only turn this on in a folder you trust.")
			} else {
				fmt.Println("Shell commands are now OFF.")
			}
			pause(scanner)

		case "15":
			webLoginMenu(scanner)
			pause(scanner)

		case "16":
			if uninstallFlow(cfg, ctrl, scanner) {
				return
			}
			pause(scanner)

		case "17":
			askUserName(cfg, scanner, false)
			system = systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model)
			messages[0].Content = system
			pause(scanner)

		case "0", "q", "quit", "exit":
			// 0 is the exit (17 is now "Your name").
			// The web UI keeps running if it was started detached.
			return

		default:
			fmt.Println("Please choose 0-17.")
			pause(scanner)
		}
	}
}

// uninstallFlow is menu option 16. It stops the web UI, shows exactly
// what will be removed, and only uninstalls after the user types YES.
// Data (~/.muse99) is kept unless the user says yes. Returns true when
// Muse99 was uninstalled, so the menu exits right after.
func uninstallFlow(cfg *Config, ctrl *webController, scanner *bufio.Scanner) bool {
	t := defaultUninstallTarget()
	plan := planUninstall(t, false)
	fmt.Println("Uninstall Muse99 — this will remove:")
	if len(plan.Remove) == 0 {
		fmt.Println("  (nothing found to remove — is the binary already gone?)")
	}
	for _, r := range plan.Remove {
		fmt.Println("  -", r)
	}
	for _, k := range plan.Keep {
		fmt.Println("  (kept)", k)
	}
	fmt.Println()
	if !confirmUninstall(scanner, "uninstall Muse99") {
		fmt.Println("Uninstall cancelled — nothing was removed.")
		return false
	}
	removeData := false
	fmt.Print("Also delete your Muse99 data (~/.muse99: config, chats, memory, usage)? [y/N]: ")
	if scanner.Scan() {
		a := strings.ToLower(strings.TrimSpace(scanner.Text()))
		removeData = a == "y" || a == "yes"
	}
	// Stop the web UI first — including one started by the installer
	// in another process.
	if ctrl.IsRunning() {
		_ = ctrl.Stop()
	} else if externalWebRunning(*cfg) {
		_ = stopExternalWeb()
	}
	removed, kept, err := runUninstall(t, removeData)
	if err != nil {
		fmt.Println("Uninstall failed:", err)
		return false
	}
	printUninstallResult(removed, kept)
	return true
}

// providerMenu lists providers, switches the active one, and lets the
// user add a key (or a custom provider) one prompt at a time.
func providerMenu(cfg *Config, client *Client, ctrl *webController, scanner *bufio.Scanner, system *string, messages *[]Message) {
	for {
		fmt.Println("Providers — current:", cfg.Provider, "· key:", maskKey(cfg.APIKey))
		fmt.Println()
		names := providerPresetNames()
		for i, name := range names {
			p := cfg.Providers[name]
			mark := " "
			if name == cfg.Provider {
				mark = "*"
			}
			fmt.Printf("  %s %d  %-10s  %-55s  %s  key: %s\n", mark, i+1, name, p.BaseURL, p.Model, maskKey(p.APIKey))
		}
		fmt.Println()
		fmt.Println("  s  Switch provider (type its number or name)")
		fmt.Println("  a  Add / update a provider key")
		fmt.Println("  l  List models for the current provider")
		fmt.Println("  t  Test a model (does it answer?)")
		fmt.Println("  0  Back to main menu")
		fmt.Print("  Choose [s/a/l/t/0]: ")
		if !scanner.Scan() {
			return
		}
		choice := strings.ToLower(strings.TrimSpace(scanner.Text()))
		fmt.Println()
		switch choice {
		case "0", "b", "back", "":
			return
		case "l", "list":
			// Fetch the current provider's models and offer to pick one.
			list, err := getModels(cfg.BaseURL, cfg.APIKey, cfg.Provider, true)
			if err != nil {
				fmt.Println("Could not fetch models:", err)
				continue
			}
			if len(list.Models) == 0 {
				fmt.Println("Provider returned no models — type a model name by hand (option 'a').")
				continue
			}
			fmt.Printf("Models for %s (%s, fetched %s):\n", cfg.Provider, list.Source, list.FetchedAt.Format("15:04"))
			for i, m := range list.Models {
				mark := " "
				if m.ID == cfg.Model {
					mark = "*"
				}
				fmt.Printf("  %s %2d  %s\n", mark, i+1, m.ID)
			}
			pick, ok := prompt(scanner, "Pick a model number (Enter to keep "+cfg.Model+", 0 to cancel)", "")
			if !ok {
				fmt.Println("Cancelled — model unchanged.")
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(pick)); err == nil && n >= 1 && n <= len(list.Models) {
				cfg.Model = normalizeModelID(list.Models[n-1].ID)
				if cfg.Providers != nil {
					p := cfg.Providers[cfg.Provider]
					p.Model = cfg.Model
					cfg.Providers[cfg.Provider] = p
				}
				saveRuntimeConfig(cfg)
				refreshClient(cfg, client, ctrl, system, messages)
				fmt.Println("Model is now", cfg.Model)
			}
		case "t", "test":
			model, ok := prompt(scanner, "Model to test (0 to cancel)", cfg.Model)
			if !ok {
				fmt.Println("Cancelled.")
				continue
			}
			model = normalizeModelID(model)
			fmt.Printf("Testing %s …\n", model)
			reply, latency, err := testModel(cfg.BaseURL, cfg.APIKey, model, cfg.Provider)
			if err != nil {
				fmt.Println("  ✗ failed:", err)
				continue
			}
			fmt.Printf("  ✓ works — replied %q in %d ms\n", reply, latency)
		case "s", "switch":
			which, ok := prompt(scanner, "Provider number or name (0 to cancel)", cfg.Provider)
			if !ok {
				fmt.Println("Cancelled — provider unchanged.")
				continue
			}
			name := which
			if n, err := strconv.Atoi(which); err == nil && n >= 1 && n <= len(names) {
				name = names[n-1]
			}
			if !cfg.switchProvider(name) {
				fmt.Println("Unknown provider:", name)
				continue
			}
			if cfg.APIKey == "" && name != "ollama" {
				fmt.Println("No API key saved for", name, ". Add one with option 'a'.")
			}
			saveRuntimeConfig(cfg)
			refreshClient(cfg, client, ctrl, system, messages)
			fmt.Println("Switched to", name, "· model", cfg.Model, "· key:", maskKey(cfg.APIKey))
		case "a", "add":
			which, ok := prompt(scanner, "Provider (spark, openai, gemini, openrouter, ollama, custom — 0 to cancel)", cfg.Provider)
			if !ok {
				fmt.Println("Cancelled — nothing saved.")
				continue
			}
			preset, ok := providerPreset(which)
			if !ok {
				fmt.Println("Unknown provider:", which)
				continue
			}
			p := cfg.Providers[which]
			if p.BaseURL == "" {
				p.BaseURL = preset.BaseURL
			}
			if p.Model == "" {
				p.Model = preset.Model
			}
			if which == "custom" {
				base, ok := prompt(scanner, "API base URL (0 to cancel)", p.BaseURL)
				if !ok {
					fmt.Println("Cancelled — nothing saved.")
					continue
				}
				p.BaseURL = base
				mdl, ok := prompt(scanner, "Model (0 to cancel)", p.Model)
				if !ok {
					fmt.Println("Cancelled — nothing saved.")
					continue
				}
				p.Model = normalizeModelID(mdl)
			} else {
				m, ok := prompt(scanner, "Model (Enter to keep "+p.Model+", 0 to cancel)", p.Model)
				if !ok {
					fmt.Println("Cancelled — nothing saved.")
					continue
				}
				if m != "" {
					p.Model = normalizeModelID(m)
				}
			}
			key, ok := prompt(scanner, "API key (Enter to keep the saved one, 0 to cancel)", "")
			if !ok {
				fmt.Println("Cancelled — nothing saved.")
				continue
			}
			if key != "" {
				p.APIKey = key
			}
			if p.APIKey == "" && which == "ollama" {
				p.APIKey = "ollama"
			}
			if p.APIKey == "" {
				fmt.Println("No key entered — nothing saved for", which)
				continue
			}
			p.Name = which
			if cfg.Providers == nil {
				cfg.Providers = map[string]Provider{}
			}
			cfg.Providers[which] = p
			cfg.switchProvider(which)
			saveRuntimeConfig(cfg)
			refreshClient(cfg, client, ctrl, system, messages)
			fmt.Println("Saved", which, "· model", cfg.Model, "· key:", maskKey(cfg.APIKey))
			fmt.Println("It is now the active provider.")
		default:
			fmt.Println("Type s, a, l, t, or b.")
		}
		fmt.Println()
	}
}

// setupSSL walks the user through Let's Encrypt for their domain.
func setupSSL(cfg *Config, ctrl *webController, scanner *bufio.Scanner, system string) {
	if strings.TrimSpace(cfg.Domain) == "" {
		fmt.Println("Set a domain first (menu option 6), then come back here.")
		return
	}
	domain := cfg.Domain
	if cert, key, ok := findCerts(domain); ok {
		fmt.Println("Certificate already found:")
		fmt.Println("  ", cert)
		fmt.Println("  ", key)
		ans, ok := prompt(scanner, "Enable HTTPS with this certificate? (y/n, 0 to cancel)", "y")
		if !ok {
			fmt.Println("Cancelled — nothing changed.")
			return
		}
		if strings.ToLower(ans) == "y" || strings.ToLower(ans) == "yes" {
			if strings.HasPrefix(cert, "/etc/letsencrypt/") {
				// Root-only key: copy it to ~/.muse99/certs and keep
				// it in sync on renewal.
				c, k, err := installUserCerts(domain)
				if err != nil {
					fmt.Println(err)
					return
				}
				cert, key = c, k
			}
			cfg.TLSEnabled = true
			cfg.CertFile, cfg.KeyFile = cert, key
			saveRuntimeConfig(cfg)
			fmt.Println("SSL is on. Restart the web UI (option 4) to serve https://" + domain)
		}
		return
	}
	fmt.Println("No certificate found yet for", domain)
	fmt.Printf("You need: a domain pointing at this server, port 80 open for the certificate check, and port %d open for the web UI.\n", cfg.WebPort)
	if _, err := exec.LookPath("certbot"); err != nil {
		fmt.Println("certbot is not installed. Run these commands on the server:")
		fmt.Println("  sudo apt update && sudo apt install -y certbot")
		fmt.Println("  sudo certbot certonly --standalone -d " + domain)
		fmt.Println("Then come back here — I will find the certificate and turn SSL on.")
		return
	}
	fmt.Println("certbot is installed.")
	email, ok := prompt(scanner, "Email for Let's Encrypt (expiry notices, 0 to cancel)", "")
	if !ok {
		fmt.Println("Cancelled — nothing changed.")
		return
	}
	if email == "" {
		fmt.Println("An email is required by certbot. Nothing changed.")
		return
	}
	if ctrl.IsRunning() {
		fmt.Println("Stopping the web UI first — certbot needs port 80 free.")
		_ = ctrl.Stop()
	}
	fmt.Println("Running certbot — this may ask for sudo…")
	cmd := exec.Command("sudo", "certbot", "certonly", "--standalone", "-d", domain, "--agree-tos", "--non-interactive", "--email", email)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Println("certbot failed:", err)
		fmt.Println("Check that the domain points at this server and port 80 is open, then try again.")
		return
	}
	cert, key, err := installUserCerts(domain)
	if err != nil {
		fmt.Println("certbot finished but:", err)
		fmt.Println("Check /etc/letsencrypt/live/" + domain + "/")
		return
	}
	fmt.Println("Renewal hook installed — renewed certificates are picked up automatically, no restart needed.")
	cfg.TLSEnabled = true
	cfg.CertFile, cfg.KeyFile = cert, key
	saveRuntimeConfig(cfg)
	fmt.Println("SSL is on — certificate found at", cert)
	fmt.Println("Restart the web UI (option 4) to serve", webAddressFor(*cfg))
	_ = system
}

// selfUpdate installs the latest release (kept for older callers).
func selfUpdate() error { return installRelease("") }

func downloadFile(url, dest string) error {
	// Small helper used only as a curl fallback in selfUpdate.
	resp, err := httpGet(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// runREPL is the terminal chat, extracted so the menu can return to it.
// Chats are persistent (v3): /chats lists them, /new starts one,
// /open switches, /summarise extracts durable facts into memory.
func runREPL(cfg *Config, client *Client, runner *toolRunner, tools []ToolDef, messages *[]Message, scanner *bufio.Scanner) {
	chatID := ""
	if appDB != nil {
		chatID = loadChatInto("", messages, (*messages)[0].Content)
		if chatID != "" {
			if c, err := appDB.GetChat(chatID); err == nil {
				fmt.Printf("(chat: %s)\n", c.Title)
			}
		}
	}
	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			return
		}
		input := strings.TrimSpace(scanner.Text())
		switch input {
		case "":
			continue
		case "/exit", "/quit", "/back", "/menu":
			fmt.Println("Back to menu.")
			return
		case "/help":
			fmt.Println("/help — this help\n/memory — show memory\n/clear — clear this chat\n/chats — list chats\n/new [title] — start a new chat\n/open <number or id> — switch chat\n/summarise — save durable facts from this chat to memory\n/shell — toggle shell commands\n/exit — back to menu")
			continue
		case "/memory":
			if appDB != nil {
				if mems, err := appDB.ListMemories(); err == nil && len(mems) > 0 {
					for _, m := range mems {
						fmt.Printf("- [%s] %s\n", m.Kind, m.Text)
					}
					continue
				}
			}
			fmt.Println(loadMemory(cfg.MemoryPath))
			continue
		case "/clear":
			if appDB != nil && chatID != "" {
				_ = appDB.ClearChat(chatID)
			}
			*messages = []Message{{Role: "system", Content: systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model)}}
			fmt.Println("Conversation cleared.")
			continue
		case "/chats":
			if appDB == nil {
				fmt.Println("No database — chats are not persistent in this run.")
				continue
			}
			chats, _ := appDB.ListChats()
			if len(chats) == 0 {
				fmt.Println("(no chats yet)")
				continue
			}
			for i, c := range chats {
				mark := " "
				if c.ID == chatID {
					mark = "*"
				}
				fmt.Printf("  %s %d  %s  (%d messages, %s)\n", mark, i+1, c.Title, c.MsgCount, c.UpdatedAt.Format("2006-01-02 15:04"))
			}
			continue
		case "/summarise":
			if appDB == nil || chatID == "" {
				fmt.Println("No database — nothing to summarise.")
				continue
			}
			n, err := summariseChat(client, appDB, chatID)
			if err != nil {
				fmt.Println("Summarise failed:", err)
				continue
			}
			fmt.Printf("Saved %d new fact(s) to memory.\n", n)
			continue
		case "/shell":
			runner.SetAllowShell(!runner.shellAllowed())
			cfg.AllowShell = runner.shellAllowed()
			saveRuntimeConfig(cfg)
			if cfg.AllowShell {
				fmt.Println("Shell commands are now ON. Only use in a folder you trust.")
			} else {
				fmt.Println("Shell commands are now OFF.")
			}
			continue
		}
		if strings.HasPrefix(input, "/new") {
			if appDB == nil {
				fmt.Println("No database — cannot start a persistent chat.")
				continue
			}
			title := strings.TrimSpace(strings.TrimPrefix(input, "/new"))
			c, err := appDB.CreateChat(title)
			if err != nil {
				fmt.Println("Could not create chat:", err)
				continue
			}
			chatID = loadChatInto(c.ID, messages, systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model))
			fmt.Printf("New chat: %s\n", c.Title)
			continue
		}
		if strings.HasPrefix(input, "/open ") {
			if appDB == nil {
				fmt.Println("No database — cannot switch chats.")
				continue
			}
			which := strings.TrimSpace(strings.TrimPrefix(input, "/open "))
			chats, _ := appDB.ListChats()
			target := ""
			if n, err := strconv.Atoi(which); err == nil && n >= 1 && n <= len(chats) {
				target = chats[n-1].ID
			} else {
				target = which
			}
			if _, err := appDB.GetChat(target); err != nil {
				fmt.Println("No such chat:", which, "— use /chats to list them.")
				continue
			}
			chatID = loadChatInto(target, messages, systemPrompt(cfg.UserName, memoryContextFor("", cfg.MemoryPath), cfg.BaseURL, cfg.Model))
			if c, err := appDB.GetChat(chatID); err == nil {
				fmt.Printf("Opened chat: %s (%d messages)\n", c.Title, c.MsgCount)
			}
			continue
		}
		before := len(*messages)
		memCtx := refreshSystemFor(cfg, messages, input)
		reply, _ := runTurnWithContext(client, nil, runner, tools, messages, input, memCtx)
		persistTurn(chatID, before, *messages)
		if n := maybeAutoSummarise(client, appDB, chatID); n > 0 {
			fmt.Printf("(saved %d new fact(s) to memory)\n", n)
		}
		fmt.Println("Muse> " + reply)
		fmt.Println()
	}
}
