//go:build jmapintegration

package jmapbackend

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestLiveJMAPRoundTrip(t *testing.T) {
	sessionURL := os.Getenv("DURIAN_JMAP_TEST_SESSION_URL")
	username := os.Getenv("DURIAN_JMAP_TEST_USERNAME")
	password := os.Getenv("DURIAN_JMAP_TEST_PASSWORD")
	if sessionURL == "" || username == "" || password == "" {
		t.Skip("live JMAP credentials not configured")
	}

	original := getCredential
	getCredential = func(_, _ string) (string, error) { return password, nil }
	t.Cleanup(func() { getCredential = original })

	account := &config.AccountConfig{
		Name: "JMAP Integration", Email: username, Alias: "jmap-integration", SyncEngine: "jmap",
		Auth: &config.AuthConfig{Username: username},
		JMAP: &config.JMAPConfig{SessionURL: sessionURL, Auth: "password"},
	}
	b, err := New(account)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

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
	if err := b.ApplyLabels(ctx, ref, []string{"archive"}, []string{"inbox"}); err != nil {
		t.Fatalf("archive labels: %v", err)
	}
	labelDelta, err := b.FetchMessages(ctx, allMailStream, delta.Cursor, 500)
	if err != nil {
		t.Fatalf("mailbox-membership sync: %v", err)
	}
	if !containsMessage(labelDelta.Messages, messageID, func(message backend.Message) bool {
		return containsString(message.Labels, "archive") && !containsString(message.Labels, "inbox")
	}) {
		t.Fatalf("incremental sync did not return changed mailbox membership for %s", messageID)
	}

	submissionMessageID := "durian-jmap-submission-" + marker + "@example.test"
	sender := &Sender{b: b}
	if err := sender.Send(ctx, &mailsend.Message{
		From: username, To: []string{username}, Subject: "Durian JMAP submission " + marker,
		Body: "submitted through JMAP", MessageID: submissionMessageID,
	}); err != nil {
		t.Fatalf("submission: %v", err)
	}

	submissionDelta, err := b.FetchMessages(ctx, allMailStream, labelDelta.Cursor, 500)
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
