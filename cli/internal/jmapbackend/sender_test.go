package jmapbackend

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestSenderCreatesStructuredEmailAndSubmits(t *testing.T) {
	s := newTestJMAPServer(t)
	var submitted bool
	var createdDraft map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			createdDraft = args["create"].(map[string]interface{})["e0"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		case "EmailSubmission/set":
			submitted = true
			// onSuccessUpdateEmail produces this second, implicit response on
			// real servers such as Stalwart.
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{"updated": map[string]interface{}{"draft-1": nil}}, "0"}}
			create := args["create"].(map[string]interface{})["s0"].(map[string]interface{})
			if create["emailId"] != "draft-1" || create["identityId"] != "identity-1" {
				t.Errorf("submission create = %#v", create)
			}
			updates := args["onSuccessUpdateEmail"].(map[string]interface{})["#s0"].(map[string]interface{})
			if updates["mailboxIds/sent-id"] != true {
				t.Errorf("onSuccessUpdateEmail = %#v", updates)
			}
			return map[string]interface{}{"accountId": "a1", "oldState": "sub1", "newState": "sub2", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	b := s.backend(t)
	sender := &Sender{b: b}
	message := &mailsend.Message{
		From: "Me <me@example.test>", To: []string{"you@example.test"}, BCC: []string{"hidden@example.test"},
		Subject: "JMAP test", Body: "<b>hello</b>", IsHTML: true, MessageID: "<jmap-send@example.test>",
		InReplyTo: "<parent@example.test>", References: "<root@example.test> <parent@example.test>",
		Attachments: []mailsend.Attachment{{Filename: "note.txt", MIMEType: "text/plain", Data: []byte("attachment")}},
	}
	if err := sender.Send(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if !submitted {
		t.Fatal("EmailSubmission/set was not called")
	}
	if string(s.uploaded) != "attachment" {
		t.Fatalf("uploaded attachment = %q", s.uploaded)
	}
	if createdDraft["subject"] != "JMAP test" || createdDraft["messageId"].([]interface{})[0] != "jmap-send@example.test" {
		t.Fatalf("structured draft headers = %#v", createdDraft)
	}
	if createdDraft["bodyValues"].(map[string]interface{})["body"].(map[string]interface{})["value"] != "<b>hello</b>" {
		t.Fatalf("structured body = %#v", createdDraft["bodyValues"])
	}
	structure := createdDraft["bodyStructure"].(map[string]interface{})
	parts := structure["subParts"].([]interface{})
	if structure["type"] != "multipart/mixed" || len(parts) != 2 || parts[1].(map[string]interface{})["blobId"] != "uploaded-blob" {
		t.Fatalf("body structure = %#v", structure)
	}
	if createdDraft["bcc"].([]interface{})[0].(map[string]interface{})["email"] != "hidden@example.test" {
		t.Fatalf("structured Bcc = %#v", createdDraft["bcc"])
	}
}

func TestClassifySendErrorTreatsLocalJMAPPreconditionsAsPermanent(t *testing.T) {
	for _, err := range []error{errNoDraftsMailbox, errSubmissionUnavailable, errNoSubmissionIdentity} {
		t.Run(err.Error(), func(t *testing.T) {
			classified := classifySendError(errors.Join(errors.New("context"), err))
			var sendErr *mailsend.Error
			if !errors.As(classified, &sendErr) || sendErr.Kind != mailsend.KindPermanent || !errors.Is(classified, err) {
				t.Fatalf("classifySendError(%v) = %#v", err, classified)
			}
		})
	}
}

func TestClassifySendError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind mailsend.Kind
	}{
		{name: "HTTP client error", err: &statusError{Status: http.StatusUnauthorized}, kind: mailsend.KindPermanent},
		{name: "HTTP rate limit", err: &statusError{Status: http.StatusTooManyRequests}, kind: mailsend.KindTransient},
		{name: "HTTP server error", err: &statusError{Status: http.StatusServiceUnavailable}, kind: mailsend.KindTransient},
		{name: "permanent method error", err: &methodError{Type: "invalidArguments"}, kind: mailsend.KindPermanent},
		{name: "server method error", err: &methodError{Type: "serverFail"}, kind: mailsend.KindTransient},
		{name: "partial server method error", err: &methodError{Type: "serverPartialFail"}, kind: mailsend.KindTransient},
		{name: "method rate limit", err: &methodError{Type: "rateLimit"}, kind: mailsend.KindTransient},
		{name: "network error", err: errors.New("connection reset"), kind: mailsend.KindNetwork},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classified := classifySendError(fmt.Errorf("send submission: %w", tt.err))
			var sendErr *mailsend.Error
			if !errors.As(classified, &sendErr) || sendErr.Kind != tt.kind {
				t.Fatalf("classifySendError(%v) = %#v, want kind %v", tt.err, classified, tt.kind)
			}
			if !errors.Is(classified, tt.err) {
				t.Fatalf("classified error does not wrap original %v", tt.err)
			}
		})
	}
}

func TestSenderRejectsHeaderInjection(t *testing.T) {
	s := newTestJMAPServer(t)
	sender := &Sender{b: s.backend(t)}
	err := sender.Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, BCC: []string{"x@example.test\r\nX-Evil: yes"},
		Subject: "test", Body: "body",
	})
	if err == nil || !strings.Contains(err.Error(), "CR or LF") {
		t.Fatalf("Send() error = %v", err)
	}
}

// A failed submission must not leave the created copy in Drafts: the outbox
// retries, so every attempt would otherwise add another draft.
func TestSenderDestroysCreatedDraftWhenSubmissionFails(t *testing.T) {
	s := newTestJMAPServer(t)
	var destroyed []string
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; creating {
				return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
			}
			for _, id := range args["destroy"].([]interface{}) {
				destroyed = append(destroyed, id.(string))
			}
			return map[string]interface{}{"accountId": "a1", "oldState": "s2", "newState": "s3", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
		case "EmailSubmission/set":
			return map[string]interface{}{"accountId": "a1", "oldState": "sub1", "newState": "sub1", "created": map[string]interface{}{}, "notCreated": map[string]interface{}{
				"s0": map[string]interface{}{"type": "forbiddenFrom", "description": "identity not allowed"},
			}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	sender := &Sender{b: s.backend(t)}
	err := sender.Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if err == nil {
		t.Fatal("Send returned nil for a rejected submission")
	}
	if len(destroyed) != 1 || destroyed[0] != "draft-1" {
		t.Fatalf("destroyed = %v, want [draft-1]: the imported draft leaked", destroyed)
	}
}

// Without a Sent mailbox the message would stay in Drafts flagged $draft while
// SavesSentCopy() suppresses the local copy — a send with no record anywhere.
func TestSenderCreatesSentMailboxWhenMissing(t *testing.T) {
	s := newTestJMAPServer(t)
	mailboxes := []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "role": "inbox", "isSubscribed": true},
		{"id": "drafts-id", "name": "Drafts", "role": "drafts", "isSubscribed": true},
	}
	created := false
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": mailboxes}
		case "Mailbox/set":
			create := args["create"].(map[string]interface{})["sent"].(map[string]interface{})
			if create["role"] != "sent" {
				t.Errorf("Mailbox/set create = %#v, want role sent", create)
			}
			created = true
			mailboxes = append(mailboxes, map[string]interface{}{"id": "sent-id", "name": "Sent", "role": "sent", "isSubscribed": true})
			return map[string]interface{}{"accountId": "a1", "oldState": "mb1", "newState": "mb2", "created": map[string]interface{}{"sent": map[string]string{"id": "sent-id"}}, "notCreated": map[string]interface{}{}}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		case "EmailSubmission/set":
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{"updated": map[string]interface{}{"draft-1": nil}}, "0"}}
			updates, ok := args["onSuccessUpdateEmail"].(map[string]interface{})["#s0"].(map[string]interface{})
			if !ok {
				t.Fatal("onSuccessUpdateEmail missing: message would stay in Drafts")
			}
			if updates["mailboxIds/sent-id"] != true || updates["mailboxIds/drafts-id"] != nil {
				t.Errorf("onSuccessUpdateEmail = %#v", updates)
			}
			if _, hasDraftKeyword := updates["keywords/$draft"]; !hasDraftKeyword {
				t.Error("$draft keyword is not cleared")
			}
			return map[string]interface{}{"accountId": "a1", "oldState": "sub1", "newState": "sub2", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	sender := &Sender{b: s.backend(t)}
	if err := sender.Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	}); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("Sent mailbox was not created")
	}
}
