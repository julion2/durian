package store

import (
	"fmt"
	"net/textproto"
)

// InsertHeader stores a message header. Overwrites if it already exists.
// ADR-0001 step 7e: only value_ct is written — the plaintext value
// column is dropped in the v17→v18 migration.
func (d *DB) InsertHeader(messageDBID int64, name, value string) error {
	valueCT, err := d.encryptHeaderValue(value)
	if err != nil {
		return fmt.Errorf("encrypt header value: %w", err)
	}
	_, err = d.db.Exec(
		"INSERT OR REPLACE INTO message_headers (message_id, name, value_ct) VALUES (?, ?, ?)",
		messageDBID, name, valueCT)
	if err != nil {
		return fmt.Errorf("insert header: %w", err)
	}
	return nil
}

// GetHeader returns a single header value for a message. Returns "" if not found.
func (d *DB) GetHeader(messageDBID int64, name string) (string, error) {
	var valueCT []byte
	err := d.db.QueryRow(
		"SELECT value_ct FROM message_headers WHERE message_id = ? AND name = ?",
		messageDBID, name).Scan(&valueCT)
	if err != nil {
		return "", nil // not found
	}
	out, err := d.decryptHeaderValue("", valueCT)
	if err != nil {
		return "", err
	}
	return out, nil
}

// HasHeaders returns true if the message has any stored headers.
func (d *DB) HasHeaders(messageDBID int64) (bool, error) {
	var count int
	err := d.db.QueryRow(
		"SELECT COUNT(*) FROM message_headers WHERE message_id = ?",
		messageDBID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetMessageDBID returns the internal DB ID for a message by its RFC822 Message-ID
// and account. Returns 0 if not found. Step 7f: account is resolved to
// its accounts.id; an empty account string matches rows where
// account_id IS NULL (the empty-Account upsert path).
func (d *DB) GetMessageDBID(messageID, account string) (int64, error) {
	var id int64
	var err error
	if account == "" {
		err = d.db.QueryRow(
			"SELECT id FROM messages WHERE message_id = ? AND account_id IS NULL",
			messageID).Scan(&id)
	} else {
		var accountID int64
		if err = d.db.QueryRow("SELECT id FROM accounts WHERE name = ?", account).Scan(&accountID); err != nil {
			return 0, nil
		}
		err = d.db.QueryRow(
			"SELECT id FROM messages WHERE message_id = ? AND account_id = ?",
			messageID, accountID).Scan(&id)
	}
	if err != nil {
		return 0, nil
	}
	return id, nil
}

// AllMessages returns all messages with fields needed for rule matching.
// from_addr/to_addrs/cc_addrs are plaintext (ADR-0001 §3 revision);
// subject/body_text are ct-only after step 7e. Step 7f: account name
// comes from the accounts LEFT JOIN + meta_key decrypt.
func (d *DB) AllMessages() ([]*Message, error) {
	rows, err := d.db.Query(`SELECT m.id, m.message_id, m.subject_ct,
		m.from_addr, m.to_addrs, m.cc_addrs,
		m.body_text_ct, ac.name_ct
		FROM messages m
		LEFT JOIN accounts ac ON ac.id = m.account_id`)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var subjectCT, bodyTextCT, accountCT []byte
		if err := rows.Scan(&m.ID, &m.MessageID, &subjectCT,
			&m.FromAddr, &m.ToAddrs, &m.CCAddrs,
			&bodyTextCT, &accountCT); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if m.Subject, err = d.decryptSubject("", subjectCT); err != nil {
			return nil, err
		}
		if m.BodyText, err = d.decryptBody("", bodyTextCT); err != nil {
			return nil, err
		}
		if m.Account, err = d.decryptMeta("", accountCT); err != nil {
			return nil, fmt.Errorf("decrypt account name: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// AllHeadersByMessage loads all stored headers, keyed by message DB ID.
// Header names are returned in canonical MIME form (e.g. "List-Unsubscribe").
func (d *DB) AllHeadersByMessage() (map[int64]map[string][]string, error) {
	rows, err := d.db.Query("SELECT message_id, name, value_ct FROM message_headers")
	if err != nil {
		return nil, fmt.Errorf("query headers: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]map[string][]string)
	for rows.Next() {
		var msgID int64
		var name string
		var valueCT []byte
		if err := rows.Scan(&msgID, &name, &valueCT); err != nil {
			return nil, fmt.Errorf("scan header: %w", err)
		}
		plain, err := d.decryptHeaderValue("", valueCT)
		if err != nil {
			return nil, err
		}
		if result[msgID] == nil {
			result[msgID] = make(map[string][]string)
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		result[msgID][canonical] = append(result[msgID][canonical], plain)
	}
	return result, rows.Err()
}

// AttachmentCounts returns the number of attachments per message DB ID.
func (d *DB) AttachmentCounts() (map[int64]int, error) {
	rows, err := d.db.Query("SELECT message_db_id, COUNT(*) FROM attachments GROUP BY message_db_id")
	if err != nil {
		return nil, fmt.Errorf("query attachment counts: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]int)
	for rows.Next() {
		var msgID int64
		var count int
		if err := rows.Scan(&msgID, &count); err != nil {
			return nil, fmt.Errorf("scan attachment count: %w", err)
		}
		result[msgID] = count
	}
	return result, rows.Err()
}

// AttachmentMeta holds minimal attachment info for rule matching.
type AttachmentMeta struct {
	ContentType string
	Filename    string
}

// AttachmentsByMessage returns attachment metadata grouped by message DB ID.
//
// After ADR-0001 attachments-encryption (v21→v22) the content_type and
// filename columns are encrypted BLOBs (content_type_ct, filename_ct)
// sealed under the meta sub-key. Each row gets two decryptMeta calls;
// the rules engine calls this once per `rules apply` so the cost is
// bounded by total attachment count, not message count.
func (d *DB) AttachmentsByMessage() (map[int64][]AttachmentMeta, error) {
	rows, err := d.db.Query("SELECT message_db_id, content_type_ct, filename_ct FROM attachments")
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]AttachmentMeta)
	for rows.Next() {
		var msgID int64
		var contentTypeCT, filenameCT []byte
		if err := rows.Scan(&msgID, &contentTypeCT, &filenameCT); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		ct, err := d.decryptMeta("", contentTypeCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt content_type msg=%d: %w", msgID, err)
		}
		fn, err := d.decryptMeta("", filenameCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt filename msg=%d: %w", msgID, err)
		}
		result[msgID] = append(result[msgID], AttachmentMeta{ContentType: ct, Filename: fn})
	}
	return result, rows.Err()
}
