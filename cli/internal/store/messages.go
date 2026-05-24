package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// nullableID maps a zero id to sql.NULL so the resulting column stays NULL
// instead of pointing at the non-existent row 0. Empty mailbox/account
// names are legal (e.g. early-sync edge cases) and must not introduce a
// dangling FK reference.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

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
	// ADR-0001 step 7f: split msg.Flags into the three boolean columns
	// + flags_other (everything else, encrypted under meta_key). Inverse
	// of flagsFromParts on the read path.
	parts := strings.Fields(msg.Flags)
	var isSeen, isFlagged, isDeleted int
	for _, p := range parts {
		switch p {
		case `\Seen`:
			isSeen = 1
		case `\Flagged`:
			isFlagged = 1
		case `\Deleted`:
			isDeleted = 1
		}
	}
	flagsOtherCT, err := d.encryptMeta(flagsOtherForEncryption(msg.Flags))
	if err != nil {
		return fmt.Errorf("encrypt flags_other: %w", err)
	}

	// ADR-0001 step 7f: resolve mailbox + account names to their FK ids,
	// inserting new rows (with encrypted name_ct) on first sight. The
	// plaintext shadow columns messages.mailbox / messages.account /
	// messages.flags are gone in v19; this insert no longer touches them.
	mailboxID, err := d.getOrCreateMailbox(tx, msg.Mailbox)
	if err != nil {
		return fmt.Errorf("resolve mailbox: %w", err)
	}
	accountID, err := d.getOrCreateAccount(tx, msg.Account)
	if err != nil {
		return fmt.Errorf("resolve account: %w", err)
	}

	// ADR-0001 step 7d / §3 revision: from_addr/to_addrs/cc_addrs stay
	// plaintext (substring-search UX, addresses already public on the
	// wire). No *_ct columns written for the addrs columns — v17
	// migration drops them.
	err = tx.QueryRow(`
		INSERT INTO messages (
			message_id, thread_id, in_reply_to, refs, subject_ct,
			from_addr, to_addrs, cc_addrs,
			date, created_at,
			body_text_ct, body_html_ct,
			mailbox_id, account_id,
			is_seen, is_flagged, is_deleted, flags_other,
			uid, size, fetched_body
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(message_id, IFNULL(account_id, 0)) DO UPDATE SET
			subject_ct = excluded.subject_ct,
			from_addr = excluded.from_addr,
			to_addrs = excluded.to_addrs,
			cc_addrs = excluded.cc_addrs,
			body_text_ct = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_text_ct ELSE messages.body_text_ct END,
			body_html_ct = CASE WHEN excluded.fetched_body = 1 AND messages.fetched_body = 0
			                 THEN excluded.body_html_ct ELSE messages.body_html_ct END,
			fetched_body = MAX(messages.fetched_body, excluded.fetched_body),
			is_seen = excluded.is_seen,
			is_flagged = excluded.is_flagged,
			is_deleted = excluded.is_deleted,
			flags_other = excluded.flags_other,
			uid = CASE WHEN excluded.uid > 0 THEN excluded.uid ELSE messages.uid END,
			mailbox_id = CASE WHEN excluded.mailbox_id IS NOT NULL
			                 THEN excluded.mailbox_id ELSE messages.mailbox_id END
		RETURNING id`,
		msg.MessageID, threadID, msg.InReplyTo, msg.Refs, subjectCT,
		msg.FromAddr, msg.ToAddrs, msg.CCAddrs,
		msg.Date, msg.CreatedAt,
		bodyTextCT, bodyHTMLCT,
		nullableID(mailboxID), nullableID(accountID),
		isSeen, isFlagged, isDeleted, flagsOtherCT,
		msg.UID, msg.Size, fetchedBody,
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
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	mailboxID, err := d.getOrCreateMailbox(tx, mailbox)
	if err != nil {
		return err
	}
	accountID, err := d.getOrCreateAccount(tx, account)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		"UPDATE messages SET mailbox_id = ?, uid = ? WHERE message_id = ? AND account_id = ?",
		nullableID(mailboxID), uid, messageID, nullableID(accountID)); err != nil {
		return fmt.Errorf("update mailbox: %w", err)
	}
	return tx.Commit()
}

// BackfillUID sets the UID and mailbox for a message that has uid=0.
// Used to populate UIDs for messages that were synced without UID info.
func (d *DB) BackfillUID(messageID, account string, uid uint32, mailbox string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	mailboxID, err := d.getOrCreateMailbox(tx, mailbox)
	if err != nil {
		return err
	}
	accountID, err := d.getOrCreateAccount(tx, account)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE messages SET uid = ?, mailbox_id = ?
		WHERE message_id = ? AND account_id = ? AND uid = 0`,
		uid, nullableID(mailboxID), messageID, nullableID(accountID)); err != nil {
		return fmt.Errorf("backfill uid: %w", err)
	}
	return tx.Commit()
}

// GetByMessageID retrieves a message by its Message-ID header value.
func (d *DB) GetByMessageID(messageID string) (*Message, error) {
	row := d.db.QueryRow(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.message_id = ? LIMIT 1`, messageID)
	msg, err := d.scanMessageRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by message_id: %w", err)
	}
	return msg, nil
}

// GetByThread retrieves all messages in a thread, ordered by date ascending.
// When a message exists in multiple accounts, only the first row is kept.
func (d *DB) GetByThread(threadID string) ([]*Message, error) {
	rows, err := d.db.Query(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.thread_id = ?
		ORDER BY m.date ASC`, threadID)
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
	rows, err := d.db.Query(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.thread_id = ?
		ORDER BY m.date ASC`, threadID)
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
// The account name is resolved to its accounts.id; an unknown account
// returns false without inserting a row (read-only path).
func (d *DB) MessageExistsForAccount(messageID, account string) (bool, error) {
	var accountID int64
	err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup account id: %w", err)
	}
	var count int
	err = d.db.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE message_id = ? AND account_id = ?",
		messageID, accountID).Scan(&count)
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
	var accountID int64
	err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	if err != nil {
		return fmt.Errorf("lookup account id: %w", err)
	}
	result, err := d.db.Exec(
		"DELETE FROM messages WHERE message_id = ? AND account_id = ?",
		messageID, accountID)
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

// messageSelectColumns is the canonical SELECT list for scanMessages /
// the singular row-scan in GetByMessageID. Step 7f dropped the plaintext
// mailbox / account / flags columns; mailbox name and account name now
// come from LEFT JOINs against mailboxes / accounts (decrypted from
// name_ct) and the flags string is reconstructed from is_*  + the
// decrypted flags_other BLOB.
const messageSelectColumns = `m.id, m.message_id, m.thread_id, m.in_reply_to, m.refs, m.subject_ct,
		m.from_addr, m.to_addrs, m.cc_addrs, m.date, m.created_at,
		m.body_text_ct, m.body_html_ct,
		mb.name_ct, ac.name_ct,
		m.is_seen, m.is_flagged, m.is_deleted, m.flags_other,
		m.uid, m.size, m.fetched_body`

const messageSelectFrom = `FROM messages m
		LEFT JOIN mailboxes mb ON mb.id = m.mailbox_id
		LEFT JOIN accounts  ac ON ac.id = m.account_id`

// scanMessageRow scans one row produced by a SELECT over
// messageSelectColumns + messageSelectFrom into a *Message. Shared
// implementation for both the singular GetByMessageID path and the
// row-by-row loop in scanMessages.
func (d *DB) scanMessageRow(scan func(...any) error) (*Message, error) {
	msg := &Message{}
	var fetchedBody int
	var subjectCT, bodyTextCT, bodyHTMLCT, flagsOtherCT, mailboxNameCT, accountNameCT []byte
	var isSeen, isFlagged, isDeleted int
	if err := scan(
		&msg.ID, &msg.MessageID, &msg.ThreadID, &msg.InReplyTo, &msg.Refs, &subjectCT,
		&msg.FromAddr, &msg.ToAddrs, &msg.CCAddrs, &msg.Date, &msg.CreatedAt,
		&bodyTextCT, &bodyHTMLCT,
		&mailboxNameCT, &accountNameCT,
		&isSeen, &isFlagged, &isDeleted, &flagsOtherCT,
		&msg.UID, &msg.Size, &fetchedBody,
	); err != nil {
		return nil, err
	}
	msg.FetchedBody = fetchedBody == 1
	var err error
	if msg.Subject, err = d.decryptSubject("", subjectCT); err != nil {
		return nil, err
	}
	if msg.BodyText, err = d.decryptBody("", bodyTextCT); err != nil {
		return nil, err
	}
	if msg.BodyHTML, err = d.decryptBody("", bodyHTMLCT); err != nil {
		return nil, err
	}
	if msg.Mailbox, err = d.decryptMeta("", mailboxNameCT); err != nil {
		return nil, fmt.Errorf("decrypt mailbox name: %w", err)
	}
	if msg.Account, err = d.decryptMeta("", accountNameCT); err != nil {
		return nil, fmt.Errorf("decrypt account name: %w", err)
	}
	otherPlain, err := d.decryptMeta("", flagsOtherCT)
	if err != nil {
		return nil, fmt.Errorf("decrypt flags_other: %w", err)
	}
	msg.Flags = flagsFromParts(isSeen == 1, isFlagged == 1, isDeleted == 1, otherPlain)
	return msg, nil
}

// scanMessages scans rows produced by SELECT messageSelectColumns +
// messageSelectFrom into a slice of Message pointers.
func (d *DB) scanMessages(rows *sql.Rows) ([]*Message, error) {
	var msgs []*Message
	for rows.Next() {
		msg, err := d.scanMessageRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msgs = append(msgs, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message rows: %w", err)
	}
	return msgs, nil
}
