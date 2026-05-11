package handler

import (
	"strings"
	"sync"
	"testing"
)

// --- cleanSnippet ---

func TestCleanSnippet_StripsSignature(t *testing.T) {
	body := "Hello world!\n\n-- \nBest,\nAlice"
	got := cleanSnippet(body, 200)
	if strings.Contains(got, "Best") || strings.Contains(got, "--") {
		t.Errorf("signature not stripped: %q", got)
	}
	if !strings.Contains(got, "Hello world") {
		t.Errorf("body lost: %q", got)
	}
}

func TestCleanSnippet_StripsQuotedLines(t *testing.T) {
	body := "My reply\n> previous line 1\n> previous line 2\nmore reply"
	got := cleanSnippet(body, 200)
	if strings.Contains(got, "previous") {
		t.Errorf("quoted content not stripped: %q", got)
	}
	if !strings.Contains(got, "My reply") || !strings.Contains(got, "more reply") {
		t.Errorf("non-quoted content lost: %q", got)
	}
}

func TestCleanSnippet_CollapsesWhitespace(t *testing.T) {
	body := "Line one\n\n\nLine two\n\nLine three"
	got := cleanSnippet(body, 200)
	if strings.Contains(got, "\n") {
		t.Errorf("newlines not collapsed: %q", got)
	}
	if got != "Line one Line two Line three" {
		t.Errorf("got %q, want %q", got, "Line one Line two Line three")
	}
}

func TestCleanSnippet_TruncatesAtWordBoundary(t *testing.T) {
	body := "The quick brown fox jumps over the lazy dog repeatedly every day"
	got := cleanSnippet(body, 25)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation marker missing: %q", got)
	}
	if len(got) > 30 {
		t.Errorf("result too long: %q (len %d)", got, len(got))
	}
	// Must end at a space boundary, not mid-word
	withoutMarker := strings.TrimSuffix(got, "…")
	if strings.HasSuffix(withoutMarker, "qui") || strings.HasSuffix(withoutMarker, "fo") {
		t.Errorf("truncated mid-word: %q", got)
	}
}

func TestCleanSnippet_ShortInputUnchanged(t *testing.T) {
	body := "Hi there"
	got := cleanSnippet(body, 100)
	if got != "Hi there" {
		t.Errorf("got %q, want %q", got, "Hi there")
	}
}

func TestCleanSnippet_EmptyInput(t *testing.T) {
	if got := cleanSnippet("", 100); got != "" {
		t.Errorf("empty input: got %q, want \"\"", got)
	}
}


func TestNewWatcherManager(t *testing.T) {
	db := newTestStore(t)
	w := NewWatcherManager(nil, db, nil, nil)
	if w == nil {
		t.Fatal("nil watcher manager")
	}
	if w.store != db {
		t.Error("store not stored")
	}
	if w.locks == nil || w.watchers == nil {
		t.Error("maps not initialized")
	}
	if w.log == nil {
		t.Error("logger not set")
	}
}

func TestWatcherManager_AccountLock_SameEmailSameLock(t *testing.T) {
	db := newTestStore(t)
	w := NewWatcherManager(nil, db, nil, nil)

	a := w.accountLock("alice@example.com")
	b := w.accountLock("alice@example.com")
	if a != b {
		t.Error("accountLock returned different locks for same email")
	}
}

func TestWatcherManager_AccountLock_DifferentEmailsDifferentLocks(t *testing.T) {
	db := newTestStore(t)
	w := NewWatcherManager(nil, db, nil, nil)

	a := w.accountLock("alice@example.com")
	b := w.accountLock("bob@example.com")
	if a == b {
		t.Error("accountLock returned same lock for different emails")
	}
}

func TestWatcherManager_AccountLock_ConcurrentSafe(t *testing.T) {
	db := newTestStore(t)
	w := NewWatcherManager(nil, db, nil, nil)

	// Hammer accountLock from multiple goroutines — should not race
	// when run under `bazel test --test_arg=-race` (go_test has race
	// detection on by default in bazel rules_go).
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.accountLock("alice@example.com")
			_ = w.accountLock("bob@example.com")
		}()
	}
	wg.Wait()

	// Should have exactly 2 locks after the storm
	if len(w.locks) != 2 {
		t.Errorf("got %d locks, want 2", len(w.locks))
	}
}

func TestWatcherManager_TriggerSync_UnknownAccount(t *testing.T) {
	db := newTestStore(t)
	w := NewWatcherManager(nil, db, nil, nil)

	// Must not panic when the account has no registered watcher
	w.TriggerSync("unknown-account")
}
