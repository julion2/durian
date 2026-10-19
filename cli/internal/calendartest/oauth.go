// Package calendartest contains opt-in live-test infrastructure. Bazel marks
// this library testonly; production binaries must never use environment OAuth.
package calendartest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Provider keeps credential namespaces separate.
type Provider string

const (
	Google    Provider = "GOOGLE"
	Microsoft Provider = "MS"
)

const microsoftTestScopes = "Calendars.ReadWrite offline_access openid email"
const microsoftMailTestScopes = "Calendars.ReadWrite Mail.ReadWrite Mail.Send offline_access openid email"

type TokenSource struct {
	mu                                   sync.Mutex
	clientID, clientSecret, refreshToken string
	provider                             Provider
	tenant, email                        string // email is a consent-verified demo identity, when available
	accessToken                          string
	expires                              time.Time
	client                               *http.Client
	persistRefresh                       func(context.Context, string) error
	persistenceFailed                    bool
	mailScopes                           bool
}

func noRedirect(_ *http.Request, _ []*http.Request) error {
	return fmt.Errorf("test adapter refuses redirects")
}

func NewTokenSource(provider Provider) (*TokenSource, error) {
	return newTokenSource(provider, false)
}

// NewMailTokenSource explicitly opts into mail + calendar scopes. The existing
// calendar constructor remains calendar-only. No keychain/config is consulted.
func NewMailTokenSource(provider Provider) (*TokenSource, error) {
	return newTokenSource(provider, true)
}

func newTokenSource(provider Provider, mailScopes bool) (*TokenSource, error) {
	optIn := "DURIAN_CALENDAR_LIVE_TEST"
	if mailScopes {
		optIn = "DURIAN_MAIL_LIVE_TEST"
	}
	if os.Getenv(optIn) != "1" {
		return nil, fmt.Errorf("live test requires explicit opt-in via %s", optIn)
	}
	if provider != Google && provider != Microsoft {
		return nil, fmt.Errorf("unsupported live calendar test provider")
	}
	prefix := "DURIAN_" + string(provider) + "_TEST_"
	s := &TokenSource{
		provider:     provider,
		mailScopes:   mailScopes,
		clientID:     os.Getenv(prefix + "CLIENT_ID"),
		refreshToken: os.Getenv(prefix + "REFRESH_TOKEN"),
		client:       &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect},
	}
	if provider == Google {
		s.clientSecret = os.Getenv(prefix + "CLIENT_SECRET")
	} else {
		s.tenant = os.Getenv(prefix + "TENANT")
		s.email = strings.TrimSpace(os.Getenv(prefix + "EMAIL"))
		s.persistRefresh = persistMicrosoftRefresh
		if s.tenant != "consumers" {
			return nil, fmt.Errorf("Microsoft calendar tests require the consumers tenant")
		}
	}
	if s.clientID == "" || s.refreshToken == "" || (provider == Google && s.clientSecret == "") {
		return nil, fmt.Errorf("missing %s test credentials", provider)
	}
	return s, nil
}

// persistMicrosoftRefresh never places credentials in argv, output or local
// config/keychain. Do not wrap the subprocess error: it may contain secrets.
func persistMicrosoftRefresh(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "amp", "secrets", "set", "DURIAN_MS_TEST_REFRESH_TOKEN", "--secret", "--data-file", "-", "--project", "julion/durian")
	cmd.Stdin = strings.NewReader(token)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second // also bound waiting on inherited subprocess pipes
	if cmd.Run() != nil {
		return fmt.Errorf("Microsoft test refresh token persistence failed (details withheld)")
	}
	return nil
}

// Token serializes refreshes, including persistence, so concurrent requests
// cannot rotate the same Microsoft token twice. Google stays memory-only;
// Microsoft must durably save a rotation before releasing any access token.
// A failed save latches this source closed: restarting with the saved secret
// (after resolving persistence) is safer than retrying an obsolete token.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistenceFailed {
		return "", fmt.Errorf("Microsoft test refresh persistence failed; token source disabled")
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("test token request canceled")
	}
	if s.accessToken != "" && time.Until(s.expires) > time.Minute {
		return s.accessToken, nil
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {s.clientID},
		"refresh_token": {s.refreshToken},
	}
	endpoint := "https://oauth2.googleapis.com/token"
	if s.provider == Microsoft {
		if s.tenant != "consumers" || s.persistRefresh == nil {
			return "", fmt.Errorf("invalid Microsoft test token source")
		}
		endpoint = "https://login.microsoftonline.com/consumers/oauth2/v2.0/token"
		form.Set("scope", microsoftTestScopes)
		if s.mailScopes {
			form.Set("scope", microsoftMailTestScopes)
		}
	} else {
		form.Set("client_secret", s.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("construct test token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("test token request failed (details withheld)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("test token refresh HTTP %d", resp.StatusCode)
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil || len(data) > 1<<20 || json.Unmarshal(data, &token) != nil {
		return "", fmt.Errorf("invalid test token response (details withheld)")
	}
	if s.provider == Microsoft && token.RefreshToken != "" {
		// Save every returned refresh token, even if it equals the previous
		// value. No access token is exposed when durability is uncertain.
		persistCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.persistRefresh(persistCtx, token.RefreshToken)
		cancel()
		if err != nil {
			s.persistenceFailed = true
			s.accessToken = ""
			return "", fmt.Errorf("Microsoft test refresh token persistence failed (details withheld)")
		}
	}
	if token.RefreshToken != "" {
		s.refreshToken = token.RefreshToken
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > 86400*365 {
		return "", fmt.Errorf("invalid test token response (details withheld)")
	}
	s.accessToken = token.AccessToken
	s.expires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

func (s *TokenSource) containsSecret(data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range []string{s.clientID, s.clientSecret, s.refreshToken, s.accessToken} {
		if value != "" && bytes.Contains(data, []byte(value)) {
			return true
		}
	}
	return false
}
