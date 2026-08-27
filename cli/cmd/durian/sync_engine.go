package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/julion2/durian/cli/internal/backendfactory"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/syncengine"
	"golang.org/x/term"
)

// runEngineSync syncs accounts through the provider-agnostic sync engine
// (backend.Backend) instead of the legacy imap.Syncer. Results are mapped into
// imap.SyncResult so the caller's output/JSON code stays unchanged. Per-account
// failures are captured in the result's Error field and the run continues to
// the next account (parity with imap.SyncAccounts).
//
// --backfill-headers is legacy-only: the engine indexes headers on every
// ingest, so there is nothing to backfill on this path (see the warning below).
func runEngineSync(ctx context.Context, accounts []*config.AccountConfig, options *imap.SyncOptions) []*imap.SyncResult {
	if options.BackfillHeaders && len(accounts) > 0 {
		slog.Warn("--backfill-headers is not supported on the engine sync path: headers are indexed at ingest; use a legacy account or re-sync", "module", "SYNCENGINE") // encgrep:allow static message text contains the word "account", no sensitive value logged
	}
	results := make([]*imap.SyncResult, 0, len(accounts))
	for _, account := range accounts {
		results = append(results, syncOneWithEngine(ctx, account, options))
	}
	return results
}

// syncOneWithEngine runs a single account through the engine on the backend its
// sync_engine setting selects: Microsoft Graph, Gmail REST, JMAP, or IMAP.
// Cursors are namespaced per backend so implementations never read each other's
// incompatible cursor payloads.
func syncOneWithEngine(ctx context.Context, account *config.AccountConfig, options *imap.SyncOptions) *imap.SyncResult {
	start := time.Now()
	result := &imap.SyncResult{Account: account.AccountIdentifier()}

	var (
		cursors syncengine.CursorStore
		err     error
	)
	b, err := backendfactory.New(account)
	if suffix := backendfactory.CursorSuffix(account); suffix != "" {
		cursors = syncengine.NewFileCursorStoreWithSuffix(account.AccountIdentifier(), suffix)
	} else {
		cursors = syncengine.NewFileCursorStore(account.AccountIdentifier())
	}
	if err != nil {
		result.Error = fmt.Errorf("connect backend: %w", err)
		result.Duration = time.Since(start)
		return result
	}
	defer b.Close()

	// A live progress line with a heartbeat spinner (unless --quiet): the spinner
	// ticks on a timer even while a page is still downloading, so a slow page
	// never looks frozen — a moving spinner with a static count means "alive but
	// throttled", a frozen spinner means the process is stuck. stderr keeps
	// stdout clean for --json.
	var (
		onProgress  func(int)
		synced      atomic.Int64
		progDone    chan struct{}
		progStopped chan struct{}
	)
	if shouldShowSyncProgress() {
		onProgress = func(n int) { synced.Store(int64(n)) }
		progDone = make(chan struct{})
		progStopped = make(chan struct{})
		go func() {
			defer close(progStopped)
			spin := []rune{'|', '/', '-', '\\'}
			t := time.NewTicker(400 * time.Millisecond)
			defer t.Stop()
			for i := 0; ; i++ {
				select {
				case <-progDone:
					return
				case <-t.C:
					fmt.Fprintf(os.Stderr, "\r%-55s",
						fmt.Sprintf("%s: %d messages synced %c", account.AccountIdentifier(), synced.Load(), spin[i%len(spin)]))
				}
			}
		}()
	}

	engine := syncengine.New(syncengine.Options{
		Store:      options.Store,
		Cursors:    cursors,
		Account:    account.AccountIdentifier(),
		BatchLimit: account.GetIMAPBatchSize(),
		// Raw max_messages: 0 = unlimited (the gmail template sets it for a full
		// offline mailbox), a positive value caps the first sync. The legacy
		// syncer keeps its own GetIMAPMaxMessages 5000 default.
		MaxPerFolder: account.IMAP.MaxMessages,
		Mode:         engineMode(options.Mode),
		Folders:      options.Mailboxes,
		DryRun:       options.DryRun,
		NoFlags:      options.NoFlags,
		OnProgress:   onProgress,
		Ingest: syncengine.IngestOptions{
			Account:        account.AccountIdentifier(),
			FilterRules:    options.FilterRules,
			Groups:         options.Groups,
			IndexedHeaders: options.IndexedHeaders,
		},
	})

	res, err := engine.Sync(ctx, b)
	if progDone != nil {
		close(progDone)
		<-progStopped // wait for the renderer to exit so it can't overwrite the final line
		// Clear the spinner and print the final count.
		fmt.Fprintf(os.Stderr, "\r%-55s\n", fmt.Sprintf("%s: %d messages synced", account.AccountIdentifier(), synced.Load()))
	}
	result.Duration = time.Since(start)
	if err != nil {
		result.Error = err
		return result
	}
	result.TotalNew = res.New
	result.TotalDeduplicated = res.Deduplicated
	result.TotalDeleted = res.Deleted
	result.TotalMoved = res.Moved
	if len(res.Errors) > 0 {
		// Surface the first per-folder error; the engine already logged them all.
		result.Error = fmt.Errorf("%d folder error(s), first: %w", len(res.Errors), res.Errors[0])
	}
	return result
}

func shouldShowSyncProgress() bool {
	return !syncQuiet && term.IsTerminal(int(os.Stderr.Fd()))
}

// engineMode maps the legacy imap.SyncMode to the engine's Mode.
func engineMode(m imap.SyncMode) syncengine.Mode {
	switch m {
	case imap.SyncDownloadOnly:
		return syncengine.DownloadOnly
	case imap.SyncUploadOnly:
		return syncengine.UploadOnly
	default:
		return syncengine.Bidirectional
	}
}
