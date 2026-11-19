package main

import (
	"strings"
	"testing"
	"time"
)

func TestShellTimeoutKillsTree(t *testing.T) {
	start := time.Now()
	out := runShellWithTimeout("sleep 30 & sleep 30; echo done", t.TempDir(), 1*time.Second)
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected timeout, got %q", out)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
}

func TestShellTimeoutEnv(t *testing.T) {
	t.Setenv("MUSE99_SHELL_TIMEOUT", "30")
	if shellTimeout() != 30*time.Second {
		t.Fatal("env not applied")
	}
	t.Setenv("MUSE99_SHELL_TIMEOUT", "1")
	if shellTimeout() != 120*time.Second {
		t.Fatal("out-of-range value should fall back")
	}
}
