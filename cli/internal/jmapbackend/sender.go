package jmapbackend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
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
	errInvalidAttachmentType = errors.New("invalid JMAP attachment type")
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
		"keywords":      map[string]bool{"$draft": true, "$seen": true},
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
	messageID, err := messageIDs(message.MessageID)
	if err != nil {
		return nil, fmt.Errorf("invalid Message-ID: %w", err)
	}
	if len(messageID) > 1 {
		return nil, fmt.Errorf("invalid Message-ID: got %d IDs, want one", len(messageID))
	}
	if len(messageID) == 1 {
		draft["messageId"] = messageID
	}
	inReplyTo, err := messageIDs(message.InReplyTo)
	if err != nil {
		return nil, fmt.Errorf("invalid In-Reply-To: %w", err)
	}
	if len(inReplyTo) > 0 {
		draft["inReplyTo"] = inReplyTo
	}
	references, err := messageIDs(message.References)
	if err != nil {
		return nil, fmt.Errorf("invalid References: %w", err)
	}
	if len(references) > 0 {
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

func messageIDs(value string) ([]string, error) {
	var result []string
	for offset := 0; ; {
		var err error
		offset, err = skipMessageIDCFWS(value, offset)
		if err != nil {
			return nil, err
		}
		if offset == len(value) {
			if len(result) == 0 && strings.TrimSpace(value) != "" {
				return nil, errors.New("field contains no Message-ID")
			}
			return result, nil
		}

		var id string
		if value[offset] == '<' {
			id, offset, err = angleMessageID(value, offset)
		} else {
			// Durian-generated IDs are bracketed, but older API clients and
			// backend fixtures also use the equivalent bare id-left@id-right.
			start := offset
			for offset < len(value) && !messageIDWhitespace(value[offset]) && value[offset] != '(' {
				offset++
			}
			id = value[start:offset]
			err = validateMessageID(id)
		}
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
}

func skipMessageIDCFWS(value string, offset int) (int, error) {
	for offset < len(value) {
		if messageIDWhitespace(value[offset]) {
			offset++
			continue
		}
		if value[offset] != '(' {
			break
		}
		depth := 1
		offset++
		for offset < len(value) && depth > 0 {
			switch value[offset] {
			case '\\':
				offset++
				if offset == len(value) {
					return 0, errors.New("unterminated quoted pair in comment")
				}
			case '(':
				depth++
			case ')':
				depth--
			}
			offset++
		}
		if depth != 0 {
			return 0, errors.New("unterminated comment")
		}
	}
	return offset, nil
}

func angleMessageID(value string, offset int) (string, int, error) {
	var id strings.Builder
	quoted := false
	domainLiteral := false
	commentDepth := 0
	for offset++; offset < len(value); offset++ {
		char := value[offset]
		if commentDepth > 0 {
			switch char {
			case '\\':
				offset++
				if offset == len(value) {
					return "", 0, errors.New("unterminated quoted pair in comment")
				}
			case '(':
				commentDepth++
			case ')':
				commentDepth--
			}
			continue
		}
		if quoted || domainLiteral {
			id.WriteByte(char)
			if char == '\\' {
				offset++
				if offset == len(value) {
					return "", 0, errors.New("unterminated quoted pair in Message-ID")
				}
				id.WriteByte(value[offset])
				continue
			}
			if quoted && char == '"' {
				quoted = false
			}
			if domainLiteral && char == ']' {
				domainLiteral = false
			}
			continue
		}
		switch char {
		case '"':
			quoted = true
			id.WriteByte(char)
		case '[':
			domainLiteral = true
			id.WriteByte(char)
		case '(':
			commentDepth = 1
		case '>':
			parsed := id.String()
			if err := validateMessageID(parsed); err != nil {
				return "", 0, err
			}
			return parsed, offset + 1, nil
		default:
			if !messageIDWhitespace(char) {
				id.WriteByte(char)
			}
		}
	}
	return "", 0, errors.New("unterminated angle-bracketed Message-ID")
}

func validateMessageID(id string) error {
	at := -1
	quoted := false
	domainLiteral := false
	for i := 0; i < len(id); i++ {
		char := id[i]
		if quoted || domainLiteral {
			if char == '\\' {
				i++
				if i == len(id) {
					return errors.New("unterminated quoted pair in Message-ID")
				}
				continue
			}
			if quoted && char == '"' {
				quoted = false
			}
			if domainLiteral && char == ']' {
				domainLiteral = false
			}
			continue
		}
		switch char {
		case '"':
			quoted = true
		case '[':
			domainLiteral = true
		case '@':
			if at != -1 {
				return fmt.Errorf("Message-ID %q has multiple separators", id)
			}
			at = i
		}
	}
	if quoted || domainLiteral {
		return fmt.Errorf("Message-ID %q has an unterminated quoted component", id)
	}
	if at <= 0 || at == len(id)-1 {
		return fmt.Errorf("Message-ID %q must contain non-empty id-left and id-right", id)
	}
	left, right := id[:at], id[at+1:]
	if !validMessageIDDotAtom(left) && !validMessageIDQuotedLeft(left) {
		return fmt.Errorf("Message-ID %q has invalid id-left", id)
	}
	if !validMessageIDDotAtom(right) && !validMessageIDDomainLiteral(right) {
		return fmt.Errorf("Message-ID %q has invalid id-right", id)
	}
	return nil
}

func validMessageIDDotAtom(value string) bool {
	if value == "" || value[0] == '.' || value[len(value)-1] == '.' {
		return false
	}
	previousDot := false
	for i := 0; i < len(value); i++ {
		if value[i] == '.' {
			if previousDot {
				return false
			}
			previousDot = true
			continue
		}
		if !messageIDAtext(value[i]) {
			return false
		}
		previousDot = false
	}
	return true
}

func validMessageIDQuotedLeft(value string) bool {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for i := 1; i < len(value)-1; i++ {
		char := value[i]
		if char == '\\' {
			i++
			if i == len(value)-1 || !messageIDQuotedPair(value[i]) {
				return false
			}
			continue
		}
		if char == '"' || !(char == ' ' || char == '\t' || char == '!' || char >= '#' && char <= '[' || char >= ']' && char <= '~') {
			return false
		}
	}
	return true
}

func validMessageIDDomainLiteral(value string) bool {
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return false
	}
	for i := 1; i < len(value)-1; i++ {
		char := value[i]
		if !(char >= '!' && char <= 'Z' || char >= '^' && char <= '~') {
			return false
		}
	}
	return true
}

func messageIDAtext(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
		strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", rune(char))
}

func messageIDQuotedPair(char byte) bool {
	return char == ' ' || char == '\t' || char >= '!' && char <= '~'
}

func messageIDWhitespace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

func (b *Backend) sendStructured(ctx context.Context, draft map[string]interface{}, attachments []mailsend.Attachment) error {
	contentTypes := make([]string, len(attachments))
	mediaTypes := make([]string, len(attachments))
	charsets := make([]string, len(attachments))
	for i, attachment := range attachments {
		contentType := attachment.MIMEType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		mediaType, parameters, err := mime.ParseMediaType(contentType)
		if err != nil {
			return fmt.Errorf("%w %q: %v", errInvalidAttachmentType, contentType, err)
		}
		contentTypes[i] = contentType
		mediaTypes[i] = mediaType
		charsets[i] = parameters["charset"]
	}
	draftsID, sentID, identityID, err := b.prepareSubmission(ctx)
	if err != nil {
		return err
	}
	draft["mailboxIds"] = map[string]bool{draftsID: true}
	if len(attachments) > 0 {
		body := draft["bodyStructure"]
		parts := []interface{}{body}
		for i, attachment := range attachments {
			blobID, err := b.client.upload(ctx, attachment.Data, contentTypes[i])
			if err != nil {
				return fmt.Errorf("upload JMAP attachment %q: %w", attachment.Filename, err)
			}
			part := map[string]interface{}{
				"blobId": blobID, "type": mediaTypes[i], "name": attachment.Filename, "disposition": "attachment",
			}
			if charsets[i] != "" {
				part["charset"] = charsets[i]
			}
			parts = append(parts, part)
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
	updatePatch := map[string]interface{}{
		"keywords/$draft":        nil,
		"mailboxIds/" + draftsID: nil,
		"mailboxIds/" + sentID:   true,
	}
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
		"#" + createID: updatePatch,
	}
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	var updateResult struct {
		Updated    map[string]interface{} `json:"updated"`
		NotUpdated map[string]methodError `json:"notUpdated"`
	}
	updateResponse := &methodResponseTarget{name: "Email/set", out: &updateResult}
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

	callErr := b.client.call(ctx, []string{coreCapability, mailCapability, submissionCapability}, "EmailSubmission/set", args, &result, updateResponse)
	if createErr, ok := result.NotCreated[createID]; ok {
		return &createErr
	}
	if _, ok := result.Created[createID]; !ok {
		if callErr != nil {
			return callErr
		}
		return errors.New("EmailSubmission/set returned no created submission")
	}
	// Once the submission exists, the message may already be irrevocably in
	// flight. Never return a retryable send failure or destroy its source Email:
	// either action could cause duplicate delivery. If the required implicit
	// Email/set did not file the copy in Sent, retry only that idempotent patch.
	submitted = true
	var filingErr error
	switch {
	case callErr != nil:
		filingErr = callErr
	case !updateResponse.seen:
		filingErr = errors.New("EmailSubmission/set returned no implicit Email/set response")
	case updateResult.NotUpdated[draftID].Type != "":
		setErr := updateResult.NotUpdated[draftID]
		filingErr = &setErr
	default:
		if _, ok := updateResult.Updated[draftID]; !ok {
			filingErr = errors.New("implicit Email/set did not update the submitted email")
		}
	}
	if filingErr != nil {
		if err := b.updateEmail(context.WithoutCancel(ctx), draftID, updatePatch); err != nil {
			slog.Warn("JMAP submission succeeded but Sent filing could not be repaired", "module", "JMAPBACKEND", // encgrep:allow remote id is operational metadata, not message content
				"id", draftID, "err", err)
		} else {
			slog.Warn("Repaired JMAP Sent filing after implicit update failed", "module", "JMAPBACKEND", // encgrep:allow remote id is operational metadata, not message content
				"id", draftID)
		}
	}
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
	if errors.Is(err, errNoDraftsMailbox) || errors.Is(err, errSubmissionUnavailable) || errors.Is(err, errNoSubmissionIdentity) ||
		errors.Is(err, errInvalidAttachmentType) {
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
