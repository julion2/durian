package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// Provider represents an OAuth 2.0 provider configuration
type Provider struct {
	Name              string
	AuthorizeEndpoint string
	TokenEndpoint     string
	Scopes            []string
	SASlMethod        string // XOAUTH2 or OAUTHBEARER

	// Default credentials embedded in the binary.
	// Users can override these in config.pkl.
	DefaultClientID     string
	DefaultClientSecret string
}

// ResolveCredentials returns the effective client ID and secret.
// Config values take priority; defaults are used as fallback.
func (p *Provider) ResolveCredentials(clientID, clientSecret string) (string, string) {
	if clientID == "" {
		clientID = p.DefaultClientID
	}
	if clientSecret == "" {
		clientSecret = p.DefaultClientSecret
	}
	return clientID, clientSecret
}

// Microsoft OAuth provider for Office 365
// Tenant "common" supports both personal and work/school accounts
func Microsoft(tenant string) *Provider {
	if tenant == "" {
		tenant = "common"
	}
	return &Provider{
		Name:              "microsoft",
		AuthorizeEndpoint: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", tenant),
		TokenEndpoint:     fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenant),
		Scopes: []string{
			"offline_access",
			"https://outlook.office.com/SMTP.Send",
			"https://outlook.office.com/IMAP.AccessAsUser.All",
		},
		SASlMethod:      "XOAUTH2",
		DefaultClientID: "d1969673-bd8a-4bf6-ad8f-f541879730a8",
	}
}

// Google OAuth provider for Gmail
func Google() *Provider {
	return &Provider{
		Name:              "google",
		AuthorizeEndpoint: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenEndpoint:     "https://oauth2.googleapis.com/token",
		Scopes: []string{
			"https://mail.google.com/",
		},
		SASlMethod: "OAUTHBEARER",
		// No default credentials — users must configure their own Google OAuth app.
		// See https://julion2.github.io/durian/docs/auth/oauth/ for instructions.
	}
}

// GetProvider returns the provider configuration by name
func GetProvider(name, tenant string) (*Provider, error) {
	switch strings.ToLower(name) {
	case "microsoft":
		return Microsoft(tenant), nil
	case "google":
		return Google(), nil
	default:
		return nil, fmt.Errorf("unknown OAuth provider: %s", name)
	}
}

// AuthorizationURL builds the OAuth authorization URL with PKCE
func (p *Provider) AuthorizationURL(clientID, redirectURI, state string, pkce *PKCE) string {
	params := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(p.Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {pkce.Method},
	}

	// Google requires access_type=offline to return a refresh token
	// prompt=consent ensures we always get a refresh token (not just on first login)
	if p.Name == "google" {
		params.Set("access_type", "offline")
		params.Set("prompt", "consent")
	}

	return p.AuthorizeEndpoint + "?" + params.Encode()
}

// BuildXOAuth2String builds the SASL XOAUTH2 authentication string
// Format: user=<email>\x01auth=Bearer <token>\x01\x01
func BuildXOAuth2String(email, accessToken string) string {
	return fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", email, accessToken)
}
