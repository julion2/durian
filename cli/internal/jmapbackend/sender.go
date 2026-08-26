package jmapbackend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/smtp"
)

// Sender delivers RFC 5322 mail via JMAP Email/import and EmailSubmission/set.
type Sender struct {
	b *Backend
}

func NewSender(account *config.AccountConfig) (*Sender, error) {
	b, err := New(account)
	if err != nil {
		return nil, fmt.Errorf("JMAP sender: %w", err)
	}
	return &Sender{b: b}, nil
}

func (s *Sender) Send(ctx context.Context, message *mailsend.Message) error {
	raw, err := smtp.FromMessage(message).Build()
	if err != nil {
		return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("build message: %w", err)}
	}
	if len(message.BCC) > 0 {
		for _, address := range message.BCC {
			if strings.ContainsAny(address, "\r\n") {
				return &mailsend.Error{Kind: mailsend.KindPermanent, Err: fmt.Errorf("invalid Bcc %q: contains CR or LF", address)}
			}
		}
		raw = append([]byte("Bcc: "+strings.Join(message.BCC, ", ")+"\r\n"), raw...)
	}
	if err := s.b.sendRaw(ctx, raw); err != nil {
		return classifySendError(err)
	}
	return nil
}

func (b *Backend) sendRaw(ctx context.Context, raw []byte) error {
	if err := b.loadMailboxes(ctx); err != nil {
		return err
	}
	draftsID := b.mailboxIDForTag("draft")
	sentID := b.mailboxIDForTag("sent")
	if draftsID == "" {
		return errors.New("JMAP account has no drafts mailbox")
	}
	identityID, err := b.identityID(ctx)
	if err != nil {
		return err
	}
	ref, err := b.Append(ctx, draftsID, backend.Flags{}, raw)
	if err != nil {
		return fmt.Errorf("import JMAP draft: %w", err)
	}

	createID := "s0"
	args := map[string]interface{}{
		"accountId": b.client.accountID,
		"create": map[string]interface{}{
			createID: map[string]interface{}{
				"emailId":    ref.ID,
				"identityId": identityID,
			},
		},
	}
	if sentID != "" {
		args["onSuccessUpdateEmail"] = map[string]interface{}{
			"#" + createID: map[string]interface{}{
				"keywords/$draft":        nil,
				"mailboxIds/" + draftsID: nil,
				"mailboxIds/" + sentID:   true,
			},
		}
	}
	var result struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated map[string]methodError `json:"notCreated"`
	}
	if err := b.client.call(ctx, []string{coreCapability, mailCapability, submissionCapability}, "EmailSubmission/set", args, &result); err != nil {
		return err
	}
	if createErr, ok := result.NotCreated[createID]; ok {
		return &createErr
	}
	if _, ok := result.Created[createID]; !ok {
		return errors.New("EmailSubmission/set returned no created submission")
	}
	return nil
}

func (b *Backend) identityID(ctx context.Context) (string, error) {
	if _, ok := b.client.session.Capabilities[submissionCapability]; !ok {
		return "", errors.New("JMAP server does not advertise mail submission")
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
		return "", errors.New("JMAP account has no submission identity")
	}
	return result.List[0].ID, nil
}

func (b *Backend) mailboxIDForTag(tag string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tagToID[strings.ToLower(tag)]
}

func classifySendError(err error) error {
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
