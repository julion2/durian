// Package syncidentity matches messages without Message-ID headers across an
// IMAP UIDVALIDITY reset.
package syncidentity

import (
	"bytes"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

type candidate struct {
	messageID     string
	uid           uint64
	rowID         int64
	ingestPending bool
}

type reservation struct {
	fingerprint [32]byte
	candidate   candidate
	current     bool
}

// Matcher consumes each recovery candidate at most once. Pre-reset candidate
// order is newest-first to mirror the IMAP fetch order; equal-content
// duplicates remain distinct rows rather than collapsing onto one key.
type Matcher struct {
	byFingerprint map[[32]byte][]candidate
	currentByID   map[string]reservation
	reservations  map[string]reservation
}

// New loads and fingerprints the synthetic rows in an account and mailbox.
// Rows already written for currentUIDValidity by a failed replacement are
// pinned to their exact provisional IDs instead of joining the pre-reset pool.
// The one-time O(N) decrypt is limited to UIDVALIDITY recovery.
func New(db *store.DB, account, mailbox string, currentUIDValidity uint32) (*Matcher, error) {
	messages, err := db.GetSyntheticMessagesForFolder(account, mailbox)
	if err != nil {
		return nil, fmt.Errorf("load synthetic message candidates: %w", err)
	}
	syntheticMessages := make([]*store.Message, 0, len(messages))
	messageIDs := make([]int64, 0, len(messages))
	for _, msg := range messages {
		if _, ok := durianmail.SyntheticMessageSequence(msg.MessageID); !ok {
			continue
		}
		syntheticMessages = append(syntheticMessages, msg)
		messageIDs = append(messageIDs, msg.ID)
	}
	attachments := make(map[int64][]store.Attachment)
	const attachmentBatchSize = 500
	for start := 0; start < len(messageIDs); start += attachmentBatchSize {
		end := min(start+attachmentBatchSize, len(messageIDs))
		batch, err := db.GetAttachmentsByMessages(messageIDs[start:end])
		if err != nil {
			return nil, fmt.Errorf("load synthetic message attachments: %w", err)
		}
		for messageID, values := range batch {
			attachments[messageID] = values
		}
	}
	m := &Matcher{
		byFingerprint: make(map[[32]byte][]candidate),
		currentByID:   make(map[string]reservation),
		reservations:  make(map[string]reservation),
	}
	for _, msg := range syntheticMessages {
		fingerprint := durianmail.SyntheticFingerprint(contentFromStore(msg, attachments[msg.ID]), msg.Date)
		if len(msg.SyntheticFingerprint) == len(fingerprint) {
			copy(fingerprint[:], msg.SyntheticFingerprint)
		}
		uid, _ := durianmail.SyntheticMessageSequence(msg.MessageID)
		value := candidate{
			messageID:     msg.MessageID,
			uid:           uid,
			rowID:         msg.ID,
			ingestPending: msg.IngestPending,
		}
		if uidValidity, ok := durianmail.SyntheticMessageUIDValidity(msg.MessageID); ok && uidValidity == currentUIDValidity {
			m.currentByID[msg.MessageID] = reservation{fingerprint: fingerprint, candidate: value, current: true}
			continue
		}
		m.byFingerprint[fingerprint] = append(m.byFingerprint[fingerprint], value)
	}
	for fingerprint := range m.byFingerprint {
		candidates := m.byFingerprint[fingerprint]
		sortCandidates(candidates)
		m.byFingerprint[fingerprint] = candidates
	}
	return m, nil
}

// MatchRaw parses a fetched RFC822 message and consumes one matching local ID.
// initialIngestComplete reports whether the matched row finished its original
// enrichment and is therefore safe to preserve without running it again.
func (m *Matcher) MatchRaw(provisionalMessageID string, raw []byte, internalDate time.Time) (messageID string, initialIngestComplete bool, err error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", false, fmt.Errorf("parse message for identity recovery: %w", err)
	}
	content := durianmail.NewParser().Parse(parsed)
	// Recovery is only for messages that truly lack a Message-ID header. A real
	// sender is allowed to choose an ID that resembles Durian's generated prefix.
	if strings.Trim(content.MessageID, "<>") != "" {
		return "", false, nil
	}
	dateUnix := internalDate.Unix()
	if parsedDate, err := mail.ParseDate(content.Date); err == nil {
		dateUnix = parsedDate.Unix()
	}
	messageID, initialIngestComplete = m.Match(provisionalMessageID, content, dateUnix)
	return messageID, initialIngestComplete, nil
}

// Match consumes and returns one local ID with the same parsed content.
func (m *Matcher) Match(provisionalMessageID string, content *durianmail.MailContent, dateUnix int64) (messageID string, initialIngestComplete bool) {
	fingerprint := durianmail.SyntheticFingerprint(content, dateUnix)
	if current, ok := m.currentByID[provisionalMessageID]; ok {
		if current.fingerprint != fingerprint {
			return "", false
		}
		delete(m.currentByID, provisionalMessageID)
		m.reservations[current.candidate.messageID] = current
		return current.candidate.messageID, !current.candidate.ingestPending
	}
	candidates := m.byFingerprint[fingerprint]
	if len(candidates) == 0 {
		return "", false
	}
	matched := candidates[0]
	if len(candidates) == 1 {
		delete(m.byFingerprint, fingerprint)
	} else {
		m.byFingerprint[fingerprint] = candidates[1:]
	}
	m.reservations[matched.messageID] = reservation{fingerprint: fingerprint, candidate: matched}
	return matched.messageID, !matched.ingestPending
}

// Commit makes a reserved match unavailable for the rest of this replacement.
// Call it after the recovered row's upsert has completed durably.
func (m *Matcher) Commit(messageID string) {
	delete(m.reservations, messageID)
}

// Restore returns a reserved match after an ingest failed before its durable
// upsert. This lets a later retry consume the same duplicate in stable order.
func (m *Matcher) Restore(messageID string) {
	reserved, ok := m.reservations[messageID]
	if !ok {
		return
	}
	delete(m.reservations, messageID)
	if reserved.current {
		m.currentByID[messageID] = reserved
		return
	}
	candidates := append(m.byFingerprint[reserved.fingerprint], reserved.candidate)
	sortCandidates(candidates)
	m.byFingerprint[reserved.fingerprint] = candidates
}

func sortCandidates(candidates []candidate) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].uid != candidates[j].uid {
			return candidates[i].uid > candidates[j].uid
		}
		return candidates[i].rowID > candidates[j].rowID
	})
}

func contentFromStore(msg *store.Message, attachments []store.Attachment) *durianmail.MailContent {
	content := &durianmail.MailContent{
		From:       msg.FromAddr,
		To:         msg.ToAddrs,
		CC:         msg.CCAddrs,
		BCC:        msg.BCCAddrs,
		Subject:    msg.Subject,
		InReplyTo:  msg.InReplyTo,
		References: msg.Refs,
		Body:       msg.BodyText,
		HTML:       msg.BodyHTML,
	}
	for _, attachment := range attachments {
		content.Attachments = append(content.Attachments, durianmail.AttachmentInfo{
			PartID:      attachment.PartID,
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			Size:        attachment.Size,
			Disposition: attachment.Disposition,
			ContentID:   attachment.ContentID,
		})
	}
	return content
}
