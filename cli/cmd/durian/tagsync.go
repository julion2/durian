package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/tagsync"
)

var tagSyncCmd = &cobra.Command{
	Use:   "tag-sync",
	Short: "Manage remote tag synchronization",
	Long: `Replicate Durian's local tags to a self-hosted tag-sync server so they
follow you across machines. Configuration lives in config.pkl under
sync { tag_sync { url; api_key } }.

Incremental push/pull happen automatically during 'durian sync' and while
'durian serve' is running — this command group is only for the one-shot
'init' bootstrap that seeds a fresh server from an existing local DB.`,
}

var tagSyncInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Bulk-push all local tags to the sync server (one-shot bootstrap)",
	Long: `Push every local tag in one batch to the configured tag-sync server.

Run this once after pointing a new machine at the server, or after standing
up a fresh server, to seed it from an existing local DB. Routine
incremental sync is handled by 'durian sync' and 'durian serve' — you do
not need to call 'init' again on the same server.`,
	Example: `  durian tag-sync init`,
	RunE:    runTagSyncInit,
}

// tagSyncPushAllCmd is the previous name for 'init' — kept as a hidden,
// deprecated alias for one release so existing scripts keep working.
var tagSyncPushAllCmd = &cobra.Command{
	Use:        "push-all",
	Short:      "Deprecated alias for 'tag-sync init'",
	Hidden:     true,
	Deprecated: "use 'durian tag-sync init' instead",
	RunE:       runTagSyncInit,
}

func init() {
	tagSyncCmd.AddCommand(tagSyncInitCmd)
	tagSyncCmd.AddCommand(tagSyncPushAllCmd)
	rootCmd.AddCommand(tagSyncCmd)
}

func runTagSyncInit(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return fmt.Errorf("no configuration loaded")
	}

	if cfg.Sync.TagSync == nil || cfg.Sync.TagSync.URL == "" || cfg.Sync.TagSync.APIKey == "" {
		return fmt.Errorf("tag sync not configured — add sync { tag_sync { url; api_key } } to config.pkl")
	}

	// Open store
	db, err := store.Open(store.DefaultDBPath(), bootstrapKeyring())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()

	// Export all tags
	allTags, err := db.ExportAllTags()
	if err != nil {
		return fmt.Errorf("export tags: %w", err)
	}

	if len(allTags) == 0 {
		fmt.Println("No tags to push.")
		return nil
	}

	// Convert to TagChanges
	now := time.Now().Unix()
	changes := make([]tagsync.TagChange, len(allTags))
	for i, t := range allTags {
		changes[i] = tagsync.TagChange{
			MessageID: t.MessageID,
			Account:   t.Account,
			Tag:       t.Tag,
			Action:    "add",
			Timestamp: now,
		}
	}

	// Push in batches
	client := tagsync.NewClient(cfg.Sync.TagSync.URL, cfg.Sync.TagSync.APIKey)
	const batchSize = 500
	pushed := 0
	for i := 0; i < len(changes); i += batchSize {
		end := i + batchSize
		if end > len(changes) {
			end = len(changes)
		}
		if err := client.Push(changes[i:end]); err != nil {
			return fmt.Errorf("push batch %d-%d: %w", i, end, err)
		}
		pushed += end - i
		slog.Debug("Pushed batch", "module", "TAGSYNC", "progress", fmt.Sprintf("%d/%d", pushed, len(changes)))
	}

	fmt.Printf("Pushed %d tags to %s\n", pushed, cfg.Sync.TagSync.URL)
	return nil
}
