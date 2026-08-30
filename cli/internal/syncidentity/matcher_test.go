package syncidentity

import (
	"bytes"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

func TestMatcherKeepsEqualMessagesDistinct(t *testing.T) {
	db := newTestDB(t)
	content := &durianmail.MailContent{
		From: "notifier@example.com", Subject: "Alert", Body: "same body",
		Attachments: []durianmail.AttachmentInfo{{Filename: "report.pdf", ContentType: "application/pdf", Size: 10}},
	}
	for _, seed := range []struct {
		id  string
		ref string
	}{
		{id: "durian-synthetic-1-INBOX@work", ref: "900"},
		{id: "durian-synthetic-2-INBOX@work", ref: "1"},
	} {
		msg := &store.Message{
			MessageID: seed.id, FromAddr: content.From, Subject: content.Subject,
			BodyText: content.Body, Date: 100, CreatedAt: 100, Mailbox: "INBOX",
			Account: "work", RemoteRef: seed.ref, FetchedBody: true, SyntheticIdentity: true,
		}
		if err := db.InsertMessage(msg); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertAttachment(&store.Attachment{
			MessageDBID: msg.ID, PartID: 1, Filename: "report.pdf", ContentType: "application/pdf", Size: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := New(db, "work", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if got := matcher.Match(content, 100); got != "durian-synthetic-2-INBOX@work" {
		t.Fatalf("first duplicate matched %q", got)
	}
	matcher.Restore("durian-synthetic-2-INBOX@work")
	if got := matcher.Match(content, 100); got != "durian-synthetic-2-INBOX@work" {
		t.Fatalf("restored duplicate matched %q", got)
	}
	matcher.Commit("durian-synthetic-2-INBOX@work")
	if got := matcher.Match(content, 100); got != "durian-synthetic-1-INBOX@work" {
		t.Fatalf("second duplicate matched %q", got)
	}
	if got := matcher.Match(content, 100); got != "" {
		t.Fatalf("consumed candidate matched again as %q", got)
	}
}

func TestMatcherScopesCandidatesToAccountAndMailbox(t *testing.T) {
	db := newTestDB(t)
	for _, msg := range []*store.Message{
		{MessageID: "durian-synthetic-1-INBOX@work", Mailbox: "INBOX", Account: "work", Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true},
		{MessageID: "durian-synthetic-2-Archive@work", Mailbox: "Archive", Account: "work", Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true},
		{MessageID: "durian-synthetic-3-INBOX@personal", Mailbox: "INBOX", Account: "personal", Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true},
		{MessageID: "durian-synthetic-alerts@example.com", Mailbox: "INBOX", Account: "work", Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true},
		// Exact grammar is still not provenance: this row represents a real
		// sender-supplied Message-ID and must never enter the candidate set.
		{MessageID: "durian-synthetic-4-INBOX@work", Mailbox: "INBOX", Account: "work", Subject: "target", Date: 1, CreatedAt: 1},
		{MessageID: "real@example.com", Mailbox: "INBOX", Account: "work", Subject: "target", Date: 1, CreatedAt: 1},
	} {
		if err := db.InsertMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	matcher, err := New(db, "work", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	content := &durianmail.MailContent{Subject: "target"}
	if got := matcher.Match(content, 1); got != "durian-synthetic-1-INBOX@work" {
		t.Fatalf("scoped match = %q", got)
	}
	if got := matcher.Match(content, 1); got != "" {
		t.Fatalf("matcher included an out-of-scope row: %q", got)
	}
}

func TestMatcherIncludesAccountlessRows(t *testing.T) {
	db := newTestDB(t)
	msg := &store.Message{
		MessageID: "durian-synthetic-1-@", Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	matcher, err := New(db, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := matcher.Match(&durianmail.MailContent{Subject: "target"}, 1); got != msg.MessageID {
		t.Fatalf("accountless match = %q, want %q", got, msg.MessageID)
	}
}

func TestMatchRawRefusesMessageIDHeader(t *testing.T) {
	db := newTestDB(t)
	msg := &store.Message{
		MessageID: "durian-synthetic-1-INBOX@work", Mailbox: "INBOX", Account: "work",
		Subject: "target", Date: 1, CreatedAt: 1, SyntheticIdentity: true,
	}
	if err := db.InsertMessage(msg); err != nil {
		t.Fatal(err)
	}
	matcher, err := New(db, "work", "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Message-ID: <durian-synthetic-99-INBOX@work>\r\nSubject: target\r\n\r\n")
	got, err := matcher.MatchRaw(raw, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("matched message carrying a real Message-ID as %q", got)
	}
}

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	keyring, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(":memory:", keyring)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
