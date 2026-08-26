package handler

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

type watchBackend struct {
	watch func(context.Context, func()) error
}

func (b *watchBackend) FetchFolders(context.Context) ([]backend.Folder, error) { return nil, nil }
func (b *watchBackend) FetchMessages(context.Context, string, backend.Cursor, int) (backend.FetchResult, error) {
	return backend.FetchResult{}, nil
}
func (b *watchBackend) FetchBody(context.Context, backend.RemoteRef, io.Writer) error { return nil }
func (b *watchBackend) ApplyFlags(context.Context, backend.RemoteRef, backend.Flags, backend.Flags) error {
	return nil
}
func (b *watchBackend) FetchFlags(context.Context, string, []backend.RemoteRef) (map[string]backend.Flags, error) {
	return nil, nil
}
func (b *watchBackend) Move(context.Context, backend.RemoteRef, string) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, nil
}
func (b *watchBackend) Append(context.Context, string, backend.Flags, []byte) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, nil
}
func (b *watchBackend) Send(context.Context, []byte) error { return nil }
func (b *watchBackend) Watch(ctx context.Context, _ string, onChange func()) error {
	return b.watch(ctx, onChange)
}
func (b *watchBackend) Capabilities() backend.Capabilities {
	return backend.Capabilities{PushWatch: true}
}
func (b *watchBackend) Close() error { return nil }

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

func TestEngineWatcherReconnectsStoppedPushBackend(t *testing.T) {
	w := NewEngineWatcher(NewEventHub(), nil, nil, nil, nil)
	account := &config.AccountConfig{Name: "IMAP", Alias: "imap"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	created := make(chan int, 2)
	count := 0
	factory := func(*config.AccountConfig) (backend.Backend, error) {
		count++
		created <- count
		if count == 1 {
			return &watchBackend{watch: func(context.Context, func()) error {
				return errors.New("IDLE connection dropped")
			}}, nil
		}
		return &watchBackend{watch: func(ctx context.Context, _ func()) error {
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	done := make(chan struct{})
	go func() {
		w.pushLoopWith(ctx, account, 0, factory, func(context.Context, *config.AccountConfig, bool) error { return nil })
		close(done)
	}()
	for want := 1; want <= 2; want++ {
		select {
		case got := <-created:
			if got != want {
				t.Fatalf("created backend %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for backend creation %d", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("push loop did not stop after cancellation")
	}
}

func TestWatchedFoldersUsesAllMailForLabelBackends(t *testing.T) {
	if got := watchedFolders(true, backend.Capabilities{LabelsAreTags: true}); got != nil {
		t.Fatalf("label-backend fast-pass folders = %v, want account-wide stream", got)
	}
	if got := watchedFolders(true, backend.Capabilities{}); len(got) != 1 || got[0] != "INBOX" {
		t.Fatalf("folder-backend fast-pass folders = %v, want INBOX", got)
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
