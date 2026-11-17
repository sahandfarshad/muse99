package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Browser capability (v3), in two layers.
//
// Layer 1 (default, always on, tiny RAM): a plain HTTP fetch with a
// browser User-Agent, redirects, gzip, 20s timeout, then the HTML is
// parsed to clean text (title, headings, main text, links). This
// covers most docs, blogs, GitHub pages and static price pages, and
// costs a few MB of RAM.
//
// Layer 2 (opt-in only): headless Chromium, if it is installed on
// the system. It renders JavaScript pages, then the DOM is parsed
// the same way. It is NOT bundled, NOT auto-installed, and NOT run
// by default — a Chromium page uses ~300-500 MB RAM, which is a lot
// on a 1 GB server. Use it sparingly, only when Layer 1 returns
// nothing useful (e.g. "enable JavaScript").

var tagRe = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
var linkRe = regexp.MustCompile(`(?i)<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
var headingRe = regexp.MustCompile(`(?i)<h[1-3][^>]*>(.*?)</h[1-3]>`)
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
var tagsRe = regexp.MustCompile(`(?s)<[^>]+>`)
var spaceRe = regexp.MustCompile(`[ \t]+`)

// htmlToText extracts a title and clean readable text from HTML.
func htmlToText(raw string) (title, text string) {
	if m := titleRe.FindStringSubmatch(raw); len(m) > 1 {
		title = strings.TrimSpace(html.UnescapeString(stripTags(m[1])))
	}
	s := tagRe.ReplaceAllString(raw, " ")
	// Keep link text with its URL, so the model can follow sources.
	s = linkRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := linkRe.FindStringSubmatch(match)
		if len(parts) < 3 {
			return stripTags(match)
		}
		txt := strings.TrimSpace(stripTags(parts[2]))
		href := strings.TrimSpace(parts[1])
		if txt == "" {
			return ""
		}
		if href != "" && !strings.HasPrefix(href, "#") {
			return txt + " (" + href + ")"
		}
		return txt
	})
	s = headingRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := headingRe.FindStringSubmatch(match)
		if len(parts) < 2 {
			return " "
		}
		return "\n## " + strings.TrimSpace(stripTags(parts[1])) + "\n"
	})
	s = tagsRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// Collapse blank lines and long runs of spaces.
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(spaceRe.ReplaceAllString(line, " "))
		if line != "" {
			lines = append(lines, line)
		}
	}
	text = strings.Join(lines, "\n")
	return title, text
}

func stripTags(s string) string {
	return strings.TrimSpace(tagsRe.ReplaceAllString(s, " "))
}

// fetchPageLayer1 is the default fetch: HTTP + clean text.
func fetchPageLayer1(url string) (title, text string, status int, err error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	var body []byte
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return "", "", resp.StatusCode, err
		}
		defer gz.Close()
		body, err = io.ReadAll(io.LimitReader(gz, 2_000_000))
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, 2_000_000))
	}
	if err != nil {
		return "", "", resp.StatusCode, err
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") && !strings.Contains(ct, "text") && ct != "" {
		return "", truncate(string(body), 12000), resp.StatusCode, nil
	}
	title, text = htmlToText(string(body))
	return title, text, resp.StatusCode, nil
}

// findChromium returns the path of a Chromium/Chrome binary if one is
// installed on this system, or "" if none is present.
func findChromium() string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// fetchPageLayer2 renders the page with headless Chromium and parses
// the resulting DOM the same way as Layer 1.
func fetchPageLayer2(url string) (title, text string, err error) {
	bin := findChromium()
	if bin == "" {
		return "", "", fmt.Errorf("headless Chromium is not installed on this server. " +
			"Install it with: sudo apt update && sudo apt install -y chromium-browser " +
			"(then ask again with JavaScript rendering). " +
			"Note: Chromium uses ~300-500 MB RAM per page, so use it sparingly on a 1 GB server — " +
			"the default fetch (no JavaScript) is the light option.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage",
		"--dump-dom", "--virtual-time-budget=5000", url)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("chromium failed: %v (%s)", err, strings.TrimSpace(errBuf.String()))
	}
	if out.Len() == 0 {
		return "", "", fmt.Errorf("chromium returned an empty page for %s", url)
	}
	title, text = htmlToText(out.String())
	return title, text, nil
}

// browsePage fetches a URL and returns "Title: …\nURL: …\n\n<clean text>".
// useJS selects Layer 2 (Chromium); Layer 1 is the default.
func browsePage(url string, useJS bool) string {
	if useJS {
		title, text, err := fetchPageLayer2(url)
		if err != nil {
			return "error: " + err.Error()
		}
		if strings.TrimSpace(text) == "" {
			return "The page rendered but contained no readable text: " + url
		}
		out := "Title: " + title + "\nURL: " + url + "\n\n" + truncate(text, 12000)
		return out
	}
	title, text, status, err := fetchPageLayer1(url)
	if err != nil {
		return "error: " + err.Error()
	}
	if status >= 400 {
		return fmt.Sprintf("HTTP %d for %s", status, url)
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Sprintf("The page at %s returned no readable text. "+
			"It may need JavaScript — ask again with JavaScript rendering, "+
			"which uses headless Chromium (~300-500 MB RAM).", url)
	}
	out := fmt.Sprintf("Title: %s\nURL: %s\n\n%s", title, url, truncate(text, 12000))
	return out
}
