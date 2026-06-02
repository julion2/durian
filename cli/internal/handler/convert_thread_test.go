package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/store"
)

// --- Helpers ---

// seedThreadMessage inserts a single message and returns its DB record.
func seedThreadMessage(t *testing.T, db *store.DB, msg *store.Message) *store.Message {
	t.Helper()
	if err := db.InsertMessage(msg); err != nil {
		t.Fatalf("insert %s: %v", msg.MessageID, err)
	}
	got, err := db.GetByMessageID(msg.MessageID)
	if err != nil || got == nil {
		t.Fatalf("get %s: %v", msg.MessageID, err)
	}
	return got
}

// --- Multi-message ordering ---

func TestConvertThread_OrdersNewestFirst(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	// Insert in random date order to verify sorting
	seedThreadMessage(t, db, &store.Message{
		MessageID: "middle@test", Subject: "Hello", FromAddr: "a@example.com",
		Date: now - 1800, CreatedAt: now, BodyText: "middle", Mailbox: "INBOX",
	})
	seedThreadMessage(t, db, &store.Message{
		MessageID: "oldest@test", Subject: "Re: Hello", FromAddr: "b@example.com",
		InReplyTo: "<middle@test>", Refs: "<middle@test>",
		Date: now - 3600, CreatedAt: now, BodyText: "oldest", Mailbox: "INBOX",
	})
	seedThreadMessage(t, db, &store.Message{
		MessageID: "newest@test", Subject: "Re: Hello", FromAddr: "c@example.com",
		InReplyTo: "<middle@test>", Refs: "<middle@test>",
		Date: now, CreatedAt: now, BodyText: "newest", Mailbox: "INBOX",
	})

	m, _ := db.GetByMessageID("middle@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	if !resp.OK {
		t.Fatalf("ShowThread failed: %s", resp.Error)
	}
	if len(resp.Thread.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(resp.Thread.Messages))
	}

	bodies := []string{resp.Thread.Messages[0].Body, resp.Thread.Messages[1].Body, resp.Thread.Messages[2].Body}
	if bodies[0] != "newest" || bodies[1] != "middle" || bodies[2] != "oldest" {
		t.Errorf("ordering wrong: got %v, want [newest, middle, oldest]", bodies)
	}

	// Verify timestamps are descending
	for i := 0; i < len(resp.Thread.Messages)-1; i++ {
		if resp.Thread.Messages[i].Timestamp < resp.Thread.Messages[i+1].Timestamp {
			t.Errorf("timestamps not descending at index %d", i)
		}
	}
}

// --- Subject inheritance ---

func TestConvertThread_SubjectFromFirstMessage(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	// First in iteration order — Subject "Original Topic"
	seedThreadMessage(t, db, &store.Message{
		MessageID: "first@test", Subject: "Original Topic",
		FromAddr: "a@example.com", Date: now - 3600, CreatedAt: now,
		BodyText: "first", Mailbox: "INBOX",
	})
	seedThreadMessage(t, db, &store.Message{
		MessageID: "reply@test", Subject: "Re: Original Topic",
		InReplyTo: "<first@test>", Refs: "<first@test>",
		FromAddr: "b@example.com", Date: now, CreatedAt: now,
		BodyText: "reply", Mailbox: "INBOX",
	})

	m, _ := db.GetByMessageID("first@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	if resp.Thread.Subject != "Original Topic" {
		t.Errorf("Subject = %q, want %q", resp.Thread.Subject, "Original Topic")
	}
}

// --- Field mapping ---

func TestConvertThread_AllFieldsMapped(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	seedThreadMessage(t, db, &store.Message{
		MessageID: "fields@test", Subject: "Field Test",
		FromAddr:  "alice@example.com",
		ToAddrs:   "bob@example.com",
		CCAddrs:   "carol@example.com, dave@example.com",
		InReplyTo: "<previous@test>",
		Refs:      "<previous@test> <other@test>",
		Date:      now, CreatedAt: now,
		BodyText: "plain body",
		BodyHTML: "<p>html body</p>",
		Mailbox:  "INBOX",
	})

	m, _ := db.GetByMessageID("fields@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	msg := resp.Thread.Messages[0]

	if msg.ID != "fields@test" {
		t.Errorf("ID = %q", msg.ID)
	}
	if msg.MessageID != "fields@test" {
		t.Errorf("MessageID = %q", msg.MessageID)
	}
	if msg.From != "alice@example.com" {
		t.Errorf("From = %q", msg.From)
	}
	if msg.To != "bob@example.com" {
		t.Errorf("To = %q", msg.To)
	}
	if msg.CC != "carol@example.com, dave@example.com" {
		t.Errorf("CC = %q", msg.CC)
	}
	if msg.InReplyTo != "<previous@test>" {
		t.Errorf("InReplyTo = %q", msg.InReplyTo)
	}
	if msg.References != "<previous@test> <other@test>" {
		t.Errorf("References = %q", msg.References)
	}
	if msg.Body != "plain body" {
		t.Errorf("Body = %q", msg.Body)
	}
	if !strings.Contains(msg.HTML, "html body") {
		t.Errorf("HTML = %q, want html body", msg.HTML)
	}
	if msg.Timestamp != now {
		t.Errorf("Timestamp = %d, want %d", msg.Timestamp, now)
	}

	// Date should be RFC1123Z format
	if _, err := time.Parse(time.RFC1123Z, msg.Date); err != nil {
		t.Errorf("Date %q is not RFC1123Z: %v", msg.Date, err)
	}
}

// --- Quote stripping is applied ---

func TestConvertThread_StripsQuotedHTML(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	htmlWithQuote := `<p>My reply text</p><div class="gmail_quote">Original message that should be stripped</div>`
	seedThreadMessage(t, db, &store.Message{
		MessageID: "quoted@test", Subject: "Quoted",
		FromAddr: "a@example.com", Date: now, CreatedAt: now,
		BodyText: "My reply text",
		BodyHTML: htmlWithQuote,
		Mailbox:  "INBOX",
	})

	m, _ := db.GetByMessageID("quoted@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	html := resp.Thread.Messages[0].HTML

	if !strings.Contains(html, "My reply text") {
		t.Error("user reply should be preserved in HTML")
	}
	if strings.Contains(html, "should be stripped") {
		t.Errorf("quoted content should be stripped, got: %s", html)
	}
}

// --- Tags per message ---

func TestConvertThread_TagsPerMessage(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	m1 := seedThreadMessage(t, db, &store.Message{
		MessageID: "tag1@test", Subject: "Tagged",
		FromAddr: "a@example.com", Date: now - 3600, CreatedAt: now,
		BodyText: "first", Mailbox: "INBOX",
	})
	m2 := seedThreadMessage(t, db, &store.Message{
		MessageID: "tag2@test", Subject: "Re: Tagged",
		InReplyTo: "<tag1@test>", Refs: "<tag1@test>",
		FromAddr: "b@example.com", Date: now, CreatedAt: now,
		BodyText: "second", Mailbox: "INBOX",
	})

	db.AddTag(m1.ID, "inbox")
	db.AddTag(m1.ID, "important")
	db.AddTag(m2.ID, "inbox")
	db.AddTag(m2.ID, "flagged")

	h := New(db, nil)
	resp := h.ShowThread(m1.ThreadID)

	tagsByMsg := make(map[string][]string)
	for _, msg := range resp.Thread.Messages {
		tagsByMsg[msg.ID] = msg.Tags
	}

	hasTag := func(tags []string, want string) bool {
		for _, t := range tags {
			if t == want {
				return true
			}
		}
		return false
	}

	if !hasTag(tagsByMsg["tag1@test"], "important") {
		t.Errorf("tag1 missing 'important', got %v", tagsByMsg["tag1@test"])
	}
	if hasTag(tagsByMsg["tag1@test"], "flagged") {
		t.Errorf("tag1 should not have 'flagged', got %v", tagsByMsg["tag1@test"])
	}
	if !hasTag(tagsByMsg["tag2@test"], "flagged") {
		t.Errorf("tag2 missing 'flagged', got %v", tagsByMsg["tag2@test"])
	}
}

// --- Attachments per message ---

func TestConvertThread_AttachmentsPerMessage(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	m1 := seedThreadMessage(t, db, &store.Message{
		MessageID: "att1@test", Subject: "With attachment",
		FromAddr: "a@example.com", Date: now - 3600, CreatedAt: now,
		BodyText: "see attached", Mailbox: "INBOX",
	})
	seedThreadMessage(t, db, &store.Message{
		MessageID: "att2@test", Subject: "Re: With attachment",
		InReplyTo: "<att1@test>", Refs: "<att1@test>",
		FromAddr: "b@example.com", Date: now, CreatedAt: now,
		BodyText: "thanks", Mailbox: "INBOX",
	})

	// m1 gets two attachments, m2 gets none
	db.InsertAttachment(&store.Attachment{
		MessageDBID: m1.ID, PartID: 1,
		Filename: "doc.pdf", ContentType: "application/pdf",
		Size: 1024, Disposition: "attachment", ContentID: "<doc1>",
	})
	db.InsertAttachment(&store.Attachment{
		MessageDBID: m1.ID, PartID: 2,
		Filename: "image.png", ContentType: "image/png",
		Size: 2048, Disposition: "inline", ContentID: "<img1>",
	})

	h := New(db, nil)
	resp := h.ShowThread(m1.ThreadID)

	attsByMsg := make(map[string]int)
	for _, msg := range resp.Thread.Messages {
		attsByMsg[msg.ID] = len(msg.Attachments)
	}

	if attsByMsg["att1@test"] != 2 {
		t.Errorf("att1 should have 2 attachments, got %d", attsByMsg["att1@test"])
	}
	if attsByMsg["att2@test"] != 0 {
		t.Errorf("att2 should have 0 attachments, got %d", attsByMsg["att2@test"])
	}

	// Verify attachment field mapping
	for _, msg := range resp.Thread.Messages {
		if msg.ID == "att1@test" {
			var pdf, png bool
			for _, a := range msg.Attachments {
				if a.Filename == "doc.pdf" {
					pdf = true
					if a.ContentType != "application/pdf" {
						t.Errorf("PDF ContentType = %q", a.ContentType)
					}
					if a.Size != 1024 {
						t.Errorf("PDF Size = %d, want 1024", a.Size)
					}
					if a.Disposition != "attachment" {
						t.Errorf("PDF Disposition = %q", a.Disposition)
					}
					if a.PartID != 1 {
						t.Errorf("PDF PartID = %d", a.PartID)
					}
				}
				if a.Filename == "image.png" {
					png = true
					if a.Disposition != "inline" {
						t.Errorf("PNG Disposition = %q, want inline", a.Disposition)
					}
					if a.ContentID != "<img1>" {
						t.Errorf("PNG ContentID = %q", a.ContentID)
					}
				}
			}
			if !pdf || !png {
				t.Error("missing PDF or PNG attachment")
			}
		}
	}
}

// --- Single-message thread ---

func TestConvertThread_SingleMessage(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	seedThreadMessage(t, db, &store.Message{
		MessageID: "lonely@test", Subject: "Solo",
		FromAddr: "a@example.com", Date: now, CreatedAt: now,
		BodyText: "only one", Mailbox: "INBOX",
	})

	m, _ := db.GetByMessageID("lonely@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	if len(resp.Thread.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(resp.Thread.Messages))
	}
	if resp.Thread.Subject != "Solo" {
		t.Errorf("Subject = %q", resp.Thread.Subject)
	}
}

// --- Mixed HTML and plaintext-only messages ---

func TestConvertThread_MixedHTMLAndPlaintext(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	seedThreadMessage(t, db, &store.Message{
		MessageID: "html@test", Subject: "Mixed",
		FromAddr: "a@example.com", Date: now - 3600, CreatedAt: now,
		BodyText: "html version",
		BodyHTML: "<p>html version</p>",
		Mailbox:  "INBOX",
	})
	seedThreadMessage(t, db, &store.Message{
		MessageID: "plain@test", Subject: "Re: Mixed",
		InReplyTo: "<html@test>", Refs: "<html@test>",
		FromAddr: "b@example.com", Date: now, CreatedAt: now,
		BodyText: "plain only, no HTML",
		Mailbox:  "INBOX",
	})

	m, _ := db.GetByMessageID("html@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	if len(resp.Thread.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(resp.Thread.Messages))
	}

	for _, msg := range resp.Thread.Messages {
		switch msg.ID {
		case "html@test":
			if msg.HTML == "" {
				t.Error("html@test should have HTML")
			}
		case "plain@test":
			if msg.HTML != "" {
				t.Errorf("plain@test should have empty HTML, got %q", msg.HTML)
			}
			if msg.Body != "plain only, no HTML" {
				t.Errorf("plain@test Body = %q", msg.Body)
			}
		}
	}
}

// --- Light mode (used for search enrichment) ---

func TestConvertThread_LightOmitsHTMLAndReplyHeaders(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	seedThreadMessage(t, db, &store.Message{
		MessageID: "light@test", Subject: "Light",
		FromAddr:  "a@example.com",
		InReplyTo: "<prev@test>",
		Refs:      "<prev@test>",
		Date:      now, CreatedAt: now,
		BodyText: "plain body",
		BodyHTML: "<p>html body</p>",
		Mailbox:  "INBOX",
	})

	m, _ := db.GetByMessageID("light@test")
	msgs, _ := db.GetByThread(m.ThreadID)
	h := New(db, nil)

	thread := h.convertThread(m.ThreadID, msgs, true, nil, nil)
	if len(thread.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(thread.Messages))
	}
	msg := thread.Messages[0]

	// Light mode: Body is preserved
	if msg.Body != "plain body" {
		t.Errorf("Body = %q, want %q", msg.Body, "plain body")
	}
	// Light mode: HTML is omitted
	if msg.HTML != "" {
		t.Errorf("HTML should be empty in light mode, got %q", msg.HTML)
	}
	// Light mode: reply headers are omitted
	if msg.InReplyTo != "" {
		t.Errorf("InReplyTo should be empty in light mode, got %q", msg.InReplyTo)
	}
	if msg.References != "" {
		t.Errorf("References should be empty in light mode, got %q", msg.References)
	}
}

func TestConvertThread_LightKeepsTagsAndAttachments(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	m := seedThreadMessage(t, db, &store.Message{
		MessageID: "lightatts@test", Subject: "With stuff",
		FromAddr: "a@example.com", Date: now, CreatedAt: now,
		BodyText: "hello", Mailbox: "INBOX",
	})
	db.AddTag(m.ID, "inbox")
	db.InsertAttachment(&store.Attachment{
		MessageDBID: m.ID, PartID: 1,
		Filename: "doc.pdf", ContentType: "application/pdf",
		Size: 1024, Disposition: "attachment",
	})

	msgs, _ := db.GetByThread(m.ThreadID)
	h := New(db, nil)
	thread := h.convertThread(m.ThreadID, msgs, true, nil, nil)

	if len(thread.Messages[0].Tags) == 0 {
		t.Error("light mode should still populate tags")
	}
	if len(thread.Messages[0].Attachments) != 1 {
		t.Errorf("light mode should still populate attachments, got %d",
			len(thread.Messages[0].Attachments))
	}
}

// --- ThreadID propagation ---

func TestConvertThread_ThreadIDPropagated(t *testing.T) {
	db := newTestStore(t)
	now := time.Now().Unix()

	seedThreadMessage(t, db, &store.Message{
		MessageID: "tid@test", Subject: "Thread ID test",
		FromAddr: "a@example.com", Date: now, CreatedAt: now,
		BodyText: "x", Mailbox: "INBOX",
	})

	m, _ := db.GetByMessageID("tid@test")
	h := New(db, nil)
	resp := h.ShowThread(m.ThreadID)
	if resp.Thread.ThreadID != m.ThreadID {
		t.Errorf("ThreadID = %q, want %q", resp.Thread.ThreadID, m.ThreadID)
	}
}
