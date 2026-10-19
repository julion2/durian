package demotest_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/demotest"
	"github.com/julion2/durian/cli/internal/mailsend"
)

const googleEmail = "google@example.test"
const msEmail = "microsoft@example.test"
const stranger = "third-party@example.test"
const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me/"
const graphBase = "https://graph.microsoft.com/v1.0/me/"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(r *http.Request, status int, body string) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func account(google bool) *config.AccountConfig {
	provider, email := "microsoft", msEmail
	if google {
		provider, email = "google", googleEmail
	}
	return &config.AccountConfig{Email: email, OAuth: &config.OAuthConfig{Provider: provider}}
}

func options(transport http.RoundTripper) demotest.Options {
	return demotest.Options{LiveOptIn: true, GoogleEmail: googleEmail, MicrosoftEmail: msEmail,
		GoogleToken:    func(context.Context) (string, error) { return "fake-google", nil },
		MicrosoftToken: func(context.Context) (string, error) { return "fake-ms", nil }, Transport: transport}
}

// fakeMail observes the real provider senders through the public factories.
// No token refresh, persistence subprocess, keychain or actual network exists.
type fakeMail struct {
	t                                               *testing.T
	calls, identities, writes, deliveries, rechecks int
	raw                                             string
	draft                                           map[string]any
	currentOverride                                 string
	identityOverride                                map[string]string
	finalStatus                                     int
}

func (f *fakeMail) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	if strings.Contains(r.URL.Path, "userinfo") {
		f.identities++
		email, token := googleEmail, "fake-google"
		if r.URL.Host == "graph.microsoft.com" {
			email, token = msEmail, "fake-ms"
		}
		if r.Header.Get("Authorization") != "Bearer "+token || r.Method != "GET" {
			f.t.Error("identity read lacks correct authentication")
		}
		body := fmt.Sprintf(`{"email":%q,"email_verified":true,"sub":"fake-subject"}`, email)
		if s, ok := f.identityOverride[email]; ok {
			body = s
		}
		return response(r, 200, body), nil
	}
	if f.identities != 2 {
		f.t.Fatal("mail operation before BOTH identities verified")
	}
	if r.Method != "GET" {
		f.writes++
	}
	if r.Method == "POST" && r.URL.String() == gmailBase+"drafts" {
		var data struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		if json.NewDecoder(r.Body).Decode(&data) != nil {
			f.t.Fatal("invalid draft body")
		}
		f.raw = data.Message.Raw
		return response(r, 200, `{"id":"d1","message":{"id":"m1"}}`), nil
	}
	if r.Method == "GET" && r.URL.Path == "/gmail/v1/users/me/drafts/d1" {
		f.rechecks++
		if r.URL.Query().Get("format") != "raw" {
			f.t.Error("Gmail must re-read current raw MIME")
		}
		body := fmt.Sprintf(`{"id":"d1","message":{"raw":%q}}`, f.raw)
		if f.currentOverride != "" {
			body = f.currentOverride
		}
		return response(r, 200, body), nil
	}
	if r.Method == "GET" && r.URL.Path == "/gmail/v1/users/me/messages/m1" {
		return response(r, 200, `{"payload":{"headers":[{"name":"Message-Id","value":"<gmail@example.test>"}]}}`), nil
	}
	if r.Method == "POST" && r.URL.String() == graphBase+"messages" {
		if json.NewDecoder(r.Body).Decode(&f.draft) != nil {
			f.t.Fatal("invalid typed Graph draft")
		}
		return response(r, 201, `{"id":"g1","internetMessageId":"<graph@example.test>"}`), nil
	}
	if r.Method == "GET" && r.URL.Path == "/v1.0/me/messages/g1" {
		f.rechecks++
		if r.URL.Query().Get("$select") != "id,isDraft,toRecipients,ccRecipients,bccRecipients" {
			f.t.Error("Graph revalidation must include every recipient class")
		}
		current := map[string]any{"id": "g1", "isDraft": true, "toRecipients": []any{}, "ccRecipients": []any{}, "bccRecipients": []any{}}
		for _, key := range []string{"toRecipients", "ccRecipients", "bccRecipients"} {
			if value, ok := f.draft[key]; ok {
				current[key] = value
			}
		}
		data, _ := json.Marshal(current)
		body := string(data)
		if f.currentOverride != "" {
			body = f.currentOverride
		}
		return response(r, 200, body), nil
	}
	if r.Method == "POST" && (r.URL.String() == gmailBase+"drafts/send" || r.URL.String() == graphBase+"messages/g1/send" ||
		r.URL.String() == gmailBase+"messages/send" || r.URL.String() == graphBase+"sendMail") {
		f.deliveries++
		status := f.finalStatus
		if status == 0 {
			status = 200
		}
		return response(r, status, `{}`), nil
	}
	if r.Method == "GET" {
		return response(r, 200, `{}`), nil
	}
	f.t.Error("unexpected operation reached fake transport")
	return response(r, 400, `{}`), nil
}

func setup(t *testing.T) (*demotest.Mail, *fakeMail) {
	t.Helper()
	fake := &fakeMail{t: t}
	m, err := demotest.New(context.Background(), options(fake))
	if err != nil {
		t.Fatal(err)
	}
	return m, fake
}

func normalMessage(google bool) *mailsend.Message {
	from, to := msEmail, googleEmail
	if google {
		from, to = googleEmail, msEmail
	}
	return &mailsend.Message{From: from, To: []string{to}, MessageID: "<test@example.test>", Subject: "Durian Test", Body: "body"}
}

func rawMIME(to, cc, bcc string) []byte {
	headers := "From: " + googleEmail + "\r\nTo: " + to + "\r\n"
	if cc != "" {
		headers += "Cc: " + cc + "\r\n"
	}
	if bcc != "" {
		headers += "Bcc: " + bcc + "\r\n"
	}
	return []byte(headers + "Subject: Test\r\n\r\nbody")
}

func TestIdentityAndOptInFailClosed(t *testing.T) {
	for _, which := range []string{"no-opt-in", "bad-google", "bad-microsoft", "google-unverified", "missing-sub", "oversized"} {
		t.Run(which, func(t *testing.T) {
			f := &fakeMail{t: t, identityOverride: map[string]string{}}
			o := options(f)
			switch which {
			case "no-opt-in":
				o.LiveOptIn = false
			case "bad-google":
				f.identityOverride[googleEmail] = `{"email":"wrong@example.test","email_verified":true,"sub":"x"}`
			case "bad-microsoft":
				f.identityOverride[msEmail] = `{"email":"wrong@example.test","sub":"x"}`
			case "google-unverified":
				f.identityOverride[googleEmail] = `{"email":"google@example.test","email_verified":false,"sub":"x"}`
			case "missing-sub":
				f.identityOverride[msEmail] = `{"email":"microsoft@example.test"}`
			case "oversized":
				f.identityOverride[msEmail] = strings.Repeat("x", (1<<20)+1)
			}
			m, err := demotest.New(context.Background(), o)
			if err == nil || m != nil || f.writes != 0 || (which == "no-opt-in" && f.calls != 0) {
				t.Fatal("unverified demo enabled or performed writes")
			}
		})
	}
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "")
	if _, err := demotest.NewFromEnv(context.Background()); err == nil {
		t.Fatal("environment path lacks independent mail opt-in")
	}
}

func TestSenderRejectsThirdPartyBeforeTransport(t *testing.T) {
	for _, google := range []bool{true, false} {
		for _, role := range []string{"To", "CC", "BCC", "RawTo", "RawCC", "RawBCC", "alias", "case", "injection"} {
			t.Run(fmt.Sprintf("google=%v/%s", google, role), func(t *testing.T) {
				m, f := setup(t)
				s, err := m.SenderFor(account(google))
				if err != nil {
					t.Fatal(err)
				}
				msg := normalMessage(google)
				switch role {
				case "To":
					msg.To = []string{stranger}
				case "CC":
					msg.CC = []string{stranger}
				case "BCC":
					msg.BCC = []string{stranger}
				case "RawTo":
					msg.RawMIME = rawMIME(stranger, "", "")
				case "RawCC":
					msg.RawMIME = rawMIME(msEmail, stranger, "")
				case "RawBCC":
					msg.RawMIME = rawMIME(msEmail, "", stranger)
				case "alias":
					msg.To = []string{"google+alias@example.test"}
				case "case":
					msg.To = []string{"GOOGLE@example.test"}
				case "injection":
					msg.BCC = []string{msEmail + "\r\nBcc: " + stranger}
				}
				before := f.calls
				persisted := false
				err = s.(mailsend.DurableSender).SendAfterPersist(context.Background(), msg, func(string) error { persisted = true; return nil })
				if err == nil || mailsend.Classify(err) != mailsend.KindPermanent || f.calls != before || persisted {
					t.Fatal("unsafe sender input reached transport or persistence")
				}
			})
		}
	}
}

func TestAllowedTwoPartyDeliveryAndCanonicalReaction(t *testing.T) {
	for _, google := range []bool{true, false} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprintf("google=%v/raw=%v", google, raw), func(t *testing.T) {
				m, f := setup(t)
				s, err := m.SenderFor(account(google))
				if err != nil {
					t.Fatal(err)
				}
				msg := normalMessage(google)
				if raw {
					msg.Body, msg.InReplyTo = "👍", "<target@example.test>"
					msg.RawMIME, err = mailsend.BuildReaction(msg, time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
					if err != nil {
						t.Fatal(err)
					}
				} else {
					msg.CC, msg.BCC = []string{"Demo <" + googleEmail + ">"}, []string{msEmail}
					msg.Attachments = []mailsend.Attachment{{Filename: "tiny.txt", Data: []byte("small"), MIMEType: "text/plain"}}
				}
				persisted := 0
				err = s.(mailsend.DurableSender).SendAfterPersist(context.Background(), msg, func(id string) error {
					if id == "" || f.deliveries != 0 {
						t.Error("must persist nonempty Message-ID before delivery")
					}
					persisted++
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				checks := 1
				if !google && raw {
					checks = 0
				}
				if f.deliveries != 1 || f.rechecks != checks || persisted != 1 || !s.SavesSentCopy() {
					t.Fatal("allowed delivery did not preserve guarded durable sender protocol")
				}
			})
		}
	}
}

func request(t *testing.T, c *http.Client, method, endpoint, contentType, body string) error {
	t.Helper()
	r, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", contentType)
	resp, err := c.Do(r)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

func TestHTTPBoundaryRejectsRecipientsWithoutSender(t *testing.T) {
	for _, role := range []string{"To", "CC", "BCC"} {
		t.Run(role, func(t *testing.T) {
			m, f := setup(t)
			gmail, _ := m.HTTPClient(account(true))
			graph, _ := m.HTTPClient(account(false))
			to, cc, bcc := msEmail, "", ""
			switch role {
			case "To":
				to = stranger
			case "CC":
				cc = stranger
			case "BCC":
				bcc = stranger
			}
			raw := rawMIME(to, cc, bcc)
			encoded := base64.URLEncoding.EncodeToString(raw)
			typed := `{"toRecipients":[{"emailAddress":{"address":"microsoft@example.test"}}]}`
			key := map[string]string{"To": "toRecipients", "CC": "ccRecipients", "BCC": "bccRecipients"}[role]
			if role == "To" {
				typed = `{"toRecipients":[{"emailAddress":{"address":"third-party@example.test"}}]}`
			} else {
				typed = strings.TrimSuffix(typed, "}") + fmt.Sprintf(`,%q:[{"emailAddress":{"address":%q}}]}`, key, stranger)
			}
			cases := []struct {
				client                      *http.Client
				endpoint, contentType, body string
			}{
				{gmail, gmailBase + "drafts", "application/json", fmt.Sprintf(`{"message":{"raw":%q}}`, encoded)},
				{gmail, gmailBase + "messages/send", "application/json", fmt.Sprintf(`{"raw":%q}`, encoded)},
				{graph, graphBase + "messages", "application/json", typed},
				{graph, graphBase + "sendMail", "text/plain", base64.StdEncoding.EncodeToString(raw)},
			}
			for _, tc := range cases {
				before := f.calls
				if err := request(t, tc.client, "POST", tc.endpoint, tc.contentType, tc.body); err == nil || f.calls != before {
					t.Fatal("HTTP bypass delivered unchecked recipients")
				}
			}
		})
	}
}

func TestDraftRecipientsRevalidatedImmediatelyBeforeSend(t *testing.T) {
	for _, google := range []bool{true, false} {
		for _, role := range []string{"To", "CC", "BCC", "missing", "oversized"} {
			t.Run(fmt.Sprintf("google=%v/%s", google, role), func(t *testing.T) {
				m, f := setup(t)
				s, _ := m.SenderFor(account(google))
				err := s.(mailsend.DurableSender).SendAfterPersist(context.Background(), normalMessage(google), func(string) error {
					if role == "missing" {
						f.currentOverride = `{}`
						return nil
					}
					if role == "oversized" {
						f.currentOverride = strings.Repeat("x", (12<<20)+1)
						return nil
					}
					if google {
						to, cc, bcc := msEmail, "", ""
						switch role {
						case "To":
							to = stranger
						case "CC":
							cc = stranger
						case "BCC":
							bcc = stranger
						}
						f.raw = base64.URLEncoding.EncodeToString(rawMIME(to, cc, bcc))
					} else {
						key := map[string]string{"To": "toRecipients", "CC": "ccRecipients", "BCC": "bccRecipients"}[role]
						f.draft[key] = []any{map[string]any{"emailAddress": map[string]string{"address": stranger}}}
					}
					return nil
				})
				if err == nil || f.rechecks != 1 || f.deliveries != 0 {
					t.Fatal("changed/invalid current draft was sent")
				}
			})
		}
	}
}

func TestUnownedDraftsAndUnknownOperationsNeverReachTransport(t *testing.T) {
	m, f := setup(t)
	for _, google := range []bool{true, false} {
		c, _ := m.HTTPClient(account(google))
		base := graphBase
		if google {
			base = gmailBase
		}
		paths := []string{"messages/x/send", "messages/x/move", "messages/x/forward", "messages/x/reply", "messages/x/createReply", "messages/x/attachments/createUploadSession", "unknown", "messages?unsafe=1"}
		if google {
			paths = append(paths, "drafts/send")
		}
		for _, path := range paths {
			before := f.calls
			if err := request(t, c, "POST", base+path, "application/json", `{"id":"unowned"}`); err == nil || f.calls != before {
				t.Fatalf("unknown/unowned operation reached transport: %s", path)
			}
		}
		for _, endpoint := range []string{"https://evil.example.test/upload", "https://graph.microsoft.com/v1.0/users/other/messages", base + "messages/x%2fsend", base + "../messages"} {
			before := f.calls
			if err := request(t, c, "GET", endpoint, "", ""); err == nil || f.calls != before {
				t.Fatal("unsafe GET reached transport")
			}
		}
		if err := request(t, c, "GET", base+"messages", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, engine := range []string{"engine", "legacy", "jmap"} {
		a := account(true)
		a.SyncEngine = engine
		if _, err := m.SenderFor(a); err == nil {
			t.Fatal("non-REST send route allowed")
		}
		if _, err := m.BackendFor(a); err == nil {
			t.Fatal("non-REST backend route allowed")
		}
	}
}

func TestAmbiguousDraftSendCannotReplayAndPersistFailureCannotSend(t *testing.T) {
	for _, google := range []bool{true, false} {
		m, f := setup(t)
		s, _ := m.SenderFor(account(google))
		if err := s.(mailsend.DurableSender).SendAfterPersist(context.Background(), normalMessage(google), func(string) error { return fmt.Errorf("disk failure") }); err == nil || f.deliveries != 0 {
			t.Fatal("failed persistence sent mail")
		}
		// A newly created draft can send only once, even if the provider's
		// final result is ambiguous. Use a new guard to avoid duplicate fake ID.
		m, f = setup(t)
		f.finalStatus = 500
		s, _ = m.SenderFor(account(google))
		err := s.Send(context.Background(), normalMessage(google))
		if err == nil || mailsend.Classify(err) != mailsend.KindAmbiguous || f.deliveries != 1 {
			t.Fatal("uncertain final send was not classified ambiguous")
		}
		c, _ := m.HTTPClient(account(google))
		endpoint, body := graphBase+"messages/g1/send", ""
		if google {
			endpoint, body = gmailBase+"drafts/send", `{"id":"d1"}`
		}
		before := f.calls
		if err := request(t, c, "POST", endpoint, "application/json", body); err == nil || f.calls != before {
			t.Fatal("consumed draft capability replayed")
		}
	}
}

func TestGraphExplicitLimitationsRejectedBeforeDraftCreation(t *testing.T) {
	for _, large := range []bool{false, true} {
		m, f := setup(t)
		s, _ := m.SenderFor(account(false))
		msg := normalMessage(false)
		if large {
			msg.Attachments = []mailsend.Attachment{{Data: make([]byte, 3<<20)}}
		} else {
			msg.InReplyTo = "<original@example.test>"
		}
		before := f.calls
		if err := s.Send(context.Background(), msg); err == nil || f.calls != before {
			t.Fatal("unsupported Graph operation created a draft")
		}
	}
}

func TestHTTPBoundaryRejectsAmbiguousShapesAndRedirects(t *testing.T) {
	m, fake := setup(t)
	gmail, _ := m.HTTPClient(account(true))
	graph, _ := m.HTTPClient(account(false))
	safe := `[{"emailAddress":{"address":"microsoft@example.test"}}]`
	for _, body := range []string{
		`{"toRecipients":` + safe + `,"bccRecipients":[],"BccRecipients":[{"emailAddress":{"address":"third-party@example.test"}}]}`,
		`{"toRecipients":` + safe + `,"bccRecipients@odata.bind":["third-party@example.test"]}`,
		`{"toRecipients":` + safe + `,"attachments":[{"@odata.type":"#microsoft.graph.itemAttachment","item":{}}]}`,
		`{"toRecipients":` + safe + `} {"bccRecipients":[]}`,
	} {
		before := fake.calls
		if err := request(t, graph, "POST", graphBase+"messages", "application/json", body); err == nil || fake.calls != before {
			t.Fatal("ambiguous or unknown typed mutation reached transport")
		}
	}
	for _, raw := range []string{
		"Resent-Bcc: " + stranger + "\r\n" + string(rawMIME(msEmail, "", "")),
		"Bcc: " + stranger + "\r\n" + string(rawMIME(msEmail, "", googleEmail)),
		"To: undisclosed:;\r\n\r\nbody",
	} {
		before := fake.calls
		body := fmt.Sprintf(`{"raw":%q}`, base64.RawURLEncoding.EncodeToString([]byte(raw)))
		if err := request(t, gmail, "POST", gmailBase+"messages/send", "application/json", body); err == nil || fake.calls != before {
			t.Fatal("ambiguous MIME recipient block reached transport")
		}
	}
	for _, c := range []*http.Client{gmail, graph} {
		base := gmailBase
		if c == graph {
			base = graphBase
		}
		before := fake.calls
		if err := request(t, c, "GET", base+"messages?%24httpMethod=POST", "", ""); err == nil || fake.calls != before {
			t.Fatal("unknown query operation reached transport")
		}
	}
	for _, status := range []int{307, 400} {
		t.Run(fmt.Sprintf("response-%d", status), func(t *testing.T) {
			f := &fakeMail{t: t}
			calls := 0
			transport := transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "userinfo") {
					return f.RoundTrip(r)
				}
				calls++
				resp := response(r, status, "private-body-and-token")
				resp.Header.Set("Location", "https://evil.example.test/private-token")
				return resp, nil
			})
			m, err := demotest.New(context.Background(), options(transport))
			if err != nil {
				t.Fatal(err)
			}
			client, _ := m.HTTPClient(account(false))
			// Even replacing the client's redirect hook cannot bypass the
			// boundary: the underlying redirect destination is stripped.
			client.CheckRedirect = nil
			r, _ := http.NewRequest("GET", graphBase+"messages", nil)
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if calls != 1 || strings.Contains(string(body), "private-") || resp.Header.Get("Location") != "" {
				t.Fatal("provider response/redirect leaked or was followed")
			}
		})
	}
}
