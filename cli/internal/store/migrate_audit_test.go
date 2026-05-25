package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/durian-dev/durian/cli/internal/dbcrypto"
)

// TestRebuildMessages_IdempotentAfterPartialFailure asserts ADR-0001
// audit H4: rebuildMessagesForStep7f tolerates re-entry after a
// previous attempt left a half-built messages_new behind. The pre-fix
// version used a bare CREATE TABLE messages_new without IF NOT EXISTS;
// a mid-INSERT-SELECT crash (OOM on the multi-GB scan, disk-full,
// power loss) wedged the next Init() at "table messages_new already
// exists".
//
// We simulate the wedged state by manually creating a messages_new
// table, then calling rebuildMessagesForStep7f directly and verifying
// it doesn't error.
func TestRebuildMessages_IdempotentAfterPartialFailure(t *testing.T) {
	db := newTestDB(t)
	// Simulate a leftover from a previous interrupted rebuild attempt.
	// Use the simplest possible schema — the pre-fix code never
	// checked the leftover's shape, just complained that the name was
	// taken; same here.
	if _, err := db.db.Exec("CREATE TABLE messages_new (id INTEGER, junk TEXT)"); err != nil {
		t.Fatalf("seed leftover: %v", err)
	}
	if _, err := db.db.Exec("INSERT INTO messages_new (id, junk) VALUES (999, 'half-built')"); err != nil {
		t.Fatalf("seed leftover row: %v", err)
	}

	// Calling rebuildMessagesForStep7f must DROP the leftover and
	// rebuild from scratch. Without the H4 fix, this would error at
	// the CREATE TABLE step.
	if err := db.rebuildMessagesForStep7f(); err != nil {
		t.Fatalf("rebuild after partial failure: %v", err)
	}

	// The junk row from the leftover must NOT appear in the rebuilt
	// messages table — confirming the DROP fired before the rebuild.
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 999").Scan(&count); err != nil {
		t.Fatalf("verify junk gone: %v", err)
	}
	if count != 0 {
		t.Errorf("junk row from previous attempt leaked into rebuilt messages table")
	}
}

// TestMigrateV17V18_VacuumBeforeBump asserts ADR-0001 audit H3: the
// v17→v18 migration runs VACUUM before bumping schema_version. The
// pre-fix order was bump-then-VACUUM; a VACUUM failure left the DB
// stuck at v18 with the dropped step-7e plaintext bytes (subject,
// body_text, body_html, message_headers.value, draft_json) stranded
// in free pages forever, defeating the at-rest encryption story for
// any cold filesystem image taken thereafter.
//
// The contract is: after migrate() completes for a fresh DB,
// schema_version reaches the latest version AND a VACUUM has been
// executed at least once on the v18 transition.
//
// We can't easily mock VACUUM failure inside SQLite, so the test
// instead asserts the migration reaches v20+ (the v19→v20 follow-up
// migration is itself a re-VACUUM that catches users who crossed v18
// under the buggy order — its presence proves the audit-H3 cleanup
// is wired in).
func TestMigrateV17V18_VacuumBeforeBump(t *testing.T) {
	dir := t.TempDir()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatalf("test keyring: %v", err)
	}
	db, err := Open(filepath.Join(dir, "vac.db"), kr)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	var version int
	if err := db.db.QueryRow("SELECT version FROM schema_version WHERE rowid = 1").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version < 20 {
		t.Errorf("schema_version = %d, want >= 20 (v19→v20 H3-followup re-VACUUM must be applied)", version)
	}
	// audit-medium follow-up: v20→v21 must also have applied (bigram
	// HMAC encoding fix that rebuilds messages_blind_fts).
	if version < 21 {
		t.Errorf("schema_version = %d, want >= 21 (v20→v21 bigram-encoding rebuild must be applied)", version)
	}
}
