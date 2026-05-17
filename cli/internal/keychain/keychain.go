// Package keychain provides cross-platform credential storage.
// On macOS it uses the security CLI, on Linux it uses secret-tool (libsecret).
package keychain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Service names used by Durian. Centralised here so callers and tests share
// one source of truth (see ADR-0001 §2 for the DB-key naming convention).
const (
	// PasswordKeychainService stores IMAP/SMTP passwords keyed by account email.
	PasswordKeychainService = "durian-password"
	// OAuthKeychainService stores OAuth tokens keyed by account email.
	OAuthKeychainService = "durian"
	// DBKeychainService stores at-rest encryption keys for Durian databases.
	DBKeychainService = "durian-db"
	// DBAccountMaster is the account name for the email.db / contacts.db master key.
	DBAccountMaster = "master"
)

var (
	// ErrNotFound is returned when a keychain item doesn't exist
	ErrNotFound = errors.New("keychain item not found")
)

// Exists checks if a keychain item exists
func Exists(service, account string) bool {
	_, err := GetPassword(service, account)
	return err == nil
}

// GetOrCreateKey returns the hex-decoded raw key stored at (service, account).
// If the entry is missing, a fresh crypto/rand key of nbytes bytes is
// generated, hex-encoded, stored under that name, and returned.
//
// The on-disk representation is hex (ASCII), so both the macOS `security`
// CLI and libsecret `secret-tool` round-trip the value cleanly (they treat
// stored secrets as UTF-8 strings).
//
// Callers must never log the returned bytes.
func GetOrCreateKey(service, account string, nbytes int) ([]byte, error) {
	if nbytes <= 0 {
		return nil, fmt.Errorf("keychain: nbytes must be > 0, got %d", nbytes)
	}

	if existing, err := GetPassword(service, account); err == nil {
		key, decodeErr := hex.DecodeString(existing)
		if decodeErr != nil {
			return nil, fmt.Errorf("keychain: stored value at %s/%s is not valid hex: %w",
				service, account, decodeErr)
		}
		if len(key) != nbytes {
			return nil, fmt.Errorf("keychain: stored key at %s/%s has length %d, want %d",
				service, account, len(key), nbytes)
		}
		return key, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("keychain: lookup %s/%s: %w", service, account, err)
	}

	key := make([]byte, nbytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("keychain: generate random key: %w", err)
	}
	if err := SetPassword(service, account, hex.EncodeToString(key)); err != nil {
		return nil, fmt.Errorf("keychain: store new key at %s/%s: %w", service, account, err)
	}
	return key, nil
}
