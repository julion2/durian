package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/contacts"
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
