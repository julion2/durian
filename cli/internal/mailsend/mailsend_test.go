package mailsend

import (
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	base := errors.New("boom")
	if got := Classify(base); got != KindTransient {
		t.Errorf("untagged error = %v, want KindTransient", got)
	}
	for _, kind := range []Kind{KindTransient, KindNetwork, KindPermanent} {
		wrapped := &Error{Kind: kind, Err: base}
		if got := Classify(wrapped); got != kind {
			t.Errorf("Classify(%v) = %v, want %v", kind, got, kind)
		}
		if !errors.Is(wrapped, base) {
			t.Errorf("Error should unwrap to the underlying cause")
		}
	}
}

func TestBuildReaction(t *testing.T) {
	msg := &Message{
		MessageID: "<reaction@example.com>", From: "Jane Doe <jane@example.com>",
		To: []string{"Bob <bob@example.net>"}, Subject: "Re: Meeting", Body: "👍",
		InReplyTo: "<target@example.net>", References: "<root@example.net>",
	}
	raw, err := BuildReaction(msg, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := "From: \"Jane Doe\" <jane@example.com>\r\n" +
		"To: \"Bob\" <bob@example.net>\r\n" +
		"Subject: Re: Meeting\r\n" +
		"Date: Thu, 27 Aug 2026 12:00:00 +0000\r\n" +
		"Message-ID: <reaction@example.com>\r\n" +
		"In-Reply-To: <target@example.net>\r\n" +
		"References: <root@example.net> <target@example.net>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Disposition: reaction\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"8J+RjQ0K\r\n"
	if string(raw) != want {
		t.Fatalf("reaction MIME differs from golden:\n got %q\nwant %q", raw, want)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.TrimSpace(strings.SplitN(string(raw), "\r\n\r\n", 2)[1])
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(body) != "👍\r\n" {
		t.Fatalf("decoded body = %q, err=%v", body, err)
	}
	if parsed.Header.Get("Content-Disposition") != "reaction" {
		t.Errorf("disposition = %q", parsed.Header.Get("Content-Disposition"))
	}
}

func TestBuildReactionRejectsInvalidInput(t *testing.T) {
	base := Message{MessageID: "<new@x>", From: "me@x.test", To: []string{"you@x.test"}, Subject: "Re: x", Body: "👍", InReplyTo: "<old@x>"}
	for name, mutate := range map[string]func(*Message){
		"emoji":        func(m *Message) { m.Body = "🔥" },
		"subject":      func(m *Message) { m.Subject = "x\r\nBcc: victim@x" },
		"recipient":    func(m *Message) { m.To[0] = "you@x.test\r\nBcc: victim@x" },
		"message id":   func(m *Message) { m.MessageID = "<new@x>\r\nBcc: victim@x" },
		"reply target": func(m *Message) { m.InReplyTo = "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			m.To = append([]string(nil), base.To...)
			mutate(&m)
			if _, err := BuildReaction(&m, time.Now()); err == nil {
				t.Fatal("BuildReaction accepted invalid input")
			}
		})
	}
}

func TestReplySubject(t *testing.T) {
	if got := ReplySubject("Meeting"); got != "Re: Meeting" {
		t.Errorf("ReplySubject = %q", got)
	}
	if got := ReplySubject("RE: Meeting"); got != "RE: Meeting" {
		t.Errorf("ReplySubject stacked prefix: %q", got)
	}
}

func TestGenerateMessageID(t *testing.T) {
	id := GenerateMessageID("Jane Doe <jane@example.com>")
	if !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("id = %q, want <uuid@example.com>", id)
	}
	a := GenerateMessageID("jane@example.com")
	b := GenerateMessageID("jane@example.com")
	if a == b {
		t.Error("two ids should be unique")
	}
	// No domain -> falls back to localhost, never empty.
	if got := GenerateMessageID("nobody"); !strings.HasSuffix(got, "@localhost>") {
		t.Errorf("id without domain = %q, want @localhost", got)
	}
}
