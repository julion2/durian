package handler

import (
	"sync"
	"time"
)

// tokenBucket is a tiny per-process token-bucket rate limiter used to
// throttle endpoints that double as a side-channel oracle. Fills at
// rate tokens/sec up to capacity; allow() consumes one token or
// returns false. Concurrency-safe; no external dep on
// golang.org/x/time/rate to keep the bazel module graph minimal.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newTokenBucket(ratePerSec, burst float64) *tokenBucket {
	return &tokenBucket{
		tokens:     burst,
		capacity:   burst,
		refillRate: ratePerSec,
		lastRefill: time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillRate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.lastRefill = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// searchOracleLimiter throttles both /api/v1/search AND /api/v1/search/count
// under a single shared budget.
//
// ADR-0001 audit H2 originally throttled only /search/count. The audit-2
// follow-up found that /search is the same oracle: an attacker who can
// email crafted content into the user's mailbox can call
// /search?query=X and count the response items themselves, bypassing
// the count-endpoint limiter entirely. A single shared bucket
// guarantees neither path can be raced past the limit by alternating
// calls between the two endpoints.
//
// The post-decrypt filter (search_filter.go) eliminates
// HMAC-collision false-positives, but the true-positive transition
// (0→1) still leaks "the user received my chosen mail" via either
// endpoint. 10 req/s with a burst of 30 is well above legitimate
// GUI use (search-as-you-type is debounced client-side) and well
// below the rate needed to mount statistical analysis over thousands
// of probes.
var searchOracleLimiter = newTokenBucket(10, 30)
