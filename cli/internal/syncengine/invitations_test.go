package syncengine

import (
	"context"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/store"
)

const testInviteICS = "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:plan@example.com\r\nSUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

// keptICS is the part as kept: the line break before a MIME boundary
// belongs to the boundary.
var keptICS = strings.TrimSuffix(testInviteICS, "\r\n")

// rawInvitation is an invitation as Outlook sends it: the calendar part is
// an alternative to the text, without a filename.
func rawInvitation(msgID string) []byte {
	return []byte("From: mara@example.com\r\nTo: " + testAccount + "\r\nSubject: Invitation: Planning\r\n" +
		"Message-ID: <" + msgID + ">\r\nDate: Mon, 20 Jul 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"alt\"\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nPlanning on Tuesday\r\n" +
		"--alt\r\nContent-Type: text/calendar; charset=utf-8; method=REQUEST\r\n\r\n" + testInviteICS +
		"--alt--\r\n")
}

// rawICSAttachment carries the invitation only as an attached .ics, without
// the words "text/calendar" anywhere.
func rawICSAttachment(msgID string) []byte {
	return []byte("From: mara@example.com\r\nTo: " + testAccount + "\r\nSubject: Planning\r\n" +
		"Message-ID: <" + msgID + ">\r\nDate: Mon, 20 Jul 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"mix\"\r\n\r\n" +
		"--mix\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSee the attachment\r\n" +
		"--mix\r\nContent-Type: application/ics; name=\"invite.ics\"\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\n\r\n" +
		testInviteICS + "--mix--\r\n")
}

func inboxBackend(messages ...backend.Message) *fakeBackend {
	folders := []backend.Folder{{Name: "INBOX", Role: backend.RoleInbox, Selectable: true}}
	return newFakeBackend(folders, map[string][]backend.FetchResult{
		"INBOX": {{Messages: messages, Cursor: backend.Cursor("c1")}},
	})
}

func invitationOf(t *testing.T, db *store.DB, messageID string) (ics string, looked bool) {
	t.Helper()
	m, err := db.GetByMessageID(messageID)
	if err != nil || m == nil {
		t.Fatalf("message %s: %v", messageID, err)
	}
	invs, err := db.InvitationsByMessages([]int64{m.ID})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := db.MissingInvitations(testAccount, store.ByRemoteRef, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	looked = true
	for _, mi := range missing {
		if mi.ID == m.ID {
			looked = false
		}
	}
	return invs[m.ID], looked
}

// Ingest keeps the invitation of new mail – and that a mail merely
// mentioning text/calendar has none – so sync never fetches them again.
func TestEngineIngestKeepsInvitations(t *testing.T) {
	db := newTestDB(t)
	fake := inboxBackend(
		backend.Message{MessageID: "invite@x", Ref: backend.RemoteRef{Folder: "INBOX", ID: "1"}, Raw: rawInvitation("invite@x")},
		backend.Message{MessageID: "mention@x", Ref: backend.RemoteRef{Folder: "INBOX", ID: "2"},
			Raw: rawMessage("mention@x", "a@example.com", testAccount, "Format", "please send it as text/calendar")},
		backend.Message{MessageID: "attached@x", Ref: backend.RemoteRef{Folder: "INBOX", ID: "3"}, Raw: rawICSAttachment("attached@x")},
	)
	if _, err := newTestEngine(db, newMemCursorStore()).Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	if ics, looked := invitationOf(t, db, "invite@x"); ics != keptICS || !looked {
		t.Errorf("invitation = %q (looked at: %v), want the calendar part", ics, looked)
	}
	if ics, looked := invitationOf(t, db, "mention@x"); ics != "" || !looked {
		t.Errorf("mention: invitation = %q (looked at: %v), want none, recorded", ics, looked)
	}
	if ics, looked := invitationOf(t, db, "attached@x"); ics != keptICS || !looked {
		t.Errorf("attached .ics: invitation = %q (looked at: %v), want the attachment", ics, looked)
	}
	if hits, err := db.Search("tag:cal", 10); err != nil || len(hits) != 3 {
		t.Errorf("tag:cal finds %d threads (err %v), want all three", len(hits), err)
	}
	if fake.bodyFetches != 0 {
		t.Errorf("%d body fetches for mail just ingested, want none", fake.bodyFetches)
	}
}

// seedOldInvitation stores calendar mail the way a sync before invitations
// were kept left it: tagged, no invitation row.
func seedOldInvitation(t *testing.T, db *store.DB, messageID, ref string) {
	t.Helper()
	m := &store.Message{MessageID: messageID, Subject: "Invitation: Planning", Date: 1, CreatedAt: 1,
		Mailbox: "INBOX", Account: testAccount, RemoteRef: ref, FetchedBody: true}
	if err := db.InsertMessage(m); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetByMessageID(messageID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"inbox", store.CalendarTag} {
		if err := db.AddTag(stored.ID, tag); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEngineCatchesUpOnOlderInvitations(t *testing.T) {
	db := newTestDB(t)
	seedOldInvitation(t, db, "old@x", "old-ref")
	fake := inboxBackend()
	fake.bodies = map[string][]byte{"old-ref": rawInvitation("old@x")}

	// an upload-only pass and a dry run leave it alone
	engine := New(Options{Store: db, Cursors: newMemCursorStore(), Account: testAccount, Mode: UploadOnly, Ingest: IngestOptions{Account: testAccount}})
	if _, err := engine.Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	dry := New(Options{Store: db, Cursors: newMemCursorStore(), Account: testAccount, DryRun: true, Ingest: IngestOptions{Account: testAccount}})
	if _, err := dry.Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	if fake.bodyFetches != 0 {
		t.Fatalf("%d body fetches in upload-only and dry-run passes, want none", fake.bodyFetches)
	}

	if _, err := newTestEngine(db, newMemCursorStore()).Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	if ics, looked := invitationOf(t, db, "old@x"); ics != keptICS || !looked {
		t.Fatalf("after a sync: invitation = %q (looked at: %v), want it caught up", ics, looked)
	}
	fetches := fake.bodyFetches
	if _, err := newTestEngine(db, newMemCursorStore()).Sync(context.Background(), fake); err != nil {
		t.Fatal(err)
	}
	if fake.bodyFetches != fetches {
		t.Errorf("a sync after catching up fetched %d bodies, want none", fake.bodyFetches-fetches)
	}
}

// A message the provider no longer serves is left for the next sync and
// doesn't fail this one.
func TestEngineInvitationCatchUpFailureIsQuiet(t *testing.T) {
	db := newTestDB(t)
	seedOldInvitation(t, db, "gone@x", "gone-ref")
	fake := inboxBackend()
	res, err := newTestEngine(db, newMemCursorStore()).Sync(context.Background(), fake)
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("sync = %v, errors %v; want a clean sync", err, res.Errors)
	}
	if _, looked := invitationOf(t, db, "gone@x"); looked {
		t.Error("a failed fetch must leave the message for the next sync")
	}
	if !strings.Contains(string(rawInvitation("x")), "text/calendar") {
		t.Fatal("fixture lost its calendar part")
	}
}
