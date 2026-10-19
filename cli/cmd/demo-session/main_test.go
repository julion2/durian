package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/demotest"
)

const googleEmail = "google@example.test"
const msEmail = "microsoft@example.test"
const subject = "OfflineSessionCiphertextMarker"

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type statuses chan string

func (s statuses) Write(p []byte) (int, error) { s <- string(p); return len(p), nil }

func offlineMail(t *testing.T, writes *atomic.Int32) func(context.Context) (*demotest.Mail, error) {
	t.Helper()
	return func(ctx context.Context) (*demotest.Mail, error) {
		return demotest.New(ctx, demotest.Options{LiveOptIn: true, GoogleEmail: googleEmail, MicrosoftEmail: msEmail,
			GoogleToken:    func(context.Context) (string, error) { return "fake-google", nil },
			MicrosoftToken: func(context.Context) (string, error) { return "fake-ms", nil },
			Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				body := ""
				flagBatch := r.Method == "POST" && r.URL.Path == "/v1.0/$batch"
				if r.Method != "GET" && !flagBatch {
					writes.Add(1)
					t.Error("unexpected provider mutation reached offline transport")
					return nil, errors.New("unexpected mutation")
				}
				p := r.URL.Path
				gmail := r.URL.Host == "gmail.googleapis.com"
				raw := "Message-ID: <offline-" + r.URL.Host + "@example.test>\r\nFrom: " + googleEmail + "\r\nTo: " + msEmail + "\r\nSubject: " + subject + "\r\nDate: Mon, 19 Oct 2026 10:00:00 +0000\r\n\r\nOffline body\r\n"
				switch {
				case flagBatch:
					var batch struct{ Requests []struct{ ID, Method string } }
					if json.NewDecoder(r.Body).Decode(&batch) != nil {
						return nil, errors.New("bad batch")
					}
					var responses []any
					for _, item := range batch.Requests {
						if item.Method != "GET" {
							t.Error("mutation in flag batch")
							return nil, errors.New("bad batch method")
						}
						responses = append(responses, map[string]any{"id": item.ID, "status": 200, "body": map[string]any{"isRead": true, "flag": map[string]string{"flagStatus": "notFlagged"}}})
					}
					encoded, _ := json.Marshal(map[string]any{"responses": responses})
					body = string(encoded)
				case strings.Contains(p, "userinfo"):
					email := googleEmail
					if r.URL.Host == "graph.microsoft.com" {
						email = msEmail
					}
					body = fmt.Sprintf(`{"email":%q,"email_verified":true,"sub":"offline-sub"}`, email)
				case gmail && strings.HasSuffix(p, "/labels"):
					body = `{"labels":[{"id":"INBOX","name":"INBOX","type":"system"}]}`
				case gmail && strings.HasSuffix(p, "/profile"):
					body = `{"historyId":"123"}`
				case gmail && strings.HasSuffix(p, "/messages"):
					body = `{"messages":[{"id":"g1"}]}`
				case gmail && strings.HasSuffix(p, "/messages/g1"):
					body = fmt.Sprintf(`{"id":"g1","threadId":"t1","historyId":"123","internalDate":"1792404000000","labelIds":["INBOX"],"raw":%q}`, base64.URLEncoding.EncodeToString([]byte(raw)))
				case !gmail && strings.HasSuffix(p, "/mailFolders"):
					body = `{"value":[{"id":"inbox","displayName":"Inbox"},{"id":"junkemail","displayName":"Junk Email"},{"id":"sentitems","displayName":"Sent Items"}]}`
				case !gmail && strings.HasSuffix(p, "/messages/delta"):
					value := `[]`
					if strings.Contains(p, "/inbox/") {
						value = `[{"id":"m1","internetMessageId":"<offline-graph.microsoft.com@example.test>","isRead":true,"receivedDateTime":"2026-10-19T10:00:00Z"}]`
					}
					body = fmt.Sprintf(`{"value":%s,"@odata.deltaLink":%q}`, value, r.URL.Scheme+"://"+r.URL.Host+p+"?$deltatoken=offline")
				case !gmail && strings.Contains(p, "/mailFolders/"):
					body = fmt.Sprintf(`{"id":%q}`, filepath.Base(p))
				case !gmail && strings.HasSuffix(p, "/$value"):
					body = raw
				default:
					t.Error("unexpected provider read reached offline transport")
					return nil, errors.New("unexpected read")
				}
				return &http.Response{Request: r, StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}),
		})
	}
}

func TestSessionOptInAndFreshPrivateRoot(t *testing.T) {
	called := false
	constructor := func(context.Context) (*demotest.Mail, error) { called = true; return nil, errors.New("stop") }
	root := filepath.Join(t.TempDir(), "session")
	o := options{outputDir: root, duration: time.Minute}
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "")
	if run(context.Background(), o, constructor, io.Discard) == nil || called {
		t.Fatal("missing opt-in reached credentials")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("missing opt-in wrote state")
	}
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "1")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if run(context.Background(), o, constructor, io.Discard) == nil || called {
		t.Fatal("existing root reached credentials")
	}
	for _, duration := range []time.Duration{0, 31 * time.Minute} {
		o.duration = duration
		if run(context.Background(), o, constructor, io.Discard) == nil || called {
			t.Fatal("unbounded session accepted")
		}
	}
}

func TestOfflineGuardedSessionHTTPAndShutdown(t *testing.T) {
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "1")
	t.Setenv("DURIAN_GOOGLE_TEST_EMAIL", googleEmail)
	t.Setenv("DURIAN_MS_TEST_EMAIL", msEmail)
	// Any default config/profile/keymap read would fail, rather than silently
	// using the test runner's or developer's configuration.
	defaultConfig := filepath.Join(t.TempDir(), "durian")
	if err := os.Mkdir(defaultConfig, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.pkl", "profiles.pkl", "keymaps.pkl"} {
		if err := os.WriteFile(filepath.Join(defaultConfig, name), []byte("invalid default config"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(defaultConfig))
	// Any accidental use of a default provider client fails without networking.
	original := http.DefaultTransport
	http.DefaultTransport = transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("default transport forbidden") })
	t.Cleanup(func() { http.DefaultTransport = original })
	root := filepath.Join(t.TempDir(), "session")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := make(statuses, 4)
	done := make(chan error, 1)
	var writes atomic.Int32
	go func() {
		done <- run(ctx, options{outputDir: root, duration: time.Minute}, offlineMail(t, &writes), status)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("session did not stop")
		}
		if _, err := os.Stat(filepath.Join(root, "connection.json")); !os.IsNotExist(err) {
			t.Error("stale connection file retained")
		}
	})
	select {
	case got := <-status:
		if got != "READY\n" {
			t.Fatal("unexpected nonsecret status")
		}
	case err := <-done:
		done <- err
		t.Fatal("offline startup failed", err)
	case <-time.After(10 * time.Second):
		t.Fatal("offline startup timed out")
	}
	var c connection
	data, err := os.ReadFile(filepath.Join(root, "connection.json"))
	if err != nil || json.Unmarshal(data, &c) != nil || len(c.Token) != 64 || c.API != 1 {
		t.Fatal("invalid private connection file")
	}
	for _, name := range []string{"config.json", "profiles.json", "connection.json"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("session file is not private")
		}
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("root is not private")
	}
	client := &http.Client{Transport: original, Timeout: 3 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	request := func(method, path, payload, token, host string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, c.Backend+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d", method, path, resp.StatusCode, want)
		}
		return body
	}
	request("GET", "/api/v1/version", "", "wrong", "", 401)
	request("GET", "/api/v1/version", "", c.Token, "evil.test", 403)
	for _, path := range []string{"/api/v1/version", "/api/v1/settings", "/api/v1/profiles", "/api/v1/keymaps", "/api/v1/compose-settings", "/api/v1/groups", "/api/v1/contacts", "/api/v1/local-drafts"} {
		request("GET", path, "", c.Token, "", 200)
	}
	for _, account := range []string{"gmail", "outlook", "gmail-demo", "outlook-demo"} {
		body := request("GET", "/api/v1/search?query=path:"+account+"/**&limit=5", "", c.Token, "", 200)
		if !bytes.Contains(body, []byte(subject)) {
			t.Fatal("guarded synced account not searchable through HTTP")
		}
	}
	compose := request("GET", "/api/v1/compose-settings", "", c.Token, "", 200)
	if !bytes.Contains(compose, []byte("Demo session signature")) {
		t.Fatal("private signature config not used")
	}
	request("PUT", "/api/v1/local-drafts/offline", `{"draft_json":{"subject":"`+subject+`"}}`, c.Token, "", 200)
	request("GET", "/api/v1/local-drafts/offline", "", c.Token, "", 200)
	request("GET", "/api/v1/calendars?account=gmail", "", c.Token, "", 409)
	request("POST", "/api/v1/calendars/sync/event", `{"account":"gmail","calendar":"demo","uid":"u","operation":"save"}`, c.Token, "", 503)
	// The REAL worker must reject this at the injected sender guard before
	// any provider mutation, leaving an explicit failed outbox row.
	request("POST", "/api/v1/outbox/send", `{"idempotency_key":"offline-reply","from":"`+msEmail+`","to":["`+googleEmail+`"],"subject":"offline","body":"test","in_reply_to":"<parent@example.test>"}`, c.Token, "", 200)
	request("POST", "/api/v1/outbox/send", `{"idempotency_key":"offline-stranger","from":"`+googleEmail+`","to":["stranger@example.test"],"subject":"offline","body":"test"}`, c.Token, "", 200)
	deadline := time.Now().Add(8 * time.Second)
	for {
		queue := request("GET", "/api/v1/outbox", "", c.Token, "", 200)
		if bytes.Contains(queue, []byte(`"last_error":"`)) && !bytes.Contains(queue, []byte(`"last_error":""`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not reject guarded Graph reply and Gmail recipient")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if writes.Load() != 0 {
		t.Fatal("download-only/blocked send performed provider mutation")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "email.db") {
			data, err := os.ReadFile(filepath.Join(root, entry.Name()))
			if err != nil {
				t.Fatal("read private database file")
			}
			if bytes.Contains(data, []byte(subject)) {
				t.Fatal("mail/draft plaintext leaked to database files")
			}
		}
		if strings.Contains(entry.Name(), "contacts") {
			t.Fatal("plaintext contacts persisted")
		}
	}
}

func TestOfflineSessionDeadline(t *testing.T) {
	t.Setenv("DURIAN_MAIL_LIVE_TEST", "1")
	t.Setenv("DURIAN_GOOGLE_TEST_EMAIL", googleEmail)
	t.Setenv("DURIAN_MS_TEST_EMAIL", msEmail)
	root := filepath.Join(t.TempDir(), "session")
	var writes atomic.Int32
	status := make(statuses, 4)
	done := make(chan error, 1)
	constructor := offlineMail(t, &writes)
	go func() {
		done <- run(context.Background(), options{outputDir: root, duration: time.Second}, constructor, status)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session exceeded its lifetime")
	}
	if len(status) != 1 || <-status != "READY\n" || writes.Load() != 0 {
		t.Fatal("deadline session did not reach guarded read-only readiness")
	}
	if _, err := os.Stat(filepath.Join(root, "connection.json")); !os.IsNotExist(err) {
		t.Fatal("deadline left stale connection details")
	}
}
