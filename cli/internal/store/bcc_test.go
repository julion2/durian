package store

import (
	"bytes"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	internmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/smtp"
)

// TestBccSurvivesTheDraftRoundTrip pins the contract a draft's blind
// recipients have to satisfy: whatever the user typed into the Bcc field
// must still be on the wire after the draft has been saved, read back, and
// saved a second time.
//
// The trip is the real one, layer by layer — BuildDraft writes the RFC822
// that gets appended to the Drafts mailbox, the parser reads that image back
// the way sync does, the store persists it, and a reopen rebuilds the
// message. Every one of those layers used to drop Bcc silently: the header
// went out, nothing read it back, and the second save mailed the message to
// fewer people than the user had addressed.
func TestBccSurvivesTheDraftRoundTrip(t *testing.T) {
	db := newTestDB(t)

	const (
		blindOne = "blind-one@example.com"
		blindTwo = "blind-two@example.com"
		visible  = "visible@example.com"
	)

	original := &smtp.Message{
		From:    "author@example.com",
		To:      []string{"to@example.com"},
		CC:      []string{visible},
		BCC:     []string{blindOne, blindTwo},
		Subject: "Quarterly numbers",
		Body:    "See attached.",
	}

	// 1. Save: the draft image that goes into the Drafts mailbox.
	raw, err := original.BuildDraft()
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}

	// 2. Read back the way sync does.
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read draft message: %v", err)
	}
	content := internmail.NewParser().Parse(parsed)
	if !strings.Contains(content.BCC, blindOne) || !strings.Contains(content.BCC, blindTwo) {
		t.Fatalf("parser lost the blind recipients: BCC = %q", content.BCC)
	}

	// 3. Persist.
	now := time.Now().Unix()
	if err := db.InsertMessage(&Message{
		MessageID:   content.MessageID,
		Account:     "work",
		Subject:     content.Subject,
		FromAddr:    content.From,
		ToAddrs:     content.To,
		CCAddrs:     content.CC,
		BCCAddrs:    content.BCC,
		Date:        now,
		CreatedAt:   now,
		FetchedBody: true,
	}); err != nil {
		t.Fatalf("insert draft: %v", err)
	}

	// 4. Reopen.
	stored, err := db.GetByMessageID(content.MessageID)
	if err != nil {
		t.Fatalf("reopen draft: %v", err)
	}
	if !strings.Contains(stored.BCCAddrs, blindOne) || !strings.Contains(stored.BCCAddrs, blindTwo) {
		t.Fatalf("store lost the blind recipients: BCCAddrs = %q", stored.BCCAddrs)
	}

	// 5. Save again — the assertion the user actually feels.
	reopened := &smtp.Message{
		From:    stored.FromAddr,
		To:      splitAddrList(stored.ToAddrs),
		CC:      splitAddrList(stored.CCAddrs),
		BCC:     splitAddrList(stored.BCCAddrs),
		Subject: stored.Subject,
		Body:    stored.BodyText,
	}
	second, err := reopened.BuildDraft()
	if err != nil {
		t.Fatalf("rebuild draft: %v", err)
	}
	for _, want := range []string{blindOne, blindTwo} {
		if !bytes.Contains(second, []byte(want)) {
			t.Errorf("second save dropped blind recipient %q:\n%s", want, second)
		}
	}
}

// TestBccIsNeverPlaintextOnDisk is the reason Bcc gets its own encrypted
// column instead of joining to_addrs/cc_addrs. Those two stay plaintext by
// the ADR-0001 §3 revision, justified by the addresses travelling on the
// wire anyway. Blind recipients are the one class of address that
// deliberately does not, so that justification does not reach them.
//
// The To and Cc assertions are not decoration: they prove the byte scan
// finds an address that really is in the file, so the Bcc assertion is a
// statement about Bcc and not about a scan that never matches anything.
func TestBccIsNeverPlaintextOnDisk(t *testing.T) {
	const (
		blind   = "blind-recipient@example.com"
		visible = "cc-recipient@example.com"
		to      = "to-recipient@example.com"
	)

	dbPath := filepath.Join(t.TempDir(), "bcc-at-rest.db")
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := Open(dbPath, kr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	now := time.Now().Unix()
	if err := db.InsertMessage(&Message{
		MessageID:   "at-rest@example.com",
		Account:     "work",
		Subject:     "Quarterly numbers",
		FromAddr:    "author@example.com",
		ToAddrs:     to,
		CCAddrs:     visible,
		BCCAddrs:    blind,
		Date:        now,
		CreatedAt:   now,
		FetchedBody: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if bytes.Contains(raw, []byte(blind)) {
		t.Errorf("blind recipient %q is readable in the database file", blind)
	}
	for _, want := range []string{to, visible} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("scan found no plaintext %q, so the Bcc assertion proves nothing", want)
		}
	}
}

// TestBccSurvivesAMetadataOnlyRedelivery covers the upsert rule. A delta that
// re-delivers a message without its body (a flag change, say) arrives with
// every content field empty. Letting those empties win would erase the blind
// recipients of a draft the user has not touched — the same silent loss the
// round trip above is about, only triggered by a background sync instead of
// by the user.
func TestBccSurvivesAMetadataOnlyRedelivery(t *testing.T) {
	db := newTestDB(t)

	const blind = "blind@example.com"
	const messageID = "redelivered@example.com"
	now := time.Now().Unix()

	if err := db.InsertMessage(&Message{
		MessageID:   messageID,
		Account:     "work",
		Subject:     "Draft",
		FromAddr:    "author@example.com",
		BCCAddrs:    blind,
		Date:        now,
		CreatedAt:   now,
		FetchedBody: true,
	}); err != nil {
		t.Fatalf("insert draft: %v", err)
	}

	// The metadata-only redelivery: no body, and therefore no Bcc either.
	if err := db.InsertMessage(&Message{
		MessageID:   messageID,
		Account:     "work",
		Subject:     "Draft",
		FromAddr:    "author@example.com",
		Date:        now,
		CreatedAt:   now,
		FetchedBody: false,
	}); err != nil {
		t.Fatalf("redeliver: %v", err)
	}

	stored, err := db.GetByMessageID(messageID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.BCCAddrs != blind {
		t.Errorf("BCCAddrs = %q, want %q — a metadata-only sync erased the blind recipients", stored.BCCAddrs, blind)
	}
}

// splitAddrList reverses the ", " join the store uses for address lists.
func splitAddrList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
