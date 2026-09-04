package store

import (
	"slices"
	"testing"
)

func TestFolderFlagStatePagingKeepsCompleteTagRows(t *testing.T) {
	db := newTestDB(t)
	const account = "work"
	const folder = "ALL"

	ids := make(map[string]int64)
	for i, ref := range []string{"r1", "r2", "r3"} {
		msg := &Message{
			StableID: ref, MessageID: ref + "@example.test", RemoteRef: ref,
			Account: account, Mailbox: folder, Date: int64(i + 1), CreatedAt: int64(i + 1),
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatal(err)
		}
		ids[ref] = msg.ID
	}
	for _, tag := range []string{"inbox", "unread", "project"} {
		if err := db.AddTag(ids["r1"], tag); err != nil {
			t.Fatal(err)
		}
	}

	first, err := db.GetFolderFlagStatePage(account, folder, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].RemoteRef != "r1" || !slices.Equal(first[0].Tags, []string{"inbox", "project", "unread"}) {
		t.Fatalf("first flag page = %+v", first)
	}
	second, err := db.GetFolderFlagStatePage(account, folder, first[0].RowID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].RemoteRef != "r2" {
		t.Fatalf("second flag page = %+v", second)
	}

	selected, err := db.GetFolderFlagStateForRefs(account, folder, []string{"r3", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].RemoteRef != "r3" {
		t.Fatalf("selected flag rows = %+v", selected)
	}
}
