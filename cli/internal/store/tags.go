package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// AddTag adds a tag to a message. No-op if the tag already exists.
func (d *DB) AddTag(messageDBID int64, tag string) error {
	_, err := d.db.Exec(
		"INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)",
		messageDBID, tag)
	if err != nil {
		return fmt.Errorf("add tag: %w", err)
	}
	return nil
}

// RemoveTag removes a tag from a message.
func (d *DB) RemoveTag(messageDBID int64, tag string) error {
	_, err := d.db.Exec(
		"DELETE FROM tags WHERE message_id = ? AND tag = ?",
		messageDBID, tag)
	if err != nil {
		return fmt.Errorf("remove tag: %w", err)
	}
	return nil
}

// TagThread adds a tag to all messages in a thread.
func (d *DB) TagThread(threadID, tag string) error {
	_, err := d.db.Exec(`
		INSERT OR IGNORE INTO tags (message_id, tag)
		SELECT id, ? FROM messages WHERE thread_id = ?`,
		tag, threadID)
	if err != nil {
		return fmt.Errorf("tag thread: %w", err)
	}
	return nil
}

// UntagThread removes a tag from all messages in a thread.
func (d *DB) UntagThread(threadID, tag string) error {
	_, err := d.db.Exec(`
		DELETE FROM tags WHERE tag = ? AND message_id IN (
			SELECT id FROM messages WHERE thread_id = ?
		)`, tag, threadID)
	if err != nil {
		return fmt.Errorf("untag thread: %w", err)
	}
	return nil
}

// ModifyTagsByThread atomically adds and removes tags for all messages in a thread.
func (d *DB) ModifyTagsByThread(threadID string, addTags, removeTags []string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	for _, tag := range addTags {
		_, err := tx.Exec(`
			INSERT OR IGNORE INTO tags (message_id, tag)
			SELECT id, ? FROM messages WHERE thread_id = ?`,
			tag, threadID)
		if err != nil {
			return fmt.Errorf("add tag %q: %w", tag, err)
		}
	}

	for _, tag := range removeTags {
		_, err := tx.Exec(`
			DELETE FROM tags WHERE tag = ? AND message_id IN (
				SELECT id FROM messages WHERE thread_id = ?
			)`, tag, threadID)
		if err != nil {
			return fmt.Errorf("remove tag %q: %w", tag, err)
		}
	}

	return tx.Commit()
}

// GetMessageTagsBatch returns tags for multiple messages in a single query.
// Returns map[messageDBID][]tags.
func (d *DB) GetMessageTagsBatch(ids []int64) (map[int64][]string, error) {
	if len(ids) == 0 {
		return make(map[int64][]string), nil
	}

	placeholders := make([]string, len(ids))
	params := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		params[i] = id
	}

	q := "SELECT message_id, tag FROM tags WHERE message_id IN (" +
		strings.Join(placeholders, ",") + ") ORDER BY message_id, tag"

	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("get tags batch: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]string)
	for rows.Next() {
		var msgID int64
		var tag string
		if err := rows.Scan(&msgID, &tag); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		result[msgID] = append(result[msgID], tag)
	}
	return result, rows.Err()
}

// GetMessageTags returns all tags for a message.
func (d *DB) GetMessageTags(messageDBID int64) ([]string, error) {
	rows, err := d.db.Query(
		"SELECT tag FROM tags WHERE message_id = ? ORDER BY tag", messageDBID)
	if err != nil {
		return nil, fmt.Errorf("get tags: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// ListTags returns all distinct tags in the database.
// When accounts are provided, only tags from those accounts are included.
func (d *DB) ListTags(accounts ...string) ([]string, error) {
	var rows *sql.Rows
	var err error
	if len(accounts) > 0 {
		// Resolve account names → ids. Unknown names contribute nothing
		// to the IN-list; if all names are unknown we short-circuit empty.
		ids := make([]int64, 0, len(accounts))
		for _, name := range accounts {
			var id int64
			err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", name).Scan(&id)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("lookup account id: %w", err)
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return nil, nil
		}
		placeholders := make([]string, len(ids))
		params := make([]interface{}, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			params[i] = id
		}
		rows, err = d.db.Query(
			"SELECT DISTINCT t.tag FROM tags t JOIN messages m ON m.id = t.message_id WHERE m.account_id IN ("+
				strings.Join(placeholders, ",")+") ORDER BY t.tag", params...)
	} else {
		rows, err = d.db.Query("SELECT DISTINCT tag FROM tags ORDER BY tag")
	}
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// ModifyTagsByMessageID adds and removes tags for a message identified by its
// RFC822 Message-ID header. No-op if the message is not in the store.
func (d *DB) ModifyTagsByMessageID(messageID string, addTags, removeTags []string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var dbID int64
	err = tx.QueryRow("SELECT id FROM messages WHERE message_id = ?", messageID).Scan(&dbID)
	if err != nil {
		return nil // message not in store — no-op
	}

	for _, tag := range addTags {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)",
			dbID, tag); err != nil {
			return fmt.Errorf("add tag %q: %w", tag, err)
		}
	}

	for _, tag := range removeTags {
		if _, err := tx.Exec(
			"DELETE FROM tags WHERE message_id = ? AND tag = ?",
			dbID, tag); err != nil {
			return fmt.Errorf("remove tag %q: %w", tag, err)
		}
	}

	return tx.Commit()
}

// ModifyTagsByMessageIDAndAccount adds and removes tags for a message scoped
// to a specific account. No-op if the (message_id, account) pair is not in the store.
func (d *DB) ModifyTagsByMessageIDAndAccount(messageID, account string, addTags, removeTags []string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var accountID int64
	err = tx.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err != nil {
		return nil // unknown account → no-op (mirrors pre-7f behavior)
	}
	var dbID int64
	err = tx.QueryRow(
		"SELECT id FROM messages WHERE message_id = ? AND account_id = ?",
		messageID, accountID).Scan(&dbID)
	if err != nil {
		return nil // message/account pair not in store — no-op
	}

	for _, tag := range addTags {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)",
			dbID, tag); err != nil {
			return fmt.Errorf("add tag %q: %w", tag, err)
		}
	}

	for _, tag := range removeTags {
		if _, err := tx.Exec(
			"DELETE FROM tags WHERE message_id = ? AND tag = ?",
			dbID, tag); err != nil {
			return fmt.Errorf("remove tag %q: %w", tag, err)
		}
	}

	return tx.Commit()
}

// GetTagsByMessageID returns all tags for a message identified by its
// RFC822 Message-ID header. Returns nil if the message is not in the store.
func (d *DB) GetTagsByMessageID(messageID string) ([]string, error) {
	rows, err := d.db.Query(`
		SELECT t.tag FROM tags t
		JOIN messages m ON m.id = t.message_id
		WHERE m.message_id = ?
		ORDER BY t.tag`, messageID)
	if err != nil {
		return nil, fmt.Errorf("get tags by message id: %w", err)
	}
	defer rows.Close()

	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

// GetAccountsByThread returns all distinct accounts that have messages in a thread.
func (d *DB) GetAccountsByThread(threadID string) ([]string, error) {
	rows, err := d.db.Query(
		"SELECT DISTINCT account FROM messages WHERE thread_id = ?", threadID)
	if err != nil {
		return nil, fmt.Errorf("get accounts by thread: %w", err)
	}
	defer rows.Close()

	var accounts []string
	for rows.Next() {
		var account string
		if err := rows.Scan(&account); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// JournalTagChange records a user-initiated tag change for sync purposes.
func (d *DB) JournalTagChange(messageID, account, tag, action string, timestamp int64) {
	d.db.Exec(`INSERT INTO tag_journal (message_id, account, tag, action, timestamp)
		VALUES (?, ?, ?, ?, ?)`, messageID, account, tag, action, timestamp)
}

// ReadTagJournal returns all pending journal entries without deleting them.
func (d *DB) ReadTagJournal() ([]struct {
	ID                              int64
	MessageID, Account, Tag, Action string
	Timestamp                       int64
}, error) {
	rows, err := d.db.Query("SELECT id, message_id, account, tag, action, timestamp FROM tag_journal ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	defer rows.Close()

	var result []struct {
		ID                              int64
		MessageID, Account, Tag, Action string
		Timestamp                       int64
	}
	for rows.Next() {
		var r struct {
			ID                              int64
			MessageID, Account, Tag, Action string
			Timestamp                       int64
		}
		if err := rows.Scan(&r.ID, &r.MessageID, &r.Account, &r.Tag, &r.Action, &r.Timestamp); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// ClearTagJournal deletes journal entries up to and including the given ID.
func (d *DB) ClearTagJournal(upToID int64) error {
	_, err := d.db.Exec("DELETE FROM tag_journal WHERE id <= ?", upToID)
	return err
}

// GetMeta reads an integer value from the metadata table.
func (d *DB) GetMeta(key string) int64 {
	var val int64
	// Table may not exist on older DBs — returns 0
	d.db.QueryRow("SELECT value FROM metadata WHERE key = ?", key).Scan(&val)
	return val
}

// SetMeta writes an integer value to the metadata table.
func (d *DB) SetMeta(key string, value int64) {
	// Ensure table exists (idempotent, handles DBs opened before v8 migration)
	d.db.Exec("CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value INTEGER NOT NULL)")
	d.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES (?, ?)", key, value)
}

// ExportAllTags returns all (message_id, account, tag) tuples in the database.
// Used for initial push to the tag sync server. Account is resolved via
// JOIN on accounts.id; the name comes from the encrypted name_ct BLOB
// (decrypted under meta_key).
func (d *DB) ExportAllTags() ([]struct{ MessageID, Account, Tag string }, error) {
	rows, err := d.db.Query(`
		SELECT m.message_id, ac.name_ct, t.tag
		FROM tags t
		JOIN messages m ON m.id = t.message_id
		LEFT JOIN accounts ac ON ac.id = m.account_id
		ORDER BY m.message_id`)
	if err != nil {
		return nil, fmt.Errorf("export tags: %w", err)
	}
	defer rows.Close()

	var result []struct{ MessageID, Account, Tag string }
	for rows.Next() {
		var r struct{ MessageID, Account, Tag string }
		var accountCT []byte
		if err := rows.Scan(&r.MessageID, &accountCT, &r.Tag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if r.Account, err = d.decryptMeta("", accountCT); err != nil {
			return nil, fmt.Errorf("decrypt account name: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// GetAllMessagesWithTags returns a map of message_id → tags for all messages
// in a given mailbox. When account is non-empty, results are scoped to that account.
// Used for IMAP flag synchronization.
//
// Step 7f: mailbox + account are resolved to their FK ids; unknown names
// return an empty map without an error (no rows can match).
func (d *DB) GetAllMessagesWithTags(mailbox string, account ...string) (map[string][]string, error) {
	if strings.EqualFold(mailbox, "INBOX") {
		mailbox = "INBOX"
	}
	var mailboxID int64
	if err := d.db.QueryRow("SELECT id FROM mailboxes WHERE name = ?", mailbox).Scan(&mailboxID); err != nil {
		if err == sql.ErrNoRows {
			return map[string][]string{}, nil
		}
		return nil, fmt.Errorf("lookup mailbox id: %w", err)
	}
	q := `
		SELECT m.message_id, t.tag
		FROM messages m
		JOIN tags t ON t.message_id = m.id
		WHERE m.mailbox_id = ?`
	params := []interface{}{mailboxID}

	if len(account) > 0 && account[0] != "" {
		var accountID int64
		if err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account[0]).Scan(&accountID); err != nil {
			if err == sql.ErrNoRows {
				return map[string][]string{}, nil
			}
			return nil, fmt.Errorf("lookup account id: %w", err)
		}
		q += " AND m.account_id = ?"
		params = append(params, accountID)
	}
	q += " ORDER BY m.message_id"

	rows, err := d.db.Query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("get messages with tags: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]string)
	for rows.Next() {
		var msgID, tag string
		if err := rows.Scan(&msgID, &tag); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		result[msgID] = append(result[msgID], tag)
	}
	return result, rows.Err()
}
