// Package dbcrypto implements application-layer encryption for Durian's
// at-rest sensitive columns. See ADR-0001 (mail content encryption at rest).
//
// Wire format (envelope V1):
//
//	version(1B=0x01) || nonce(12B) || ciphertext || tag(16B)
//
// Primitives are AES-256-GCM with random nonces (NIST SP 800-38D §8.2.2) and
// HKDF-SHA256 sub-key derivation (RFC 5869). No third-party crypto.
package dbcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

const (
	// EnvelopeV1 is the version byte that prefixes every ciphertext written
	// by this package. Readers route on this byte; rotating the cipher
	// suite means introducing EnvelopeV2 alongside it.
	EnvelopeV1 byte = 0x01

	KeyLen   = 32 // AES-256
	NonceLen = 12 // AES-GCM standard nonce size
	TagLen   = 16 // AES-GCM tag size

	// MasterKeyLen is the length of the root key stored in the OS keychain.
	MasterKeyLen = 32

	// envelopeOverhead is the minimum bytes any V1 ciphertext occupies,
	// even for empty plaintext: version + nonce + tag.
	envelopeOverhead = 1 + NonceLen + TagLen
)

// Label is the HKDF "info" string for one sub-key purpose. The exact byte
// values are part of the on-disk format: changing a label invalidates every
// existing ciphertext for that column. ADR-0001 §3 freezes these strings.
type Label string

const (
	LabelBody     Label = "durian/v1/body"
	LabelSubject  Label = "durian/v1/subject"
	LabelHeaders  Label = "durian/v1/headers"
	LabelAddrs    Label = "durian/v1/addrs"
	LabelDraft    Label = "durian/v1/draft"
	LabelContact  Label = "durian/v1/contact"
	LabelFTSToken Label = "durian/v1/fts-token"
	LabelMeta     Label = "durian/v1/meta"
)

var (
	ErrInvalidKey      = errors.New("dbcrypto: key must be 32 bytes")
	ErrShortCiphertext = errors.New("dbcrypto: ciphertext shorter than envelope overhead")
	ErrUnknownVersion  = errors.New("dbcrypto: unknown envelope version")
)

// DeriveSubKey extracts a 32-byte purpose-specific sub-key from a 32-byte
// master via HKDF-SHA256. Salt is nil; the label is the HKDF `info` input.
// Deterministic — same (master, label) always yields the same sub-key.
func DeriveSubKey(master []byte, label Label) ([]byte, error) {
	if len(master) != MasterKeyLen {
		return nil, ErrInvalidKey
	}
	return hkdf.Key(sha256.New, master, nil, string(label), KeyLen)
}

// Encrypt seals plaintext under key using AES-256-GCM and wraps the result
// in a V1 envelope. A fresh random nonce is drawn for every call.
//
// The returned slice is newly allocated and safe for the caller to retain.
// Plaintext is not modified.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, ErrInvalidKey
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	// Pre-allocate the full envelope so Seal appends in place.
	out := make([]byte, 1+NonceLen, envelopeOverhead+len(plaintext))
	out[0] = EnvelopeV1
	nonce := out[1 : 1+NonceLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("dbcrypto: nonce: %w", err)
	}
	return gcm.Seal(out, nonce, plaintext, nil), nil
}

// Decrypt opens a V1 envelope produced by Encrypt. It returns the plaintext
// or an error — never both, and never a partial result.
//
// Check order matters: key length first (AES constructor would panic on a
// bad-length key), then envelope length (no slice access without a bound
// check), then version routing, then the AEAD open. The gcm.Open error is
// returned unwrapped on purpose — wrapping it with %w would give callers a
// distinguishable string for "tag failed" vs other failures and that's the
// shape of a padding-oracle-style side channel.
func Decrypt(key, ciphertext []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, ErrInvalidKey
	}
	if len(ciphertext) < envelopeOverhead {
		return nil, ErrShortCiphertext
	}
	if ciphertext[0] != EnvelopeV1 {
		return nil, ErrUnknownVersion
	}
	nonce := ciphertext[1 : 1+NonceLen]
	body := ciphertext[1+NonceLen:]
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, body, nil)
}

// newGCM is the shared AES-GCM construction step. Pulled out so Encrypt and
// Decrypt share the exact same primitive setup — no chance of drift.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: aes init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("dbcrypto: gcm init: %w", err)
	}
	return gcm, nil
}
