// Package invitationsync fills in the invitations of mail synced before
// invitations were kept. Ingest stores the iCalendar part of every message it
// tags as calendar mail; this catches up on the older ones, a few per sync,
// newest first, until none are left – after which a sync costs one query.
package invitationsync

import (
	"context"
	"log/slog"
	"time"

	"github.com/julion2/durian/cli/internal/store"
)

const (
	// batchSize is how many messages one query hands out.
	batchSize = 50
	// Budget bounds the time one sync spends catching up, so new mail is
	// never held up behind it; what's left waits for the next sync.
	Budget = 10 * time.Second
)

// Fetch returns the iCalendar part of one message, "" when it has none.
type Fetch func(ctx context.Context, m store.MissingInvitation) (string, error)

// Fill fetches and stores missing invitations of an account until none are
// left or ctx ends; callers bound ctx with Budget. A message whose fetch
// fails is skipped for the rest of the run and tried again on the next one.
// It returns how many invitations it stored.
func Fill(ctx context.Context, db *store.DB, account string, source store.InvitationSource, fetch Fetch) (int, error) {
	var failed []int64
	stored := 0
	for ctx.Err() == nil {
		missing, err := db.MissingInvitations(account, source, failed, batchSize)
		if err != nil {
			return stored, err
		}
		if len(missing) == 0 {
			break
		}
		for _, m := range missing {
			ics, err := fetch(ctx, m)
			if ctx.Err() != nil {
				// out of time: the fetch didn't fail, it was cut short
				return stored, nil
			}
			if err != nil {
				slog.Debug("Invitation fetch failed", "module", "INVITATIONS", "err", err)
				failed = append(failed, m.ID)
				continue
			}
			if err := db.SetInvitation(m.ID, ics); err != nil {
				return stored, err
			}
			stored++
		}
	}
	if stored > 0 || len(failed) > 0 {
		slog.Debug("Invitations filled in", "module", "INVITATIONS", "stored", stored, "failed", len(failed))
	}
	return stored, nil
}
