package store

import "fmt"

// migrateProviderNative applies the schema changes introduced with native
// provider identities and the durable recovery mechanisms built on top of
// them. Keeping these recent migrations together avoids further growing the
// legacy migration history in store.go.
func (d *DB) migrateProviderNative(version int) error {
	if version < 30 {
		// A JMAP Email.id is immutable and unique within an account, while an RFC
		// Message-ID is optional, duplicable, and sender-controlled. Keep the
		// latter as message metadata and fallback identity, but let capable
		// backends key rows by their native stable id. Partial unique indexes let
		// both identity modes coexist without collapsing duplicate Message-IDs.
		has, err := hasColumn(d.db, "messages", "stable_id")
		if err != nil {
			return fmt.Errorf("migrate v29→v30 inspect stable_id: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE messages ADD COLUMN stable_id TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("migrate v29→v30 add stable_id: %w", err)
			}
		}
		stmts := []string{
			`DROP INDEX IF EXISTS idx_messages_msgid_acctid_uniq`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_stableid_acctid_uniq
				ON messages(stable_id, IFNULL(account_id, 0)) WHERE stable_id != ''`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_msgid_fallback_acctid_uniq
				ON messages(message_id, IFNULL(account_id, 0)) WHERE stable_id = ''`,
			`CREATE TABLE IF NOT EXISTS provider_tag_mutations (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				message_db_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
				tag TEXT NOT NULL,
				action TEXT NOT NULL CHECK(action IN ('add', 'remove')),
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_provider_tag_mutations_message
				ON provider_tag_mutations(message_db_id, id)`,
			`UPDATE schema_version SET version = 30 WHERE rowid = 1`,
		}
		for _, stmt := range stmts {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v29→v30: %w", err)
			}
		}
	}

	if version < 31 {
		// Enrichment spans several transactions. Give each writer a durable
		// generation so completion by an older overlapping writer cannot clear the
		// pending marker while a newer writer is still rebuilding attachments.
		has, err := hasColumn(d.db, "messages", "ingest_generation")
		if err != nil {
			return fmt.Errorf("migrate v30→v31 inspect ingest_generation: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE messages ADD COLUMN ingest_generation INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("migrate v30→v31 add ingest_generation: %w", err)
			}
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 31 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v30→v31 bump: %w", err)
		}
		version = 31
	}

	if version < 32 {
		// Claim an outbox item durably before any delivery attempt. A process
		// crash after the provider accepts a message cannot safely distinguish
		// success from failure, so a claimed item must remain blocked for manual
		// reconciliation rather than being submitted automatically again. Keep
		// confirmed delivery as a separate durable bit so an accepted message can
		// never be requeued through the not-delivered transition.
		has, err := hasColumn(d.db, "outbox", "in_flight")
		if err != nil {
			return fmt.Errorf("migrate v31→v32 inspect in_flight: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE outbox ADD COLUMN in_flight INTEGER NOT NULL DEFAULT 0 CHECK(in_flight IN (0, 1))"); err != nil {
				return fmt.Errorf("migrate v31→v32 add in_flight: %w", err)
			}
		}
		has, err = hasColumn(d.db, "outbox", "delivery_confirmed")
		if err != nil {
			return fmt.Errorf("migrate v31→v32 inspect delivery_confirmed: %w", err)
		}
		if !has {
			if _, err := d.db.Exec("ALTER TABLE outbox ADD COLUMN delivery_confirmed INTEGER NOT NULL DEFAULT 0 CHECK(delivery_confirmed IN (0, 1))"); err != nil {
				return fmt.Errorf("migrate v31→v32 add delivery_confirmed: %w", err)
			}
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 32 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v31→v32 bump: %w", err)
		}
		version = 32
	}

	if version < 33 {
		stmts := []string{
			`CREATE TABLE IF NOT EXISTS snapshot_episodes (
				account TEXT NOT NULL, folder TEXT NOT NULL,
				base_cursor BLOB NOT NULL,
				checkpoint_cursor BLOB NOT NULL,
				complete INTEGER NOT NULL DEFAULT 0 CHECK(complete IN (0, 1)),
				PRIMARY KEY(account, folder))`,
			`CREATE TABLE IF NOT EXISTS snapshot_present_refs (
				account TEXT NOT NULL, folder TEXT NOT NULL, remote_ref TEXT NOT NULL,
				seen INTEGER NOT NULL DEFAULT 0 CHECK(seen IN (0, 1)),
				present INTEGER NOT NULL DEFAULT 0 CHECK(present IN (0, 1)),
				PRIMARY KEY(account, folder, remote_ref),
				FOREIGN KEY(account, folder) REFERENCES snapshot_episodes(account, folder) ON DELETE CASCADE)`,
			`CREATE INDEX IF NOT EXISTS idx_snapshot_present_episode ON snapshot_present_refs(account, folder, remote_ref)`,
			`UPDATE schema_version SET version = 33 WHERE rowid = 1`,
		}
		for _, stmt := range stmts {
			if _, err := d.db.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v32→v33: %w", err)
			}
		}
		version = 33
	}

	if version < 34 {
		if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS outbox_idempotency (
			idempotency_key TEXT PRIMARY KEY,
			outbox_id INTEGER NOT NULL,
			send_after INTEGER NOT NULL,
			created_at INTEGER NOT NULL)`); err != nil {
			return fmt.Errorf("migrate v33→v34 create outbox idempotency: %w", err)
		}
		if _, err := d.db.Exec("UPDATE schema_version SET version = 34 WHERE rowid = 1"); err != nil {
			return fmt.Errorf("migrate v33→v34 bump: %w", err)
		}
	}

	return nil
}
