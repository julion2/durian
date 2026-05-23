// Package agekey wraps filippo.io/age for the single use case ADR-0001 §3
// reserves for it: passphrase-protected export and import of the master
// encryption key for disaster recovery.
//
// Encryption uses age's scrypt recipient (RFC-7914 password-based KDF) at
// the library's default work factor. The resulting ciphertext is ASCII-
// armored age format so it can be pasted into a password manager.
//
// This package must NOT be used on hot mail paths — the per-blob age
// envelope overhead and the scrypt cost would dominate. AES-GCM via
// cli/internal/dbcrypto is the at-rest cipher for stored mail.
package agekey

import (
	"bytes"
	"fmt"
	"io"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Encrypt encrypts plaintext under a passphrase-derived scrypt recipient and
// returns ASCII-armored age ciphertext. The caller is responsible for not
// retaining or logging passphrase or plaintext.
func Encrypt(plaintext []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("agekey: empty passphrase")
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("agekey: scrypt recipient: %w", err)
	}
	var buf bytes.Buffer
	armorW := armor.NewWriter(&buf)
	w, err := age.Encrypt(armorW, r)
	if err != nil {
		return nil, fmt.Errorf("agekey: encrypt init: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("agekey: encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("agekey: encrypt close: %w", err)
	}
	if err := armorW.Close(); err != nil {
		return nil, fmt.Errorf("agekey: armor close: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt reverses Encrypt. A wrong passphrase produces an error from the
// underlying age library — it is returned unwrapped so callers cannot
// distinguish "bad passphrase" from other failures via the error string
// (defence in depth against passphrase-guessing oracles).
func Decrypt(ciphertext []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("agekey: empty passphrase")
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("agekey: scrypt identity: %w", err)
	}
	armorR := armor.NewReader(bytes.NewReader(ciphertext))
	r, err := age.Decrypt(armorR, id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}
