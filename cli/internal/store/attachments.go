package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// DeleteAttachmentsByMessageDBID removes all attachments for a message DB row.
func (d *DB) DeleteAttachmentsByMessageDBID(messageDBID int64) error {
	_, err := d.db.Exec("DELETE FROM attachments WHERE message_db_id = ?", messageDBID)
	if err != nil {
		return fmt.Errorf("delete attachments: %w", err)
	}
	return nil
}

// InsertAttachment inserts attachment metadata for a message.
func (d *DB) InsertAttachment(att *Attachment) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := d.insertAttachmentTx(tx, att); err != nil {
		return err
	}
	return tx.Commit()
}

// insertAttachmentTx inserts attachment metadata within an existing
// transaction. ADR-0001 attachments-encryption step: filename,
// content_type and size are encrypted with meta_key — the three columns
// that carry content-classification signal (filename "Tax_Return.pdf"
// is subject-grade, content_type "application/dicom" reveals medical
// context, size combined with from_addr + date is a sharp activity
// pattern). disposition / content_id / part_id stay plaintext (low
// entropy / opaque / structural).
func (d *DB) insertAttachmentTx(tx *sql.Tx, att *Attachment) error {
	filenameCT, err := d.encryptMeta(att.Filename)
	if err != nil {
		return fmt.Errorf("encrypt filename: %w", err)
	}
	contentTypeCT, err := d.encryptMeta(att.ContentType)
	if err != nil {
		return fmt.Errorf("encrypt content_type: %w", err)
	}
	// Size is an int64 — encrypt the decimal-string form so the
	// envelope is human-debuggable post-decrypt and the round-trip
	// stays pure ASCII through the meta_key path. 0 maps to empty
	// plaintext → NULL ct (no separate "encrypted zero" sentinel).
	var sizeStr string
	if att.Size != 0 {
		sizeStr = strconv.Itoa(att.Size)
	}
	sizeCT, err := d.encryptMeta(sizeStr)
	if err != nil {
		return fmt.Errorf("encrypt size: %w", err)
	}
	result, err := tx.Exec(`
		INSERT INTO attachments (message_db_id, part_id, filename_ct, content_type_ct, size_ct, disposition, content_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		att.MessageDBID, att.PartID, filenameCT, contentTypeCT,
		sizeCT, att.Disposition, att.ContentID)
	if err != nil {
		return fmt.Errorf("insert attachment: %w", err)
	}
	id, _ := result.LastInsertId()
	att.ID = id
	return nil
}

// attachmentSelectColumns is the canonical SELECT list for every
// attachments read site. Centralised so the column order tracks the
// scan helper below without per-call drift.
const attachmentSelectColumns = `id, message_db_id, part_id,
	filename_ct, content_type_ct, size_ct,
	disposition, content_id`

// scanAttachmentRow scans one row whose SELECT names attachmentSelectColumns
// (in order) and decrypts the three meta-key columns into the Attachment
// struct's plaintext fields.
func (d *DB) scanAttachmentRow(scan func(...any) error) (Attachment, error) {
	var a Attachment
	var filenameCT, contentTypeCT, sizeCT []byte
	if err := scan(&a.ID, &a.MessageDBID, &a.PartID,
		&filenameCT, &contentTypeCT, &sizeCT,
		&a.Disposition, &a.ContentID); err != nil {
		return a, err
	}
	var err error
	if a.Filename, err = d.decryptMeta("", filenameCT); err != nil {
		return a, fmt.Errorf("decrypt filename id=%d: %w", a.ID, err)
	}
	if a.ContentType, err = d.decryptMeta("", contentTypeCT); err != nil {
		return a, fmt.Errorf("decrypt content_type id=%d: %w", a.ID, err)
	}
	sizeStr, err := d.decryptMeta("", sizeCT)
	if err != nil {
		return a, fmt.Errorf("decrypt size id=%d: %w", a.ID, err)
	}
	if sizeStr != "" {
		if a.Size, err = strconv.Atoi(sizeStr); err != nil {
			return a, fmt.Errorf("parse size id=%d: %w", a.ID, err)
		}
	}
	return a, nil
}

// GetAttachmentsByMessages returns attachments for multiple messages in a single query.
// Returns map[messageDBID][]Attachment.
func (d *DB) GetAttachmentsByMessages(ids []int64) (map[int64][]Attachment, error) {
	if len(ids) == 0 {
		return make(map[int64][]Attachment), nil
	}

	placeholders := make([]string, len(ids))
	params := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		params[i] = id
	}

	q := "SELECT " + attachmentSelectColumns + " FROM attachments WHERE message_db_id IN (" +
		strings.Join(placeholders, ",") + ") ORDER BY message_db_id, part_id"

	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("query attachments batch: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]Attachment)
	for rows.Next() {
		a, err := d.scanAttachmentRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		result[a.MessageDBID] = append(result[a.MessageDBID], a)
	}
	return result, rows.Err()
}

// GetAttachmentsByMessage returns all attachments for a message by its DB ID.
func (d *DB) GetAttachmentsByMessage(messageDBID int64) ([]Attachment, error) {
	return d.queryAttachments(
		"SELECT "+attachmentSelectColumns+" FROM attachments WHERE message_db_id = ? ORDER BY part_id",
		messageDBID)
}

// GetAttachmentsByMessageID returns all attachments for a message by its Message-ID header.
func (d *DB) GetAttachmentsByMessageID(messageID string) ([]Attachment, error) {
	return d.queryAttachments(`
		SELECT `+attachmentSelectColumns+`
		FROM attachments
		WHERE message_db_id = (SELECT id FROM messages WHERE message_id = ? LIMIT 1)
		ORDER BY part_id`, messageID)
}

func (d *DB) queryAttachments(query string, args ...interface{}) ([]Attachment, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	var atts []Attachment
	for rows.Next() {
		a, err := d.scanAttachmentRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		atts = append(atts, a)
	}
	return atts, rows.Err()
}
