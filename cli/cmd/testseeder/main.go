// testseeder creates SQLite databases with test data for integration tests.
// Usage: testseeder [-list-rows N] [-list-body-bytes N] <email-db-path> <contacts-db-path> [reply-html-fixture]
package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

// testKeyring is a deterministic keyring for seeded test DBs. Production
// code never touches this path — the seeder builds throwaway fixtures
// for integration tests against an HTTP server it controls.
//
// Honours DURIAN_MASTER_KEY_HEX_SECRET (the env var that durian serve
// reads — ADR-0001 audit medium renamed it from DURIAN_MASTER_KEY_HEX
// so secret-detection tooling catches it). Legacy name still
// recognized for grace period. Falls back to a fixed 0xee*32 master
// when neither env var is set.
func testKeyring() *dbcrypto.Keyring {
	var master []byte
	raw := strings.TrimSpace(os.Getenv("DURIAN_MASTER_KEY_HEX_SECRET"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("DURIAN_MASTER_KEY_HEX"))
	}
	if raw != "" {
		m, err := hex.DecodeString(raw)
		if err != nil || len(m) != dbcrypto.MasterKeyLen {
			panic(fmt.Sprintf("testseeder: DURIAN_MASTER_KEY_HEX_SECRET must be 64-char hex of 32 bytes (got len=%d, err=%v)", len(m), err))
		}
		master = m
	} else {
		master = bytes.Repeat([]byte{0xee}, dbcrypto.MasterKeyLen)
	}
	kr, err := dbcrypto.NewKeyring(master)
	if err != nil {
		panic(fmt.Sprintf("testseeder keyring: %v", err))
	}
	return kr
}

func main() {
	listRows := flag.Int("list-rows", 0, "additional inbox threads for native preview/paging tests")
	listBodyBytes := flag.Int("list-body-bytes", 0, "minimum body bytes in each additional preview thread")
	flag.Parse()
	args := flag.Args()
	if (len(args) != 2 && len(args) != 3) || *listRows < 0 || *listBodyBytes < 0 {
		fmt.Fprintln(os.Stderr, "usage: testseeder [-list-rows N] [-list-body-bytes N] <email-db-path> <contacts-db-path> [reply-html-fixture]")
		os.Exit(1)
	}

	var replyHTML string
	if len(args) == 3 {
		data, err := os.ReadFile(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "html fixture: %v\n", err)
			os.Exit(1)
		}
		replyHTML = string(data)
	}
	if err := seedEmailDB(args[0], replyHTML, *listRows, *listBodyBytes); err != nil {
		fmt.Fprintf(os.Stderr, "email db: %v\n", err)
		os.Exit(1)
	}

	if err := seedContactsDB(args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "contacts db: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Seeded test databases successfully")
}

func seedEmailDB(path, replyHTML string, listRows, listBodyBytes int) error {
	db, err := store.Open(path, testKeyring())
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	if err := db.Init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}

	now := time.Now().Unix()

	msgs := []*store.Message{
		{
			StableID:  "seed-email-1",
			MessageID: "msg1@test", Subject: "Hello World",
			FromAddr: "alice@example.com", ToAddrs: "bob@example.com",
			Date: now - 3600, CreatedAt: now, BodyText: "First message body",
			BodyHTML: "<p>First message body</p>", Mailbox: "INBOX", FetchedBody: true,
			Account: "test",
		},
		{
			StableID:  "seed-email-1-duplicate",
			MessageID: "msg1@test", Subject: "Hello World",
			FromAddr: "alice@example.com", ToAddrs: "bob@example.com",
			Date: now - 1800, CreatedAt: now, BodyText: "Duplicate provider body",
			BodyHTML: "<p>Duplicate provider body</p>", Mailbox: "INBOX", FetchedBody: true,
			Account: "test",
		},
		{
			MessageID: "msg2@test", Subject: "Re: Hello World",
			FromAddr: "bob@example.com", ToAddrs: "alice@example.com",
			InReplyTo: "<msg1@test>", Refs: "<msg1@test>",
			Date: now, CreatedAt: now, BodyText: "Reply body",
			BodyHTML: "<p>Reply body</p>", Mailbox: "INBOX", FetchedBody: true,
			Account: "test",
		},
		{
			MessageID: "msg3@test", Subject: "Other Thread",
			FromAddr: "charlie@example.com", ToAddrs: "alice@example.com",
			Date: now - 7200, CreatedAt: now, BodyText: "Different thread",
			BodyHTML: "<p>Different thread</p>", Mailbox: "INBOX", FetchedBody: true,
			Account: "test",
		},
	}

	if replyHTML != "" {
		msgs[2].BodyHTML = replyHTML
	}
	for _, msg := range msgs {
		if err := db.InsertMessage(msg); err != nil {
			return fmt.Errorf("insert %s: %w", msg.MessageID, err)
		}
	}

	// An indexed Reply-To — empty means "replies go to From" — lets a reaction
	// derive its recipient without a provider roundtrip. Seed it on the first
	// message only: the others carry no marker and no RemoteRef, which is the
	// legacy IMAP shape the contract test asserts still refuses.
	if err := db.InsertHeader(msgs[0].ID, "reply-to", ""); err != nil {
		return fmt.Errorf("insert reply-to marker: %w", err)
	}

	seedTags := []struct {
		message *store.Message
		tag     string
	}{
		{msgs[0], "inbox"},
		{msgs[0], "unread"},
		{msgs[1], "inbox"},
		{msgs[2], "inbox"},
		{msgs[3], "inbox"},
		{msgs[3], "flagged"},
	}
	for _, seedTag := range seedTags {
		if err := db.AddTag(seedTag.message.ID, seedTag.tag); err != nil {
			return fmt.Errorf("tag %s with %s: %w", seedTag.message.MessageID, seedTag.tag, err)
		}
	}

	for i := 1; i <= listRows; i++ {
		msg := &store.Message{
			MessageID: fmt.Sprintf("preview-%03d@test", i), Subject: fmt.Sprintf("Preview row %03d", i),
			FromAddr: "sender@example.com", ToAddrs: "alice@example.com", Account: "test",
			Date: now - int64(i+1000)*60, CreatedAt: now, Mailbox: "INBOX", FetchedBody: true,
			BodyText: fmt.Sprintf("Snippetnumber %03d is visible without opening this message.", i),
		}
		if padding := listBodyBytes - len(msg.BodyText); padding > 0 {
			const filler = "Long message content. "
			msg.BodyText += "\n" + strings.Repeat(filler, (padding+len(filler)-2)/len(filler))
		}
		if err := db.InsertMessage(msg); err != nil {
			return fmt.Errorf("insert preview row: %w", err)
		}
		if err := db.AddTag(msg.ID, "inbox"); err != nil {
			return fmt.Errorf("tag preview row: %w", err)
		}
	}

	fmt.Printf("  email.db: %d messages at %s\n", len(msgs)+listRows, path) // encgrep:allow filename, not PII
	return nil
}

func seedContactsDB(path string) error {
	db, err := contacts.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	if err := db.Init(); err != nil {
		return fmt.Errorf("init: %w", err)
	}

	testContacts := []struct{ email, name string }{
		{"alice@example.com", "Alice Smith"},
		{"bob@example.com", "Bob Jones"},
		{"charlie@example.com", "Charlie Brown"},
	}

	for _, c := range testContacts {
		if err := db.Add(c.email, c.name, "imported"); err != nil {
			return fmt.Errorf("add %s: %w", c.email, err)
		}
	}

	fmt.Printf("  contacts.db: %d contacts at %s\n", len(testContacts), path)
	return nil
}
