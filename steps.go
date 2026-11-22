package main

// v0.10.3: one chat message may take more steps (default 60, set in
// Settings or MUSE_CHAT_STEPS), and when the limit is reached the model
// writes a short report of what is done and what is left instead of a
// bare "stopped" line. Sending "continue" goes on from there.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const defaultChatSteps = 60

var chatSteps atomic.Int64

func clampSteps(n int) int {
	if n < 5 {
		return 5
	}
	if n > 300 {
		return 300
	}
	return n
}

func setChatStepLimit(n int) {
	if n <= 0 {
		chatSteps.Store(0)
		return
	}
	chatSteps.Store(int64(clampSteps(n)))
}

func chatStepLimit() int {
	if n := chatSteps.Load(); n > 0 {
		return int(n)
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("MUSE_CHAT_STEPS"))); err == nil && v > 0 {
		return clampSteps(v)
	}
	return defaultChatSteps
}

// stepLimitReply asks the model, with no tools, for a short progress
// report. Every tool call is already answered, so the history is valid.
func stepLimitReply(client *Client, messages *[]Message, limit int) string {
	head := fmt.Sprintf("⏸ I used all %d steps for one message, so I paused here. Send \"continue\" and I'll go on from where I stopped (or raise \"Steps per message\" in Settings).", limit)
	ask := append(append([]Message{}, *messages...), Message{Role: "user", Content: "[Step limit reached. Do not call tools. In a few short lines: what you have done so far (files written), what is left, and the next step. Answer in the user's language.]"})
	msg, err := client.Chat(ask, nil)
	if err != nil || strings.TrimSpace(msg.Content) == "" || len(msg.ToolCalls) > 0 {
		return head
	}
	*messages = append(*messages, Message{Role: "assistant", Content: msg.Content})
	return head + "\n\n" + strings.TrimSpace(msg.Content)
}
