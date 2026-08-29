package imap

import (
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

// StoreMessageFromContent builds the content-derived part of a store.Message
// from a parsed RFC822 message. Callers set the transport-specific remainder
// (mailbox, flags, account, UID or RemoteRef) on the returned value.
//
// Both ingest paths share this deliberately. The legacy syncer and the
// provider-neutral engine each used to spell the field list out for
// themselves, so a field added to one silently stayed missing from the other
// — Bcc reached the engine path first and the legacy path not at all, which is
// invisible until someone runs an account that has not opted into the engine.
// One mapper means a field can only be present for both paths or neither, and
// a single test can hold that line.
func StoreMessageFromContent(messageID string, content *durianmail.MailContent, dateUnix, createdAt int64) *store.Message {
	return &store.Message{
		MessageID: messageID,
		Subject:   content.Subject,
		FromAddr:  content.From,
		ToAddrs:   content.To,
		CCAddrs:   content.CC,
		BCCAddrs:  content.BCC,
		InReplyTo: content.InReplyTo,
		Refs:      content.References,
		BodyText:  content.Body,
		BodyHTML:  content.HTML,
		Date:      dateUnix,
		CreatedAt: createdAt,
	}
}
