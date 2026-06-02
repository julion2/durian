package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/keychain"
	"github.com/julion2/durian/cli/internal/oauth"
	"github.com/julion2/durian/cli/internal/smtp"
)

func restoreFuncs() {
	getKeychainPassword = keychain.GetPassword
	getValidOAuthToken = oauth.GetValidToken
}

// --- Password auth ---

func TestGetSMTPAuth_PasswordSuccess(t *testing.T) {
	getKeychainPassword = func(service, account string) (string, error) {
		if service != PasswordKeychainService {
			t.Errorf("service = %q, want %q", service, PasswordKeychainService)
		}
		if account != "user@example.com" {
			t.Errorf("account = %q, want %q", account, "user@example.com")
		}
		return "s3cret", nil
	}
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "password"},
	}
	auth, err := GetSMTPAuth(acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pa, ok := auth.(*smtp.PasswordAuth)
	if !ok {
		t.Fatalf("got %T, want *smtp.PasswordAuth", auth)
	}
	if pa.Username != "user@example.com" {
		t.Errorf("Username = %q, want %q", pa.Username, "user@example.com")
	}
	if pa.Password != "s3cret" {
		t.Errorf("Password = %q, want %q", pa.Password, "s3cret")
	}
}

func TestGetSMTPAuth_PasswordExplicitUsername(t *testing.T) {
	getKeychainPassword = func(_, _ string) (string, error) { return "pw", nil }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "password"},
		Auth:  &config.AuthConfig{Username: "custom-user"},
	}
	auth, err := GetSMTPAuth(acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pa := auth.(*smtp.PasswordAuth)
	if pa.Username != "custom-user" {
		t.Errorf("Username = %q, want %q", pa.Username, "custom-user")
	}
}

func TestGetSMTPAuth_PasswordFallbackUsername(t *testing.T) {
	getKeychainPassword = func(_, _ string) (string, error) { return "pw", nil }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "password"},
		Auth:  &config.AuthConfig{}, // empty username
	}
	auth, err := GetSMTPAuth(acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pa := auth.(*smtp.PasswordAuth)
	if pa.Username != "user@example.com" {
		t.Errorf("Username = %q, want email fallback %q", pa.Username, "user@example.com")
	}
}

func TestGetSMTPAuth_PasswordNotFound(t *testing.T) {
	getKeychainPassword = func(_, _ string) (string, error) { return "", keychain.ErrNotFound }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "password"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no password stored") {
		t.Errorf("error = %q, want 'no password stored'", err.Error())
	}
}

func TestGetSMTPAuth_PasswordKeychainError(t *testing.T) {
	getKeychainPassword = func(_, _ string) (string, error) { return "", errors.New("keychain locked") }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "password"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "keychain") {
		t.Errorf("error = %q, should mention keychain", err.Error())
	}
}

// --- OAuth2 auth ---

func TestGetSMTPAuth_OAuth2Success(t *testing.T) {
	getValidOAuthToken = func(email, clientID, clientSecret, tenant string) (*oauth.Token, error) {
		if email != "user@example.com" {
			t.Errorf("email = %q, want %q", email, "user@example.com")
		}
		return &oauth.Token{AccessToken: "tok123", Expiry: time.Now().Add(time.Hour)}, nil
	}
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "oauth2"},
		OAuth: &config.OAuthConfig{Provider: "google", ClientID: "cid", ClientSecret: "cs"},
	}
	auth, err := GetSMTPAuth(acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oa, ok := auth.(*smtp.OAuth2Auth)
	if !ok {
		t.Fatalf("got %T, want *smtp.OAuth2Auth", auth)
	}
	if oa.Email != "user@example.com" {
		t.Errorf("Email = %q, want %q", oa.Email, "user@example.com")
	}
	if oa.AccessToken != "tok123" {
		t.Errorf("AccessToken = %q, want %q", oa.AccessToken, "tok123")
	}
}

func TestGetSMTPAuth_OAuth2MissingProvider(t *testing.T) {
	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "oauth2"},
		OAuth: &config.OAuthConfig{}, // no provider
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "provider not configured") {
		t.Errorf("error = %q, want 'provider not configured'", err.Error())
	}
}

func TestGetSMTPAuth_OAuth2TokenNotFound(t *testing.T) {
	getValidOAuthToken = func(_, _, _, _ string) (*oauth.Token, error) { return nil, oauth.ErrTokenNotFound }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "oauth2"},
		OAuth: &config.OAuthConfig{Provider: "google"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error = %q, want 'not authenticated'", err.Error())
	}
}

func TestGetSMTPAuth_OAuth2TokenExpired(t *testing.T) {
	getValidOAuthToken = func(_, _, _, _ string) (*oauth.Token, error) { return nil, oauth.ErrTokenExpired }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "oauth2"},
		OAuth: &config.OAuthConfig{Provider: "microsoft"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want 'expired'", err.Error())
	}
}

func TestGetSMTPAuth_OAuth2OtherError(t *testing.T) {
	getValidOAuthToken = func(_, _, _, _ string) (*oauth.Token, error) { return nil, errors.New("network timeout") }
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "oauth2"},
		OAuth: &config.OAuthConfig{Provider: "google"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "OAuth token") {
		t.Errorf("error = %q, should mention OAuth token", err.Error())
	}
}

func TestGetSMTPAuth_OAuth2SharedMailbox(t *testing.T) {
	var calledEmail string
	getValidOAuthToken = func(email, _, _, _ string) (*oauth.Token, error) {
		calledEmail = email
		return &oauth.Token{AccessToken: "delegated-tok", Expiry: time.Now().Add(time.Hour)}, nil
	}
	defer restoreFuncs()

	acct := &config.AccountConfig{
		Email:     "shared@example.com",
		AuthEmail: "delegator@example.com",
		SMTP:      config.SMTPConfig{Auth: "oauth2"},
		OAuth:     &config.OAuthConfig{Provider: "microsoft", ClientID: "cid", Tenant: "t"},
	}
	auth, err := GetSMTPAuth(acct)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calledEmail != "delegator@example.com" {
		t.Errorf("token lookup email = %q, want delegator %q", calledEmail, "delegator@example.com")
	}
	oa := auth.(*smtp.OAuth2Auth)
	if oa.Email != "delegator@example.com" {
		t.Errorf("OAuth2Auth.Email = %q, want delegator %q", oa.Email, "delegator@example.com")
	}
}

// --- Unsupported ---

func TestGetSMTPAuth_UnsupportedMethod(t *testing.T) {
	acct := &config.AccountConfig{
		Email: "user@example.com",
		SMTP:  config.SMTPConfig{Auth: "plain"},
	}
	_, err := GetSMTPAuth(acct)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported auth method") {
		t.Errorf("error = %q, want 'unsupported auth method'", err.Error())
	}
}
