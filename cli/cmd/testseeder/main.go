// testseeder creates SQLite databases with test data for integration tests.
// Usage: testseeder <email-db-path> <contacts-db-path>
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/durian-dev/durian/cli/internal/contacts"
	"github.com/durian-dev/durian/cli/internal/store"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testseeder <email-db-path> <contacts-db-path>")
		os.Exit(1)
	}

	if err := seedEmailDB(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "email db: %v\n", err)
		os.Exit(1)
	}

	if err := seedContactsDB(os.Args[2]); err != nil {
		fmt.Fprintf(os.Stderr, "contacts db: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Seeded test databases successfully")
}

func seedEmailDB(path string) error {
	db, err := store.Open(path)
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
			MessageID: "msg1@test", Subject: "Hello World",
			FromAddr: "alice@example.com", ToAddrs: "bob@example.com",
			Date: now - 3600, CreatedAt: now, BodyText: "First message body",
			BodyHTML: "<p>First message body</p>", Mailbox: "INBOX", FetchedBody: true,
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

	for _, msg := range msgs {
		if err := db.InsertMessage(msg); err != nil {
			return fmt.Errorf("insert %s: %w", msg.MessageID, err)
		}
	}

	m1, _ := db.GetByMessageID("msg1@test")
	m2, _ := db.GetByMessageID("msg2@test")
	m3, _ := db.GetByMessageID("msg3@test")

	db.AddTag(m1.ID, "inbox")
	db.AddTag(m1.ID, "unread")
	db.AddTag(m2.ID, "inbox")
	db.AddTag(m3.ID, "inbox")
	db.AddTag(m3.ID, "flagged")

	fmt.Printf("  email.db: %d messages at %s\n", len(msgs), path) // encgrep:allow filename, not PII
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
