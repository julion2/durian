package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/imapbackend"
	"github.com/julion2/durian/cli/internal/syncengine"
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
// sync_engine setting selects: the Microsoft Graph backend ("graph") or the IMAP
// backend ("engine"). Cursors are namespaced per backend so the two never read
// each other's incompatible cursor payloads.
func syncOneWithEngine(ctx context.Context, account *config.AccountConfig, options *imap.SyncOptions) *imap.SyncResult {
	start := time.Now()
	result := &imap.SyncResult{Account: account.AccountIdentifier()}

	var (
		b       backend.Backend
		cursors syncengine.CursorStore
		err     error
	)
	if account.UsesGraphBackend() {
		b, err = graphbackend.New(account)
		cursors = syncengine.NewFileCursorStoreWithSuffix(account.AccountIdentifier(), "-graph")
	} else {
		b, err = imapbackend.New(account)
		cursors = syncengine.NewFileCursorStore(account.AccountIdentifier())
	}
	if err != nil {
		result.Error = fmt.Errorf("connect backend: %w", err)
		result.Duration = time.Since(start)
		return result
	}
	defer b.Close()

	engine := syncengine.New(syncengine.Options{
		Store:        options.Store,
		Cursors:      cursors,
		Account:      account.AccountIdentifier(),
		BatchLimit:   account.GetIMAPBatchSize(),
		MaxPerFolder: account.GetIMAPMaxMessages(),
		Mode:         engineMode(options.Mode),
		Folders:      options.Mailboxes,
		DryRun:       options.DryRun,
		NoFlags:      options.NoFlags,
		Ingest: syncengine.IngestOptions{
			Account:        account.AccountIdentifier(),
			FilterRules:    options.FilterRules,
			Groups:         options.Groups,
			IndexedHeaders: options.IndexedHeaders,
		},
	})

	res, err := engine.Sync(ctx, b)
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
