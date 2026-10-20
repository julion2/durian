//go:build demointegration

package demotest_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/demotest"
	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncengine"
)

const liveAttachment = "Durian LIVE attachment v1\ncafé / 日本語 / 🥭\n"

// Computed independently from the constant fixture, not from any provider
// response, attachment metadata, sender serialization or store readback.
const liveAttachmentSHA256 = "d60a3987e9d99b784cab1225fc9b79d60776a33db312f429e0d6836af5277602"

type liveMailCase int

const (
	livePlain liveMailCase = iota
	liveHTML
	liveMIMEEdges
	liveInvitation
)

// TestLiveMailPublicFlow is deliberately absent from normal test builds. It
// leaves TWO intentional demo mails in the dedicated mailboxes, never deletes
// existing mail, and never enqueues a replacement after a timeout/error.
// Run only after the Microsoft credential refresh worker has finished.
func TestLiveMailPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0, 1}, livePlain)
}

// One additional, distinct Gmail→Graph message, not a retry of the unresolved
// Graph→Gmail delivery. Covers HTML, Unicode filenames, empty attachments and
// a nontrivial binary attachment through the same public outbox/sync path.
func TestLiveHTMLPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0}, liveHTML)
}

// Independently authored integration inputs inspired by Thunderbird's
// test_bcc.js and test_attachment.js contracts, not copied MPL fixtures/code.
// One new Gmail→Outlook Bcc delivery covers a nested non-UTF-8 email, a long
// quoted Unicode filename, and binary bytes after a substantial ASCII prefix.
func TestLiveMIMEEdgesPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0}, liveMIMEEdges)
}

// A real iTIP mail, not a parser fixture: both transport recipients and the
// embedded organizer/attendee are exclusively the two verified demo accounts.
func TestLiveInvitationPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0}, liveInvitation)
}

func exerciseLiveMail(t *testing.T, directions []int, variant liveMailCase) {
	t.Helper()
	if os.Getenv("DURIAN_MAIL_LIVE_TEST") != "1" {
		t.Skip("requires explicit mail live-test opt-in")
	}
	resume := os.Getenv("DURIAN_MAIL_READBACK_MARKER")
	if resume != "" && (!regexp.MustCompile(`^durianlive[0-9a-f]{33}$`).MatchString(resume) || len(directions) != 1) {
		t.Fatal("readback requires one exact earlier run marker and one direction")
	}
	// Production logging includes provider/local IDs and may log mailbox
	// content on failures. This process runs only this test: discard it entirely,
	// rather than attempt to redact an open-ended set of provider error shapes.
	previousLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLog) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		t.Log("No remote cleanup performed. On failure, inspect both dedicated mailboxes privately before any rerun; do not resend an uncertain delivery.")
	})
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	master := make([]byte, dbcrypto.MasterKeyLen)
	if _, err := rand.Read(master); err != nil {
		t.Fatal("generate ephemeral master key")
	}
	kr, err := dbcrypto.NewKeyring(master)
	clear(master)
	if err != nil {
		t.Fatal("derive ephemeral store keys")
	}
	db, err := store.Open(filepath.Join(root, "email.db"), kr)
	if err != nil {
		t.Fatal("open isolated encrypted store")
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Init(); err != nil {
		t.Fatal("initialize isolated encrypted store")
	}
	// The same identity-verified composition root owns every sender and read
	// backend; no default credentials, testseeder, watcher, or raw-send shortcut.
	mailDemo, err := demotest.NewFromEnv(ctx)
	if err != nil {
		t.Fatal("verify both live mail identities/credentials (details withheld)")
	}
	cfg := &config.Config{Accounts: []config.AccountConfig{
		{Name: "google-demo", Email: os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), OAuth: &config.OAuthConfig{Provider: "google"}},
		{Name: "microsoft-demo", Email: os.Getenv("DURIAN_MS_TEST_EMAIL"), OAuth: &config.OAuthConfig{Provider: "microsoft"}},
	}}
	h := handler.New(db, nil)
	h.SetConfig(cfg)
	h.SetBackendFactory(mailDemo.BackendFor)
	r := mux.NewRouter()
	r.UseEncodedPath()
	r.HandleFunc("/api/v1/outbox/send", h.EnqueueOutboxHandler).Methods("POST")
	r.HandleFunc("/api/v1/outbox", h.ListOutboxHandler).Methods("GET")
	r.HandleFunc("/api/v1/search", h.SearchHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}", h.ShowThreadHandler).Methods("GET")
	r.HandleFunc("/api/v1/message/body", h.ShowMessageBodyHandler).Methods("GET")
	r.HandleFunc("/api/v1/messages/{message_id}/attachments/{part_id}", h.DownloadAttachmentHandler).Methods("GET")
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 65 * time.Second

	// Keep the worker's real durable sender protocol and cancellation, but
	// permit only ONE sender invocation per account, even on errors.
	attempted := map[string]*atomic.Bool{"google-demo": {}, "microsoft-demo": {}}
	worker := handler.NewOutboxWorker(db, cfg, nil)
	worker.SetSenderFactory(func(a *config.AccountConfig) (mailsend.Sender, error) {
		s, err := mailDemo.SenderFor(a)
		if err != nil {
			return nil, err
		}
		return &singleLiveSend{sender: s.(mailsend.DurableSender), run: ctx, attempted: attempted[a.AccountIdentifier()]}, nil
	})
	done := make(chan struct{})
	go func() { defer close(done); worker.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("outbox worker did not stop within cancellation bound")
		}
	})
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate unique run marker")
	}
	for _, direction := range directions {
		from, to := &cfg.Accounts[direction], &cfg.Accounts[1-direction]
		marker := "durianlive" + hex.EncodeToString(nonce[:]) + fmt.Sprint(direction)
		if resume != "" {
			marker = resume
		}
		// No subtest: a failure aborts the entire run instead of sending the
		// other direction after an uncertain outcome.
		t.Logf("case %d, run marker %s (readback-only=%v)", variant, marker, resume != "")
		exerciseLiveDirection(t, ctx, client, server.URL, db, mailDemo, from, to, marker, variant, resume != "")
		if attempted[from.AccountIdentifier()].Load() != (resume == "") {
			t.Fatal("public outbox did not invoke the guarded sender")
		}
		if t.Failed() {
			t.FailNow()
		}
		t.Logf("direction %d passed (readback-only=%v): provider/sync/search/thread/body/attachment bytes and account ownership", direction+1, resume != "")
	}
}

type singleLiveSend struct {
	sender    mailsend.DurableSender
	run       context.Context
	attempted *atomic.Bool
}

func (s *singleLiveSend) SavesSentCopy() bool { return s.sender.SavesSentCopy() }
func (s *singleLiveSend) Send(ctx context.Context, msg *mailsend.Message) error {
	return s.SendAfterPersist(ctx, msg, func(string) error { return nil })
}
func (s *singleLiveSend) SendAfterPersist(ctx context.Context, msg *mailsend.Message, persist func(string) error) error {
	if s.attempted == nil || !s.attempted.CompareAndSwap(false, true) {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("live harness refuses another send attempt")}
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.run, cancel)
	defer stop()
	defer cancel()
	if s.run.Err() != nil {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("live harness run ended")}
	}
	return s.sender.SendAfterPersist(ctx, msg, persist)
}

func exerciseLiveDirection(t *testing.T, parent context.Context, client *http.Client, base string, db *store.DB, m *demotest.Mail, from, to *config.AccountConfig, marker string, variant liveMailCase, readback bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	html := variant == liveHTML
	subject := "Durian Test LIVE café 日本語 🥭 " + marker
	body := "Bonjour café — 日本語 — 🥭\nBounded two-party mail demo.\n" + marker
	type attachment struct {
		name, mime string
		data       []byte
	}
	attachments := []attachment{{"durian-live.txt", "text/plain", []byte(liveAttachment)}}
	if html {
		body = "<p>Bonjour café — 日本語 — 🥭</p><p><strong>Bounded HTML demo.</strong></p><p>" + marker + "</p>"
		attachments[0].name = "résumé 日本語 🥭.txt"
		pattern := make([]byte, 256)
		for i := range pattern {
			pattern[i] = byte(i)
		}
		attachments = append(attachments,
			attachment{"empty.txt", "text/plain", []byte{}},
			attachment{"binary-768k.bin", "application/octet-stream", bytes.Repeat(pattern, 3072)})
	}
	if variant == liveMIMEEdges {
		body = "Bcc: is ordinary body text, not a transport header.\n" + body
		// Python's independent Shift_JIS codec encodes 会議の資料です as
		// ie+LY4LMjpGXv4LFgrc=. All nested addresses are synthetic fixtures.
		nested := "From: Nested Author <author@example.invalid>\r\nTo: Reader <reader@example.invalid>\r\n" +
			"Subject: Encoded meeting notes\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=Shift_JIS\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\nie+LY4LMjpGXv4LFgrc=\r\n"
		lateBinary := append(bytes.Repeat([]byte("ASCII planning notes\n"), 4096), 0, 255, 128, 1, 2, 3)
		attachments = append(attachments,
			attachment{"nested-shift-jis.eml", "message/rfc822", []byte(nested)},
			attachment{"récapitulatif 日本語; \"budget\" " + strings.Repeat("segment-", 18) + ".bin", "application/octet-stream", []byte{0, 255, 127, 128, 42}},
			attachment{"ascii-then-binary.bin", "application/octet-stream", lateBinary})
	}
	if variant == liveInvitation {
		ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Durian//Live integration//EN\r\nMETHOD:REQUEST\r\n" +
			"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\n" +
			"BEGIN:STANDARD\r\nDTSTART:20001029T030000\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\n" +
			"BEGIN:DAYLIGHT\r\nDTSTART:20000326T020000\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\nTZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\n" +
			"BEGIN:VEVENT\r\nUID:" + marker + "@durian.invalid\r\nDTSTAMP:20261020T120000Z\r\nSEQUENCE:0\r\n" +
			"DTSTART;TZID=Europe/Berlin:20261103T090000\r\nDTEND;TZID=Europe/Berlin:20261103T100000\r\n" +
			"SUMMARY:Durian Test meeting " + marker + "\r\n" +
			"ORGANIZER:mailto:" + from.Email + "\r\n" +
			"ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:\r\n mailto:" + to.Email + "\r\n" +
			"END:VEVENT\r\nEND:VCALENDAR\r\n"
		attachments = append(attachments, attachment{"invitation.ics", "text/calendar; charset=UTF-8; method=REQUEST", []byte(ics)})
	}
	draft := handler.OutboxDraft{IdempotencyKey: marker, From: from.Email, To: []string{to.Email}, Subject: subject, Body: body,
		IsHTML: html}
	expectedTo := to.Email
	if variant == liveMIMEEdges {
		draft.To, draft.BCC = []string{from.Email}, []string{to.Email}
		expectedTo = from.Email
	}
	for _, a := range attachments {
		draft.Attachments = append(draft.Attachments, handler.OutboxAttachment{Filename: a.name, MIMEType: a.mime, DataBase64: base64.StdEncoding.EncodeToString(a.data)})
	}
	received := attachments
	if html {
		// Real Gmail Sent retains all three, but Outlook's received MIME and
		// attachment API omit empty.txt. Characterize that delivery behavior,
		// not a Durian parser exception: nonempty filenames/bytes must survive.
		received = []attachment{attachments[0], attachments[2]}
	}
	if variant == liveMIMEEdges {
		// The real Graph $value response reserializes the attached message:
		// charset gains quotes (+2 bytes) and MIME-Version moves after CTE.
		// Confirmed independently with Python's email parser; the nested
		// decoded text remains 会議の資料です. Other attachments stay exact.
		received = append([]attachment(nil), attachments...)
		received[1].data = []byte("From: Nested Author <author@example.invalid>\r\nTo: Reader <reader@example.invalid>\r\n" +
			"Subject: Encoded meeting notes\r\nContent-Type: text/plain; charset=\"Shift_JIS\"\r\n" +
			"Content-Transfer-Encoding: base64\r\nMIME-Version: 1.0\r\n\r\nie+LY4LMjpGXv4LFgrc=\r\n")
	}
	var enqueued struct {
		OK bool  `json:"ok"`
		ID int64 `json:"id"`
	}
	if !readback {
		liveJSON(t, ctx, client, base, "POST", "/api/v1/outbox/send", draft, &enqueued)
		if !enqueued.OK || enqueued.ID <= 0 {
			t.Fatal("HTTP enqueue did not acknowledge one outbox action")
		}
	}
	// Only the recipient account is synced. A local Sent projection made by
	// the worker can therefore never satisfy the received-message assertion.
	b, err := m.BackendFor(to)
	if err != nil {
		t.Fatal("construct guarded receiving backend")
	}
	defer b.Close()
	folders, suffix := []string{"INBOX", "junk"}, "-demo-graph"
	if to.OAuth.Provider == "google" {
		folders, suffix = []string{"ALL"}, "-demo-gmail"
	}
	engine := syncengine.New(syncengine.Options{Store: db,
		Cursors: syncengine.NewFileCursorStoreWithSuffix(to.AccountIdentifier(), suffix),
		Account: to.AccountIdentifier(), Ingest: syncengine.IngestOptions{Account: to.AccountIdentifier()},
		Mode: syncengine.DownloadOnly, Folders: folders, BatchLimit: 20, MaxPerFolder: 20,
		Timeout: 30 * time.Second, RecoveryTimeout: 30 * time.Second})
	for {
		if ctx.Err() != nil {
			t.Fatal("90s delivery/readback bound reached; outcome uncertain, inspect privately before rerun")
		}
		var queue []struct {
			ID        int64  `json:"id"`
			Attempts  int    `json:"attempts"`
			LastError string `json:"last_error"`
		}
		liveJSON(t, ctx, client, base, "GET", "/api/v1/outbox", nil, &queue)
		if len(queue) > 1 || len(queue) == 1 && (queue[0].ID != enqueued.ID || queue[0].Attempts != 0 || queue[0].LastError != "") {
			t.Fatal("outbox failure/reconciliation state; no resend, inspect privately")
		}
		if len(queue) == 0 {
			result, err := engine.Sync(ctx, b)
			if err != nil || result == nil || len(result.Errors) != 0 {
				t.Fatal("real receiving sync failed (details withheld); no resend")
			}
			var search protocol.Response
			query := "path:" + to.AccountIdentifier() + " subject:" + marker
			liveJSON(t, ctx, client, base, "GET", "/api/v1/search?limit=5&query="+url.QueryEscape(query), nil, &search)
			if !search.OK || len(search.Results) > 1 {
				t.Fatal("HTTP search failed or matched duplicate demo threads")
			}
			if len(search.Results) == 1 {
				if search.Results[0].Subject != subject {
					t.Fatal("unicode subject roundtrip mismatch")
				}
				var thread protocol.Response
				liveJSON(t, ctx, client, base, "GET", "/api/v1/threads/"+url.PathEscape(search.Results[0].ThreadID), nil, &thread)
				if !thread.OK || thread.Thread == nil || thread.Thread.Subject != subject {
					t.Fatal("HTTP thread readback mismatch")
				}
				matches := 0
				for _, msg := range thread.Thread.Messages {
					if msg.Account != to.AccountIdentifier() {
						continue // thread may also contain the worker's local Sent row
					}
					matches++
					row, err := db.GetByIdentifier(msg.ID)
					if err != nil || row == nil || row.Account != to.AccountIdentifier() || row.RemoteRef == "" || !row.FetchedBody {
						t.Fatal("readback did not belong to the provider-synced recipient account")
					}
					fromAddr, fromErr := mail.ParseAddress(msg.From)
					toAddrs, toErr := mail.ParseAddressList(msg.To)
					if fromErr != nil || toErr != nil || fromAddr.Address != from.Email || len(toAddrs) != 1 || toAddrs[0].Address != expectedTo {
						t.Fatal("received envelope or unicode body mismatch")
					}
					if variant == liveInvitation {
						inv := msg.Invitation
						if inv == nil || inv.Method != "REQUEST" || inv.Occurrence || inv.Event.UID != marker+"@durian.invalid" ||
							inv.Event.Start.UTC().Format(time.RFC3339) != "2026-11-03T08:00:00Z" || inv.Event.End.UTC().Format(time.RFC3339) != "2026-11-03T09:00:00Z" ||
							inv.Event.Subject != "Durian Test meeting "+marker || inv.Event.Organizer == nil || inv.Event.Organizer.Email != from.Email ||
							len(inv.Event.Attendees) != 1 || inv.Event.Attendees[0].Email != to.Email {
							t.Fatal("received invitation lost method, identity, participants or Berlin winter time")
						}
						t.Log("received REQUEST projects the exact two-party meeting and 09:00 Berlin as 08:00 UTC")
					}
					if variant == liveMIMEEdges && msg.BCC != "" {
						// RFC 5322 §3.6.3 permits a blind recipient's copy to
						// retain its own Bcc address. This two-party case cannot
						// prove privacy between multiple blind recipients.
						bcc, err := mail.ParseAddressList(msg.BCC)
						if err != nil || len(bcc) != 1 || bcc[0].Address != to.Email {
							t.Fatal("received Bcc metadata exposed an unexpected recipient")
						}
						t.Log("blind recipient copy contains only its own Bcc address (RFC 5322 §3.6.3)")
					}
					var quoted protocol.Response
					liveJSON(t, ctx, client, base, "GET", "/api/v1/message/body?id="+url.QueryEscape(msg.ID), nil, &quoted)
					if !quoted.OK || quoted.MessageBody == nil || len(msg.Attachments) != len(received) {
						t.Fatal("HTTP body or attachment metadata mismatch")
					}
					if html {
						for _, text := range []string{"Bonjour café — 日本語 — 🥭", "Bounded HTML demo.", marker} {
							if !strings.Contains(msg.HTML, text) || !strings.Contains(msg.Body, text) || !strings.Contains(quoted.MessageBody.Body, text) {
								t.Fatal("HTML or its plain-text reading projection lost content")
							}
						}
						if !strings.Contains(msg.HTML, "<strong>") {
							t.Fatal("HTML markup disappeared")
						}
					} else if normalizeLiveBody(msg.Body) != body || normalizeLiveBody(quoted.MessageBody.Body) != body {
						t.Fatal("plain-text body changed")
					}
					for _, expected := range received {
						found := 0
						for _, actual := range msg.Attachments {
							if actual.Filename != expected.name {
								continue
							}
							found++
							if actual.Size != len(expected.data) {
								t.Errorf("attachment %q metadata size %d differs from submitted %d bytes", expected.name, actual.Size, len(expected.data))
							}
							data := liveHTTP(t, ctx, client, base, "GET", fmt.Sprintf("/api/v1/messages/%s/attachments/%d", url.PathEscape(msg.ID), actual.PartID), nil)
							if !bytes.Equal(data, expected.data) {
								t.Errorf("attachment %q readback (%d bytes) differs from submitted input", expected.name, len(data))
							}
							if expected.mime == "text/plain" && len(expected.data) > 0 {
								sum := sha256.Sum256(data)
								if hex.EncodeToString(sum[:]) != liveAttachmentSHA256 {
									t.Fatal("HTTP attachment hash differs from independent input fixture")
								}
							}
						}
						if found != 1 {
							t.Fatal("attachment filename lost or duplicated")
						}
					}
				}
				if matches != 1 {
					t.Fatal("expected exactly one synced receiving-account message")
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("90s delivery poll expired; no resend, inspect privately")
		case <-time.After(5 * time.Second):
		}
	}
}

func normalizeLiveBody(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func liveJSON(t *testing.T, ctx context.Context, c *http.Client, base, method, path string, payload, out any) {
	t.Helper()
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			t.Fatal("encode local HTTP request (details withheld)")
		}
	}
	if json.Unmarshal(liveHTTP(t, ctx, c, base, method, path, data), out) != nil {
		t.Fatal("decode local HTTP response (details withheld)")
	}
}

func liveHTTP(t *testing.T, ctx context.Context, c *http.Client, base, method, path string, data []byte) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal("construct local HTTP request (details withheld)")
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal("local HTTP request failed (details withheld); no resend")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 || resp.StatusCode != http.StatusOK {
		t.Fatal("local HTTP status/body failed (details withheld); no resend")
	}
	return body
}
