package store

import (
	"fmt"
	"net/textproto"
)

// InsertHeader stores a message header. Overwrites if the header already exists.
// Writes both plaintext value (so existing reads still work) and value_ct
// (ADR-0001 step 6). The plaintext column dies in step 7.
func (d *DB) InsertHeader(messageDBID int64, name, value string) error {
	valueCT, err := d.encryptHeaderValue(value)
	if err != nil {
		return fmt.Errorf("encrypt header value: %w", err)
	}
	_, err = d.db.Exec(
		"INSERT OR REPLACE INTO message_headers (message_id, name, value, value_ct) VALUES (?, ?, ?, ?)",
		messageDBID, name, value, valueCT)
	if err != nil {
		return fmt.Errorf("insert header: %w", err)
	}
	return nil
}

// GetHeader returns a single header value for a message. Returns "" if not found.
func (d *DB) GetHeader(messageDBID int64, name string) (string, error) {
	var value string
	var valueCT []byte
	err := d.db.QueryRow(
		"SELECT value, value_ct FROM message_headers WHERE message_id = ? AND name = ?",
		messageDBID, name).Scan(&value, &valueCT)
	if err != nil {
		return "", nil // not found
	}
	out, err := d.decryptHeaderValue(value, valueCT)
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
// and account. Returns 0 if not found.
func (d *DB) GetMessageDBID(messageID, account string) (int64, error) {
	var id int64
	err := d.db.QueryRow(
		"SELECT id FROM messages WHERE message_id = ? AND account = ?",
		messageID, account).Scan(&id)
	if err != nil {
		return 0, nil
	}
	return id, nil
}

// AllMessages returns all messages with fields needed for rule matching.
// from_addr/to_addrs/cc_addrs are plaintext (ADR-0001 §3 revision).
func (d *DB) AllMessages() ([]*Message, error) {
	rows, err := d.db.Query(`SELECT id, message_id, subject, subject_ct,
		from_addr, to_addrs, cc_addrs,
		body_text, body_text_ct, account FROM messages`)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var msgs []*Message
	for rows.Next() {
		m := &Message{}
		var subjectCT, bodyTextCT []byte
		if err := rows.Scan(&m.ID, &m.MessageID, &m.Subject, &subjectCT,
			&m.FromAddr, &m.ToAddrs, &m.CCAddrs,
			&m.BodyText, &bodyTextCT, &m.Account); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if m.Subject, err = d.decryptSubject(m.Subject, subjectCT); err != nil {
			return nil, err
		}
		if m.BodyText, err = d.decryptBody(m.BodyText, bodyTextCT); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// AllHeadersByMessage loads all stored headers, keyed by message DB ID.
// Header names are returned in canonical MIME form (e.g. "List-Unsubscribe").
func (d *DB) AllHeadersByMessage() (map[int64]map[string][]string, error) {
	rows, err := d.db.Query("SELECT message_id, name, value, value_ct FROM message_headers")
	if err != nil {
		return nil, fmt.Errorf("query headers: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]map[string][]string)
	for rows.Next() {
		var msgID int64
		var name, value string
		var valueCT []byte
		if err := rows.Scan(&msgID, &name, &value, &valueCT); err != nil {
			return nil, fmt.Errorf("scan header: %w", err)
		}
		plain, err := d.decryptHeaderValue(value, valueCT)
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
func (d *DB) AttachmentsByMessage() (map[int64][]AttachmentMeta, error) {
	rows, err := d.db.Query("SELECT message_db_id, content_type, filename FROM attachments")
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]AttachmentMeta)
	for rows.Next() {
		var msgID int64
		var ct, fn string
		if err := rows.Scan(&msgID, &ct, &fn); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		result[msgID] = append(result[msgID], AttachmentMeta{ContentType: ct, Filename: fn})
	}
	return result, rows.Err()
}
