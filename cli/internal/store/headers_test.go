package store

import (
	"testing"
)

func TestInsertAndGetHeader(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "hdr@x")

	err := db.InsertHeader(msgID, "list-id", "<dev.example.com>")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	val, err := db.GetHeader(msgID, "list-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val != "<dev.example.com>" {
		t.Errorf("value = %q, want %q", val, "<dev.example.com>")
	}
}

func TestGetHeaderNotFound(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "hdr-nf@x")

	val, err := db.GetHeader(msgID, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "" {
		t.Errorf("expected empty string, got %q", val)
	}
}

func TestInsertHeaderUpsert(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "hdr-ups@x")

	db.InsertHeader(msgID, "list-id", "old")
	db.InsertHeader(msgID, "list-id", "new")

	val, _ := db.GetHeader(msgID, "list-id")
	if val != "new" {
		t.Errorf("value = %q, want %q (upsert)", val, "new")
	}
}

func TestHasHeaders(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "has-hdr@x")

	has, err := db.HasHeaders(msgID)
	if err != nil {
		t.Fatalf("has headers: %v", err)
	}
	if has {
		t.Error("should not have headers yet")
	}

	db.InsertHeader(msgID, "x-mailer", "durian")

	has, _ = db.HasHeaders(msgID)
	if !has {
		t.Error("should have headers after insert")
	}
}

func TestHasHeaderDistinguishesIndexedEmptyValue(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "empty-reply-to@x")

	if has, err := db.HasHeader(msgID, "reply-to"); err != nil || has {
		t.Fatalf("HasHeader before insert = %v, %v", has, err)
	}
	if err := db.InsertHeader(msgID, "reply-to", ""); err != nil {
		t.Fatal(err)
	}
	if has, err := db.HasHeader(msgID, "reply-to"); err != nil || !has {
		t.Fatalf("HasHeader after empty insert = %v, %v", has, err)
	}
}

func TestGetMessageDBID(t *testing.T) {
	db := newTestDB(t)
	insertTestMessage(t, db, "dbid@x")

	id, err := db.GetMessageDBID("dbid@x", "")
	if err != nil {
		t.Fatalf("get db id: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero DB ID")
	}
}

func TestGetMessageDBIDNotFound(t *testing.T) {
	db := newTestDB(t)

	id, err := db.GetMessageDBID("nonexistent@x", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 0 {
		t.Errorf("expected 0 for nonexistent, got %d", id)
	}
}

func TestAllHeadersByMessage(t *testing.T) {
	db := newTestDB(t)
	id1 := insertTestMessage(t, db, "allhdr1@x")
	id2 := insertTestMessage(t, db, "allhdr2@x")

	db.InsertHeader(id1, "list-id", "<dev@x>")
	db.InsertHeader(id1, "x-mailer", "durian")
	db.InsertHeader(id2, "precedence", "bulk")

	headers, err := db.AllHeadersByMessage()
	if err != nil {
		t.Fatalf("all headers: %v", err)
	}

	if len(headers[id1]) != 2 {
		t.Errorf("msg1 headers = %d, want 2", len(headers[id1]))
	}
	// Keys should be canonical MIME form
	if v := headers[id1]["List-Id"]; len(v) == 0 || v[0] != "<dev@x>" {
		t.Errorf("List-Id = %v", headers[id1]["List-Id"])
	}
	if v := headers[id1]["X-Mailer"]; len(v) == 0 || v[0] != "durian" {
		t.Errorf("X-Mailer = %v", headers[id1]["X-Mailer"])
	}
	if v := headers[id2]["Precedence"]; len(v) == 0 || v[0] != "bulk" {
		t.Errorf("Precedence = %v", headers[id2]["Precedence"])
	}
}

func TestHeadersCascadeOnDelete(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "hdr-casc@x")

	db.InsertHeader(msgID, "list-id", "<test>")

	has, _ := db.HasHeaders(msgID)
	if !has {
		t.Fatal("should have headers before delete")
	}

	db.DeleteByMessageID("hdr-casc@x")

	// Headers should be cascade-deleted
	has, _ = db.HasHeaders(msgID)
	if has {
		t.Error("headers should be cascade-deleted")
	}
}
