package invitationsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

// seed stores n calendar messages of account "work", the newest last; their
// row ids are returned newest first, the order Fill works in.
func seed(t *testing.T, n int) (*store.DB, []int64) {
	t.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(":memory:", kr)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("invite%d@example.com", i)
		m := &store.Message{MessageID: id, Subject: "Invitation", Date: int64(1000 + i), CreatedAt: 1,
			Mailbox: "INBOX", Account: "work", UID: uint32(i + 1), FetchedBody: true}
		if err := db.InsertMessage(m); err != nil {
			t.Fatal(err)
		}
		stored, err := db.GetByMessageID(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddTag(stored.ID, store.CalendarTag); err != nil {
			t.Fatal(err)
		}
		ids[n-1-i] = stored.ID
	}
	return db, ids
}

func icsFor(id int64) string {
	return fmt.Sprintf("BEGIN:VCALENDAR\r\nUID:%d\r\nEND:VCALENDAR\r\n", id)
}

func TestFill_CatchesUpNewestFirstThenStops(t *testing.T) {
	db, ids := seed(t, batchSize+7) // more than one batch
	var fetched []int64
	fetch := func(_ context.Context, m store.MissingInvitation) (string, error) {
		fetched = append(fetched, m.ID)
		if m.ID == ids[1] {
			return "", nil // looked at: no invitation in it
		}
		return icsFor(m.ID), nil
	}
	stored, err := Fill(context.Background(), db, "work", store.ByUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if stored != len(ids) || !reflect.DeepEqual(fetched, ids) {
		t.Fatalf("stored %d, fetched %v; want all %d, newest first %v", stored, fetched, len(ids), ids)
	}
	got, err := db.InvitationsByMessages(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids)-1 || got[ids[0]] != icsFor(ids[0]) {
		t.Fatalf("stored invitations = %d (first %q), want %d", len(got), got[ids[0]], len(ids)-1)
	}

	// caught up: the next sync fetches nothing
	fetched = nil
	if stored, err := Fill(context.Background(), db, "work", store.ByUID, fetch); err != nil || stored != 0 || len(fetched) != 0 {
		t.Fatalf("second run stored %d, fetched %v, err %v; want nothing", stored, fetched, err)
	}
}

// A message that fails doesn't hold up the others, and is tried again next
// run.
func TestFill_SkipsFailuresForTheRun(t *testing.T) {
	db, ids := seed(t, 3)
	attempts := map[int64]int{}
	broken := true
	fetch := func(_ context.Context, m store.MissingInvitation) (string, error) {
		attempts[m.ID]++
		if m.ID == ids[0] && broken {
			return "", errors.New("connection reset")
		}
		return icsFor(m.ID), nil
	}
	stored, err := Fill(context.Background(), db, "work", store.ByUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 2 || attempts[ids[0]] != 1 {
		t.Fatalf("stored %d with %d attempts on the failing one; want 2 and 1", stored, attempts[ids[0]])
	}
	if got, _ := db.InvitationsByMessages(ids[:1]); len(got) != 0 {
		t.Fatal("a failed fetch must not be stored as 'no invitation'")
	}

	broken = false
	if stored, err := Fill(context.Background(), db, "work", store.ByUID, fetch); err != nil || stored != 1 {
		t.Fatalf("next run stored %d (err %v), want the one that failed", stored, err)
	}
}

// Running out of time cuts a fetch short; that isn't the message's failure,
// and nothing is stored for it.
func TestFill_StopsWhenTimeIsUp(t *testing.T) {
	db, ids := seed(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	fetch := func(ctx context.Context, m store.MissingInvitation) (string, error) {
		if m.ID == ids[1] {
			cancel()
			return "", ctx.Err()
		}
		return icsFor(m.ID), nil
	}
	stored, err := Fill(ctx, db, "work", store.ByUID, fetch)
	if err != nil || stored != 1 {
		t.Fatalf("stored %d (err %v), want only the one before time ran out", stored, err)
	}
	missing, err := db.MissingInvitations("work", store.ByUID, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || missing[0].ID != ids[1] {
		t.Fatalf("missing after the cut = %+v, want the cut one first, then the rest", missing)
	}
}
