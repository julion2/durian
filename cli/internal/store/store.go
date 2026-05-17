package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite database connection for the email store.
type DB struct {
	db *sql.DB
}

// Open opens or creates an email store database at the given path.
// Use ":memory:" for in-memory databases (useful for testing).
func Open(dbPath string) (*DB, error) {
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

	return &DB{db: db}, nil
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
	}

	return nil
}
