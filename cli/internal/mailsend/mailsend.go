// Package mailsend defines a provider-neutral outgoing message and the Sender
// seam implemented by SMTP, Gmail, JMAP, and Microsoft Graph. The outbox and
// CLI send command hand each account's Sender either structured fields for a
// normal message or canonical MIME that must be submitted unchanged.
package mailsend

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Message is a provider-neutral outgoing email. It carries the structured
// fields every backend needs; each Sender turns it into its provider's wire
// form unless RawMIME supplies the canonical wire message.
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
	// RawMIME, when set, is the canonical RFC 5322 message to submit unchanged.
	// Envelope recipients remain in To/CC/BCC. Normal compose leaves this nil.
	RawMIME []byte
}

// IsReactionEmoji reports whether emoji is in the fixed send vocabulary.
func IsReactionEmoji(emoji string) bool {
	switch emoji {
	case "👍", "❤️", "😂", "😮", "😢":
		return true
	default:
		return false
	}
}

// ReplySubject applies normal reply subject handling without stacking Re:.
func ReplySubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if len(subject) >= 3 && strings.EqualFold(subject[:3], "re:") {
		return subject
	}
	return "Re: " + subject
}

// ReactionReferences appends target to an existing References chain once.
func ReactionReferences(references, target string) (string, error) {
	ids := strings.Fields(references)
	target, err := messageID(target)
	if err != nil {
		return "", err
	}
	normalized := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		id, err = messageID(id)
		if err != nil {
			return "", err
		}
		if id != target {
			normalized = append(normalized, id)
		}
	}
	return strings.Join(append(normalized, target), " "), nil
}

// BuildReaction constructs the canonical single-part RFC 9078 message.
func BuildReaction(msg *Message, sentAt time.Time) ([]byte, error) {
	if msg == nil || !IsReactionEmoji(msg.Body) {
		return nil, fmt.Errorf("reaction emoji is not allowed")
	}
	if len(msg.To) != 1 || len(msg.CC) != 0 || len(msg.BCC) != 0 {
		return nil, fmt.Errorf("reaction requires exactly one To recipient")
	}
	from, err := safeAddress(msg.From)
	if err != nil {
		return nil, fmt.Errorf("invalid From: %w", err)
	}
	to, err := safeAddress(msg.To[0])
	if err != nil {
		return nil, fmt.Errorf("invalid To: %w", err)
	}
	messageIDValue, err := messageID(msg.MessageID)
	if err != nil {
		return nil, fmt.Errorf("invalid Message-ID: %w", err)
	}
	target, err := messageID(msg.InReplyTo)
	if err != nil {
		return nil, fmt.Errorf("invalid In-Reply-To: %w", err)
	}
	references, err := ReactionReferences(msg.References, target)
	if err != nil {
		return nil, fmt.Errorf("invalid References: %w", err)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return nil, fmt.Errorf("invalid Subject: contains CR or LF")
	}

	var out bytes.Buffer
	fmt.Fprintf(&out, "From: %s\r\n", from)
	fmt.Fprintf(&out, "To: %s\r\n", to)
	fmt.Fprintf(&out, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", msg.Subject))
	fmt.Fprintf(&out, "Date: %s\r\n", sentAt.Format(time.RFC1123Z))
	fmt.Fprintf(&out, "Message-ID: %s\r\n", messageIDValue)
	fmt.Fprintf(&out, "In-Reply-To: %s\r\n", target)
	fmt.Fprintf(&out, "References: %s\r\n", references)
	out.WriteString("MIME-Version: 1.0\r\n")
	out.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	out.WriteString("Content-Disposition: reaction\r\n")
	out.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	out.WriteString(base64.StdEncoding.EncodeToString([]byte(msg.Body + "\r\n")))
	out.WriteString("\r\n")
	return out.Bytes(), nil
}

func safeAddress(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("contains CR or LF")
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return "", err
	}
	return address.String(), nil
}

func messageID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") || strings.ContainsAny(value, " \t") {
		return "", fmt.Errorf("malformed message id")
	}
	value = strings.Trim(value, "<>")
	if value == "" || strings.ContainsAny(value, "<>") {
		return "", fmt.Errorf("malformed message id")
	}
	return "<" + value + ">", nil
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
