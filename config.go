package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Production config: file > env > flags (flags already applied in main).
// Stored at ~/.muse99/config.json — never commit it, it holds your API key.
// Also loads a local .env file if present (KEY=VALUE lines).

type FileConfig struct {
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	UserName string `json:"user_name,omitempty"`
	Workdir  string `json:"workdir,omitempty"`
	Port     int    `json:"port,omitempty"`
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muse99")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

func loadFileConfig() FileConfig {
	var c FileConfig
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveFileConfig(c FileConfig) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), data, 0o600)
}

func loadDotEnv(workdir string) {
	for _, p := range []string{filepath.Join(workdir, ".env"), filepath.Join(configDir(), ".env")} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range splitLines(string(data)) {
			line = trimSpace(line)
			if line == "" || len(line) > 0 && line[0] == '#' {
				continue
			}
			parts := splitN(line, "=", 2)
			if len(parts) == 2 {
				k, v := trimSpace(parts[0]), trimSpace(trimQuotes(parts[1]))
				if os.Getenv(k) == "" {
					_ = os.Setenv(k, v)
				}
			}
		}
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
func trimQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}
func splitN(s, sep string, n int) []string {
	var out []string
	for len(out) < n-1 {
		i := indexOf(s, sep)
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
	return append(out, s)
}
func indexOf(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}
