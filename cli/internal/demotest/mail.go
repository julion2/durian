// Package demotest is a test-only composition root for two-party mail demos.
// It never enables a production CLI environment bypass. Keep both dedicated
// mailboxes quiescent: draft recipient revalidation cannot be atomic with send
// at either provider, so another client must not edit drafts during a demo.
// Submitted recipients are guarded, not server-side forwarding/routing rules;
// the dedicated mailboxes must not automatically forward to third parties.
package demotest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/calendartest"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/gmailbackend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/mailsend"
)

type TokenFunc func(context.Context) (string, error)

// Options takes trusted private identity references, not user-submitted compose
// addresses. Live callers should use NewFromEnv; explicit inputs permit fully
// offline tests. Transport must be trusted (nil uses http.DefaultTransport).
type Options struct {
	LiveOptIn                   bool
	GoogleEmail, MicrosoftEmail string
	GoogleToken, MicrosoftToken TokenFunc
	Transport                   http.RoundTripper
}

// Mail owns guards shared by every backend/sender it constructs. Draft ownership
// is memory-only and never restored from arbitrary mailbox IDs or local config.
type Mail struct {
	google, microsoft *mailGuard
}

// NewFromEnv requires separate mail opt-in, reuses calendartest's memory-only
// Google and durable-rotation Microsoft token sources, then verifies BOTH
// authenticated userinfo identities against the exact private references.
// A combined calendar/mail root should instead construct shared token sources
// once and pass their Token methods to New, avoiding concurrent MS rotations.
func NewFromEnv(ctx context.Context) (*Mail, error) {
	if os.Getenv("DURIAN_MAIL_LIVE_TEST") != "1" {
		return nil, fmt.Errorf("mail demo requires explicit live opt-in")
	}
	google, err := calendartest.NewMailTokenSource(calendartest.Google)
	if err != nil {
		return nil, err
	}
	microsoft, err := calendartest.NewMailTokenSource(calendartest.Microsoft)
	if err != nil {
		return nil, err
	}
	return New(ctx, Options{LiveOptIn: true,
		GoogleEmail: os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), MicrosoftEmail: os.Getenv("DURIAN_MS_TEST_EMAIL"),
		GoogleToken: google.Token, MicrosoftToken: microsoft.Token})
}

// New performs bounded authenticated identity reads only. No mail operation is
// available until both identities match exactly (no aliases, suffixes or case
// folding). Microsoft userinfo is authenticated at Graph's OIDC endpoint.
func New(ctx context.Context, o Options) (*Mail, error) {
	if !o.LiveOptIn || !bareAddress(o.GoogleEmail) || !bareAddress(o.MicrosoftEmail) ||
		o.GoogleEmail == o.MicrosoftEmail || o.GoogleToken == nil || o.MicrosoftToken == nil {
		return nil, fmt.Errorf("mail demo requires opt-in, two exact identities and explicit token sources")
	}
	transport := o.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := verifyIdentity(ctx, transport, o.GoogleToken, "https://openidconnect.googleapis.com/v1/userinfo", o.GoogleEmail, true); err != nil {
		return nil, err
	}
	if err := verifyIdentity(ctx, transport, o.MicrosoftToken, "https://graph.microsoft.com/oidc/userinfo", o.MicrosoftEmail, false); err != nil {
		return nil, err
	}
	allowed := map[string]bool{o.GoogleEmail: true, o.MicrosoftEmail: true}
	return &Mail{
		google:    newGuard(true, allowed, o.GoogleEmail, o.GoogleToken, transport),
		microsoft: newGuard(false, allowed, o.MicrosoftEmail, o.MicrosoftToken, transport),
	}, nil
}

func verifyIdentity(ctx context.Context, transport http.RoundTripper, tokenFn TokenFunc, endpoint, expected string, google bool) error {
	token, err := tokenFn(ctx)
	if err != nil || token == "" {
		return fmt.Errorf("mail demo identity token unavailable (details withheld)")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mail demo identity read failed (details withheld)")
	}
	defer resp.Body.Close()
	var identity struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Subject  string `json:"sub"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadata+1))
	_, validObject := object(data)
	if err != nil || len(data) > maxMetadata || resp.StatusCode != http.StatusOK ||
		!validObject || json.Unmarshal(data, &identity) != nil || identity.Subject == "" || identity.Email != expected || (google && !identity.Verified) {
		return fmt.Errorf("mail demo authenticated identity mismatch or invalid userinfo (details withheld)")
	}
	return nil
}

func noRedirect(_ *http.Request, _ []*http.Request) error {
	return fmt.Errorf("mail demo refuses redirects")
}

func (m *Mail) guard(account *config.AccountConfig) (*mailGuard, error) {
	if m == nil || account == nil || account.OAuth == nil || account.IsDelegatedMailbox() {
		return nil, fmt.Errorf("mail demo refuses unverified account or delegated mailbox")
	}
	if account.UsesGmailBackend() && account.OAuth.Provider == "google" && m.google != nil && account.Email == m.google.owner {
		return m.google, nil
	}
	if account.UsesGraphBackend() && account.OAuth.Provider == "microsoft" && m.microsoft != nil && account.Email == m.microsoft.owner {
		return m.microsoft, nil
	}
	return nil, fmt.Errorf("mail demo allows only verified Gmail/Graph accounts; no SMTP/IMAP/JMAP routes")
}

// HTTPClient exposes the same fail-closed boundary used by the factories, for
// extra test-only provider operations. It cannot access other mailboxes/hosts.
func (m *Mail) HTTPClient(account *config.AccountConfig) (*http.Client, error) {
	g, err := m.guard(account)
	if err != nil {
		return nil, err
	}
	return g.client(), nil
}

// BackendFor plugs into Handler.SetBackendFactory and engine construction.
func (m *Mail) BackendFor(account *config.AccountConfig) (backend.Backend, error) {
	g, err := m.guard(account)
	if err != nil {
		return nil, err
	}
	if g.google {
		return gmailbackend.NewWithToken(account, g.token, g.client())
	}
	return graphbackend.NewWithToken(account, g.token, g.client())
}

// SenderFor plugs into OutboxWorker.SetSenderFactory. Each sender shares its
// guarded backend; it never constructs a fresh default credential/client path.
func (m *Mail) SenderFor(account *config.AccountConfig) (mailsend.Sender, error) {
	b, err := m.BackendFor(account)
	if err != nil {
		return nil, err
	}
	g, _ := m.guard(account)
	var s mailsend.DurableSender
	if gmail, ok := b.(*gmailbackend.Backend); ok {
		s, err = gmailbackend.NewSenderWithBackend(gmail)
	} else {
		s, err = graphbackend.NewSenderWithBackend(b.(*graphbackend.Backend))
	}
	if err != nil {
		return nil, err
	}
	return &safeSender{sender: s, guard: g}, nil
}

type safeSender struct {
	sender mailsend.DurableSender
	guard  *mailGuard
}

func (s *safeSender) SavesSentCopy() bool { return s.sender.SavesSentCopy() }
func (s *safeSender) Send(ctx context.Context, msg *mailsend.Message) error {
	return s.SendAfterPersist(ctx, msg, func(string) error { return nil })
}

func (s *safeSender) SendAfterPersist(ctx context.Context, msg *mailsend.Message, persist func(string) error) error {
	if msg == nil || persist == nil || !s.guard.address(msg.From) || !s.guard.envelope(msg.To, msg.CC, msg.BCC) ||
		(len(msg.RawMIME) > 0 && !s.guard.mime(msg.RawMIME)) {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("mail demo blocked unsafe recipients or MIME")}
	}
	if !s.guard.google {
		if msg.InReplyTo != "" && len(msg.RawMIME) == 0 {
			return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("mail demo does not support Graph inferred-recipient reply drafts")}
		}
		for _, a := range msg.Attachments {
			if len(a.Data) >= 3<<20 {
				return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("mail demo does not support Graph large attachment upload sessions")}
			}
		}
	}
	return s.sender.SendAfterPersist(ctx, msg, persist)
}
