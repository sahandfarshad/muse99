package main

import (
	"fmt"
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
}

var (
	usageMu    sync.Mutex
	usageLabel string
)

func estimatedCost(prompt, completion int) float64 {
	return float64(prompt)/1_000_000*sparkInputPerMillion +
		float64(completion)/1_000_000*sparkOutputPerMillion
}

// loadUsage reads the persisted totals (zero value when none yet).
func loadUsage() UsageTotals {
	var u UsageTotals
	loadJSON("usage.json", &u)
	return u
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
	u.EstimatedCostUSD = estimatedCost(u.PromptTokens, u.CompletionTokens)
	u.UpdatedAt = time.Now()
	_ = saveJSON("usage.json", u)
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
	}
}

// usageShort is the one-line summary used in the menu header.
func usageShort() string {
	u := loadUsage()
	if u.Calls == 0 {
		return "Tokens: none yet"
	}
	return usageLine(u)
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
