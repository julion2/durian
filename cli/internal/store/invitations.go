package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Invitations are the iCalendar parts of mail tagged CalendarTag, encrypted
// like bodies. A row whose ics_ct is NULL records that the message was looked
// at and carries none – the tag comes from a byte match on "text/calendar",
// which a mail merely mentioning it also has. No row means the message hasn't
// been looked at: it was synced before invitations were kept, and sync fills
// it in (MissingInvitations).

// CalendarTag marks mail that may carry an invitation.
const CalendarTag = "cal"

// SetInvitation stores the iCalendar part of a message; an empty ics records
// that it has none.
func (d *DB) SetInvitation(messageDBID int64, ics string) error {
	ct, err := d.encryptBody(ics)
	if err != nil {
		return fmt.Errorf("encrypt invitation: %w", err)
	}
	if _, err := d.db.Exec(`INSERT INTO message_invitations (message_db_id, ics_ct) VALUES (?, ?)
		ON CONFLICT(message_db_id) DO UPDATE SET ics_ct = excluded.ics_ct`, messageDBID, ct); err != nil {
		return fmt.Errorf("store invitation: %w", err)
	}
	return nil
}

// InvitationsByMessages returns the iCalendar part of each given message that
// has one.
func (d *DB) InvitationsByMessages(ids []int64) (map[int64]string, error) {
	result := make(map[int64]string)
	if len(ids) == 0 {
		return result, nil
	}
	params := make([]any, len(ids))
	for i, id := range ids {
		params[i] = id
	}
	rows, err := d.db.Query(`SELECT message_db_id, ics_ct FROM message_invitations
		WHERE ics_ct IS NOT NULL AND message_db_id IN (`+placeholders(len(ids))+`)`, params...)
	if err != nil {
		return nil, fmt.Errorf("query invitations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ct []byte
		if err := rows.Scan(&id, &ct); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		ics, err := d.decryptBody("", ct)
		if err != nil {
			return nil, err
		}
		result[id] = ics
	}
	return result, rows.Err()
}

// InvitationSource says how a sync path addresses a message on the server.
type InvitationSource int

const (
	// ByUID addresses messages by mailbox and IMAP UID (the legacy syncer).
	ByUID InvitationSource = iota
	// ByRemoteRef addresses messages by their provider handle (the engine).
	ByRemoteRef
)

// MissingInvitation is a message whose invitation hasn't been looked at.
type MissingInvitation struct {
	ID        int64
	Mailbox   string
	UID       uint32
	RemoteRef string
}

// MissingInvitations returns up to limit messages of an account tagged
// CalendarTag that have no invitation row and can be fetched the given way,
// newest first: the invitations a user may still answer come first. Messages
// in exclude (failed fetches) are left out so they don't hold up the rest.
//
// The calendar tag drives: it covers a few percent of a mailbox, the account
// often all of it, so the account's index is kept out of the plan (+).
func (d *DB) MissingInvitations(account string, source InvitationSource, exclude []int64, limit int) ([]MissingInvitation, error) {
	q, params := missingInvitationsQuery(account, source, exclude, limit)
	rows, err := d.query(q, params...)
	if err != nil {
		return nil, fmt.Errorf("query missing invitations: %w", err)
	}
	defer rows.Close()
	var missing []MissingInvitation
	for rows.Next() {
		var m MissingInvitation
		var mailboxCT []byte
		var uid sql.NullInt64
		if err := rows.Scan(&m.ID, &mailboxCT, &uid, &m.RemoteRef); err != nil {
			return nil, fmt.Errorf("scan missing invitation: %w", err)
		}
		if m.Mailbox, err = d.decryptMeta("", mailboxCT); err != nil {
			return nil, err
		}
		m.UID = uint32(uid.Int64)
		missing = append(missing, m)
	}
	return missing, rows.Err()
}

// missingInvitationsQuery is MissingInvitations' SQL. Already looked-at
// mail is ruled out on the tag row, before its message row is read.
func missingInvitationsQuery(account string, source InvitationSource, exclude []int64, limit int) (string, []any) {
	fetchable := "m.uid != 0 AND mb.id IS NOT NULL"
	if source == ByRemoteRef {
		fetchable = "m.remote_ref != ''"
	}
	params := []any{CalendarTag, account}
	notExcluded := ""
	if len(exclude) > 0 {
		notExcluded = " AND m.id NOT IN (" + placeholders(len(exclude)) + ")"
		for _, id := range exclude {
			params = append(params, id)
		}
	}
	params = append(params, limit)
	return `SELECT m.id, mb.name_ct, m.uid, m.remote_ref
		FROM tags t JOIN messages m ON m.id = t.message_id
		LEFT JOIN mailboxes mb ON mb.id = m.mailbox_id
		WHERE t.tag = ? AND +m.account_id = (SELECT id FROM accounts WHERE name = ?)
		  AND NOT EXISTS (SELECT 1 FROM message_invitations i WHERE i.message_db_id = t.message_id)
		  AND ` + fetchable + notExcluded + `
		ORDER BY m.date DESC, m.id DESC
		LIMIT ?`, params
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
