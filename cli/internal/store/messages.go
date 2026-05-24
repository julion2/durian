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

	// ADR-0001 step 5: encrypted subject in subject_ct, plaintext subject
	// stays for FTS5 until step 7.
	subjectCT, err := d.encryptSubject(msg.Subject)
	if err != nil {
		return fmt.Errorf("encrypt subject: %w", err)
	}
	// ADR-0001 step 6: same pattern for body_text / body_html.
	bodyTextCT, err := d.encryptBody(msg.BodyText)
	if err != nil {
		return fmt.Errorf("encrypt body_text: %w", err)
	}
	bodyHTMLCT, err := d.encryptBody(msg.BodyHTML)
	if err != nil {
		return fmt.Errorf("encrypt body_html: %w", err)
	}
	// ADR-0001 step 6: non-boolean flag parts go to flags_other under
	// the meta sub-key. Booleans (is_seen/is_flagged/is_deleted) are
	// derived elsewhere in this insert path's downstream tag flow.
	flagsOtherCT, err := d.encryptMeta(flagsOtherForEncryption(msg.Flags))
	if err != nil {
		return fmt.Errorf("encrypt flags_other: %w", err)
	}

	// ADR-0001 step 7d / §3 revision: from_addr/to_addrs/cc_addrs stay
	// plaintext (substring-search UX, addresses already public on the
	// wire). No *_ct columns written for the addrs columns — v17
	// migration drops them.

	// ADR-0001 step 7e: subject / body_text / body_html plaintext
	// columns are gone. Only the *_ct columns are written. flags TEXT
	// still alive for tag-system + mailbox/account stay TEXT (step 7f
	// handles those).
	err = tx.QueryRow(`
		INSERT INTO messages (
			message_id, thread_id, in_reply_to, refs, subject_ct,
			from_addr, to_addrs, cc_addrs,
			date, created_at,
			body_text_ct, body_html_ct,
			mailbox, flags, flags_other, uid, size, fetched_body, account
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, account) DO UPDATE SET
			subject_ct = excluded.subject_ct,
			from_addr = excluded.from_addr,
			to_addrs = excluded.to_addrs,
			cc_addrs = excluded.cc_addrs,
			body_text_ct = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_text_ct ELSE messages.body_text_ct END,
			body_html_ct = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_html_ct ELSE messages.body_html_ct END,
			fetched_body = MAX(messages.fetched_body, excluded.fetched_body),
			flags = excluded.flags,
			flags_other = excluded.flags_other,
			uid = CASE WHEN excluded.uid > 0 THEN excluded.uid ELSE messages.uid END,
			mailbox = CASE WHEN excluded.mailbox != '' THEN excluded.mailbox ELSE messages.mailbox END
		RETURNING id`,
		msg.MessageID, threadID, msg.InReplyTo, msg.Refs, subjectCT,
		msg.FromAddr, msg.ToAddrs, msg.CCAddrs,
		msg.Date, msg.CreatedAt,
		bodyTextCT, bodyHTMLCT,
		msg.Mailbox, msg.Flags, flagsOtherCT, msg.UID, msg.Size, fetchedBody, msg.Account,
	).Scan(&msg.ID)
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	// ADR-0001 step 7 (a+b): maintain the parallel blind FTS5 row. The
	// old messages_fts trigger-pair still fires for the plaintext columns
	// — step 7c flips reads to messages_blind_fts and step 7e drops the
	// old triggers. DELETE+INSERT (vs UPDATE) because contentless FTS5
	// columns can't be updated in place.
	sTok, fTok, tTok, bTok := d.blindTokens(msg.Subject, msg.FromAddr, msg.ToAddrs, msg.BodyText)
	if _, err := tx.Exec("DELETE FROM messages_blind_fts WHERE rowid = ?", msg.ID); err != nil {
		return fmt.Errorf("blind fts delete: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO messages_blind_fts(rowid, subject_tok, from_tok, to_tok, body_tok)
		VALUES (?, ?, ?, ?, ?)`, msg.ID, sTok, fTok, tTok, bTok); err != nil {
		return fmt.Errorf("blind fts insert: %w", err)
	}

	return nil
}

// UpdateBody updates the body text and HTML for a message (lazy body fetch).
// Writes both the plaintext columns (FTS5 still indexes body_text until
// step 7) and the encrypted *_ct columns introduced in v11.
func (d *DB) UpdateBody(messageID, bodyText, bodyHTML string) error {
	bodyTextCT, err := d.encryptBody(bodyText)
	if err != nil {
		return fmt.Errorf("encrypt body_text: %w", err)
	}
	bodyHTMLCT, err := d.encryptBody(bodyHTML)
	if err != nil {
		return fmt.Errorf("encrypt body_html: %w", err)
	}
	// Wrap in a tx so the messages UPDATE and the blind FTS refresh are
	// atomic. Without this, a crash between them would leave body_tok
	// stale (search wouldn't find body content of lazy-fetched mail).
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	result, err := tx.Exec(`
		UPDATE messages SET body_text_ct = ?,
		                    body_html_ct = ?,
		                    fetched_body = 1
		WHERE message_id = ?`,
		bodyTextCT, bodyHTMLCT, messageID)
	if err != nil {
		return fmt.Errorf("update body: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s", messageID)
	}

	// ADR-0001 step 7d: refresh the blind FTS row so body_tok matches
	// the newly-fetched body. Need the other three fields too because
	// contentless FTS5 can't UPDATE column-by-column — DELETE + INSERT
	// the whole row is the only path.
	// Step 7e: subject plaintext column is gone — fetch subject_ct and
	// decrypt for the tokenization. from_addr/to_addrs stay plaintext.
	var rowid int64
	var subjectCT []byte
	var fromAddr, toAddrs string
	if err := tx.QueryRow(`SELECT id, subject_ct, COALESCE(from_addr, ''), COALESCE(to_addrs, '')
		FROM messages WHERE message_id = ? LIMIT 1`, messageID).Scan(&rowid, &subjectCT, &fromAddr, &toAddrs); err != nil {
		return fmt.Errorf("fetch row for blind FTS refresh: %w", err)
	}
	subject, err := d.decryptSubject("", subjectCT)
	if err != nil {
		return fmt.Errorf("decrypt subject for blind FTS refresh: %w", err)
	}
	sTok, fTok, tTok, bTok := d.blindTokens(subject, fromAddr, toAddrs, bodyText)
	if _, err := tx.Exec("DELETE FROM messages_blind_fts WHERE rowid = ?", rowid); err != nil {
		return fmt.Errorf("blind fts delete: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO messages_blind_fts(rowid, subject_tok, from_tok, to_tok, body_tok)
		VALUES (?, ?, ?, ?, ?)`, rowid, sTok, fTok, tTok, bTok); err != nil {
		return fmt.Errorf("blind fts insert: %w", err)
	}
	return tx.Commit()
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
	var subjectCT, bodyTextCT, bodyHTMLCT []byte
	err := d.db.QueryRow(`
		SELECT id, message_id, thread_id, in_reply_to, refs, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text_ct, body_html_ct,
		       mailbox, flags, uid, size, fetched_body, account
		FROM messages WHERE message_id = ? LIMIT 1`, messageID,
	).Scan(
		&msg.ID, &msg.MessageID, &msg.ThreadID, &msg.InReplyTo, &msg.Refs, &subjectCT,
		&msg.FromAddr, &msg.ToAddrs, &msg.CCAddrs, &msg.Date, &msg.CreatedAt,
		&bodyTextCT, &bodyHTMLCT,
		&msg.Mailbox, &msg.Flags, &msg.UID, &msg.Size, &fetchedBody, &msg.Account,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by message_id: %w", err)
	}
	msg.FetchedBody = fetchedBody == 1
	if msg.Subject, err = d.decryptSubject("", subjectCT); err != nil {
		return nil, err
	}
	// from_addr/to_addrs/cc_addrs stay plaintext (ADR-0001 §3 revision).
	if msg.BodyText, err = d.decryptBody("", bodyTextCT); err != nil {
		return nil, err
	}
	if msg.BodyHTML, err = d.decryptBody("", bodyHTMLCT); err != nil {
		return nil, err
	}
	return msg, nil
}

// GetByThread retrieves all messages in a thread, ordered by date ascending.
// When a message exists in multiple accounts, only the first row is kept.
func (d *DB) GetByThread(threadID string) ([]*Message, error) {
	rows, err := d.db.Query(`
		SELECT id, message_id, thread_id, in_reply_to, refs, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text_ct, body_html_ct,
		       mailbox, flags, uid, size, fetched_body, account
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
		SELECT id, message_id, thread_id, in_reply_to, refs, subject_ct,
		       from_addr, to_addrs, cc_addrs, date, created_at,
		       body_text_ct, body_html_ct,
		       mailbox, flags, uid, size, fetched_body, account
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
// SELECT must list subject_ct, body_text_ct, body_html_ct (no plaintext
// counterparts — dropped in step 7e). from_addr/to_addrs/cc_addrs stay
// plaintext (ADR-0001 §3 revision).
func (d *DB) scanMessages(rows *sql.Rows) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		msg := &Message{}
		var fetchedBody int
		var subjectCT, bodyTextCT, bodyHTMLCT []byte
		err := rows.Scan(
			&msg.ID, &msg.MessageID, &msg.ThreadID, &msg.InReplyTo, &msg.Refs, &subjectCT,
			&msg.FromAddr, &msg.ToAddrs, &msg.CCAddrs, &msg.Date, &msg.CreatedAt,
			&bodyTextCT, &bodyHTMLCT,
			&msg.Mailbox, &msg.Flags, &msg.UID, &msg.Size, &fetchedBody, &msg.Account,
		)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.FetchedBody = fetchedBody == 1
		if msg.Subject, err = d.decryptSubject("", subjectCT); err != nil {
			return nil, err
		}
		if msg.BodyText, err = d.decryptBody("", bodyTextCT); err != nil {
			return nil, err
		}
		if msg.BodyHTML, err = d.decryptBody("", bodyHTMLCT); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message rows: %w", err)
	}
	return msgs, nil
}
