package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultWebPort is the web UI port when the user has not set one.
// (v5: was 8080; 3759 is the new default. A port saved in
// ~/.muse99/config.json always wins over this default.)
const DefaultWebPort = 3759

// Production config: file > env > flags (flags already applied in main).
// Stored at ~/.muse99/config.json — never commit it, it holds your API keys.
// Also loads a local .env file if present (KEY=VALUE lines).
//
// v2: multiple API providers. Each provider keeps its own base URL, model
// and API key, so switching back to Spark after trying OpenAI restores the
// right key. The old single-provider fields (base_url / model / api_key)
// are still read and treated as the first provider, for backward compat.

type Provider struct {
	Name    string `json:"name,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
}

type FileConfig struct {
	BaseURL        string              `json:"base_url,omitempty"`
	APIKey         string              `json:"api_key,omitempty"`
	Model          string              `json:"model,omitempty"`
	UserName       string              `json:"user_name,omitempty"`
	Workdir        string              `json:"workdir,omitempty"`
	Port           int                 `json:"port,omitempty"`
	Domain         string              `json:"domain,omitempty"`
	TLSEnabled     bool                `json:"tls_enabled,omitempty"`
	CertFile       string              `json:"cert_file,omitempty"`
	KeyFile        string              `json:"key_file,omitempty"`
	ActiveProvider string              `json:"active_provider,omitempty"`
	Providers      map[string]Provider `json:"providers,omitempty"`
	AllowShell     bool                `json:"allow_shell,omitempty"`
}

// Provider presets. All are OpenAI-compatible /chat/completions endpoints,
// so the same agent code works with any of them.
func providerPresets() []Provider {
	return []Provider{
		{Name: "spark", BaseURL: "https://api.meta.ai/v1", Model: "muse-spark-1.3"},
		{Name: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-5"},
		{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/", Model: "gemini-2.5-flash"},
		{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "openai/gpt-5"},
		{Name: "ollama", BaseURL: "http://localhost:11434/v1", Model: "llama3.1", APIKey: "ollama"},
		{Name: "custom", BaseURL: "", Model: ""},
	}
}

func providerPresetNames() []string {
	out := []string{}
	for _, p := range providerPresets() {
		out = append(out, p.Name)
	}
	return out
}

// providerPreset returns the preset for a name, or false if unknown.
func providerPreset(name string) (Provider, bool) {
	for _, p := range providerPresets() {
		if p.Name == strings.ToLower(strings.TrimSpace(name)) {
			return p, true
		}
	}
	return Provider{}, false
}

// matchProvider guesses which preset a base URL / model belongs to.
func matchProvider(baseURL, model string) string {
	b := strings.ToLower(baseURL)
	m := strings.ToLower(model)
	switch {
	case strings.Contains(b, "api.meta.ai") || strings.Contains(m, "spark"):
		return "spark"
	case strings.Contains(b, "api.openai.com"):
		return "openai"
	case strings.Contains(b, "generativelanguage.googleapis.com"):
		return "gemini"
	case strings.Contains(b, "openrouter.ai"):
		return "openrouter"
	case strings.Contains(b, "localhost:11434") || strings.Contains(b, "127.0.0.1:11434"):
		return "ollama"
	case b == "":
		return "spark"
	default:
		return "custom"
	}
}

func maskKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 4 {
		return "••••"
	}
	return "••••••••" + key[len(key)-4:]
}

// ensureProviders fills in preset entries (with empty keys) for any
// provider the user has not configured yet, and picks a sane active one.
func (fc *FileConfig) ensureProviders() {
	if fc.Providers == nil {
		fc.Providers = map[string]Provider{}
	}
	for _, p := range providerPresets() {
		if _, ok := fc.Providers[p.Name]; !ok {
			fc.Providers[p.Name] = p
		}
	}
	if fc.ActiveProvider == "" {
		fc.ActiveProvider = "spark"
	}
	if _, ok := fc.Providers[fc.ActiveProvider]; !ok {
		fc.ActiveProvider = "spark"
	}
}

// activeProvider returns the active provider, falling back to the
// legacy single-provider fields when no providers map exists.
func (fc FileConfig) activeProvider() Provider {
	if fc.Providers != nil {
		if p, ok := fc.Providers[fc.ActiveProvider]; ok {
			return p
		}
	}
	return Provider{Name: fc.ActiveProvider, BaseURL: fc.BaseURL, Model: fc.Model, APIKey: fc.APIKey}
}

// setProvider stores one provider's settings (keeping its existing key
// when the new key is blank) and makes it active.
func (fc *FileConfig) setProvider(p Provider) {
	fc.ensureProviders()
	if p.Name == "" {
		return
	}
	// v6: never persist a "models/..." prefixed id (Gemini's native
	// list format) — chat completions need the bare id.
	p.Model = normalizeModelID(p.Model)
	old := fc.Providers[p.Name]
	if p.BaseURL == "" {
		p.BaseURL = old.BaseURL
	}
	if p.Model == "" {
		p.Model = old.Model
	}
	if p.APIKey == "" {
		p.APIKey = old.APIKey
	}
	fc.Providers[p.Name] = p
	fc.ActiveProvider = p.Name
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muse99")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

func certDir() string { return filepath.Join(configDir(), "certs") }

func loadFileConfig() FileConfig {
	var c FileConfig
	data, err := os.ReadFile(configPath())
	if err != nil {
		// No file yet: still return the presets so Settings / the menu
		// show real provider defaults instead of empty fields.
		c.ensureProviders()
		return c
	}
	_ = json.Unmarshal(data, &c)
	// Backward compat: a pre-v2 file only has base_url/model/api_key.
	// Fold those into the matching provider so nothing is lost.
	if len(c.Providers) == 0 && (c.BaseURL != "" || c.Model != "" || c.APIKey != "") {
		name := matchProvider(c.BaseURL, c.Model)
		c.Providers = map[string]Provider{name: {Name: name, BaseURL: c.BaseURL, Model: c.Model, APIKey: c.APIKey}}
		c.ActiveProvider = name
	}
	c.ensureProviders()
	return c
}

func saveFileConfig(c FileConfig) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	c.ensureProviders()
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), data, 0o600)
}

// configFromRuntime builds a FileConfig to save from the live Config.
func configFromRuntime(cfg Config) FileConfig {
	fc := loadFileConfig()
	fc.BaseURL = cfg.BaseURL
	fc.Model = cfg.Model
	fc.APIKey = cfg.APIKey
	fc.UserName = cfg.UserName
	fc.Workdir = cfg.Workdir
	fc.Port = cfg.WebPort
	fc.Domain = cfg.Domain
	fc.TLSEnabled = cfg.TLSEnabled
	fc.CertFile = cfg.CertFile
	fc.KeyFile = cfg.KeyFile
	fc.ActiveProvider = cfg.Provider
	fc.AllowShell = cfg.AllowShell
	if len(cfg.Providers) > 0 {
		fc.Providers = cfg.Providers
	}
	// Keep the active provider's entry in sync with the live values.
	if cfg.Provider != "" {
		fc.setProvider(Provider{Name: cfg.Provider, BaseURL: cfg.BaseURL, Model: cfg.Model, APIKey: cfg.APIKey})
	}
	fc.ensureProviders()
	return fc
}

// syncTopLevel copies the active provider's base URL / model / key
// into the legacy top-level fields, so every reader sees the same
// settings that are actually in use — whether they read the
// providers map or the old single-provider fields.
func syncTopLevel(fc FileConfig) FileConfig {
	ap := fc.activeProvider()
	fc.BaseURL = ap.BaseURL
	fc.Model = ap.Model
	fc.APIKey = ap.APIKey
	return fc
}

// applyFileConfig copies the persisted provider / web settings into cfg.
func applyFileConfig(cfg *Config, fc FileConfig) {
	fc.ensureProviders()
	p := fc.activeProvider()
	if p.BaseURL != "" {
		cfg.BaseURL = p.BaseURL
	}
	if p.Model != "" {
		cfg.Model = p.Model
	}
	if p.APIKey != "" {
		cfg.APIKey = p.APIKey
	}
	cfg.Provider = fc.ActiveProvider
	cfg.Providers = fc.Providers
	if fc.UserName != "" {
		cfg.UserName = fc.UserName
	}
	if fc.Port != 0 {
		cfg.WebPort = fc.Port
	}
	if fc.Domain != "" {
		cfg.Domain = fc.Domain
	}
	cfg.TLSEnabled = fc.TLSEnabled
	cfg.CertFile = fc.CertFile
	cfg.KeyFile = fc.KeyFile
	cfg.AllowShell = fc.AllowShell
}

// findCerts returns the cert/key pair to use for a domain, if any exist.
// It checks ~/.muse99/certs/ first, then /etc/letsencrypt/live/<domain>/.
func findCerts(domain string) (certFile, keyFile string, ok bool) {
	candidates := [][2]string{
		{filepath.Join(certDir(), "fullchain.pem"), filepath.Join(certDir(), "privkey.pem")},
	}
	if domain != "" {
		candidates = append(candidates, [2]string{
			filepath.Join("/etc/letsencrypt/live", domain, "fullchain.pem"),
			filepath.Join("/etc/letsencrypt/live", domain, "privkey.pem"),
		})
	}
	for _, c := range candidates {
		if _, err := os.Stat(c[0]); err == nil {
			if _, err := os.Stat(c[1]); err == nil {
				return c[0], c[1], true
			}
		}
	}
	return "", "", false
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
