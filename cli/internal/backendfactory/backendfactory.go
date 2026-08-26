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
