package imapbackend

import (
	"bytes"
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/imap"
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
