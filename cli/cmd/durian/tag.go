package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/spf13/cobra"
)

var tagAccountFilter string

var tagListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all tags",
	Example: `  durian tag list
  durian tag list --account work
  durian tag list --account work,personal`,
	RunE: runTagList,
}

var tagCmd = &cobra.Command{
	Use:   "tag <query> <tags...>",
	Short: "Modify tags on emails",
	Long:  "Add or remove tags. Tags must be prefixed with + (add) or - (remove).",
	Example: `  durian tag "thread:00000000000022ca" +read
  durian tag "thread:00000000000022ca" +read -unread
  durian tag "tag:inbox" +archived -inbox
  durian tag "from:alice@example.com" +important`,
	Args: cobra.MinimumNArgs(2),
	RunE: runTag,
}

func init() {
	tagCmd.Flags().SetInterspersed(false)
	tagListCmd.Flags().StringVar(&tagAccountFilter, "account", "", "filter by account (comma-separated)")
	_ = tagListCmd.RegisterFlagCompletionFunc("account", completeAccounts)
	tagCmd.AddCommand(tagListCmd)
	rootCmd.AddCommand(tagCmd)
}

func runTagList(cmd *cobra.Command, args []string) error {
	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	h := handler.New(emailDB, nil)

	var resp protocol.Response
	if tagAccountFilter != "" {
		resp = h.ListTagsForAccounts(strings.Split(tagAccountFilter, ","))
	} else {
		resp = h.ListTags()
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}

	for _, tag := range resp.Tags {
		fmt.Println(tag)
	}
	return nil
}

func runTag(cmd *cobra.Command, args []string) error {
	query := args[0]
	tags := args[1:]

	// Validate tags
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "+") && !strings.HasPrefix(tag, "-") {
			fmt.Fprintf(os.Stderr, "Error: invalid tag format: %q (must start with + or -)\n", tag)
			os.Exit(2)
		}
	}

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	h := handler.New(emailDB, nil)

	// Enable tag journal if tag sync is configured
	cfg := GetConfig()
	if cfg != nil && cfg.Sync.TagSync != nil && cfg.Sync.TagSync.URL != "" {
		h.EnableTagJournal()
	}

	// Join tags back to string for handler (current interface expects string)
	tagsStr := strings.Join(tags, " ")
	resp := h.Tag(query, tagsStr)

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if !jsonOutput {
		fmt.Println("Tags applied successfully")
	}

	return nil
}
