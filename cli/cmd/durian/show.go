package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/spf13/cobra"
)

var showHTML bool

var showCmd = &cobra.Command{
	Use:   "show <thread-id>",
	Short: "Display email thread content",
	Long:  "Display the content of an email thread by its thread ID.",
	Example: `  durian show 00000000000022ca
  durian show 00000000000022ca --html
  durian show 00000000000022ca --json`,
	Args: cobra.ExactArgs(1),
	RunE: runShow,
}

func init() {
	showCmd.Flags().BoolVar(&showHTML, "html", false, "show HTML body instead of plain text")
	rootCmd.AddCommand(showCmd)
}

func runShow(cmd *cobra.Command, args []string) error {
	threadID := args[0]

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	h := handler.New(emailDB, nil)

	// Use new ShowThread for full thread support
	resp := h.ShowThread(threadID)

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		os.Exit(1)
	}

	if resp.Thread == nil {
		fmt.Fprintln(os.Stderr, "Error: no thread content returned")
		os.Exit(1)
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Thread)
	}

	return outputThreadFormatted(resp.Thread)
}

func outputThreadFormatted(t *mail.ThreadContent) error {
	fmt.Printf("Thread: %s\n", t.ThreadID)
	fmt.Printf("Subject: %s\n", t.Subject)
	fmt.Printf("Messages: %d\n", len(t.Messages))
	fmt.Println(strings.Repeat("=", 60))

	for i, msg := range t.Messages {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(t.Messages), msg.Date)
		fmt.Printf("From: %s\n", msg.From)
		if msg.To != "" {
			fmt.Printf("To:   %s\n", msg.To)
		}
		if len(msg.Attachments) > 0 {
			names := make([]string, len(msg.Attachments))
			for i, a := range msg.Attachments {
				names[i] = a.Filename
			}
			fmt.Printf("Attachments: %s\n", strings.Join(names, ", "))
		}
		fmt.Println(strings.Repeat("-", 40))

		if showHTML && msg.HTML != "" {
			fmt.Println(msg.HTML)
		} else if msg.Body != "" {
			fmt.Println(msg.Body)
		} else if msg.HTML != "" {
			fmt.Println("[HTML-only message - use --html to view]")
		} else {
			fmt.Println("[No content]")
		}

		if i < len(t.Messages)-1 {
			fmt.Println()
		}
	}

	return nil
}
