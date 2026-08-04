package graphcalendar

import (
	"net/http"
	"testing"
	"time"
)

// respWithRetryAfter builds a bare response carrying just the header.
func respWithRetryAfter(value string) *http.Response {
	h := http.Header{}
	if value != "" {
		h.Set("Retry-After", value)
	}
	return &http.Response{Header: h}
}

func TestRetryAfterParsesBothForms(t *testing.T) {
	if d, ok := retryAfter(respWithRetryAfter("30")); !ok || d != 30*time.Second {
		t.Errorf("delay-seconds form = (%v, %v), want (30s, true)", d, ok)
	}

	// The HTTP-date form is the one the old numeric-only parser silently fell
	// back to a one-second wait for — retrying far too early and earning
	// another throttle.
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	d, ok := retryAfter(respWithRetryAfter(future))
	if !ok {
		t.Fatal("HTTP-date form was not recognized")
	}
	if d < 80*time.Second || d > 90*time.Second {
		t.Errorf("HTTP-date delay = %v, want roughly 90s", d)
	}

	// A date that has already passed means "you may retry now", not "no header".
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	if d, ok := retryAfter(respWithRetryAfter(past)); !ok || d != 0 {
		t.Errorf("elapsed date = (%v, %v), want (0, true)", d, ok)
	}
}

func TestRetryAfterReportsAbsentAndUnusableHeaders(t *testing.T) {
	for _, value := range []string{"", "soon", "-5"} {
		if d, ok := retryAfter(respWithRetryAfter(value)); ok {
			t.Errorf("Retry-After %q = (%v, true), want no usable delay", value, d)
		}
	}
}

func TestThrottleDelayPrefersTheServersHeader(t *testing.T) {
	if d := throttleDelay(respWithRetryAfter("7"), 3); d != 7*time.Second {
		t.Errorf("delay = %v, want the server's 7s rather than a computed backoff", d)
	}
}

func TestBackoffGrowsAndStaysCapped(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 4; attempt++ {
		d := backoffWithJitter(attempt)
		if d <= prev {
			t.Errorf("attempt %d = %v, want more than the previous %v", attempt, d, prev)
		}
		prev = d
	}
	if d := backoffWithJitter(20); d > maxThrottleBackoff+jitterSpan {
		t.Errorf("attempt 20 = %v, want it capped at %v (+jitter)", d, maxThrottleBackoff)
	}
}

// Without jitter every calendar of an account retries at the identical
// instant and reproduces the burst that caused the throttle.
func TestBackoffIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 50 {
		seen[backoffWithJitter(2)] = true
	}
	if len(seen) < 2 {
		t.Error("backoff produced a single fixed value; concurrent retries would collide")
	}
	for d := range seen {
		base := 4 * time.Second
		if d < base || d >= base+jitterSpan {
			t.Errorf("backoff %v outside [%v, %v)", d, base, base+jitterSpan)
		}
	}
}
