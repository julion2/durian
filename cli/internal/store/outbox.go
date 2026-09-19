package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrOutboxItemNotFound      = errors.New("outbox item not found")
	ErrOutboxItemInFlight      = errors.New("outbox item is already in flight")
	ErrOutboxItemNotInFlight   = errors.New("outbox item is not in flight")
	ErrOutboxDeliveryConfirmed = errors.New("outbox delivery is already confirmed")
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
		"INSERT INTO outbox (draft_json_ct, created_at, send_after) VALUES (?, ?, ?)",
		ct, time.Now().Unix(), sendAfter)
	if err != nil {
		return 0, fmt.Errorf("enqueue: %w", err)
	}
	return result.LastInsertId()
}

// EnqueueIdempotent inserts one logical send action. Repeating the same key
// returns the original row and schedule without creating another deliverable
// message, including when the first HTTP response was lost after commit.
func (d *DB) EnqueueIdempotent(draftJSON string, sendAfter int64, key string) (int64, int64, error) {
	if key == "" {
		return 0, 0, errors.New("outbox idempotency key is empty")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin idempotent enqueue: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	reservation, err := tx.Exec(`INSERT OR IGNORE INTO outbox_idempotency
		(idempotency_key, outbox_id, send_after, created_at) VALUES (?, 0, ?, ?)`, key, sendAfter, now)
	if err != nil {
		return 0, 0, fmt.Errorf("reserve idempotent enqueue: %w", err)
	}
	reserved, err := reservation.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("inspect idempotent enqueue reservation: %w", err)
	}
	if reserved == 0 {
		var id, effectiveSendAfter int64
		if err := tx.QueryRow("SELECT outbox_id, send_after FROM outbox_idempotency WHERE idempotency_key = ?", key).Scan(&id, &effectiveSendAfter); err != nil {
			return 0, 0, fmt.Errorf("lookup idempotent enqueue: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, 0, fmt.Errorf("commit idempotent enqueue lookup: %w", err)
		}
		return id, effectiveSendAfter, nil
	}
	ct, err := d.encryptDraftJSON(draftJSON)
	if err != nil {
		return 0, 0, fmt.Errorf("encrypt draft_json: %w", err)
	}
	result, err := tx.Exec("INSERT INTO outbox (draft_json_ct, created_at, send_after) VALUES (?, ?, ?)", ct, now, sendAfter)
	if err != nil {
		return 0, 0, fmt.Errorf("enqueue idempotent: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("read idempotent enqueue id: %w", err)
	}
	if _, err := tx.Exec("UPDATE outbox_idempotency SET outbox_id = ? WHERE idempotency_key = ?", id, key); err != nil {
		return 0, 0, fmt.Errorf("record idempotent enqueue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit idempotent enqueue: %w", err)
	}
	return id, sendAfter, nil
}

// ClaimNextOutboxItem atomically marks and returns the next outbox item to
// send. Claimed items remain blocked across restarts until the worker records a
// definite failure or deletes a definitely-sent item. Items with fewer attempts
// are prioritized, and items with 5+ attempts are skipped as poison messages.
// Exponential backoff: after each failure, the item must wait before retry:
//
//	attempt 1 → 30s,  attempt 2 → 120s,  attempt 3 → 270s,  attempt 4 → 480s
func (d *DB) ClaimNextOutboxItem() (*OutboxItem, error) {
	now := time.Now().Unix()
	row := d.db.QueryRow(`
		UPDATE outbox SET in_flight = 1, delivery_confirmed = 0
		WHERE id = (
			SELECT id FROM outbox
			WHERE attempts < 5
			  AND in_flight = 0
			  AND send_after <= ?
			  AND (attempts = 0 OR last_attempted_at + attempts * attempts * 30 <= ?)
			ORDER BY attempts ASC, created_at ASC
			LIMIT 1
		)
		RETURNING id, draft_json_ct, attempts, last_error, created_at, in_flight, delivery_confirmed`, now, now)
	return d.scanOutboxItem(row)
}

// UpdateClaimedOutboxDraft persists provider-correlation metadata, notably the
// Message-ID, before the worker contacts the provider.
func (d *DB) UpdateClaimedOutboxDraft(id int64, draftJSON string) error {
	ct, err := d.encryptDraftJSON(draftJSON)
	if err != nil {
		return fmt.Errorf("encrypt claimed outbox draft: %w", err)
	}
	result, err := d.db.Exec("UPDATE outbox SET draft_json_ct = ? WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0", ct, id)
	if err != nil {
		return fmt.Errorf("update claimed outbox draft: %w", err)
	}
	return requireOutboxTransition(result, id, "update claimed outbox draft")
}

// MarkOutboxReconciliationRequired records a safe operator-facing reason while
// deliberately retaining the durable claim to prevent automatic redelivery.
func (d *DB) MarkOutboxReconciliationRequired(id int64, reason string) error {
	result, err := d.db.Exec("UPDATE outbox SET last_error = ? WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0", reason, id)
	if err != nil {
		return fmt.Errorf("mark outbox reconciliation required: %w", err)
	}
	return requireOutboxTransition(result, id, "mark outbox reconciliation required")
}

// MarkOutboxDeliveryConfirmed durably records provider acceptance before the
// worker deletes the claim. If deletion or the process then fails, the item
// cannot be requeued through the verified-not-delivered path.
func (d *DB) MarkOutboxDeliveryConfirmed(id int64, reason string) error {
	result, err := d.db.Exec(
		"UPDATE outbox SET last_error = NULLIF(?, ''), delivery_confirmed = 1 WHERE id = ? AND in_flight = 1",
		reason, id)
	if err != nil {
		return fmt.Errorf("mark outbox delivery confirmed: %w", err)
	}
	return requireOutboxTransition(result, id, "mark outbox delivery confirmed")
}

// RequeueClaimedOutboxItem releases a claim only after an operator has verified
// that the provider did not deliver the message.
func (d *DB) RequeueClaimedOutboxItem(id int64, reason string) error {
	result, err := d.db.Exec(
		"UPDATE outbox SET in_flight = 0, delivery_confirmed = 0, send_after = 0, last_error = ?, last_attempted_at = ? WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0",
		reason, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("requeue claimed outbox item: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("requeue claimed outbox item rows affected: %w", err)
	}
	if rows == 1 {
		return nil
	}
	var inFlight, deliveryConfirmed bool
	if err := d.db.QueryRow("SELECT in_flight, delivery_confirmed FROM outbox WHERE id = ?", id).Scan(&inFlight, &deliveryConfirmed); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %d", ErrOutboxItemNotFound, id)
		}
		return fmt.Errorf("requeue claimed outbox item inspect state: %w", err)
	}
	if !inFlight {
		return fmt.Errorf("%w: %d", ErrOutboxItemNotInFlight, id)
	}
	if deliveryConfirmed {
		return fmt.Errorf("%w: %d", ErrOutboxDeliveryConfirmed, id)
	}
	return fmt.Errorf("requeue claimed outbox item: invalid state for outbox item %d", id)
}

// MarkAttempted increments the attempt count, records the error, and
// timestamps the attempt for exponential backoff.
func (d *DB) MarkAttempted(id int64, lastErr string) error {
	result, err := d.db.Exec(
		"UPDATE outbox SET attempts = attempts + 1, last_error = ?, last_attempted_at = ?, in_flight = 0 WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0",
		lastErr, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("mark attempted: %w", err)
	}
	return requireOutboxTransition(result, id, "mark attempted")
}

// DeferOutboxItem postpones a retry without incrementing the permanent-failure
// attempt count. It is used for offline/network failures so the worker does not
// immediately dequeue the same item in a tight loop.
func (d *DB) DeferOutboxItem(id int64, sendAfter int64, lastErr string) error {
	result, err := d.db.Exec(
		"UPDATE outbox SET send_after = ?, last_error = ?, last_attempted_at = ?, in_flight = 0 WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0",
		sendAfter, lastErr, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("defer outbox item: %w", err)
	}
	return requireOutboxTransition(result, id, "defer outbox item")
}

// PoisonOutboxItem marks an item as permanently failed by setting attempts to 5.
func (d *DB) PoisonOutboxItem(id int64, reason string) error {
	result, err := d.db.Exec(
		"UPDATE outbox SET attempts = 5, last_error = ?, in_flight = 0 WHERE id = ? AND in_flight = 1 AND delivery_confirmed = 0",
		reason, id)
	if err != nil {
		return fmt.Errorf("poison outbox item: %w", err)
	}
	return requireOutboxTransition(result, id, "poison outbox item")
}

// DeletePendingOutboxItem cancels an item only while no worker owns its
// delivery. Claiming and cancellation are competing atomic SQL transitions:
// once the claim wins, Undo must report a conflict rather than falsely reopen
// a compose window for a message that may already be sending.
func (d *DB) DeletePendingOutboxItem(id int64) error {
	return d.deleteOutboxItem(id, false, "delete pending outbox item")
}

// DeleteClaimedOutboxItem removes an in-flight item after either a successful
// worker delivery or explicit, provider-verified manual reconciliation.
func (d *DB) DeleteClaimedOutboxItem(id int64) error {
	return d.deleteOutboxItem(id, true, "delete claimed outbox item")
}

func (d *DB) deleteOutboxItem(id int64, inFlight bool, action string) error {
	result, err := d.db.Exec("DELETE FROM outbox WHERE id = ? AND in_flight = ?", id, inFlight)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", action, err)
	}
	if rows == 1 {
		return nil
	}

	var actualInFlight bool
	if err := d.db.QueryRow("SELECT in_flight FROM outbox WHERE id = ?", id).Scan(&actualInFlight); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %d", ErrOutboxItemNotFound, id)
		}
		return fmt.Errorf("%s inspect state: %w", action, err)
	}
	if actualInFlight {
		return fmt.Errorf("%w: %d", ErrOutboxItemInFlight, id)
	}
	return fmt.Errorf("%w: %d", ErrOutboxItemNotInFlight, id)
}

// ListOutbox returns all outbox items ordered by creation time (newest first).
func (d *DB) ListOutbox() ([]OutboxItem, error) {
	rows, err := d.db.Query(`
		SELECT id, draft_json_ct, attempts, last_error, created_at, in_flight, delivery_confirmed
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
		if err := rows.Scan(&item.ID, &ct, &item.Attempts, &lastErr, &item.CreatedAt, &item.InFlight, &item.DeliveryConfirmed); err != nil {
			return nil, fmt.Errorf("scan outbox item: %w", err)
		}
		if item.DraftJSON, err = d.decryptDraftJSON("", ct); err != nil {
			return nil, err
		}
		if lastErr.Valid {
			item.LastError = lastErr.String
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// scanOutboxItem scans a single row into an OutboxItem. Caller's SELECT
// lists draft_json_ct, in_flight, and delivery_confirmed (the plaintext
// draft_json column is dropped in 7e).
func (d *DB) scanOutboxItem(row *sql.Row) (*OutboxItem, error) {
	item := &OutboxItem{}
	var ct []byte
	var lastErr sql.NullString
	err := row.Scan(&item.ID, &ct, &item.Attempts, &lastErr, &item.CreatedAt, &item.InFlight, &item.DeliveryConfirmed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan outbox item: %w", err)
	}
	if item.DraftJSON, err = d.decryptDraftJSON("", ct); err != nil {
		return nil, err
	}
	if lastErr.Valid {
		item.LastError = lastErr.String
	}
	return item, nil
}

func requireOutboxTransition(result sql.Result, id int64, action string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", action, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s: outbox item %d is missing or not claimed", action, id)
	}
	return nil
}
