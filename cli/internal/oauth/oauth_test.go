package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error: %v", err)
	}

	// Verifier should be 43 characters (32 bytes base64url encoded without padding)
	if len(pkce.Verifier) != 43 {
		t.Errorf("Verifier length = %d, want 43", len(pkce.Verifier))
	}

	// Challenge should be 43 characters (32 bytes SHA256 hash base64url encoded without padding)
	if len(pkce.Challenge) != 43 {
		t.Errorf("Challenge length = %d, want 43", len(pkce.Challenge))
	}

	// Method should always be S256
	if pkce.Method != "S256" {
		t.Errorf("Method = %q, want %q", pkce.Method, "S256")
	}

	// Verify challenge is correct SHA256 of verifier
	hash := sha256.Sum256([]byte(pkce.Verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(hash[:])
	if pkce.Challenge != expectedChallenge {
		t.Errorf("Challenge mismatch: got %q, want %q", pkce.Challenge, expectedChallenge)
	}
}

func TestGeneratePKCEUniqueness(t *testing.T) {
	// Generate multiple PKCEs and ensure they're unique
	seen := make(map[string]bool)

	for i := 0; i < 100; i++ {
		pkce, err := GeneratePKCE()
		if err != nil {
			t.Fatalf("GeneratePKCE() error: %v", err)
		}

		if seen[pkce.Verifier] {
			t.Error("GeneratePKCE() produced duplicate verifier")
		}
		seen[pkce.Verifier] = true
	}
}

func TestMicrosoftProvider(t *testing.T) {
	// Test with default tenant
	provider := Microsoft("")
	if provider.Name != "microsoft" {
		t.Errorf("Name = %q, want %q", provider.Name, "microsoft")
	}
	if !strings.Contains(provider.AuthorizeEndpoint, "/common/") {
		t.Errorf("AuthorizeEndpoint should contain /common/, got %q", provider.AuthorizeEndpoint)
	}
	if provider.SASlMethod != "XOAUTH2" {
		t.Errorf("SASlMethod = %q, want %q", provider.SASlMethod, "XOAUTH2")
	}

	// Test with custom tenant
	provider = Microsoft("my-tenant-id")
	if !strings.Contains(provider.AuthorizeEndpoint, "/my-tenant-id/") {
		t.Errorf("AuthorizeEndpoint should contain /my-tenant-id/, got %q", provider.AuthorizeEndpoint)
	}
}

func TestGoogleProvider(t *testing.T) {
	provider := Google()
	if provider.Name != "google" {
		t.Errorf("Name = %q, want %q", provider.Name, "google")
	}
	if provider.SASlMethod != "OAUTHBEARER" {
		t.Errorf("SASlMethod = %q, want %q", provider.SASlMethod, "OAUTHBEARER")
	}
}

func TestGetProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErr  bool
	}{
		{"microsoft", "microsoft", false},
		{"Microsoft", "Microsoft", false},
		{"google", "google", false},
		{"unknown", "unknown", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GetProvider(tt.provider, "")
			if (err != nil) != tt.wantErr {
				t.Errorf("GetProvider(%q) error = %v, wantErr %v", tt.provider, err, tt.wantErr)
			}
		})
	}
}

func TestAuthorizationURL(t *testing.T) {
	provider := Microsoft("")
	pkce := &PKCE{
		Verifier:  "test-verifier",
		Challenge: "test-challenge",
		Method:    "S256",
	}

	url := provider.AuthorizationURL("client-123", "http://localhost:8080/callback", "state-xyz", "user@example.com", pkce)

	// Check URL contains required parameters
	requiredParams := []string{
		"client_id=client-123",
		"response_type=code",
		"redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fcallback",
		"state=state-xyz",
		"code_challenge=test-challenge",
		"code_challenge_method=S256",
		"login_hint=user%40example.com",
	}

	for _, param := range requiredParams {
		if !strings.Contains(url, param) {
			t.Errorf("URL missing parameter %q\nURL: %s", param, url)
		}
	}
}

func TestBuildXOAuth2String(t *testing.T) {
	result := BuildXOAuth2String("user@example.com", "access-token-123")
	expected := "user=user@example.com\x01auth=Bearer access-token-123\x01\x01"

	if result != expected {
		t.Errorf("BuildXOAuth2String() = %q, want %q", result, expected)
	}
}

func TestTokenExpiry(t *testing.T) {
	// Token that expires in 1 hour
	token := &Token{
		Provider:     "microsoft",
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		Expiry:       time.Now().Add(1 * time.Hour),
	}

	if token.IsExpired() {
		t.Error("Token should not be expired")
	}

	if token.IsExpiredWithBuffer(30 * time.Minute) {
		t.Error("Token should not be expired with 30min buffer")
	}

	// Token that expires in 3 minutes
	token.Expiry = time.Now().Add(3 * time.Minute)

	if token.IsExpired() {
		t.Error("Token should not be expired")
	}

	if !token.IsExpiredWithBuffer(5 * time.Minute) {
		t.Error("Token should be expired with 5min buffer")
	}

	// Expired token
	token.Expiry = time.Now().Add(-1 * time.Minute)

	if !token.IsExpired() {
		t.Error("Token should be expired")
	}
}

func TestTokenExpiresIn(t *testing.T) {
	token := &Token{
		Expiry: time.Now().Add(30 * time.Minute),
	}

	expiresIn := token.ExpiresIn()
	if expiresIn < 29*time.Minute || expiresIn > 31*time.Minute {
		t.Errorf("ExpiresIn() = %v, want ~30min", expiresIn)
	}
}
