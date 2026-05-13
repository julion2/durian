package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"github.com/spf13/cobra"

	"github.com/durian-dev/durian/cli/internal/imap"
	"github.com/durian-dev/durian/cli/internal/store"
)

var attachmentCmd = &cobra.Command{
	Use:   "attachment <message-id>",
	Short: "List or download attachments",
	Long: "List attachments for a message, or download a specific part with --save.",
	Example: `  durian attachment msg-id@example.com
  durian attachment msg-id@example.com --save 1
  durian attachment msg-id@example.com --save 1 --output ~/Downloads/`,
	Args: cobra.ExactArgs(1),
	RunE: runAttachment,
}

var (
	attachSavePart int
	attachOutput   string
)

func init() {
	attachmentCmd.Flags().IntVar(&attachSavePart, "save", 0, "download part ID (0 = list only)")
	attachmentCmd.Flags().StringVarP(&attachOutput, "output", "o", ".", "output directory for download")
	rootCmd.AddCommand(attachmentCmd)
}

func runAttachment(cmd *cobra.Command, args []string) error {
	messageID := args[0]

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	atts, err := emailDB.GetAttachmentsByMessageID(messageID)
	if err != nil {
		return fmt.Errorf("get attachments: %w", err)
	}
	if len(atts) == 0 {
		fmt.Fprintln(os.Stderr, "No attachments found")
		return nil
	}

	// List mode
	if attachSavePart == 0 {
		if jsonOutput {
			return json.NewEncoder(os.Stdout).Encode(atts)
		}
		for _, a := range atts {
			size := formatSize(a.Size)
			fmt.Fprintf(os.Stdout, "  [%d] %s (%s, %s)\n", a.PartID, a.Filename, a.ContentType, size)
		}
		return nil
	}

	// Download mode
	var att *store.Attachment
	for i := range atts {
		if atts[i].PartID == attachSavePart {
			att = &atts[i]
			break
		}
	}
	if att == nil {
		return fmt.Errorf("part %d not found", attachSavePart)
	}

	// Get message IMAP metadata
	msg, err := emailDB.GetByMessageID(messageID)
	if err != nil || msg == nil {
		return fmt.Errorf("message not found in store")
	}
	if msg.UID == 0 || msg.Account == "" || msg.Mailbox == "" {
		return fmt.Errorf("message missing IMAP metadata (try syncing first)")
	}

	// Find account config
	account, err := cfg.GetAccountByName(msg.Account)
	if err != nil {
		return fmt.Errorf("account %q not found in config", msg.Account)
	}

	// Connect to IMAP and fetch
	client := imap.NewClient(account)
	if err := client.Connect(); err != nil {
		return err
	}
	defer client.Close()
	if err := client.Authenticate(); err != nil {
		return err
	}
	if _, err := client.SelectMailbox(msg.Mailbox); err != nil {
		return err
	}

	safeFilename := filepath.Base(att.Filename)
	if safeFilename == "" || safeFilename == "." {
		safeFilename = "attachment"
	}
	outPath := filepath.Join(attachOutput, safeFilename)
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if err := client.FetchDecodedAttachment(msg.UID, att.Filename, att.PartID, f); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("download failed: %w", err)
	}

	fi, statErr := f.Stat()
	var size int64
	if statErr == nil {
		size = fi.Size()
	}
	fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", outPath, formatSize(int(size)))
	return nil
}

func formatSize(bytes int) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

