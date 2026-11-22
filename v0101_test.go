package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV0101SearchParsers(t *testing.T) {
	ddg := `<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Flmstudio.ai%2Fdocs&amp;rut=x">LM <b>Studio</b> docs</a>
<a class="result__snippet" href="x">Run a <b>local</b> server &amp; more</a>`
	rs := parseDDG(ddg)
	if len(rs) != 1 || rs[0].URL != "https://lmstudio.ai/docs" || rs[0].Title != "LM Studio docs" || rs[0].Snippet != "Run a local server & more" {
		t.Fatalf("ddg: %+v", rs)
	}
	bing := `<li class="b_algo"><h2><a href="https://www.bing.com/ck/a?!&amp;&amp;p=1&amp;u=a1aHR0cHM6Ly9sbXN0dWRpby5haS8&amp;ntb=1">LM Studio</a></h2><p>Local <strong>LLMs</strong></p></li>`
	rs = parseBing(bing)
	if len(rs) != 1 || rs[0].URL != "https://lmstudio.ai/" || rs[0].Snippet != "Local LLMs" {
		t.Fatalf("bing: %+v", rs)
	}
	out := formatResults("q", "Bing", append(rs, rs[0]), 8)
	if strings.Count(out, "https://lmstudio.ai/") != 1 || !strings.Contains(out, "fetch_url") {
		t.Fatalf("format: %s", out)
	}
}

func TestV0101Workdir(t *testing.T) {
	d := t.TempDir()
	if got, msg := cleanWorkdir(` "` + d + `" `); msg != "" || got != d {
		t.Fatalf("clean: %q %q", got, msg)
	}
	if _, msg := cleanWorkdir(filepath.Join(d, "nope")); msg == "" {
		t.Fatal("missing folder accepted")
	}
	f := filepath.Join(d, "f.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, msg := cleanWorkdir(f); msg == "" {
		t.Fatal("file accepted")
	}
	r := &toolRunner{workdir: t.TempDir()}
	r.setWorkdir(d)
	if !strings.Contains(r.run("list_files", `{"path":""}`), "f.txt") {
		t.Fatal("list after setWorkdir")
	}
}

func TestV0102Confine(t *testing.T) {
	d := t.TempDir()
	out := t.TempDir()
	_ = os.WriteFile(filepath.Join(out, "secret.txt"), []byte("s"), 0o644)
	_ = os.Symlink(out, filepath.Join(d, "link"))
	r := &toolRunner{workdir: d, confine: true}
	for _, p := range []string{filepath.Join(out, "secret.txt"), "../" + filepath.Base(out) + "/secret.txt", "link/secret.txt"} {
		if res := r.run("read_file", `{"path":`+strconvQuote(p)+`}`); !strings.Contains(res, "outside the work folder") {
			t.Fatalf("%s escaped: %s", p, res)
		}
	}
	if res := r.run("write_file", `{"path":"sub/new.txt","content":"hi"}`); strings.HasPrefix(res, "error") {
		t.Fatalf("inside write: %s", res)
	}
	if res := r.run("write_file", `{"path":`+strconvQuote(filepath.Join(d, "abs.txt"))+`,"content":"hi"}`); strings.HasPrefix(res, "error") {
		t.Fatalf("absolute inside: %s", res)
	}
	if res := r.run("list_files", `{"path":"/"}`); !strings.Contains(res, "outside") {
		t.Fatalf("root listed: %s", res)
	}
	open := &toolRunner{workdir: d}
	if res := open.run("read_file", `{"path":`+strconvQuote(filepath.Join(out, "secret.txt"))+`}`); res != "s" {
		t.Fatalf("server mode unchanged: %s", res)
	}
}
