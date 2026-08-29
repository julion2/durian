package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/spf13/cobra"
)

var (
	tagAccountFilter []string
	tagAccounts      []string
	tagDryRun        bool
	tagAll           bool
)

var tagListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all tags",
	Example: `  durian tag list
  durian tag list --account work
  durian tag list --account work --account personal`,
	RunE: runTagList,
}

var tagCmd = &cobra.Command{
	Use:   "tag <query> <tags...>",
	Short: "Modify tags on emails",
	Long:  "Add or remove tags. Tags must be prefixed with + (add) or - (remove).",
	Example: `  durian tag "thread:00000000000022ca" +read
  durian tag "thread:00000000000022ca" +read -unread
  durian tag --account work --dry-run "tag:inbox" +archived -inbox
  durian tag "from:alice@example.com" +important`,
	Args: cobra.MinimumNArgs(2),
	RunE: runTag,
}

func init() {
	tagCmd.Flags().SetInterspersed(false)
	tagCmd.Flags().StringArrayVarP(&tagAccounts, "account", "a", nil, "limit the mutation to an account (repeatable)")
	tagCmd.Flags().BoolVar(&tagDryRun, "dry-run", false, "show the effect without changing tags")
	tagCmd.Flags().BoolVar(&tagAll, "all", false, "allow an unbounded '*' selector")
	tagListCmd.Flags().StringSliceVarP(&tagAccountFilter, "account", "a", nil, "filter by account (repeatable or comma-separated)")
	_ = tagCmd.RegisterFlagCompletionFunc("account", completeAccounts)
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
	if len(tagAccountFilter) > 0 {
		accounts, err := resolveAccountStoreKeys(tagAccountFilter)
		if err != nil {
			return err
		}
		resp = h.ListTagsForAccounts(accounts)
	} else {
		resp = h.ListTags()
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if jsonOutput {
		tags := resp.Tags
		if tags == nil {
			tags = []string{}
		}
		return writeJSON(tags)
	}

	for _, tag := range resp.Tags {
		fmt.Println(tag)
	}
	return nil
}

func runTag(cmd *cobra.Command, args []string) error {
	query := args[0]
	tags := args[1:]
	if strings.TrimSpace(query) == "*" && !tagAll {
		return fmt.Errorf("an unbounded tag selector requires --all")
	}
	// The flag narrows the search through the query, and is also handed to the
	// handler as the mutation scope. Both, not either: the query form cannot be
	// told apart from a user writing path: themselves, so the handler must be
	// given the scope rather than left to infer it.
	accountScope, err := resolveAccountStoreKeys(tagAccounts)
	if err != nil {
		return err
	}
	query, err = scopeQueryByAccounts(query, tagAccounts)
	if err != nil {
		return err
	}

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
	var resp protocol.Response
	if tagDryRun {
		resp = h.PreviewTag(query, tagsStr, accountScope)
	} else {
		resp = h.Tag(query, tagsStr, accountScope)
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if jsonOutput {
		return writeJSON(struct {
			MatchedThreads int      `json:"matched_threads"`
			ChangedThreads int      `json:"changed_threads"`
			AddedTags      []string `json:"added_tags"`
			RemovedTags    []string `json:"removed_tags"`
			DryRun         bool     `json:"dry_run"`
		}{
			MatchedThreads: *resp.MatchedThreads,
			ChangedThreads: *resp.ChangedThreads,
			AddedTags:      tagsWithPrefix(tags, "+"),
			RemovedTags:    tagsWithPrefix(tags, "-"),
			DryRun:         tagDryRun,
		})
	}

	if tagDryRun {
		fmt.Printf("Would update %d of %d matching threads\nNo changes applied.\n", *resp.ChangedThreads, *resp.MatchedThreads)
		return nil
	}
	fmt.Printf("Updated %d of %d matching threads\n", *resp.ChangedThreads, *resp.MatchedThreads)
	return nil
}

func tagsWithPrefix(tags []string, prefix string) []string {
	out := make([]string, 0)
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			out = append(out, strings.TrimPrefix(tag, prefix))
		}
	}
	return out
}
