// Background download-only calendar autosync for `durian serve`.
//
// Each eligible account gets one loop that periodically pulls remote calendar
// changes into the local vdir. The loop is DOWNLOAD-ONLY by construction:
// every planned action passes through calendarsync.FilterDownloadOnly before
// ApplyAll, which strips every action whose RemoteMutation() is true —
// uploads, remote deletes, conflicts, RSVPs. Those remain exclusively behind
// the interactive `durian calendar sync` confirmation gate. This file must
// never call ApplyAll on an unfiltered plan.

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
		slog.Info("Started calendar autosync loops (download-only)", "module", "CALSYNC",
			"accounts", started, "interval", interval) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
}

// calendarAutosyncLoop runs download-only sync cycles for one account until
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

// calendarAutosyncOnce executes one download-only sync cycle for the account:
// run lock -> Load -> PlanAll -> FilterDownloadOnly -> ApplyAll -> Save, then
// an SSE calendar_updated broadcast when anything changed locally. Suppressed
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

	// The safety mechanism: strip every action that could write to the
	// remote calendar. ApplyAll below only ever sees the filtered plans.
	filtered, suppressed := calendarsync.FilterDownloadOnly(plans)

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

	if suppressed > 0 {
		slog.Info("Calendar autosync suppressed remote mutations, run 'durian calendar sync' to review them", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "suppressed", suppressed) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	}
	if stats.Downloaded+stats.Pruned > 0 {
		slog.Info("Calendar autosync updated local calendars", "module", "CALSYNC",
			"account", account.GetAliasOrName(), "downloaded", stats.Downloaded, "pruned", stats.Pruned) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		hub.BroadcastCalendar(handler.CalendarUpdatedEvent{
			Account:    account.GetAliasOrName(),
			Downloaded: stats.Downloaded,
			Pruned:     stats.Pruned,
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
