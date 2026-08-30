package imapbackend

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/redact"
)

func TestCursorRoundTrip(t *testing.T) {
	state := &imap.MailboxState{
		UIDValidity:    42,
		LastUID:        7,
		SyncedUIDs:     []uint32{5, 7},
		UIDToMessageID: map[uint32]string{5: "five@example.test", 7: "seven@example.test"},
	}

	for _, replacement := range []bool{false, true} {
		cursor, err := encodeCursor(state, replacement)
		if err != nil {
			t.Fatalf("encodeCursor(replacement=%v): %v", replacement, err)
		}
		got, gotReplacement, err := decodeCursor(cursor)
		if err != nil {
			t.Fatalf("decodeCursor(replacement=%v): %v", replacement, err)
		}
		if got.UIDValidity != state.UIDValidity || got.LastUID != state.LastUID || len(got.SyncedUIDs) != len(state.SyncedUIDs) {
			t.Fatalf("decoded state = %+v, want %+v", got, state)
		}
		if gotReplacement != replacement {
			t.Fatalf("replacement = %v, want %v", gotReplacement, replacement)
		}
	}
}

func TestCompletedReplacementCursorUsesLegacyFormat(t *testing.T) {
	state := &imap.MailboxState{UIDValidity: 42, SyncedUIDs: []uint32{1}}
	cursor, err := encodeCursor(state, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cursor, []byte(`"full_replacement"`)) || bytes.Contains(cursor, []byte(`"state"`)) {
		t.Fatalf("completed cursor unexpectedly uses replacement envelope: %s", cursor)
	}

	decoded, replacement, err := decodeCursor(backend.Cursor(`{"uid_validity":42,"synced_uids":[1]}`))
	if err != nil {
		t.Fatalf("decode legacy cursor: %v", err)
	}
	if replacement || decoded.UIDValidity != 42 || len(decoded.SyncedUIDs) != 1 {
		t.Fatalf("decoded legacy cursor = %+v, replacement=%v", decoded, replacement)
	}
}

func TestReconnectFailureRedactsBothProviderResponses(t *testing.T) {
	const firstResponse = "first short IMAP provider response"
	const reconnectResponse = "second short IMAP provider response"
	firstRaw := errors.New(firstResponse)
	first := redact.ExternalError(firstRaw, "IMAP operation failed: server response "+redact.Placeholder)
	reconnect := redact.ExternalError(errors.New(reconnectResponse), "IMAP reconnect failed: server response "+redact.Placeholder)
	err := reconnectFailure(first, reconnect)

	wantError := firstResponse + " (reconnect failed: " + reconnectResponse + ")"
	if err.Error() != wantError {
		t.Fatalf("Error() = %q, want %q", err.Error(), wantError)
	}
	if !errors.Is(err, firstRaw) {
		t.Fatal("reconnect aggregate broke the original operation error chain")
	}

	var logOutput bytes.Buffer
	logger := slog.New(redact.Wrap(slog.NewTextHandler(&logOutput, nil)))
	logger.Error("IMAP operation failed", "err", err)
	for _, response := range []string{firstResponse, reconnectResponse} {
		if strings.Contains(logOutput.String(), response) {
			t.Errorf("provider response %q leaked into log:\n%s", response, logOutput.String())
		}
	}
	if !strings.Contains(logOutput.String(), redact.Placeholder) {
		t.Fatalf("redaction marker missing from log:\n%s", logOutput.String())
	}
}
