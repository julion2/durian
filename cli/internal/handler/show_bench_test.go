package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

// seedLongThread stores a thread like a long real one: messages replying to
// each other, each with an HTML part of a newsletter's size (tables, a quoted
// earlier message), two tags and two attachments.
func seedLongThread(b *testing.B, messages int) (*Handler, string) {
	b.Helper()
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		b.Fatal(err)
	}
	db, err := store.Open(":memory:", kr)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Init(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })

	row := `<tr><td style="padding:8px;font-family:Arial">Line %d of the update, with <a href="https://example.com/%d">a link</a> and some text to read.</td></tr>`
	var table strings.Builder
	for i := 0; i < 220; i++ {
		fmt.Fprintf(&table, row, i, i)
	}
	html := `<html><body><table width="600">` + table.String() + `</table>` +
		`<div class="gmail_quote"><blockquote type="cite">An earlier message, quoted.<br>` + strings.Repeat("Quoted line.<br>", 200) + `</blockquote></div></body></html>`

	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC).Unix()
	previous := ""
	for i := 0; i < messages; i++ {
		id := fmt.Sprintf("long%d@example.com", i)
		m := &store.Message{
			MessageID: id, Subject: "Re: Quarterly update", FromAddr: fmt.Sprintf("Person %d <p%d@example.com>", i%3, i%3),
			ToAddrs: "team@example.com", Date: at + int64(i)*600, CreatedAt: at, Mailbox: "INBOX", FetchedBody: true,
			BodyText: strings.Repeat("Plain text of the update. ", 60) + "\n\nOn Monday someone wrote:\n> earlier\n",
			BodyHTML: html, InReplyTo: previous, Refs: previous,
		}
		if err := db.InsertMessage(m); err != nil {
			b.Fatal(err)
		}
		previous = "<" + id + ">"
		stored, err := db.GetByMessageID(id)
		if err != nil {
			b.Fatal(err)
		}
		for _, tag := range []string{"inbox", "unread"} {
			if err := db.AddTag(stored.ID, tag); err != nil {
				b.Fatal(err)
			}
		}
		for part := 2; part <= 3; part++ {
			att := &store.Attachment{MessageDBID: stored.ID, PartID: part, Filename: fmt.Sprintf("file%d.pdf", part), ContentType: "application/pdf", Size: 120000, Disposition: "attachment"}
			if err := db.InsertAttachment(att); err != nil {
				b.Fatal(err)
			}
		}
	}
	first, err := db.GetByMessageID("long0@example.com")
	if err != nil {
		b.Fatal(err)
	}
	return New(db, nil), first.ThreadID
}

// go test ./internal/handler -run '^$' -bench ShowThread
func BenchmarkShowThread(b *testing.B) {
	h, thread := seedLongThread(b, 14)
	router := newTestRouter(h, nil)
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/threads/"+thread, nil))
		if w.Code != http.StatusOK {
			b.Fatalf("status %d", w.Code)
		}
	}
}
