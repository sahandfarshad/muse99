package main

// v0.10.4: the reply budget (max tokens one model reply may use,
// thinking included) is set in Settings > Reply budget, saved as
// reply_tokens in config.json. 0 = MUSE99_MAX_TOKENS or 8192.
// When a thinking model spends the whole budget thinking and writes
// nothing, the call is tried once more with a bigger budget.

import "sync/atomic"

const (
	defaultReplyTokens = 8192
	lengthRetryTokens  = 32768
	maxReplyTokens     = 200000
)

var replyTokens atomic.Int64

func clampReplyTokens(n int) int {
	if n <= 0 {
		return 0
	}
	if n < 1024 {
		return 1024
	}
	if n > maxReplyTokens {
		return maxReplyTokens
	}
	return n
}

func setReplyTokens(n int) { replyTokens.Store(int64(clampReplyTokens(n))) }

// biggerBudget is the budget for the retry after a reply ran out while
// thinking, or 0 when there is no bigger one worth trying.
func biggerBudget(cur int) int {
	if cur <= 0 {
		return 0
	}
	next := cur * 4
	if next < lengthRetryTokens {
		next = lengthRetryTokens
	}
	if next > 65536 {
		next = 65536
	}
	if next <= cur {
		return 0
	}
	return next
}
