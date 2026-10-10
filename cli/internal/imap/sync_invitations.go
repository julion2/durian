package imap

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	goimap "github.com/emersion/go-imap"
	"github.com/julion2/durian/cli/internal/invitationsync"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

// fillInvitations catches up on the invitations of mail synced before they
// were kept (see invitationsync). Of each message it fetches the structure and
// the calendar part only, not the whole mail with its attachments.
func (s *Syncer) fillInvitations() {
	ctx, cancel := context.WithTimeout(context.Background(), invitationsync.Budget)
	defer cancel()
	selected := ""
	stored, err := invitationsync.Fill(ctx, s.store, s.accountName(), store.ByUID,
		func(_ context.Context, m store.MissingInvitation) (string, error) {
			if m.Mailbox != selected {
				if _, err := s.client.SelectMailbox(m.Mailbox); err != nil {
					return "", err
				}
				selected = m.Mailbox
			}
			bs, err := s.client.FetchBodyStructure(m.UID)
			if err != nil {
				return "", err
			}
			return fetchCalendarPart(bs, func(path []int) ([]byte, error) {
				var raw bytes.Buffer
				err := s.client.FetchBodySection(m.UID, path, &raw)
				return raw.Bytes(), err
			})
		})
	if err != nil {
		slog.Debug("Invitation catch-up stopped", "module", "SYNC", "err", err)
		return
	}
	if stored > 0 {
		fmt.Fprintf(s.output, "  Kept %d earlier invitations\n", stored)
	}
}

// calendarPart is a calendar leaf of a BODYSTRUCTURE: its IMAP section path
// and what decoding it needs.
type calendarPart struct {
	path              []int
	encoding, charset string
}

// fetchCalendarPart returns the iCalendar part the parser would keep from
// the whole message: the first that decodes to something within
// durianmail.MaxCalendarBytes, inline parts before attached ones.
func fetchCalendarPart(bs *goimap.BodyStructure, fetch func(path []int) ([]byte, error)) (string, error) {
	for _, part := range calendarParts(bs) {
		raw, err := fetch(part.path)
		if err != nil {
			return "", err
		}
		if ics := durianmail.DecodeCalendar(raw, part.encoding, part.charset); ics != "" {
			return ics, nil
		}
	}
	return "", nil
}

// calendarParts lists a message's calendar parts in the order the parser
// prefers them (see durianmail.Parser): inline ones, then attached ones, each
// in document order. Like the parser it looks into nested multiparts but not
// into attached messages.
func calendarParts(bs *goimap.BodyStructure) []calendarPart {
	var inline, attached []calendarPart
	add := func(leaf *goimap.BodyStructure, path []int, isAttached bool) {
		part := calendarPart{path: path, encoding: leaf.Encoding, charset: leaf.Params["charset"]}
		if isAttached {
			attached = append(attached, part)
		} else {
			inline = append(inline, part)
		}
	}
	if bs == nil {
		return nil
	}
	if !strings.EqualFold(bs.MIMEType, "multipart") {
		// A single-part message is section 1, and its only candidate.
		if isCalendarPart(bs) {
			add(bs, []int{1}, false)
		}
		return inline
	}
	var walk func(parts []*goimap.BodyStructure, prefix []int)
	walk = func(parts []*goimap.BodyStructure, prefix []int) {
		for i, part := range parts {
			path := append(append([]int(nil), prefix...), i+1)
			if strings.EqualFold(part.MIMEType, "multipart") {
				walk(part.Parts, path)
				continue
			}
			if isCalendarPart(part) {
				// the parser's attachment test for a part of a multipart
				isAttached := strings.EqualFold(part.Disposition, "attachment") ||
					(part.DispositionParams["filename"] != "" && !strings.EqualFold(part.MIMEType, "text"))
				add(part, path, isAttached)
			}
		}
	}
	walk(bs.Parts, nil)
	return append(inline, attached...)
}

func isCalendarPart(bs *goimap.BodyStructure) bool {
	t := strings.ToLower(bs.MIMEType + "/" + bs.MIMESubType)
	return t == "text/calendar" || t == "application/ics"
}
