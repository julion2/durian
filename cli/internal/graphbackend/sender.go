package graphbackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
)

// uploadThreshold is the size above which an attachment must go through a Graph
// upload session instead of riding inline: Graph's /messages endpoint (and the
// draft-create POST) caps the request at ~4 MB, so files ≥3 MB are uploaded
// separately in chunks.
const uploadThreshold = 3 * 1024 * 1024

// uploadChunkSize is the per-PUT chunk size; Graph requires a multiple of 320
// KiB and ≤4 MB.
const uploadChunkSize = 5 * 320 * 1024 // ~1.6 MB

// Sender delivers mail via Microsoft Graph as create-draft-then-send: it POSTs a
// draft message with typed recipients — so BCC is honored and hidden without the
// unreliable MIME Bcc header — attaches files, and sends the draft. Small
// attachments go inline here; large ones (>3 MB) via an upload session land in a
// follow-up. Graph files the sent copy server-side, so no Sent-folder append.
type Sender struct {
	b *Backend
}

// NewSender builds a Graph sender for a Microsoft OAuth account.
func NewSender(account *config.AccountConfig) (*Sender, error) {
	b, err := New(account)
	if err != nil {
		return nil, err
	}
	return &Sender{b: b}, nil
}

// graphRecipient is Graph's recipient shape: {emailAddress: {address, name}}.
type graphRecipient struct {
	EmailAddress graphEmailAddress `json:"emailAddress"`
}

type graphEmailAddress struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

type graphBody struct {
	ContentType string `json:"contentType"` // "HTML" or "Text"
	Content     string `json:"content"`
}

type graphFileAttachment struct {
	ODataType    string `json:"@odata.type"` // "#microsoft.graph.fileAttachment"
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ContentBytes string `json:"contentBytes"` // base64
}

type graphDraft struct {
	Subject       string                `json:"subject"`
	Body          graphBody             `json:"body"`
	ToRecipients  []graphRecipient      `json:"toRecipients"`
	CcRecipients  []graphRecipient      `json:"ccRecipients,omitempty"`
	BccRecipients []graphRecipient      `json:"bccRecipients,omitempty"`
	Attachments   []graphFileAttachment `json:"attachments,omitempty"`
}

// Send creates a draft from m and sends it. BCC recipients are delivered via the
// typed bccRecipients field and never appear in the To/Cc headers.
func (s *Sender) Send(ctx context.Context, m *mailsend.Message) error {
	small, large := splitAttachments(m.Attachments)

	// A reply gets its threading headers from Graph via createReply; a new
	// message (or a reply whose original we can't resolve) is a fresh draft.
	var draftID, msgID string
	var err error
	if m.InReplyTo != "" {
		draftID, msgID, err = s.replyDraft(ctx, m, small)
		if err != nil {
			return classifyGraphSendError(err)
		}
	}
	if draftID == "" {
		draftID, msgID, err = s.newDraft(ctx, m, small)
		if err != nil {
			return classifyGraphSendError(err)
		}
	}
	// Adopt Graph's Message-ID so the caller's local Sent row carries the same
	// id as the server's sent copy and the two dedupe on the next sync.
	if msgID != "" {
		m.MessageID = msgID
	}

	// Large attachments go up in resumable chunks against the draft.
	for _, a := range large {
		if err := s.uploadAttachment(ctx, draftID, a); err != nil {
			return classifyGraphSendError(err)
		}
	}

	if err := s.b.doJSON(ctx, http.MethodPost,
		s.b.baseURL+s.b.mailbox+"/messages/"+url.PathEscape(draftID)+"/send", nil, nil); err != nil {
		return classifyGraphSendError(err)
	}
	return nil
}

// newDraft creates a fresh draft with small attachments inline, returning its
// id and Graph-assigned Message-ID.
func (s *Sender) newDraft(ctx context.Context, m *mailsend.Message, small []mailsend.Attachment) (string, string, error) {
	draft := graphDraft{
		Subject:       m.Subject,
		Body:          bodyOf(m),
		ToRecipients:  toRecipients(m.To),
		CcRecipients:  toRecipients(m.CC),
		BccRecipients: toRecipients(m.BCC),
		Attachments:   inlineAttachments(small),
	}
	var created draftResult
	if err := s.b.doJSON(ctx, http.MethodPost, s.b.baseURL+s.b.mailbox+"/messages", draft, &created); err != nil {
		return "", "", err
	}
	return created.ID, created.InternetMessageID, nil
}

// replyDraft creates a reply draft via createReply (so Graph stamps the
// In-Reply-To / References headers), then overwrites its body, subject and
// recipients with our composed values — never touching internetMessageHeaders,
// which would drop the threading. Returns ("", "", nil) when the original
// message can't be found, so the caller falls back to a plain draft.
func (s *Sender) replyDraft(ctx context.Context, m *mailsend.Message, small []mailsend.Attachment) (string, string, error) {
	originalID := s.resolveMessageID(ctx, m.InReplyTo)
	if originalID == "" {
		return "", "", nil
	}
	var reply draftResult
	if err := s.b.doJSON(ctx, http.MethodPost,
		s.b.baseURL+s.b.mailbox+"/messages/"+url.PathEscape(originalID)+"/createReply", nil, &reply); err != nil {
		return "", "", err
	}
	// Overwrite content only — leave internetMessageHeaders (threading) intact.
	patch := graphDraft{
		Subject:       m.Subject,
		Body:          bodyOf(m),
		ToRecipients:  toRecipients(m.To),
		CcRecipients:  toRecipients(m.CC),
		BccRecipients: toRecipients(m.BCC),
	}
	if err := s.b.doJSON(ctx, http.MethodPatch,
		s.b.baseURL+s.b.mailbox+"/messages/"+url.PathEscape(reply.ID), patch, nil); err != nil {
		return "", "", err
	}
	// The reply draft has no attachments yet; add the small ones via the
	// attachments endpoint (large ones are uploaded by the caller).
	for _, a := range small {
		if err := s.addSmallAttachment(ctx, reply.ID, a); err != nil {
			return "", "", err
		}
	}
	return reply.ID, reply.InternetMessageID, nil
}

// draftResult captures the fields we read back from a draft create / createReply.
type draftResult struct {
	ID                string `json:"id"`
	InternetMessageID string `json:"internetMessageId"`
}

func bodyOf(m *mailsend.Message) graphBody {
	b := graphBody{ContentType: "Text", Content: m.Body}
	if m.IsHTML {
		b.ContentType = "HTML"
	}
	return b
}

// resolveMessageID finds the Graph id of the message with the given RFC822
// Message-ID, or "" if none / on error (threading is best-effort).
func (s *Sender) resolveMessageID(ctx context.Context, internetMessageID string) string {
	// Graph stores internetMessageId with angle brackets; the store (and thus a
	// reply's InReplyTo) may carry it without. Normalize so the filter matches.
	id := strings.TrimSpace(internetMessageID)
	if id != "" && !strings.HasPrefix(id, "<") {
		id = "<" + id + ">"
	}
	q := url.Values{}
	q.Set("$filter", "internetMessageId eq '"+id+"'")
	q.Set("$select", "id")
	q.Set("$top", "1")
	var res struct {
		Value []struct {
			ID string `json:"id"`
		} `json:"value"`
	}
	if err := s.b.doJSON(ctx, http.MethodGet, s.b.baseURL+s.b.mailbox+"/messages?"+q.Encode(), nil, &res); err != nil {
		return ""
	}
	if len(res.Value) > 0 {
		return res.Value[0].ID
	}
	return ""
}

// addSmallAttachment POSTs a single small file attachment to an existing draft.
func (s *Sender) addSmallAttachment(ctx context.Context, draftID string, a mailsend.Attachment) error {
	atts := inlineAttachments([]mailsend.Attachment{a})
	return s.b.doJSON(ctx, http.MethodPost,
		s.b.baseURL+s.b.mailbox+"/messages/"+url.PathEscape(draftID)+"/attachments", atts[0], nil)
}

// splitAttachments partitions attachments into those small enough to ride inline
// in the draft and those that must go through an upload session.
func splitAttachments(atts []mailsend.Attachment) (small, large []mailsend.Attachment) {
	for _, a := range atts {
		if len(a.Data) >= uploadThreshold {
			large = append(large, a)
		} else {
			small = append(small, a)
		}
	}
	return small, large
}

// uploadAttachment attaches one large file to the draft via a Graph upload
// session, PUTting it in ≤4 MB chunks.
func (s *Sender) uploadAttachment(ctx context.Context, draftID string, a mailsend.Attachment) error {
	ct := a.MIMEType
	if ct == "" {
		ct = "application/octet-stream"
	}
	reqBody := map[string]any{
		"AttachmentItem": map[string]any{
			"attachmentType": "file",
			"name":           a.Filename,
			"size":           len(a.Data),
			"contentType":    ct,
		},
	}
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := s.b.doJSON(ctx, http.MethodPost,
		s.b.baseURL+s.b.mailbox+"/messages/"+url.PathEscape(draftID)+"/attachments/createUploadSession",
		reqBody, &sess); err != nil {
		return err
	}
	if sess.UploadURL == "" {
		return fmt.Errorf("graph upload session returned no uploadUrl")
	}

	total := len(a.Data)
	for start := 0; start < total; start += uploadChunkSize {
		end := start + uploadChunkSize
		if end > total {
			end = total
		}
		if err := s.putChunk(ctx, sess.UploadURL, a.Data[start:end], start, end, total); err != nil {
			return err
		}
	}
	return nil
}

// putChunk PUTs one byte range to the upload-session URL, which is
// pre-authorized (no bearer token). Graph returns 200/201 on the final chunk and
// 202 while more are expected.
func (s *Sender) putChunk(ctx context.Context, uploadURL string, chunk []byte, start, end, total int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, total))
	req.ContentLength = int64(len(chunk))
	resp, err := s.b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload chunk: %w", err)
	}
	defer drainClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStatusError(resp)
	}
	return nil
}

// toRecipients parses "Name <addr>" / "addr" strings into Graph recipients,
// skipping blanks.
func toRecipients(addrs []string) []graphRecipient {
	out := make([]graphRecipient, 0, len(addrs))
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		addr, name := a, ""
		if parsed, err := mail.ParseAddress(a); err == nil {
			addr, name = parsed.Address, parsed.Name
		}
		out = append(out, graphRecipient{EmailAddress: graphEmailAddress{Address: addr, Name: name}})
	}
	return out
}

// inlineAttachments encodes each attachment as a Graph fileAttachment. Large
// files still ride inline here (a follow-up moves >3 MB to an upload session).
func inlineAttachments(atts []mailsend.Attachment) []graphFileAttachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]graphFileAttachment, len(atts))
	for i, a := range atts {
		ct := a.MIMEType
		if ct == "" {
			ct = "application/octet-stream"
		}
		out[i] = graphFileAttachment{
			ODataType:    "#microsoft.graph.fileAttachment",
			Name:         a.Filename,
			ContentType:  ct,
			ContentBytes: base64.StdEncoding.EncodeToString(a.Data),
		}
	}
	return out
}

// classifyGraphSendError maps a Graph send failure to a mailsend.Kind: 4xx
// (except 429) is permanent (bad message/recipient → poison); 429 and 5xx are
// transient; a non-HTTP error (no response) is treated as a network error.
func classifyGraphSendError(err error) error {
	var se *statusError
	if errors.As(err, &se) {
		kind := mailsend.KindTransient
		if se.status >= 400 && se.status < 500 && se.status != http.StatusTooManyRequests {
			kind = mailsend.KindPermanent
		}
		return &mailsend.Error{Kind: kind, Err: err}
	}
	return &mailsend.Error{Kind: mailsend.KindNetwork, Err: err}
}
