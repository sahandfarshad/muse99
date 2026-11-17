package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

// SSH menu — what you see when you SSH into your Ubuntu server and run `muse`.
//
// The web address is always on top, so you can copy it into your browser:
//
//   ═══════════════════════════════════════════════════
//   ✦ Muse99 — Web UI: http://192.168.1.10:8080
//   ═══════════════════════════════════════════════════
//
// Then a simple numbered menu. No TUI library, no mouse — just type 1-6.

func webAddress(port int) string {
	// Best-effort LAN IP, so the address works from another machine.
	// Falls back to localhost if we cannot determine it.
	if ip := outboundIP(); ip != "" {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}
	hostname, _ := os.Hostname()
	if hostname != "" {
		return fmt.Sprintf("http://%s:%d  (or http://localhost:%d)", hostname, port, port)
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

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

func printMenuHeader(cfg Config, webRunning bool) {
	fmt.Print("\033[H\033[2J") // clear screen, keep it tidy over SSH
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("  ✦ Muse99  —  Web UI: %s\n", webAddress(cfg.WebPort))
	if webRunning {
		fmt.Println("  Status: web UI is RUNNING  ·  open the address above in your browser")
	} else {
		fmt.Println("  Status: web UI is stopped  ·  choose 1 to start it")
	}
	mode := "generic"
	if isSpark(cfg.BaseURL, cfg.Model) {
		mode = "Muse Spark — Muse mode"
	}
	fmt.Printf("  Model: %s  ·  %s\n", cfg.Model, mode)
	fmt.Printf("  User: %s  ·  Workdir: %s\n", cfg.UserName, cfg.Workdir)
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println()
}

func showMenu(cfg *Config, client *Client, runner *toolRunner, tools []ToolDef, system string, webRunning *bool, startWebFn func() error) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	messages := []Message{{Role: "system", Content: system}}

	for {
		printMenuHeader(*cfg, *webRunning)
		fmt.Println("  1  Start web UI")
		fmt.Println("  2  Terminal chat")
		fmt.Println("  3  Show web address / status")
		fmt.Println("  4  Change API / model")
		fmt.Println("  5  Show memory")
		fmt.Println("  6  Quit")
		fmt.Println()
		fmt.Print("  Choose [1-6]: ")
		if !scanner.Scan() {
			return
		}
		choice := strings.TrimSpace(scanner.Text())
		fmt.Println()

		switch choice {
		case "1":
			if *webRunning {
				fmt.Println("Web UI is already running at", webAddress(cfg.WebPort))
			} else {
				go func() {
					_ = startWebFn()
				}()
				*webRunning = true
				fmt.Println("Starting web UI…")
				fmt.Println("Open in your browser:", webAddress(cfg.WebPort))
				fmt.Println("Also try: http://localhost:" + fmt.Sprint(cfg.WebPort))
			}
			fmt.Print("\nPress Enter to return to the menu… ")
			scanner.Scan()

		case "2":
			fmt.Println("Terminal chat — type /exit to return to the menu.")
			fmt.Println()
			runREPL(cfg, client, runner, tools, &messages, scanner)
			messages = []Message{{Role: "system", Content: system}}

		case "3":
			fmt.Println("Web UI address:", webAddress(cfg.WebPort))
			fmt.Println("Local address:  http://localhost:" + fmt.Sprint(cfg.WebPort))
			fmt.Println("Running:", *webRunning)
			fmt.Println("Model:", cfg.Model)
			fmt.Println("API base:", cfg.BaseURL)
			fmt.Print("\nPress Enter to return to the menu… ")
			scanner.Scan()

		case "4":
			fmt.Printf("API base URL [%s]: ", cfg.BaseURL)
			if scanner.Scan() {
				if v := strings.TrimSpace(scanner.Text()); v != "" {
					cfg.BaseURL = v
				}
			}
			fmt.Printf("Model [%s]: ", cfg.Model)
			if scanner.Scan() {
				if v := strings.TrimSpace(scanner.Text()); v != "" {
					cfg.Model = v
				}
			}
			fmt.Printf("API key [hidden, Enter to keep]: ")
			if scanner.Scan() {
				if v := strings.TrimSpace(scanner.Text()); v != "" {
					cfg.APIKey = v
				}
			}
			*client = *NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model)
			// Refresh the system prompt — Spark detection may have changed.
			system = systemPrompt(cfg.UserName, loadMemory(cfg.MemoryPath), cfg.BaseURL, cfg.Model)
			messages = []Message{{Role: "system", Content: system}}
			fmt.Println("Updated. New web address is still:", webAddress(cfg.WebPort))
			fmt.Print("\nPress Enter to return to the menu… ")
			scanner.Scan()

		case "5":
			mem := loadMemory(cfg.MemoryPath)
			if strings.TrimSpace(mem) == "" {
				fmt.Println("(no memory saved yet)")
			} else {
				fmt.Println(mem)
			}
			fmt.Print("\nPress Enter to return to the menu… ")
			scanner.Scan()

		case "6", "q", "quit", "exit":
			fmt.Println("Bye, " + cfg.UserName + ".")
			return

		default:
			fmt.Println("Please choose 1-6.")
			fmt.Print("\nPress Enter to return to the menu… ")
			scanner.Scan()
		}
	}
}

// runREPL is the terminal chat, extracted so the menu can return to it.
func runREPL(cfg *Config, client *Client, runner *toolRunner, tools []ToolDef, messages *[]Message, scanner *bufio.Scanner) {
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
			fmt.Println("/help — this help\n/memory — show memory\n/clear — clear conversation\n/exit — back to menu")
			continue
		case "/memory":
			fmt.Println(loadMemory(cfg.MemoryPath))
			continue
		case "/clear":
			*messages = []Message{{Role: "system", Content: systemPrompt(cfg.UserName, loadMemory(cfg.MemoryPath), cfg.BaseURL, cfg.Model)}}
			fmt.Println("Conversation cleared.")
			continue
		}
		reply := runTurn(client, runner, tools, messages, input)
		fmt.Println("Muse> " + reply)
		fmt.Println()
	}
}
