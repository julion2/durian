package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
)

func TestPublicAttachmentsExcludeStoreIDs(t *testing.T) {
	data, err := json.Marshal(publicAttachments([]store.Attachment{{
		ID: 99, MessageDBID: 42, PartID: 2, Filename: "report.pdf", ContentType: "application/pdf", Size: 12,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"id"`) || strings.Contains(text, "message_db_id") {
		t.Fatalf("public attachment leaks store IDs: %s", text)
	}
	if !strings.Contains(text, `"part_id":2`) {
		t.Fatalf("public attachment missing part ID: %s", text)
	}
}

func TestPublicContactsExcludeStoreIDsAndUseRFC3339(t *testing.T) {
	when := time.Date(2026, 8, 27, 12, 30, 0, 0, time.UTC)
	data, err := json.Marshal(publicContacts([]contacts.Contact{{ID: "internal", Email: "a@example.com", LastUsed: when}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "internal") || !strings.Contains(text, when.Format(time.RFC3339)) {
		t.Fatalf("unexpected public contact JSON: %s", text)
	}
}

func TestOutputSearchJSONUsesEmptyArray(t *testing.T) {
	results := protocol.SuccessWithResults(nil)
	if encoded, err := json.Marshal(publicSearchResults(results)); err != nil || string(encoded) != "[]" {
		t.Fatalf("empty result JSON = %s, %v", encoded, err)
	}
}

// TestJSONOutputKeepsRemoteValuesVerbatim is the other half of the terminal
// escaping. That escaping exists for a terminal; a JSON consumer needs the
// value the provider actually sent, and rewriting it would corrupt any
// round-trip through the API — the value would no longer match the one the
// server holds.
//
// The cases are the fields the human renderer now escapes: Message-ID, tags,
// iCal UID, and the attachment filename. Asserting only the filename would
// leave the three that changed unguarded.
func TestJSONOutputKeepsRemoteValuesVerbatim(t *testing.T) {
	const hostile = "\x1b]0;pwned\x07‮"

	t.Run("attachment filename", func(t *testing.T) {
		name := "report" + hostile + ".pdf"
		encoded, err := json.Marshal(publicAttachments([]store.Attachment{{
			PartID: 1, Filename: name, ContentType: "application/pdf", Size: 10,
		}}))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded []struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(decoded) != 1 || decoded[0].Filename != name {
			t.Errorf("filename = %+v, want the value verbatim (%q)", decoded, name)
		}
		assertNoTerminalMarker(t, encoded)
	})

	t.Run("message id and tags", func(t *testing.T) {
		info := mail.MessageInfo{
			MessageID: "evil" + hostile + "@example.com",
			Tags:      []string{"inbox", "label" + hostile},
		}
		encoded, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded mail.MessageInfo
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.MessageID != info.MessageID {
			t.Errorf("message id = %q, want %q", decoded.MessageID, info.MessageID)
		}
		if !slices.Equal(decoded.Tags, info.Tags) {
			t.Errorf("tags = %v, want %v", decoded.Tags, info.Tags)
		}
		assertNoTerminalMarker(t, encoded)
	})

	t.Run("ical uid", func(t *testing.T) {
		// Through ToCalendarEvent, which is what the CLI actually marshals.
		// Asserting calendar.Event would leave the projection itself
		// unguarded — that is where a stray sanitization would sit.
		uid := "uid" + hostile + "@example.com"
		dto := calendar.ToCalendarEvent("Work", calendar.Event{
			ICalUID: uid,
			Subject: "Standup",
		}, true)

		encoded, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded calendar.CalendarEvent
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.UID != uid {
			t.Errorf("uid = %q, want %q", decoded.UID, uid)
		}
		assertNoTerminalMarker(t, encoded)
	})
}

// assertNoTerminalMarker fails when the human renderer's escape marker appears
// in machine-readable output, which would mean the escaping had leaked out of
// the terminal path.
func assertNoTerminalMarker(t *testing.T, encoded []byte) {
	t.Helper()
	if strings.Contains(string(encoded), "U+001B") || strings.Contains(string(encoded), "U+202E") {
		t.Errorf("JSON carries a terminal escape marker: %s", encoded)
	}
}
