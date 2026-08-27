package smtp

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/julion2/durian/cli/internal/mailsend"
)

// Sender adapts the SMTP client to the provider-neutral mailsend.Sender seam.
type Sender struct {
	host          string
	port          int
	auth          Auth
	savesSentCopy bool
}

// NewSender returns a mailsend.Sender that delivers over SMTP.
func NewSender(host string, port int, auth Auth, savesSentCopy bool) *Sender {
	return &Sender{host: host, port: port, auth: auth, savesSentCopy: savesSentCopy}
}

// Send renders the neutral message as MIME and delivers it, tagging any failure
// with the mailsend.Kind the outbox needs to decide retry vs poison.
func (s *Sender) Send(_ context.Context, m *mailsend.Message) error {
	if err := NewClient(s.host, s.port, s.auth).Send(FromMessage(m)); err != nil {
		return &mailsend.Error{Kind: classifySMTPError(err), Err: err}
	}
	return nil
}

// SavesSentCopy reports whether this SMTP provider files submissions in Sent.
func (s *Sender) SavesSentCopy() bool { return s.savesSentCopy }

// FromMessage converts a provider-neutral message into an smtp.Message — for the
// SMTP transport and for building a MIME copy to append to the Sent folder. The
// preset Message-ID is carried through so the sent mail, the local Sent row and
// the Sent-folder copy all share one id.
func FromMessage(m *mailsend.Message) *Message {
	return &Message{
		GeneratedMessageID: m.MessageID,
		From:               m.From,
		To:                 m.To,
		CC:                 m.CC,
		BCC:                m.BCC,
		Subject:            m.Subject,
		Body:               m.Body,
		IsHTML:             m.IsHTML,
		InReplyTo:          m.InReplyTo,
		References:         m.References,
		Attachments:        toSMTPAttachments(m.Attachments),
		RawMIME:            m.RawMIME,
	}
}

func toSMTPAttachments(atts []mailsend.Attachment) []Attachment {
	out := make([]Attachment, len(atts))
	for i, a := range atts {
		out[i] = Attachment{Filename: a.Filename, Data: a.Data, MIMEType: a.MIMEType}
	}
	return out
}

// classifySMTPError maps an SMTP send failure to a mailsend.Kind: network
// errors retry silently, 5xx poisons, everything else retries with an attempt.
func classifySMTPError(err error) mailsend.Kind {
	if isNetworkErr(err) {
		return mailsend.KindNetwork
	}
	if se := ParseSMTPError(err); se != nil && se.IsPermanent() {
		return mailsend.KindPermanent
	}
	return mailsend.KindTransient
}

// isNetworkErr reports a transient transport failure (offline, timeout, refused,
// DNS) as opposed to a server rejection.
func isNetworkErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to connect") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout")
}
