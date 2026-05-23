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
// Step 5 only derives Subject. Subsequent steps fill in Body, Headers,
// Addrs, Draft, Contact, FTSToken, Meta — each as its own field so callers
// never have to remember which label to pass.
type Keyring struct {
	Subject []byte // encrypts messages.subject (LabelSubject)
}

// NewKeyring derives every step-5 sub-key from a 32-byte master key.
// The master is read once and never retained by the returned Keyring; the
// caller is free to (and should) wipe its own copy once this returns.
func NewKeyring(master []byte) (*Keyring, error) {
	subject, err := DeriveSubKey(master, LabelSubject)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: derive subject sub-key: %w", err)
	}
	return &Keyring{Subject: subject}, nil
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
	k.Subject = nil
}

// zero overwrites b with zero bytes in constant time. Used by Wipe.
func zero(b []byte) {
	if len(b) == 0 {
		return
	}
	subtle.ConstantTimeCopy(1, b, make([]byte, len(b)))
}
