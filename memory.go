package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func loadMemory(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Keep memory bounded in the prompt — last ~6000 chars is plenty.
	s := string(data)
	if len(s) > 6000 {
		s = s[len(s)-6000:]
	}
	return strings.TrimSpace(s)
}

func rememberFact(path, fact string) error {
	if path == "" || strings.TrimSpace(fact) == "" {
		return fmt.Errorf("no memory file or empty fact")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf("- %s — %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(fact))
	_, err = f.WriteString(line)
	return err
}

func defaultMemoryPath(workdir string) string {
	// Prefer ./MEMORY.md in the working directory, like Muse does.
	if workdir != "" {
		return filepath.Join(workdir, "MEMORY.md")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muse-go", "MEMORY.md")
}
