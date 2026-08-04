package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/imapbackend"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

var attachmentCmd = &cobra.Command{
	Use:   "attachment <message-id>",
	Short: "List or download attachments",
	Long:  "List attachments for a message, or download a specific part with --save.",
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

	msg, err := emailDB.GetByMessageID(messageID)
	if err != nil || msg == nil {
		return fmt.Errorf("message not found in store")
	}
	account, err := cfg.GetAccountByIdentifier(msg.Account)
	if err != nil {
		return fmt.Errorf("account %q not found in config", msg.Account)
	}

	safeFilename := filepath.Base(att.Filename)
	if safeFilename == "" || safeFilename == "." {
		safeFilename = "attachment"
	}
	outPath := filepath.Join(attachOutput, safeFilename)

	// Backend-neutral path for engine/Graph-synced messages, which carry a
	// provider handle (remote_ref) rather than an IMAP UID: fetch the raw body
	// through the account's backend and slice out the part.
	if msg.RemoteRef != "" {
		data, err := fetchAttachmentPartViaBackend(cmd.Context(), account, msg, att.PartID)
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", outPath, formatSize(len(data)))
		return nil
	}

	// Legacy IMAP path (fetch BODY[section] by UID).
	if msg.UID == 0 || msg.Mailbox == "" {
		return fmt.Errorf("message missing IMAP metadata (try syncing first)")
	}
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
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	if err := client.FetchDecodedAttachment(msg.UID, att.Filename, att.PartID, f); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("download failed: %w", err)
	}
	fi, _ := f.Stat()
	fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", outPath, formatSize(int(fi.Size())))
	return nil
}

// fetchAttachmentPartViaBackend fetches msg's raw body through the account's
// backend and returns the requested attachment part's decoded bytes.
func fetchAttachmentPartViaBackend(ctx context.Context, account *config.AccountConfig, msg *store.Message, partID int) ([]byte, error) {
	var b backend.Backend
	var err error
	if account.UsesGraphBackend() {
		b, err = graphbackend.New(account)
	} else {
		b, err = imapbackend.New(account)
	}
	if err != nil {
		return nil, err
	}
	defer b.Close()
	var buf bytes.Buffer
	ref := backend.RemoteRef{Folder: msg.Mailbox, ID: msg.RemoteRef}
	if err := b.FetchBody(ctx, ref, &buf); err != nil {
		return nil, fmt.Errorf("fetch body: %w", err)
	}
	data, _, err := mail.ExtractAttachmentPart(buf.Bytes(), partID)
	return data, err
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
