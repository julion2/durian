package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrDuplicateSnapshotRef means a provider repeated a ref in a later page.
// The page transaction is rolled back, so its cursor must not be checkpointed.
var ErrDuplicateSnapshotRef = errors.New("duplicate snapshot ref")

// SnapshotState is the durable side of a replacement-snapshot checkpoint.
// CheckpointCursor may be ahead of the cursor file after a process crash; all
// ingestion and presence writes for that page were already committed in that
// case, so the engine can safely finish the cursor-file checkpoint on restart.
type SnapshotState struct {
	Active           bool
	BaseCursor       []byte
	CheckpointCursor []byte
	Complete         bool
}

func (d *DB) BeginSnapshot(account, folder string, baseCursor []byte) error {
	baseCursor = nonNilCursor(baseCursor)
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM snapshot_episodes WHERE account=? AND folder=?`, account, folder); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO snapshot_episodes(account,folder,base_cursor,checkpoint_cursor) VALUES(?,?,?,?)`,
		account, folder, baseCursor, baseCursor); err != nil {
		return err
	}
	return tx.Commit()
}

func nonNilCursor(cursor []byte) []byte {
	if cursor == nil {
		return []byte{}
	}
	return cursor
}

func (d *DB) GetSnapshotState(account, folder string) (SnapshotState, error) {
	var state SnapshotState
	err := d.db.QueryRow(`SELECT base_cursor,checkpoint_cursor,complete FROM snapshot_episodes WHERE account=? AND folder=?`,
		account, folder).Scan(&state.BaseCursor, &state.CheckpointCursor, &state.Complete)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.Active = true
	return state, nil
}

// StageSnapshotPage adds one fully processed provider page and its resulting
// cursor atomically. reported contains every ref emitted by the provider;
// present is the subset that survived hydration. preserved contains legacy
// local refs retained after a permanently malformed no-stable-ID replacement.
// A provider ref repeated in this or an earlier page rejects the complete page.
func (d *DB) StageSnapshotPage(account, folder string, reported, present, preserved []string, cursor []byte, complete bool) error {
	cursor = nonNilCursor(cursor)
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var one int
	if err = tx.QueryRow(`SELECT 1 FROM snapshot_episodes WHERE account=? AND folder=?`, account, folder).Scan(&one); err != nil {
		return fmt.Errorf("snapshot episode inactive: %w", err)
	}
	pageSeen := make(map[string]struct{}, len(reported))
	for _, ref := range reported {
		if ref == "" {
			return fmt.Errorf("empty snapshot ref")
		}
		if _, duplicate := pageSeen[ref]; duplicate {
			return fmt.Errorf("%w: %q", ErrDuplicateSnapshotRef, ref)
		}
		pageSeen[ref] = struct{}{}
		var seen bool
		err = tx.QueryRow(`SELECT seen FROM snapshot_present_refs WHERE account=? AND folder=? AND remote_ref=?`, account, folder, ref).Scan(&seen)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("inspect snapshot ref %q: %w", ref, err)
		}
		if seen {
			return fmt.Errorf("%w: %q", ErrDuplicateSnapshotRef, ref)
		}
		if err == sql.ErrNoRows {
			_, err = tx.Exec(`INSERT INTO snapshot_present_refs(account,folder,remote_ref,seen) VALUES(?,?,?,1)`, account, folder, ref)
		} else {
			_, err = tx.Exec(`UPDATE snapshot_present_refs SET seen=1 WHERE account=? AND folder=? AND remote_ref=?`, account, folder, ref)
		}
		if err != nil {
			return fmt.Errorf("stage snapshot ref %q: %w", ref, err)
		}
	}
	for _, refs := range [][]string{present, preserved} {
		for _, ref := range refs {
			if ref == "" {
				return fmt.Errorf("empty present snapshot ref")
			}
			if _, err = tx.Exec(`INSERT INTO snapshot_present_refs(account,folder,remote_ref,present) VALUES(?,?,?,1)
				ON CONFLICT(account,folder,remote_ref) DO UPDATE SET present=1`, account, folder, ref); err != nil {
				return fmt.Errorf("mark snapshot ref %q present: %w", ref, err)
			}
		}
	}
	if _, err = tx.Exec(`UPDATE snapshot_episodes SET checkpoint_cursor=?,complete=? WHERE account=? AND folder=?`,
		cursor, complete, account, folder); err != nil {
		return fmt.Errorf("update snapshot checkpoint: %w", err)
	}
	return tx.Commit()
}

// ValidateSnapshotPageRefs rejects provider duplicates before hydration. This
// avoids downloading a body for a page whose presence contract is already
// invalid. StageSnapshotPage repeats the check in its write transaction.
func (d *DB) ValidateSnapshotPageRefs(account, folder string, refs []string) error {
	pageSeen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref == "" {
			return errors.New("empty snapshot ref")
		}
		if _, duplicate := pageSeen[ref]; duplicate {
			return fmt.Errorf("%w: %q", ErrDuplicateSnapshotRef, ref)
		}
		pageSeen[ref] = struct{}{}
		var seen bool
		err := d.db.QueryRow(`SELECT seen FROM snapshot_present_refs WHERE account=? AND folder=? AND remote_ref=?`, account, folder, ref).Scan(&seen)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("inspect snapshot ref %q: %w", ref, err)
		}
		if seen {
			return fmt.Errorf("%w: %q", ErrDuplicateSnapshotRef, ref)
		}
	}
	return nil
}

// HasFolderRemoteRefs reports whether authoritative initial sync has old local
// provider identities to reconcile, without loading the mailbox into memory.
func (d *DB) HasFolderRemoteRefs(account, folder string) (bool, error) {
	var one int
	err := d.db.QueryRow(`SELECT 1 FROM messages m
		JOIN accounts a ON a.id=m.account_id JOIN mailboxes mb ON mb.id=m.mailbox_id
		WHERE a.name=? AND mb.name=? AND m.remote_ref!='' LIMIT 1`, account, folder).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// SnapshotRowsForRefs loads only the local state referenced by one provider
// page. The result is bounded by refs rather than mailbox size.
func (d *DB) SnapshotRowsForRefs(account, folder string, refs []string) ([]FolderFlagRow, error) {
	return d.snapshotRowsForValues(account, folder, "m.remote_ref", refs)
}

// SnapshotRefsForMessageIDs returns local refs matching one page's legacy
// no-stable-ID failures. Message-ID is intentionally not assumed unique.
func (d *DB) SnapshotRefsForMessageIDs(account, folder string, messageIDs []string) (map[string][]string, error) {
	rows, err := d.snapshotRowsForValues(account, folder, "m.message_id", messageIDs)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string, len(rows))
	for _, row := range rows {
		result[row.MessageID] = append(result[row.MessageID], row.RemoteRef)
	}
	return result, nil
}

func (d *DB) snapshotRowsForValues(account, folder, column string, values []string) ([]FolderFlagRow, error) {
	if len(values) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	query := fmt.Sprintf(`SELECT m.id,m.message_id,m.remote_ref FROM messages m
		JOIN accounts a ON a.id=m.account_id JOIN mailboxes mb ON mb.id=m.mailbox_id
		WHERE a.name=? AND mb.name=? AND m.remote_ref!='' AND %s IN (%s)
		ORDER BY m.id`, column, placeholders)
	args := make([]any, 0, len(values)+2)
	args = append(args, account, folder)
	for _, value := range values {
		args = append(args, value)
	}
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FolderFlagRow
	for rows.Next() {
		var row FolderFlagRow
		if err := rows.Scan(&row.RowID, &row.MessageID, &row.RemoteRef); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// SnapshotAbsentRows pages local provider rows not represented by staging.
func (d *DB) SnapshotAbsentRows(account, folder string, afterID int64, limit int) ([]FolderFlagRow, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.db.Query(`SELECT m.id,m.message_id,m.remote_ref FROM messages m
		JOIN accounts a ON a.id=m.account_id JOIN mailboxes mb ON mb.id=m.mailbox_id
		WHERE a.name=? AND mb.name=? AND m.id>? AND m.remote_ref!=''
		AND NOT EXISTS(SELECT 1 FROM snapshot_present_refs s WHERE s.account=? AND s.folder=? AND s.remote_ref=m.remote_ref AND s.present=1)
		ORDER BY m.id LIMIT ?`, account, folder, afterID, account, folder, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderFlagRow
	for rows.Next() {
		var r FolderFlagRow
		if err := rows.Scan(&r.RowID, &r.MessageID, &r.RemoteRef); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) ClearSnapshot(account, folder string) error {
	_, err := d.db.Exec(`DELETE FROM snapshot_episodes WHERE account=? AND folder=?`, account, folder)
	return err
}
