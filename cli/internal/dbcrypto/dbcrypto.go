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

	// MaxPlaintextLen caps the bytes any single Encrypt / EncryptX call
	// will accept. SQLite's default SQLITE_MAX_LENGTH is 1 GiB, so any
	// value the store could possibly hand us already fits; this guard
	// exists so the `envelopeOverhead + len(plaintext)` expression in
	// sealAEAD provably can't overflow `int` on 32-bit platforms or trip
	// CodeQL's go/allocation-size-overflow check.
	MaxPlaintextLen = 1 << 30 // 1 GiB
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
	ErrInvalidKey       = errors.New("dbcrypto: key must be 32 bytes")
	ErrShortCiphertext  = errors.New("dbcrypto: ciphertext shorter than envelope overhead")
	ErrUnknownVersion   = errors.New("dbcrypto: unknown envelope version")
	ErrPlaintextTooLong = fmt.Errorf("dbcrypto: plaintext exceeds %d bytes", MaxPlaintextLen)
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
//
// Hot paths that already hold a Keyring should prefer the per-sub-key
// methods (e.g. (*Keyring).EncryptSubject), which reuse a cached AEAD
// instead of re-running aes.NewCipher + cipher.NewGCM per call.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != KeyLen {
		return nil, ErrInvalidKey
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return sealAEAD(gcm, plaintext)
}

// Decrypt opens a V1 envelope produced by Encrypt. It returns the plaintext
// or an error — never both, and never a partial result.
//
// Hot paths that already hold a Keyring should prefer the per-sub-key
// methods (e.g. (*Keyring).DecryptSubject), which reuse a cached AEAD
// instead of re-running aes.NewCipher + cipher.NewGCM per call.
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
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return openAEAD(gcm, ciphertext)
}

// sealAEAD writes the V1 envelope around aead.Seal. The aead must have a
// NonceSize of NonceLen (true for AES-GCM constructed via newGCM). Pulled
// out so both Encrypt (fresh AEAD per call) and the Keyring per-sub-key
// methods (cached AEAD) share one identical wire-format producer — no risk
// of envelope-shape drift between the two paths.
//
// The MaxPlaintextLen guard bounds the make() capacity argument below so
// `envelopeOverhead + len(plaintext)` provably stays within int range
// (CodeQL go/allocation-size-overflow).
func sealAEAD(aead cipher.AEAD, plaintext []byte) ([]byte, error) {
	if len(plaintext) > MaxPlaintextLen {
		return nil, ErrPlaintextTooLong
	}
	// Pre-allocate the full envelope so Seal appends in place.
	out := make([]byte, 1+NonceLen, envelopeOverhead+len(plaintext))
	out[0] = EnvelopeV1
	nonce := out[1 : 1+NonceLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("dbcrypto: nonce: %w", err)
	}
	return aead.Seal(out, nonce, plaintext, nil), nil
}

// openAEAD parses the V1 envelope and calls aead.Open. Callers are
// expected to have already validated len(ciphertext) >= envelopeOverhead
// and ciphertext[0] == EnvelopeV1.
func openAEAD(aead cipher.AEAD, ciphertext []byte) ([]byte, error) {
	nonce := ciphertext[1 : 1+NonceLen]
	body := ciphertext[1+NonceLen:]
	return aead.Open(nil, nonce, body, nil)
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
