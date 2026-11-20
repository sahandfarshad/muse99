package main

// STOP / PAUSE / PLAY (v0.9.9). One control for all running work (the
// chat turn and background tasks):
//   - Stop cancels the model call in flight, kills a running command
//     (local or on a saved server) and ends the turn. Steps done so far
//     stay in the chat; "continue" picks up from there.
//   - Pause lets the current step finish, then waits before the next
//     one until Play (or Stop) is pressed.
// The web handlers here never take webServer.mu, which a chat turn
// holds for its whole length.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errStopped = errors.New("stopped by you")

type runControl struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	paused bool
	resume chan struct{}
	// stopping: Stop was pressed and no new work began since.
	stopping bool
}

var ctl = newRunControl()

func newRunControl() *runControl {
	c := &runControl{resume: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c
}

// context is the current stop context. After a Stop, the next call
// starts a fresh one, so new work runs normally.
func (c *runControl) context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil && !c.stopping {
		c.ctx, c.cancel = context.WithCancel(context.Background())
	}
	return c.ctx
}

// stop cancels everything running now and clears a pause.
func (c *runControl) stop() {
	c.mu.Lock()
	c.cancel()
	c.stopping = true
	if c.paused {
		c.paused = false
		close(c.resume)
		c.resume = make(chan struct{})
	}
	c.mu.Unlock()
}

// begin is called when new work starts (a chat message, a task): a
// Stop pressed earlier no longer applies to it.
func (c *runControl) begin() {
	c.mu.Lock()
	c.stopping = false
	c.mu.Unlock()
	c.setPaused(false)
}

func (c *runControl) setPaused(p bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p == c.paused {
		return
	}
	c.paused = p
	if !p {
		close(c.resume)
		c.resume = make(chan struct{})
	}
}

func (c *runControl) state() (paused, stopped bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused, c.stopping && c.ctx.Err() != nil
}

// checkpoint runs between steps: it waits while paused and reports a
// Stop as errStopped.
func (c *runControl) checkpoint(src string) error {
	ctx := c.context()
	if ctx.Err() != nil {
		return errStopped
	}
	c.mu.Lock()
	paused, ch := c.paused, c.resume
	c.mu.Unlock()
	if !paused {
		return nil
	}
	id := activity.add(src, "retry", "Paused — press Play to go on, or Stop", "The step before this one finished. Nothing runs until you press Play.", true)
	select {
	case <-ch:
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		activity.finish(id, false, "Stopped while paused", "")
		return errStopped
	}
	activity.finish(id, true, "Resumed", "")
	return nil
}

// sleepCtx sleeps d unless Stop is pressed first.
func sleepCtx(d time.Duration) error {
	ctx := ctl.context()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return errStopped
	}
}

// handleControl serves GET /api/control (state) and POST /api/control
// {action: stop|pause|play}.
func (s *webServer) handleControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		var body struct {
			Action string `json:"action"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Action {
		case "stop":
			ctl.stop()
			activity.add("", "error", "Stop pressed", "The model call and any running command were cancelled.", false)
		case "pause":
			ctl.setPaused(true)
			activity.add("", "retry", "Pause pressed", "Muse99 stops after the step it is on.", false)
		case "play":
			ctl.setPaused(false)
		default:
			http.Error(w, "action must be stop, pause or play", http.StatusBadRequest)
			return
		}
	}
	p, st := ctl.state()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "paused": p, "stopped": st})
}
