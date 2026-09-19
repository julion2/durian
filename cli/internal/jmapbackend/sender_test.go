package jmapbackend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/redact"
)

func TestSenderCreatesStructuredEmailAndSubmits(t *testing.T) {
	s := newTestJMAPServer(t)
	var submitted bool
	var createdDraft map[string]interface{}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			createdDraft = args["create"].(map[string]interface{})["e0"].(map[string]interface{})
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		case "EmailSubmission/set":
			submitted = true
			// onSuccessUpdateEmail produces this second, implicit response on
			// real servers such as Stalwart.
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{
				"accountId": "a1", "updated": map[string]interface{}{
					"draft-1": map[string]interface{}{"keywords": map[string]bool{"$seen": true}},
				},
			}, "0"}}
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

func TestClassifySendErrorTreatsJMAPOutcomeStatesDistinctly(t *testing.T) {
	for _, test := range []struct {
		err  error
		kind mailsend.Kind
	}{
		{errEmailCreationOutcomeUnknown, mailsend.KindAmbiguous},
		{errSubmissionOutcomeUnknown, mailsend.KindAmbiguous},
		{errSentFilingFailed, mailsend.KindDeliveredWithWarning},
		{errSubmissionUnavailable, mailsend.KindPermanent},
		{errNoSubmissionIdentity, mailsend.KindPermanent},
		{errAmbiguousSubmissionIdentity, mailsend.KindPermanent},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			classified := classifySendError(errors.Join(errors.New("context"), test.err))
			var sendErr *mailsend.Error
			if !errors.As(classified, &sendErr) || sendErr.Kind != test.kind || !errors.Is(classified, test.err) {
				t.Fatalf("classifySendError(%v) = %#v", test.err, classified)
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
		{name: "server unavailable", err: &methodError{Type: "serverUnavailable"}, kind: mailsend.KindTransient},
		{name: "method rate limit", err: &methodError{Type: "rateLimit"}, kind: mailsend.KindTransient},
		{name: "network error", err: &url.Error{Op: "POST", URL: "https://example.test", Err: io.EOF}, kind: mailsend.KindNetwork},
		{name: "local provider limit", err: fmt.Errorf("attachment: %w", errJMAPLocalPermanent), kind: mailsend.KindPermanent},
		{name: "untyped deterministic error", err: errors.New("invalid local input"), kind: mailsend.KindPermanent},
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

func TestSenderTreatsProviderUploadLimitAsPermanent(t *testing.T) {
	s := newTestJMAPServer(t)
	s.limits = map[string]interface{}{"maxSizeUpload": 1}
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "test", Body: "body",
		Attachments: []mailsend.Attachment{{Filename: "large.txt", MIMEType: "text/plain", Data: []byte("xx")}},
	})
	if err == nil || mailsend.Classify(err) != mailsend.KindPermanent || !errors.Is(err, errJMAPLocalPermanent) {
		t.Fatalf("Send() error = %#v, want permanent provider limit", err)
	}
	if len(s.uploaded) != 0 {
		t.Fatal("oversized attachment was uploaded")
	}
}

func TestSenderStopsAfterAmbiguousEmailCreation(t *testing.T) {
	s := newTestJMAPServer(t)
	var created bool
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			created = true
			s.dropAPIResponse = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if !created || err == nil || !errors.Is(err, errEmailCreationOutcomeUnknown) || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("created=%v Send() error=%#v, want ambiguous unknown creation", created, err)
	}
}

func TestSenderRejectsIncompleteCreatedEmail(t *testing.T) {
	s := newTestJMAPServer(t)
	var submitted bool
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			s.rawResponses = true
			return map[string]interface{}{
				"accountId": "a1", "oldState": "s1", "newState": "s2",
				"created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{},
			}
		case "EmailSubmission/set":
			submitted = true
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if err == nil || !errors.Is(err, errEmailCreationOutcomeUnknown) || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("Send() error=%#v, want ambiguous unknown creation", err)
	}
	if submitted {
		t.Fatal("submitted an Email whose created response omitted required properties")
	}
}

func TestValidateCreatedEmailRequiresAllServerProperties(t *testing.T) {
	size := uint64(1)
	valid := createdEmail{ID: "email-1", BlobID: "blob-1", ThreadID: "thread-1", Size: &size}
	for _, test := range []struct {
		name   string
		mutate func(*createdEmail)
	}{
		{name: "id", mutate: func(email *createdEmail) { email.ID = "" }},
		{name: "blobId", mutate: func(email *createdEmail) { email.BlobID = "" }},
		{name: "threadId", mutate: func(email *createdEmail) { email.ThreadID = "" }},
		{name: "size", mutate: func(email *createdEmail) { email.Size = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			email := valid
			test.mutate(&email)
			if err := validateCreatedEmail("Email/import", email); err == nil {
				t.Fatalf("validateCreatedEmail(%+v) succeeded", email)
			}
		})
	}
}

func TestRawSenderStopsAfterAmbiguousEmailImport(t *testing.T) {
	s := newTestJMAPServer(t)
	var imported bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/import":
			imported = true
			email := args["emails"].(map[string]interface{})["0"].(map[string]interface{})
			if email["keywords"].(map[string]interface{})["$draft"] != true {
				t.Fatal("temporary raw Email was not marked $draft")
			}
			s.dropAPIResponse = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := s.backend(t).Send(t.Context(), []byte(testRaw))
	if !imported || err == nil || !errors.Is(err, errEmailCreationOutcomeUnknown) {
		t.Fatalf("imported=%v Send() error=%#v, want unknown creation", imported, err)
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
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
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
			s.rawResponses = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{}, "notCreated": map[string]interface{}{
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

func TestSenderAcceptsSubmissionAndImplicitUpdateWithoutOldState(t *testing.T) {
	s := newTestJMAPServer(t)
	var repaired bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; !creating {
				repaired = true
				t.Fatal("complete implicit Email/set outcome triggered a redundant repair")
			}
			return map[string]interface{}{
				"accountId": "a1", "oldState": "s1", "newState": "s2",
				"created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{},
			}
		case "EmailSubmission/set":
			s.rawResponses = true
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{
				"accountId": "a1", "newState": "s3", "updated": map[string]interface{}{"draft-1": nil}, "notUpdated": map[string]interface{}{},
			}, "0"}}
			return map[string]interface{}{
				"accountId": "a1", "newState": "sub2", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{},
			}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}

	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if err != nil {
		t.Fatalf("Send() rejected complete submission outcomes without oldState: %v", err)
	}
	if repaired {
		t.Fatal("implicit update without oldState was repaired despite its complete outcome")
	}
}

func TestSubmissionSetStatesAllowProviderOldStateOmission(t *testing.T) {
	state := "s2"
	for _, test := range []struct {
		name  string
		state setResponseState
		ok    bool
	}{
		{name: "both omitted"},
		{name: "only new", state: setResponseState{NewState: &state}, ok: true},
		{name: "both present", state: setResponseState{OldState: json.RawMessage(`"s1"`), NewState: &state}, ok: true},
		{name: "only old", state: setResponseState{OldState: json.RawMessage(`"s1"`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateSubmissionSetResponseState("EmailSubmission/set", test.state)
			if (err == nil) != test.ok {
				t.Fatalf("validateSubmissionSetResponseState() error = %v, want success %v", err, test.ok)
			}
		})
	}
}

func TestSenderPreservesDraftWhenSubmissionResponseIsLost(t *testing.T) {
	s := newTestJMAPServer(t)
	mailboxes := []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "role": "inbox", "isSubscribed": true},
		{"id": "sent-id", "name": "Sent", "role": "sent", "isSubscribed": true},
	}
	var accepted, destroyed bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; creating {
				draft := args["create"].(map[string]interface{})["e0"].(map[string]interface{})
				if draft["mailboxIds"].(map[string]interface{})["sent-id"] != true || draft["keywords"].(map[string]interface{})["$draft"] != true {
					t.Fatalf("no-Drafts temporary Email = %#v", draft)
				}
				return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
			}
			destroyed = true
			return map[string]interface{}{"accountId": "a1", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
		case "EmailSubmission/set":
			accepted = true
			s.dropAPIResponse = true
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if !accepted || err == nil || !errors.Is(err, errSubmissionOutcomeUnknown) || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("accepted=%v Send() error=%#v, want ambiguous unknown outcome", accepted, err)
	}
	if destroyed {
		t.Fatal("source Email was destroyed after an ambiguous accepted submission")
	}
}

func TestSenderTreatsServerPartialFailAsUnknownOutcome(t *testing.T) {
	s := newTestJMAPServer(t)
	var destroyed bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; creating {
				return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
			}
			destroyed = true
			return map[string]interface{}{"accountId": "a1", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
		case "EmailSubmission/set":
			return testMethodResponse{name: "error", value: map[string]interface{}{"type": "serverPartialFail"}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if err == nil || !errors.Is(err, errSubmissionOutcomeUnknown) || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("Send() error=%#v, want ambiguous unknown outcome", err)
	}
	if destroyed {
		t.Fatal("source Email was destroyed after serverPartialFail")
	}
}

func TestSenderTreatsContradictorySubmissionResponsesAsUnknown(t *testing.T) {
	s := newTestJMAPServer(t)
	var accepted, destroyed bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; creating {
				return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
			}
			destroyed = true
			return map[string]interface{}{"accountId": "a1", "destroyed": []string{"draft-1"}, "notDestroyed": map[string]interface{}{}}
		case "EmailSubmission/set":
			accepted = true
			s.before = []interface{}{[]interface{}{"error", map[string]interface{}{"type": "serverFail"}, "0"}}
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if !accepted || err == nil || !errors.Is(err, errSubmissionOutcomeUnknown) || mailsend.Classify(err) != mailsend.KindAmbiguous {
		t.Fatalf("accepted=%v Send() error=%#v, want ambiguous unknown outcome", accepted, err)
	}
	if destroyed {
		t.Fatal("source Email was destroyed after contradictory submission responses")
	}
}

func TestSenderRepairsSentFilingAfterImplicitUpdateFails(t *testing.T) {
	s := newTestJMAPServer(t)
	var repaired map[string]interface{}
	var destroyed bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
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
				"accountId": "a1", "updated": map[string]interface{}{},
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

func TestSenderReportsAcceptedDeliveryWhenSentRepairFails(t *testing.T) {
	const (
		implicitSecret = "private implicit detail"
		repairSecret   = "private repair detail"
	)
	var logOutput bytes.Buffer
	priorLogger := slog.Default()
	slog.SetDefault(slog.New(redact.Wrap(slog.NewTextHandler(&logOutput, nil))))
	t.Cleanup(func() { slog.SetDefault(priorLogger) })

	s := newTestJMAPServer(t)
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			if _, creating := args["create"]; creating {
				return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
			}
			return map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{}, "notUpdated": map[string]interface{}{
				"draft-1": map[string]interface{}{"type": "serverFail", "description": repairSecret},
			}}
		case "EmailSubmission/set":
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{}, "notUpdated": map[string]interface{}{
				"draft-1": map[string]interface{}{"type": "serverFail", "description": implicitSecret},
			}}, "0"}}
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	})
	if err == nil || !errors.Is(err, errSentFilingFailed) || mailsend.Classify(err) != mailsend.KindDeliveredWithWarning {
		t.Fatalf("Send() error=%#v, want delivered-with-warning outcome", err)
	}
	slog.Error("outbox send failed", "err", err)
	if got := logOutput.String(); strings.Contains(got, implicitSecret) || strings.Contains(got, repairSecret) || !strings.Contains(got, redact.Placeholder) {
		t.Fatalf("redacted filing logs = %q", got)
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
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			var repaired map[string]interface{}
			var destroyed bool
			s.handler = func(method string, args map[string]interface{}) interface{} {
				switch method {
				case "Mailbox/get":
					return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": testMailboxes(), "notFound": nil}
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
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
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
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"draft-1": nil}}, "0"}}
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

func TestSenderUsesSentMailboxWhenDraftsRoleIsMissing(t *testing.T) {
	s := newTestJMAPServer(t)
	mailboxes := []map[string]interface{}{
		{"id": "inbox-id", "name": "Inbox", "role": "inbox", "isSubscribed": true},
		{"id": "sent-id", "name": "Sent", "role": "sent", "isSubscribed": true},
	}
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"accountId": "a1", "state": "mb1", "list": mailboxes, "notFound": nil}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/set":
			create := args["create"].(map[string]interface{})["e0"].(map[string]interface{})
			if mailboxes := create["mailboxIds"].(map[string]interface{}); mailboxes["sent-id"] != true {
				t.Fatalf("draft mailboxIds = %#v, want sent-id", mailboxes)
			}
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"e0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
		case "EmailSubmission/set":
			update := args["onSuccessUpdateEmail"].(map[string]interface{})["#s0"].(map[string]interface{})
			if len(update) != 2 || update["mailboxIds/sent-id"] != true || update["keywords/$draft"] != nil {
				t.Fatalf("submission update = %#v", update)
			}
			s.extra = []interface{}{[]interface{}{"Email/set", map[string]interface{}{"accountId": "a1", "updated": map[string]interface{}{"draft-1": nil}}, "0"}}
			return map[string]interface{}{"accountId": "a1", "created": map[string]interface{}{"s0": map[string]string{"id": "submission-1"}}, "notCreated": map[string]interface{}{}}
		}
		t.Fatalf("unexpected method %s", method)
		return nil
	}
	if err := (&Sender{b: s.backend(t)}).Send(t.Context(), &mailsend.Message{
		From: "me@example.test", To: []string{"you@example.test"}, Subject: "x", Body: "y",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityGetRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{name: "missing account", mutate: func(r map[string]interface{}) { delete(r, "accountId") }},
		{name: "wrong account", mutate: func(r map[string]interface{}) { r["accountId"] = "other" }},
		{name: "missing state", mutate: func(r map[string]interface{}) { delete(r, "state") }},
		{name: "missing list", mutate: func(r map[string]interface{}) { delete(r, "list") }},
		{name: "missing not found", mutate: func(r map[string]interface{}) { delete(r, "notFound") }},
		{name: "non-empty not found", mutate: func(r map[string]interface{}) { r["notFound"] = []string{"missing"} }},
		{name: "invalid id", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{map[string]interface{}{"id": "bad=", "email": "me@example.test"}}
		}},
		{name: "duplicate id", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{
				map[string]interface{}{"id": "identity-1", "email": "me@example.test"},
				map[string]interface{}{"id": "identity-1", "email": "other@example.test"},
			}
		}},
		{name: "missing email", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{map[string]interface{}{"id": "identity-1"}}
		}},
		{name: "invalid email", mutate: func(r map[string]interface{}) {
			r["list"] = []interface{}{map[string]interface{}{"id": "identity-1", "email": "Me <me@example.test>"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestJMAPServer(t)
			s.rawResponses = true
			s.handler = func(method string, _ map[string]interface{}) interface{} {
				if method != "Identity/get" {
					t.Fatalf("unexpected method %s", method)
				}
				response := map[string]interface{}{
					"accountId": "a1", "state": "i1", "list": []interface{}{
						map[string]interface{}{"id": "identity-1", "email": "me@example.test"},
					}, "notFound": []interface{}{},
				}
				tt.mutate(response)
				return response
			}
			b := s.backend(t)
			if err := b.ensure(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := b.identityID(t.Context()); err == nil {
				t.Fatal("malformed Identity/get response accepted")
			}
		})
	}
}

func TestIdentityGetAcceptsEmptyState(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Identity/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "", "list": []interface{}{
				map[string]interface{}{"id": "identity-1", "email": "me@example.test"},
			}, "notFound": []interface{}{},
		}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if id, err := b.identityID(t.Context()); err != nil || id != "identity-1" {
		t.Fatalf("identityID() = %q, %v", id, err)
	}
}

func TestIdentityGetSelectsMatchingWildcard(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Identity/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "i1", "list": []interface{}{
				map[string]interface{}{"id": "other", "email": "other@elsewhere.test"},
				map[string]interface{}{"id": "wildcard", "email": "*@example.test"},
			},
		}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := b.identityID(t.Context()); err != nil || got != "wildcard" {
		t.Fatalf("identityID() = %q, %v, want wildcard", got, err)
	}
}

func TestIdentityGetPrefersExactCaseCorrectLocalPart(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Identity/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "i1", "list": []interface{}{
				map[string]interface{}{"id": "wildcard", "email": "*@example.test"},
				map[string]interface{}{"id": "wrong-case", "email": "Me@example.test"},
				map[string]interface{}{"id": "exact", "email": "me@EXAMPLE.TEST"},
			},
		}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := b.identityID(t.Context()); err != nil || got != "exact" {
		t.Fatalf("identityID() = %q, %v, want exact", got, err)
	}
}

func TestIdentityGetRejectsAmbiguousExactIdentities(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Identity/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "i1", "list": []interface{}{
				map[string]interface{}{"id": "first", "email": "me@example.test"},
				map[string]interface{}{"id": "second", "email": "me@EXAMPLE.TEST"},
			},
		}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.identityID(t.Context()); !errors.Is(err, errAmbiguousSubmissionIdentity) {
		t.Fatalf("identityID() error = %v, want ambiguous identity", err)
	}
}

func TestIdentityGetRejectsUnmatchedIdentities(t *testing.T) {
	s := newTestJMAPServer(t)
	s.handler = func(method string, _ map[string]interface{}) interface{} {
		if method != "Identity/get" {
			t.Fatalf("unexpected method %s", method)
		}
		return map[string]interface{}{
			"accountId": "a1", "state": "i1", "list": []interface{}{
				map[string]interface{}{"id": "other", "email": "other@elsewhere.test"},
			},
		}
	}
	b := s.backend(t)
	if err := b.ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.identityID(t.Context()); !errors.Is(err, errNoSubmissionIdentity) {
		t.Fatalf("identityID() error = %v, want errNoSubmissionIdentity", err)
	}
}
