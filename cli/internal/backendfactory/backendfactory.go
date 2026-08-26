// Package backendfactory is the composition root for provider-neutral mail
// backends. Keeping selection here prevents sync, body loading, attachments,
// and watchers from drifting to different provider rules.
package backendfactory

import (
	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/gmailbackend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/imapbackend"
	"github.com/julion2/durian/cli/internal/jmapbackend"
)

// New opens the backend selected by account.sync_engine.
func New(account *config.AccountConfig) (backend.Backend, error) {
	switch {
	case account.UsesGraphBackend():
		return graphbackend.New(account)
	case account.UsesGmailBackend():
		return gmailbackend.New(account)
	case account.UsesJMAPBackend():
		return jmapbackend.New(account)
	default:
		return imapbackend.New(account)
	}
}

// CursorSuffix namespaces opaque cursor files by backend implementation.
func CursorSuffix(account *config.AccountConfig) string {
	switch {
	case account.UsesGraphBackend():
		return "-graph"
	case account.UsesGmailBackend():
		return "-gmail"
	case account.UsesJMAPBackend():
		return "-jmap"
	default:
		return ""
	}
}

// PushWatch reports whether the selected backend has a native long-lived push
// transport. It is deliberately static so watcher startup does not connect and
// authenticate an IMAP account merely to read a capability constant.
func PushWatch(account *config.AccountConfig) bool {
	return !account.UsesGraphBackend() && !account.UsesGmailBackend()
}

// PushInboxOnly reports whether the backend's push transport watches only the
// inbox. JMAP EventSource is account-wide; IMAP IDLE watches INBOX.
func PushInboxOnly(account *config.AccountConfig) bool {
	return !account.UsesGraphBackend() && !account.UsesGmailBackend() && !account.UsesJMAPBackend()
}
