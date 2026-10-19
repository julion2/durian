package calendartest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMicrosoftTestOAuthPublicClientConfiguration(t *testing.T) {
	// Constructors perform no I/O. Do not depend on this orb's real secrets.
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "")
	t.Setenv("DURIAN_MS_TEST_CLIENT_ID", "fake-public-client")
	t.Setenv("DURIAN_MS_TEST_CLIENT_SECRET", "must-not-be-used")
	t.Setenv("DURIAN_MS_TEST_TENANT", "consumers")
	t.Setenv("DURIAN_MS_TEST_REFRESH_TOKEN", "fake-refresh")
	t.Setenv("DURIAN_MS_TEST_EMAIL", "")
	if _, err := NewTokenSource(Microsoft); err == nil {
		t.Fatal("Microsoft source enabled without live opt-in")
	}
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "1")
	s, err := NewTokenSource(Microsoft)
	if err != nil {
		t.Fatal(err)
	}
	if s.clientSecret != "" || s.email != "" || s.persistRefresh == nil || s.client.Timeout <= 0 || s.client.CheckRedirect == nil {
		t.Fatal("Microsoft source must be a bounded public client with optional email and durable rotation")
	}
	t.Setenv("DURIAN_MS_TEST_EMAIL", " demo@example.test ")
	s, err = NewTokenSource(Microsoft)
	if err != nil || s.email != "demo@example.test" {
		t.Fatal("consent-verified email not retained")
	}
	for _, tenant := range []string{"", "common", "organizations", "example/../../tenant"} {
		t.Setenv("DURIAN_MS_TEST_TENANT", tenant)
		if _, err := NewTokenSource(Microsoft); err == nil {
			t.Fatal("non-consumer tenant accepted")
		}
	}
	t.Setenv("DURIAN_MS_TEST_TENANT", "consumers")
	for _, name := range []string{"DURIAN_MS_TEST_CLIENT_ID", "DURIAN_MS_TEST_REFRESH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "")
			if _, err := NewTokenSource(Microsoft); err == nil {
				t.Fatal("missing test credentials accepted")
			}
		})
	}
}

func microsoftMockSource(transport http.RoundTripper, persist func(context.Context, string) error) *TokenSource {
	// Never use the real persistence CLI or network in these tests.
	return &TokenSource{provider: Microsoft, tenant: "consumers", clientID: "fake-public-client", refreshToken: "original",
		client: &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: noRedirect}, persistRefresh: persist}
}

func TestMicrosoftTestOAuthSerialRefreshAndDurableRotation(t *testing.T) {
	calls, saves := 0, 0
	var s *TokenSource
	s = microsoftMockSource(graphTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.String() != "https://login.microsoftonline.com/consumers/oauth2/v2.0/token" {
			t.Error("wrong Microsoft token endpoint")
		}
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		wantRefresh := "original"
		if calls > 1 {
			wantRefresh = "rotated"
		}
		if len(r.Form) != 4 || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "fake-public-client" ||
			r.Form.Get("refresh_token") != wantRefresh || r.Form.Get("scope") != "Calendars.ReadWrite offline_access openid email" {
			t.Error("refresh must use exact calendar scopes, last rotation, and no client secret")
		}
		return graphTestResponse(r, 200, `{"access_token":"access","refresh_token":"rotated","expires_in":3600}`), nil
	}), func(ctx context.Context, token string) error {
		saves++
		deadline, ok := ctx.Deadline()
		if token != "rotated" || !ok || time.Until(deadline) > 15*time.Second || s.accessToken == "access" {
			t.Error("rotation must be persisted with bounded context before releasing access token")
		}
		return nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if token, err := s.Token(context.Background()); err != nil || token != "access" {
				t.Error("concurrent token request failed")
			}
		}()
	}
	wg.Wait()
	if calls != 1 || saves != 1 {
		t.Fatal("concurrent requests performed multiple refreshes or missed persistence")
	}
	s.accessToken, s.expires = "", time.Now()
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || saves != 2 {
		t.Fatal("every returned refresh token must be persisted, even if unchanged")
	}
}

func TestMicrosoftTestOAuthPersistenceFailureDisablesSource(t *testing.T) {
	calls, saves := 0, 0
	s := microsoftMockSource(graphTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return graphTestResponse(r, 200, `{"access_token":"sensitive-access","refresh_token":"sensitive-rotation","expires_in":3600}`), nil
	}), func(context.Context, string) error {
		saves++
		return fmt.Errorf("sensitive-rotation provider body must not escape")
	})
	for i := 0; i < 2; i++ {
		token, err := s.Token(context.Background())
		if err == nil || token != "" || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "provider body") {
			t.Fatal("persistence failure released credentials or private error details")
		}
	}
	if calls != 1 || saves != 1 || s.accessToken != "" {
		t.Fatal("failed persistence must stop refreshes and cached-token use")
	}
}

func TestMicrosoftTestOAuthInvalidResponsesAndMissingRotation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		allowed    bool
		saves      int
	}{
		{"no-rotation", `{"access_token":"access","expires_in":3600}`, 200, true, 0},
		{"provider-error", `{"error_description":"sensitive provider body"}`, 400, false, 0},
		{"malformed", `{"access_token":"sensitive`, 200, false, 0},
		{"oversized", strings.Repeat("x", (1<<20)+1), 200, false, 0},
		{"bad-expiry", `{"access_token":"sensitive","expires_in":0}`, 200, false, 0},
		{"save-rotation-even-without-access", `{"refresh_token":"rotated","expires_in":3600}`, 200, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saves := 0
			s := microsoftMockSource(graphTestTransport(func(r *http.Request) (*http.Response, error) {
				return graphTestResponse(r, tc.status, tc.body), nil
			}), func(context.Context, string) error { saves++; return nil })
			token, err := s.Token(context.Background())
			if (err == nil) != tc.allowed || (err != nil && (token != "" || strings.Contains(err.Error(), "sensitive"))) || saves != tc.saves {
				t.Fatal("unexpected response acceptance, persistence or sensitive error")
			}
			if tc.name == "no-rotation" && s.refreshToken != "original" {
				t.Fatal("missing refresh token erased existing credential")
			}
		})
	}
}

func TestMicrosoftTestOAuthRefusesRedirectsAndCancellation(t *testing.T) {
	calls := 0
	s := microsoftMockSource(graphTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		resp := graphTestResponse(r, 307, "sensitive provider body")
		resp.Header.Set("Location", "https://example.test/stolen")
		return resp, nil
	}), func(context.Context, string) error { t.Fatal("unexpected persistence"); return nil })
	if token, err := s.Token(context.Background()); err == nil || token != "" || calls != 1 || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("OAuth redirect accepted, followed or disclosed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Token(ctx); err == nil || calls != 1 {
		t.Fatal("canceled refresh reached transport")
	}
	s.client.Transport = graphTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("sensitive: %w", io.ErrUnexpectedEOF)
	})
	if token, err := s.Token(context.Background()); err == nil || token != "" || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("transport error disclosed credentials")
	}
}

func TestMailOAuthOptInAndScopeIsolation(t *testing.T) {
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "1")
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "")
	if _, err := NewMailTokenSource(Microsoft); err == nil {
		t.Fatal("calendar opt-in must not authorize mail scopes")
	}
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "1")
	t.Setenv("DURIAN_MS_TEST_CLIENT_ID", "fake-public-client")
	t.Setenv("DURIAN_MS_TEST_REFRESH_TOKEN", "fake-refresh")
	t.Setenv("DURIAN_MS_TEST_TENANT", microsoftMockSource(nil, nil).tenant)
	mailSource, err := NewMailTokenSource(Microsoft)
	if err != nil || !mailSource.mailScopes {
		t.Fatal("explicit mail constructor did not retain scoped opt-in")
	}
	calendarSource, err := NewTokenSource(Microsoft)
	if err != nil || calendarSource.mailScopes {
		t.Fatal("calendar constructor gained mail scopes")
	}
	for _, source := range []*TokenSource{mailSource, calendarSource} {
		calls, saves := 0, 0
		source.client.Transport = graphTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			want := "Calendars.ReadWrite offline_access openid email"
			if source.mailScopes {
				want = "Calendars.ReadWrite Mail.ReadWrite Mail.Send offline_access openid email"
			}
			if r.Form.Get("scope") != want || r.Form.Get("client_secret") != "" {
				t.Error("wrong explicit scope set or public-client authentication")
			}
			return graphTestResponse(r, 200, `{"access_token":"fake-access","refresh_token":"fake-rotation","expires_in":3600}`), nil
		})
		source.persistRefresh = func(context.Context, string) error { saves++; return nil }
		if _, err := source.Token(context.Background()); err != nil || calls != 1 || saves != 1 {
			t.Fatal("scoped source did not preserve durable rotation")
		}
	}
	t.Setenv("DURIAN_CALENDAR_LIVE_TEST", "")
	if _, err := NewTokenSource(Microsoft); err == nil {
		t.Fatal("mail opt-in must not silently enable calendar constructor")
	}
}
