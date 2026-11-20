package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStopKillsShell(t *testing.T) {
	defer ctl.begin()
	go func() { time.Sleep(300 * time.Millisecond); ctl.stop() }()
	start := time.Now()
	out := runShellWithTimeout("sleep 30", t.TempDir(), time.Minute)
	if time.Since(start) > 10*time.Second || !strings.Contains(out, "stopped by the user") {
		t.Fatalf("shell not stopped: %q after %s", out, time.Since(start))
	}
	ctl.begin()
	if out := runShellWithTimeout("echo ok", t.TempDir(), time.Minute); !strings.Contains(out, "ok") {
		t.Fatalf("after begin: %q", out)
	}
}

func TestStopCancelsModelCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer ctl.begin()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", "m")
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	msgs := []Message{{Role: "system", Content: "s"}}
	go func() { time.Sleep(300 * time.Millisecond); ctl.stop() }()
	start := time.Now()
	reply, _ := runTurnFallback(client, nil, runner, nil, &msgs, "hi")
	if time.Since(start) > 10*time.Second || !strings.Contains(reply, "Stopped by you") {
		t.Fatalf("reply %q after %s", reply, time.Since(start))
	}
}

func TestPauseWaitsForPlay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer ctl.begin()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "done"}}}})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, "k", "m")
	runner := &toolRunner{workdir: t.TempDir(), memoryPath: t.TempDir() + "/MEMORY.md"}
	msgs := []Message{{Role: "system", Content: "s"}}
	ctl.setPaused(true)
	done := make(chan string, 1)
	go func() { r, _ := runTurnFallback(client, nil, runner, nil, &msgs, "hi"); done <- r }()
	time.Sleep(400 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("ran while paused")
	}
	s := &webServer{}
	rec := httptest.NewRecorder()
	s.handleControl(rec, httptest.NewRequest("POST", "/api/control", strings.NewReader(`{"action":"play"}`)))
	select {
	case r := <-done:
		if r != "done" {
			t.Fatalf("reply %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("play did not resume")
	}
}
