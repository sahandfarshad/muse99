package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
)

// v0.9.17: the user's own price for each model.
//
// For every model the user can type what 1M input tokens and 1M output
// tokens cost, in US dollars or in Toman. Each API call's cost is worked
// out when the call is made (with the price saved at that moment) and
// added to the usage totals, so a later price change does not rewrite
// what earlier messages cost. The chat header then shows what the chat
// cost so far and what the last message cost.
//
// Saved in config.json as "prices": {"<model id>": {...}}.

type ModelPrice struct {
	In       float64 `json:"in"`       // per 1M input (prompt) tokens
	Out      float64 `json:"out"`      // per 1M output (completion) tokens
	Currency string  `json:"currency"` // "usd" or "toman"
}

func normCurrency(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "toman", "irt", "tmn", "تومان":
		return "toman"
	}
	return "usd"
}

// priceFor returns the saved price for a model id.
func priceFor(fc FileConfig, model string) (ModelPrice, bool) {
	if model == "" || len(fc.Prices) == 0 {
		return ModelPrice{}, false
	}
	p, ok := fc.Prices[model]
	if !ok {
		p, ok = fc.Prices[strings.TrimSpace(model)]
	}
	if !ok || (p.In <= 0 && p.Out <= 0) {
		return ModelPrice{}, false
	}
	p.Currency = normCurrency(p.Currency)
	return p, true
}

// callCost is what one call cost in the model's currency.
func callCost(p ModelPrice, prompt, completion int) float64 {
	return float64(prompt)/1_000_000*p.In + float64(completion)/1_000_000*p.Out
}

// addCost adds one call's cost to the totals (and to the model's row).
// Calls of a model with no price are counted as unpriced.
func addCost(u *UsageTotals, m *ModelUsage, model string, prompt, completion int) {
	p, ok := priceFor(loadFileConfig(), model)
	if !ok {
		u.UnpricedCalls++
		return
	}
	c := callCost(p, prompt, completion)
	if p.Currency == "toman" {
		u.CostToman += c
		if m != nil {
			m.CostToman += c
		}
	} else {
		u.CostUSD += c
		if m != nil {
			m.CostUSD += c
		}
	}
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }

// knownModels is every model id the user is likely to want to price:
// the active one, the backup, recent picks, models that have usage,
// and models that already have a price.
func knownModels(fc FileConfig, active string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	add(active)
	if fc.Fallback != nil {
		add(fc.Fallback.Model)
	}
	for _, r := range fc.RecentModels {
		add(r.Model)
	}
	for _, p := range fc.Providers {
		add(p.Model)
	}
	u := loadUsage()
	names := []string{}
	for n := range u.ByModel {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		add(n)
	}
	priced := []string{}
	for n := range fc.Prices {
		priced = append(priced, n)
	}
	sort.Strings(priced)
	for _, n := range priced {
		add(n)
	}
	return out
}

type priceRow struct {
	Model    string  `json:"model"`
	In       float64 `json:"in"`
	Out      float64 `json:"out"`
	Currency string  `json:"currency"`
	Set      bool    `json:"set"`
}

// handlePrices serves GET /api/prices (every known model with its
// price) and POST /api/prices {model, in, out, currency}. Both prices
// 0 removes the model's price.
func (s *webServer) handlePrices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(msg string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
	}
	if r.Method == http.MethodPost {
		var body struct {
			Model    string  `json:"model"`
			In       float64 `json:"in"`
			Out      float64 `json:"out"`
			Currency string  `json:"currency"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fail("bad request: " + err.Error())
			return
		}
		model := strings.TrimSpace(body.Model)
		if model == "" || len(model) > 200 {
			fail("model id is required")
			return
		}
		if body.In < 0 || body.Out < 0 || math.IsNaN(body.In) || math.IsNaN(body.Out) || body.In > 1e12 || body.Out > 1e12 {
			fail("prices must be 0 or more")
			return
		}
		fc := loadFileConfig()
		if body.In == 0 && body.Out == 0 {
			delete(fc.Prices, model)
		} else {
			if fc.Prices == nil {
				fc.Prices = map[string]ModelPrice{}
			}
			fc.Prices[model] = ModelPrice{In: body.In, Out: body.Out, Currency: normCurrency(body.Currency)}
		}
		if err := saveFileConfig(fc); err != nil {
			fail(err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	fc := loadFileConfig()
	active := ""
	if s != nil && s.client != nil {
		active = s.client.Model
	}
	rows := []priceRow{}
	for _, m := range knownModels(fc, active) {
		row := priceRow{Model: m, Currency: "usd"}
		if p, ok := fc.Prices[m]; ok {
			row.In, row.Out, row.Currency, row.Set = p.In, p.Out, normCurrency(p.Currency), true
		}
		rows = append(rows, row)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "active": active, "prices": rows})
}
