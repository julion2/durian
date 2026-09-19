package syncengine

import (
	"errors"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
)

type failCommitCursorStore struct {
	store  *memCursorStore
	failAt int
	calls  int
}

func (s *failCommitCursorStore) GetState(account, folder string) (FolderState, error) {
	return s.store.GetState(account, folder)
}

func (s *failCommitCursorStore) Commit(account, folder string, cursor backend.Cursor, pending PendingFlags) error {
	s.calls++
	if s.calls == s.failAt {
		return errors.New("injected cursor commit failure")
	}
	return s.store.Commit(account, folder, cursor, pending)
}

func TestSnapshotRestartRepairsSQLiteCheckpointBeforeFetchingNextPage(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}
	failing := &failCommitCursorStore{store: cursors, failAt: 1}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, nil)
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "base":
			return backend.FetchResult{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true}
		case "page-1":
			return backend.FetchResult{Cursor: backend.Cursor("final"), FullSnapshot: true}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}
	engine := newTestEngine(db, failing)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("first Sync returned top-level error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("first Sync errors = %v, want cursor commit failure", result.Errors)
	}
	state, err := db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || state.Complete || string(state.BaseCursor) != "base" || string(state.CheckpointCursor) != "page-1" {
		t.Fatalf("staged state after interrupted checkpoint = %+v", state)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "base" {
		t.Fatalf("cursor after interrupted checkpoint = %q, want base", got)
	}

	result, err = engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("restart result=%+v err=%v", result, err)
	}
	if len(fake.seenCursors["ALL"]) != 2 || string(fake.seenCursors["ALL"][1]) != "page-1" {
		t.Fatalf("provider cursors after restart = %q, want [base page-1]", fake.seenCursors["ALL"])
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "final" {
		t.Fatalf("final cursor = %q", got)
	}
	state, err = db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if state.Active {
		t.Fatalf("snapshot staging remains after recovery: %+v", state)
	}
}

func TestSnapshotRestartReconcilesCompleteStagingWithoutProviderFetch(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}
	failing := &failCommitCursorStore{store: cursors, failAt: 1}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, nil)
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		return backend.FetchResult{Cursor: backend.Cursor("final"), FullSnapshot: true}
	}
	engine := newTestEngine(db, failing)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("interrupted result=%+v err=%v", result, err)
	}
	state, err := db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || !state.Complete || string(state.CheckpointCursor) != "final" {
		t.Fatalf("complete staging after interrupted checkpoint = %+v", state)
	}

	result, err = engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("restart result=%+v err=%v", result, err)
	}
	if got := fake.seenCursors["ALL"]; len(got) != 1 || string(got[0]) != "base" {
		t.Fatalf("provider was fetched during local staged replay: %q", got)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "final" {
		t.Fatalf("final cursor = %q", got)
	}
}

func TestSnapshotCleanupWaitsForFinalCursorCommit(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}
	// The page checkpoint is commit 1. Fail commit 2, which clears the
	// SnapshotInProgress marker after reconciliation.
	failing := &failCommitCursorStore{store: cursors, failAt: 2}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {{Cursor: backend.Cursor("final"), FullSnapshot: true}},
	})
	engine := newTestEngine(db, failing)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("failed final commit result=%+v err=%v", result, err)
	}
	state, err := db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || !state.Complete {
		t.Fatalf("staging was cleared before final cursor commit: %+v", state)
	}
	cursorState, err := cursors.GetState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if string(cursorState.Cursor) != "final" || !cursorState.PendingFlags.SnapshotInProgress {
		t.Fatalf("checkpoint state after failed final commit = %+v", cursorState)
	}

	result, err = engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("cleanup retry result=%+v err=%v", result, err)
	}
	state, err = db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if state.Active {
		t.Fatalf("staging remains after successful final commit: %+v", state)
	}
}

func TestSnapshotOrphanedCursorMarkerRestartsAuthoritatively(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Commit(testAccount, "ALL", backend.Cursor("orphan"), PendingFlags{
		FullScan: true, SnapshotInProgress: true,
	}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, nil)
	fake.caps.InitialSnapshotIsAuthoritative = true
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		return backend.FetchResult{Cursor: backend.Cursor("fresh")}
	}

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("orphan recovery result=%+v err=%v", result, err)
	}
	if got := fake.seenCursors["ALL"]; len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("provider resumed from orphaned cursor instead of restarting: %q", got)
	}
	state, err := cursors.GetState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Cursor) != "fresh" || state.PendingFlags.SnapshotInProgress {
		t.Fatalf("state after orphan recovery = %+v", state)
	}
}
