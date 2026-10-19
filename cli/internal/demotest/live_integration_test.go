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

// TestLiveMailPublicFlow is deliberately absent from normal test builds. It
// leaves TWO intentional demo mails in the dedicated mailboxes, never deletes
// existing mail, and never enqueues a replacement after a timeout/error.
// Run only after the Microsoft credential refresh worker has finished.
func TestLiveMailPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0, 1}, false)
}

// One additional, distinct Gmail→Graph message, not a retry of the unresolved
// Graph→Gmail delivery. Covers HTML, Unicode filenames, empty attachments and
// a nontrivial binary attachment through the same public outbox/sync path.
func TestLiveHTMLPublicFlow(t *testing.T) {
	exerciseLiveMail(t, []int{0}, true)
}

func exerciseLiveMail(t *testing.T, directions []int, html bool) {
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
		exerciseLiveDirection(t, ctx, client, server.URL, db, mailDemo, from, to, marker, html, resume != "")
		if attempted[from.AccountIdentifier()].Load() != (resume == "") {
			t.Fatal("public outbox did not invoke the guarded sender")
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

func exerciseLiveDirection(t *testing.T, parent context.Context, client *http.Client, base string, db *store.DB, m *demotest.Mail, from, to *config.AccountConfig, marker string, html, readback bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
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
	draft := handler.OutboxDraft{IdempotencyKey: marker, From: from.Email, To: []string{to.Email}, Subject: subject, Body: body,
		IsHTML: html}
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
					if fromErr != nil || toErr != nil || fromAddr.Address != from.Email || len(toAddrs) != 1 || toAddrs[0].Address != to.Email {
						t.Fatal("received envelope or unicode body mismatch")
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
								t.Fatal("attachment metadata reports encoded rather than decoded bytes")
							}
							data := liveHTTP(t, ctx, client, base, "GET", fmt.Sprintf("/api/v1/messages/%s/attachments/%d", url.PathEscape(msg.ID), actual.PartID), nil)
							if !bytes.Equal(data, expected.data) {
								t.Fatal("HTTP attachment bytes differ from independently constructed input")
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
