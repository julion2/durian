// Background calendar autosync for `durian serve`.
//
// Each eligible account gets one loop that periodically pulls remote calendar
// changes into the local vdir. Every planned action passes through a safety
// filter before ApplyAll — this file must never call ApplyAll on an
// unfiltered plan:
//
//   - Default (calendar.autosync_upload "none"): FilterDownloadOnly strips
//     every action whose RemoteMutation() is true — uploads, remote deletes,
//     conflicts, RSVPs. The loop is download-only by construction.
//   - Safe upload mode ("safe"): FilterNonNotifying additionally keeps the
//     provably non-notifying uploads — creates/edits of attendee-less events
//     (Action.Notifies() false) — and still defers every remote delete
//     (attendees or not), every conflict, every RSVP call and every upload
//     that may make the provider email anyone.
//
// In BOTH modes the deferred actions remain exclusively behind the
// interactive `durian calendar sync` confirmation gate; no autosync run may
// ever cause the provider to email a real person or delete a remote event.

package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/handler"
)

// calendarAutosyncRunTimeout bounds one autosync cycle so a hung provider
// call cannot wedge the loop (and the run lock) forever.
const calendarAutosyncRunTimeout = 3 * time.Minute

// startCalendarAutosync launches one calendarAutosyncLoop per account that
// has OAuth configured, a supported calendar provider, and autosync enabled
// (per-account override, else the global calendar.autosync setting).
func startCalendarAutosync(ctx context.Context, hub *handler.EventHub, cfg *config.Config) {
	interval := cfg.CalendarAutosyncInterval()
	started := 0
	for i := range cfg.Accounts {
		account := &cfg.Accounts[i]
		if account.OAuth == nil {
			continue
		}
		if !cfg.CalendarAutosyncEnabled(account) {
			slog.Debug("Calendar autosync disabled for account", "module", "CALSYNC", // encgrep:allow static message text, no content
				"account", account.GetAliasOrName()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			continue
		}
		// Support check only — each tick constructs its provider fresh.
		if _, err := newCalendarProvider(account); err != nil {
			slog.Debug("Calendar autosync skipped, provider unsupported", "module", "CALSYNC",
				"account", account.GetAliasOrName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			continue
		}
		go calendarAutosyncLoop(ctx, hub, account, cfg, interval)
		started++
	}
	if started > 0 {
		slog.Info("Started calendar autosync loops", "module", "CALSYNC",
			"accounts", started, "interval", interval) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
}

// calendarAutosyncLoop runs autosync cycles for one account until
// ctx is cancelled: a jittered initial delay (30-90 s, so calendar sync does
// not stampede with mail sync at server startup), then one run per ticker
// interval. Errors never stop the loop — they are logged and the next tick
// retries; an auth error logs the consent hint once and then goes quiet
// instead of spamming the log every tick.
func calendarAutosyncLoop(ctx context.Context, hub *handler.EventHub, account *config.AccountConfig, cfg *config.Config, interval time.Duration) {
	// math/rand jitter — scheduling only, nothing security-sensitive.
	initialDelay := 30*time.Second + rand.N(60*time.Second)
	select {
	case <-ctx.Done():
		return
	case <-time.After(initialDelay):
	}

	// In-process overlap guard (mirrors WatcherManager.accountLock): the
	// ticker never overlaps runs by itself, but a still-running previous
	// cycle must make the next tick skip, not queue.
	var running sync.Mutex
	var authHintLogged bool

	runOnce := func() {
		if !running.TryLock() {
			slog.Debug("Calendar autosync still running, skipping tick", "module", "CALSYNC",
				"account", account.GetAliasOrName()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			return
		}
		defer running.Unlock()
		calendarAutosyncOnce(ctx, hub, account, cfg, &authHintLogged)
	}

	runOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// calendarAutosyncOnce executes one autosync cycle for the account: run lock
// -> Load -> PlanAll -> FilterDownloadOnly (or FilterNonNotifying in safe
// upload mode) -> ApplyAll -> Save, then an SSE calendar_updated broadcast
// when anything changed (downloads, prunes, auto-applied uploads). Deferred
// remote mutations are only counted/logged — they wait for the interactive
// `durian calendar sync`.
func calendarAutosyncOnce(ctx context.Context, hub *handler.EventHub, account *config.AccountConfig, cfg *config.Config, authHintLogged *bool) {
	accountDir := filepath.Join(config.CalendarBaseDir(cfg, ""), account.CalendarDir())

	// Cross-process run lock: never plan/apply concurrently with a manual
	// `durian calendar sync` (or another serve) on the same directory.
	release, ok, err := calendarsync.AcquireRunLock(accountDir)
	if err != nil {
		slog.Warn("Calendar autosync run lock failed", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}
	if !ok {
		slog.Debug("Calendar autosync skipped, another sync holds the run lock", "module", "CALSYNC",
			"account", account.GetAliasOrName()) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, calendarAutosyncRunTimeout)
	defer cancel()

	provider, err := newCalendarProvider(account)
	if err != nil {
		slog.Warn("Calendar autosync provider construction failed", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	store := calendarsync.NewFileStateStore(accountDir)
	state, err := store.Load()
	if err != nil {
		slog.Warn("Calendar autosync failed to load state", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	plans, err := calendarsync.PlanAll(runCtx, provider, accountDir, account.CalendarInclude(), state)
	if err != nil {
		logCalendarAutosyncError(provider, account, "planning", err, authHintLogged)
		return
	}

	// The safety mechanism: ApplyAll below only ever sees the filtered plans.
	// Default mode strips every action that could write to the remote
	// calendar; safe upload mode keeps only the provably non-notifying,
	// non-delete uploads and defers everything else.
	uploadSafe := cfg.CalendarAutosyncUploadSafe(account)
	var filtered []calendarsync.CalendarPlan
	var autoUploads, deferred int
	if uploadSafe {
		filtered, autoUploads, deferred = calendarsync.FilterNonNotifying(plans)
	} else {
		filtered, deferred = calendarsync.FilterDownloadOnly(plans)
	}

	// Pending-push visibility (both modes): the deferred local changes wait
	// for the interactive confirmation gate. Per-action detail at Debug —
	// action kind and event UID only, never event content.
	if deferred > 0 {
		slog.Info("Calendar autosync: local change(s) pending manual push, run 'durian calendar sync <account>' to apply them", "module", "CALSYNC", // encgrep:allow static message text, no content
			"account", account.GetAliasOrName(), "pending", deferred, "uploadSafe", uploadSafe) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		for _, p := range plans {
			for _, a := range p.Actions {
				if (uploadSafe && a.AutoDeferred()) || (!uploadSafe && a.RemoteMutation()) {
					slog.Debug("Calendar autosync deferred action", "module", "CALSYNC",
						"account", account.GetAliasOrName(), "kind", a.Kind, "uid", a.UID) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				}
			}
		}
	}
	if autoUploads > 0 {
		slog.Info("Calendar autosync auto-applying non-notifying local change(s)", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "actions", autoUploads) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}

	stats, applyErr := calendarsync.ApplyAll(runCtx, provider, state, filtered, calendarsync.SyncOptions{})
	// Save also on partial failure: completed local writes are recorded and
	// must not be misread as local edits on the next run.
	if saveErr := store.Save(state); saveErr != nil {
		slog.Warn("Calendar autosync failed to save state", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "err", saveErr) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
	if applyErr != nil {
		logCalendarAutosyncError(provider, account, "apply", applyErr, authHintLogged)
		return
	}
	*authHintLogged = false // a full clean cycle resets the auth-hint damper

	if stats.Downloaded+stats.Pruned+stats.Uploaded > 0 {
		slog.Info("Calendar autosync updated calendars", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "downloaded", stats.Downloaded, "pruned", stats.Pruned, "uploaded", stats.Uploaded) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		hub.BroadcastCalendar(handler.CalendarUpdatedEvent{
			Account:    account.GetAliasOrName(),
			Downloaded: stats.Downloaded,
			Pruned:     stats.Pruned,
			Uploaded:   stats.Uploaded,
		})
	}
}

// logCalendarAutosyncError logs one failed autosync phase: auth/consent
// problems get the `durian auth login` hint exactly once and then only a
// Debug line per tick (the loop keeps ticking, it does not spin); everything
// else is a Warn each time.
func logCalendarAutosyncError(provider calendarsync.CalendarProvider, account *config.AccountConfig, phase string, err error, authHintLogged *bool) {
	if provider.IsAuthError(err) {
		if !*authHintLogged {
			*authHintLogged = true
			slog.Warn("Calendar autosync auth failed — calendar access may not be granted, run 'durian auth login' for this account to consent", "module", "CALSYNC", // encgrep:allow static message text, no content
				"account", account.GetAliasOrName(), "phase", phase, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		} else {
			slog.Debug("Calendar autosync auth still failing", "module", "CALSYNC",
				"account", account.GetAliasOrName(), "phase", phase) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		}
		return
	}
	slog.Warn("Calendar autosync failed", "module", "CALSYNC",
		"account", account.GetAliasOrName(), "phase", phase, "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
}
