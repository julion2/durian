package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/spf13/cobra"
)

var (
	showHTML    bool
	showHeaders bool
	showHeader  string
)

var showCmd = &cobra.Command{
	Use:   "show <thread-id>",
	Short: "Display email thread content",
	Long:  "Display the content of an email thread by its thread ID.",
	Example: `  durian show 00000000000022ca
  durian show 00000000000022ca --html
  durian show 00000000000022ca --headers
  durian show 00000000000022ca --header list-id
  durian show 00000000000022ca --json`,
	Args: cobra.ExactArgs(1),
	RunE: runShow,
}

func init() {
	showCmd.Flags().BoolVar(&showHTML, "html", false, "show HTML body instead of plain text")
	showCmd.Flags().BoolVar(&showHeaders, "headers", false, "print all stored headers per message instead of the body (useful for writing rules)")
	showCmd.Flags().StringVar(&showHeader, "header", "", "print only this header (case-insensitive); implies --headers")
	rootCmd.AddCommand(showCmd)
}

func runShow(cmd *cobra.Command, args []string) error {
	threadID := args[0]

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	if showHeaders || showHeader != "" {
		return runShowHeaders(emailDB, threadID, showHeader)
	}

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

// runShowHeaders prints stored headers for every message in the thread.
// When filterName is non-empty, only headers whose canonical name matches
// (case-insensitive) are printed — handy for writing rules.pkl entries
// against things like List-Id or X-GitHub-Reason.
func runShowHeaders(emailDB *store.DB, threadID, filterName string) error {
	msgs, err := emailDB.GetByThread(threadID)
	if err != nil {
		return fmt.Errorf("get thread: %w", err)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("no messages found for thread %s", threadID)
	}

	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	headersByID, err := emailDB.HeadersByMessageDBIDs(ids)
	if err != nil {
		return fmt.Errorf("load headers: %w", err)
	}

	// JSON: nested by message_id (RFC-5322), preserving order.
	if jsonOutput {
		type messageHeaders struct {
			MessageID string              `json:"message_id"`
			Headers   map[string][]string `json:"headers"`
		}
		out := make([]messageHeaders, 0, len(msgs))
		for _, m := range msgs {
			h := headersByID[m.ID]
			if filterName != "" {
				h = filterHeaders(h, filterName)
			}
			out = append(out, messageHeaders{MessageID: m.MessageID, Headers: h})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Text: mailx-style. Per message: "[i/n] <message-id>", then "Name: value"
	// lines, blank line between messages.
	for i, m := range msgs {
		h := headersByID[m.ID]
		if filterName != "" {
			h = filterHeaders(h, filterName)
		}
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("[%d/%d] %s\n", i+1, len(msgs), m.MessageID)
		if len(h) == 0 {
			if filterName != "" {
				fmt.Printf("  (no %s header)\n", filterName)
			} else {
				fmt.Println("  (no headers stored)")
			}
			continue
		}
		// Sort header names for stable output.
		names := make([]string, 0, len(h))
		for name := range h {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, v := range h[name] {
				fmt.Printf("%s: %s\n", name, v)
			}
		}
	}
	return nil
}

// filterHeaders keeps only entries whose canonical name equals filterName
// case-insensitively. Returns a fresh map; the input is not mutated.
func filterHeaders(h map[string][]string, filterName string) map[string][]string {
	out := make(map[string][]string)
	for name, values := range h {
		if strings.EqualFold(name, filterName) {
			out[name] = values
		}
	}
	return out
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
