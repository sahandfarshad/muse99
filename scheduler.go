package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Simple scheduler: tasks in ~/.muse99/data/scheduled.json
// { "time": "08:00", "prompt": "Check my tasks and summarise", "days": "mon-fri" }
// Run with: muse99 --schedule
// It wakes every minute, runs due prompts once per day, and logs to scheduled.log.
// This is the production-lite version of Muse's proactive checks —
// no cron needed, but you can also use cron to call `muse99 --prompt "..."`.

type Scheduled struct {
	Time   string `json:"time"` // HH:MM, local time
	Prompt string `json:"prompt"`
	Days   string `json:"days,omitempty"`
	LastRun string `json:"last_run,omitempty"`
}

func loadScheduled() []Scheduled {
	var s []Scheduled
	loadJSON("scheduled.json", &s)
	return s
}

func runScheduler(client *Client, runner *toolRunner, tools []ToolDef, system string) {
	fmt.Println("Muse99 scheduler running — Ctrl+C to stop. Checks every minute.")
	seen := map[string]string{}
	for {
		now := time.Now()
		for i, sc := range loadScheduled() {
			if sc.Time != now.Format("15:04") {
				continue
			}
			key := sc.Time + "|" + sc.Prompt
			if seen[key] == now.Format("2006-01-02") {
				continue
			}
			seen[key] = now.Format("2006-01-02")
			fmt.Printf("[%s] Running scheduled: %s\n", now.Format("15:04"), sc.Prompt)
			messages := []Message{{Role: "system", Content: system}, {Role: "user", Content: sc.Prompt}}
			reply := runTurn(client, runner, tools, &messages, sc.Prompt)
			log := fmt.Sprintf("%s — %s\n%s\n\n", now.Format(time.RFC3339), sc.Prompt, reply)
			_ = os.WriteFile(filepath.Join(dataDir(), "scheduled.log"), []byte(log), 0o644)
			_ = loadScheduled
			_ = i
		}
		time.Sleep(60 * time.Second)
	}
}
