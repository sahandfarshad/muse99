package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestModelPricesCost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	old := appDB
	appDB = nil
	defer func() { appDB = old }()
	fc := loadFileConfig()
	fc.Prices = map[string]ModelPrice{
		"glm":   {In: 0.5, Out: 2, Currency: "usd"},
		"cheap": {In: 100000, Out: 400000, Currency: "toman"},
	}
	if err := saveFileConfig(fc); err != nil {
		t.Fatal(err)
	}
	before := loadUsage()
	recordUsageFor("p", "glm", 1_000_000, 500_000) // 0.5 + 1.0 = $1.5
	recordUsageFor("p", "cheap", 10_000, 1_000)    // 1000 + 400 = 1400 toman
	recordUsageFor("p", "noprice", 5000, 100)
	d := diffUsage(before, loadUsage())
	if !near(d.CostUSD, 1.5) || !near(d.CostToman, 1400) || d.Unpriced != 1 {
		t.Fatalf("cost %+v", d)
	}
	u := loadUsage()
	if !near(u.ByModel["glm"].CostUSD, 1.5) || !near(u.ByModel["cheap"].CostToman, 1400) {
		t.Fatalf("per model %+v %+v", u.ByModel["glm"], u.ByModel["cheap"])
	}
	// A later price change does not rewrite earlier cost.
	fc = loadFileConfig()
	fc.Prices["glm"] = ModelPrice{In: 10, Out: 10, Currency: "usd"}
	_ = saveFileConfig(fc)
	if !near(loadUsage().CostUSD, 1.5) {
		t.Fatal("old cost changed")
	}
}

func TestChatCostAdds(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, _ := d.CreateChat("x")
	d.AddChatUsage(c.ID, usageDelta{TotalTokens: 10, Calls: 1, CostUSD: 0.25, CostToman: 100})
	ch, _ := d.AddChatUsage(c.ID, usageDelta{TotalTokens: 5, Calls: 1, CostUSD: 0.5, Unpriced: 1})
	if !near(ch.Tokens.CostUSD, 0.75) || !near(ch.Tokens.CostToman, 100) || ch.Tokens.Unpriced != 1 || !near(ch.LastTokens.CostUSD, 0.5) {
		t.Fatalf("chat cost %+v last %+v", ch.Tokens, ch.LastTokens)
	}
}

func TestPricesAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &webServer{client: NewClient("http://x", "k", "glm-5.3-flash")}
	post := func(body string) map[string]any {
		w := httptest.NewRecorder()
		s.handlePrices(w, httptest.NewRequest(http.MethodPost, "/api/prices", bytes.NewBufferString(body)))
		var j map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &j)
		return j
	}
	if j := post(`{"model":"glm-5.3-flash","in":0.3,"out":1.2,"currency":"toman"}`); j["ok"] != true {
		t.Fatalf("save %v", j)
	}
	if j := post(`{"model":"x","in":-1,"out":1}`); j["ok"] == true {
		t.Fatal("negative accepted")
	}
	p, ok := priceFor(loadFileConfig(), "glm-5.3-flash")
	if !ok || p.Currency != "toman" || !near(p.Out, 1.2) {
		t.Fatalf("price %+v %v", p, ok)
	}
	w := httptest.NewRecorder()
	s.handlePrices(w, httptest.NewRequest(http.MethodGet, "/api/prices", nil))
	var g struct {
		Active string     `json:"active"`
		Prices []priceRow `json:"prices"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	if g.Active != "glm-5.3-flash" || len(g.Prices) == 0 || g.Prices[0].Model != "glm-5.3-flash" || !g.Prices[0].Set {
		t.Fatalf("list %+v", g)
	}
	post(`{"model":"glm-5.3-flash","in":0,"out":0}`)
	if _, ok := priceFor(loadFileConfig(), "glm-5.3-flash"); ok {
		t.Fatal("price not removed")
	}
}
