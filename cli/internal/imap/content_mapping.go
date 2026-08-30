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
// themselves, so nothing but care kept them in step: when Bcc was added, both
// copies happened to get it, but only the engine path had a test, and a
// regression on the legacy path would have stayed green. One mapper means a
// field can only be present for both paths or neither, and a single test
// holds that line for both.
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
