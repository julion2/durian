package main

import (
	"context"
	"fmt"
	"time"

	"github.com/julion2/durian/cli/internal/config"
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
// Not yet honored on this path (legacy-only for now): --no-flags and
// --backfill-headers. The engine always runs its download-side flag pass unless
// the mode is upload-only or dry-run.
func runEngineSync(ctx context.Context, accounts []*config.AccountConfig, options *imap.SyncOptions) []*imap.SyncResult {
	results := make([]*imap.SyncResult, 0, len(accounts))
	for _, account := range accounts {
		results = append(results, syncOneWithEngine(ctx, account, options))
	}
	return results
}

// syncOneWithEngine runs a single account through imapbackend + syncengine.
func syncOneWithEngine(ctx context.Context, account *config.AccountConfig, options *imap.SyncOptions) *imap.SyncResult {
	start := time.Now()
	result := &imap.SyncResult{Account: account.AccountIdentifier()}

	b, err := imapbackend.New(account)
	if err != nil {
		result.Error = fmt.Errorf("connect backend: %w", err)
		result.Duration = time.Since(start)
		return result
	}
	defer b.Close()

	engine := syncengine.New(syncengine.Options{
		Store:        options.Store,
		Cursors:      syncengine.NewFileCursorStore(account.AccountIdentifier()),
		Account:      account.AccountIdentifier(),
		BatchLimit:   account.GetIMAPBatchSize(),
		MaxPerFolder: account.GetIMAPMaxMessages(),
		Mode:         engineMode(options.Mode),
		Folders:      options.Mailboxes,
		DryRun:       options.DryRun,
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
	result.TotalDeleted = res.Deleted
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
