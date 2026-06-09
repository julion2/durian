package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/tagsync"
)

var (
	syncDryRun               bool
	syncQuiet                bool
	syncNoFlags              bool
	syncDownloadOnly         bool
	syncUploadOnly           bool
	syncBackfillHeaders      bool
	syncBackfillHeadersForce bool
)

var syncCmd = &cobra.Command{
	Use:   "sync [account] [mailbox]",
	Short: "Sync email via IMAP",
	Long:  "Sync email from IMAP to local SQLite. Bidirectional by default.",
	Example: `  durian sync
  durian sync gmail
  durian sync you@company.com
  durian sync gmail INBOX
  durian sync --download-only
  durian sync --upload-only
  durian sync --no-flags
  durian sync --dry-run`,
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeAccounts(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: runSync,
}

func init() {
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "show what would be synced without syncing")
	syncCmd.Flags().BoolVarP(&syncQuiet, "quiet", "q", false, "suppress progress output")
	syncCmd.Flags().BoolVar(&syncNoFlags, "no-flags", false, "skip flag synchronization")
	syncCmd.Flags().BoolVar(&syncDownloadOnly, "download-only", false, "only download from server (no flag upload)")
	syncCmd.Flags().BoolVar(&syncUploadOnly, "upload-only", false, "only upload local changes to server")
	syncCmd.Flags().BoolVar(&syncBackfillHeaders, "backfill-headers", false, "fetch and store headers for messages that don't have any yet")
	syncCmd.Flags().BoolVar(&syncBackfillHeadersForce, "force", false, "with --backfill-headers, re-fetch headers for ALL messages even if they already have some (needed after changing sync.indexed_headers in config.pkl)")

	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	// Load config
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}

	// Load filter rules (non-fatal if missing)
	rules, err := config.LoadRules("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load rules: %v\n", err)
	}

	// Determine sync mode
	mode := imap.SyncBidirectional
	if syncDownloadOnly {
		mode = imap.SyncDownloadOnly
	} else if syncUploadOnly {
		mode = imap.SyncUploadOnly
	}

	// Open email store (required)
	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("failed to open email store: %w", err)
	}
	defer emailDB.Close()

	// Build sync options
	options := &imap.SyncOptions{
		DryRun:               syncDryRun,
		Quiet:                syncQuiet,
		NoFlags:              syncNoFlags,
		Mode:                 mode,
		Store:                emailDB,
		FilterRules:          rules,
		BackfillHeaders:      syncBackfillHeaders,
		BackfillHeadersForce: syncBackfillHeadersForce,
		IndexedHeaders:       GetConfig().Sync.IndexedHeaders,
		Groups:               func() map[string]config.GroupEntry { g, _ := config.LoadGroups(""); return g }(),
	}

	// Determine which accounts to sync
	var accounts []*config.AccountConfig

	if len(args) > 0 {
		// Sync specific account
		account, err := cfg.GetAccountByIdentifier(args[0])
		if err != nil {
			return fmt.Errorf("account not found: %s\nAvailable accounts: %s", args[0], cfg.ListAccountIdentifiers())
		}

		if account.IMAP.Host == "" {
			return fmt.Errorf("account %s has no IMAP configuration", account.Email)
		}

		accounts = append(accounts, account)

		// Check for specific mailbox
		if len(args) > 1 {
			options.Mailboxes = args[1:]
		}
	} else {
		// Sync all accounts with IMAP config
		accounts = cfg.GetAccountsWithIMAP()
		if len(accounts) == 0 {
			return fmt.Errorf("no accounts with IMAP configuration found")
		}
	}

	// Regular sync
	results, err := imap.SyncAccounts(accounts, options)
	if err != nil {
		return err
	}

	// Tag sync: push journal entries, then pull remote changes
	if cfg.Sync.TagSync != nil && cfg.Sync.TagSync.URL != "" && cfg.Sync.TagSync.APIKey != "" {
		client := tagsync.NewClient(cfg.Sync.TagSync.URL, cfg.Sync.TagSync.APIKey)
		client.SetStore(emailDB)

		// Push pending local changes from journal
		journal, journalErr := emailDB.ReadTagJournal()
		if journalErr == nil && len(journal) > 0 {
			changes := make([]tagsync.TagChange, len(journal))
			var maxID int64
			for i, j := range journal {
				changes[i] = tagsync.TagChange{
					MessageID: j.MessageID, Account: j.Account,
					Tag: j.Tag, Action: j.Action, Timestamp: j.Timestamp,
				}
				if j.ID > maxID {
					maxID = j.ID
				}
			}
			if err := client.Push(changes); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: tag sync push failed: %v\n", err)
			} else {
				emailDB.ClearTagJournal(maxID)
				fmt.Fprintf(os.Stderr, "✓ Pushed %d tag changes\n", len(changes))
			}
		}

		// Pull remote changes
		since := client.LoadLastSync()
		changes, syncAt, pullErr := client.Pull(since)
		if pullErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: tag sync pull failed: %v\n", pullErr)
		} else {
			applied := 0
			for _, c := range changes {
				switch c.Action {
				case "add":
					if err := emailDB.ModifyTagsByMessageIDAndAccount(c.MessageID, c.Account, []string{c.Tag}, nil); err == nil {
						applied++
					}
				case "remove":
					if err := emailDB.ModifyTagsByMessageIDAndAccount(c.MessageID, c.Account, nil, []string{c.Tag}); err == nil {
						applied++
					}
				}
			}
			client.SaveLastSync(syncAt)
			if applied > 0 {
				fmt.Fprintf(os.Stderr, "✓ Applied %d remote tag changes\n", applied)
			}
		}
	}

	if jsonOutput {
		type syncJSON struct {
			Account      string  `json:"account"`
			New          int     `json:"new"`
			Deleted      int     `json:"deleted"`
			Dedup        int     `json:"deduplicated"`
			FlagsUp      int     `json:"flags_uploaded"`
			FlagsDown    int     `json:"flags_downloaded"`
			Moved        int     `json:"moved"`
			DurationSecs float64 `json:"duration_secs"`
			Error        string  `json:"error,omitempty"`
		}
		var out []syncJSON
		for _, r := range results {
			s := syncJSON{
				Account:      r.Account,
				New:          r.TotalNew,
				Deleted:      r.TotalDeleted,
				Dedup:        r.TotalDeduplicated,
				FlagsUp:      r.FlagsUploaded,
				FlagsDown:    r.FlagsDownload,
				Moved:        r.TotalMoved,
				DurationSecs: r.Duration.Seconds(),
			}
			if r.Error != nil {
				s.Error = r.Error.Error()
			}
			out = append(out, s)
		}
		json.NewEncoder(os.Stdout).Encode(out)
	}

	// Check for errors
	for _, result := range results {
		if result.Error != nil {
			return fmt.Errorf("sync failed for %s: %w", result.Account, result.Error)
		}
	}

	return nil
}
