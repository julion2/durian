package gmailbackend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/smtp"
)

// Sender delivers mail through the Gmail REST API (users.messages.send). It is
// the Gmail arm of the provider-neutral mailsend.Sender seam, next to
// smtp.Sender and graphbackend.Sender.
type Sender struct {
	b *Backend
}

// NewSender builds a Gmail Sender for a Google OAuth account.
func NewSender(account *config.AccountConfig) (*Sender, error) {
	b, err := New(account)
	if err != nil {
		return nil, fmt.Errorf("gmail sender: %w", err)
	}
	return &Sender{b: b}, nil
}

// gmailSendResponse is the messages.send reply (also the shape of a metadata get).
type gmailSendResponse struct {
	ID      string `json:"id"`
	Payload struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

// Send builds the RFC822 message and posts it to users.messages.send. Gmail
// threads the reply from the In-Reply-To / References headers the MIME already
// carries, delivers Bcc recipients (see below), and auto-saves a Sent copy.
func (s *Sender) Send(ctx context.Context, m *mailsend.Message) error {
	return s.SendAfterPersist(ctx, m, func(string) error { return nil })
}

// SendAfterPersist creates a Gmail draft and durably records Gmail's normalized
// Message-Id before drafts.send can deliver it.
func (s *Sender) SendAfterPersist(ctx context.Context, m *mailsend.Message, persist func(string) error) error {
	mime, err := smtp.FromMessage(m).Build()
	if err != nil {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("build message: %w", err)}
	}
	// Build() omits Bcc (SMTP delivers it via the envelope, not the DATA). Gmail's
	// raw send instead reads a Bcc header, delivers to it, and strips it from the
	// stored/delivered message — so prepend it as a header.
	if len(m.BCC) > 0 {
		for _, addr := range m.BCC {
			// A CR/LF in a value we splice straight into a raw MIME header would
			// let a caller inject additional headers. Reject rather than send.
			if strings.ContainsAny(addr, "\r\n") {
				return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("invalid Bcc %q: contains CR or LF", addr)}
			}
		}
		mime = append([]byte("Bcc: "+strings.Join(m.BCC, ", ")+"\r\n"), mime...)
	}

	body := map[string]any{"message": map[string]string{"raw": base64.URLEncoding.EncodeToString(mime)}}
	var draft struct {
		ID      string            `json:"id"`
		Message gmailSendResponse `json:"message"`
	}
	if err := s.b.doJSON(ctx, http.MethodPost, s.b.baseURL+"/users/me/drafts", body, &draft); err != nil {
		return classifyGmailSendError(err)
	}
	if draft.Message.ID == "" {
		if err := s.b.doJSON(ctx, http.MethodGet, s.b.baseURL+"/users/me/drafts/"+url.PathEscape(draft.ID), nil, &draft); err != nil {
			return classifyGmailSendError(err)
		}
	}
	id := s.sentMessageID(ctx, draft.Message.ID)
	if id == "" {
		return &mailsend.Error{Kind: mailsend.KindTransient, Err: errors.New("gmail draft has no Message-Id")}
	}
	m.MessageID = id
	if err := persist(id); err != nil {
		return &mailsend.Error{Kind: mailsend.KindTransient, Err: fmt.Errorf("persist gmail Message-Id: %w", err)}
	}
	var sent gmailSendResponse
	if err := s.b.doJSON(ctx, http.MethodPost, s.b.baseURL+"/users/me/drafts/send", map[string]string{"id": draft.ID}, &sent); err != nil {
		var se *statusError
		if !errors.As(err, &se) {
			return &mailsend.Error{Kind: mailsend.KindAmbiguous, Err: err}
		}
		// drafts.send is irreversible. A 5xx may be emitted after Gmail
		// accepted the draft, so an automatic retry could deliver twice.
		if se.status >= 500 {
			return &mailsend.Error{Kind: mailsend.KindAmbiguous, Err: err}
		}
		return classifyGmailSendError(err)
	}
	return nil
}

// SavesSentCopy reports that Gmail stores sent mail server-side.
func (s *Sender) SavesSentCopy() bool { return true }

// sentMessageID reads the RFC822 Message-Id header of the just-sent message.
func (s *Sender) sentMessageID(ctx context.Context, gmailID string) string {
	if gmailID == "" {
		return ""
	}
	var resp gmailSendResponse
	reqURL := s.b.baseURL + "/users/me/messages/" + url.PathEscape(gmailID) + "?format=metadata&metadataHeaders=Message-Id"
	if err := s.b.doJSON(ctx, http.MethodGet, reqURL, nil, &resp); err != nil {
		return ""
	}
	for _, h := range resp.Payload.Headers {
		if strings.EqualFold(h.Name, "Message-Id") {
			return h.Value
		}
	}
	return ""
}

// classifyGmailSendError maps a send failure to a mailsend.Kind. A quota 403 is
// retryable (transient); other 4xx (bad recipient, permission) poison the item;
// 429/5xx retry; a non-HTTP error is treated as network.
func classifyGmailSendError(err error) error {
	var se *statusError
	if errors.As(err, &se) {
		kind := mailsend.KindTransient
		if se.status >= 400 && se.status < 500 && se.status != http.StatusTooManyRequests &&
			!(se.status == http.StatusForbidden && isQuotaBody([]byte(se.body))) {
			kind = mailsend.KindPermanent
		}
		return &mailsend.Error{Kind: kind, Err: err}
	}
	return &mailsend.Error{Kind: mailsend.KindNetwork, Err: err}
}
