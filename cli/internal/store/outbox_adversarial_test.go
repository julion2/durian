package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openAdversarialStore(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, testKeyring(t))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if err := db.Init(); err != nil {
		_ = db.Close()
		t.Fatalf("init %s: %v", path, err)
	}
	return db
}

func TestAdversarialUndoRacingClaimHasOneAtomicWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	workerDB := openAdversarialStore(t, path)
	defer workerDB.Close()
	undoDB := openAdversarialStore(t, path)
	defer undoDB.Close()

	// Exercise fresh SQL transactions on distinct connections. The start gate
	// creates the race without timing assumptions or sleeps.
	for iteration := 0; iteration < 24; iteration++ {
		id, err := workerDB.Enqueue(`{"subject":"claim versus undo"}`, 0)
		if err != nil {
			t.Fatalf("iteration %d enqueue: %v", iteration, err)
		}
		start := make(chan struct{})
		claimed := make(chan *OutboxItem, 1)
		claimErr := make(chan error, 1)
		undoErr := make(chan error, 1)
		var ready sync.WaitGroup
		ready.Add(2)
		go func() {
			ready.Done()
			<-start
			item, err := workerDB.ClaimNextOutboxItem()
			claimed <- item
			claimErr <- err
		}()
		go func() {
			ready.Done()
			<-start
			undoErr <- undoDB.DeletePendingOutboxItem(id)
		}()
		ready.Wait()
		close(start)

		item, claimResult := <-claimed, <-claimErr
		undoResult := <-undoErr
		if claimResult != nil {
			t.Fatalf("iteration %d claim error: %v", iteration, claimResult)
		}
		switch {
		case item == nil && undoResult == nil:
			// Undo committed first; there is no payload left to submit.
		case item != nil && item.ID == id && errors.Is(undoResult, ErrOutboxItemInFlight):
			// Claim committed first; Undo reports conflict and must not reopen.
			if err := workerDB.DeleteClaimedOutboxItem(id); err != nil {
				t.Fatalf("iteration %d cleanup claimed row: %v", iteration, err)
			}
		default:
			t.Fatalf("iteration %d non-atomic outcome: claim=%#v undo=%v", iteration, item, undoResult)
		}
		items, err := workerDB.ListOutbox()
		if err != nil || len(items) != 0 {
			t.Fatalf("iteration %d residue after winner handled: %#v, %v", iteration, items, err)
		}
	}
}

func TestAdversarialConcurrentDraftUpdatesRemainWholeAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	firstDB := openAdversarialStore(t, path)
	secondDB := openAdversarialStore(t, path)
	const firstPayload = `{"subject":"first version","body":"AAAAAAAAAAAAAAAA"}`
	const secondPayload = `{"subject":"second version","body":"BBBBBBBBBBBBBBBB"}`

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, update := range []struct {
		db      *DB
		payload string
	}{{firstDB, firstPayload}, {secondDB, secondPayload}} {
		update := update
		go func() {
			ready.Done()
			<-start
			errs <- update.db.SaveLocalDraft(&LocalDraft{ID: "reply-draft", DraftJSON: update.payload})
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent draft update: %v", err)
		}
	}
	if err := firstDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secondDB.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openAdversarialStore(t, path)
	defer reopened.Close()
	draft, err := reopened.GetLocalDraft("reply-draft")
	if err != nil {
		t.Fatalf("reopen concurrent draft: %v", err)
	}
	if draft.DraftJSON != firstPayload && draft.DraftJSON != secondPayload {
		t.Fatalf("concurrent update stored torn/lost payload %q", draft.DraftJSON)
	}
	drafts, err := reopened.ListLocalDrafts()
	if err != nil || len(drafts) != 1 {
		t.Fatalf("concurrent update rows = %#v, %v", drafts, err)
	}
}

func TestAdversarialOutboxLifecycleLockRejectsSymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "mail.db")
	if err := os.WriteFile(realPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(dir, "mail-alias.db")
	if err := os.Symlink(filepath.Base(realPath), aliasPath); err != nil {
		t.Fatal(err)
	}

	release, err := AcquireOutboxLifecycle(realPath)
	if err != nil {
		t.Fatalf("first lifecycle owner: %v", err)
	}
	defer release()
	aliasRelease, err := AcquireOutboxLifecycle(aliasPath)
	if aliasRelease != nil {
		defer aliasRelease()
	}
	if !errors.Is(err, ErrOutboxLifecycleLocked) {
		t.Fatalf("same SQLite store acquired through symlink alias: err=%v; daemon delivery and direct reconciliation can overlap", err)
	}
}
