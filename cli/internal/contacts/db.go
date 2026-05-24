package contacts

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/durian-dev/durian/cli/internal/dbcrypto"

	_ "modernc.org/sqlite"
)

// DefaultDBPath returns the default contacts database path.
// Respects XDG_DATA_HOME, falls back to ~/.local/share/durian/contacts.db
func DefaultDBPath() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "durian", "contacts.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "durian", "contacts.db")
}

// DB represents a contacts database connection
type DB struct {
	db      *sql.DB
	keyring *dbcrypto.Keyring
}

// Open opens or creates a contacts database at the given path.
//
// kr must be a non-nil Keyring derived from the OS-keychain master key.
// ADR-0001 step 6 requires the Contact sub-key for the email/name
// encrypt-on-write path and the one-shot backfill in Init().
func Open(dbPath string, kr *dbcrypto.Keyring) (*DB, error) {
	if kr == nil {
		return nil, fmt.Errorf("contacts: Open requires a non-nil keyring (see ADR-0001)")
	}

	// Expand ~ if present
	if strings.HasPrefix(dbPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home dir: %w", err)
		}
		dbPath = filepath.Join(home, dbPath[2:])
	}

	// Ensure parent directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("chmod db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Use DELETE journal mode for simpler file handling
	// (WAL mode creates -wal and -shm files that complicate read-only access)
	if _, err := db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set journal mode: %w", err)
	}

	return &DB{db: db, keyring: kr}, nil
}

// Close closes the database connection
func (d *DB) Close() error {
	return d.db.Close()
}

// Init creates the contacts table and indexes if they don't exist, and
// runs any pending one-shot migrations (ADR-0001 step 6 encryption is
// detected via PRAGMA table_info since contacts.db has no schema_version
// — first migration ever and likely the only one until step 7).
func (d *DB) Init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS contacts (
		id TEXT PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		name TEXT,
		last_used TIMESTAMP,
		usage_count INTEGER DEFAULT 0,
		source TEXT DEFAULT 'imported',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_contacts_email ON contacts(email);
	CREATE INDEX IF NOT EXISTS idx_contacts_name ON contacts(name);
	CREATE INDEX IF NOT EXISTS idx_contacts_usage ON contacts(usage_count DESC, last_used DESC);
	`

	if _, err := d.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	// ADR-0001 step 6: add email_ct + name_ct BLOB columns for at-rest
	// encryption of contact PII. Idempotent via PRAGMA table_info — once
	// both columns exist the backfill becomes a no-op (zero rows match
	// WHERE *_ct IS NULL). Plaintext email/name columns stay until
	// step 7 introduces deterministic lookup tokens — the UNIQUE
	// constraint on email and the indexed LIKE prefix on both columns
	// cannot be served from random-nonce ciphertext.
	for _, col := range []string{"email_ct", "name_ct"} {
		has, err := hasColumn(d.db, "contacts", col)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", col, err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE contacts ADD COLUMN " + col + " BLOB"); err != nil {
				return fmt.Errorf("add %s: %w", col, err)
			}
		}
	}
	if err := d.backfillContactCt(); err != nil {
		return fmt.Errorf("backfill contacts ct: %w", err)
	}

	return nil
}

// hasColumn returns true if the named column already exists on table.
// Mirrors the helper in cli/internal/store/store.go — kept package-local
// here so contacts has no cross-package dependency on store internals.
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

// encryptContact seals plain with the contact sub-key. Empty plaintext
// maps to nil so the ct column stays NULL — same convention as the
// store package's encrypt* helpers.
func (d *DB) encryptContact(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	return dbcrypto.Encrypt(d.keyring.Contact, []byte(plain))
}

// decryptContact returns plaintext for a contact_key column, preferring
// the ct when present and falling back to plaintext only when ct IS NULL.
// Decrypt failures on non-nil ct are hard errors. Not yet called by
// production reads — step 7 will wire the lookup paths once the
// plaintext columns die.
func (d *DB) decryptContact(plain string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return plain, nil
	}
	out, err := dbcrypto.Decrypt(d.keyring.Contact, ct)
	if err != nil {
		return "", fmt.Errorf("decrypt contact: %w", err)
	}
	return string(out), nil
}

// backfillContactCt encrypts every existing contacts.email + contacts.name
// into the new BLOB columns. Single transaction so a mid-loop failure
// rolls back cleanly. Contact volumes are typically in the low thousands
// even for heavy users; loading the batch in RAM is fine.
func (d *DB) backfillContactCt() error {
	if d.keyring == nil || d.keyring.Contact == nil {
		return fmt.Errorf("no keyring available for contact backfill")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.Query(`SELECT id, email, name FROM contacts
		WHERE (email IS NOT NULL AND email <> '' AND email_ct IS NULL)
		   OR (name  IS NOT NULL AND name  <> '' AND name_ct  IS NULL)`)
	if err != nil {
		return fmt.Errorf("select rows: %w", err)
	}
	type pending struct {
		id          string
		email, name string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		var name sql.NullString
		if err := rows.Scan(&p.id, &p.email, &name); err != nil {
			rows.Close()
			return fmt.Errorf("scan: %w", err)
		}
		if name.Valid {
			p.name = name.String
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	stmt, err := tx.Prepare("UPDATE contacts SET email_ct = ?, name_ct = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	defer stmt.Close()
	for _, p := range batch {
		emailCT, err := d.encryptContact(p.email)
		if err != nil {
			return fmt.Errorf("encrypt email id=%s: %w", p.id, err)
		}
		nameCT, err := d.encryptContact(p.name)
		if err != nil {
			return fmt.Errorf("encrypt name id=%s: %w", p.id, err)
		}
		if _, err := stmt.Exec(emailCT, nameCT, p.id); err != nil {
			return fmt.Errorf("update id=%s: %w", p.id, err)
		}
	}
	return tx.Commit()
}

// Add adds a new contact to the database
// If the email already exists, it updates the name if provided
func (d *DB) Add(email, name, source string) error {
	if !isValidEmail(email) {
		return fmt.Errorf("invalid email address: %s", email)
	}

	id := uuid.New().String()
	now := time.Now()
	lowered := strings.ToLower(email)
	emailCT, err := d.encryptContact(lowered)
	if err != nil {
		return fmt.Errorf("encrypt email: %w", err)
	}
	nameCT, err := d.encryptContact(name)
	if err != nil {
		return fmt.Errorf("encrypt name: %w", err)
	}

	_, err = d.db.Exec(`
		INSERT INTO contacts (id, email, email_ct, name, name_ct, source, created_at, usage_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(email) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), contacts.name),
			name_ct = CASE WHEN NULLIF(excluded.name, '') IS NOT NULL
			               THEN excluded.name_ct ELSE contacts.name_ct END
	`, id, lowered, emailCT, name, nameCT, source, now)

	if err != nil {
		return fmt.Errorf("add contact: %w", err)
	}

	return nil
}

// AddBatch adds multiple contacts efficiently in a single transaction
func (d *DB) AddBatch(contacts []Contact) (added, updated int, err error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO contacts (id, email, email_ct, name, name_ct, source, created_at, usage_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), contacts.name),
			name_ct = CASE WHEN NULLIF(excluded.name, '') IS NOT NULL
			               THEN excluded.name_ct ELSE contacts.name_ct END,
			usage_count = excluded.usage_count
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("prepare statement: %w", err)
	}
	defer stmt.Close()

	for _, c := range contacts {
		if !isValidEmail(c.Email) {
			continue
		}
		lowered := strings.ToLower(c.Email)
		emailCT, err := d.encryptContact(lowered)
		if err != nil {
			return added, updated, fmt.Errorf("encrypt contact %s email: %w", c.Email, err)
		}
		nameCT, err := d.encryptContact(c.Name)
		if err != nil {
			return added, updated, fmt.Errorf("encrypt contact %s name: %w", c.Email, err)
		}
		result, err := stmt.Exec(c.ID, lowered, emailCT, c.Name, nameCT, c.Source, c.CreatedAt, c.UsageCount)
		if err != nil {
			return added, updated, fmt.Errorf("insert contact %s: %w", c.Email, err)
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			// Check if it was an insert or update by checking if lastInsertId changed
			// This is a simplification - we count all as added for now
			added++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit transaction: %w", err)
	}

	return added, updated, nil
}

// CleanInvalid removes contacts with invalid email addresses from the database
func (d *DB) CleanInvalid() (int, error) {
	result, err := d.db.Exec(`DELETE FROM contacts WHERE email NOT LIKE '%_@_%.__%'`)
	if err != nil {
		return 0, fmt.Errorf("clean invalid contacts: %w", err)
	}
	removed, _ := result.RowsAffected()
	return int(removed), nil
}

// FindByExactName finds a contact by exact name match (case-insensitive).
// Returns the most frequently used contact with that name.
func (d *DB) FindByExactName(name string) (*Contact, error) {
	row := d.db.QueryRow(`
		SELECT id, email, name, last_used, usage_count, source, created_at
		FROM contacts
		WHERE LOWER(name) = LOWER(?)
		ORDER BY usage_count DESC
		LIMIT 1
	`, name)

	var c Contact
	var lastUsed sql.NullTime
	err := row.Scan(&c.ID, &c.Email, &c.Name, &lastUsed, &c.UsageCount, &c.Source, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find by exact name: %w", err)
	}
	if lastUsed.Valid {
		c.LastUsed = lastUsed.Time
	}
	return &c, nil
}

// Search searches for contacts by email or name prefix
// Results are ordered by usage_count DESC, last_used DESC
func (d *DB) Search(query string, limit int) ([]Contact, error) {
	if limit <= 0 {
		limit = 20
	}

	query = strings.ToLower(query)
	pattern := query + "%"

	rows, err := d.db.Query(`
		SELECT id, email, name, last_used, usage_count, source, created_at
		FROM contacts
		WHERE email LIKE ? OR LOWER(name) LIKE ?
		ORDER BY usage_count DESC, last_used DESC NULLS LAST
		LIMIT ?
	`, pattern, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("search contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// List returns all contacts, ordered by usage
func (d *DB) List(limit int) ([]Contact, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := d.db.Query(`
		SELECT id, email, name, last_used, usage_count, source, created_at
		FROM contacts
		ORDER BY usage_count DESC, last_used DESC NULLS LAST
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()

	return scanContacts(rows)
}

// Count returns the total number of contacts
func (d *DB) Count() (int, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM contacts").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count contacts: %w", err)
	}
	return count, nil
}

// IncrementUsage increments the usage count and updates last_used for an email
func (d *DB) IncrementUsage(email string) error {
	_, err := d.db.Exec(`
		UPDATE contacts
		SET usage_count = usage_count + 1, last_used = ?
		WHERE email = ?
	`, time.Now(), strings.ToLower(email))
	if err != nil {
		return fmt.Errorf("increment usage: %w", err)
	}
	return nil
}

// Delete removes a contact by email
func (d *DB) Delete(email string) error {
	result, err := d.db.Exec("DELETE FROM contacts WHERE email = ?", strings.ToLower(email))
	if err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("contact not found: %s", email)
	}

	return nil
}

// scanContacts scans rows into a slice of Contact
func scanContacts(rows *sql.Rows) ([]Contact, error) {
	var contacts []Contact

	for rows.Next() {
		var c Contact
		var lastUsed sql.NullTime

		err := rows.Scan(&c.ID, &c.Email, &c.Name, &lastUsed, &c.UsageCount, &c.Source, &c.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}

		if lastUsed.Valid {
			c.LastUsed = lastUsed.Time
		}

		contacts = append(contacts, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return contacts, nil
}
