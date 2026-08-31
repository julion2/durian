package handler

import (
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/store"
)

// TestConvertThread_BccOnlyInFullView pins where the blind recipients are
// allowed to appear. Reopening a draft for editing is the one caller that
// needs them, and that caller asks for the full view. Search enrichment uses
// the light view and would otherwise decrypt and ship Bcc on every hit that
// happens to be a draft.
//
// Both halves are load-bearing: the full assertion is what lets the GUI
// restore the field on a reopen, and the light assertion is what keeps blind
// recipients out of every other response.
func TestConvertThread_BccOnlyInFullView(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	const blind = "blind@example.com"
	seedThreadMessage(t, db, &store.Message{
		MessageID: "bcc-view@test", Subject: "Draft",
		FromAddr: "a@example.com",
		ToAddrs:  "to@example.com",
		BCCAddrs: blind,
		Date:     now, CreatedAt: now,
		BodyText: "plain body",
		Mailbox:  "Drafts",
	})

	m, _ := db.GetByMessageID("bcc-view@test")
	msgs, _ := db.GetByThread(m.ThreadID)
	h := New(db, nil)

	full := h.convertThread(m.ThreadID, msgs, false, nil, nil)
	if len(full.Messages) != 1 {
		t.Fatalf("full view: expected 1 message, got %d", len(full.Messages))
	}
	if full.Messages[0].BCC != blind {
		t.Errorf("full view BCC = %q, want %q — a reopen cannot restore what the API never sends",
			full.Messages[0].BCC, blind)
	}

	light := h.convertThread(m.ThreadID, msgs, true, nil, nil)
	if len(light.Messages) != 1 {
		t.Fatalf("light view: expected 1 message, got %d", len(light.Messages))
	}
	if light.Messages[0].BCC != "" {
		t.Errorf("light view BCC = %q, want empty — search enrichment must not ship blind recipients",
			light.Messages[0].BCC)
	}
}
