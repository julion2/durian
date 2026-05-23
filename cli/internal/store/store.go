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
