package syncengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
)

func TestEngineReconcilesReplacementFullSnapshot(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id, ref string) backend.Message {
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("keep@example.com", "r1"), message("stale@example.com", "r2")}, Cursor: backend.Cursor("c1")},
			{Messages: []backend.Message{message("keep@example.com", "r1")}, Cursor: backend.Cursor("replacement-page-1"), HasMore: true, FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
			{Messages: []backend.Message{message("new@example.com", "r3")}, Cursor: backend.Cursor("c2"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r3"}}},
		},
	})
	cursors := newMemCursorStore()
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, MaxPerFolder: 1,
		Ingest: IngestOptions{Account: testAccount},
	})
	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	res, err := engine.Sync(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", res.Deleted)
	}
	if stale, _ := db.GetByMessageID("stale@example.com"); stale != nil {
		t.Fatalf("stale message still present: %#v", stale)
	}
	if keep, _ := db.GetByMessageID("keep@example.com"); keep == nil {
		t.Fatal("present message was removed")
	}
	if added, _ := db.GetByMessageID("new@example.com"); added == nil {
		t.Fatal("second replacement page was skipped by the normal message cap")
	}
	if got := string(cursors.cursors[cursors.key(testAccount, "ALL")]); got != "c2" {
		t.Fatalf("persisted cursor = %q, want final replacement cursor", got)
	}
}

func TestEngineRejectsDuplicateRefsAcrossReplacementPages(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	stale := backend.Message{
		MessageID: "stale@example.test", Ref: backend.RemoteRef{Folder: "ALL", ID: "stale"},
		Raw: rawMessage("stale@example.test", "sender@example.test", testAccount, "Stale", "body"),
	}
	if _, _, _, err := Ingest(db, stale, "ALL", backend.RoleAll, IngestOptions{Account: testAccount}); err != nil {
		t.Fatal(err)
	}
	ref := backend.RemoteRef{Folder: "ALL", ID: "duplicate"}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true, Present: []backend.RemoteRef{ref}},
			{Cursor: backend.Cursor("final"), FullSnapshot: true, Present: []backend.RemoteRef{ref}, Deleted: []backend.Deletion{{Ref: stale.Ref, MessageID: stale.MessageID}}},
		},
	})
	fake.snapshotMissing = map[string]bool{ref.ID: true}

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "duplicate snapshot ref") {
		t.Fatalf("Sync errors = %v, want duplicate replacement ref error", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q", got)
	}
	if got, _ := db.GetByMessageID(stale.MessageID); got == nil {
		t.Fatal("invalid replacement page deleted local state before validation")
	}
}

func TestEngineHydratesLocallyMissingSnapshotMessages(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id, ref string) backend.Message {
		return backend.Message{MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref}, Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body")}
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("keep@example.com", "r1")}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}, {Folder: "ALL", ID: "r2"}}},
		},
	})
	fake.snapshotMessages = map[string]backend.Message{"r2": message("missed@example.com", "r2")}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if _, err := engine.Sync(t.Context(), fake); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if len(fake.snapshotHydrated) != 1 || fake.snapshotHydrated[0].ID != "r2" {
		t.Fatalf("hydrated refs = %+v, want only r2", fake.snapshotHydrated)
	}
	if got, _ := db.GetByMessageID("missed@example.com"); got == nil {
		t.Fatal("locally missing snapshot message was not ingested")
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want new", got)
	}
}

func TestEngineBodyBatchLimitDoesNotShrinkSnapshotMetadataBatches(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	present := make([]backend.RemoteRef, 0, 10)
	for i := 0; i < 10; i++ {
		ref := backend.RemoteRef{Folder: folder.Name, ID: fmt.Sprintf("r%d", i)}
		present = append(present, ref)
		if i >= 5 {
			continue
		}
		messageID := fmt.Sprintf("existing-%d@example.com", i)
		msg := backend.Message{
			StableID: ref.ID, MessageID: messageID, Ref: ref,
			Raw: rawMessage(messageID, "a@example.com", testAccount, "Existing", "body"),
		}
		if _, _, _, err := Ingest(db, msg, folder.Name, folder.Role, IngestOptions{Account: testAccount}); err != nil {
			t.Fatal(err)
		}
	}

	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		folder.Name: {{Cursor: backend.Cursor("replacement"), FullSnapshot: true, Present: present}},
	})
	fake.caps.BodyBatchLimit = 2
	fake.snapshotMessages = make(map[string]backend.Message)
	for i := 5; i < 10; i++ {
		ref := present[i]
		messageID := fmt.Sprintf("missing-%d@example.com", i)
		fake.snapshotMessages[ref.ID] = backend.Message{
			StableID: ref.ID, MessageID: messageID, Ref: ref,
			Raw: rawMessage(messageID, "a@example.com", testAccount, "Missing", "body"),
		}
	}

	engine := New(Options{
		Store: db, Cursors: newMemCursorStore(), Account: testAccount, BatchLimit: 5,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, want := fake.fetchLimits[folder.Name], []int{2}; !slices.Equal(got, want) {
		t.Fatalf("FetchMessages limits = %v, want %v", got, want)
	}
	if got, want := fake.metadataBatches, []int{5}; !slices.Equal(got, want) {
		t.Fatalf("snapshot metadata batches = %v, want %v", got, want)
	}
	if got, want := fake.snapshotBatches, []int{2, 2, 1}; !slices.Equal(got, want) {
		t.Fatalf("snapshot body batches = %v, want %v", got, want)
	}
}

func TestEngineCompletesSnapshotWhenListedMessagesDisappearDuringHydration(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	seed := backend.Message{
		MessageID: "existing@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("existing@example.com", "a@example.com", testAccount, "Existing", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{seed}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}, {Folder: "ALL", ID: "r2"}}},
		},
	})
	fake.metadataMissing = map[string]bool{"r1": true}
	fake.snapshotMissing = map[string]bool{"r2": true}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted = %d, want existing ref removed after provider reported it gone", result.Deleted)
	}
	if got, _ := db.GetByMessageID("existing@example.com"); got != nil {
		t.Fatalf("disappeared existing message still present: %#v", got)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want replacement cursor new", got)
	}
}

func TestEngineRefreshesExistingSnapshotMetadata(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	seed := backend.Message{
		MessageID: "existing@example.com",
		Ref:       backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw:       rawMessage("existing@example.com", "a@example.com", testAccount, "Hello", "body"),
		Flags:     backend.Flags{Seen: true},
		Labels:    []string{"old-label"},
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{seed}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
			{Cursor: backend.Cursor("new"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "r1"}}},
		},
	})
	fake.caps.LabelsAreTags = true
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	fake.snapshotMetadata = map[string]backend.Message{
		"r1": {Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"}, Labels: []string{"new-label"}},
	}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("existing@example.com", testAccount, []string{"local"}, nil); err != nil {
		t.Fatal(err)
	}
	fake.fetchFlagsErr = errors.New("temporary flag fetch failure")
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("failed replacement sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor after failed flag reconciliation = %q, want old", got)
	}
	fake.fetchFlagsErr = nil
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("replacement sync result=%+v err=%v", result, err)
	}
	if got := fake.metadataFetched; len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("metadata refs = %+v, want one durable refresh of r1", got)
	}
	rows, err := db.GetFolderFlagState(testAccount, "ALL")
	if err != nil || len(rows) != 1 {
		t.Fatalf("flag state = %+v, err=%v", rows, err)
	}
	for _, want := range []string{"new-label", "local", "unread"} {
		if !slices.Contains(rows[0].Tags, want) {
			t.Errorf("tags = %v, missing %q", rows[0].Tags, want)
		}
	}
	if slices.Contains(rows[0].Tags, "old-label") {
		t.Errorf("tags = %v, stale label remains", rows[0].Tags)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "new" {
		t.Fatalf("cursor = %q, want new", got)
	}
}

func TestEngineAdvancesReplacementAfterRepeatedFlagFetchFailure(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		MessageID: "pending@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("pending@example.com", "a@example.com", testAccount, "Pending", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("replacement-1"), FullSnapshot: true, Present: []backend.RemoteRef{message.Ref}},
			{Cursor: backend.Cursor("delta-1")},
			{Cursor: backend.Cursor("delta-2")},
		},
	})
	fake.flagsByRef["r1"] = backend.Flags{}
	cursors := newMemCursorStore()
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	fake.fetchFlagsErr = errors.New("permanent flag endpoint failure")
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("first failed replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor after first failure = %q, want old", got)
	}
	pending, _ := cursors.GetPendingFlags(testAccount, "ALL")
	if !pending.FullScan || pending.ReplayCount != 1 || !pending.SnapshotInProgress {
		t.Fatalf("pending after first failure = %+v", pending)
	}
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("second failed replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "replacement-1" {
		t.Fatalf("cursor after staged retry failure = %q, want replacement-1", got)
	}
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
		t.Fatalf("pending retry result=%+v err=%v", result, err)
	}
	if got := fake.seenCursors["ALL"]; len(got) != 3 || string(got[1]) != "old" || string(got[2]) != "replacement-1" {
		t.Fatalf("FetchMessages cursors = %q, want staged replay without provider refetch", got)
	}
	fake.fetchFlagsErr = nil
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("successful pending retry result=%+v err=%v", result, err)
	}
	pending, _ = cursors.GetPendingFlags(testAccount, "ALL")
	if len(pending.Refs) != 0 || pending.ReplayCount != 0 {
		t.Fatalf("pending flags were not cleared: %+v", pending)
	}
}

func TestEngineCursorDrivenReplacementEpisodesSurviveUploadOnlyPasses(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		MessageID: "cursor-driven@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("cursor-driven@example.com", "a@example.com", testAccount, "Cursor", "body"),
	}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.caps.FlagChangesInDelta = true
	fake.flagsByRef["r1"] = backend.Flags{}
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		switch string(cursor) {
		case "":
			return backend.FetchResult{Messages: []backend.Message{message}, Cursor: backend.Cursor("steady-0")}
		case "steady-0":
			return backend.FetchResult{
				Cursor: backend.Cursor("replacement-1"), FullSnapshot: true,
				Present: []backend.RemoteRef{message.Ref},
			}
		case "replacement-1":
			return backend.FetchResult{
				Cursor: backend.Cursor("replacement-2"), FullSnapshot: true,
				Present: []backend.RemoteRef{message.Ref},
			}
		case "replacement-2":
			return backend.FetchResult{Cursor: backend.Cursor("steady-3")}
		default:
			return backend.FetchResult{Cursor: cursor}
		}
	}

	cursors := newMemCursorStore()
	download := newTestEngine(db, cursors)
	upload := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, Mode: UploadOnly,
		Ingest: IngestOptions{Account: testAccount},
	})
	if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	for episode := 0; episode < 2; episode++ {
		if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
			t.Fatalf("episode %d first replacement result=%+v err=%v", episode, result, err)
		}
		state, _ := cursors.GetState(testAccount, "ALL")
		if state.PendingFlags.ReplayCount != 1 || !state.PendingFlags.FullScan || !state.PendingFlags.SnapshotInProgress {
			t.Fatalf("episode %d held state = %+v", episode, state)
		}

		// The watcher runs upload-only after local mutations. Neither the
		// mutation nor this unrelated pass may reset the replacement replay.
		add, remove := []string{"flagged"}, []string(nil)
		if episode == 1 {
			add, remove = nil, []string{"flagged"}
		}
		if err := db.ModifyTagsByMessageIDAndAccount(message.MessageID, testAccount, add, remove); err != nil {
			t.Fatal(err)
		}
		if result, err := upload.Sync(t.Context(), fake); err != nil {
			t.Fatalf("episode %d upload-only result=%+v err=%v", episode, result, err)
		}
		state, _ = cursors.GetState(testAccount, "ALL")
		if state.PendingFlags.ReplayCount != 1 || !state.PendingFlags.FullScan || !state.PendingFlags.SnapshotInProgress {
			t.Fatalf("episode %d state after upload-only = %+v", episode, state)
		}

		if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) == 0 {
			t.Fatalf("episode %d replay result=%+v err=%v", episode, result, err)
		}
	}

	fake.fetchFlagsErr = nil
	if result, err := download.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "steady-3" || len(state.PendingFlags.Refs) != 0 || state.PendingFlags.ReplayCount != 0 {
		t.Fatalf("recovered state = %+v", state)
	}
	wantCursors := []backend.Cursor{
		backend.Cursor(""), backend.Cursor("steady-0"), backend.Cursor("replacement-1"), backend.Cursor("replacement-2"),
	}
	if got := fake.seenCursors["ALL"]; !slices.EqualFunc(got, wantCursors, func(a, b backend.Cursor) bool { return bytes.Equal(a, b) }) {
		t.Fatalf("FetchMessages cursors = %q, want %q", got, wantCursors)
	}
	for _, count := range cursors.serializedRefCounts {
		if count > 1 {
			t.Fatalf("duplicate pending queue growth: serialized ref counts = %v", cursors.serializedRefCounts)
		}
	}
}

func TestEngineDeltaCommitPreservesReplacementReplay(t *testing.T) {
	db := newTestDB(t)
	message := backend.Message{
		MessageID: "delta-interleave@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "m1"},
		Raw: rawMessage("delta-interleave@example.com", "a@example.com", testAccount, "Delta", "body"),
	}
	if _, _, _, err := Ingest(db, message, "INBOX", backend.RoleInbox, IngestOptions{Account: testAccount}); err != nil {
		t.Fatal(err)
	}
	cursors := newMemCursorStore()
	key := cursors.key(testAccount, "INBOX")
	cursors.cursors[key] = backend.Cursor("c1")
	cursors.pending[key] = PendingFlags{Refs: []string{"m1"}, ReplayCount: 1}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{Messages: []backend.Message{message}, Cursor: backend.Cursor("c2")}},
	})
	fake.caps.FlagChangesInDelta = true
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("delta result=%+v err=%v", result, err)
	}
	state, _ := cursors.GetState(testAccount, "INBOX")
	if string(state.Cursor) != "c2" || state.PendingFlags.ReplayCount != 1 || !slices.Equal(state.PendingFlags.Refs, []string{"m1"}) {
		t.Fatalf("state after unrelated delta = %+v", state)
	}
}

func TestEngineRejectsSnapshotToDeltaTransition(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Cursor: backend.Cursor("snapshot"), HasMore: true, FullSnapshot: true},
			{Cursor: backend.Cursor("delta"), FullSnapshot: false},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "replacement-snapshot back to delta") {
		t.Fatalf("Sync errors = %v", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q", got)
	}
}

func TestEngineRestartsInvalidatedPagedSnapshotFromBaseCursor(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, nil)
	fake.fetchErrByCursor = map[string]error{"page-1": backend.ErrSnapshotInvalidated}
	baseFetches := 0
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		if string(cursor) != "base" {
			t.Fatalf("unexpected provider cursor %q", cursor)
		}
		baseFetches++
		if baseFetches == 1 {
			return backend.FetchResult{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true}
		}
		return backend.FetchResult{Cursor: backend.Cursor("final"), FullSnapshot: true}
	}
	engine := newTestEngine(db, cursors)

	result, err := engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 1 || !errors.Is(result.Errors[0], backend.ErrSnapshotInvalidated) {
		t.Fatalf("invalidated sync result=%+v err=%v", result, err)
	}
	snapshot, err := db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Active {
		t.Fatalf("invalidated staging remains active: %+v", snapshot)
	}
	state, err := cursors.GetState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if string(state.Cursor) != "base" || state.PendingFlags.SnapshotInProgress {
		t.Fatalf("state after invalidation = %+v", state)
	}

	result, err = engine.Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("restart result=%+v err=%v", result, err)
	}
	if baseFetches != 2 {
		t.Fatalf("base cursor fetches = %d, want 2", baseFetches)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "final" {
		t.Fatalf("final cursor = %q", got)
	}
}

func TestInvalidatedSnapshotKeepsStagingUntilBaseCursorIsDurable(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}
	failing := &failCommitCursorStore{store: cursors, failAt: 2}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, nil)
	fake.fetchErrByCursor = map[string]error{"page-1": backend.ErrSnapshotInvalidated}
	fake.fetchByCursor = func(_ string, cursor backend.Cursor) backend.FetchResult {
		if string(cursor) != "base" {
			t.Fatalf("unexpected provider cursor %q", cursor)
		}
		return backend.FetchResult{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true}
	}

	result, err := newTestEngine(db, failing).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "injected cursor commit failure") {
		t.Fatalf("interrupted invalidation result=%+v err=%v", result, err)
	}
	snapshot, err := db.GetSnapshotState(testAccount, "ALL")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Active || snapshot.Complete || string(snapshot.BaseCursor) != "base" || string(snapshot.CheckpointCursor) != "page-1" {
		t.Fatalf("staging was cleared before base cursor commit: %+v", snapshot)
	}
	state, err := cursors.GetState(testAccount, "ALL")
	if err != nil || string(state.Cursor) != "page-1" || !state.PendingFlags.SnapshotInProgress {
		t.Fatalf("durable cursor state after interrupted invalidation = %+v, err=%v", state, err)
	}
}

func TestEngineAllowsDeltaToReplacementRestart(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	message := func(id, ref string) backend.Message {
		return backend.Message{
			MessageID: id, Ref: backend.RemoteRef{Folder: "ALL", ID: ref},
			Raw: rawMessage(id, "a@example.com", testAccount, "Hello", "body"),
		}
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message("delta@example.com", "d1")}, Cursor: backend.Cursor("expired-next"), HasMore: true},
			{Messages: []backend.Message{message("snapshot@example.com", "s1")}, Cursor: backend.Cursor("replacement"), FullSnapshot: true, Present: []backend.RemoteRef{{Folder: "ALL", ID: "d1"}, {Folder: "ALL", ID: "s1"}}},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
	for _, id := range []string{"delta@example.com", "snapshot@example.com"} {
		if got, _ := db.GetByMessageID(id); got == nil {
			t.Fatalf("message %s was not retained", id)
		}
	}
}

func TestEngineUsesLatestOrdinaryCheckpointAsReplacementReplayBase(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "ALL", backend.Cursor("old"))
	message := backend.Message{
		MessageID: "transition@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("transition@example.com", "a@example.com", testAccount, "Transition", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("ordinary-page"), HasMore: true},
			{Messages: []backend.Message{message}, Present: []backend.RemoteRef{message.Ref}, Cursor: backend.Cursor("replacement"), FullSnapshot: true},
		},
	})
	fake.caps.FlagChangesInDelta = true
	fake.fetchFlagsErr = errors.New("flag endpoint unavailable")

	result, err := newTestEngine(db, cursors).Sync(t.Context(), fake)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("Sync result=%+v err=%v, want flag error", result, err)
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "ordinary-page" || state.PendingFlags.ReplayCount != 1 {
		t.Fatalf("replacement replay base = %+v, want ordinary-page with replay count 1", state)
	}
}

func TestEngineDryRunReplacementDoesNotRequireHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	_ = cursors.Set(testAccount, "INBOX", backend.Cursor("old"))
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "1"}, {Folder: "INBOX", ID: "2"}},
		}},
	})
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, DryRun: true,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if result.New != 2 {
		t.Fatalf("New = %d, want 2 projected arrivals", result.New)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "old" {
		t.Fatalf("dry-run cursor = %q, want old", got)
	}
}

func TestEngineReplacementSkipsAbsentRefWithoutHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "collapsed-or-expunged"}},
		}},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineReplacementRemovesRefExpungedOnLaterPage(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "expunged@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "gone"},
		Raw: rawMessage("expunged@example.com", "a@example.com", testAccount, "Gone", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{
				Messages: []backend.Message{message}, Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true,
				Present: []backend.RemoteRef{message.Ref},
			},
			{
				Cursor: backend.Cursor("replacement"), FullSnapshot: true,
				Deleted: []backend.Deletion{{Ref: message.Ref, MessageID: message.MessageID}},
			},
		},
	})
	result, err := newTestEngine(db, cursors).Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if got, _ := db.GetByMessageID(message.MessageID); got != nil {
		t.Fatalf("expunged message remains: %#v", got)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineDryRunFullBodyReplacementCountsEachArrivalOnce(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "projected@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "1"},
		Raw: rawMessage("projected@example.com", "a@example.com", testAccount, "Projected", "body"),
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{message}, Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{message.Ref},
		}},
	})
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, DryRun: true,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v", result, err)
	}
	if result.New != 1 {
		t.Fatalf("New = %d, want one projected arrival", result.New)
	}
}

func TestEngineReplacementSkipsPermanentIngestFailureWithoutHydrator(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {{
			Messages: []backend.Message{{
				MessageID: "broken@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "broken"},
				Raw: []byte("malformed header line\r\n\r\nbody"),
			}},
			Cursor: backend.Cursor("replacement"), FullSnapshot: true,
			Present: []backend.RemoteRef{{Folder: "INBOX", ID: "broken"}},
		}},
	})
	engine := newTestEngine(db, cursors)
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil {
		t.Fatalf("Sync error = %v", err)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "parse message") {
		t.Fatalf("Sync errors = %v, want one permanent parse failure", result.Errors)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement", got)
	}
}

func TestEngineLetsReplacementFinishBeyondOrdinaryTimeout(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true},
			{Cursor: backend.Cursor("final"), FullSnapshot: true},
		},
	})
	delayed := &delayedBackend{Backend: fake, delays: []time.Duration{0, 100 * time.Millisecond}}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Timeout: 50 * time.Millisecond, RecoveryTimeout: 500 * time.Millisecond,
		Ingest: IngestOptions{Account: testAccount},
	})
	result, err := engine.Sync(t.Context(), delayed)
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Errors) != 0 {
		t.Fatalf("Sync result=%+v err=%v, want ordinary deadline after completed recovery", result, err)
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "final" {
		t.Fatalf("cursor = %q, want final", got)
	}
}

func TestEngineReplacementFlagDeadlineMakesBoundedProgress(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "deadline@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "r1"},
		Raw: rawMessage("deadline@example.com", "a@example.com", testAccount, "Deadline", "body"),
	}
	if _, _, _, err := Ingest(db, message, "ALL", backend.RoleAll, IngestOptions{Account: testAccount}); err != nil {
		t.Fatal(err)
	}
	if err := cursors.Set(testAccount, "ALL", backend.Cursor("old")); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}, nil)
	replacements := 0
	fake.fetchByCursor = func(_ string, _ backend.Cursor) backend.FetchResult {
		replacements++
		return backend.FetchResult{
			Cursor: backend.Cursor(fmt.Sprintf("replacement-%d", replacements)), FullSnapshot: true,
			Messages: []backend.Message{message}, Present: []backend.RemoteRef{message.Ref},
		}
	}
	delayed := &delayedFlagsBackend{Backend: fake, delay: 100 * time.Millisecond}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Timeout: 5 * time.Millisecond, RecoveryTimeout: 20 * time.Millisecond,
		Ingest: IngestOptions{Account: testAccount},
	})

	for pass := 0; pass < 2; pass++ {
		result, err := engine.Sync(t.Context(), delayed)
		if !errors.Is(err, context.DeadlineExceeded) || len(result.Errors) == 0 {
			t.Fatalf("pass %d result=%+v err=%v, want recorded flag deadline", pass, result, err)
		}
	}
	state, _ := cursors.GetState(testAccount, "ALL")
	if string(state.Cursor) != "replacement-1" || state.PendingFlags.ReplayCount != 0 || !state.PendingFlags.FullScan || state.PendingFlags.SnapshotInProgress {
		t.Fatalf("state after two deadline passes = %+v", state)
	}
	if got, want := fake.seenCursors["ALL"], []backend.Cursor{backend.Cursor("old")}; !slices.EqualFunc(got, want, func(a, b backend.Cursor) bool { return bytes.Equal(a, b) }) {
		t.Fatalf("FetchMessages cursors = %q, want staged replay without provider refetch %q", got, want)
	}
}

func TestEngineDoesNotPersistSnapshotCursorWhenReconciliationFails(t *testing.T) {
	db := newTestDB(t)
	message := backend.Message{
		MessageID: "stale@example.com", Ref: backend.RemoteRef{Folder: "ALL", ID: "stale"},
		Raw: rawMessage("stale@example.com", "a@example.com", testAccount, "Stale", "body"),
	}
	cursors := newMemCursorStore()
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Selectable: true}}, map[string][]backend.FetchResult{
		"ALL": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("final"), FullSnapshot: true},
		},
	})
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed sync result=%+v err=%v", result, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Sync(t.Context(), fake)
	if err != nil {
		t.Fatalf("Sync returned top-level error: %v", err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("Sync unexpectedly succeeded with closed reconciliation store")
	}
	if got, _ := cursors.Get(testAccount, "ALL"); string(got) != "old" {
		t.Fatalf("cursor advanced to %q after reconciliation failure", got)
	}
}

func TestEnginePersistsReplacementCursorWhenFlagUploadFails(t *testing.T) {
	db := newTestDB(t)
	cursors := newMemCursorStore()
	message := backend.Message{
		MessageID: "upload-denied@example.com", Ref: backend.RemoteRef{Folder: "INBOX", ID: "m1"},
		Raw:   rawMessage("upload-denied@example.com", "a@example.com", testAccount, "Denied", "body"),
		Flags: backend.Flags{Flagged: true},
	}
	fake := newFakeBackend([]backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}, map[string][]backend.FetchResult{
		"INBOX": {
			{Messages: []backend.Message{message}, Cursor: backend.Cursor("old")},
			{Cursor: backend.Cursor("replacement"), FullSnapshot: true, Present: []backend.RemoteRef{message.Ref}},
		},
	})
	fake.flagsByRef["m1"] = backend.Flags{Flagged: true}
	engine := newTestEngine(db, cursors)
	if result, err := engine.Sync(t.Context(), fake); err != nil || len(result.Errors) != 0 {
		t.Fatalf("seed result=%+v err=%v", result, err)
	}
	if err := db.ModifyTagsByMessageIDAndAccount("upload-denied@example.com", testAccount, nil, []string{"unread"}); err != nil {
		t.Fatal(err)
	}
	fake.applyFlagsErr = errors.New("permanent access denied")
	result, err := engine.Sync(t.Context(), &backendOnly{Backend: fake})
	if err != nil || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "flag upload") {
		t.Fatalf("replacement result=%+v err=%v", result, err)
	}
	if got, _ := cursors.Get(testAccount, "INBOX"); string(got) != "replacement" {
		t.Fatalf("cursor = %q, want replacement despite durable local upload failure", got)
	}
}
