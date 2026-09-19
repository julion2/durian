package syncengine

import (
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/smtp"
)

// TestIngestKeepsBlindRecipients covers the production edge between the parser
// and the store: Ingest is what sync actually calls, and it is where the
// MailContent -> store.Message mapping lives.
//
// The store-level round trip proves Bcc can be persisted; this proves the code
// path that runs in production actually persists it. Without this, deleting
// the BCCAddrs line from ingest.go leaves every other Bcc test green while
// real syncs drop the field.
func TestIngestKeepsBlindRecipients(t *testing.T) {
	db := newTestDB(t)

	const blind = "blind@example.com"
	draft := &smtp.Message{
		From:    testAccount,
		To:      []string{"to@example.com"},
		BCC:     []string{blind},
		Subject: "Draft with blind recipients",
		Body:    "body",
	}
	raw, err := draft.BuildDraft()
	if err != nil {
		t.Fatalf("build draft: %v", err)
	}
	messageID := strings.Trim(draft.GeneratedMessageID, "<>")

	msg := backend.Message{
		MessageID: messageID,
		Ref:       backend.RemoteRef{Folder: "Drafts", ID: "d1"},
		Raw:       raw,
	}
	if _, _, _, err := Ingest(db, msg, "Drafts", backend.RoleDrafts, IngestOptions{Account: testAccount}); err != nil {
		t.Fatalf("ingest draft: %v", err)
	}

	stored, err := db.GetByMessageID(messageID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(stored.BCCAddrs, blind) {
		t.Errorf("BCCAddrs = %q, want it to contain %q — sync dropped the blind recipients", stored.BCCAddrs, blind)
	}
}
