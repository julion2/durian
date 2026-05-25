package store

import (
	"bytes"
	"strings"
	"testing"
)

// TestAttachmentMetadata_EncryptedAtRest asserts ADR-0001 attachments-
// encryption: filename, content_type, and size never appear as their
// distinctive plaintext bytes in the raw filename_ct / content_type_ct
// / size_ct BLOB columns. A forensic analyst opening the .db file
// must not be able to grep for "Tax_Return_2026.pdf" and find it.
func TestAttachmentMetadata_EncryptedAtRest(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "atrest@x")

	const distinctiveFilename = "Tax_Return_2026.pdf"
	const distinctiveContentType = "application/dicom"
	const distinctiveSize = 314159

	if err := db.InsertAttachment(&Attachment{
		MessageDBID: msgID,
		PartID:      1,
		Filename:    distinctiveFilename,
		ContentType: distinctiveContentType,
		Size:        distinctiveSize,
		Disposition: "attachment",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read the raw ct bytes for the row and assert none of them
	// contain the plaintext substrings.
	var filenameCT, contentTypeCT, sizeCT []byte
	if err := db.db.QueryRow(`SELECT filename_ct, content_type_ct, size_ct
		FROM attachments WHERE message_db_id = ?`, msgID).Scan(&filenameCT, &contentTypeCT, &sizeCT); err != nil {
		t.Fatalf("read raw ct: %v", err)
	}
	if bytes.Contains(filenameCT, []byte(distinctiveFilename)) {
		t.Errorf("filename leaked into filename_ct ciphertext bytes")
	}
	if bytes.Contains(contentTypeCT, []byte(distinctiveContentType)) {
		t.Errorf("content_type leaked into content_type_ct ciphertext bytes")
	}
	if bytes.Contains(sizeCT, []byte("314159")) {
		t.Errorf("size leaked into size_ct ciphertext bytes")
	}

	// Read-through-API decrypts and returns the plaintext correctly.
	atts, err := db.GetAttachmentsByMessage(msgID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(atts) != 1 || atts[0].Filename != distinctiveFilename || atts[0].ContentType != distinctiveContentType || atts[0].Size != distinctiveSize {
		t.Errorf("round-trip failed: got %+v", atts)
	}

	// Plaintext columns must not exist after the v21→v22 migration.
	for _, col := range []string{"filename", "content_type", "size"} {
		has, err := hasColumn(db.db, "attachments", col)
		if err != nil {
			t.Fatalf("hasColumn: %v", err)
		}
		if has {
			t.Errorf("plaintext column %q still exists post-migration", col)
		}
	}
	// Sanity: the only attachment columns left are the structural /
	// low-entropy ones documented as plaintext-by-design.
	cols, err := db.db.Query(`SELECT name FROM pragma_table_info('attachments')`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer cols.Close()
	var present []string
	for cols.Next() {
		var n string
		cols.Scan(&n)
		present = append(present, n)
	}
	if strings.Contains(strings.Join(present, ","), "filename,") || strings.Contains(strings.Join(present, ","), ",size,") {
		t.Errorf("post-migration attachments table still carries plaintext columns: %v", present)
	}
}

func TestInsertAndGetAttachments(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "att@x")

	att := &Attachment{
		MessageDBID: msgID,
		PartID:      1,
		Filename:    "report.pdf",
		ContentType: "application/pdf",
		Size:        1024,
		Disposition: "attachment",
	}
	if err := db.InsertAttachment(att); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if att.ID == 0 {
		t.Error("expected non-zero ID after insert")
	}

	atts, err := db.GetAttachmentsByMessage(msgID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("got %d attachments, want 1", len(atts))
	}
	if atts[0].Filename != "report.pdf" {
		t.Errorf("Filename = %q, want %q", atts[0].Filename, "report.pdf")
	}
	if atts[0].ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", atts[0].ContentType)
	}
	if atts[0].Size != 1024 {
		t.Errorf("Size = %d, want 1024", atts[0].Size)
	}
}

func TestGetAttachmentsByMessageID(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "att-mid@x")

	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 1,
		Filename: "doc.txt", ContentType: "text/plain",
		Size: 100, Disposition: "attachment",
	})

	atts, err := db.GetAttachmentsByMessageID("att-mid@x")
	if err != nil {
		t.Fatalf("get by message ID: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("got %d, want 1", len(atts))
	}
	if atts[0].Filename != "doc.txt" {
		t.Errorf("Filename = %q", atts[0].Filename)
	}
}

func TestGetAttachmentsByMessageIDNotFound(t *testing.T) {
	db := newTestDB(t)

	atts, err := db.GetAttachmentsByMessageID("nonexistent@x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(atts) != 0 {
		t.Errorf("got %d attachments for nonexistent message", len(atts))
	}
}

func TestMultipleAttachments(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "multi-att@x")

	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 1,
		Filename: "a.pdf", ContentType: "application/pdf",
		Size: 100, Disposition: "attachment",
	})
	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 2,
		Filename: "b.png", ContentType: "image/png",
		Size: 200, Disposition: "inline", ContentID: "cid:img1",
	})

	atts, _ := db.GetAttachmentsByMessage(msgID)
	if len(atts) != 2 {
		t.Fatalf("got %d, want 2", len(atts))
	}
	// Ordered by part_id
	if atts[0].PartID != 1 || atts[1].PartID != 2 {
		t.Error("attachments not ordered by part_id")
	}
	if atts[1].ContentID != "cid:img1" {
		t.Errorf("ContentID = %q, want %q", atts[1].ContentID, "cid:img1")
	}
	if atts[1].Disposition != "inline" {
		t.Errorf("Disposition = %q, want %q", atts[1].Disposition, "inline")
	}
}

func TestDeleteAttachmentsByMessageDBID(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "del-att@x")

	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 1,
		Filename: "x.txt", ContentType: "text/plain",
	})
	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 2,
		Filename: "y.txt", ContentType: "text/plain",
	})

	err := db.DeleteAttachmentsByMessageDBID(msgID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	atts, _ := db.GetAttachmentsByMessage(msgID)
	if len(atts) != 0 {
		t.Errorf("got %d attachments after delete, want 0", len(atts))
	}
}

func TestDeleteAttachmentsCascade(t *testing.T) {
	db := newTestDB(t)
	msgID := insertTestMessage(t, db, "cascade@x")

	db.InsertAttachment(&Attachment{
		MessageDBID: msgID, PartID: 1,
		Filename: "z.txt", ContentType: "text/plain",
	})

	// Delete the message — attachments should cascade
	db.DeleteByMessageID("cascade@x")

	atts, _ := db.GetAttachmentsByMessage(msgID)
	if len(atts) != 0 {
		t.Errorf("attachments should be cascade-deleted, got %d", len(atts))
	}
}

func TestAttachmentCounts(t *testing.T) {
	db := newTestDB(t)
	id1 := insertTestMessage(t, db, "count1@x")
	id2 := insertTestMessage(t, db, "count2@x")
	insertTestMessage(t, db, "count3@x") // no attachments

	db.InsertAttachment(&Attachment{MessageDBID: id1, PartID: 1, Filename: "a.pdf"})
	db.InsertAttachment(&Attachment{MessageDBID: id1, PartID: 2, Filename: "b.pdf"})
	db.InsertAttachment(&Attachment{MessageDBID: id2, PartID: 1, Filename: "c.pdf"})

	counts, err := db.AttachmentCounts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts[id1] != 2 {
		t.Errorf("msg1 count = %d, want 2", counts[id1])
	}
	if counts[id2] != 1 {
		t.Errorf("msg2 count = %d, want 1", counts[id2])
	}
}
