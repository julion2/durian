package syncengine

import (
	"fmt"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/store"
)

func TestUploadOnlyNativePatchPassDoesNotLoadFullScanMailbox(t *testing.T) {
	db := newTestDB(t)
	var lastID int64
	for i := 0; i < maxFullScanRowsPerSync+1; i++ {
		ref := fmt.Sprintf("ref-%04d", i)
		msg := &store.Message{
			StableID: ref, MessageID: ref + "@example.test", RemoteRef: ref,
			Account: testAccount, Mailbox: "ALL", Date: int64(i + 1), CreatedAt: int64(i + 1),
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
		lastID = msg.ID
	}
	if err := db.AddTag(lastID, "project"); err != nil {
		t.Fatal(err)
	}
	fake := newFakeBackend([]backend.Folder{{Name: "ALL", Role: backend.RoleAll, Selectable: true}}, nil)
	fake.caps.LabelsAreTags = true
	native := &scopedNativeLabelBackend{arbitraryLabelBackend: &arbitraryLabelBackend{fakeBackend: fake}}
	cursors := newMemCursorStore()
	pending := PendingFlags{FullScan: true, ScanAfterID: 123}
	if err := cursors.Commit(testAccount, "ALL", nil, pending); err != nil {
		t.Fatal(err)
	}
	engine := New(Options{
		Store: db, Cursors: cursors, Account: testAccount, Mode: UploadOnly,
		Ingest: IngestOptions{Account: testAccount},
	})

	result, err := engine.Sync(t.Context(), native)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("upload-only result=%+v err=%v", result, err)
	}
	state, err := cursors.GetState(testAccount, "ALL")
	if err != nil || !folderStateEqual(FolderState{PendingFlags: state.PendingFlags}, FolderState{PendingFlags: pending}) {
		t.Fatalf("upload-only pending state = %+v, err=%v", state, err)
	}
	if len(fake.fetchFlagsCalls) != 0 {
		t.Fatalf("upload-only native pass fetched server flags: %+v", fake.fetchFlagsCalls)
	}
	if len(fake.labelCalls) != 1 || fake.labelCalls[0].ref.ID != fmt.Sprintf("ref-%04d", maxFullScanRowsPerSync) {
		t.Fatalf("paged label uploads = %+v, want final row", fake.labelCalls)
	}
}
