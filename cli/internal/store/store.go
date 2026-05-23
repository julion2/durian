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
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox ON messages(mailbox)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_addr ON messages(from_addr)`,

		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			subject, from_addr, to_addrs, body_text,
			content='messages',
			content_rowid='id'
		)`,

		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,

		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
		END`,

		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,

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

	// Indexes and triggers that may have been dropped by migrations.
	postMigration := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_account ON messages(account)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_mailbox ON messages(mailbox)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_from_addr ON messages(from_addr)`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, to_addrs, body_text)
			VALUES ('delete', old.id, old.subject, old.from_addr, old.to_addrs, old.body_text);
			INSERT INTO messages_fts(rowid, subject, from_addr, to_addrs, body_text)
			VALUES (new.id, new.subject, new.from_addr, new.to_addrs, new.body_text);
		END`,
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
