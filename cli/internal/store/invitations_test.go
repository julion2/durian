package store

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const testICS = "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:plan@example.com\r\nSUMMARY:Quarterly planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// seedCalMessage stores a message of an account, tagged as calendar mail
// when cal is set, and returns its row id.
func seedCalMessage(t *testing.T, db *DB, id, account string, date int64, uid uint32, remoteRef string, cal bool) int64 {
	t.Helper()
	m := &Message{MessageID: id, Subject: "Planning", Date: date, CreatedAt: date, Mailbox: "INBOX",
		Account: account, UID: uid, RemoteRef: remoteRef, FetchedBody: true}
	if err := db.InsertMessage(m); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetByMessageID(id)
	if err != nil {
		t.Fatal(err)
	}
	if cal {
		if err := db.AddTag(stored.ID, CalendarTag); err != nil {
			t.Fatal(err)
		}
	}
	return stored.ID
}

func TestInvitations_StoredEncryptedAndRead(t *testing.T) {
	db := newTestDB(t)
	withICS := seedCalMessage(t, db, "a@x", "work", 1, 1, "", true)
	without := seedCalMessage(t, db, "b@x", "work", 2, 2, "", true)
	if err := db.SetInvitation(withICS, testICS); err != nil {
		t.Fatal(err)
	}
	if err := db.SetInvitation(without, ""); err != nil {
		t.Fatal(err)
	}

	var ct []byte
	if err := db.db.QueryRow(`SELECT ics_ct FROM message_invitations WHERE message_db_id = ?`, withICS).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if len(ct) == 0 || bytes.Contains(ct, []byte("Quarterly planning")) {
		t.Fatalf("ics_ct must hold the invitation encrypted, got %q", ct)
	}

	got, err := db.InvitationsByMessages([]int64{withICS, without})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]string{withICS: testICS}; !reflect.DeepEqual(got, want) {
		t.Fatalf("InvitationsByMessages = %v, want %v", got, want)
	}

	// a later store replaces the earlier one
	if err := db.SetInvitation(without, testICS); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.InvitationsByMessages([]int64{without}); got[without] != testICS {
		t.Fatalf("after storing again = %q, want the invitation", got[without])
	}
}

func TestInvitations_GoWithTheirMessage(t *testing.T) {
	db := newTestDB(t)
	id := seedCalMessage(t, db, "a@x", "work", 1, 1, "", true)
	if err := db.SetInvitation(id, testICS); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteByDBID(id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM message_invitations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d invitation rows left after deleting their message", n)
	}
}

func missingIDs(t *testing.T, db *DB, account string, source InvitationSource, exclude []int64, limit int) []int64 {
	t.Helper()
	missing, err := db.MissingInvitations(account, source, exclude, limit)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, m := range missing {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestMissingInvitations(t *testing.T) {
	db := newTestDB(t)
	older := seedCalMessage(t, db, "older@x", "work", 100, 7, "", true)
	newer := seedCalMessage(t, db, "newer@x", "work", 300, 9, "", true)
	middle := seedCalMessage(t, db, "middle@x", "work", 200, 8, "", true)
	looked := seedCalMessage(t, db, "looked@x", "work", 400, 10, "", true)
	seedCalMessage(t, db, "plain@x", "work", 500, 11, "", false) // not calendar mail
	seedCalMessage(t, db, "other@x", "home", 600, 12, "", true)  // another account
	seedCalMessage(t, db, "nouid@x", "work", 700, 0, "", true)   // not fetchable by UID
	byRef := seedCalMessage(t, db, "engine@x", "work", 800, 0, "AAMk1", true)
	if err := db.SetInvitation(looked, ""); err != nil {
		t.Fatal(err)
	}

	if got, want := missingIDs(t, db, "work", ByUID, nil, 10), []int64{newer, middle, older}; !reflect.DeepEqual(got, want) {
		t.Errorf("by UID = %v, want newest first %v", got, want)
	}
	if got, want := missingIDs(t, db, "work", ByUID, nil, 2), []int64{newer, middle}; !reflect.DeepEqual(got, want) {
		t.Errorf("by UID, limit 2 = %v, want %v", got, want)
	}
	if got, want := missingIDs(t, db, "work", ByUID, []int64{newer}, 10), []int64{middle, older}; !reflect.DeepEqual(got, want) {
		t.Errorf("by UID without the failed one = %v, want %v", got, want)
	}
	if got, want := missingIDs(t, db, "work", ByRemoteRef, nil, 10), []int64{byRef}; !reflect.DeepEqual(got, want) {
		t.Errorf("by remote ref = %v, want %v", got, want)
	}
	if got := missingIDs(t, db, "nobody", ByUID, nil, 10); len(got) != 0 {
		t.Errorf("unknown account = %v, want none", got)
	}

	missing, err := db.MissingInvitations("work", ByUID, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := (MissingInvitation{ID: newer, Mailbox: "INBOX", UID: 9}); len(missing) != 1 || missing[0] != want {
		t.Errorf("MissingInvitations = %+v, want %+v", missing, want)
	}
}

// Every sync asks for missing invitations, also long after it caught up, so
// the question must stay cheap in a big mailbox.
// go test ./internal/store -run '^$' -bench MissingInvitations
func BenchmarkMissingInvitations(b *testing.B) {
	db, _, err := seedTagBench(60000, 1000)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.db.Exec(`INSERT INTO tags (message_id, tag) SELECT id, ? FROM messages ORDER BY date DESC LIMIT 1500`, CalendarTag); err != nil {
		b.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE messages SET remote_ref = 'ref-' || id`); err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		looked string // which calendar mail already has its invitation row
	}{
		{"caught up", `SELECT message_id FROM tags WHERE tag = 'cal'`},
		{"after upgrade", `SELECT message_id FROM tags WHERE 0`},
	} {
		if _, err := db.db.Exec(`DELETE FROM message_invitations`); err != nil {
			b.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO message_invitations (message_db_id) ` + c.looked); err != nil {
			b.Fatal(err)
		}
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := db.MissingInvitations("main", ByRemoteRef, nil, 50); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The calendar tag must drive the query, not the account: an account's index
// walks its whole mailbox on every sync (18 ms at 60k messages against 0.9).
func TestMissingInvitations_DrivenByTheTag(t *testing.T) {
	db := newTestDB(t)
	seedCalMessage(t, db, "a@x", "work", 1, 1, "ref", true)
	for _, source := range []InvitationSource{ByUID, ByRemoteRef} {
		q, params := missingInvitationsQuery("work", source, []int64{1}, 50)
		rows, err := db.db.Query("EXPLAIN QUERY PLAN "+q, params...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "\n")
		if !strings.Contains(plan[0], "SEARCH t USING") || strings.Contains(joined, "idx_messages_account_id") {
			t.Errorf("source %d: plan doesn't start from the tag:\n%s", source, joined)
		}
	}
}
