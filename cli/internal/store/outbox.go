package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Enqueue adds a draft to the outbox for sending.
// sendAfter is a Unix timestamp before which the worker will not dequeue this item.
// Use 0 for immediate sending.
func (d *DB) Enqueue(draftJSON string, sendAfter int64) (int64, error) {
	ct, err := d.encryptDraftJSON(draftJSON)
	if err != nil {
		return 0, fmt.Errorf("encrypt draft_json: %w", err)
	}
	result, err := d.db.Exec(
		"INSERT INTO outbox (draft_json, draft_json_ct, created_at, send_after) VALUES (?, ?, ?, ?)",
		draftJSON, ct, time.Now().Unix(), sendAfter)
	if err != nil {
		return 0, fmt.Errorf("enqueue: %w", err)
	}
	return result.LastInsertId()
}

// Dequeue returns the next outbox item to send. Items with fewer attempts
// are prioritized, and items with 5+ attempts are skipped as poison messages.
// Exponential backoff: after each failure, the item must wait before retry:
//
//	attempt 1 → 30s,  attempt 2 → 120s,  attempt 3 → 270s,  attempt 4 → 480s
func (d *DB) Dequeue() (*OutboxItem, error) {
	now := time.Now().Unix()
	row := d.db.QueryRow(`
		SELECT id, draft_json, draft_json_ct, attempts, last_error, created_at
		FROM outbox
		WHERE attempts < 5
		  AND send_after <= ?
		  AND (attempts = 0 OR last_attempted_at + attempts * attempts * 30 <= ?)
		ORDER BY attempts ASC, created_at ASC
		LIMIT 1`, now, now)
	return d.scanOutboxItem(row)
}

// MarkAttempted increments the attempt count, records the error, and
// timestamps the attempt for exponential backoff.
func (d *DB) MarkAttempted(id int64, lastErr string) error {
	_, err := d.db.Exec(
		"UPDATE outbox SET attempts = attempts + 1, last_error = ?, last_attempted_at = ? WHERE id = ?",
		lastErr, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("mark attempted: %w", err)
	}
	return nil
}

// PoisonOutboxItem marks an item as permanently failed by setting attempts to 5.
func (d *DB) PoisonOutboxItem(id int64, reason string) error {
	_, err := d.db.Exec(
		"UPDATE outbox SET attempts = 5, last_error = ? WHERE id = ?",
		reason, id)
	if err != nil {
		return fmt.Errorf("poison outbox item: %w", err)
	}
	return nil
}

// DeleteOutboxItem removes a sent item from the outbox.
func (d *DB) DeleteOutboxItem(id int64) error {
	result, err := d.db.Exec("DELETE FROM outbox WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete outbox item: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("outbox item not found: %d", id)
	}
	return nil
}

// ListOutbox returns all outbox items ordered by creation time (newest first).
func (d *DB) ListOutbox() ([]OutboxItem, error) {
	rows, err := d.db.Query(`
		SELECT id, draft_json, draft_json_ct, attempts, last_error, created_at
		FROM outbox
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list outbox: %w", err)
	}
	defer rows.Close()

	var items []OutboxItem
	for rows.Next() {
		var item OutboxItem
		var ct []byte
		var lastErr sql.NullString
		if err := rows.Scan(&item.ID, &item.DraftJSON, &ct, &item.Attempts, &lastErr, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan outbox item: %w", err)
		}
		if item.DraftJSON, err = d.decryptDraftJSON(item.DraftJSON, ct); err != nil {
			return nil, err
		}
		if lastErr.Valid {
			item.LastError = lastErr.String
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// scanOutboxItem scans a single row into an OutboxItem. The caller's
// SELECT must place draft_json_ct directly after draft_json.
func (d *DB) scanOutboxItem(row *sql.Row) (*OutboxItem, error) {
	item := &OutboxItem{}
	var ct []byte
	var lastErr sql.NullString
	err := row.Scan(&item.ID, &item.DraftJSON, &ct, &item.Attempts, &lastErr, &item.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan outbox item: %w", err)
	}
	if item.DraftJSON, err = d.decryptDraftJSON(item.DraftJSON, ct); err != nil {
		return nil, err
	}
	if lastErr.Valid {
		item.LastError = lastErr.String
	}
	return item, nil
}
