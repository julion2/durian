package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/durian-dev/durian/cli/internal/store"
	"github.com/durian-dev/durian/cli/internal/tagsync"
)

var tagSyncCmd = &cobra.Command{
	Use:   "tag-sync",
	Short: "Manage remote tag synchronization",
}

var tagSyncPushAllCmd = &cobra.Command{
	Use:   "push-all",
	Short: "Push all local tags to the sync server (initial sync)",
	RunE:  runTagSyncPushAll,
}

func init() {
	tagSyncCmd.AddCommand(tagSyncPushAllCmd)
	rootCmd.AddCommand(tagSyncCmd)
}

func runTagSyncPushAll(cmd *cobra.Command, args []string) error {
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
