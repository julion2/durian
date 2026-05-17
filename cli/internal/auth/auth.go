// Package auth provides shared authentication helpers for SMTP sending.
package auth

import (
	"errors"
	"fmt"

	"github.com/durian-dev/durian/cli/internal/config"
	"github.com/durian-dev/durian/cli/internal/keychain"
	"github.com/durian-dev/durian/cli/internal/oauth"
	"github.com/durian-dev/durian/cli/internal/smtp"
)

// PasswordKeychainService re-exports the canonical constant from the
// keychain package so existing callers (and tests) keep compiling.
const PasswordKeychainService = keychain.PasswordKeychainService

// Replaceable function vars for testability.
var (
	getKeychainPassword = keychain.GetPassword
	getValidOAuthToken  = oauth.GetValidToken
)

// GetSMTPAuth returns the appropriate SMTP auth method for the given account.
func GetSMTPAuth(account *config.AccountConfig) (smtp.Auth, error) {
	switch account.SMTP.Auth {
	case "oauth2":
		if account.OAuth == nil || account.OAuth.Provider == "" {
			return nil, fmt.Errorf("OAuth provider not configured for %s", account.Email)
		}

		// For shared mailboxes, the token belongs to the delegating user (AuthEmail).
		// SMTP also requires the SASL user to be the delegator — shared mailboxes
		// cannot perform SMTP AUTH. MAIL FROM / From: header use account.Email downstream.
		authEmail := account.GetAuthEmail()
		token, err := getValidOAuthToken(authEmail, account.OAuth.ClientID, account.OAuth.ClientSecret, account.OAuth.Tenant)
		if err != nil {
			if errors.Is(err, oauth.ErrTokenNotFound) {
				return nil, fmt.Errorf("not authenticated for %s", authEmail)
			}
			if errors.Is(err, oauth.ErrTokenExpired) {
				return nil, fmt.Errorf("authentication expired for %s", authEmail)
			}
			return nil, fmt.Errorf("failed to get OAuth token: %w", err)
		}

		return &smtp.OAuth2Auth{
			Email:       authEmail,
			AccessToken: token.AccessToken,
		}, nil

	case "password":
		password, err := getKeychainPassword(PasswordKeychainService, account.Email)
		if err != nil {
			if errors.Is(err, keychain.ErrNotFound) {
				return nil, fmt.Errorf("no password stored for %s", account.Email)
			}
			return nil, fmt.Errorf("failed to get password from keychain: %w", err)
		}

		username := account.Email
		if account.Auth != nil && account.Auth.Username != "" {
			username = account.Auth.Username
		}

		return &smtp.PasswordAuth{
			Username: username,
			Password: password,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported auth method: %s", account.SMTP.Auth)
	}
}
