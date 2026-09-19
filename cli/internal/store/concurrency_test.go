package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// Durian writes to one database file from more than one process: the daemon's
// watchers run inside `durian serve` while `durian sync` and the one-shot tag
// commands are separate invocations. SetMaxOpenConns(1) bounds a single
// process to one connection, which is why intra-process contention never shows
// up in tests, but it says nothing across processes.
//
// The store's explicit transactions read before they write —
// ModifyTagsByMessageID resolves the row id with a SELECT and then issues its
// INSERT and DELETE against it. Under a deferred BEGIN both processes take a
// shared read lock, and whichever tries to write second has to upgrade a lock
// the other still holds. SQLite answers that with SQLITE_BUSY immediately: the
// busy handler is deliberately not consulted, because both sides waiting would
// be a deadlock rather than a delay. PRAGMA busy_timeout, however generous,
// cannot help here — it only covers a writer waiting for a lock it has not yet
// taken a conflicting read on.
//
// This is the failure Open's _txlock=immediate setting addresses: an immediate
// transaction takes the writer lock at BEGIN, before its first read, so the
// second process blocks on the busy handler and waits instead of erroring.
func TestConcurrentTagWritesDoNotFailOnLockUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "contention.db")
	kr := testKeyring(t)

	first, err := Open(dbPath, kr)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer first.Close()
	if err := first.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A second handle on the same file is the stand-in for a second process.
	// Two connections through one *sql.DB would be serialized by the pool and
	// would never reproduce this.
	second, err := Open(dbPath, kr)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.Close()

	const rounds = 40
	dbs := []*DB{first, second}

	for round := range rounds {
		messageID := fmt.Sprintf("contended-%d@example.com", round)
		if err := first.InsertMessage(&Message{
			MessageID: messageID, Subject: "subject", Date: 1, CreatedAt: 1,
			Mailbox: "INBOX", Account: "work", RemoteRef: messageID,
		}); err != nil {
			t.Fatalf("round %d seed: %v", round, err)
		}

		// Both handles enter their read-then-write transaction at the same
		// time, each touching a different tag so the outcome is unambiguous:
		// this is about the lock, not about who wins a conflicting edit.
		errs := make([]error, len(dbs))
		var wg sync.WaitGroup
		var start sync.WaitGroup
		start.Add(1)
		for i, db := range dbs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start.Wait()
				errs[i] = db.ModifyTagsByMessageID(messageID,
					[]string{fmt.Sprintf("writer-%d", i)}, nil)
			}()
		}
		start.Done()
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: writer %d failed: %v", round, i, err)
			}
		}

		// Both writes have to survive. A lost update would be the quieter
		// failure: no error, one tag missing.
		tags, err := first.GetTagsByMessageID(messageID)
		if err != nil {
			t.Fatalf("round %d read back: %v", round, err)
		}
		for i := range dbs {
			want := fmt.Sprintf("writer-%d", i)
			if !slices.Contains(tags, want) {
				t.Fatalf("round %d tags=%v, missing %q", round, tags, want)
			}
		}
	}
}

func TestMessageIngestLockSerializesIndependentHandles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ingest-lock.db")
	kr := testKeyring(t)
	first, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	second, err := Open(dbPath, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })

	releaseFirst, err := first.AcquireMessageIngest("work", "same@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	type result struct {
		release func()
		err     error
	}
	started := make(chan struct{})
	acquired := make(chan result, 1)
	go func() {
		close(started)
		release, err := second.AcquireMessageIngest("work", "same@example.com")
		acquired <- result{release: release, err: err}
	}()
	<-started

	select {
	case got := <-acquired:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("second handle acquired the ingest lock before release: %v", got.err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()
	select {
	case got := <-acquired:
		if got.err != nil {
			t.Fatalf("second handle acquire after release: %v", got.err)
		}
		got.release()
	case <-time.After(5 * time.Second):
		t.Fatal("second handle did not acquire the ingest lock after release")
	}
}
