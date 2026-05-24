package dbcrypto

import (
	"crypto/subtle"
	"fmt"
)

// Keyring holds the purpose-specific sub-keys derived from a single master
// key via HKDF-SHA256 (see ADR-0001 §3). It is created once at process
// start, retained in memory for the lifetime of the daemon, and consumed
// by the store layer for transparent encrypt-on-write / decrypt-on-read.
//
// Subsequent steps fill in Headers, Addrs, Draft, Contact, FTSToken, Meta
// — each as its own field so callers never have to remember which label
// to pass.
type Keyring struct {
	Subject []byte // encrypts messages.subject (LabelSubject)
	Body    []byte // encrypts messages.body_text + body_html (LabelBody)
	Addrs   []byte // encrypts messages.from_addr + to_addrs + cc_addrs (LabelAddrs)
	Headers []byte // encrypts message_headers.value (LabelHeaders)
	Draft   []byte // encrypts local_drafts.draft_json + outbox.draft_json (LabelDraft)
	Meta    []byte // encrypts mailboxes.name + accounts.name + messages.flags_other (LabelMeta)
	Contact []byte // encrypts contacts.email + contacts.name (LabelContact)
}

// NewKeyring derives every currently-shipped sub-key from a 32-byte master.
// The master is read once and never retained by the returned Keyring; the
// caller is free to (and should) wipe its own copy once this returns.
func NewKeyring(master []byte) (*Keyring, error) {
	subject, err := DeriveSubKey(master, LabelSubject)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive subject sub-key: %w", err)
	}
	body, err := DeriveSubKey(master, LabelBody)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive body sub-key: %w", err)
	}
	addrs, err := DeriveSubKey(master, LabelAddrs)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive addrs sub-key: %w", err)
	}
	headers, err := DeriveSubKey(master, LabelHeaders)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive headers sub-key: %w", err)
	}
	draft, err := DeriveSubKey(master, LabelDraft)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive draft sub-key: %w", err)
	}
	meta, err := DeriveSubKey(master, LabelMeta)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive meta sub-key: %w", err)
	}
	contact, err := DeriveSubKey(master, LabelContact)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive contact sub-key: %w", err)
	}
	return &Keyring{Subject: subject, Body: body, Addrs: addrs, Headers: headers, Draft: draft, Meta: meta, Contact: contact}, nil
}

// Wipe overwrites every sub-key in place with zeroes and nils the slice
// headers, making post-Wipe use of the Keyring panic instead of silently
// returning all-zero ciphertexts. Intended for shutdown paths; Go's GC
// otherwise reclaims the underlying memory eventually.
func (k *Keyring) Wipe() {
	if k == nil {
		return
	}
	zero(k.Subject)
	zero(k.Body)
	zero(k.Addrs)
	zero(k.Headers)
	zero(k.Draft)
	zero(k.Meta)
	zero(k.Contact)
	k.Subject = nil
	k.Body = nil
	k.Addrs = nil
	k.Headers = nil
	k.Draft = nil
	k.Meta = nil
	k.Contact = nil
}

// zero overwrites b with zero bytes in constant time. Used by Wipe.
func zero(b []byte) {
	if len(b) == 0 {
		return
	}
	subtle.ConstantTimeCopy(1, b, make([]byte, len(b)))
}
