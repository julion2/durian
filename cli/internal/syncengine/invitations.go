package syncengine

import (
	"bytes"
	"context"
	"log/slog"
	"net/mail"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/invitationsync"
	durianmail "github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

// fillInvitations catches up on the invitations of mail synced before they
// were kept (see invitationsync). It reads each message as ingest does, so
// it keeps the same part ingest would have.
func (e *Engine) fillInvitations(ctx context.Context, b backend.Backend) {
	ctx, cancel := context.WithTimeout(ctx, invitationsync.Budget)
	defer cancel()
	parser := durianmail.NewParser()
	_, err := invitationsync.Fill(ctx, e.opts.Store, e.opts.Account, store.ByRemoteRef,
		func(ctx context.Context, m store.MissingInvitation) (string, error) {
			var raw bytes.Buffer
			if err := b.FetchBody(ctx, backend.RemoteRef{Folder: m.Mailbox, ID: m.RemoteRef}, &raw); err != nil {
				return "", err
			}
			parsed, err := mail.ReadMessage(&raw)
			if err != nil {
				return "", err
			}
			return parser.Parse(parsed).Calendar, nil
		})
	if err != nil {
		slog.Debug("Invitation catch-up stopped", "module", "SYNCENGINE", "err", err)
	}
}
