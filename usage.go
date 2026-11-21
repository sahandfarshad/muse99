package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Token usage tracking (v2).
//
// Every /chat/completions response carries a usage block
// (prompt_tokens / completion_tokens / total_tokens). We capture it
// for every chat call and accumulate it in ~/.muse99/data/usage.json,
// so the user can see how many tokens their tasks use and roughly
// what they cost.
//
// Cost is an ESTIMATE at Spark rates: $1.25 per 1M input tokens and
// $4.25 per 1M output tokens. Those are the only rates we know for
// sure; other providers are estimated at the same rates and labelled
// as such. Ollama (local) costs $0 in API fees, but we still count
// its tokens.

const (
	sparkInputPerMillion  = 1.25
	sparkOutputPerMillion = 4.25
)

type UsageTotals struct {
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Calls            int       `json:"calls"`
	LastPromptTokens int       `json:"last_prompt_tokens"`
	LastCompletion   int       `json:"last_completion_tokens"`
	LastTotal        int       `json:"last_total_tokens"`
	LastLabel        string    `json:"last_label,omitempty"`
	EstimatedCostUSD float64   `json:"estimated_cost_usd"`
	UpdatedAt        time.Time `json:"updated_at"`
	// v0.8: which model the last call used, and totals per model
	// (key = model id as sent to the provider).
	LastModel string                 `json:"last_model,omitempty"`
	// v0.9.10: input tokens the provider served from its cache.
	CachedTokens int `json:"cached_tokens,omitempty"`
	ByModel   map[string]*ModelUsage `json:"by_model,omitempty"`
	// v0.9.17: cost at the user's own prices (Settings > Model prices),
	// worked out per call. UnpricedCalls = calls of a model with no price.
	CostUSD       float64 `json:"cost_usd,omitempty"`
	CostToman     float64 `json:"cost_toman,omitempty"`
	UnpricedCalls int     `json:"unpriced_calls,omitempty"`
}

// ModelUsage is the token count for one model.
type ModelUsage struct {
	Model            string    `json:"model"`
	Provider         string    `json:"provider,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Calls            int       `json:"calls"`
	CachedTokens     int       `json:"cached_tokens,omitempty"`
	LastUsed         time.Time `json:"last_used"`
	CostUSD          float64   `json:"cost_usd,omitempty"`
	CostToman        float64   `json:"cost_toman,omitempty"`
}

var (
	usageMu    sync.Mutex
	usageLabel string
)

func estimatedCost(prompt, completion int) float64 {
	return float64(prompt)/1_000_000*sparkInputPerMillion +
		float64(completion)/1_000_000*sparkOutputPerMillion
}

// loadUsageFromJSONFile reads the old usage.json totals (zero value when none yet).
func loadUsageFromJSONFile() UsageTotals {
	var u UsageTotals
	loadJSON("usage.json", &u)
	return u
}

// loadUsage reads the persisted totals (zero value when none yet).
// The DB is the source of truth when it is open; usage.json is the
// fallback (and is still written on every call as an export).
func loadUsage() UsageTotals {
	if appDB != nil {
		if u, ok := appDB.loadUsageDB(); ok {
			return u
		}
	}
	return loadUsageFromJSONFile()
}

// setUsageLabel tags the next recorded calls, e.g. "task task-123".
// The last task's tokens are then shown in the menu and web UI.
func setUsageLabel(label string) {
	usageMu.Lock()
	defer usageMu.Unlock()
	usageLabel = label
}

// recordUsage adds one API call's tokens to the totals and persists them.
// It is a no-op when the provider reported no usage (some local models
// do not return a usage block).
func recordUsage(prompt, completion int) UsageTotals {
	return recordUsageFor("", "", prompt, completion)
}

// recordUsageFor is recordUsage tagged with the model (and provider)
// that answered, so usage can be shown per model.
func recordUsageFor(provider, model string, prompt, completion int) UsageTotals {
	usageMu.Lock()
	defer usageMu.Unlock()
	u := loadUsage()
	if prompt <= 0 && completion <= 0 {
		return u
	}
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	u.TotalTokens += prompt + completion
	u.Calls++
	u.LastPromptTokens = prompt
	u.LastCompletion = completion
	u.LastTotal = prompt + completion
	u.LastLabel = usageLabel
	u.LastModel = model
	var mu *ModelUsage
	if model != "" {
		if u.ByModel == nil {
			u.ByModel = map[string]*ModelUsage{}
		}
		m := u.ByModel[model]
		if m == nil {
			m = &ModelUsage{Model: model}
			u.ByModel[model] = m
		}
		if provider != "" {
			m.Provider = provider
		}
		m.PromptTokens += prompt
		m.CompletionTokens += completion
		m.TotalTokens += prompt + completion
		m.Calls++
		m.LastUsed = time.Now()
		mu = m
	}
	addCost(&u, mu, model, prompt, completion)
	u.EstimatedCostUSD = estimatedCost(u.PromptTokens, u.CompletionTokens)
	u.UpdatedAt = time.Now()
	_ = saveJSON("usage.json", u)
	if appDB != nil {
		_ = appDB.saveUsageDB(u)
	}
	return u
}

// usageLine formats totals as one line, e.g.
// "Tokens: 1,234 in / 567 out, est. $0.004 (Spark rates)".
func usageLine(u UsageTotals) string {
	return fmt.Sprintf("Tokens: %s in / %s out, est. $%.4f (Spark rates)",
		formatInt(u.PromptTokens), formatInt(u.CompletionTokens), u.EstimatedCostUSD)
}

// usageTotalsJSON is the usage object returned by /api/status and
// /api/settings (same field names as the persisted file).
func usageTotalsJSON() map[string]any {
	u := loadUsage()
	return map[string]any{
		"prompt_tokens": u.PromptTokens, "completion_tokens": u.CompletionTokens,
		"total_tokens": u.TotalTokens, "calls": u.Calls,
		"estimated_cost_usd": u.EstimatedCostUSD,
		"last_prompt_tokens": u.LastPromptTokens, "last_completion_tokens": u.LastCompletion,
		"last_total_tokens": u.LastTotal, "last_label": u.LastLabel,
		"last_model": u.LastModel, "by_model": modelUsageList(u),
		"cost_usd": round6(u.CostUSD), "cost_toman": round6(u.CostToman), "unpriced_calls": u.UnpricedCalls,
	}
}

// modelUsageList returns per-model totals, most tokens first.
func modelUsageList(u UsageTotals) []ModelUsage {
	out := []ModelUsage{}
	for _, m := range u.ByModel {
		if m != nil {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// usageDelta is what was used between two snapshots, e.g. one chat
// reply or one task: tokens, calls, and the model(s) that answered.
type usageDelta struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Calls            int    `json:"calls"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	Model            string `json:"model,omitempty"`
	// v0.9.17: cost at the user's prices; Unpriced = calls with no price.
	CostUSD   float64 `json:"cost_usd,omitempty"`
	CostToman float64 `json:"cost_toman,omitempty"`
	Unpriced  int     `json:"unpriced,omitempty"`
}

func diffUsage(before, after UsageTotals) usageDelta {
	d := usageDelta{
		PromptTokens:     after.PromptTokens - before.PromptTokens,
		CompletionTokens: after.CompletionTokens - before.CompletionTokens,
		TotalTokens:      after.TotalTokens - before.TotalTokens,
		Calls:            after.Calls - before.Calls,
		CachedTokens:     after.CachedTokens - before.CachedTokens,
		CostUSD:          round6(after.CostUSD - before.CostUSD),
		CostToman:        round6(after.CostToman - before.CostToman),
		Unpriced:         after.UnpricedCalls - before.UnpricedCalls,
	}
	models := []string{}
	for name, m := range after.ByModel {
		var prev int
		if b := before.ByModel[name]; b != nil {
			prev = b.Calls
		}
		if m != nil && m.Calls > prev {
			models = append(models, name)
		}
	}
	sort.Strings(models)
	d.Model = strings.Join(models, ", ")
	if d.Model == "" && d.Calls > 0 {
		d.Model = after.LastModel
	}
	return d
}

// usageShort is the one-line summary used in the menu header.
func usageShort() string {
	u := loadUsage()
	if u.Calls == 0 {
		return "Tokens: none yet"
	}
	line := usageLine(u)
	if u.LastModel != "" {
		line += " · last model: " + u.LastModel
	}
	return line
}

func formatInt(n int) string {
	s := fmt.Sprint(n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

// recordCached adds input tokens a provider reported as cached.
func recordCached(model string, n int) {
	if n <= 0 {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	u := loadUsage()
	u.CachedTokens += n
	if m := u.ByModel[model]; m != nil {
		m.CachedTokens += n
	}
	_ = saveJSON("usage.json", u)
	if appDB != nil {
		_ = appDB.saveUsageDB(u)
	}
}
