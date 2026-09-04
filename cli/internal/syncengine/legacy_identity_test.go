package syncengine

import (
	"errors"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
)

type legacyIdentityBackend struct {
	*nativeTagPatchBackend
}

func (b *legacyIdentityBackend) LegacyIdentityMigration(cursor backend.Cursor) (backend.Cursor, string, bool) {
	if string(cursor) != "legacy-cursor" {
		return nil, "", false
	}
	return nil, "account-scope:", true
}

func TestEngineScopesLegacyRowsBeforeUploadingProviderMutations(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		StableID: "email-1", MessageID: "legacy@example.test",
		Ref: backend.RemoteRef{Folder: folder.Name, ID: "email-1"},
		Raw: rawMessage("legacy@example.test", "sender@example.test", testAccount, "Legacy", "body"),
	}
	_, rowID, _, err := Ingest(db, message, folder.Name, folder.Role, IngestOptions{Account: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ModifyTagsByMessageDBIDAndJournal(rowID, []string{"flagged", "local-only"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, folder.Name, backend.Cursor("legacy-cursor")); err != nil {
		t.Fatal(err)
	}
	scopedMessage := message
	scopedMessage.StableID = "account-scope:email-1"
	scopedMessage.Ref.ID = scopedMessage.StableID
	fake := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		folder.Name: {{
			Messages: []backend.Message{scopedMessage}, Present: []backend.RemoteRef{scopedMessage.Ref},
			Cursor: backend.Cursor("scoped-cursor"), FullSnapshot: true,
		}},
	})
	native := &nativeTagPatchBackend{fakeBackend: fake}
	b := &legacyIdentityBackend{nativeTagPatchBackend: native}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Ingest: IngestOptions{Account: testAccount},
	})

	result, err := engine.Sync(t.Context(), b)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("legacy migration result=%+v err=%v", result, err)
	}
	if len(native.tagMutationCalls) != 1 || native.tagMutationCalls[0].ref.ID != "account-scope:email-1" {
		t.Fatalf("provider mutation calls = %+v", native.tagMutationCalls)
	}
	row, err := db.GetByRemoteRef(testAccount, folder.Name, "account-scope:email-1")
	if err != nil || row == nil || row.ID != rowID || row.StableID != "account-scope:email-1" {
		t.Fatalf("scoped row = %#v, err=%v", row, err)
	}
	state, err := cursors.GetState(testAccount, folder.Name)
	if err != nil || string(state.Cursor) != "scoped-cursor" {
		t.Fatalf("scoped cursor state = %+v, err=%v", state, err)
	}
	mutations, err := db.ReadProviderTagMutations(testAccount)
	if err != nil || len(mutations) != 0 {
		t.Fatalf("provider mutation queue = %+v, err=%v", mutations, err)
	}
}

func TestEngineDefersProviderMutationsDuringSQLiteOnlySnapshotRecovery(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		StableID: "account-scope:email-1", MessageID: "legacy@example.test",
		Ref: backend.RemoteRef{Folder: folder.Name, ID: "account-scope:email-1"},
		Raw: rawMessage("legacy@example.test", "sender@example.test", testAccount, "Legacy", "body"),
	}
	_, rowID, _, err := Ingest(db, message, folder.Name, folder.Role, IngestOptions{Account: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ModifyTagsByMessageDBIDAndJournal(rowID, []string{"flagged"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, folder.Name, backend.Cursor("scoped-base")); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginSnapshot(testAccount, folder.Name, backend.Cursor("scoped-base")); err != nil {
		t.Fatal(err)
	}
	if err := db.StageSnapshotPage(
		testAccount, folder.Name,
		[]string{message.Ref.ID}, []string{message.Ref.ID}, nil,
		backend.Cursor("page-1"), false,
	); err != nil {
		t.Fatal(err)
	}

	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.fetchErrByCursor = map[string]error{"page-1": errors.New("replacement unavailable")}
	native := &nativeTagPatchBackend{fakeBackend: fake}
	b := &legacyIdentityBackend{nativeTagPatchBackend: native}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Ingest: IngestOptions{Account: testAccount},
	})

	result, err := engine.Sync(t.Context(), b)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("snapshot recovery result=%+v err=%v", result, err)
	}
	if len(native.tagMutationCalls) != 0 {
		t.Fatalf("provider mutation crossed incomplete snapshot: %+v", native.tagMutationCalls)
	}
	mutations, err := db.ReadProviderTagMutations(testAccount)
	if err != nil || len(mutations) != 1 {
		t.Fatalf("provider mutation queue = %+v, err=%v", mutations, err)
	}
}

func TestLegacyIdentityMarkerBlocksMutationsBeforeFirstSnapshotPage(t *testing.T) {
	db := newTestDB(t)
	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := backend.Message{
		StableID: "email-1", MessageID: "legacy@example.test",
		Ref: backend.RemoteRef{Folder: folder.Name, ID: "email-1"},
		Raw: rawMessage("legacy@example.test", "sender@example.test", testAccount, "Legacy", "body"),
	}
	_, rowID, _, err := Ingest(db, message, folder.Name, folder.Role, IngestOptions{Account: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ModifyTagsByMessageDBIDAndJournal(rowID, []string{"flagged"}, nil, 1); err != nil {
		t.Fatal(err)
	}

	cursors := newMemCursorStore()
	if err := cursors.Set(testAccount, folder.Name, backend.Cursor("legacy-cursor")); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{folder}, nil)
	fake.fetchErrByCursor = make(map[string]error)
	native := &nativeTagPatchBackend{fakeBackend: fake}
	b := &legacyIdentityBackend{nativeTagPatchBackend: native}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount,
		Ingest: IngestOptions{Account: testAccount},
	})

	for attempt := 0; attempt < 2; attempt++ {
		fake.fetchErrByCursor[""] = errors.New("replacement unavailable before first page")
		result, err := engine.Sync(t.Context(), b)
		if err != nil || len(result.Errors) != 1 {
			t.Fatalf("attempt %d result=%+v err=%v", attempt+1, result, err)
		}
		if len(native.tagMutationCalls) != 0 {
			t.Fatalf("attempt %d uploaded mutation before first snapshot page: %+v", attempt+1, native.tagMutationCalls)
		}
		state, err := cursors.GetState(testAccount, folder.Name)
		if err != nil || len(state.Cursor) != 0 || !state.PendingFlags.SnapshotInProgress || !state.PendingFlags.FullScan {
			t.Fatalf("attempt %d durable migration marker = %+v, err=%v", attempt+1, state, err)
		}
		snapshot, err := db.GetSnapshotState(testAccount, folder.Name)
		if err != nil || snapshot.Active {
			t.Fatalf("attempt %d snapshot state = %+v, err=%v", attempt+1, snapshot, err)
		}
	}
	mutations, err := db.ReadProviderTagMutations(testAccount)
	if err != nil || len(mutations) != 1 || mutations[0].RemoteRef != "account-scope:email-1" {
		t.Fatalf("preserved scoped provider mutation = %+v, err=%v", mutations, err)
	}
}
