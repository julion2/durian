package demotest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/demotest"
	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncengine"
)

// Replay the actual received Graph MIME through the real backend, sync engine,
// encrypted store and HTTP handlers. Only provider transport/identity/cursors
// are offline substitutes; this does not replace the live delivery test.
func TestReceivedMIMEReplayThroughSyncAndHTTP(t *testing.T) {
	compressed, err := os.ReadFile("testdata/graph-mime-edges.eml.gz")
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	z.Close()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMail{t: t}
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "userinfo") {
			return fake.RoundTrip(r)
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1.0/me/messages/m1/$value":
			return response(r, 200, string(raw)), nil
		case r.Method == "GET" && r.URL.Path == "/v1.0/me/mailFolders/inbox/messages/delta":
			return response(r, 200, `{"value":[{"id":"m1","internetMessageId":"<mime-edges@example.test>","isRead":false,"receivedDateTime":"2026-10-20T12:00:00Z"}],"@odata.deltaLink":"https://graph.microsoft.com/v1.0/me/mailFolders/inbox/messages/delta?$deltatoken=replay"}`), nil
		case r.Method == "GET" && r.URL.Path == "/v1.0/me/mailFolders":
			return response(r, 200, `{"value":[{"id":"inbox","displayName":"Inbox"}]}`), nil
		case r.Method == "GET" && r.URL.Path == "/v1.0/me/mailFolders/inbox":
			return response(r, 200, `{"id":"inbox"}`), nil
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1.0/me/mailFolders/"):
			return response(r, 404, `{}`), nil
		case r.Method == "POST" && r.URL.String() == graphBatch:
			return response(r, 200, `{"responses":[{"id":"0","status":200,"body":{"id":"m1","isRead":false,"flag":{"flagStatus":"notFlagged"}}}]}`), nil
		default:
			return nil, fmt.Errorf("offline replay refuses unrecorded operation: %s %s", r.Method, r.URL.Path)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := demotest.New(ctx, options(transport))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	key := make([]byte, dbcrypto.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kr, err := dbcrypto.NewKeyring(key)
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	a := account(false)
	a.Name = "microsoft-demo"
	for _, phase := range []string{"initial", "reopen-and-resync"} {
		t.Run(phase, func(t *testing.T) {
			db, err := store.Open(filepath.Join(root, "mail.db"), kr)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.Init(); err != nil {
				t.Fatal(err)
			}
			b, err := m.BackendFor(a)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			engine := syncengine.New(syncengine.Options{Store: db,
				Cursors: syncengine.NewFileCursorStoreWithSuffix(a.AccountIdentifier(), "-replay"),
				Account: a.AccountIdentifier(), Ingest: syncengine.IngestOptions{Account: a.AccountIdentifier()},
				Mode: syncengine.DownloadOnly, Folders: []string{"INBOX"}})
			result, err := engine.Sync(ctx, b)
			if err != nil || result == nil || len(result.Errors) != 0 {
				t.Fatalf("replay sync: result=%+v err=%v", result, err)
			}
			h := handler.New(db, nil)
			h.SetConfig(&config.Config{Accounts: []config.AccountConfig{*a}})
			h.SetBackendFactory(m.BackendFor)
			r := mux.NewRouter()
			r.UseEncodedPath()
			r.HandleFunc("/api/v1/search", h.SearchHandler).Methods("GET")
			r.HandleFunc("/api/v1/threads/{thread_id}", h.ShowThreadHandler).Methods("GET")
			r.HandleFunc("/api/v1/message/body", h.ShowMessageBodyHandler).Methods("GET")
			r.HandleFunc("/api/v1/messages/{message_id}/attachments/{part_id}", h.DownloadAttachmentHandler).Methods("GET")
			server := httptest.NewServer(r)
			defer server.Close()
			get := func(path string, out any) []byte {
				t.Helper()
				resp, err := server.Client().Get(server.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP readback: status=%d err=%v", resp.StatusCode, err)
				}
				if out != nil && json.Unmarshal(data, out) != nil {
					t.Fatal("invalid HTTP JSON")
				}
				return data
			}
			var search, thread, body protocol.Response
			const marker = "durianlive17cece5e7efa721186ad091a091d23180"
			get("/api/v1/search?query="+url.QueryEscape("subject:"+marker), &search)
			if !search.OK || len(search.Results) != 1 {
				t.Fatal("synced message missing or duplicated")
			}
			get("/api/v1/threads/"+url.PathEscape(search.Results[0].ThreadID), &thread)
			if !thread.OK || thread.Thread == nil || len(thread.Thread.Messages) != 1 {
				t.Fatal("thread lost or duplicated message")
			}
			msg := thread.Thread.Messages[0]
			get("/api/v1/message/body?id="+url.QueryEscape(msg.ID), &body)
			if !body.OK || body.MessageBody == nil || !strings.Contains(body.MessageBody.Body, "Bonjour café — 日本語 — 🥭") ||
				!strings.HasPrefix(msg.Body, "Bcc: is ordinary body text") || msg.BCC != msEmail ||
				msg.From != googleEmail || msg.To != googleEmail || msg.Account != a.AccountIdentifier() {
				t.Fatal("body, blind-copy identity or receiving account changed")
			}
			// Independent Python SHA-256 of the authored data / inspected .eml
			// normalization, not values computed by the parser under test.
			want := map[string]struct {
				size int
				hash string
			}{
				"durian-live.txt":       {51, "d60a3987e9d99b784cab1225fc9b79d60776a33db312f429e0d6836af5277602"},
				"nested-shift-jis.eml":  {240, "da75de9de70221b06424acdd4853bc1aeb73e690353c22dbb7247348153ce5e0"},
				"ascii-then-binary.bin": {86022, "0a50a17860dcdd3ca4e77ab6211199e01d835ac0b755cb62d43136c855cbcae0"},
				"récapitulatif 日本語; \"budget\" " + strings.Repeat("segment-", 18) + ".bin": {5, "d935316942792ca4d24d5660b5037ca227f6dcd4e6fcad48996c4f4252ca8fb9"},
			}
			for _, att := range msg.Attachments {
				expected, ok := want[att.Filename]
				data := get(fmt.Sprintf("/api/v1/messages/%s/attachments/%d", url.PathEscape(msg.ID), att.PartID), nil)
				if !ok || att.Size != expected.size || len(data) != expected.size || fmt.Sprintf("%x", sha256.Sum256(data)) != expected.hash {
					t.Fatalf("attachment filename, metadata or downloaded bytes changed: %q", att.Filename)
				}
				delete(want, att.Filename)
			}
			if len(want) != 0 {
				t.Fatal("attachments missing")
			}
		})
	}
}
