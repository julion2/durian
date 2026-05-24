package contacts

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

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
	db *sql.DB
}

// Open opens or creates a contacts database at the given path.
//
// ADR-0001 step 7g (β-revision): contact email + name stay plaintext on
// disk. They are on the wire any time a mail is sent or received, so the
// asymmetric protection of encrypting only the local mirror is not worth
// losing UNIQUE(email) enforcement and prefix-LIKE search. The contacts
// store therefore takes no keyring; see ADR §3 "Stays plaintext".
func Open(dbPath string) (*DB, error) {
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

	return &DB{db: db}, nil
}

// Close closes the database connection
func (d *DB) Close() error {
	return d.db.Close()
}

// Init creates the contacts table and indexes if they don't exist, and
// runs any pending one-shot migrations. contacts.db has no
// schema_version table — migrations are detected via PRAGMA table_info.
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

	// ADR-0001 step 7g (β-revision): drop the email_ct + name_ct BLOB
	// columns that step 6 added. They were written but never read — the
	// UNIQUE(email) constraint and the indexed LIKE prefix search both
	// need plaintext, and the β-revision argument (address PII is already
	// on the wire) removed any motivation to keep the ciphertext mirror.
	// Idempotent via PRAGMA table_info — once dropped, the column
	// disappears and the loop becomes a no-op on subsequent opens.
	for _, col := range []string{"email_ct", "name_ct"} {
		has, err := hasColumn(d.db, "contacts", col)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", col, err)
		}
		if has {
			if _, err := d.db.Exec("ALTER TABLE contacts DROP COLUMN " + col); err != nil {
				return fmt.Errorf("drop %s: %w", col, err)
			}
		}
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

// Add adds a new contact to the database
// If the email already exists, it updates the name if provided
func (d *DB) Add(email, name, source string) error {
	if !isValidEmail(email) {
		return fmt.Errorf("invalid email address: %s", email)
	}

	id := uuid.New().String()
	now := time.Now()
	lowered := strings.ToLower(email)

	_, err := d.db.Exec(`
		INSERT INTO contacts (id, email, name, source, created_at, usage_count)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(email) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), contacts.name)
	`, id, lowered, name, source, now)

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
		INSERT INTO contacts (id, email, name, source, created_at, usage_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), contacts.name),
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
		result, err := stmt.Exec(c.ID, lowered, c.Name, c.Source, c.CreatedAt, c.UsageCount)
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
