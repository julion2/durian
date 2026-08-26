// Package sender selects the mailsend.Sender for an account: Microsoft Graph,
// Gmail, JMAP, or SMTP. It is the single place that maps an account to its
// outbound transport, shared by the outbox worker and the CLI send command.
package sender

import (
	"github.com/julion2/durian/cli/internal/auth"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/gmailbackend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/jmapbackend"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/smtp"
)

// For returns the Sender that delivers mail for the given account: Microsoft
// Graph for Graph-backed accounts, the Gmail API for Gmail accounts, JMAP
// submission for JMAP accounts, and SMTP otherwise.
func For(account *config.AccountConfig) (mailsend.Sender, error) {
	switch {
	case account.UsesGraphBackend():
		return graphbackend.NewSender(account)
	case account.UsesGmailBackend():
		return gmailbackend.NewSender(account)
	case account.UsesJMAPBackend():
		return jmapbackend.NewSender(account)
	}
	smtpAuth, err := auth.GetSMTPAuth(account)
	if err != nil {
		return nil, err
	}
	return smtp.NewSender(account.SMTP.Host, account.SMTP.Port, smtpAuth), nil
}
