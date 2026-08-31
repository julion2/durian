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

	matcher, err := New(db, "work", "INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := matcher.Match("", content, 100); got != "durian-synthetic-2-INBOX@work" {
		t.Fatalf("first duplicate matched %q", got)
	}
	matcher.Restore("durian-synthetic-2-INBOX@work")
	if got, _ := matcher.Match("", content, 100); got != "durian-synthetic-2-INBOX@work" {
		t.Fatalf("restored duplicate matched %q", got)
	}
	matcher.Commit("durian-synthetic-2-INBOX@work")
	if got, _ := matcher.Match("", content, 100); got != "durian-synthetic-1-INBOX@work" {
		t.Fatalf("second duplicate matched %q", got)
	}
	if got, _ := matcher.Match("", content, 100); got != "" {
		t.Fatalf("consumed candidate matched again as %q", got)
	}
}

func TestMatcherPinsCurrentEpochCopyToProvisionalID(t *testing.T) {
	db := newTestDB(t)
	content := &durianmail.MailContent{From: "notifier@example.com", Subject: "Alert", Body: "same body"}
	for _, messageID := range []string{
		"durian-synthetic-v2-10-4-INBOX@work",
		"durian-synthetic-v2-10-3-INBOX@work",
		"durian-synthetic-v2-99-7-INBOX@work",
	} {
		if err := db.InsertMessage(&store.Message{
			MessageID: messageID, FromAddr: content.From, Subject: content.Subject,
			BodyText: content.Body, Date: 100, CreatedAt: 100, Mailbox: "INBOX",
			Account: "work", FetchedBody: true, SyntheticIdentity: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	matcher, err := New(db, "work", "INBOX", 99)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		"durian-synthetic-v2-99-9-INBOX@work": "durian-synthetic-v2-10-4-INBOX@work",
		"durian-synthetic-v2-99-8-INBOX@work": "durian-synthetic-v2-10-3-INBOX@work",
		"durian-synthetic-v2-99-7-INBOX@work": "durian-synthetic-v2-99-7-INBOX@work",
	}
	for _, provisional := range []string{
		"durian-synthetic-v2-99-9-INBOX@work",
		"durian-synthetic-v2-99-8-INBOX@work",
		"durian-synthetic-v2-99-7-INBOX@work",
	} {
		if got, _ := matcher.Match(provisional, content, 100); got != wants[provisional] {
			t.Fatalf("match %s = %q, want %q", provisional, got, wants[provisional])
		}
		matcher.Commit(wants[provisional])
	}

	matcher, err = New(db, "work", "INBOX", 99)
	if err != nil {
		t.Fatal(err)
	}
	const current = "durian-synthetic-v2-99-7-INBOX@work"
	if got, complete := matcher.Match(current, content, 100); got != current || !complete {
		t.Fatalf("current retry match = %q, complete=%t, want %q complete", got, complete, current)
	}
	matcher.Restore(current)
	if got, complete := matcher.Match(current, content, 100); got != current || !complete {
		t.Fatalf("restored current retry match = %q, complete=%t, want %q complete", got, complete, current)
	}
}

func TestMatcherReportsPendingCurrentEpochCopy(t *testing.T) {
	db := newTestDB(t)
	const current = "durian-synthetic-v2-99-7-INBOX@work"
	content := &durianmail.MailContent{From: "notifier@example.com", Subject: "Alert", Body: "same body"}
	if err := db.InsertMessage(&store.Message{
		MessageID: current, FromAddr: content.From, Subject: content.Subject,
		BodyText: content.Body, Date: 100, CreatedAt: 100, Mailbox: "INBOX",
		Account: "work", FetchedBody: true, SyntheticIdentity: true, IngestPending: true,
	}); err != nil {
		t.Fatal(err)
	}
	matcher, err := New(db, "work", "INBOX", 99)
	if err != nil {
		t.Fatal(err)
	}
	if got, complete := matcher.Match(current, content, 100); got != current || complete {
		t.Fatalf("pending current match = %q, complete=%t, want %q incomplete", got, complete, current)
	}
}

func TestMatcherUsesDurableFingerprintWhenPendingAttachmentsAreMissing(t *testing.T) {
	db := newTestDB(t)
	const oldID = "durian-synthetic-v2-10-4-INBOX@work"
	content := &durianmail.MailContent{
		From: "notifier@example.com", Subject: "Alert", Body: "same body",
		Attachments: []durianmail.AttachmentInfo{{Filename: "report.pdf", ContentType: "application/pdf", Size: 10}},
	}
	fingerprint := durianmail.SyntheticFingerprint(content, 100)
	if err := db.InsertMessage(&store.Message{
		MessageID: oldID, FromAddr: content.From, Subject: content.Subject,
		BodyText: content.Body, Date: 100, CreatedAt: 100, Mailbox: "INBOX",
		Account: "work", FetchedBody: true, SyntheticIdentity: true,
		SyntheticFingerprint: fingerprint[:], IngestPending: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately do not insert the attachment: this is the durable state left
	// by a failure after the core upsert but before attachment enrichment.
	matcher, err := New(db, "work", "INBOX", 99)
	if err != nil {
		t.Fatal(err)
	}
	provisional := "durian-synthetic-v2-99-9-INBOX@work"
	if got, complete := matcher.Match(provisional, content, 100); got != oldID || complete {
		t.Fatalf("pending attachment match = %q, complete=%t, want %q incomplete", got, complete, oldID)
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
	matcher, err := New(db, "work", "INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	content := &durianmail.MailContent{Subject: "target"}
	if got, _ := matcher.Match("", content, 1); got != "durian-synthetic-1-INBOX@work" {
		t.Fatalf("scoped match = %q", got)
	}
	if got, _ := matcher.Match("", content, 1); got != "" {
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
	matcher, err := New(db, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := matcher.Match("", &durianmail.MailContent{Subject: "target"}, 1); got != msg.MessageID {
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
	matcher, err := New(db, "work", "INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Message-ID: <durian-synthetic-99-INBOX@work>\r\nSubject: target\r\n\r\n")
	got, _, err := matcher.MatchRaw("durian-synthetic-v2-99-1-INBOX@work", raw, time.Unix(1, 0))
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
