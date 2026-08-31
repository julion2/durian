//go:build jmapintegration

package jmapbackend

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestLiveJMAPRoundTrip(t *testing.T) {
	sessionURL := os.Getenv("DURIAN_JMAP_TEST_SESSION_URL")
	username := os.Getenv("DURIAN_JMAP_TEST_USERNAME")
	password := os.Getenv("DURIAN_JMAP_TEST_PASSWORD")
	auth := liveJMAPAuth()
	if sessionURL == "" || username == "" || password == "" {
		t.Skip("live JMAP credentials not configured")
	}

	original := getCredential
	getCredential = func(_, _ string) (string, error) { return password, nil }
	t.Cleanup(func() { getCredential = original })

	b := newLiveBackend(t, sessionURL, auth, "JMAP Integration", username)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := b.FetchFolders(ctx); err != nil {
		t.Fatalf("discover folders: %v", err)
	}
	inboxID := b.mailboxIDForTag("inbox")
	if inboxID == "" {
		t.Fatal("server has no inbox mailbox")
	}

	marker := time.Now().UTC().Format("20060102T150405.000000000")
	messageID := "durian-jmap-integration-" + marker + "@example.test"
	raw := []byte("From: " + username + "\r\nTo: " + username + "\r\nSubject: Durian JMAP integration " + marker + "\r\nMessage-ID: <" + messageID + ">\r\nDate: Wed, 26 Aug 2026 01:05:00 +0000\r\n\r\nround trip\r\n")
	ref, err := b.Append(ctx, inboxID, backend.Flags{}, raw)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	t.Cleanup(func() { destroyLiveEmail(b, ref.ID) })

	initial, err := b.FetchMessages(ctx, allMailStream, nil, 500)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if !containsMessage(initial.Messages, messageID, func(message backend.Message) bool {
		return strings.Contains(string(message.Raw), "round trip") && containsString(message.Labels, "inbox")
	}) {
		t.Fatalf("initial sync did not return imported message %s", messageID)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	watchResult := make(chan error, 1)
	changed := make(chan struct{}, 1)
	go func() {
		watchResult <- b.Watch(watchCtx, "", func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
	}()
	time.Sleep(300 * time.Millisecond)
	if err := b.ApplyFlags(ctx, ref, backend.Flags{Seen: true, Flagged: true}, backend.Flags{}); err != nil {
		t.Fatalf("apply flags: %v", err)
	}
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("JMAP EventSource did not announce Email state change")
	}
	stopWatch()
	if err := <-watchResult; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("watch: %v", err)
	}

	delta, err := b.FetchMessages(ctx, allMailStream, initial.Cursor, 500)
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if !containsMessage(delta.Messages, messageID, func(message backend.Message) bool {
		return message.Flags.Seen && message.Flags.Flagged
	}) {
		t.Fatalf("incremental sync did not return changed flags for %s", messageID)
	}
	if err := b.ApplyTagMutation(ctx, ref, "unread", true); err != nil {
		t.Fatalf("apply unread tag mutation: %v", err)
	}
	unreadDelta, err := b.FetchMessages(ctx, allMailStream, delta.Cursor, 500)
	if err != nil {
		t.Fatalf("explicit tag mutation sync: %v", err)
	}
	if !containsMessage(unreadDelta.Messages, messageID, func(message backend.Message) bool {
		return !message.Flags.Seen && message.Flags.Flagged
	}) {
		t.Fatalf("incremental sync did not return explicit unread mutation for %s", messageID)
	}
	customLabel := "Durian JMAP, native/label"
	if err := b.ApplyLabels(ctx, ref, []string{"archive", customLabel}, []string{"inbox"}); err != nil {
		t.Fatalf("archive labels: %v", err)
	}
	labelDelta, err := b.FetchMessages(ctx, allMailStream, unreadDelta.Cursor, 500)
	if err != nil {
		t.Fatalf("mailbox-membership sync: %v", err)
	}
	if !containsMessage(labelDelta.Messages, messageID, func(message backend.Message) bool {
		return containsString(message.Labels, "archive") && containsString(message.Labels, customLabel) &&
			!containsString(message.Labels, "inbox")
	}) {
		t.Fatalf("incremental sync did not return mailbox and custom keyword labels for %s", messageID)
	}
	if err := b.ApplyLabels(ctx, ref, nil, []string{customLabel}); err != nil {
		t.Fatalf("remove custom keyword label: %v", err)
	}
	removedLabelDelta, err := b.FetchMessages(ctx, allMailStream, labelDelta.Cursor, 500)
	if err != nil {
		t.Fatalf("custom keyword removal sync: %v", err)
	}
	if !containsMessage(removedLabelDelta.Messages, messageID, func(message backend.Message) bool {
		return containsString(message.Labels, "archive") && !containsString(message.Labels, customLabel)
	}) {
		t.Fatalf("incremental sync did not return custom keyword removal for %s", messageID)
	}

	submissionMessageID := "durian-jmap-submission-" + marker + "@example.test"
	sender := &Sender{b: b}
	if err := sender.Send(ctx, &mailsend.Message{
		From: username, To: []string{username}, Subject: "Durian JMAP submission " + marker,
		Body: "submitted through JMAP", MessageID: submissionMessageID,
	}); err != nil {
		t.Fatalf("submission: %v", err)
	}

	submissionDelta, err := b.FetchMessages(ctx, allMailStream, removedLabelDelta.Cursor, 500)
	if err != nil {
		t.Fatalf("submission delta: %v", err)
	}
	foundSent := false
	for _, message := range submissionDelta.Messages {
		if strings.Trim(message.MessageID, "<>") != submissionMessageID {
			continue
		}
		t.Cleanup(func() { destroyLiveEmail(b, message.Ref.ID) })
		if containsString(message.Labels, "sent") {
			foundSent = true
		}
	}
	if !foundSent {
		t.Fatalf("submission %s did not appear in the sent mailbox", submissionMessageID)
	}
}

func TestLiveJMAPHTMLThreading(t *testing.T) {
	sessionURL := os.Getenv("DURIAN_JMAP_TEST_SESSION_URL")
	username := os.Getenv("DURIAN_JMAP_TEST_USERNAME")
	password := os.Getenv("DURIAN_JMAP_TEST_PASSWORD")
	auth := liveJMAPAuth()
	if sessionURL == "" || username == "" || password == "" {
		t.Skip("live JMAP credentials not configured")
	}

	original := getCredential
	getCredential = func(_, _ string) (string, error) { return password, nil }
	t.Cleanup(func() { getCredential = original })

	b := newLiveBackend(t, sessionURL, auth, "JMAP HTML Threading", username)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := b.FetchFolders(ctx); err != nil {
		t.Fatalf("discover folders: %v", err)
	}
	initial, err := b.FetchMessages(ctx, allMailStream, nil, 500)
	if err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	marker := time.Now().UTC().Format("20060102T150405.000000000")
	parentID := "durian-jmap-html-parent-" + marker + "@example.test"
	replyID := "durian-jmap-html-reply-" + marker + "@example.test"
	sender := &Sender{b: b}
	if err := sender.Send(ctx, &mailsend.Message{
		From: username, To: []string{username}, Subject: "Durian JMAP HTML threading " + marker,
		Body: "<p>HTML body with <strong>formatting</strong>.</p>", IsHTML: true, MessageID: parentID,
	}); err != nil {
		t.Fatalf("HTML parent submission: %v", err)
	}
	if err := sender.Send(ctx, &mailsend.Message{
		From: username, To: []string{username}, Subject: "Re: Durian JMAP HTML threading " + marker,
		Body: "threaded reply", MessageID: replyID, InReplyTo: "<" + parentID + ">", References: "<" + parentID + ">",
	}); err != nil {
		t.Fatalf("thread reply submission: %v", err)
	}

	delta, err := b.FetchMessages(ctx, allMailStream, initial.Cursor, 500)
	if err != nil {
		t.Fatalf("submission delta: %v", err)
	}
	refs := make(map[string]string)
	for _, message := range delta.Messages {
		messageID := strings.Trim(message.MessageID, "<>")
		if messageID != parentID && messageID != replyID {
			continue
		}
		t.Cleanup(func() { destroyLiveEmail(b, message.Ref.ID) })
		refs[messageID] = message.Ref.ID
		raw := strings.ToLower(string(message.Raw))
		if messageID == parentID && (!strings.Contains(raw, "content-type: text/html") || !strings.Contains(raw, "<strong>formatting</strong>")) {
			t.Fatal("HTML parent was not preserved as text/html")
		}
		parentIDLower := strings.ToLower(parentID)
		if messageID == replyID && (!strings.Contains(raw, "in-reply-to: <"+parentIDLower+">") || !strings.Contains(raw, "references: <"+parentIDLower+">")) {
			t.Fatal("reply threading headers were not preserved")
		}
	}
	if refs[parentID] == "" || refs[replyID] == "" {
		t.Fatalf("submission delta did not contain both thread messages: parent=%t reply=%t", refs[parentID] != "", refs[replyID] != "")
	}
	objects, missing, _, err := b.getEmailObjects(ctx, []string{refs[parentID], refs[replyID]})
	if err != nil {
		t.Fatalf("fetch JMAP thread metadata: %v", err)
	}
	if len(missing) != 0 || len(objects) != 2 || objects[0].ThreadID == "" || objects[0].ThreadID != objects[1].ThreadID {
		t.Fatalf("server did not group parent and reply into one JMAP thread: objects=%v missing=%v", objects, missing)
	}
}

func TestLiveJMAPReplacementRecovery(t *testing.T) {
	sessionURL := os.Getenv("DURIAN_JMAP_TEST_SESSION_URL")
	username := os.Getenv("DURIAN_JMAP_TEST_USERNAME")
	password := os.Getenv("DURIAN_JMAP_TEST_PASSWORD")
	expiredState := os.Getenv("DURIAN_JMAP_TEST_EXPIRED_STATE")
	if sessionURL == "" || username == "" || password == "" {
		t.Skip("live JMAP credentials not configured")
	}
	if expiredState == "" {
		t.Skip("live JMAP expired state not configured")
	}

	original := getCredential
	getCredential = func(_, _ string) (string, error) { return password, nil }
	t.Cleanup(func() { getCredential = original })
	b := newLiveBackend(t, sessionURL, liveJMAPAuth(), "JMAP Replacement Recovery", username)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := b.FetchFolders(ctx); err != nil {
		t.Fatalf("discover folders: %v", err)
	}
	cursor := encodeCursor(jmapCursor{EmailState: expiredState})
	present := make(map[string]struct{})
	for page := 0; page < 10_000; page++ {
		result, err := b.FetchMessages(ctx, allMailStream, cursor, 1)
		if err != nil {
			t.Fatalf("replacement page %d: %v", page+1, err)
		}
		if !result.FullSnapshot {
			t.Fatalf("replacement page %d was returned as a delta", page+1)
		}
		for _, ref := range result.Present {
			if _, duplicate := present[ref.ID]; duplicate {
				t.Fatalf("replacement returned duplicate ref %q", ref.ID)
			}
			present[ref.ID] = struct{}{}
		}
		cursor = result.Cursor
		if result.HasMore {
			continue
		}
		state := decodeCursor(cursor)
		if state.EmailState == "" || state.Replacement {
			t.Fatalf("replacement ended with invalid cursor %+v", state)
		}
		if len(present) == 0 {
			t.Fatal("replacement snapshot unexpectedly contained no messages")
		}
		return
	}
	t.Fatal("replacement snapshot did not finish within 10000 pages")
}

func TestLiveJMAPTwoAccountDelivery(t *testing.T) {
	sessionURL := os.Getenv("DURIAN_JMAP_TEST_SESSION_URL")
	senderUsername := os.Getenv("DURIAN_JMAP_TEST_USERNAME")
	senderPassword := os.Getenv("DURIAN_JMAP_TEST_PASSWORD")
	recipientUsername := os.Getenv("DURIAN_JMAP_TEST_RECIPIENT_USERNAME")
	recipientPassword := os.Getenv("DURIAN_JMAP_TEST_RECIPIENT_PASSWORD")
	auth := liveJMAPAuth()
	if sessionURL == "" || senderUsername == "" || senderPassword == "" || recipientUsername == "" || recipientPassword == "" {
		t.Skip("live two-account JMAP credentials not configured")
	}

	original := getCredential
	getCredential = func(_, account string) (string, error) {
		switch account {
		case senderUsername:
			return senderPassword, nil
		case recipientUsername:
			return recipientPassword, nil
		default:
			return "", errors.New("unexpected live JMAP account")
		}
	}
	t.Cleanup(func() { getCredential = original })

	senderBackend := newLiveBackend(t, sessionURL, auth, "JMAP Integration Sender", senderUsername)
	recipientBackend := newLiveBackend(t, sessionURL, auth, "JMAP Integration Recipient", recipientUsername)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for name, b := range map[string]*Backend{"sender": senderBackend, "recipient": recipientBackend} {
		if _, err := b.FetchFolders(ctx); err != nil {
			t.Fatalf("discover %s folders: %v", name, err)
		}
	}

	senderInitial, err := senderBackend.FetchMessages(ctx, allMailStream, nil, 500)
	if err != nil {
		t.Fatalf("initial sender sync: %v", err)
	}
	recipientInitial, err := recipientBackend.FetchMessages(ctx, allMailStream, nil, 500)
	if err != nil {
		t.Fatalf("initial recipient sync: %v", err)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	watchResult := make(chan error, 1)
	changed := make(chan struct{}, 1)
	go func() {
		watchResult <- recipientBackend.Watch(watchCtx, "", func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
	}()
	time.Sleep(300 * time.Millisecond)

	marker := time.Now().UTC().Format("20060102T150405.000000000")
	messageID := "durian-jmap-two-account-" + marker + "@example.test"
	attachmentData := []byte("Durian JMAP attachment " + marker)
	if err := (&Sender{b: senderBackend}).Send(ctx, &mailsend.Message{
		From: senderUsername, To: []string{senderUsername}, BCC: []string{recipientUsername},
		Subject: "Durian JMAP two-account " + marker, Body: "delivered between two JMAP accounts", MessageID: messageID,
		Attachments: []mailsend.Attachment{{Filename: "durian-jmap.txt", MIMEType: "text/plain; charset=utf-8", Data: attachmentData}},
	}); err != nil {
		t.Fatalf("two-account submission: %v", err)
	}

	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("recipient JMAP EventSource did not announce delivered email")
	}
	stopWatch()
	if err := <-watchResult; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("recipient watch: %v", err)
	}

	recipientDelta, err := recipientBackend.FetchMessages(ctx, allMailStream, recipientInitial.Cursor, 500)
	if err != nil {
		t.Fatalf("recipient incremental sync: %v", err)
	}
	if !containsMessage(recipientDelta.Messages, messageID, func(message backend.Message) bool {
		t.Cleanup(func() { destroyLiveEmail(recipientBackend, message.Ref.ID) })
		parsed, err := mail.ReadMessage(bytes.NewReader(message.Raw))
		if err != nil {
			t.Errorf("parse recipient MIME: %v", err)
			return false
		}
		content := durianmail.NewParser().Parse(parsed)
		if content.BCC != "" {
			t.Errorf("delivered recipient copy exposed Bcc header %q", content.BCC)
			return false
		}
		if !strings.Contains(content.To, senderUsername) || len(content.Attachments) != 1 || content.Attachments[0].Filename != "durian-jmap.txt" {
			t.Errorf("delivered recipient MIME metadata = To %q, attachments %#v", content.To, content.Attachments)
			return false
		}
		attachment, contentType, err := durianmail.ExtractAttachmentPart(message.Raw, 1)
		if err != nil || !bytes.Equal(attachment, attachmentData) || contentType != "text/plain" {
			t.Errorf("delivered attachment = %q, type %q, err %v", attachment, contentType, err)
			return false
		}
		return strings.Contains(content.Body, "delivered between two JMAP accounts") && containsString(message.Labels, "inbox")
	}) {
		t.Fatalf("recipient sync did not return delivered message %s in inbox", messageID)
	}

	senderDelta, err := senderBackend.FetchMessages(ctx, allMailStream, senderInitial.Cursor, 500)
	if err != nil {
		t.Fatalf("sender incremental sync: %v", err)
	}
	if !containsMessage(senderDelta.Messages, messageID, func(message backend.Message) bool {
		t.Cleanup(func() { destroyLiveEmail(senderBackend, message.Ref.ID) })
		parsed, err := mail.ReadMessage(bytes.NewReader(message.Raw))
		if err != nil {
			t.Errorf("parse sender MIME: %v", err)
			return false
		}
		content := durianmail.NewParser().Parse(parsed)
		return strings.Contains(content.BCC, recipientUsername) && message.Flags.Seen && containsString(message.Labels, "sent")
	}) {
		t.Fatalf("sender sync did not return submitted message %s in sent with Bcc preserved and seen", messageID)
	}
}

func liveJMAPAuth() string {
	if auth := os.Getenv("DURIAN_JMAP_TEST_AUTH"); auth != "" {
		return auth
	}
	return "password"
}

func newLiveBackend(t *testing.T, sessionURL, auth, name, username string) *Backend {
	t.Helper()
	account := &config.AccountConfig{
		Name: name, Email: username, SyncEngine: "jmap",
		Auth: &config.AuthConfig{Username: username},
		JMAP: &config.JMAPConfig{SessionURL: sessionURL, Auth: auth},
	}
	b, err := New(account)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func containsMessage(messages []backend.Message, messageID string, check func(backend.Message) bool) bool {
	for _, message := range messages {
		if strings.Trim(message.MessageID, "<>") == messageID && check(message) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func destroyLiveEmail(b *Backend, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result struct {
		NotDestroyed map[string]methodError `json:"notDestroyed"`
	}
	_ = b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/set", map[string]interface{}{
		"accountId": b.client.accountID,
		"destroy":   []string{id},
	}, &result)
}
