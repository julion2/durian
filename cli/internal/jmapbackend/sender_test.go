package jmapbackend

import (
	"strings"
	"testing"

	"github.com/julion2/durian/cli/internal/mailsend"
)

func TestSenderImportsAndSubmits(t *testing.T) {
	s := newTestJMAPServer(t)
	var submitted bool
	s.handler = func(method string, args map[string]interface{}) interface{} {
		switch method {
		case "Mailbox/get":
			return map[string]interface{}{"state": "mb1", "list": testMailboxes()}
		case "Identity/get":
			return map[string]interface{}{"accountId": "a1", "state": "i1", "list": []interface{}{map[string]string{"id": "identity-1", "email": "me@example.test"}}}
		case "Email/import":
			return map[string]interface{}{"accountId": "a1", "oldState": "s1", "newState": "s2", "created": map[string]interface{}{"0": map[string]string{"id": "draft-1"}}, "notCreated": map[string]interface{}{}}
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
		From: "me@example.test", To: []string{"you@example.test"}, BCC: []string{"hidden@example.test"},
		Subject: "JMAP test", Body: "hello", MessageID: "jmap-send@example.test",
	}
	if err := sender.Send(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if !submitted {
		t.Fatal("EmailSubmission/set was not called")
	}
	if !strings.Contains(string(s.uploaded), "Bcc: hidden@example.test") || !strings.Contains(string(s.uploaded), "Subject: JMAP test") {
		t.Fatalf("uploaded MIME missing headers:\n%s", s.uploaded)
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
