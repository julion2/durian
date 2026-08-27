package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"
)

const syncedFlagsEmpty = "$DurianEmpty"

func encodeSyncedFlags(flags string) string {
	if flags == "" {
		return syncedFlagsEmpty
	}
	return flags
}

func decodeSyncedFlags(flags string) (string, bool) {
	switch flags {
	case "":
		return "", false
	case syncedFlagsEmpty:
		return "", true
	default:
		return flags, true
	}
}

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
	_, err := d.upsertMessage(msg, nil)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	return nil
}

// UpsertMessage inserts or updates a message and reports whether this call
// created the row. The result is determined inside the write transaction, so
// it remains authoritative when multiple Durian processes ingest concurrently.
func (d *DB) UpsertMessage(msg *Message) (bool, error) {
	created, err := d.upsertMessage(msg, nil)
	if err != nil {
		return false, fmt.Errorf("upsert message: %w", err)
	}
	return created, nil
}

// UpsertMessageWithInitialTags atomically seeds tags when this call creates the
// message. Existing rows are updated without touching their local tags.
func (d *DB) UpsertMessageWithInitialTags(msg *Message, initialTags []string) (bool, error) {
	created, err := d.upsertMessage(msg, initialTags)
	if err != nil {
		return false, fmt.Errorf("upsert message with initial tags: %w", err)
	}
	return created, nil
}

func (d *DB) upsertMessage(msg *Message, initialTags []string) (bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var created bool
	if err := d.insertMessageTx(tx, msg, &created); err != nil {
		return false, fmt.Errorf("write message: %w", err)
	}
	if created {
		for _, tag := range initialTags {
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO tags (message_id, tag) VALUES (?, ?)",
				msg.ID, tag); err != nil {
				return false, fmt.Errorf("add initial tag %q: %w", tag, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit message upsert: %w", err)
	}
	return created, nil
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
		if err := d.insertMessageTx(tx, msg, nil); err != nil {
			return fmt.Errorf("insert %q: %w", msg.MessageID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message batch: %w", err)
	}
	return nil
}

// insertMessageTx inserts a message within an existing transaction.
func (d *DB) insertMessageTx(tx *sql.Tx, msg *Message, createdResult *bool) error {
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
	parts := splitMessageFlags(msg.Flags)
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
	accountKey := accountID
	if msg.StableID != "" {
		// Upgrade a legacy Message-ID-keyed row lazily when the stable backend
		// sees it again. Requiring a matching (or empty) remote_ref avoids
		// claiming the wrong row when duplicate Message-IDs were previously
		// collapsed; the matching duplicate will claim it later in the page.
		if _, err := tx.Exec(`UPDATE messages SET stable_id = ?
			WHERE id = (
				SELECT id FROM messages
				WHERE message_id = ? AND IFNULL(account_id, 0) = ? AND stable_id = ''
				  AND (remote_ref = '' OR remote_ref = ?)
				ORDER BY CASE WHEN remote_ref = ? THEN 0 ELSE 1 END, id
				LIMIT 1
			)
			AND NOT EXISTS (
				SELECT 1 FROM messages WHERE stable_id = ? AND IFNULL(account_id, 0) = ?
			)`, msg.StableID, msg.MessageID, accountKey, msg.RemoteRef, msg.RemoteRef, msg.StableID, accountKey); err != nil {
			return fmt.Errorf("claim stable message identity: %w", err)
		}
	}

	// Open configures BEGIN IMMEDIATE, so competing Durian processes serialize
	// before this existence check even when mailbox and account are empty. This
	// is the only point that can capture the old provider flags before the upsert
	// below overwrites them.
	var existingID int64
	var storedBaseline string
	err = tx.QueryRow(`SELECT id, synced_flags FROM messages
		WHERE message_id = ? AND IFNULL(account_id, 0) = ?`,
		msg.MessageID, accountID).Scan(&existingID, &storedBaseline)
	switch err {
	case nil:
	case sql.ErrNoRows:
		if createdResult != nil {
			*createdResult = true
		}
	default:
		return fmt.Errorf("check existing message: %w", err)
	}
	if err == nil && storedBaseline == "" {
		var isSeen, isFlagged, isDeleted bool
		var flagsOtherCT []byte
		if err := tx.QueryRow(`SELECT is_seen, is_flagged, is_deleted, flags_other
			FROM messages WHERE id = ?`, existingID).
			Scan(&isSeen, &isFlagged, &isDeleted, &flagsOtherCT); err != nil {
			return fmt.Errorf("read flag baseline before image: %w", err)
		}
		// Keep this decrypt strictly gated on synced_flags == "". Initialized
		// rows are the permanent hot path; decrypting their flags_other on every
		// metadata upsert would undo the allocation savings of metadata-only
		// ingest. A legacy row crosses this transition exactly once.
		flagsOther, err := d.decryptMeta("", flagsOtherCT)
		if err != nil {
			// The plaintext boolean columns still provide a useful baseline. Do
			// not permanently block this row (or roll back healthy batch siblings)
			// because encrypted auxiliary flags were already damaged.
			slog.Warn("Could not decrypt legacy baseline; capturing provider booleans only",
				"module", "STORE", "err", err)
			flagsOther = ""
		}
		captured := syncedFlagBaseline(isSeen, isFlagged, isDeleted, flagsOther)
		if _, err := tx.Exec(`UPDATE messages SET synced_flags = ?
			WHERE id = ? AND synced_flags = ''`, encodeSyncedFlags(captured), existingID); err != nil {
			return fmt.Errorf("capture flag baseline before image: %w", err)
		}
	}

	syncedFlags := msg.SyncedFlags
	if msg.SyncedFlagsInitialized || syncedFlags != "" {
		syncedFlags = encodeSyncedFlags(syncedFlags)
	}

	// ADR-0001 step 7d / §3 revision: from_addr/to_addrs/cc_addrs stay
	// plaintext (substring-search UX, addresses already public on the
	// wire). No *_ct columns written for the addrs columns — v17
	// migration drops them.
	err = tx.QueryRow(`
		INSERT INTO messages (
			stable_id, message_id, thread_id, in_reply_to, refs, subject_ct,
			from_addr, to_addrs, cc_addrs,
			date, created_at,
			body_text_ct, body_html_ct,
			mailbox_id, account_id,
			is_seen, is_flagged, is_deleted, flags_other,
			uid, size, fetched_body,
			remote_ref, synced_flags
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET
			subject_ct = CASE WHEN excluded.fetched_body = 1
			                  THEN excluded.subject_ct ELSE messages.subject_ct END,
			from_addr = CASE WHEN excluded.fetched_body = 1
			                 THEN excluded.from_addr ELSE messages.from_addr END,
			to_addrs = CASE WHEN excluded.fetched_body = 1
			                THEN excluded.to_addrs ELSE messages.to_addrs END,
			cc_addrs = CASE WHEN excluded.fetched_body = 1
			                THEN excluded.cc_addrs ELSE messages.cc_addrs END,
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
			                 THEN excluded.mailbox_id ELSE messages.mailbox_id END,
			remote_ref = CASE WHEN excluded.remote_ref != ''
			                 THEN excluded.remote_ref ELSE messages.remote_ref END
			-- synced_flags is deliberately NOT updated on conflict: it is the
			-- flag-sync baseline, initialized at insert or captured from the old
			-- row above and thereafter owned by reconciliation (SetSyncedFlags).
			-- Overwriting it from the incoming row when a delta
			-- re-delivers a message after a server-side flag change would corrupt
			-- the three-way merge and revert that change.
		RETURNING id`,
		msg.StableID, msg.MessageID, threadID, msg.InReplyTo, msg.Refs, subjectCT,
		msg.FromAddr, msg.ToAddrs, msg.CCAddrs,
		msg.Date, msg.CreatedAt,
		bodyTextCT, bodyHTMLCT,
		nullableID(mailboxID), nullableID(accountID),
		isSeen, isFlagged, isDeleted, flagsOtherCT,
		msg.UID, msg.Size, fetchedBody,
		msg.RemoteRef, syncedFlags,
	).Scan(&msg.ID)
	if err != nil {
		return fmt.Errorf("upsert message: %w", err)
	}

	// Metadata-only updates preserve every indexed value. Keep an existing FTS
	// row untouched so flag/reference refreshes do not decrypt and retokenize
	// the full stored body. A first insert or a missing FTS row still needs one.
	if !msg.FetchedBody {
		var ftsExists bool
		if err := tx.QueryRow(`SELECT EXISTS(
			SELECT 1 FROM messages_blind_fts WHERE rowid = ?
		)`, msg.ID).Scan(&ftsExists); err != nil {
			return fmt.Errorf("check blind FTS row: %w", err)
		}
		if ftsExists {
			return nil
		}
	}

	// Read back after the SQL upsert so FTS always reflects the effective stored
	// values. In particular, a full reingest updates metadata but retains an
	// already-fetched body, which can differ from the incoming body.
	var storedSubjectCT, storedBodyCT []byte
	var ftsFrom, ftsTo string
	if err := tx.QueryRow(`SELECT subject_ct, COALESCE(from_addr, ''), COALESCE(to_addrs, ''), body_text_ct
		FROM messages WHERE id = ?`, msg.ID).Scan(&storedSubjectCT, &ftsFrom, &ftsTo, &storedBodyCT); err != nil {
		return fmt.Errorf("fetch message for blind FTS refresh: %w", err)
	}
	ftsSubject, err := d.decryptSubject("", storedSubjectCT)
	if err != nil {
		return fmt.Errorf("decrypt subject for blind FTS refresh: %w", err)
	}
	ftsBody, err := d.decryptBody("", storedBodyCT)
	if err != nil {
		return fmt.Errorf("decrypt body for blind FTS refresh: %w", err)
	}

	sTok, fTok, tTok, bTok := d.blindTokens(ftsSubject, ftsFrom, ftsTo, ftsBody)
	if _, err := tx.Exec("DELETE FROM messages_blind_fts WHERE rowid = ?", msg.ID); err != nil {
		return fmt.Errorf("blind fts delete: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO messages_blind_fts(rowid, subject_tok, from_tok, to_tok, body_tok)
		VALUES (?, ?, ?, ?, ?)`, msg.ID, sTok, fTok, tTok, bTok); err != nil {
		return fmt.Errorf("blind fts insert: %w", err)
	}

	return nil
}

// syncedFlagBaseline serializes only the five flags the sync engine tracks.
// Unknown provider keywords remain message metadata, not merge state.
func syncedFlagBaseline(isSeen, isFlagged, isDeleted bool, flagsOther string) string {
	present := make(map[string]bool, 5)
	if isSeen {
		present[`\Seen`] = true
	}
	if isFlagged {
		present[`\Flagged`] = true
	}
	if isDeleted {
		present[`\Deleted`] = true
	}
	for _, flag := range strings.FieldsFunc(flagsOther, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}) {
		switch flag {
		case `\Answered`, "$Completed":
			present[flag] = true
		}
	}
	ordered := []string{`\Seen`, `\Flagged`, `\Answered`, `\Deleted`, "$Completed"}
	baseline := make([]string, 0, len(present))
	for _, flag := range ordered {
		if present[flag] {
			baseline = append(baseline, flag)
		}
	}
	return strings.Join(baseline, ",")
}

// UpdateBody updates the body text and HTML for a message (lazy body fetch).
// Writes only the encrypted body_text_ct / body_html_ct columns; the blind
// FTS index (body_tok) is refreshed in the same transaction below.
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

// GetByMessageID retrieves a message by its Message-ID header value. Stable
// rows require an opaque identifier when the header is ambiguous; silently
// selecting one would defeat their provider-native identity.
func (d *DB) GetByMessageID(messageID string) (*Message, error) {
	rows, err := d.db.Query(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.message_id = ? AND m.stable_id != '' ORDER BY m.id LIMIT 2`, messageID)
	if err != nil {
		return nil, fmt.Errorf("query stable rows by message_id: %w", err)
	}
	stable, err := d.scanMessages(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(stable) > 1 {
		return nil, fmt.Errorf("message-ID %q is ambiguous; use the opaque message identifier", messageID)
	}
	if len(stable) == 1 {
		return stable[0], nil
	}

	row := d.db.QueryRow(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.message_id = ? AND m.stable_id = '' LIMIT 1`, messageID)
	msg, err := d.scanMessageRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by message_id: %w", err)
	}
	return msg, nil
}

// GetByDBID retrieves one message by Durian's local row identity.
func (d *DB) GetByDBID(id int64) (*Message, error) {
	row := d.db.QueryRow(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.id = ?`, id)
	msg, err := d.scanMessageRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by database id: %w", err)
	}
	return msg, nil
}

// GetByIdentifier accepts the opaque local:<rowid> identifier returned by the
// HTTP API and, for backward compatibility, an RFC Message-ID.
func (d *DB) GetByIdentifier(identifier string) (*Message, error) {
	if rawID, ok := strings.CutPrefix(identifier, "local:"); ok {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid local message identifier %q", identifier)
		}
		return d.GetByDBID(id)
	}
	return d.GetByMessageID(identifier)
}

// GetByRemoteRef retrieves the message addressed by a provider ref within an
// account and mailbox. The mailbox scope matters for IMAP, where UIDs are only
// unique within one mailbox. Unknown names or refs return nil.
func (d *DB) GetByRemoteRef(account, mailbox, remoteRef string) (*Message, error) {
	row := d.db.QueryRow(`SELECT `+messageSelectColumns+`
		`+messageSelectFrom+`
		WHERE m.account_id = (SELECT id FROM accounts WHERE name = ?)
		  AND m.mailbox_id = (SELECT id FROM mailboxes WHERE name = ?)
		  AND m.remote_ref = ? LIMIT 1`, account, mailbox, remoteRef)
	msg, err := d.scanMessageRow(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get by remote ref: %w", err)
	}
	return msg, nil
}

// GetByThread retrieves all messages in a thread, ordered by date ascending.
// Cross-account fallback rows for one RFC Message-ID are deduplicated. Rows
// with provider-stable identity are always retained: Message-ID cannot prove
// that two native objects, even across accounts, are the same message.
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

	seenAccount := make(map[string]string, len(all))
	deduped := make([]*Message, 0, len(all))
	for _, msg := range all {
		if msg.StableID != "" {
			deduped = append(deduped, msg)
			continue
		}
		if account, seen := seenAccount[msg.MessageID]; seen && account != msg.Account {
			continue
		}
		seenAccount[msg.MessageID] = msg.Account
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

// MessageIdentityExistsForAccount checks the identity mode used by an incoming
// message: a native stable id when present, otherwise its RFC Message-ID. A
// stable message also matches the legacy fallback row that InsertMessage would
// claim, so upgrading an existing JMAP row is not reported as a new arrival.
func (d *DB) MessageIdentityExistsForAccount(stableID, messageID, remoteRef, account string) (bool, error) {
	var accountID int64
	err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup account id: %w", err)
	}
	var count int
	if stableID != "" {
		err = d.db.QueryRow(`SELECT COUNT(*) FROM messages
			WHERE account_id = ? AND (
				stable_id = ? OR (
					stable_id = '' AND message_id = ?
					AND (remote_ref = '' OR remote_ref = ?)
				)
			)`, accountID, stableID, messageID, remoteRef).Scan(&count)
	} else {
		err = d.db.QueryRow(
			"SELECT COUNT(*) FROM messages WHERE stable_id = '' AND message_id = ? AND account_id = ?",
			messageID, accountID).Scan(&count)
	}
	if err != nil {
		return false, fmt.Errorf("check message identity for account: %w", err)
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

// DeleteByDBID deletes exactly one local message row.
func (d *DB) DeleteByDBID(id int64) error {
	result, err := d.db.Exec("DELETE FROM messages WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete message row: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message row not found: %d", id)
	}
	return nil
}

// FolderFlagRow is one message's flag-sync state within a folder: its
// provider handle (RemoteRef), the last-synced flag baseline
// (SyncedFlags, comma-joined IMAP-style flag string) and its current
// local tags. The sync engine's flag three-way merge is driven off this.
type FolderFlagRow struct {
	RowID                  int64
	MessageID              string
	RemoteRef              string
	SyncedFlags            string
	SyncedFlagsInitialized bool
	Tags                   []string
	// IsSeen / IsFlagged are the server flag state stored at last ingest, used to
	// seed an empty synced_flags baseline (a legacy-migrated row) from the server
	// side rather than guessing from local tags.
	IsSeen    bool
	IsFlagged bool
}

// GetFolderFlagState returns the flag-sync state for every message in the
// given account+mailbox that has a non-empty remote_ref.
//
// Mailbox + account are resolved to their FK ids the same way
// GetAllMessagesWithTags does; unknown names return an empty slice
// without an error (no rows can match).
func (d *DB) GetFolderFlagState(account, mailbox string) ([]FolderFlagRow, error) {
	if strings.EqualFold(mailbox, "INBOX") {
		mailbox = "INBOX"
	}
	var mailboxID int64
	if err := d.db.QueryRow("SELECT id FROM mailboxes WHERE name = ?", mailbox).Scan(&mailboxID); err != nil {
		if err == sql.ErrNoRows {
			return []FolderFlagRow{}, nil
		}
		return nil, fmt.Errorf("lookup mailbox id: %w", err)
	}
	var accountID int64
	if err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return []FolderFlagRow{}, nil
		}
		return nil, fmt.Errorf("lookup account id: %w", err)
	}

	// LEFT JOIN (vs the inner JOIN in GetAllMessagesWithTags): a message
	// with no tags still needs a row so the three-way sees its empty
	// local state. ORDER BY m.id groups a message's tag rows together.
	rows, err := d.db.Query(`
		SELECT m.id, m.message_id, m.remote_ref, m.synced_flags, m.is_seen, m.is_flagged, IFNULL(t.tag, '')
		FROM messages m
		LEFT JOIN tags t ON t.message_id = m.id
		WHERE m.mailbox_id = ? AND m.account_id = ? AND m.remote_ref != ''
		ORDER BY m.id`, mailboxID, accountID)
	if err != nil {
		return nil, fmt.Errorf("get folder flag state: %w", err)
	}
	defer rows.Close()

	result := []FolderFlagRow{}
	for rows.Next() {
		var rowID int64
		var msgID, remoteRef, storedSyncedFlags, tag string
		var isSeen, isFlagged bool
		if err := rows.Scan(&rowID, &msgID, &remoteRef, &storedSyncedFlags, &isSeen, &isFlagged, &tag); err != nil {
			return nil, fmt.Errorf("scan folder flag row: %w", err)
		}
		if n := len(result); n == 0 || result[n-1].RowID != rowID {
			syncedFlags, initialized := decodeSyncedFlags(storedSyncedFlags)
			result = append(result, FolderFlagRow{
				RowID:                  rowID,
				MessageID:              msgID,
				RemoteRef:              remoteRef,
				SyncedFlags:            syncedFlags,
				SyncedFlagsInitialized: initialized,
				IsSeen:                 isSeen,
				IsFlagged:              isFlagged,
			})
		}
		if tag != "" {
			result[len(result)-1].Tags = append(result[len(result)-1].Tags, tag)
		}
	}
	return result, rows.Err()
}

// SetSyncedFlags updates the last-synced flag baseline for a message
// identified by message_id and account. The account name is resolved to
// its accounts.id; an unknown account or message returns an error.
func (d *DB) SetSyncedFlags(messageID, account, syncedFlags string) error {
	var accountID int64
	err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	if err != nil {
		return fmt.Errorf("lookup account id: %w", err)
	}
	result, err := d.db.Exec(
		"UPDATE messages SET synced_flags = ? WHERE message_id = ? AND account_id = ?",
		encodeSyncedFlags(syncedFlags), messageID, accountID)
	if err != nil {
		return fmt.Errorf("set synced flags: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	return nil
}

// SetSyncedFlagsByDBID updates one row without relying on a duplicable
// Message-ID. Provider-engine reconciliation should prefer this method.
func (d *DB) SetSyncedFlagsByDBID(id int64, syncedFlags string) error {
	result, err := d.db.Exec("UPDATE messages SET synced_flags = ? WHERE id = ?", syncedFlags, id)
	if err != nil {
		return fmt.Errorf("set synced flags by row: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message row not found: %d", id)
	}
	return nil
}

// GetSyncedLabels returns the last-synced label baseline (comma-joined tag
// names) for a message, or "" when the message is unknown — used by the label
// three-way to tell which of a message's tags were Gmail-mirrored last sync.
func (d *DB) GetSyncedLabels(messageID, account string) (string, error) {
	var labels string
	err := d.db.QueryRow(
		`SELECT synced_labels FROM messages
		 WHERE message_id = ? AND account_id = (SELECT id FROM accounts WHERE name = ?)`,
		messageID, account).Scan(&labels)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get synced labels: %w", err)
	}
	return labels, nil
}

// SetSyncedLabels updates the last-synced label baseline for a message
// identified by message_id and account. An unknown account or message errors.
func (d *DB) SetSyncedLabels(messageID, account, syncedLabels string) error {
	var accountID int64
	err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	if err != nil {
		return fmt.Errorf("lookup account id: %w", err)
	}
	result, err := d.db.Exec(
		"UPDATE messages SET synced_labels = ? WHERE message_id = ? AND account_id = ?",
		syncedLabels, messageID, accountID)
	if err != nil {
		return fmt.Errorf("set synced labels: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message not found: %s (account %s)", messageID, account)
	}
	return nil
}

// GetSyncedLabelsByDBID returns one row's label baseline.
func (d *DB) GetSyncedLabelsByDBID(id int64) (string, error) {
	var labels string
	err := d.db.QueryRow("SELECT synced_labels FROM messages WHERE id = ?", id).Scan(&labels)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get synced labels by row: %w", err)
	}
	return labels, nil
}

// SetSyncedLabelsByDBID updates one row's label baseline.
func (d *DB) SetSyncedLabelsByDBID(id int64, syncedLabels string) error {
	result, err := d.db.Exec("UPDATE messages SET synced_labels = ? WHERE id = ?", syncedLabels, id)
	if err != nil {
		return fmt.Errorf("set synced labels by row: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("message row not found: %d", id)
	}
	return nil
}

// LabelStateRow is one message's label-upload state: its durable Message-ID,
// the provider ref to modify, the last-synced label baseline, and the current
// local tags. Used by the label three-way upload (Gmail/JMAP).
type LabelStateRow struct {
	RowID        int64
	MessageID    string
	RemoteRef    string
	SyncedLabels string
	Tags         []string
}

// GetLabelState returns the label-upload state for every message in the account
// that has a non-empty remote_ref, across all mailboxes (a label backend like
// Gmail keeps every message in one synthetic "All Mail" stream, so this is not
// scoped to a folder). Unknown account returns an empty slice without error.
func (d *DB) GetLabelState(account string) ([]LabelStateRow, error) {
	var accountID int64
	if err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return []LabelStateRow{}, nil
		}
		return nil, fmt.Errorf("lookup account id: %w", err)
	}

	// LEFT JOIN so a message with no tags still yields a row (its empty local
	// state matters to the three-way). ORDER BY m.id groups a message's tags.
	rows, err := d.db.Query(`
		SELECT m.id, m.message_id, m.remote_ref, m.synced_labels, IFNULL(t.tag, '')
		FROM messages m
		LEFT JOIN tags t ON t.message_id = m.id
		WHERE m.account_id = ? AND m.remote_ref != ''
		ORDER BY m.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("get label state: %w", err)
	}
	defer rows.Close()

	result := []LabelStateRow{}
	for rows.Next() {
		var rowID int64
		var msgID, remoteRef, syncedLabels, tag string
		if err := rows.Scan(&rowID, &msgID, &remoteRef, &syncedLabels, &tag); err != nil {
			return nil, fmt.Errorf("scan label state row: %w", err)
		}
		if n := len(result); n == 0 || result[n-1].RowID != rowID {
			result = append(result, LabelStateRow{
				RowID:        rowID,
				MessageID:    msgID,
				RemoteRef:    remoteRef,
				SyncedLabels: syncedLabels,
			})
		}
		if tag != "" {
			result[len(result)-1].Tags = append(result[len(result)-1].Tags, tag)
		}
	}
	return result, rows.Err()
}

// GetMessageIDByRemoteRef returns the Message-ID of the message in the given
// account+mailbox whose remote_ref matches, or "" if none. Used to resolve a
// backend deletion that carries only the provider handle (e.g. a Graph delta
// @removed item, which has no internetMessageId) to the durable key. Unknown
// account/mailbox or no match returns "" without an error.
func (d *DB) GetMessageIDByRemoteRef(account, mailbox, remoteRef string) (string, error) {
	if remoteRef == "" {
		return "", nil
	}
	if strings.EqualFold(mailbox, "INBOX") {
		mailbox = "INBOX"
	}
	var mailboxID int64
	if err := d.db.QueryRow("SELECT id FROM mailboxes WHERE name = ?", mailbox).Scan(&mailboxID); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("lookup mailbox id: %w", err)
	}
	var accountID int64
	if err := d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("lookup account id: %w", err)
	}
	var messageID string
	err := d.db.QueryRow(
		"SELECT message_id FROM messages WHERE mailbox_id = ? AND account_id = ? AND remote_ref = ? LIMIT 1",
		mailboxID, accountID, remoteRef).Scan(&messageID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("lookup message by remote_ref: %w", err)
	}
	return messageID, nil
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
		m.uid, m.size, m.fetched_body, m.remote_ref, m.stable_id`

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
		&msg.UID, &msg.Size, &fetchedBody, &msg.RemoteRef, &msg.StableID,
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
