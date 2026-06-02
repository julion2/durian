package oauth

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/julion2/durian/cli/internal/keychain"
)

const (
	// KeychainService is the service name used for storing OAuth tokens
	KeychainService = "durian"
)

// SaveToken stores an OAuth token in the macOS Keychain
func SaveToken(email string, token *Token) error {
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}

	if err := keychain.SetPassword(KeychainService, email, string(data)); err != nil {
		return fmt.Errorf("failed to save token to keychain: %w", err)
	}

	return nil
}

// LoadToken retrieves an OAuth token from the macOS Keychain
func LoadToken(email string) (*Token, error) {
	data, err := keychain.GetPassword(KeychainService, email)
	if err != nil {
		if errors.Is(err, keychain.ErrNotFound) {
			return nil, ErrTokenNotFound
		}
		return nil, fmt.Errorf("failed to query keychain: %w", err)
	}

	var token Token
	if err := json.Unmarshal([]byte(data), &token); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token: %w", err)
	}

	return &token, nil
}

// DeleteToken removes an OAuth token from the macOS Keychain
func DeleteToken(email string) error {
	if err := keychain.DeletePassword(KeychainService, email); err != nil {
		return fmt.Errorf("failed to delete token from keychain: %w", err)
	}
	return nil
}
