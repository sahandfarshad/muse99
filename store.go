package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Persistent store for tasks, servers, sessions and goals.
// All JSON under ~/.muse99/ — portable, human-readable, no database needed.

type Task struct {
	ID        string    `json:"id"`
	Goal      string    `json:"goal"`
	Status    string    `json:"status"` // pending, running, done, failed
	Steps     []string  `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Summary   string    `json:"summary,omitempty"`
}

type Server struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	KeyPath  string `json:"key_path,omitempty"`
	Enabled  bool   `json:"enabled"`
	LastSeen string `json:"last_seen,omitempty"`
}

type Goal struct {
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func dataDir() string { return filepath.Join(configDir(), "data") }

func storePath(name string) string {
	_ = os.MkdirAll(dataDir(), 0o755)
	return filepath.Join(dataDir(), name)
}

func saveJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(storePath(name), data, 0o644)
}

func loadJSON(name string, v any) bool {
	data, err := os.ReadFile(storePath(name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func loadTasks() []Task {
	var t []Task
	loadJSON("tasks.json", &t)
	return t
}
func saveTasks(t []Task) error { return saveJSON("tasks.json", t) }

func addTask(goal string) Task {
	tasks := loadTasks()
	t := Task{ID: fmt.Sprintf("task-%d", time.Now().Unix()), Goal: goal, Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	tasks = append(tasks, t)
	_ = saveTasks(tasks)
	return t
}
func updateTask(id, status, summary string, steps []string) {
	tasks := loadTasks()
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].Status = status
			tasks[i].Summary = summary
			tasks[i].Steps = steps
			tasks[i].UpdatedAt = time.Now()
		}
	}
	_ = saveTasks(tasks)
}

func loadServers() []Server {
	var s []Server
	loadJSON("servers.json", &s)
	return s
}
func saveServers(s []Server) error { return saveJSON("servers.json", s) }

func loadGoals() []Goal {
	var g []Goal
	loadJSON("goals.json", &g)
	return g
}
func saveGoals(g []Goal) error { return saveJSON("goals.json", g) }

func saveSession(name string, messages []Message) error {
	return saveJSON("session-"+name+".json", messages)
}
func loadSession(name string) []Message {
	var m []Message
	loadJSON("session-"+name+".json", &m)
	return m
}
