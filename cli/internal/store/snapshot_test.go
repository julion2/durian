package store

import (
	"errors"
	"slices"
	"testing"
)

func TestSnapshotPersistsEpisodeCheckpointAndCompletion(t *testing.T) {
	db := newTestDB(t)
	const account = "work"
	const folder = "ALL"

	if err := db.BeginSnapshot(account, folder, []byte("base")); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetSnapshotState(account, folder)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Active || string(state.BaseCursor) != "base" || string(state.CheckpointCursor) != "base" || state.Complete {
		t.Fatalf("initial snapshot state = %+v", state)
	}

	if err := db.StageSnapshotPage(account, folder, []string{"r1"}, []string{"r1"}, nil, []byte("page-1"), false); err != nil {
		t.Fatal(err)
	}
	state, err = db.GetSnapshotState(account, folder)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.BaseCursor) != "base" || string(state.CheckpointCursor) != "page-1" || state.Complete {
		t.Fatalf("intermediate snapshot state = %+v", state)
	}

	if err := db.StageSnapshotPage(account, folder, []string{"r2"}, []string{"r2"}, nil, []byte("final"), true); err != nil {
		t.Fatal(err)
	}
	state, err = db.GetSnapshotState(account, folder)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.BaseCursor) != "base" || string(state.CheckpointCursor) != "final" || !state.Complete {
		t.Fatalf("complete snapshot state = %+v", state)
	}

	if err := db.ClearSnapshot(account, folder); err != nil {
		t.Fatal(err)
	}
	state, err = db.GetSnapshotState(account, folder)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active {
		t.Fatalf("cleared snapshot remains active: %+v", state)
	}
	var refs int
	if err := db.db.QueryRow(`SELECT count(*) FROM snapshot_present_refs WHERE account=? AND folder=?`, account, folder).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("staged refs after clear = %d", refs)
	}
}

func TestSnapshotRejectsDuplicateRefsWithoutAdvancingCheckpoint(t *testing.T) {
	db := newTestDB(t)
	const account = "work"
	const folder = "ALL"

	if err := db.BeginSnapshot(account, folder, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.StageSnapshotPage(account, folder, []string{"r1"}, []string{"r1"}, nil, []byte("page-1"), false); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateSnapshotPageRefs(account, folder, []string{"r2", "r1"}); !errors.Is(err, ErrDuplicateSnapshotRef) {
		t.Fatalf("ValidateSnapshotPageRefs error = %v, want duplicate", err)
	}
	if err := db.StageSnapshotPage(account, folder, []string{"r2", "r2"}, []string{"r2"}, nil, []byte("page-2"), false); !errors.Is(err, ErrDuplicateSnapshotRef) {
		t.Fatalf("StageSnapshotPage error = %v, want duplicate", err)
	}

	state, err := db.GetSnapshotState(account, folder)
	if err != nil {
		t.Fatal(err)
	}
	if string(state.CheckpointCursor) != "page-1" || state.Complete {
		t.Fatalf("snapshot state advanced after rejected page: %+v", state)
	}
	var refs int
	if err := db.db.QueryRow(`SELECT count(*) FROM snapshot_present_refs WHERE account=? AND folder=?`, account, folder).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 1 {
		t.Fatalf("staged refs after rejected page = %d, want 1", refs)
	}
}

func TestSnapshotPresenceDistinguishesSeenPreservedAndAbsentRefs(t *testing.T) {
	db := newTestDB(t)
	const account = "work"
	const folder = "ALL"

	rows := make(map[string]int64)
	for i, ref := range []string{"present", "seen-only", "preserved", "unreported"} {
		msg := &Message{
			StableID: "stable-" + ref, MessageID: ref + "@example.test", RemoteRef: ref,
			Account: account, Mailbox: folder, Date: int64(i + 1), CreatedAt: int64(i + 1),
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatalf("insert %s: %v", ref, err)
		}
		rows[ref] = msg.ID
	}

	if err := db.BeginSnapshot(account, folder, []byte("base")); err != nil {
		t.Fatal(err)
	}
	if err := db.StageSnapshotPage(
		account,
		folder,
		[]string{"present", "seen-only"},
		[]string{"present"},
		[]string{"preserved"},
		[]byte("final"),
		true,
	); err != nil {
		t.Fatal(err)
	}

	first, err := db.SnapshotAbsentRows(account, folder, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].RemoteRef != "seen-only" {
		t.Fatalf("first absent page = %+v, want seen-only", first)
	}
	second, err := db.SnapshotAbsentRows(account, folder, first[0].RowID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].RemoteRef != "unreported" {
		t.Fatalf("second absent page = %+v, want unreported", second)
	}
	final, err := db.SnapshotAbsentRows(account, folder, second[0].RowID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Fatalf("final absent page = %+v, want empty", final)
	}

	selected, err := db.SnapshotRowsForRefs(account, folder, []string{"present", "unreported"})
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := []int64{selected[0].RowID, selected[1].RowID}
	wantIDs := []int64{rows["present"], rows["unreported"]}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("bounded snapshot rows = %v, want %v", gotIDs, wantIDs)
	}

	byMessageID, err := db.SnapshotRefsForMessageIDs(account, folder, []string{"preserved@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := byMessageID["preserved@example.test"]; !slices.Equal(got, []string{"preserved"}) {
		t.Fatalf("preserved refs = %v", got)
	}
}
