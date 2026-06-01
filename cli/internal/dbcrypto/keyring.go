package dbcrypto

import (
	"crypto/cipher"
	"crypto/subtle"
	"fmt"
)

// Keyring holds the purpose-specific sub-keys derived from a single master
// key via HKDF-SHA256 (see ADR-0001 §3). It is created once at process
// start, retained in memory for the lifetime of the daemon, and consumed
// by the store layer for transparent encrypt-on-write / decrypt-on-read.
//
// Each AES-GCM sub-key has its cipher.AEAD constructed once in NewKeyring
// and cached on the unexported *AEAD field. The per-sub-key methods
// (EncryptSubject, DecryptSubject, …) reuse that cached AEAD on every
// call; on a full-mailbox decrypt scan (~50k rows) this is ≥2× the cost
// of the package-level Encrypt/Decrypt path, which re-runs aes.NewCipher
// + cipher.NewGCM per row.
type Keyring struct {
	Subject  []byte // encrypts messages.subject (LabelSubject)
	Body     []byte // encrypts messages.body_text + body_html (LabelBody)
	Addrs    []byte // encrypts messages.from_addr + to_addrs + cc_addrs (LabelAddrs)
	Headers  []byte // encrypts message_headers.value (LabelHeaders)
	Draft    []byte // encrypts local_drafts.draft_json + outbox.draft_json (LabelDraft)
	Meta     []byte // encrypts mailboxes.name + accounts.name + messages.flags_other (LabelMeta)
	Contact  []byte // encrypts contacts.email + contacts.name (LabelContact)
	FTSToken []byte // HMAC key for blind FTS5 search tokens (LabelFTSToken)

	// Cached AES-GCM constructions, one per AEAD sub-key. FTSToken is
	// excluded — it is consumed as an HMAC key, not an AEAD.
	subjectAEAD cipher.AEAD
	bodyAEAD    cipher.AEAD
	addrsAEAD   cipher.AEAD
	headersAEAD cipher.AEAD
	draftAEAD   cipher.AEAD
	metaAEAD    cipher.AEAD
	contactAEAD cipher.AEAD
}

// NewKeyring derives every currently-shipped sub-key from a 32-byte master
// and pre-builds the AES-GCM AEAD for each AEAD sub-key. The master is
// read once and never retained by the returned Keyring; the caller is free
// to (and should) wipe its own copy once this returns.
func NewKeyring(master []byte) (*Keyring, error) {
	type spec struct {
		label Label
		dst   *[]byte
	}
	subkeys := []spec{
		{LabelSubject, nil},
		{LabelBody, nil},
		{LabelAddrs, nil},
		{LabelHeaders, nil},
		{LabelDraft, nil},
		{LabelMeta, nil},
		{LabelContact, nil},
		{LabelFTSToken, nil},
	}
	kr := &Keyring{}
	subkeys[0].dst = &kr.Subject
	subkeys[1].dst = &kr.Body
	subkeys[2].dst = &kr.Addrs
	subkeys[3].dst = &kr.Headers
	subkeys[4].dst = &kr.Draft
	subkeys[5].dst = &kr.Meta
	subkeys[6].dst = &kr.Contact
	subkeys[7].dst = &kr.FTSToken

	for _, s := range subkeys {
		k, err := DeriveSubKey(master, s.label)
		if err != nil {
			return nil, fmt.Errorf("dbcrypto: derive %s sub-key: %w", s.label, err)
		}
		*s.dst = k
	}

	// Build the AEAD for each encryption sub-key exactly once. FTSToken
	// is omitted on purpose; it never reaches AES-GCM.
	aeadFor := func(key []byte, label Label) (cipher.AEAD, error) {
		aead, err := newGCM(key)
		if err != nil {
			return nil, fmt.Errorf("dbcrypto: build %s AEAD: %w", label, err)
		}
		return aead, nil
	}
	var err error
	if kr.subjectAEAD, err = aeadFor(kr.Subject, LabelSubject); err != nil {
		return nil, err
	}
	if kr.bodyAEAD, err = aeadFor(kr.Body, LabelBody); err != nil {
		return nil, err
	}
	if kr.addrsAEAD, err = aeadFor(kr.Addrs, LabelAddrs); err != nil {
		return nil, err
	}
	if kr.headersAEAD, err = aeadFor(kr.Headers, LabelHeaders); err != nil {
		return nil, err
	}
	if kr.draftAEAD, err = aeadFor(kr.Draft, LabelDraft); err != nil {
		return nil, err
	}
	if kr.metaAEAD, err = aeadFor(kr.Meta, LabelMeta); err != nil {
		return nil, err
	}
	if kr.contactAEAD, err = aeadFor(kr.Contact, LabelContact); err != nil {
		return nil, err
	}
	return kr, nil
}

// EncryptSubject seals plaintext under the cached subject AEAD.
func (k *Keyring) EncryptSubject(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.subjectAEAD, plaintext)
}

// DecryptSubject opens a V1 envelope under the cached subject AEAD.
func (k *Keyring) DecryptSubject(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.subjectAEAD, ciphertext)
}

// EncryptBody seals plaintext under the cached body AEAD.
func (k *Keyring) EncryptBody(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.bodyAEAD, plaintext)
}

// DecryptBody opens a V1 envelope under the cached body AEAD.
func (k *Keyring) DecryptBody(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.bodyAEAD, ciphertext)
}

// EncryptAddrs seals plaintext under the cached addrs AEAD.
func (k *Keyring) EncryptAddrs(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.addrsAEAD, plaintext)
}

// DecryptAddrs opens a V1 envelope under the cached addrs AEAD.
func (k *Keyring) DecryptAddrs(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.addrsAEAD, ciphertext)
}

// EncryptHeaders seals plaintext under the cached headers AEAD.
func (k *Keyring) EncryptHeaders(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.headersAEAD, plaintext)
}

// DecryptHeaders opens a V1 envelope under the cached headers AEAD.
func (k *Keyring) DecryptHeaders(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.headersAEAD, ciphertext)
}

// EncryptDraft seals plaintext under the cached draft AEAD.
func (k *Keyring) EncryptDraft(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.draftAEAD, plaintext)
}

// DecryptDraft opens a V1 envelope under the cached draft AEAD.
func (k *Keyring) DecryptDraft(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.draftAEAD, ciphertext)
}

// EncryptMeta seals plaintext under the cached meta AEAD.
func (k *Keyring) EncryptMeta(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.metaAEAD, plaintext)
}

// DecryptMeta opens a V1 envelope under the cached meta AEAD.
func (k *Keyring) DecryptMeta(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.metaAEAD, ciphertext)
}

// EncryptContact seals plaintext under the cached contact AEAD.
func (k *Keyring) EncryptContact(plaintext []byte) ([]byte, error) {
	return sealAEAD(k.contactAEAD, plaintext)
}

// DecryptContact opens a V1 envelope under the cached contact AEAD.
func (k *Keyring) DecryptContact(ciphertext []byte) ([]byte, error) {
	return decryptCached(k.contactAEAD, ciphertext)
}

// decryptCached duplicates the structural checks from package-level
// Decrypt (length and version) so the cached-AEAD path errors with the
// same shape as the fresh-AEAD path. Pulled out of every per-sub-key
// method to keep that contract in one place.
func decryptCached(aead cipher.AEAD, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < envelopeOverhead {
		return nil, ErrShortCiphertext
	}
	if ciphertext[0] != EnvelopeV1 {
		return nil, ErrUnknownVersion
	}
	return openAEAD(aead, ciphertext)
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
	zero(k.FTSToken)
	k.Subject = nil
	k.Body = nil
	k.Addrs = nil
	k.Headers = nil
	k.Draft = nil
	k.Meta = nil
	k.Contact = nil
	k.FTSToken = nil
}

// zero overwrites b with zero bytes in constant time. Used by Wipe.
func zero(b []byte) {
	if len(b) == 0 {
		return
	}
	subtle.ConstantTimeCopy(1, b, make([]byte, len(b)))
}
