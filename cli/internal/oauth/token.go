package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrTokenNotFound is returned when no token exists for the account
	ErrTokenNotFound = errors.New("no token found for this account")
	// ErrTokenExpired is returned when the token is expired and refresh failed
	ErrTokenExpired = errors.New("token expired, please re-authenticate")
	// ErrRefreshFailed is returned when token refresh fails
	ErrRefreshFailed = errors.New("failed to refresh token")
)

// Token represents an OAuth 2.0 token stored in keychain
type Token struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// TokenResponse is the response from the OAuth token endpoint
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Error        string `json:"error,omitempty"`
	ErrorDesc    string `json:"error_description,omitempty"`
}

// IsExpired returns true if the token is expired or will expire within the buffer time
func (t *Token) IsExpired() bool {
	return t.IsExpiredWithBuffer(0)
}

// IsExpiredWithBuffer returns true if the token will expire within the given duration
func (t *Token) IsExpiredWithBuffer(buffer time.Duration) bool {
	return time.Now().Add(buffer).After(t.Expiry)
}

// ExpiresIn returns the duration until the token expires
func (t *Token) ExpiresIn() time.Duration {
	return time.Until(t.Expiry)
}

// ExchangeCode exchanges an authorization code for tokens
// clientSecret is optional for Microsoft (PKCE only) but required for Google
func ExchangeCode(provider *Provider, clientID, clientSecret, redirectURI, code, codeVerifier string) (*Token, error) {
	clientID, clientSecret = provider.ResolveCredentials(clientID, clientSecret)

	data := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {codeVerifier},
		// Single-resource scope for the primary (IMAP/SMTP) access token. Azure
		// requires this when the consent spanned multiple resources (otherwise
		// AADSTS28003: scope cannot be empty); the refresh token still covers all
		// consented scopes, so GetGraphToken can mint a Graph token separately.
		// Google ignores this scope on the code grant.
		"scope": {strings.Join(provider.Scopes, " ")},
	}

	// Google requires client_secret even with PKCE
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	resp, err := http.PostForm(provider.TokenEndpoint, data)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.Error != "" {
		return nil, fmt.Errorf("token error: %s - %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	return &Token{
		Provider:     provider.Name,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}, nil
}

// RefreshAccessToken uses the refresh token to get a new access token for the
// provider's primary (IMAP/SMTP) resource.
// clientSecret is optional for Microsoft but required for Google
func RefreshAccessToken(provider *Provider, clientID, clientSecret string, token *Token) (*Token, error) {
	return refreshWithScopes(provider, clientID, clientSecret, token, provider.Scopes)
}

// refreshWithScopes redeems the refresh token for an access token scoped to the
// given scopes. Azure issues one token per resource, so callers pass a
// single-resource scope set (provider.Scopes for IMAP/SMTP, provider.GraphScopes
// for Graph); the refresh token is shared across resources.
func refreshWithScopes(provider *Provider, clientID, clientSecret string, token *Token, scopes []string) (*Token, error) {
	clientID, clientSecret = provider.ResolveCredentials(clientID, clientSecret)

	if token.RefreshToken == "" {
		return nil, errors.New("no refresh token available")
	}

	data := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {token.RefreshToken},
		"scope":         {strings.Join(scopes, " ")},
	}

	// Google requires client_secret for refresh
	if clientSecret != "" {
		data.Set("client_secret", clientSecret)
	}

	resp, err := http.PostForm(provider.TokenEndpoint, data)
	if err != nil {
		return nil, fmt.Errorf("refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	if tokenResp.Error != "" {
		// If refresh token is invalid, user needs to re-authenticate
		if tokenResp.Error == "invalid_grant" {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("refresh error: %s - %s", tokenResp.Error, tokenResp.ErrorDesc)
	}

	// Keep the old refresh token if a new one wasn't provided
	refreshToken := tokenResp.RefreshToken
	if refreshToken == "" {
		refreshToken = token.RefreshToken
	}

	return &Token{
		Provider:     provider.Name,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: refreshToken,
		Expiry:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}, nil
}

// GetValidToken loads a token and refreshes it if needed
// Returns a valid access token ready for use
// clientSecret is required for Google, optional for Microsoft
func GetValidToken(email, clientID, clientSecret, tenant string) (*Token, error) {
	token, err := LoadToken(email)
	if err != nil {
		return nil, err
	}

	// If token is still valid (with 5 minute buffer), return it
	if !token.IsExpiredWithBuffer(5 * time.Minute) {
		return token, nil
	}

	// Token expired or expiring soon, try to refresh
	provider, err := GetProvider(token.Provider, tenant)
	if err != nil {
		return nil, err
	}

	clientID, clientSecret = provider.ResolveCredentials(clientID, clientSecret)

	newToken, err := RefreshAccessToken(provider, clientID, clientSecret, token)
	if err != nil {
		// If refresh failed, delete the invalid token
		if errors.Is(err, ErrTokenExpired) {
			_ = DeleteToken(email)
		}
		return nil, err
	}

	// Save the refreshed token
	if err := SaveToken(email, newToken); err != nil {
		return nil, fmt.Errorf("failed to save refreshed token: %w", err)
	}

	return newToken, nil
}

// graphTokenBuffer is how long before expiry a cached Graph token is treated as
// stale, so a token never expires mid-request.
const graphTokenBuffer = 5 * time.Minute

// graphGate serialises Graph token acquisition for one auth identity and caches
// the result until it nears expiry.
type graphGate struct {
	mu    sync.Mutex
	token *Token
}

var (
	graphGatesMu sync.Mutex
	graphGates   = map[string]*graphGate{}
)

// graphGateFor returns the gate for an auth email, creating it on first use.
func graphGateFor(email string) *graphGate {
	graphGatesMu.Lock()
	defer graphGatesMu.Unlock()
	g, ok := graphGates[email]
	if !ok {
		g = &graphGate{}
		graphGates[email] = g
	}
	return g
}

// GetGraphToken mints a Microsoft Graph access token for the account from the
// stored refresh token, which must have been consented for the provider's
// GraphScopes (a one-time re-consent via `durian auth login`). Azure issues one
// token per resource, so this is separate from the IMAP/SMTP token: it redeems
// the same refresh token with the Graph scopes.
//
// Azure rotates refresh tokens on use, so the rotated refresh token is persisted
// back into the stored token while its IMAP/SMTP access token is left untouched
// — both resources keep working from one shared, always-current refresh token.
// The returned Graph token is NOT stored as the primary token; callers should
// cache it in memory until it expires.
//
// Calls are serialised and cached per auth email, because several accounts can
// legitimately share one identity — a user's own mailbox plus the shared
// mailboxes they have access to all authenticate as that user. Without this,
// syncing them concurrently would redeem the same refresh token in parallel;
// since Azure rotates the refresh token on every use, the racing redemptions
// invalidate each other's rotated token and fight over the single keychain
// item that holds it.
func GetGraphToken(email, clientID, clientSecret, tenant string) (*Token, error) {
	gate := graphGateFor(email)
	gate.mu.Lock()
	defer gate.mu.Unlock()

	// A cached token serves every account sharing this identity, so N accounts
	// cost one refresh per token lifetime instead of N per sync pass.
	if gate.token != nil && !gate.token.IsExpiredWithBuffer(graphTokenBuffer) {
		return gate.token, nil
	}

	stored, err := LoadToken(email)
	if err != nil {
		return nil, err
	}

	provider, err := GetProvider(stored.Provider, tenant)
	if err != nil {
		return nil, err
	}
	if len(provider.GraphScopes) == 0 {
		return nil, fmt.Errorf("provider %q has no Graph scopes", provider.Name)
	}

	graphToken, err := refreshWithScopes(provider, clientID, clientSecret, stored, provider.GraphScopes)
	if err != nil {
		// A missing Graph consent surfaces as invalid_grant/interaction_required;
		// surface the re-authenticate hint like the IMAP path does.
		return nil, err
	}

	// Persist the rotated refresh token so the IMAP/SMTP path keeps working;
	// keep the stored access token (IMAP resource) as-is.
	if graphToken.RefreshToken != "" && graphToken.RefreshToken != stored.RefreshToken {
		stored.RefreshToken = graphToken.RefreshToken
		if err := SaveToken(email, stored); err != nil {
			return nil, fmt.Errorf("failed to persist rotated refresh token: %w", err)
		}
	}

	gate.token = graphToken
	return graphToken, nil
}
