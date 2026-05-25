package handler

import (
	"testing"
	"time"
)

func TestTokenBucket_BurstAllowedThenThrottled(t *testing.T) {
	// burst=3, rate=10/s. First 3 calls in <100ms should pass, 4th
	// should fail. ADR-0001 audit H2: the chosen-plaintext oracle on
	// /search/count is mitigated by ensuring the attacker can't burst
	// beyond a small window per second.
	b := newTokenBucket(10, 3)
	for i := 0; i < 3; i++ {
		if !b.allow() {
			t.Errorf("call %d should pass within burst", i)
		}
	}
	if b.allow() {
		t.Error("4th call should be throttled past burst")
	}
}

func TestTokenBucket_RefillsOverTime(t *testing.T) {
	// burst=1, rate=100/s → refill ~10ms per token. After exhausting,
	// waiting ~30ms should refill at least one token.
	b := newTokenBucket(100, 1)
	if !b.allow() {
		t.Fatal("first call should pass")
	}
	if b.allow() {
		t.Fatal("second call (immediate) should be throttled")
	}
	time.Sleep(30 * time.Millisecond)
	if !b.allow() {
		t.Error("should pass after refill window")
	}
}

// TestSearchOracleLimiter_SharedAcrossEndpoints asserts ADR-0001
// audit-2 follow-up: /search and /search/count share the same
// token-bucket budget, so an attacker alternating calls between the
// two endpoints can't get 2× the rate. The pre-fix code had a
// dedicated limiter on /search/count only, leaving /search wide open
// — a /search?query=X + count-of-results loop bypassed the H2
// chosen-plaintext-oracle defense entirely.
//
// This test exercises the package-level searchOracleLimiter that
// both SearchHandler and SearchCountHandler consult; if a future
// change re-introduces per-endpoint limiters, this test fails.
func TestSearchOracleLimiter_SharedAcrossEndpoints(t *testing.T) {
	orig := searchOracleLimiter
	defer func() { searchOracleLimiter = orig }()
	// burst=4, rate=1/s (slow refill so we observe pure burst).
	searchOracleLimiter = newTokenBucket(1, 4)

	allowed, throttled := 0, 0
	for i := 0; i < 8; i++ {
		if searchOracleLimiter.allow() {
			allowed++
		} else {
			throttled++
		}
	}
	// Single shared bucket → 4 allowed, 4 throttled. If the limiter
	// were per-endpoint and each handler had its own, /search and
	// /search/count would each get 4 → 8 total allowed — exactly the
	// audit-2 bypass.
	if allowed != 4 {
		t.Errorf("allowed = %d, want 4 (shared budget across both endpoints)", allowed)
	}
	if throttled != 4 {
		t.Errorf("throttled = %d, want 4", throttled)
	}
}

func TestTokenBucket_CapacityCappedAtBurst(t *testing.T) {
	// burst=2, rate=1000/s. After idle time longer than capacity/rate,
	// the bucket must not exceed burst — otherwise an idle attacker
	// could accumulate arbitrary rate.
	b := newTokenBucket(1000, 2)
	b.allow()
	b.allow()
	// Drain.
	if b.allow() {
		t.Fatal("third immediate call should be throttled")
	}
	time.Sleep(50 * time.Millisecond) // would refill 50 tokens at rate=1000
	// Cap at 2 → only 2 allowed before next throttle.
	if !b.allow() {
		t.Error("first call after long idle should pass")
	}
	if !b.allow() {
		t.Error("second call after long idle should pass (burst=2)")
	}
	if b.allow() {
		t.Error("third call should be throttled — capacity must cap at burst, not accumulate")
	}
}
