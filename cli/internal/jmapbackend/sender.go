package jmapbackend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
)

var (
	errNoDraftsMailbox       = errors.New("JMAP account has no drafts mailbox")
	errSubmissionUnavailable = errors.New("JMAP mail submission is unavailable")
	errNoSubmissionIdentity  = errors.New("JMAP account has no submission identity")
)

// Sender creates a structured JMAP Email and submits it for delivery.
type Sender struct {
	b *Backend
}

// NewSender creates a JMAP submission sender for account.
func NewSender(account *config.AccountConfig) (*Sender, error) {
	b, err := New(account)
	if err != nil {
		return nil, fmt.Errorf("JMAP sender: %w", err)
	}
	return &Sender{b: b}, nil
}

func (s *Sender) Send(ctx context.Context, message *mailsend.Message) error {
	draft, err := structuredEmail(message)
	if err != nil {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: err}
	}
	if err := s.b.sendStructured(ctx, draft, message.Attachments); err != nil {
		return classifySendError(err)
	}
	return nil
}

// SavesSentCopy reports that JMAP Submission stores sent mail server-side.
func (s *Sender) SavesSentCopy() bool { return true }

func structuredEmail(message *mailsend.Message) (map[string]interface{}, error) {
	from, err := jmapAddresses([]string{message.From}, "From")
	if err != nil {
		return nil, err
	}
	to, err := jmapAddresses(message.To, "To")
	if err != nil {
		return nil, err
	}
	cc, err := jmapAddresses(message.CC, "Cc")
	if err != nil {
		return nil, err
	}
	bcc, err := jmapAddresses(message.BCC, "Bcc")
	if err != nil {
		return nil, err
	}
	bodyType := "text/plain"
	if message.IsHTML {
		bodyType = "text/html"
	}
	draft := map[string]interface{}{
		"from":          from,
		"subject":       message.Subject,
		"keywords":      map[string]bool{"$draft": true},
		"bodyValues":    map[string]interface{}{"body": map[string]interface{}{"value": message.Body}},
		"bodyStructure": map[string]interface{}{"partId": "body", "type": bodyType},
	}
	if len(to) > 0 {
		draft["to"] = to
	}
	if len(cc) > 0 {
		draft["cc"] = cc
	}
	if len(bcc) > 0 {
		draft["bcc"] = bcc
	}
	if id := bareMessageID(message.MessageID); id != "" {
		draft["messageId"] = []string{id}
	}
	if id := bareMessageID(message.InReplyTo); id != "" {
		draft["inReplyTo"] = []string{id}
	}
	if references := messageIDs(message.References); len(references) > 0 {
		draft["references"] = references
	}
	return draft, nil
}

func jmapAddresses(values []string, field string) ([]map[string]interface{}, error) {
	addresses := make([]map[string]interface{}, 0, len(values))
	for _, value := range values {
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid %s %q: contains CR or LF", field, value)
		}
		address, err := mail.ParseAddress(value)
		if err != nil {
			return nil, fmt.Errorf("invalid %s %q: %w", field, value, err)
		}
		name := interface{}(nil)
		if address.Name != "" {
			name = address.Name
		}
		addresses = append(addresses, map[string]interface{}{"name": name, "email": address.Address})
	}
	return addresses, nil
}

func bareMessageID(value string) string {
	return strings.Trim(strings.TrimSpace(value), "<>")
}

func messageIDs(value string) []string {
	fields := strings.Fields(value)
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if id := bareMessageID(strings.Trim(field, ",")); id != "" {
			result = append(result, id)
		}
	}
	return result
}

func (b *Backend) sendStructured(ctx context.Context, draft map[string]interface{}, attachments []mailsend.Attachment) error {
	draftsID, sentID, identityID, err := b.prepareSubmission(ctx)
	if err != nil {
		return err
	}
	draft["mailboxIds"] = map[string]bool{draftsID: true}
	if len(attachments) > 0 {
		body := draft["bodyStructure"]
		parts := []interface{}{body}
		for _, attachment := range attachments {
			contentType := attachment.MIMEType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			blobID, err := b.client.upload(ctx, attachment.Data, contentType)
			if err != nil {
				return fmt.Errorf("upload JMAP attachment %q: %w", attachment.Filename, err)
			}
			parts = append(parts, map[string]interface{}{
				"blobId": blobID, "type": contentType, "name": attachment.Filename, "disposition": "attachment",
			})
		}
		draft["bodyStructure"] = map[string]interface{}{"type": "multipart/mixed", "subParts": parts}
	}

	const emailCreateID = "e0"
	var emailResult struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	emailArgs := map[string]interface{}{
		"accountId": b.client.accountID,
		"create":    map[string]interface{}{emailCreateID: draft},
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability}, "Email/set", emailArgs, &emailResult); err != nil {
		return err
	}
	if createErr, ok := emailResult.NotCreated[emailCreateID]; ok {
		return &createErr
	}
	draftID := emailResult.Created[emailCreateID].ID
	if draftID == "" {
		return errors.New("Email/set returned no created draft")
	}
	return b.submitDraft(ctx, draftID, draftsID, sentID, identityID)
}

func (b *Backend) sendRaw(ctx context.Context, raw []byte) error {
	draftsID, sentID, identityID, err := b.prepareSubmission(ctx)
	if err != nil {
		return err
	}
	ref, err := b.Append(ctx, draftsID, backend.Flags{}, raw)
	if err != nil {
		return fmt.Errorf("import JMAP draft: %w", err)
	}
	return b.submitDraft(ctx, ref.ID, draftsID, sentID, identityID)
}

func (b *Backend) prepareSubmission(ctx context.Context) (string, string, string, error) {
	if err := b.loadMailboxes(ctx); err != nil {
		return "", "", "", err
	}
	draftsID := b.mailboxIDForTag("draft")
	if draftsID == "" {
		return "", "", "", errNoDraftsMailbox
	}
	// Without a Sent mailbox there is nowhere to move the message to on success,
	// and RFC 8621 forbids clearing its last mailbox — it would stay in Drafts,
	// flagged $draft, while SavesSentCopy() tells the engine not to write a local
	// Sent copy either. Create the role mailbox rather than report a send that
	// left no record anywhere.
	sentID := b.mailboxIDForTag("sent")
	if sentID == "" {
		if err := b.createRoleMailbox(ctx, "sent", "Sent"); err != nil {
			return "", "", "", fmt.Errorf("create JMAP sent mailbox: %w", err)
		}
		if sentID = b.mailboxIDForTag("sent"); sentID == "" {
			return "", "", "", errors.New("created JMAP sent mailbox could not be resolved")
		}
	}
	identityID, err := b.identityID(ctx)
	if err != nil {
		return "", "", "", err
	}
	return draftsID, sentID, identityID, nil
}

func (b *Backend) submitDraft(ctx context.Context, draftID, draftsID, sentID, identityID string) error {
	createID := "s0"
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"create": map[string]interface{}{
			createID: map[string]interface{}{
				"emailId":    draftID,
				"identityId": identityID,
			},
		},
	}
	args["onSuccessUpdateEmail"] = map[string]interface{}{
		"#" + createID: map[string]interface{}{
			"keywords/$draft":        nil,
			"mailboxIds/" + draftsID: nil,
			"mailboxIds/" + sentID:   true,
		},
	}
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	// Every failure past this point leaves the created copy behind. The outbox
	// retries a transient send, so without cleanup each attempt would add
	// another draft to the user's Drafts mailbox.
	submitted := false
	defer func() {
		if submitted {
			return
		}
		if err := b.destroyEmail(context.WithoutCancel(ctx), draftID); err != nil {
			slog.Warn("Failed to remove JMAP draft after unsuccessful submission", "module", "JMAPBACKEND", // encgrep:allow remote id is operational metadata, not message content
				"id", draftID, "err", err)
		}
	}()

	if err := b.client.call(ctx, []string{coreCapability, mailCapability, submissionCapability}, "EmailSubmission/set", args, &result); err != nil {
		return err
	}
	if createErr, ok := result.NotCreated[createID]; ok {
		return &createErr
	}
	if _, ok := result.Created[createID]; !ok {
		return errors.New("EmailSubmission/set returned no created submission")
	}
	submitted = true
	return nil
}

func (b *Backend) identityID(ctx context.Context) (string, error) {
	if _, ok := b.client.session.Capabilities[submissionCapability]; !ok {
		return "", fmt.Errorf("%w: server capability missing", errSubmissionUnavailable)
	}
	account, ok := b.client.session.Accounts[b.client.accountID]
	if _, supportsSubmission := account.AccountCapabilities[submissionCapability]; !ok || !supportsSubmission {
		return "", fmt.Errorf("%w: account capability missing", errSubmissionUnavailable)
	}
	var result struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"list"`
	}
	args := map[string]interface{}{"accountId": b.client.accountID, "properties": []string{"id", "email"}}
	if err := b.client.call(ctx, []string{coreCapability, submissionCapability}, "Identity/get", args, &result); err != nil {
		return "", err
	}
	for _, identity := range result.List {
		if strings.EqualFold(identity.Email, b.account.Email) {
			return identity.ID, nil
		}
	}
	if len(result.List) == 0 {
		return "", errNoSubmissionIdentity
	}
	return result.List[0].ID, nil
}

func (b *Backend) mailboxIDForTag(tag string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tagToID[canonicalMailboxSegment(tag)]
}

func classifySendError(err error) error {
	if errors.Is(err, errNoDraftsMailbox) || errors.Is(err, errSubmissionUnavailable) || errors.Is(err, errNoSubmissionIdentity) {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: err}
	}
	var statusErr *statusError
	if errors.As(err, &statusErr) {
		kind := mailsend.KindTransient
		if statusErr.Status >= 400 && statusErr.Status < 500 && statusErr.Status != http.StatusTooManyRequests {
			kind = mailsend.KindPermanent
		}
		return &mailsend.Error{Kind: kind, Err: err}
	}
	var methodErr *methodError
	if errors.As(err, &methodErr) {
		kind := mailsend.KindPermanent
		if methodErr.Type == "serverFail" || methodErr.Type == "serverPartialFail" || methodErr.Type == "rateLimit" {
			kind = mailsend.KindTransient
		}
		return &mailsend.Error{Kind: kind, Err: err}
	}
	return &mailsend.Error{Kind: mailsend.KindNetwork, Err: err}
}
