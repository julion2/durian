package store

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/dbcrypto"
)

// A mailbox where one tag covers everything and another a little:
// 60,001 messages, all tagged archive, 1,000 tagged inbox, one with a unique
// word in its body. Searches combining the big tag with something selective
// must stay cheap; searches by tag alone must not scan.
var (
	tagBenchOnce   sync.Once
	tagBenchDB     *DB
	tagBenchThread string
	tagBenchErr    error
)

func tagBenchMailbox(b *testing.B) (*DB, string) {
	b.Helper()
	tagBenchOnce.Do(func() {
		tagBenchDB, tagBenchThread, tagBenchErr = seedTagBench(60000, 1000)
	})
	if tagBenchErr != nil {
		b.Fatal(tagBenchErr)
	}
	return tagBenchDB, tagBenchThread
}

func seedTagBench(copies, inbox int) (*DB, string, error) {
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		return nil, "", err
	}
	db, err := Open(":memory:", kr)
	if err != nil {
		return nil, "", err
	}
	if err := db.Init(); err != nil {
		return nil, "", err
	}
	now := time.Now().Unix()
	template := &Message{MessageID: "template@x", Subject: "Status", FromAddr: "a@example.com", ToAddrs: "b@example.com",
		Date: now, CreatedAt: now, BodyText: "routine status update", Mailbox: "INBOX", FetchedBody: true}
	needle := &Message{MessageID: "needle@x", Subject: "Status", FromAddr: "a@example.com", ToAddrs: "b@example.com",
		Date: now, CreatedAt: now, BodyText: "the uniqueneedle is here", Mailbox: "INBOX", FetchedBody: true}
	for _, m := range []*Message{template, needle} {
		if err := db.InsertMessage(m); err != nil {
			return nil, "", err
		}
	}
	tmpl, err := db.GetByMessageID("template@x")
	if err != nil {
		return nil, "", err
	}
	// copies of the template in one statement (InsertMessage resolves threads
	// with a scan per insert – quadratic at this size)
	rows, err := db.db.Query("SELECT name FROM pragma_table_info('messages') WHERE name != 'id'")
	if err != nil {
		return nil, "", err
	}
	var cols, exprs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, "", err
		}
		cols = append(cols, name)
		switch name {
		case "message_id":
			exprs = append(exprs, "'bench-' || seq.n || '@x'")
		case "thread_id":
			exprs = append(exprs, "printf('%016x', seq.n)")
		case "date":
			exprs = append(exprs, "t.date - seq.n")
		default:
			exprs = append(exprs, "t."+name)
		}
	}
	rows.Close()
	insert := fmt.Sprintf(`INSERT INTO messages (%s)
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		SELECT %s FROM seq, (SELECT * FROM messages WHERE id = ?) AS t`,
		strings.Join(cols, ", "), strings.Join(exprs, ", "))
	if _, err := db.db.Exec(insert, copies, tmpl.ID); err != nil {
		return nil, "", fmt.Errorf("copies: %w", err)
	}
	if _, err := db.db.Exec(`INSERT INTO tags (message_id, tag) SELECT id, 'archive' FROM messages`); err != nil {
		return nil, "", err
	}
	if _, err := db.db.Exec(`INSERT INTO tags (message_id, tag) SELECT id, 'inbox' FROM messages ORDER BY id DESC LIMIT ?`, inbox); err != nil {
		return nil, "", err
	}
	n, err := db.GetByMessageID("needle@x")
	if err != nil {
		return nil, "", err
	}
	return db, n.ThreadID, nil
}

// go test ./internal/store -run '^$' -bench TagSearch -benchtime 20x
func BenchmarkTagSearch(b *testing.B) {
	db, thread := tagBenchMailbox(b)
	// each op: Search and SearchCount, as a client lists and counts a view
	for _, c := range []struct{ name, query string }{
		{"tag:inbox", "tag:inbox"},
		{"tag:inbox AND NOT tag:archive", "tag:inbox AND NOT tag:archive"},
		// two tags compete: the only case that counts (the smaller drives)
		{"tag:archive AND tag:inbox", "tag:archive AND tag:inbox"},
		{"tag:archive AND uniqueneedle", "tag:archive AND uniqueneedle"},
		{"tag:archive AND thread", "tag:archive AND thread:" + thread},
		{"tag:archive AND (uniqueneedle OR missingneedle)", "tag:archive AND (uniqueneedle OR missingneedle)"},
		{"tag:archive AND (thread OR thread:missing)", "tag:archive AND (thread:" + thread + " OR thread:missing)"},
	} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := db.Search(c.query, 50); err != nil {
					b.Fatal(err)
				}
				if _, err := db.SearchCount(c.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
