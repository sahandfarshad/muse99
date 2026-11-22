package main

// v0.10.1: web_search returns clean results (title, link, snippet)
// instead of raw HTML, so small local models can use them. It tries
// DuckDuckGo first and falls back to Bing when DuckDuckGo blocks or
// returns nothing. Optional: MUSE_BRAVE_KEY (Brave Search API) is used
// first when set.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type searchResult struct {
	Title, URL, Snippet string
}

const searchUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

var (
	reTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace    = regexp.MustCompile(`\s+`)
	reDDGLink  = regexp.MustCompile(`(?s)<a[^>]*class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reDDGSnip  = regexp.MustCompile(`(?s)class="result__snippet"[^>]*>(.*?)</a>`)
	reBingItem = regexp.MustCompile(`(?s)<li class="b_algo"(.*?)</li>`)
	reBingA    = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reBingP    = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
)

func cleanText(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTag.ReplaceAllString(s, " ")), " "))
}

func searchGet(method, u, form string) (string, int, error) {
	var req *http.Request
	var err error
	if method == "POST" {
		req, err = http.NewRequest("POST", u, strings.NewReader(form))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequest("GET", u, nil)
	}
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", searchUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode, nil
}

// ddgRealURL unwraps //duckduckgo.com/l/?uddg=<url>.
func ddgRealURL(h string) string {
	h = html.UnescapeString(h)
	if strings.HasPrefix(h, "//") {
		h = "https:" + h
	}
	if u, err := url.Parse(h); err == nil && strings.Contains(u.Host, "duckduckgo.com") {
		if v := u.Query().Get("uddg"); v != "" {
			return v
		}
	}
	return h
}

func parseDDG(body string) []searchResult {
	links := reDDGLink.FindAllStringSubmatch(body, -1)
	snips := reDDGSnip.FindAllStringSubmatch(body, -1)
	var out []searchResult
	for i, m := range links {
		r := searchResult{Title: cleanText(m[2]), URL: ddgRealURL(m[1])}
		if strings.Contains(r.URL, "duckduckgo.com/y.js") { // ad
			continue
		}
		if i < len(snips) {
			r.Snippet = cleanText(snips[i][1])
		}
		out = append(out, r)
	}
	return out
}

// bingRealURL unwraps bing.com/ck/a?...&u=a1<base64url>.
func bingRealURL(h string) string {
	h = html.UnescapeString(h)
	u, err := url.Parse(h)
	if err != nil || !strings.Contains(u.Host, "bing.com") {
		return h
	}
	v := u.Query().Get("u")
	if len(v) > 2 && strings.HasPrefix(v, "a1") {
		enc := v[2:]
		if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(enc, "=")); err == nil {
			return string(b)
		}
	}
	return h
}

func parseBing(body string) []searchResult {
	var out []searchResult
	for _, m := range reBingItem.FindAllStringSubmatch(body, -1) {
		a := reBingA.FindStringSubmatch(m[1])
		if a == nil {
			continue
		}
		r := searchResult{Title: cleanText(a[2]), URL: bingRealURL(a[1])}
		if p := reBingP.FindStringSubmatch(m[1]); p != nil {
			r.Snippet = cleanText(p[1])
		}
		out = append(out, r)
	}
	return out
}

func braveSearch(q, key string) ([]searchResult, error) {
	req, _ := http.NewRequest("GET", "https://api.search.brave.com/res/v1/web/search?count=8&q="+url.QueryEscape(q), nil)
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("brave: HTTP %d", resp.StatusCode)
	}
	var j struct {
		Web struct {
			Results []struct {
				Title, URL, Description string
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, err
	}
	var out []searchResult
	for _, r := range j.Web.Results {
		out = append(out, searchResult{cleanText(r.Title), r.URL, cleanText(r.Description)})
	}
	return out, nil
}

func formatResults(q, engine string, rs []searchResult, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q (%s):\n", q, engine)
	seen := map[string]bool{}
	n := 0
	for _, r := range rs {
		if r.URL == "" || seen[r.URL] || !strings.HasPrefix(r.URL, "http") {
			continue
		}
		seen[r.URL] = true
		n++
		fmt.Fprintf(&b, "%d. %s\n   %s\n", n, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", truncate(r.Snippet, 300))
		}
		if n >= max {
			break
		}
	}
	b.WriteString("Use fetch_url on a link to read the page.")
	return b.String()
}

// webSearch runs a search and returns up to 8 results as plain text.
func webSearch(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "error: empty query"
	}
	var errs []string
	if key := strings.TrimSpace(os.Getenv("MUSE_BRAVE_KEY")); key != "" {
		if rs, err := braveSearch(q, key); err == nil && len(rs) > 0 {
			return formatResults(q, "Brave", rs, 8)
		} else if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if body, code, err := searchGet("POST", "https://html.duckduckgo.com/html/", "q="+url.QueryEscape(q)); err != nil {
		errs = append(errs, "duckduckgo: "+err.Error())
	} else if rs := parseDDG(body); code == 200 && len(rs) > 0 {
		return formatResults(q, "DuckDuckGo", rs, 8)
	} else {
		errs = append(errs, fmt.Sprintf("duckduckgo: HTTP %d, %d results", code, len(rs)))
	}
	if body, code, err := searchGet("GET", "https://www.bing.com/search?setlang=en&q="+url.QueryEscape(q), ""); err != nil {
		errs = append(errs, "bing: "+err.Error())
	} else if rs := parseBing(body); len(rs) > 0 {
		return formatResults(q, "Bing", rs, 8)
	} else {
		errs = append(errs, fmt.Sprintf("bing: HTTP %d, no results", code))
	}
	return "error: web search failed (" + strings.Join(errs, "; ") + "). Is this computer online?"
}
