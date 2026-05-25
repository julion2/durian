package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/durian-dev/durian/cli/internal/dbcrypto"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite database connection for the email store. The Keyring
// is held by reference so the store layer can transparently encrypt and
// decrypt the ADR-0001 §3 sensitive columns. Nil is not allowed — every
// caller must derive a keyring from the master key at startup.
type DB struct {
	db      *sql.DB
	keyring *dbcrypto.Keyring
}

// Open opens or creates an email store database at the given path.
// Use ":memory:" for in-memory databases (useful for testing).
//
// kr must be a non-nil Keyring derived from the OS-keychain master key.
// Pilot encryption (ADR-0001 step 5) requires the keyring for the v9→v10
// backfill and for every subsequent encrypted-column read/write.
func Open(dbPath string, kr *dbcrypto.Keyring) (*DB, error) {
	if kr == nil {
		return nil, fmt.Errorf("store: Open requires a non-nil keyring (see ADR-0001)")
	}
	if dbPath != ":memory:" {
		if strings.HasPrefix(dbPath, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("get home dir: %w", err)
			}
			dbPath = filepath.Join(home, dbPath[2:])
		}

		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, fmt.Errorf("chmod db directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite only supports one writer at a time, and :memory: databases
	// create a separate DB per connection. A single connection avoids both issues.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
		// ADR-0001 step 8: overwrite freed pages with zeros on DELETE /
		// UPDATE so the ciphertext-and-still-plaintext columns we keep
		// (from_addr, to_addrs, cc_addrs, message_id, dates, sizes,
		// is_seen / is_flagged / is_deleted) don't leak old values via
		// page-reuse delay to a forensic analyst with the raw .db file.
		// Encrypted columns are safe under page reuse anyway, but
		// secure_delete is the forward-looking guarantee for the
		// plaintext set documented in ADR §3 "Stays plaintext".
		"PRAGMA secure_delete = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("set pragma %q: %w", p, err)
		}
	}

	return &DB{db: db, keyring: kr}, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// Init creates all tables, indexes, triggers, and FTS5 virtual tables.
// It also runs any pending schema migrations.
//
// Statements are executed individually because trigger bodies contain
// semicolons that confuse multi-statement Exec parsing.
func (d *DB) Init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_version (
			version INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO schema_version (rowid, version) VALUES (1, 6)`,

		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			subject TEXT,
			from_addr TEXT,
			to_addrs TEXT,
			cc_addrs TEXT,
			date INTEGER,
			created_at INTEGER NOT NULL,
			body_text TEXT,
			body_html TEXT,
			mailbox TEXT,
			flags TEXT,
			uid INTEGER DEFAULT 0,
			size INTEGER DEFAULT 0,
			fetched_body INTEGER DEFAULT 0,
			account TEXT DEFAULT '',
			UNIQUE(message_id, account)
		)`,

		`CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_addr ON messages(from_addr)`,

		// idx_messages_mailbox / idx_messages_account, the old messages_fts
		// table, and the messages_ai / messages_ad / messages_au triggers
		// were removed from the fresh-install schema by ADR-0001 step 7f.
		// On an old DB they still exist at this point and get cleaned up
		// by the v17→v18 (FTS drop) and v18→v19 (column rebuild) migrations
		// below. Listing them here would only re-create them under
		// CREATE IF NOT EXISTS, then break the next Init() when their
		// referenced columns are gone.

		`CREATE TABLE IF NOT EXISTS tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			tag TEXT NOT NULL,
			UNIQUE(message_id, tag)
		)`,

		`CREATE INDEX IF NOT EXISTS idx_tags_message_id ON tags(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag)`,

		`CREATE TABLE IF NOT EXISTS attachments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_db_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			part_id INTEGER,
			filename TEXT,
			content_type TEXT,
			size INTEGER DEFAULT 0,
			disposition TEXT,
			content_id TEXT
		)`,

		`CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_db_id)`,

		`CREATE TABLE IF NOT EXISTS message_headers (
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			value TEXT NOT NULL,
			PRIMARY KEY (message_id, name)
		)`,

		`CREATE TABLE IF NOT EXISTS outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			draft_json TEXT NOT NULL,
			attempts INTEGER DEFAULT 0,
			last_error TEXT,
			created_at INTEGER NOT NULL,
			last_attempted_at INTEGER DEFAULT 0,
			send_after INTEGER DEFAULT 0
		)`,

		// local_drafts was first added in the v6→v7 migration. Listing it
		// here too keeps it created for any fresh DB regardless of which
		// version Init() lands on, so later ALTER TABLE migrations don't
		// trip on its absence in tests that seed schema_version > 7.
		`CREATE TABLE IF NOT EXISTS local_drafts (
			id TEXT PRIMARY KEY,
			draft_json TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			modified_at INTEGER NOT NULL
		)`,

		// mailboxes and accounts were first added in the v8→v9 migration
		// (ADR-0001 step 1). Same idempotency story as local_drafts —
		// keep them present here so seeds at schema_version > 9 don't
		// fail later ALTER TABLE migrations that target them.
		`CREATE TABLE IF NOT EXISTS mailboxes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		)`,
	}

	for _, stmt := range stmts {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}

	if err := d.migrate(); err != nil {
		return err
	}

	// Indexes that may have been dropped by migrations. Step 7f removed
	// idx_messages_account + idx_messages_mailbox along with the columns
	// they covered; the messages_ai / messages_ad / messages_au triggers
	// died with the rebuilt table (step 7e had already patched them to
	// SELECT-1 no-ops, and messages_fts itself is gone).
	postMigration := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_addr ON messages(from_addr)`,
	}
	for _, stmt := range postMigration {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("post-migration index: %w", err)
		}
	}

	return nil
}

// DefaultDBPath returns the default database path for the email store.
// Respects XDG_DATA_HOME, falls back to ~/.local/share/durian/email.db
func DefaultDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "durian", "email.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "durian", "email.db")
}

// migrate checks the current schema version and applies pending migrations.
func (d *DB) migrate() error {
	var version int
	err := d.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if version < 2 {
		migrations := []string{
			"ALTER TABLE messages ADD COLUMN account TEXT DEFAULT ''",
			"CREATE INDEX IF NOT EXISTS idx_messages_account ON messages(account)",
			"UPDATE schema_version SET version = 2 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v1→v2: %w", err)
			}
		}
		version = 2
	}

	if version < 3 {
		// Migrate UNIQUE(message_id) → UNIQUE(message_id, account).
		// Data is truncated — user must re-sync after upgrade.
		migrations := []string{
			`CREATE TABLE messages_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				message_id TEXT NOT NULL,
				thread_id TEXT NOT NULL,
				in_reply_to TEXT,
				refs TEXT,
				subject TEXT,
				from_addr TEXT,
				to_addrs TEXT,
				cc_addrs TEXT,
				date INTEGER,
				created_at INTEGER NOT NULL,
				body_text TEXT,
				body_html TEXT,
				mailbox TEXT,
				flags TEXT,
				uid INTEGER DEFAULT 0,
				size INTEGER DEFAULT 0,
				fetched_body INTEGER DEFAULT 0,
				account TEXT DEFAULT '',
				UNIQUE(message_id, account)
			)`,
			"DROP TABLE IF EXISTS messages",
			"ALTER TABLE messages_new RENAME TO messages",
			// Rebuild FTS5 (old content table was dropped)
			"DROP TABLE IF EXISTS messages_fts",
			`CREATE VIRTUAL TABLE messages_fts USING fts5(
				subject, from_addr, to_addrs, body_text,
				content='messages',
				content_rowid='id'
			)`,
			"UPDATE schema_version SET version = 3 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v2→v3: %w", err)
			}
		}
		version = 3
	}

	if version < 4 {
		_, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS message_headers (
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			value TEXT NOT NULL,
			PRIMARY KEY (message_id, name)
		)`)
		if err != nil {
			return fmt.Errorf("migrate v3→v4: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 4 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v3→v4 version bump: %w", err)
		}
	}

	if version < 5 {
		migrations := []string{
			"ALTER TABLE outbox ADD COLUMN last_attempted_at INTEGER DEFAULT 0",
			"UPDATE schema_version SET version = 5 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v4→v5: %w", err)
			}
		}
		version = 5
	}

	if version < 6 {
		migrations := []string{
			"ALTER TABLE outbox ADD COLUMN send_after INTEGER DEFAULT 0",
			"UPDATE schema_version SET version = 6 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v5→v6: %w", err)
			}
		}
		version = 6
	}

	if version < 7 {
		migrations := []string{
			`CREATE TABLE IF NOT EXISTS local_drafts (
				id TEXT PRIMARY KEY,
				draft_json TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				modified_at INTEGER NOT NULL
			)`,
			"UPDATE schema_version SET version = 7 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v6→v7: %w", err)
			}
		}
		version = 7
	}

	if version < 8 {
		migrations := []string{
			`CREATE TABLE IF NOT EXISTS tag_journal (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				message_id TEXT NOT NULL,
				account TEXT NOT NULL,
				tag TEXT NOT NULL,
				action TEXT NOT NULL,
				timestamp INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS metadata (
				key TEXT PRIMARY KEY,
				value INTEGER NOT NULL
			)`,
			"UPDATE schema_version SET version = 8 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v7→v8: %w", err)
			}
		}
		version = 8
	}

	if version < 9 {
		// ADR-0001 step 1: introduce mailbox/account lookup tables and
		// activity booleans derived from `flags`. Old messages.mailbox /
		// account / flags columns stay in place — this is the smallest
		// diff that adds the new join keys without breaking any existing
		// query path.
		//
		// TODO(ADR-0001): drop mailbox/account TEXT columns after step 5
		// and convert flags TEXT to flags_other BLOB (encrypted) once the
		// dbcrypto package lands.
		migrations := []string{
			`CREATE TABLE IF NOT EXISTS mailboxes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL UNIQUE
			)`,
			`CREATE TABLE IF NOT EXISTS accounts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL UNIQUE
			)`,
			"ALTER TABLE messages ADD COLUMN mailbox_id INTEGER REFERENCES mailboxes(id)",
			"ALTER TABLE messages ADD COLUMN account_id INTEGER REFERENCES accounts(id)",
			"ALTER TABLE messages ADD COLUMN is_seen INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE messages ADD COLUMN is_flagged INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE messages ADD COLUMN is_deleted INTEGER NOT NULL DEFAULT 0",

			// Populate mailboxes from distinct messages.mailbox. INBOX is
			// case-normalized to "INBOX" (RFC 3501 reserves it case-
			// insensitive); custom folders stay exact.
			`INSERT OR IGNORE INTO mailboxes (name)
			 SELECT DISTINCT
			   CASE WHEN UPPER(mailbox) = 'INBOX' THEN 'INBOX' ELSE mailbox END
			 FROM messages
			 WHERE mailbox IS NOT NULL AND mailbox <> ''`,

			// Populate accounts from distinct messages.account.
			`INSERT OR IGNORE INTO accounts (name)
			 SELECT DISTINCT account FROM messages
			 WHERE account IS NOT NULL AND account <> ''`,

			// Backfill FK columns. The CASE handles INBOX normalization
			// symmetrically with the INSERT above so 'inbox' / 'Inbox' /
			// 'INBOX' all resolve to the same mailbox_id.
			`UPDATE messages
			 SET mailbox_id = (
			   SELECT id FROM mailboxes
			   WHERE name = CASE WHEN UPPER(messages.mailbox) = 'INBOX'
			                     THEN 'INBOX' ELSE messages.mailbox END
			 )
			 WHERE mailbox IS NOT NULL AND mailbox <> ''`,

			`UPDATE messages
			 SET account_id = (SELECT id FROM accounts WHERE name = messages.account)
			 WHERE account IS NOT NULL AND account <> ''`,

			// Derive activity booleans from existing flags TEXT. INSTR is
			// used over LIKE to avoid SQLite's backslash-escaping ambiguity
			// with the leading '\' in standard IMAP flags.
			`UPDATE messages SET
			   is_seen    = CASE WHEN INSTR(IFNULL(flags, ''), '\Seen')    > 0 THEN 1 ELSE 0 END,
			   is_flagged = CASE WHEN INSTR(IFNULL(flags, ''), '\Flagged') > 0 THEN 1 ELSE 0 END,
			   is_deleted = CASE WHEN INSTR(IFNULL(flags, ''), '\Deleted') > 0 THEN 1 ELSE 0 END`,

			"CREATE INDEX IF NOT EXISTS idx_messages_mailbox_id ON messages(mailbox_id)",
			"CREATE INDEX IF NOT EXISTS idx_messages_account_id ON messages(account_id)",

			"UPDATE schema_version SET version = 9 WHERE rowid = 1",
		}
		for _, stmt := range migrations {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v8→v9: %w", err)
			}
		}
		version = 9
	}

	if version < 10 {
		// ADR-0001 step 5: pilot encryption of messages.subject. Add the
		// subject_ct BLOB column and encrypt every existing plaintext
		// subject into it. The plaintext `subject` column stays in place
		// for now — FTS5 still indexes it, and the blind-token FTS5
		// rebuild plus plaintext drop both land in step 7.
		//
		// ALTER TABLE cannot be wrapped in a transaction with the backfill
		// (SQLite implicitly commits before DDL), so the column-add is
		// idempotent: if a previous attempt added the column but failed in
		// the backfill, we re-detect it via PRAGMA and skip straight to
		// the backfill. The backfill is itself a transaction that rolls
		// back cleanly on failure.
		has, err := hasColumn(d.db, "messages", "subject_ct")
		if err != nil {
			return fmt.Errorf("migrate v9→v10 inspect: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE messages ADD COLUMN subject_ct BLOB"); err != nil {
				return fmt.Errorf("migrate v9→v10 add column: %w", err)
			}
		}
		if err := d.backfillSubjectCt(); err != nil {
			return fmt.Errorf("migrate v9→v10 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 10 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v9→v10 bump: %w", err)
		}
		version = 10
	}

	if version < 11 {
		// ADR-0001 step 6: extend pilot encryption to messages.body_text
		// and messages.body_html. Same pattern as v10 (subject) — two new
		// BLOB columns, plaintext columns stay so FTS5 keeps indexing
		// body_text until the step-7 rebuild.
		for _, col := range []string{"body_text_ct", "body_html_ct"} {
			has, err := hasColumn(d.db, "messages", col)
			if err != nil {
				return fmt.Errorf("migrate v10→v11 inspect %s: %w", col, err)
			}
			if !has {
				if _, err := d.db.Exec("ALTER TABLE messages ADD COLUMN " + col + " BLOB"); err != nil {
					return fmt.Errorf("migrate v10→v11 add %s: %w", col, err)
				}
			}
		}
		if err := d.backfillBodyCt(); err != nil {
			return fmt.Errorf("migrate v10→v11 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 11 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v10→v11 bump: %w", err)
		}
		version = 11
	}

	if version < 12 {
		// ADR-0001 step 6: extend encryption to messages.from_addr,
		// to_addrs, cc_addrs (addrs_key). Same column-doubling pattern;
		// plaintext columns stay so FTS5 keeps indexing from_addr/to_addrs
		// and search.go's GROUP_CONCAT(from_addr) AS authors keeps working
		// until step 7.
		//
		// idx_messages_from_addr is intentionally NOT dropped yet — the
		// plaintext column it covers still exists and powers GetSenderCounts
		// + search. ADR-0001 §3 schedules that index drop for step 7 when
		// the plaintext column itself goes.
		for _, col := range []string{"from_addr_ct", "to_addrs_ct", "cc_addrs_ct"} {
			has, err := hasColumn(d.db, "messages", col)
			if err != nil {
				return fmt.Errorf("migrate v11→v12 inspect %s: %w", col, err)
			}
			if !has {
				if _, err := d.db.Exec("ALTER TABLE messages ADD COLUMN " + col + " BLOB"); err != nil {
					return fmt.Errorf("migrate v11→v12 add %s: %w", col, err)
				}
			}
		}
		if err := d.backfillAddrsCt(); err != nil {
			return fmt.Errorf("migrate v11→v12 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 12 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v11→v12 bump: %w", err)
		}
		version = 12
	}

	if version < 13 {
		// ADR-0001 step 6: encrypt message_headers.value (headers_key).
		// Header `name` stays plaintext — already a public registry
		// (Return-Path, Authentication-Results, List-Unsubscribe, etc.)
		// per ADR-0001 §3, and we need it for rule matching joins.
		has, err := hasColumn(d.db, "message_headers", "value_ct")
		if err != nil {
			return fmt.Errorf("migrate v12→v13 inspect value_ct: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE message_headers ADD COLUMN value_ct BLOB"); err != nil {
				return fmt.Errorf("migrate v12→v13 add value_ct: %w", err)
			}
		}
		if err := d.backfillHeaderValueCt(); err != nil {
			return fmt.Errorf("migrate v12→v13 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 13 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v12→v13 bump: %w", err)
		}
		version = 13
	}

	if version < 14 {
		// ADR-0001 step 6: encrypt local_drafts.draft_json and
		// outbox.draft_json (draft_key). Two separate tables, same
		// sub-key — both hold pending outgoing mail, both deserve at-rest
		// protection. Schema is parallel ALTER on each table.
		for _, table := range []string{"local_drafts", "outbox"} {
			has, err := hasColumn(d.db, table, "draft_json_ct")
			if err != nil {
				return fmt.Errorf("migrate v13→v14 inspect %s.draft_json_ct: %w", table, err)
			}
			if !has {
				if _, err := d.db.Exec("ALTER TABLE " + table + " ADD COLUMN draft_json_ct BLOB"); err != nil {
					return fmt.Errorf("migrate v13→v14 add %s.draft_json_ct: %w", table, err)
				}
			}
		}
		if err := d.backfillDraftJSONCt(); err != nil {
			return fmt.Errorf("migrate v13→v14 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 14 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v13→v14 bump: %w", err)
		}
		version = 14
	}

	if version < 15 {
		// ADR-0001 step 6: encrypt the meta_key columns — mailbox names
		// (folder labels), account names, and the non-boolean parts of
		// messages.flags. Plaintext columns stay alongside until step 7;
		// reads keep using them. This commit just lays down the cipher-
		// text columns plus encrypt-on-write for messages.flags_other.
		//
		// mailboxes.name and accounts.name have UNIQUE constraints that
		// the encrypted (random-nonce) form cannot replicate, so the
		// plaintext UNIQUE column stays as the lookup key until step 7
		// introduces deterministic blind-tokens.
		alters := []struct{ table, col string }{
			{"mailboxes", "name_ct"},
			{"accounts", "name_ct"},
			{"messages", "flags_other"},
		}
		for _, a := range alters {
			has, err := hasColumn(d.db, a.table, a.col)
			if err != nil {
				return fmt.Errorf("migrate v14→v15 inspect %s.%s: %w", a.table, a.col, err)
			}
			if !has {
				if _, err := d.db.Exec("ALTER TABLE " + a.table + " ADD COLUMN " + a.col + " BLOB"); err != nil {
					return fmt.Errorf("migrate v14→v15 add %s.%s: %w", a.table, a.col, err)
				}
			}
		}
		if err := d.backfillMetaCt(); err != nil {
			return fmt.Errorf("migrate v14→v15 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 15 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v14→v15 bump: %w", err)
		}
		version = 15
	}

	if version < 16 {
		// ADR-0001 step 7 (a+b): blind-token FTS5 infrastructure.
		// Stand up a parallel contentless FTS5 table that indexes
		// HMAC-truncated tokens (no plaintext leaves the encrypted
		// column world). The existing messages_fts table keeps serving
		// search.go reads until step 7c flips the search path.
		//
		// contentless_delete=1 lets us DELETE FROM the FTS table by
		// rowid (no FTS-specific 'delete' sentinel insert) — needed
		// for the messages-DELETE trigger to stay simple.
		stmts := []string{
			`CREATE VIRTUAL TABLE IF NOT EXISTS messages_blind_fts USING fts5(
				subject_tok, from_tok, to_tok, body_tok,
				content='',
				contentless_delete=1,
				tokenize='unicode61 remove_diacritics 0'
			)`,
			// On messages DELETE, drop the parallel FTS row by rowid.
			// INSERT/UPDATE maintenance happens Go-side (insertMessageTx /
			// UpdateBody) since we need the Go tokenizer with the
			// fts_token sub-key, which SQLite triggers can't reach.
			`CREATE TRIGGER IF NOT EXISTS messages_blind_fts_ad AFTER DELETE ON messages BEGIN
				DELETE FROM messages_blind_fts WHERE rowid = old.id;
			END`,
		}
		for _, s := range stmts {
			if _, err := d.db.Exec(s); err != nil {
				return fmt.Errorf("migrate v15→v16 schema: %w", err)
			}
		}
		if err := d.backfillBlindFTS(); err != nil {
			return fmt.Errorf("migrate v15→v16 backfill: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 16 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v15→v16 bump: %w", err)
		}
		version = 16
	}

	if version < 17 {
		// ADR-0001 step 7d (prep for 7e): drop the dead from_addr_ct /
		// to_addrs_ct / cc_addrs_ct columns the v11→v12 migration added.
		// During that step the addresses were on the encrypt-at-rest
		// roadmap; ADR-0001 §3 was updated to keep them plaintext
		// (substring search UX outweighs the asymmetric protection vs.
		// wire-plaintext leak). The BLOB columns are written but never
		// read, so dropping them is pure cleanup. SQLite 3.35+ supports
		// ALTER TABLE DROP COLUMN; idempotent via PRAGMA table_info.
		for _, col := range []string{"from_addr_ct", "to_addrs_ct", "cc_addrs_ct"} {
			has, err := hasColumn(d.db, "messages", col)
			if err != nil {
				return fmt.Errorf("migrate v16→v17 inspect %s: %w", col, err)
			}
			if has {
				if _, err := d.db.Exec("ALTER TABLE messages DROP COLUMN " + col); err != nil {
					return fmt.Errorf("migrate v16→v17 drop %s: %w", col, err)
				}
			}
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 17 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v16→v17 bump: %w", err)
		}
		version = 17
	}

	if version < 18 {
		// ADR-0001 step 7e: drop the now-dead plaintext columns whose
		// encrypted *_ct counterparts have been written and read by every
		// path since steps 6 / 7c. messages.mailbox / account / flags
		// stay alive — they still drive lookups; step 7f handles those.
		//
		// Also drops the old messages_fts external-content FTS5 table
		// plus its insert/update/delete triggers. messages_blind_fts has
		// served every search read since step 7c; the parallel index
		// is dead weight.
		// modernc.org/sqlite has a quirk where DROP TRIGGER fired from
		// within Init's migrate() loop is silently no-op (verified by
		// step-7e debug: same SQL works post-Init, fails mid-migration).
		// Workaround: patch the trigger bodies in sqlite_master directly
		// to a no-op SELECT before dropping the table, so the now-gone
		// columns and table they reference can never be touched. Then
		// drop the old FTS5 table.
		patch := []string{
			`PRAGMA writable_schema = ON`,
			`UPDATE sqlite_master
			 SET sql = 'CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN SELECT 1; END'
			 WHERE type='trigger' AND name='messages_ai'`,
			`UPDATE sqlite_master
			 SET sql = 'CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN SELECT 1; END'
			 WHERE type='trigger' AND name='messages_ad'`,
			`UPDATE sqlite_master
			 SET sql = 'CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN SELECT 1; END'
			 WHERE type='trigger' AND name='messages_au'`,
			`PRAGMA writable_schema = OFF`,
			`DROP TABLE IF EXISTS messages_fts`,
		}
		for _, s := range patch {
			if _, err := d.db.Exec(s); err != nil {
				return fmt.Errorf("migrate v17→v18 trigger patch: %w", err)
			}
		}

		// Idempotent column drops via PRAGMA table_info — partial
		// migration on retry skips already-dropped columns.
		drops := []struct{ table, col string }{
			{"messages", "subject"},
			{"messages", "body_text"},
			{"messages", "body_html"},
			{"message_headers", "value"},
			{"local_drafts", "draft_json"},
			{"outbox", "draft_json"},
		}
		for _, d2 := range drops {
			has, err := hasColumn(d.db, d2.table, d2.col)
			if err != nil {
				return fmt.Errorf("migrate v17→v18 inspect %s.%s: %w", d2.table, d2.col, err)
			}
			if has {
				if _, err := d.db.Exec("ALTER TABLE " + d2.table + " DROP COLUMN " + d2.col); err != nil {
					return fmt.Errorf("migrate v17→v18 drop %s.%s: %w", d2.table, d2.col, err)
				}
			}
		}


		// VACUUM rebuilds the DB file to reclaim space from the dropped
		// columns + FTS5 table. On a 20k-message mailbox this rewrites
		// ~900 MB (the body plaintext duplication that's been sitting
		// alongside body_text_ct since step 6). Slow — typically 30-90 s
		// on a warm filesystem cache — but one-shot, after which the
		// 1.8 GB DB should shrink back near the pre-encryption ~980 MB
		// baseline.
		//
		// ADR-0001 audit H3: VACUUM runs BEFORE the schema_version bump.
		// Earlier versions bumped first; a VACUUM crash (out of disk,
		// interrupt, power loss) then left the DB stuck at v18 with the
		// dropped columns' plaintext bytes still in free pages, and no
		// migration code ever revisiting VACUUM. The v19→v20 migration
		// below catches users who already crossed v18 under the old
		// ordering and re-VACUUMs idempotently.
		if _, err := d.db.Exec("VACUUM"); err != nil {
			return fmt.Errorf("migrate v17→v18 vacuum: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 18 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v17→v18 bump: %w", err)
		}
	}

	if version < 19 {
		// ADR-0001 step 7f: drop the messages.mailbox / messages.account /
		// messages.flags plaintext shadow columns. The structured
		// replacements (mailbox_id FK, account_id FK, is_seen / is_flagged
		// / is_deleted booleans, encrypted flags_other BLOB) have been
		// alive since step 1 / step 6 but the INSERT path has been writing
		// only the plaintext shadows — so before dropping anything we
		// re-run the step-1 backfill for any row that the steady-state
		// insert path left with NULL FK ids or zero booleans.
		if err := d.backfillMessageMetaShadows(); err != nil {
			return fmt.Errorf("migrate v18→v19 backfill: %w", err)
		}

		// DROP COLUMN cannot remove a column that participates in a UNIQUE
		// constraint, and account is half of UNIQUE(message_id, account).
		// SQLite's recommended escape hatch (https://www.sqlite.org/lang_altertable.html
		// 12-step procedure) is a full table-rebuild: CREATE table with
		// the desired schema, INSERT SELECT, DROP old, RENAME. FK refs
		// from tags / attachments / message_headers / messages_blind_fts
		// to messages.id survive the rebuild because we leave foreign_keys
		// OFF for the duration and the FK lookups are by table name (which
		// the final RENAME preserves).
		if err := d.rebuildMessagesForStep7f(); err != nil {
			return fmt.Errorf("migrate v18→v19 rebuild: %w", err)
		}

		if _, err := d.db.Exec("UPDATE schema_version SET version = 19 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v18→v19 bump: %w", err)
		}
	}

	if version < 20 {
		// ADR-0001 audit H3 follow-up: users who crossed v18 under the
		// pre-fix bump-then-VACUUM ordering may have had VACUUM fail or
		// be killed between the bump and the rewrite; the dropped step-7e
		// plaintext bytes (subject / body_text / body_html / message_headers
		// .value / draft_json) would have stayed stranded in free pages
		// forever, defeating the at-rest encryption story for any cold
		// filesystem image taken thereafter. Backups taken from such a
		// DB carry the same residue. One-time idempotent re-VACUUM
		// scrubs the free pages either way; on a clean v18→v19 install
		// it is a relatively cheap no-op rewrite.
		//
		// VACUUM runs BEFORE the schema_version bump (lesson from H3
		// itself): if it crashes, we want v20 to NOT be marked done so
		// the next Init() retries it.
		if _, err := d.db.Exec("VACUUM"); err != nil {
			return fmt.Errorf("migrate v19→v20 vacuum: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 20 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v19→v20 bump: %w", err)
		}
	}

	return nil
}

// backfillMessageMetaShadows populates mailbox_id / account_id / is_seen /
// is_flagged / is_deleted / flags_other for any row that the post-step-1
// INSERT path left with NULL FKs or zero booleans. Idempotent — rows
// already populated are filtered out of every UPDATE's WHERE clause.
// Runs inside the v18→v19 migration just before the table-rebuild that
// drops the source columns.
func (d *DB) backfillMessageMetaShadows() error {
	// Populate mailboxes/accounts from any plaintext shadow value that's
	// not yet represented. INBOX gets the same case normalization as the
	// step-1 backfill so 'inbox' / 'Inbox' / 'INBOX' merge.
	pre := []string{
		`INSERT OR IGNORE INTO mailboxes (name)
		 SELECT DISTINCT
		   CASE WHEN UPPER(mailbox) = 'INBOX' THEN 'INBOX' ELSE mailbox END
		 FROM messages
		 WHERE mailbox IS NOT NULL AND mailbox <> '' AND mailbox_id IS NULL`,
		`INSERT OR IGNORE INTO accounts (name)
		 SELECT DISTINCT account FROM messages
		 WHERE account IS NOT NULL AND account <> '' AND account_id IS NULL`,
		`UPDATE messages
		 SET mailbox_id = (
		   SELECT id FROM mailboxes
		   WHERE name = CASE WHEN UPPER(messages.mailbox) = 'INBOX'
		                     THEN 'INBOX' ELSE messages.mailbox END
		 )
		 WHERE mailbox IS NOT NULL AND mailbox <> '' AND mailbox_id IS NULL`,
		`UPDATE messages
		 SET account_id = (SELECT id FROM accounts WHERE name = messages.account)
		 WHERE account IS NOT NULL AND account <> '' AND account_id IS NULL`,
		// Re-derive activity booleans for any row where they're still at
		// the default 0 but flags TEXT actually carries one of the system
		// flags (catches rows synced post-step-1 with the old insert path).
		`UPDATE messages SET
		   is_seen    = CASE WHEN INSTR(IFNULL(flags, ''), '\Seen')    > 0 THEN 1 ELSE is_seen END,
		   is_flagged = CASE WHEN INSTR(IFNULL(flags, ''), '\Flagged') > 0 THEN 1 ELSE is_flagged END,
		   is_deleted = CASE WHEN INSTR(IFNULL(flags, ''), '\Deleted') > 0 THEN 1 ELSE is_deleted END
		 WHERE flags IS NOT NULL AND flags <> ''`,
	}
	for _, stmt := range pre {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("pre-rebuild backfill: %w", err)
		}
	}

	// Mailboxes/accounts rows freshly created above need their name_ct
	// populated too — the v14→v15 backfill ran before they existed.
	// Encrypt-on-read isn't an option; the SELECT path JOINs name_ct.
	if err := d.encryptMissingMetaNames(); err != nil {
		return fmt.Errorf("encrypt new meta names: %w", err)
	}

	// flags_other_ct backfill for any row with non-empty plaintext flags
	// but a NULL flags_other (rows synced after step 1 but before step 6
	// landed encrypt-on-write).
	if err := d.backfillFlagsOtherFromShadow(); err != nil {
		return fmt.Errorf("flags_other backfill: %w", err)
	}
	return nil
}

// encryptMissingMetaNames writes name_ct for any mailboxes / accounts
// row that has plaintext name but no encrypted name_ct yet. Covers the
// freshly-created rows from backfillMessageMetaShadows. Idempotent.
func (d *DB) encryptMissingMetaNames() error {
	if d.keyring == nil || d.keyring.Meta == nil {
		return fmt.Errorf("no keyring for meta name encrypt")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, table := range []string{"mailboxes", "accounts"} {
		rows, err := tx.Query(`SELECT id, name FROM ` + table + `
			WHERE name IS NOT NULL AND name <> '' AND name_ct IS NULL`)
		if err != nil {
			return fmt.Errorf("select %s: %w", table, err)
		}
		type pending struct {
			id   int64
			name string
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.name); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s: %w", table, err)
			}
			batch = append(batch, p)
		}
		rows.Close()
		stmt, err := tx.Prepare("UPDATE " + table + " SET name_ct = ? WHERE id = ?")
		if err != nil {
			return fmt.Errorf("prepare %s update: %w", table, err)
		}
		for _, p := range batch {
			ct, err := d.encryptMeta(p.name)
			if err != nil {
				stmt.Close()
				return fmt.Errorf("encrypt %s name id=%d: %w", table, p.id, err)
			}
			if _, err := stmt.Exec(ct, p.id); err != nil {
				stmt.Close()
				return fmt.Errorf("update %s id=%d: %w", table, p.id, err)
			}
		}
		stmt.Close()
	}
	return tx.Commit()
}

// backfillFlagsOtherFromShadow encrypts any messages row's plaintext
// flags TEXT (minus the boolean-covered system flags) into flags_other
// when the latter is still NULL. Idempotent. Mirrors backfillMetaCt
// but only targets rows still showing the pre-step-6 NULL.
func (d *DB) backfillFlagsOtherFromShadow() error {
	if d.keyring == nil || d.keyring.Meta == nil {
		return fmt.Errorf("no keyring for flags_other backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.Query(`SELECT id, flags FROM messages
		WHERE flags IS NOT NULL AND flags <> '' AND flags_other IS NULL`)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	type pending struct {
		id    int64
		flags string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.flags); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	stmt, err := tx.Prepare("UPDATE messages SET flags_other = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		ct, err := d.encryptMeta(flagsOtherForEncryption(p.flags))
		if err != nil {
			return fmt.Errorf("encrypt id=%d: %w", p.id, err)
		}
		if _, err := stmt.Exec(ct, p.id); err != nil {
			return fmt.Errorf("update id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// rebuildMessagesForStep7f rebuilds the messages table to drop the
// plaintext mailbox / account / flags columns and replace the
// UNIQUE(message_id, account) constraint with UNIQUE(message_id,
// account_id). SQLite's 12-step ALTER procedure.
//
// ADR-0001 audit H4: idempotent on retry. Earlier versions used a bare
// CREATE TABLE messages_new without IF NOT EXISTS — a mid-stream
// failure (OOM / disk-full / power loss during the multi-GB INSERT
// SELECT) left the half-built temp table behind, and the next Init()
// crashed at CREATE TABLE with "table already exists" → migration
// permanently wedged. We now DROP the temp table at entry and wrap
// the whole rebuild in a transaction so partial states roll back
// cleanly.
func (d *DB) rebuildMessagesForStep7f() error {
	// DROP any half-built messages_new from a previous interrupted
	// attempt. Outside the transaction — if it errors (it shouldn't,
	// IF EXISTS is non-failing) we want to surface that before any
	// destructive operation. Outside the BEGIN/COMMIT block because
	// SQLite implicit-commits on DDL anyway, so wrapping it in the tx
	// would only obscure the ordering.
	if _, err := d.db.Exec(`DROP TABLE IF EXISTS messages_new`); err != nil {
		return fmt.Errorf("rebuild pre-cleanup: %w", err)
	}
	stmts := []string{
		`PRAGMA foreign_keys = OFF`,
		// The new table mirrors the current messages schema minus the
		// three doomed columns. Column order chosen to keep CREATE TABLE
		// readable rather than to match the in-place order — INSERT
		// SELECT names columns explicitly so order is irrelevant.
		`CREATE TABLE messages_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			in_reply_to TEXT,
			refs TEXT,
			from_addr TEXT,
			to_addrs TEXT,
			cc_addrs TEXT,
			date INTEGER,
			created_at INTEGER NOT NULL,
			uid INTEGER DEFAULT 0,
			size INTEGER DEFAULT 0,
			fetched_body INTEGER DEFAULT 0,
			mailbox_id INTEGER REFERENCES mailboxes(id),
			account_id INTEGER REFERENCES accounts(id),
			is_seen INTEGER NOT NULL DEFAULT 0,
			is_flagged INTEGER NOT NULL DEFAULT 0,
			is_deleted INTEGER NOT NULL DEFAULT 0,
			subject_ct BLOB,
			body_text_ct BLOB,
			body_html_ct BLOB,
			flags_other BLOB
		)`,
		`INSERT INTO messages_new (
			id, message_id, thread_id, in_reply_to, refs,
			from_addr, to_addrs, cc_addrs,
			date, created_at, uid, size, fetched_body,
			mailbox_id, account_id, is_seen, is_flagged, is_deleted,
			subject_ct, body_text_ct, body_html_ct, flags_other
		) SELECT
			id, message_id, thread_id, in_reply_to, refs,
			from_addr, to_addrs, cc_addrs,
			date, created_at, uid, size, fetched_body,
			mailbox_id, account_id, is_seen, is_flagged, is_deleted,
			subject_ct, body_text_ct, body_html_ct, flags_other
		FROM messages`,
		// modernc.org/sqlite step 7e quirk: DROP TRIGGER fired inside
		// migrate() silently no-ops. The patched-to-no-op triggers
		// (messages_ai / messages_ad / messages_au + messages_blind_fts_ad)
		// disappear with the table drop, which works fine, but we have
		// to re-create the blind-FTS trigger against the renamed table
		// below since DROP TABLE takes its triggers with it.
		`DROP TABLE messages`,
		`ALTER TABLE messages_new RENAME TO messages`,
		// Recreate the indexes that survived step 7e (idx_messages_mailbox
		// + idx_messages_account died with the columns they covered).
		`CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_addr ON messages(from_addr)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox_id ON messages(mailbox_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_account_id ON messages(account_id)`,
		// Replacement for the old UNIQUE(message_id, account) inline
		// constraint. account_id can legitimately be NULL (empty Account
		// on the Message struct → NULL FK), and SQLite treats NULLs as
		// distinct under a plain UNIQUE — which would silently allow
		// duplicate (message_id, NULL) rows and break the ON CONFLICT
		// upsert path. Wrapping in IFNULL collapses NULL to the sentinel 0.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_msgid_acctid_uniq
			ON messages(message_id, IFNULL(account_id, 0))`,
		// Re-attach the blind-FTS delete trigger so messages_blind_fts
		// stays in sync. INSERT/UPDATE maintenance still happens Go-side.
		`CREATE TRIGGER IF NOT EXISTS messages_blind_fts_ad AFTER DELETE ON messages BEGIN
			DELETE FROM messages_blind_fts WHERE rowid = old.id;
		END`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA foreign_key_check`,
	}
	for _, s := range stmts {
		if _, err := d.db.Exec(s); err != nil {
			return fmt.Errorf("rebuild step %q: %w", s[:min(60, len(s))], err)
		}
	}
	if _, err := d.db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("rebuild vacuum: %w", err)
	}
	return nil
}

// hasColumn returns true if the named column already exists on table.
// Used to make ALTER TABLE ADD COLUMN steps idempotent after a partial
// migration (SQLite ALTER has no IF NOT EXISTS).
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// encryptSubject seals plain with the subject sub-key and returns the
// envelope blob. An empty plaintext maps to a nil BLOB so the column
// stays distinguishable from "encrypted empty string" — readers fall
// back to the plaintext column when subject_ct IS NULL.
func (d *DB) encryptSubject(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Subject, []byte(plain))
}

// decryptSubject returns the plaintext subject for a row. It prefers the
// encrypted ciphertext when present and only falls back to the plaintext
// column for legacy rows the v9→v10 backfill hasn't reached. A non-nil
// subject_ct that fails to decrypt is a hard error — a silent fallback
// would mask key/cipher mismatches that turn into corruption in step 7.
func (d *DB) decryptSubject(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Subject, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt subject: %w", err)
	}
	return string(out), nil
}

// backfillSubjectCt encrypts every existing messages.subject into the new
// subject_ct BLOB column. Run inside a single transaction so a mid-loop
// failure rolls back and leaves the schema_version unbumped — next Init()
// retries the whole migration.
//
// Empty/NULL subjects are skipped to keep subject_ct = NULL distinguishable
// from "encrypted empty string" — readers fall back to the plaintext column
// when subject_ct IS NULL.
func (d *DB) backfillSubjectCt() error {
	if d.keyring == nil || d.keyring.Subject == nil {
		return fmt.Errorf("no keyring available for subject backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rolled back if Commit not reached

	rows, err := tx.Query(`SELECT id, subject FROM messages
		WHERE subject IS NOT NULL AND subject <> '' AND subject_ct IS NULL`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		id      int64
		subject string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.subject); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE messages SET subject_ct = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		ct, err := dbcrypto.Encrypt(d.keyring.Subject, []byte(p.subject))
		if err != nil {
			return fmt.Errorf("encrypt id=%d: %w", p.id, err)
		}
		if _, err := stmt.Exec(ct, p.id); err != nil {
			return fmt.Errorf("update id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// encryptBody seals plain with the body sub-key. Empty plaintext maps to
// nil so the ct column stays NULL — same convention as encryptSubject.
func (d *DB) encryptBody(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Body, []byte(plain))
}

// decryptBody returns the plaintext body for a row, preferring the
// encrypted ciphertext when present and falling back to the plaintext
// column only when ct IS NULL (legacy/unmigrated rows). Decrypt failures
// on a non-nil ct are hard errors — silent fallback would mask a
// key/cipher mismatch and become data loss in step 7.
func (d *DB) decryptBody(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Body, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt body: %w", err)
	}
	return string(out), nil
}

// backfillBodyCt encrypts every existing messages.body_text and
// body_html into the new BLOB columns. Single transaction so a mid-loop
// failure rolls back and leaves schema_version unbumped — next Init()
// retries the migration from scratch.
//
// Bodies can be large (KB to MB); we still load the whole batch into RAM
// before the UPDATE pass to keep the row + tx-prepared-statement lifetimes
// simple. Memory cap on a typical mailbox (~50k rows × ~50 KB avg) is in
// the hundreds-of-MB range, acceptable for a one-shot migration.
func (d *DB) backfillBodyCt() error {
	if d.keyring == nil || d.keyring.Body == nil {
		return fmt.Errorf("no keyring available for body backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rolled back if Commit not reached

	rows, err := tx.Query(`SELECT id, body_text, body_html FROM messages
		WHERE (body_text IS NOT NULL AND body_text <> '' AND body_text_ct IS NULL)
		   OR (body_html IS NOT NULL AND body_html <> '' AND body_html_ct IS NULL)`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		id       int64
		bodyText string
		bodyHTML string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.bodyText, &p.bodyHTML); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE messages SET body_text_ct = ?, body_html_ct = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		textCT, err := d.encryptBody(p.bodyText)
		if err != nil {
			return fmt.Errorf("encrypt body_text id=%d: %w", p.id, err)
		}
		htmlCT, err := d.encryptBody(p.bodyHTML)
		if err != nil {
			return fmt.Errorf("encrypt body_html id=%d: %w", p.id, err)
		}
		if _, err := stmt.Exec(textCT, htmlCT, p.id); err != nil {
			return fmt.Errorf("update id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// blindTokens returns the four space-separated FTS5-ready token strings
// for one message in the order (subject_tok, from_tok, to_tok, body_tok).
// Each field is run through dbcrypto.TokenizeFTS under the FTSToken
// sub-key. Empty plaintext maps to "" — FTS5 stores empty columns fine.
func (d *DB) blindTokens(subject, fromAddr, toAddrs, bodyText string) (sTok, fTok, tTok, bTok string) {
	k := d.keyring.FTSToken
	return dbcrypto.TokenizeFTS(k, subject),
		dbcrypto.TokenizeFTS(k, fromAddr),
		dbcrypto.TokenizeFTS(k, toAddrs),
		dbcrypto.TokenizeFTS(k, bodyText)
}

// backfillBlindFTS populates messages_blind_fts for every existing row.
// Reads plaintext (still present until step 7e) via the encrypted-ct
// columns where available so the tokenization is identical to what
// step-7c reads will produce after the plaintext columns die.
//
// Streams via row iteration rather than batching into RAM because body
// payloads can be MB-sized on real mail and the working set across 20k+
// messages would otherwise top a gigabyte.
func (d *DB) backfillBlindFTS() error {
	if d.keyring == nil || d.keyring.FTSToken == nil {
		return fmt.Errorf("no keyring available for blind FTS backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Skip rows that already have a blind FTS entry (idempotent retry).
	// Joining against messages_blind_fts directly is awkward (contentless
	// FTS5 doesn't expose a real table), so use NOT EXISTS on rowid.
	// COALESCE the plaintext columns to '' — early-schema messages can
	// have NULL subject/from/to/body where the new schema treats them as
	// empty strings. NULL would error scanning into a *string.
	// from_addr/to_addrs are plaintext per ADR-0001 §3 revision; no
	// decrypt needed here.
	rows, err := tx.Query(`SELECT m.id,
		COALESCE(m.subject,   ''), m.subject_ct,
		COALESCE(m.from_addr, ''),
		COALESCE(m.to_addrs,  ''),
		COALESCE(m.body_text, ''), m.body_text_ct
		FROM messages m
		WHERE NOT EXISTS (SELECT 1 FROM messages_blind_fts WHERE rowid = m.id)`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		id                                  int64
		subject, fromAddr, toAddrs, bodyText string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		var sCT, bCT []byte
		if err := rows.Scan(&p.id, &p.subject, &sCT, &p.fromAddr, &p.toAddrs, &p.bodyText, &bCT); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		if p.subject, err = d.decryptSubject(p.subject, sCT); err != nil {
			rows.Close()
			return fmt.Errorf("decrypt subject id=%d: %w", p.id, err)
		}
		if p.bodyText, err = d.decryptBody(p.bodyText, bCT); err != nil {
			rows.Close()
			return fmt.Errorf("decrypt body_text id=%d: %w", p.id, err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO messages_blind_fts(rowid, subject_tok, from_tok, to_tok, body_tok)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		sTok, fTok, tTok, bTok := d.blindTokens(p.subject, p.fromAddr, p.toAddrs, p.bodyText)
		if _, err := stmt.Exec(p.id, sTok, fTok, tTok, bTok); err != nil {
			return fmt.Errorf("insert id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// encryptMeta seals plain with the meta sub-key (mailbox/account names,
// non-boolean flag parts). Empty plaintext maps to nil → NULL column.
func (d *DB) encryptMeta(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Meta, []byte(plain))
}

// decryptMeta returns plaintext for a meta_key column, falling back to
// the plaintext column when ct IS NULL. Decrypt failures on non-nil ct
// are hard errors. Not yet called by production reads — step 7 will wire
// the lookup paths to prefer ct over the plaintext columns that die then.
func (d *DB) decryptMeta(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Meta, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt meta: %w", err)
	}
	return string(out), nil
}

// flagsOtherForEncryption returns the subset of a space-separated IMAP
// flags string that ADR-0001 §3 marks for meta_key encryption: everything
// except the three boolean-tracked standard flags (\Seen, \Flagged,
// \Deleted) that already live as O(1) integer columns. The remaining
// flags include \Answered, \Draft, \Recent, $Sensitive and any
// user-defined keywords — all preserved verbatim in their original order.
func flagsOtherForEncryption(flags string) string {
	if flags == "" {
		return ""
	}
	parts := strings.Fields(flags)
	out := parts[:0]
	for _, p := range parts {
		switch p {
		case `\Seen`, `\Flagged`, `\Deleted`:
			// covered by is_seen/is_flagged/is_deleted booleans
		default:
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// flagsFromParts reconstructs the space-separated IMAP flags string from
// the three boolean columns plus the decrypted flags_other plaintext.
// Inverse of flagsOtherForEncryption + the is_*-derivation in
// insertMessageTx — together they must round-trip such that
// flagsOtherForEncryption(flagsFromParts(s,f,d, other)) == other for
// every other that contains no \Seen/\Flagged/\Deleted token (which is
// guaranteed by flagsOtherForEncryption itself).
//
// Callers: scanMessages, GetByMessageID, GetByThread, GetAllByThread.
// They feed the result into Message.Flags which IMAP/SMTP/tagsync code
// re-tokenizes with strings.Fields — so the function only needs to
// produce a *valid* space-separated flag list; the canonical IMAP order
// is not enforceable here anyway (the original UID FETCH order is lost
// the moment the booleans were extracted).
func flagsFromParts(isSeen, isFlagged, isDeleted bool, other string) string {
	if !isSeen && !isFlagged && !isDeleted {
		return other
	}
	parts := make([]string, 0, 4)
	if isSeen {
		parts = append(parts, `\Seen`)
	}
	if isFlagged {
		parts = append(parts, `\Flagged`)
	}
	if isDeleted {
		parts = append(parts, `\Deleted`)
	}
	if other != "" {
		parts = append(parts, other)
	}
	return strings.Join(parts, " ")
}

// getOrCreateMailbox returns the mailboxes.id for name, inserting the
// row (with encrypted name_ct under meta_key) if it does not yet exist.
// INBOX is case-normalized to "INBOX" to match the v8→v9 backfill so
// 'inbox' / 'Inbox' / 'INBOX' continue to resolve to the same id.
//
// Runs inside the caller's transaction so failures roll back along with
// the message insert that triggered the lookup. Returns 0 + nil for an
// empty name — caller decides whether to store NULL.
func (d *DB) getOrCreateMailbox(tx *sql.Tx, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	nameCT, err := d.encryptMeta(name)
	if err != nil {
		return 0, fmt.Errorf("encrypt mailbox name: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO mailboxes (name, name_ct) VALUES (?, ?)",
		name, nameCT); err != nil {
		return 0, fmt.Errorf("insert mailbox: %w", err)
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM mailboxes WHERE name = ?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup mailbox id: %w", err)
	}
	return id, nil
}

// getOrCreateAccount mirrors getOrCreateMailbox for the accounts table.
// Account names are not case-normalized — they are full email addresses
// that callers already canonicalize at sync-config load.
func (d *DB) getOrCreateAccount(tx *sql.Tx, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	nameCT, err := d.encryptMeta(name)
	if err != nil {
		return 0, fmt.Errorf("encrypt account name: %w", err)
	}
	if _, err := tx.Exec(
		"INSERT OR IGNORE INTO accounts (name, name_ct) VALUES (?, ?)",
		name, nameCT); err != nil {
		return 0, fmt.Errorf("insert account: %w", err)
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM accounts WHERE name = ?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup account id: %w", err)
	}
	return id, nil
}

// backfillMetaCt encrypts existing mailbox names, account names, and the
// non-boolean flags subset of each message into the new BLOB columns.
// Single transaction across all three so a mid-loop failure rolls back
// cleanly.
func (d *DB) backfillMetaCt() error {
	if d.keyring == nil || d.keyring.Meta == nil {
		return fmt.Errorf("no keyring available for meta backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// mailboxes + accounts share the same shape: id + name → name_ct.
	for _, table := range []string{"mailboxes", "accounts"} {
		rows, err := tx.Query(`SELECT id, name FROM ` + table + `
			WHERE name IS NOT NULL AND name <> '' AND name_ct IS NULL`)
		if err != nil {
			return fmt.Errorf("select %s: %w", table, err)
		}
		type pending struct {
			id   int64
			name string
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.name); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s: %w", table, err)
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate %s: %w", table, err)
		}

		stmt, err := tx.Prepare("UPDATE " + table + " SET name_ct = ? WHERE id = ?")
		if err != nil {
			return fmt.Errorf("prepare %s update: %w", table, err)
		}
		for _, p := range batch {
			ct, err := d.encryptMeta(p.name)
			if err != nil {
				stmt.Close()
				return fmt.Errorf("encrypt %s name id=%d: %w", table, p.id, err)
			}
			if _, err := stmt.Exec(ct, p.id); err != nil {
				stmt.Close()
				return fmt.Errorf("update %s id=%d: %w", table, p.id, err)
			}
		}
		stmt.Close()
	}

	// messages.flags_other gets the non-boolean subset of the existing
	// flags TEXT — booleans are already covered by is_{seen,flagged,deleted}.
	rows, err := tx.Query(`SELECT id, flags FROM messages
		WHERE flags IS NOT NULL AND flags <> '' AND flags_other IS NULL`)
	if err != nil {
		return fmt.Errorf("select messages: %w", err)
	}
	type pending struct {
		id    int64
		flags string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.flags); err != nil {
			rows.Close()
			return fmt.Errorf("scan messages: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate messages: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE messages SET flags_other = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare messages update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		other := flagsOtherForEncryption(p.flags)
		ct, err := d.encryptMeta(other)
		if err != nil {
			return fmt.Errorf("encrypt flags id=%d: %w", p.id, err)
		}
		if _, err := stmt.Exec(ct, p.id); err != nil {
			return fmt.Errorf("update messages id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// encryptDraftJSON seals plain with the draft sub-key. Empty plaintext
// maps to nil so draft_json_ct stays NULL — same convention as the other
// encrypt* helpers.
func (d *DB) encryptDraftJSON(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Draft, []byte(plain))
}

// decryptDraftJSON returns plaintext draft JSON, preferring ct when set.
// Decrypt failures on non-nil ct are hard errors.
func (d *DB) decryptDraftJSON(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Draft, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt draft_json: %w", err)
	}
	return string(out), nil
}

// backfillDraftJSONCt encrypts every existing draft_json across both
// local_drafts and outbox into their new draft_json_ct BLOB columns.
// One transaction spans both tables so a mid-loop failure rolls back
// cleanly. Volumes are small in practice (drafts plus pending sends)
// so a single batch in RAM is fine.
func (d *DB) backfillDraftJSONCt() error {
	if d.keyring == nil || d.keyring.Draft == nil {
		return fmt.Errorf("no keyring available for draft backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Two tables, same shape — fold them through one loop.
	for _, table := range []struct {
		name    string
		pkCol   string // primary key column (TEXT for local_drafts, INTEGER for outbox)
		pkScanT string // "string" or "int64"
	}{
		{"local_drafts", "id", "string"},
		{"outbox", "id", "int64"},
	} {
		rows, err := tx.Query(`SELECT ` + table.pkCol + `, draft_json FROM ` + table.name + `
			WHERE draft_json IS NOT NULL AND draft_json <> '' AND draft_json_ct IS NULL`)
		if err != nil {
			return fmt.Errorf("select %s: %w", table.name, err)
		}
		type pending struct {
			idStr string
			idInt int64
			json  string
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if table.pkScanT == "string" {
				if err := rows.Scan(&p.idStr, &p.json); err != nil {
					rows.Close()
					return fmt.Errorf("scan %s: %w", table.name, err)
				}
			} else {
				if err := rows.Scan(&p.idInt, &p.json); err != nil {
					rows.Close()
					return fmt.Errorf("scan %s: %w", table.name, err)
				}
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate %s rows: %w", table.name, err)
		}

		stmt, err := tx.Prepare("UPDATE " + table.name + " SET draft_json_ct = ? WHERE " + table.pkCol + " = ?")
		if err != nil {
			return fmt.Errorf("prepare update %s: %w", table.name, err)
		}
		for _, p := range batch {
			ct, err := d.encryptDraftJSON(p.json)
			if err != nil {
				stmt.Close()
				return fmt.Errorf("encrypt %s draft: %w", table.name, err)
			}
			if table.pkScanT == "string" {
				_, err = stmt.Exec(ct, p.idStr)
			} else {
				_, err = stmt.Exec(ct, p.idInt)
			}
			if err != nil {
				stmt.Close()
				return fmt.Errorf("update %s: %w", table.name, err)
			}
		}
		stmt.Close()
	}
	return tx.Commit()
}

// encryptHeaderValue seals plain with the headers sub-key. Empty plaintext
// maps to nil so value_ct stays NULL — same convention as encryptSubject.
func (d *DB) encryptHeaderValue(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Headers, []byte(plain))
}

// decryptHeaderValue returns the plaintext header value, preferring ct
// when present. Decrypt failures on non-nil ct are hard errors.
func (d *DB) decryptHeaderValue(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Headers, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt header value: %w", err)
	}
	return string(out), nil
}

// backfillHeaderValueCt encrypts every existing message_headers.value into
// the new value_ct BLOB column. Single transaction so a mid-loop failure
// rolls back and leaves schema_version unbumped. message_headers can be
// 5-10x the messages row count (each message has several persisted
// headers); the prepared-statement loop keeps per-row overhead minimal.
func (d *DB) backfillHeaderValueCt() error {
	if d.keyring == nil || d.keyring.Headers == nil {
		return fmt.Errorf("no keyring available for header backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// message_headers has a composite PK (message_id, name) — both are
	// needed to address the row in the UPDATE.
	rows, err := tx.Query(`SELECT message_id, name, value FROM message_headers
		WHERE value IS NOT NULL AND value <> '' AND value_ct IS NULL`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		messageID int64
		name      string
		value     string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.messageID, &p.name, &p.value); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE message_headers SET value_ct = ? WHERE message_id = ? AND name = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		ct, err := d.encryptHeaderValue(p.value)
		if err != nil {
			return fmt.Errorf("encrypt header (%d/%s): %w", p.messageID, p.name, err)
		}
		if _, err := stmt.Exec(ct, p.messageID, p.name); err != nil {
			return fmt.Errorf("update (%d/%s): %w", p.messageID, p.name, err)
		}
	}
	return tx.Commit()
}

// encryptAddr seals plain with the addrs sub-key. Empty plaintext maps to
// nil so the ct column stays NULL — same convention as encryptSubject.
func (d *DB) encryptAddr(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Addrs, []byte(plain))
}

// decryptAddr returns the plaintext address for a row, preferring the
// encrypted ciphertext and falling back to the plaintext column only when
// ct IS NULL. Decrypt failures on non-nil ct are hard errors.
func (d *DB) decryptAddr(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Addrs, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt addr: %w", err)
	}
	return string(out), nil
}

// backfillAddrsCt encrypts every existing from_addr / to_addrs / cc_addrs
// into the new BLOB columns. Single transaction so a mid-loop failure
// rolls back cleanly. Rows where all three are empty/NULL are skipped so
// their *_ct columns stay NULL.
func (d *DB) backfillAddrsCt() error {
	if d.keyring == nil || d.keyring.Addrs == nil {
		return fmt.Errorf("no keyring available for addrs backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rolled back if Commit not reached

	rows, err := tx.Query(`SELECT id, from_addr, to_addrs, cc_addrs FROM messages
		WHERE (from_addr IS NOT NULL AND from_addr <> '' AND from_addr_ct IS NULL)
		   OR (to_addrs  IS NOT NULL AND to_addrs  <> '' AND to_addrs_ct  IS NULL)
		   OR (cc_addrs  IS NOT NULL AND cc_addrs  <> '' AND cc_addrs_ct  IS NULL)`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		id                       int64
		fromAddr, toAddrs, ccAddrs string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.fromAddr, &p.toAddrs, &p.ccAddrs); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE messages SET from_addr_ct = ?, to_addrs_ct = ?, cc_addrs_ct = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		fromCT, err := d.encryptAddr(p.fromAddr)
		if err != nil {
			return fmt.Errorf("encrypt from_addr id=%d: %w", p.id, err)
		}
		toCT, err := d.encryptAddr(p.toAddrs)
		if err != nil {
			return fmt.Errorf("encrypt to_addrs id=%d: %w", p.id, err)
		}
		ccCT, err := d.encryptAddr(p.ccAddrs)
		if err != nil {
			return fmt.Errorf("encrypt cc_addrs id=%d: %w", p.id, err)
		}
		if _, err := stmt.Exec(fromCT, toCT, ccCT, p.id); err != nil {
			return fmt.Errorf("update id=%d: %w", p.id, err)
		}
	}
	return tx.Commit()
}
