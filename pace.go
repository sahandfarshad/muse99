package main

// Pacing (v0.9.5): wait a set number of seconds between the end of one
// model call and the start of the next, so free tiers with a
// requests-per-minute limit (OpenRouter :free models) are not hit.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	paceMu      sync.Mutex
	lastCallEnd time.Time
)

// callPace is the pause between model calls: MUSE99_CALL_DELAY
// (seconds) wins, else the saved setting.
func callPace() time.Duration {
	if v := strings.TrimSpace(os.Getenv("MUSE99_CALL_DELAY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 300 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(loadFileConfig().CallDelaySec) * time.Second
}

// paceWait sleeps until the pause since the last call has passed.
func paceWait() error {
	d := callPace()
	if d <= 0 {
		return nil
	}
	paceMu.Lock()
	wait := time.Until(lastCallEnd.Add(d))
	paceMu.Unlock()
	if wait <= 0 {
		return nil
	}
	id := activity.add("", "retry", fmt.Sprintf("Pacing: waiting %s before the next model call", wait.Round(100*time.Millisecond)), "Set in the Pace menu at the top of the chat.", true)
	if sleepCtx(wait) != nil {
		activity.finish(id, false, "Stopped", "")
		return errStopped
	}
	activity.finish(id, true, "Paced "+wait.Round(100*time.Millisecond).String(), "")
	return nil
}

func paceDone() {
	paceMu.Lock()
	lastCallEnd = time.Now()
	paceMu.Unlock()
}

// handlePace serves POST /api/pace {seconds}.
func (s *webServer) handlePace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		_ = json.NewEncoder(w).Encode(map[string]any{"seconds": int(callPace() / time.Second)})
		return
	}
	var body struct {
		Seconds int `json:"seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Seconds < 0 || body.Seconds > 300 {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "0 to 300 seconds"})
		return
	}
	fc := loadFileConfig()
	fc.CallDelaySec = body.Seconds
	if err := saveFileConfig(fc); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "seconds": body.Seconds})
}
