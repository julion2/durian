package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
	return d.modifyTagsByThread(threadID, addTags, removeTags, 0)
}

// ModifyTagsByThreadAndJournal applies a user mutation and records the final
// intent per local message row in the same transaction. Provider sync consumes
// this journal independently of the optional cross-device tag_journal.
func (d *DB) ModifyTagsByThreadAndJournal(threadID string, addTags, removeTags []string, timestamp int64) error {
	return d.modifyTagsByThread(threadID, addTags, removeTags, timestamp)
}

func (d *DB) modifyTagsByThread(threadID string, addTags, removeTags []string, timestamp int64) error {
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
		if timestamp > 0 {
			if err := journalProviderTagMutationTx(tx, threadID, tag, "add", timestamp); err != nil {
				return err
			}
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
		if timestamp > 0 {
			if err := journalProviderTagMutationTx(tx, threadID, tag, "remove", timestamp); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

func journalProviderTagMutationTx(tx *sql.Tx, threadID, tag, action string, timestamp int64) error {
	if tag != "unread" && tag != "flagged" && tag != "replied" {
		return nil
	}
	// Only the latest user intent for one row/tag matters. Replacing it keeps the
	// queue bounded when a user toggles a tag repeatedly while offline.
	if _, err := tx.Exec(`DELETE FROM provider_tag_mutations
		WHERE tag = ? AND message_db_id IN (SELECT id FROM messages WHERE thread_id = ?)`, tag, threadID); err != nil {
		return fmt.Errorf("replace provider tag mutation %q: %w", tag, err)
	}
	if _, err := tx.Exec(`INSERT INTO provider_tag_mutations (message_db_id, tag, action, created_at)
		SELECT id, ?, ?, ? FROM messages WHERE thread_id = ?`, tag, action, timestamp, threadID); err != nil {
		return fmt.Errorf("journal provider tag mutation %q: %w", tag, err)
	}
	return nil
}

// ModifyTagsByMessageDBID applies tag changes to exactly one local row.
func (d *DB) ModifyTagsByMessageDBID(messageDBID int64, addTags, removeTags []string) error {
	return d.modifyTagsByMessageDBID(messageDBID, addTags, removeTags, 0)
}

// ModifyTagsByMessageDBIDAndJournal applies an explicit local mutation to one
// row and records provider-native flag intent in the same transaction.
func (d *DB) ModifyTagsByMessageDBIDAndJournal(messageDBID int64, addTags, removeTags []string, timestamp int64) error {
	return d.modifyTagsByMessageDBID(messageDBID, addTags, removeTags, timestamp)
}

func (d *DB) modifyTagsByMessageDBID(messageDBID int64, addTags, removeTags []string, timestamp int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	for _, tag := range addTags {
		if _, err := tx.Exec("INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)", messageDBID, tag); err != nil {
			return fmt.Errorf("add tag %q: %w", tag, err)
		}
		if timestamp > 0 {
			if err := journalProviderTagMutationForMessageTx(tx, messageDBID, tag, "add", timestamp); err != nil {
				return err
			}
		}
	}
	for _, tag := range removeTags {
		if _, err := tx.Exec("DELETE FROM tags WHERE message_id = ? AND tag = ?", messageDBID, tag); err != nil {
			return fmt.Errorf("remove tag %q: %w", tag, err)
		}
		if timestamp > 0 {
			if err := journalProviderTagMutationForMessageTx(tx, messageDBID, tag, "remove", timestamp); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func journalProviderTagMutationForMessageTx(tx *sql.Tx, messageDBID int64, tag, action string, timestamp int64) error {
	if tag != "unread" && tag != "flagged" && tag != "replied" {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM provider_tag_mutations WHERE message_db_id = ? AND tag = ?", messageDBID, tag); err != nil {
		return fmt.Errorf("replace provider tag mutation %q: %w", tag, err)
	}
	if _, err := tx.Exec(`INSERT INTO provider_tag_mutations (message_db_id, tag, action, created_at)
		VALUES (?, ?, ?, ?)`, messageDBID, tag, action, timestamp); err != nil {
		return fmt.Errorf("journal provider tag mutation %q: %w", tag, err)
	}
	return nil
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
	return d.modifyTagsByMessageIDAndAccount(messageID, account, addTags, removeTags, 0)
}

// ModifyTagsByMessageIDAndAccountAndJournal applies a cross-device user
// mutation and records provider-native flag intent in the same transaction.
func (d *DB) ModifyTagsByMessageIDAndAccountAndJournal(messageID, account string, addTags, removeTags []string, timestamp int64) error {
	return d.modifyTagsByMessageIDAndAccount(messageID, account, addTags, removeTags, timestamp)
}

func (d *DB) modifyTagsByMessageIDAndAccount(messageID, account string, addTags, removeTags []string, timestamp int64) error {
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
		if timestamp > 0 {
			if err := journalProviderTagMutationForMessageTx(tx, dbID, tag, "add", timestamp); err != nil {
				return err
			}
		}
	}

	for _, tag := range removeTags {
		if _, err := tx.Exec(
			"DELETE FROM tags WHERE message_id = ? AND tag = ?",
			dbID, tag); err != nil {
			return fmt.Errorf("remove tag %q: %w", tag, err)
		}
		if timestamp > 0 {
			if err := journalProviderTagMutationForMessageTx(tx, dbID, tag, "remove", timestamp); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// ModifyFlagTagsIfUnchanged is ModifyTagsByMessageIDAndAccount for a caller
// that decided what to write from a snapshot it read earlier, and must not
// overwrite a change that landed in between.
//
// watch names the tags the caller's decision depended on, and expected is the
// subset of those the snapshot held. Both the comparison and the write happen
// in one transaction, so with immediate transactions the pair is atomic against
// another process as well as another goroutine.
//
// Returns false and writes nothing when the current state disagrees. The caller
// is expected to leave its own before-image alone and retry, rather than force
// a decision computed against a state that no longer exists.
func (d *DB) ModifyFlagTagsIfUnchanged(messageID, account string, watch, expected, addTags, removeTags []string) (bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// A missing row is a genuine no-op, but any other failure is not: reporting
	// it as an applied write would let the caller advance a before-image over a
	// change it never made.
	var accountID int64
	switch err := tx.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("resolve account %q: %w", account, err)
	}
	var dbID int64
	switch err := tx.QueryRow(
		"SELECT id FROM messages WHERE message_id = ? AND account_id = ?",
		messageID, accountID).Scan(&dbID); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("resolve message %q: %w", messageID, err)
	}

	rows, err := tx.Query("SELECT tag FROM tags WHERE message_id = ?", dbID)
	if err != nil {
		return false, fmt.Errorf("read current tags: %w", err)
	}
	watched := make(map[string]bool, len(watch))
	for _, tag := range watch {
		watched[tag] = true
	}
	var current []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan tag: %w", err)
		}
		if watched[tag] {
			current = append(current, tag)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("read current tags: %w", err)
	}
	rows.Close()

	want := slices.Clone(expected)
	slices.Sort(want)
	slices.Sort(current)
	if !slices.Equal(current, want) {
		return false, nil
	}

	for _, tag := range addTags {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)",
			dbID, tag); err != nil {
			return false, fmt.Errorf("add tag %q: %w", tag, err)
		}
	}
	for _, tag := range removeTags {
		if _, err := tx.Exec(
			"DELETE FROM tags WHERE message_id = ? AND tag = ?",
			dbID, tag); err != nil {
			return false, fmt.Errorf("remove tag %q: %w", tag, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
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
	rows, err := d.db.Query(`
		SELECT DISTINCT a.name_ct
		FROM messages m
		JOIN accounts a ON a.id = m.account_id
		WHERE m.thread_id = ?`, threadID)
	if err != nil {
		return nil, fmt.Errorf("get accounts by thread: %w", err)
	}
	defer rows.Close()

	var accounts []string
	for rows.Next() {
		var accountCT []byte
		if err := rows.Scan(&accountCT); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		account, err := d.decryptMeta("", accountCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt account name: %w", err)
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

// ProviderTagMutation is one durable user intent awaiting provider upload.
type ProviderTagMutation struct {
	ID        int64
	MessageID string
	RowID     int64
	RemoteRef string
	Tag       string
	Action    string
}

// ReadProviderTagMutations returns pending user intents for one account.
func (d *DB) ReadProviderTagMutations(account string) ([]ProviderTagMutation, error) {
	rows, err := d.db.Query(`SELECT p.id, m.id, m.message_id, m.remote_ref, p.tag, p.action
		FROM provider_tag_mutations p
		JOIN messages m ON m.id = p.message_db_id
		WHERE m.account_id = (SELECT id FROM accounts WHERE name = ?)
		ORDER BY p.id`, account)
	if err != nil {
		return nil, fmt.Errorf("read provider tag mutations: %w", err)
	}
	defer rows.Close()
	var result []ProviderTagMutation
	for rows.Next() {
		var mutation ProviderTagMutation
		if err := rows.Scan(&mutation.ID, &mutation.RowID, &mutation.MessageID,
			&mutation.RemoteRef, &mutation.Tag, &mutation.Action); err != nil {
			return nil, fmt.Errorf("scan provider tag mutation: %w", err)
		}
		result = append(result, mutation)
	}
	return result, rows.Err()
}

// ClearProviderTagMutation removes one successfully applied intent.
func (d *DB) ClearProviderTagMutation(id int64) error {
	if _, err := d.db.Exec("DELETE FROM provider_tag_mutations WHERE id = ?", id); err != nil {
		return fmt.Errorf("clear provider tag mutation: %w", err)
	}
	return nil
}

// ClearProviderTagMutationsForAccount discards intents for a backend that does
// not support native property patches; its existing baseline merge remains the
// owner of those flag changes.
func (d *DB) ClearProviderTagMutationsForAccount(account string) error {
	if _, err := d.db.Exec(`DELETE FROM provider_tag_mutations
		WHERE message_db_id IN (
			SELECT id FROM messages
			WHERE account_id = (SELECT id FROM accounts WHERE name = ?)
		)`, account); err != nil {
		return fmt.Errorf("clear account provider tag mutations: %w", err)
	}
	return nil
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
