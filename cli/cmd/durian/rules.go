package main

import (
	"fmt"
	"net/mail"
	"os"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
)

var rulesApplyDryRun bool

var rulesCmd = &cobra.Command{
	Use:   "rules",
	Short: "Manage filter rules",
}

var rulesApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply filter rules to all existing messages",
	Long:  "Apply filter rules from rules.pkl. Use --dry-run to preview.",
	Example: `  durian rules apply --dry-run
  durian rules apply`,
	RunE: runRulesApply,
}

func init() {
	rulesApplyCmd.Flags().BoolVar(&rulesApplyDryRun, "dry-run", false, "show what would be changed without applying")

	rulesCmd.AddCommand(rulesApplyCmd)
	rootCmd.AddCommand(rulesCmd)
}

func runRulesApply(cmd *cobra.Command, args []string) error {
	rules, err := config.LoadRules("")
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	if len(rules) == 0 {
		fmt.Fprintln(os.Stderr, "No rules found in rules.pkl")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Loaded %d rules\n", len(rules))

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("failed to open email store: %w", err)
	}
	defer emailDB.Close()

	// Load everything in 3 queries
	msgs, err := emailDB.AllMessages()
	if err != nil {
		return fmt.Errorf("load messages: %w", err)
	}

	headers, err := emailDB.AllHeadersByMessage()
	if err != nil {
		return fmt.Errorf("load headers: %w", err)
	}

	attachments, err := emailDB.AttachmentsByMessage()
	if err != nil {
		return fmt.Errorf("load attachments: %w", err)
	}

	groups, _ := config.LoadGroups("")

	fmt.Fprintf(os.Stderr, "Processing %d messages...\n\n", len(msgs))

	// Track per-rule stats
	type ruleStats struct {
		name    string
		adds    map[string]int // tag → count
		removes map[string]int
	}
	stats := make(map[string]*ruleStats)
	for _, r := range rules {
		stats[r.Name] = &ruleStats{name: r.Name, adds: make(map[string]int), removes: make(map[string]int)}
	}

	totalAdds := 0
	totalRemoves := 0

	for _, msg := range msgs {
		hdr := mail.Header(headers[msg.ID])
		var atts []imap.RuleAttachment
		for _, a := range attachments[msg.ID] {
			atts = append(atts, imap.RuleAttachment{ContentType: a.ContentType, Filename: a.Filename})
		}
		matched := imap.MatchingRules(rules, msg, atts, hdr, msg.Account, groups)

		for _, rule := range matched {
			for _, tag := range rule.AddTags {
				if !rulesApplyDryRun {
					if err := emailDB.AddTag(msg.ID, tag); err != nil {
						return fmt.Errorf("add tag %q: %w", tag, err)
					}
				}
				stats[rule.Name].adds[tag]++
				totalAdds++
			}
			for _, tag := range rule.RemoveTags {
				if !rulesApplyDryRun {
					if err := emailDB.RemoveTag(msg.ID, tag); err != nil {
						return fmt.Errorf("remove tag %q: %w", tag, err)
					}
				}
				stats[rule.Name].removes[tag]++
				totalRemoves++
			}
		}
	}

	// Print summary
	if rulesApplyDryRun {
		fmt.Fprintf(os.Stderr, "=== Dry Run ===\n")
	} else {
		fmt.Fprintf(os.Stderr, "=== Applied ===\n")
	}

	for _, r := range rules {
		s := stats[r.Name]
		hasChanges := len(s.adds) > 0 || len(s.removes) > 0
		if !hasChanges {
			fmt.Fprintf(os.Stderr, "  %s: no matches\n", r.Name)
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s:\n", r.Name)
		for tag, count := range s.adds {
			fmt.Fprintf(os.Stderr, "    +%s  %d messages\n", tag, count)
		}
		for tag, count := range s.removes {
			fmt.Fprintf(os.Stderr, "    -%s  %d messages\n", tag, count)
		}
	}

	fmt.Fprintf(os.Stderr, "\nTotal: %d tag adds, %d tag removes across %d messages\n", totalAdds, totalRemoves, len(msgs))

	return nil
}
