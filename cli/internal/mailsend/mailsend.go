// Package mailsend defines a provider-neutral outgoing message and the Sender
// seam that SMTP, Microsoft Graph and (later) Gmail implement. The outbox and
// the CLI send command build a Message and hand it to the account's Sender,
// which renders it the way its provider prefers — MIME for SMTP/Gmail, a Graph
// draft for Microsoft — without the caller knowing which.
package mailsend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Message is a provider-neutral outgoing email. It carries the structured
// fields every backend needs; each Sender turns it into its provider's wire
// form (an RFC822 MIME body, or a Graph draft with typed recipients).
type Message struct {
	// MessageID is the RFC822 Message-ID (with angle brackets) assigned before
	// send, so the transmitted mail, the local Sent row and any Sent-folder
	// append all share one id and dedupe on the next sync. Generate it with
	// GenerateMessageID.
	MessageID   string
	From        string // "Display Name <addr>" or a bare address
	To          []string
	CC          []string
	BCC         []string
	Subject     string
	Body        string
	IsHTML      bool
	InReplyTo   string // Message-ID being replied to (threading)
	References  string // space-separated thread Message-IDs
	Attachments []Attachment
}

// GenerateMessageID returns a fresh RFC822 Message-ID (with angle brackets)
// rooted at the sender address's domain.
func GenerateMessageID(from string) string {
	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at != -1 {
		d := strings.TrimRight(strings.TrimSpace(from[at+1:]), ">")
		if d != "" {
			domain = d
		}
	}
	return fmt.Sprintf("<%s@%s>", uuid.New().String(), domain)
}

// Attachment is one decoded file to attach.
type Attachment struct {
	Filename string
	MIMEType string
	Data     []byte
}

// Sender delivers a Message via one provider and reports whether that transport
// stores the submitted message in Sent itself.
type Sender interface {
	Send(ctx context.Context, msg *Message) error
	SavesSentCopy() bool
}

// DurableSender prepares a provider draft, reports the provider-exact RFC
// Message-ID for durable persistence, and only then performs the irreversible
// delivery operation. Implementations must not deliver when persist returns an
// error. Ordinary callers may continue to use Sender.Send.
type DurableSender interface {
	Sender
	SendAfterPersist(ctx context.Context, msg *Message, persist func(messageID string) error) error
}

// Kind classifies a send failure so the outbox's retry/poison policy stays
// provider-neutral.
type Kind int

const (
	// KindTransient — retry later, counting the attempt (4xx, unknown error).
	KindTransient Kind = iota
	// KindNetwork — offline/timeout; retry silently without counting an attempt.
	KindNetwork
	// KindPermanent — will never send (5xx, bad recipient); poison the item.
	KindPermanent
	// KindAmbiguous — the provider may have accepted the delivery. Keep the
	// durable claim until an operator verifies whether it was delivered.
	KindAmbiguous
	// KindDeliveredWithWarning — delivery definitely succeeded, but a
	// post-delivery provider action needs manual remediation.
	KindDeliveredWithWarning
)

// Error wraps a provider error with its Kind. Senders return this so callers
// branch on Classify without knowing the provider.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Classify returns the Kind of a send error, defaulting to KindTransient for an
// error a Sender did not tag.
func Classify(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindTransient
}
