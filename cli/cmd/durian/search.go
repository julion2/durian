package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/protocol"
	"github.com/spf13/cobra"
)

var searchLimit int

var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search emails using notmuch query syntax",
	Long:  "Search the local email database using notmuch query syntax.",
	Example: `  durian search "tag:inbox"
  durian search "tag:inbox AND tag:unread"
  durian search "from:alice@example.com" --limit 10
  durian search "group:investor AND date:month"
  durian search "tag:unread" --json`,
	Args: cobra.MinimumNArgs(1),
	RunE: runSearch,
}

var countCmd = &cobra.Command{
	Use:   "count <query>",
	Short: "Count threads matching a query",
	Example: `  durian count "tag:inbox"
  durian count "tag:unread AND from:alice"
  durian count "group:investor AND date:month"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runCount,
}

func init() {
	searchCmd.Flags().IntVarP(&searchLimit, "limit", "l", 50, "maximum number of results")
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(countCmd)
}

func runSearch(cmd *cobra.Command, args []string) error {
	// Join all arguments to allow unquoted queries like: durian search tag:inbox AND date:today
	query := strings.Join(args, " ")

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	h := handler.New(emailDB, nil)

	// Load contact groups for group: query expansion
	if groups, err := config.LoadGroups(""); err == nil && len(groups) > 0 {
		h.SetGroups(groups)
	}

	resp := h.Search(query, searchLimit, 0)

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if jsonOutput {
		return outputSearchJSON(resp)
	}

	return outputSearchTable(resp)
}

func outputSearchJSON(resp protocol.Response) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(resp.Results)
}

func outputSearchTable(resp protocol.Response) error {
	if len(resp.Results) == 0 {
		fmt.Println("No results found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "THREAD\tSUBJECT\tFROM\tDATE\tTAGS")

	for _, mail := range resp.Results {
		subject := truncate(mail.Subject, 45)
		from := truncate(mail.From, 20)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			mail.ThreadID,
			subject,
			from,
			mail.Date,
			mail.Tags,
		)
	}

	return w.Flush()
}

func runCount(cmd *cobra.Command, args []string) error {
	query := strings.Join(args, " ")

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	// Expand groups before counting
	if groups, err := config.LoadGroups(""); err == nil && len(groups) > 0 {
		expanded, err := config.ExpandGroupsInQuery(query, groups)
		if err != nil {
			return fmt.Errorf("expand groups: %w", err)
		}
		query = expanded
	}

	count, err := emailDB.SearchCount(query)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}

	fmt.Println(count)
	return nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
