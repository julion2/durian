package handler

import (
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
)

type recordingSyncTrigger struct{ accounts []string }

func (r *recordingSyncTrigger) TriggerSync(account string) { r.accounts = append(r.accounts, account) }

func TestSyncTriggerGroupFansOut(t *testing.T) {
	first, second := &recordingSyncTrigger{}, &recordingSyncTrigger{}
	SyncTriggerGroup{first, nil, second}.TriggerSync("work")
	if len(first.accounts) != 1 || first.accounts[0] != "work" || len(second.accounts) != 1 || second.accounts[0] != "work" {
		t.Fatalf("fan-out = %v / %v", first.accounts, second.accounts)
	}
}

func TestEngineWatcherRegistersTriggersSynchronously(t *testing.T) {
	w := NewEngineWatcher(NewEventHub(), nil, nil, nil, nil)
	account := &config.AccountConfig{Name: "JMAP", Alias: "jmap"}
	w.RegisterAccounts([]*config.AccountConfig{account})
	w.TriggerSync("jmap")
	w.mu.Lock()
	queued := len(w.triggers["jmap"])
	w.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued triggers = %d, want 1 before Start", queued)
	}
	w.TriggerSync("jmap")
	w.mu.Lock()
	coalesced := len(w.triggers["jmap"])
	w.mu.Unlock()
	if coalesced != 1 {
		t.Fatalf("coalesced triggers = %d, want 1", coalesced)
	}
}

// TestEngineWatcherIntervalTiers proves the cadence policy: fast on the inbox
// while a GUI is attached, slower everywhere else, and slower again once the
// GUI disconnects. Getting this backwards is invisible in production until a
// user notices mail is late, so it is pinned here.
func TestEngineWatcherIntervalTiers(t *testing.T) {
	hub := NewEventHub()
	w := NewEngineWatcher(hub, nil, nil, nil, nil)

	// No SSE client connected: idle tiers.
	assertInterval(t, "idle inbox", w.nextInterval(true, 0), inboxIntervalIdle)
	assertInterval(t, "idle full", w.nextInterval(false, 0), fullIntervalIdle)

	// A connected SSE client means the GUI is up and the user can see arrivals.
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	if !hub.HasClients() {
		t.Fatal("HasClients() = false after Subscribe()")
	}
	assertInterval(t, "active inbox", w.nextInterval(true, 0), inboxIntervalActive)
	assertInterval(t, "active full", w.nextInterval(false, 0), fullIntervalActive)

	if inboxIntervalActive >= inboxIntervalIdle {
		t.Error("active inbox cadence must be faster than the idle one")
	}
}

// TestEngineWatcherBackoff proves consecutive failures back off exponentially
// and stop at the cap, so an account that is broken (revoked token, tenant
// offline) cannot hammer the API for as long as the daemon runs.
func TestEngineWatcherBackoff(t *testing.T) {
	hub := NewEventHub()
	ch := hub.Subscribe()
	defer hub.Unsubscribe(ch)
	w := NewEngineWatcher(hub, nil, nil, nil, nil)

	assertInterval(t, "1 failure", w.nextInterval(true, 1), 2*inboxIntervalActive)
	assertInterval(t, "3 failures", w.nextInterval(true, 3), 8*inboxIntervalActive)

	// Far past the cap, and still capped.
	for _, failures := range []int{10, 50} {
		got := w.nextInterval(true, failures)
		if got > time.Duration(float64(maxProbeBackoff)*(1+probeJitter)) {
			t.Errorf("nextInterval(%d failures) = %v, want <= %v (+jitter)", failures, got, maxProbeBackoff)
		}
	}
}

// assertInterval checks that got is want ± the configured jitter. The jitter
// itself matters: without it, several accounts would converge on one instant.
func assertInterval(t *testing.T, name string, got, want time.Duration) {
	t.Helper()
	lo := time.Duration(float64(want) * (1 - probeJitter))
	hi := time.Duration(float64(want) * (1 + probeJitter))
	if got < lo || got > hi {
		t.Errorf("%s: interval = %v, want %v ± %.0f%%", name, got, want, probeJitter*100)
	}
}

// TestEventHubHasClients proves the "is a GUI attached?" signal tracks
// subscribe/unsubscribe, since the whole cadence policy hangs off it.
func TestEventHubHasClients(t *testing.T) {
	hub := NewEventHub()
	if hub.HasClients() {
		t.Error("HasClients() = true on a fresh hub")
	}
	a := hub.Subscribe()
	b := hub.Subscribe()
	if !hub.HasClients() {
		t.Error("HasClients() = false with two subscribers")
	}
	hub.Unsubscribe(a)
	if !hub.HasClients() {
		t.Error("HasClients() = false while one subscriber remains")
	}
	hub.Unsubscribe(b)
	if hub.HasClients() {
		t.Error("HasClients() = true after the last unsubscribe")
	}
}
