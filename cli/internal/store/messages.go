package store

import (
	"database/sql"
	"fmt"
)

// InsertMessage inserts or upserts a single message, resolving its thread ID.
func (d *DB) InsertMessage(msg *Message) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	if err := d.insertMessageTx(tx, msg); err != nil {
		return err
	}

	return tx.Commit()
}

// InsertBatch inserts multiple messages in a single transaction.
// Thread resolution within the batch sees earlier inserts (tx visibility).
func (d *DB) InsertBatch(msgs []*Message) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, msg := range msgs {
		if err := d.insertMessageTx(tx, msg); err != nil {
			return fmt.Errorf("insert %q: %w", msg.MessageID, err)
		}
	}

	return tx.Commit()
}

// insertMessageTx inserts a message within an existing transaction.
func (d *DB) insertMessageTx(tx *sql.Tx, msg *Message) error {
	threadID, err := resolveThreadID(tx, msg.MessageID, msg.InReplyTo, msg.Refs)
	if err != nil {
		return fmt.Errorf("resolve thread: %w", err)
	}
	msg.ThreadID = threadID

	fetchedBody := 0
	if msg.FetchedBody {
		fetchedBody = 1
	}

	// ADR-0001 step 5: encrypted subject lives in subject_ct alongside the
	// plaintext subject column (which FTS5 still indexes until step 7).
	subjectCT, err := d.encryptSubject(msg.Subject)
	if err != nil {
		return fmt.Errorf("encrypt subject: %w", err)
	}

	err = tx.QueryRow(`
		INSERT INTO messages (
			message_id, thread_id, in_reply_to, refs, subject, subject_ct,
			from_addr, to_addrs, cc_addrs, date, created_at,
			body_text, body_html, mailbox, flags, uid, size, fetched_body, account
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, account) DO UPDATE SET
			subject = excluded.subject,
			subject_ct = excluded.subject_ct,
			from_addr = excluded.from_addr,
			to_addrs = excluded.to_addrs,
			cc_addrs = excluded.cc_addrs,
			body_text = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_text ELSE messages.body_text END,
			body_html = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_html ELSE messages.body_html END,
			fetched_body = MAX(messages.fetched_body, excluded.fetched_body),
			flags = excluded.flags,
			uid = CASE WHEN excluded.uid > 0 THEN excluded.uid ELSE messages.uid END,
			mailbox = CASE WHEN excluded.mailbox != '' THEN excluded.mailbox ELSE messages.mailbox END
		RETURNING id`,
		msg.MessageID, threadID, msg.InReplyTo, msg.Refs, msg.Subject, subjectCT,
		msg.FromAddr, msg.ToAddrs, msg.CCAddrs, msg.Date, msg.CreatedAt,
		msg.BodyText, msg.BodyHTML, msg.Mailbox, msg.Flags, msg.UID, msg.Size, fetchedBody, msg.Account,
	).Scan(&msg.ID)
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	return nil
}

// UpdateBody updates the body text and HTML for a message (lazy body fetch).
func (d *DB) UpdateBody(messageID, bodyText, bodyHTML string) error {
	result, err := d.db.Exec(`
		UPDATE messages SET body_text = ?, body_html = ?, fetched_body = 1
		WHERE message_id = ?`,
		bodyText, bodyHTML, messageID)
	if err != nil {
		return fmt.Errorf("update body: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s", messageID)
	}
	return nil
}

// UpdateMailbox sets the mailbox and UID for a message identified by message_id and account.
func (d *DB) UpdateMailbox(messageID, account, mailbox string, uid uint32) error {
	_, err := d.db.Exec(
		"UPDATE messages SET mailbox = ?, uid = ? WHERE message_id = ? AND account = ?",
		mailbox, uid, messageID, account)
	if err != nil {
		return fmt.Errorf("update mailbox: %w", err)
	}
	return nil
}

// BackfillUID sets the UID and mailbox for a message that has uid=0.
// Used to populate UIDs for messages that were synced without UID info.
func (d *DB) BackfillUID(messageID, account string, uid uint32, mailbox string) error {
	_, err := d.db.Exec(`
		UPDATE messages SET uid = ?, mailbox = ?
		WHERE message_id = ? AND account = ? AND uid = 0`,
		uid, mailbox, messageID, account)
	if err != nil {
		return fmt.Errorf("backfill uid: %w", err)
	}
	return nil
}

// GetByMessageID retrieves a message by its Message-ID header value.
func (d *DB) GetByMessageID(messageID string) (*Message, error) {
	msg := &Message{}
	var fetchedBody int
	var subjectCT []byte
	err := d.db.QueryRow(`
		SELECT id, message_id, thread_id, in_reply_to, refs, subject, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text, body_html, mailbox, flags, uid, size, fetched_body, account
		FROM messages WHERE message_id = ? LIMIT 1`, messageID,
	).Scan(
		&msg.ID, &msg.MessageID, &msg.ThreadID, &msg.InReplyTo, &msg.Refs, &msg.Subject, &subjectCT,
		&msg.FromAddr, &msg.ToAddrs, &msg.CCAddrs, &msg.Date, &msg.CreatedAt,
		&msg.BodyText, &msg.BodyHTML, &msg.Mailbox, &msg.Flags, &msg.UID, &msg.Size, &fetchedBody, &msg.Account,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by message_id: %w", err)
	}
	msg.FetchedBody = fetchedBody == 1
	if msg.Subject, err = d.decryptSubject(msg.Subject, subjectCT); err != nil {
		return nil, err
	}
	return msg, nil
}

// GetByThread retrieves all messages in a thread, ordered by date ascending.
// When a message exists in multiple accounts, only the first row is kept.
func (d *DB) GetByThread(threadID string) ([]*Message, error) {
	rows, err := d.db.Query(`
		SELECT id, message_id, thread_id, in_reply_to, refs, subject, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text, body_html, mailbox, flags, uid, size, fetched_body, account
		FROM messages WHERE thread_id = ?
		ORDER BY date ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("get by thread: %w", err)
	}
	defer rows.Close()

	all, err := d.scanMessages(rows)
	if err != nil {
		return nil, err
	}

	// Dedup: same message_id across accounts appears once in thread view.
	// Arbitrary account is fine — tags are fetched separately and content is identical.
	seen := make(map[string]bool, len(all))
	deduped := make([]*Message, 0, len(all))
	for _, msg := range all {
		if seen[msg.MessageID] {
			continue
		}
		seen[msg.MessageID] = true
		deduped = append(deduped, msg)
	}
	return deduped, nil
}

// GetAllByThread retrieves all messages in a thread without deduplication.
// Returns all rows including multi-account duplicates. Used for tag sync.
func (d *DB) GetAllByThread(threadID string) ([]*Message, error) {
	rows, err := d.db.Query(`
		SELECT id, message_id, thread_id, in_reply_to, refs, subject, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text, body_html, mailbox, flags, uid, size, fetched_body, account
		FROM messages WHERE thread_id = ?
		ORDER BY date ASC`, threadID)
	if err != nil {
		return nil, fmt.Errorf("get all by thread: %w", err)
	}
	defer rows.Close()
	return d.scanMessages(rows)
}

// MessageExists checks if a message with the given Message-ID exists.
func (d *DB) MessageExists(messageID string) (bool, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM messages WHERE message_id = ?", messageID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check message exists: %w", err)
	}
	return count > 0, nil
}

// MessageExistsForAccount checks if a message exists for a specific account.
func (d *DB) MessageExistsForAccount(messageID, account string) (bool, error) {
	var count int
	err := d.db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE message_id = ? AND account = ?",
		messageID, account).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check message exists for account: %w", err)
	}
	return count > 0, nil
}

// GetAllMessageIDSet returns a set of all Message-IDs in the store.
// Used for efficient bulk existence checks during backfill.
func (d *DB) GetAllMessageIDSet() (map[string]bool, error) {
	rows, err := d.db.Query("SELECT DISTINCT message_id FROM messages")
	if err != nil {
		return nil, fmt.Errorf("get all message ids: %w", err)
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message id: %w", err)
		}
		result[id] = true
	}
	return result, rows.Err()
}

// DeleteByMessageID deletes a message by its Message-ID header value.
func (d *DB) DeleteByMessageID(messageID string) error {
	result, err := d.db.Exec("DELETE FROM messages WHERE message_id = ?", messageID)
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s", messageID)
	}
	return nil
}

// DeleteByMessageIDAndAccount deletes a message by its Message-ID and account.
func (d *DB) DeleteByMessageIDAndAccount(messageID, account string) error {
	result, err := d.db.Exec(
		"DELETE FROM messages WHERE message_id = ? AND account = ?",
		messageID, account)
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	return nil
}

// GetSenderCounts returns unique From addresses with their message counts.
func (d *DB) GetSenderCounts() (map[string]int, error) {
	rows, err := d.db.Query(`SELECT from_addr, COUNT(*) FROM messages WHERE from_addr != '' GROUP BY from_addr`)
	if err != nil {
		return nil, fmt.Errorf("query senders: %w", err)
	}
	defer rows.Close()
	result := make(map[string]int)
	for rows.Next() {
		var addr string
		var count int
		if err := rows.Scan(&addr, &count); err != nil {
			return nil, fmt.Errorf("scan sender: %w", err)
		}
		result[addr] = count
	}
	return result, rows.Err()
}

// GetRecipientAddresses returns all non-empty To and CC address field values.
func (d *DB) GetRecipientAddresses() ([]string, error) {
	rows, err := d.db.Query(`SELECT to_addrs FROM messages WHERE to_addrs != '' UNION ALL SELECT cc_addrs FROM messages WHERE cc_addrs != ''`)
	if err != nil {
		return nil, fmt.Errorf("query recipients: %w", err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		result = append(result, addr)
	}
	return result, rows.Err()
}

// scanMessages scans rows into a slice of Message pointers. Caller's
// SELECT must include subject_ct between subject and from_addr.
func (d *DB) scanMessages(rows *sql.Rows) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		msg := &Message{}
		var fetchedBody int
		var subjectCT []byte
		err := rows.Scan(
			&msg.ID, &msg.MessageID, &msg.ThreadID, &msg.InReplyTo, &msg.Refs, &msg.Subject, &subjectCT,
			&msg.FromAddr, &msg.ToAddrs, &msg.CCAddrs, &msg.Date, &msg.CreatedAt,
			&msg.BodyText, &msg.BodyHTML, &msg.Mailbox, &msg.Flags, &msg.UID, &msg.Size, &fetchedBody, &msg.Account,
		)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.FetchedBody = fetchedBody == 1
		if msg.Subject, err = d.decryptSubject(msg.Subject, subjectCT); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message rows: %w", err)
	}
	return msgs, nil
}
