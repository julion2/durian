package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/handler"
)

// guiCalendarSyncer turns an explicit GUI action into one event-scoped sync.
// It intentionally discards the newly fetched remote mirror: the filtered run
// applies no other event, so advancing the account cursor would skip those
// unapplied remote changes. The next autosync fetches them again normally.
type guiCalendarSyncer struct {
	cfg *config.Config
}

func (s guiCalendarSyncer) SyncCalendarEvent(ctx context.Context, identifier, calendarName, uid, operation string) (bool, error) {
	if s.cfg == nil {
		return false, errors.New("config not loaded")
	}
	if err := rejectLocalCalendarAccount(identifier); err != nil {
		return false, fmt.Errorf("%w: %v", handler.ErrCalendarSyncBadRequest, err)
	}
	account, err := s.cfg.GetAccountByIdentifier(identifier)
	if err != nil {
		return false, fmt.Errorf("%w: %v", handler.ErrCalendarSyncNotFound, err)
	}
	if !account.CalendarEnabled() {
		return false, handler.ErrCalendarSyncDisabled
	}
	provider, err := newCalendarProvider(account)
	if err != nil {
		return false, fmt.Errorf("%w: %v", handler.ErrCalendarSyncBadRequest, err)
	}
	accountDir := filepath.Join(config.CalendarBaseDir(s.cfg, ""), account.CalendarDir())
	release, ok, err := calendarsync.AcquireRunLock(accountDir)
	if err != nil {
		return false, fmt.Errorf("failed to acquire calendar sync run lock: %w", err)
	}
	if !ok {
		return false, handler.ErrCalendarSyncBusy
	}
	defer release()

	store := calendarsync.NewFileStateStore(accountDir)
	state, err := store.Load()
	if err != nil {
		return false, fmt.Errorf("failed to load calendar sync state: %w", err)
	}
	state, mailboxBackup, err := calendarsync.BindMailbox(accountDir, state, account.Email, account.IsDelegatedMailbox(), true)
	if err != nil {
		return false, fmt.Errorf("failed to bind calendar vdir to mailbox: %w", err)
	}
	if mailboxBackup != "" {
		slog.Warn("Quarantined legacy calendar vdir before targeted sync", "module", "CALSYNC", // encgrep:allow static message text, no content
			"account", account.GetAliasOrName(), "backup", mailboxBackup) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
	mirror, err := calendarsync.NewFileMirrorStore(accountDir).Load()
	if err != nil {
		return false, fmt.Errorf("failed to load calendar mirror: %w", err)
	}
	plans, err := calendarsync.PlanAll(ctx, provider, accountDir, account.CalendarInclude(), state, mirror)
	if err != nil {
		return false, fmt.Errorf("failed to plan calendar sync: %w", err)
	}
	filtered, found := calendarsync.FilterEvent(plans, calendarName, uid)
	if !found {
		// The background loop may already have converged a non-notifying save.
		return false, nil
	}
	applied := false
	for _, plan := range filtered {
		for _, action := range plan.Actions {
			if !guiOperationAllows(operation, action) {
				return false, handler.ErrCalendarSyncConflict
			}
			applied = applied || action.RemoteMutation()
		}
	}

	stats, applyErr := calendarsync.ApplyAll(ctx, provider, state, filtered, calendarsync.SyncOptions{
		Conflict: s.cfg.CalendarConflictPolicy(account),
	})
	// ApplyAll records each completed provider operation immediately in state.
	// Persist that even after a partial failure so retrying cannot send twice.
	if saveErr := store.Save(state); saveErr != nil {
		if applyErr != nil {
			return false, errors.Join(applyErr, fmt.Errorf("failed to save calendar sync state: %w", saveErr))
		}
		return false, fmt.Errorf("failed to save calendar sync state: %w", saveErr)
	}
	if applyErr != nil {
		return false, fmt.Errorf("failed to apply calendar sync: %w", applyErr)
	}
	if stats.Skipped > 0 || stats.Failed > 0 {
		return false, fmt.Errorf("calendar event was not synced (%d skipped, %d failed)", stats.Skipped, stats.Failed)
	}
	return applied, nil
}

// guiOperationAllows binds the planner's output back to the action the user
// actually took. In particular, a missing/corrupt baseline must not turn Save
// into a remote-wins download or Delete into a download that resurrects the
// event locally.
func guiOperationAllows(operation string, action calendarsync.Action) bool {
	switch operation {
	case "save":
		return action.Kind == calendarsync.ActionUploadCreate ||
			action.Kind == calendarsync.ActionUploadUpdate ||
			action.Kind == calendarsync.ActionAdopt
	case "rsvp":
		return action.Kind == calendarsync.ActionRsvp
	case "delete":
		return action.Kind == calendarsync.ActionDeleteRemote ||
			action.Kind == calendarsync.ActionDropStatus
	default:
		return false
	}
}
