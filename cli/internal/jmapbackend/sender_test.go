package jmapbackend

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
		Attachments: []mailsend.Attachment{{Filename: "note.txt", MIMEType: "text/plain; charset=utf-8", Data: []byte("attachment")}},
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
	keywords := createdDraft["keywords"].(map[string]interface{})
	if keywords["$draft"] != true || keywords["$seen"] != true {
		t.Fatalf("structured draft keywords = %#v", keywords)
	}
	structure := createdDraft["bodyStructure"].(map[string]interface{})
	parts := structure["subParts"].([]interface{})
	attachment := parts[1].(map[string]interface{})
	if structure["type"] != "multipart/mixed" || len(parts) != 2 || attachment["blobId"] != "uploaded-blob" ||
		attachment["type"] != "text/plain" || attachment["charset"] != "utf-8" {
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

func TestStructuredEmailParsesRFCMessageIDLists(t *testing.T) {
	draft, err := structuredEmail(&mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "test", Body: "body",
		MessageID:  "(created here) <message(comment)@example.test> (tail)",
		InReplyTo:  "(first)\r\n\t<a@example.test> (nested (comment)) <\"b c\"@example.test>",
		References: "(root) <root@example.test>\t(parent) <parent@example.test>",
	})
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string][]string{
		"messageId":  {"message@example.test"},
		"inReplyTo":  {"a@example.test", `"b c"@example.test`},
		"references": {"root@example.test", "parent@example.test"},
	} {
		if got := draft[field]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", field, got, want)
		}
	}
}

func TestMessageIDsRejectInvalidRFCGrammar(t *testing.T) {
	for _, value := range []string{
		"<a..b@example.test>",
		`<a@"quoted-domain">`,
		"<[literal]@example.test>",
	} {
		t.Run(value, func(t *testing.T) {
			if ids, err := messageIDs(value); err == nil {
				t.Fatalf("messageIDs(%q) = %#v, want error", value, ids)
			}
		})
	}
	if ids, err := messageIDs(`<"quoted local"@[127.0.0.1]>`); err != nil || !reflect.DeepEqual(ids, []string{`"quoted local"@[127.0.0.1]`}) {
		t.Fatalf("valid quoted/literal Message-ID = %#v, %v", ids, err)
	}
}

func TestSenderRejectsInvalidAttachmentTypeAsPermanent(t *testing.T) {
	s := newTestJMAPServer(t)
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "test", Body: "body",
		Attachments: []mailsend.Attachment{{Filename: "bad.txt", MIMEType: "text/plain; charset", Data: []byte("x")}},
	})
	if err == nil || mailsend.Classify(err) != mailsend.KindPermanent || !errors.Is(err, errInvalidAttachmentType) {
		t.Fatalf("Send() error = %#v, want permanent invalid attachment type", err)
	}
	if len(s.uploaded) != 0 {
		t.Fatal("attachment was uploaded before all content types were validated")
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

func TestSenderRepairsSentFilingAfterImplicitUpdateFails(t *testing.T) {
	s := newTestJMAPServer(t)
	var repaired map[string]interface{}
	var destroyed bool
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
			if update, ok := args["update"]; ok {
				repaired = update.(map[string]interface{})["draft-1"].(map[string]interface{})
				return map[string]interface{}{"accountId": "a1", "oldState": "s2", "newState": "s3", "updated": map[string]interface{}{"draft-1": nil}, "notUpdated": map[string]interface{}{}}
			}
			destroyed = true
			return map[string]interface{}{"accountId": "a1", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
		case "EmailSubmission/set":
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{
				"updated": map[string]interface{}{},
				"notUpdated": map[string]interface{}{"draft-1": map[string]interface{}{
					"type": "serverFail", "description": "temporary filing failure",
				}},
			}, "0"}}
			return map[string]interface{}{"accountId": "a1", "oldState": "sub1", "newState": "sub2", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}

	sender := &Sender{b: s.backend(t)}
	if err := sender.Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	}); err != nil {
		t.Fatalf("Send() returned an error after the server accepted delivery: %v", err)
	}
	if repaired == nil || repaired["mailboxIds/sent-id"] != true || repaired["mailboxIds/drafts-id"] != nil || repaired["keywords/$draft"] != nil {
		t.Fatalf("repair patch = %#v", repaired)
	}
	if destroyed {
		t.Fatal("submitted email was destroyed after a filing-only failure")
	}
}

func TestSenderRepairsSentFilingAfterUncorrelatedImplicitResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		before []interface{}
		after  []interface{}
	}{
		{
			name: "mismatched call ID",
			after: []interface{}{[]interface{}{
				"Email/set", map[string]interface{}{"updated": map[string]interface{}{"draft-1": nil}}, "wrong",
			}},
		},
		{
			name: "missing call ID",
			after: []interface{}{[]interface{}{
				"Email/set", map[string]interface{}{"updated": map[string]interface{}{"draft-1": nil}},
			}},
		},
		{name: "non-tuple before acceptance", before: []interface{}{map[string]interface{}{"malformed": true}}},
		{name: "invalid response name before acceptance", before: []interface{}{[]interface{}{12, map[string]interface{}{}, "0"}}},
		{name: "malformed implicit result before acceptance", before: []interface{}{[]interface{}{"Email/set", "not an object", "0"}}},
		{name: "null primary result before acceptance", before: []interface{}{[]interface{}{"EmailSubmission/set", nil, "0"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			var repaired map[string]interface{}
			var destroyed bool
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
					if update, ok := args["update"]; ok {
						repaired = update.(map[string]interface{})["draft-1"].(map[string]interface{})
						return map[string]interface{}{"accountId": "a1", "oldState": "s2", "newState": "s3", "updated": map[string]interface{}{"draft-1": nil}, "notUpdated": map[string]interface{}{}}
					}
					destroyed = true
					return map[string]interface{}{"accountId": "a1", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
				case "EmailSubmission/set":
					s.before = test.before
					s.extra = test.after
					return map[string]interface{}{"accountId": "a1", "oldState": "sub1", "newState": "sub2", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
				}
				t.Fatalf("unexpected method %s", method)
				return nil
			}

			err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
				From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
			})
			if err != nil {
				t.Fatalf("Send() returned an error after accepted delivery: %v", err)
			}
			if repaired == nil || repaired["mailboxIds/sent-id"] != true {
				t.Fatalf("repair patch = %#v", repaired)
			}
			if destroyed {
				t.Fatal("submitted email was destroyed after an uncorrelated implicit response")
			}
		})
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
