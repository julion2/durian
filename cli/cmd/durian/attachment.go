package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/backendfactory"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/mail"
	"github.com/julion2/durian/cli/internal/store"
)

var attachmentCmd = &cobra.Command{
	Use:   "attachment <message-id>",
	Short: "List or download attachments",
	Long:  "List attachments for a message, or download a specific part with --save.",
	Example: `  durian attachment msg-id@example.com
  durian attachment msg-id@example.com --account work --save 1
  durian attachment msg-id@example.com --save 1 --output ~/Downloads/`,
	Args: cobra.ExactArgs(1),
	RunE: runAttachment,
}

var (
	attachSavePart int
	attachOutput   string
	attachAccount  string
	attachForce    bool
)

func init() {
	attachmentCmd.Flags().IntVar(&attachSavePart, "save", 0, "download part ID (0 = list only)")
	attachmentCmd.Flags().StringVarP(&attachOutput, "output", "o", ".", "output directory for download")
	attachmentCmd.Flags().StringVarP(&attachAccount, "account", "a", "", "account containing the message")
	attachmentCmd.Flags().BoolVar(&attachForce, "force", false, "overwrite an existing file")
	_ = attachmentCmd.RegisterFlagCompletionFunc("account", completeAccounts)
	rootCmd.AddCommand(attachmentCmd)
}

func runAttachment(cmd *cobra.Command, args []string) error {
	messageID := normalizeMessageReference(args[0])

	emailDB, err := openEmailDB()
	if err != nil {
		return fmt.Errorf("email store unavailable: %w", err)
	}
	defer emailDB.Close()

	msg, err := resolveAttachmentMessage(emailDB, cfg, messageID, attachAccount)
	if err != nil {
		return err
	}
	atts, err := emailDB.GetAttachmentsByMessage(msg.ID)
	if err != nil {
		return fmt.Errorf("get attachments: %w", err)
	}
	// List mode
	if attachSavePart == 0 {
		if jsonOutput {
			return writeJSON(publicAttachments(atts))
		}
		if len(atts) == 0 {
			fmt.Fprintln(os.Stderr, "No attachments found")
			return nil
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

	account, err := cfg.GetAccountByIdentifier(msg.Account)
	if err != nil {
		return fmt.Errorf("account %q not found in config", msg.Account)
	}

	safeFilename := filepath.Base(att.Filename)
	if safeFilename == "" || safeFilename == "." {
		safeFilename = "attachment"
	}
	outPath := filepath.Join(attachOutput, safeFilename)
	if !attachForce {
		if _, err := os.Lstat(outPath); err == nil {
			return fmt.Errorf("output file already exists: %s (use --force to overwrite)", outPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("check output file: %w", err)
		}
	}

	// Backend-neutral path for engine/Graph-synced messages, which carry a
	// provider handle (remote_ref) rather than an IMAP UID: fetch the raw body
	// through the account's backend and slice out the part.
	if msg.RemoteRef != "" {
		data, err := fetchAttachmentPartViaBackend(cmd.Context(), account, msg, att.PartID)
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		f, err := createAttachmentFile(outPath, attachForce)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(outPath)
			return fmt.Errorf("write file: %w", err)
		}
		if err := f.Close(); err != nil {
			os.Remove(outPath)
			return fmt.Errorf("close file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", outPath, formatSize(len(data)))
		return writeAttachmentSaveJSON(outPath, att.PartID, len(data))
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
	f, err := createAttachmentFile(outPath, attachForce)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := client.FetchDecodedAttachment(msg.UID, att.Filename, att.PartID, f); err != nil {
		os.Remove(outPath)
		return fmt.Errorf("download failed: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat downloaded attachment: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Saved %s (%s)\n", outPath, formatSize(int(fi.Size())))
	return writeAttachmentSaveJSON(outPath, att.PartID, int(fi.Size()))
}

type attachmentJSON struct {
	PartID      int    `json:"part_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Disposition string `json:"disposition,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

func publicAttachments(atts []store.Attachment) []attachmentJSON {
	out := make([]attachmentJSON, 0, len(atts))
	for _, att := range atts {
		out = append(out, attachmentJSON{
			PartID: att.PartID, Filename: att.Filename, ContentType: att.ContentType,
			Size: att.Size, Disposition: att.Disposition, ContentID: att.ContentID,
		})
	}
	return out
}

func writeAttachmentSaveJSON(path string, partID, size int) error {
	if !jsonOutput {
		return nil
	}
	return writeJSON(struct {
		Saved  string `json:"saved"`
		PartID int    `json:"part_id"`
		Size   int    `json:"size"`
	}{Saved: path, PartID: partID, Size: size})
}

func normalizeMessageReference(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) >= len("message:") && strings.EqualFold(ref[:len("message:")], "message:") {
		ref = strings.TrimSpace(ref[len("message:"):])
	}
	ref = strings.TrimPrefix(ref, "<")
	return strings.TrimSuffix(ref, ">")
}

func resolveAttachmentMessage(emailDB *store.DB, cfg *config.Config, messageID, accountIdentifier string) (*store.Message, error) {
	messages, err := emailDB.GetAllByMessageID(messageID)
	if err != nil {
		return nil, fmt.Errorf("resolve message: %w", err)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("message %q not found in store", messageID)
	}

	if accountIdentifier != "" {
		account, err := cfg.GetAccountByIdentifier(accountIdentifier)
		if err != nil {
			return nil, fmt.Errorf("account %q not found in config", accountIdentifier)
		}
		for _, message := range messages {
			if strings.EqualFold(message.Account, account.AccountIdentifier()) {
				return message, nil
			}
		}
		return nil, fmt.Errorf("message %q was not found in account %q", messageID, account.GetAliasOrName())
	}

	if len(messages) > 1 {
		accounts := make([]string, 0, len(messages))
		for _, message := range messages {
			name := message.Account
			if account, err := cfg.GetAccountByIdentifier(message.Account); err == nil {
				name = account.GetAliasOrName()
			}
			accounts = append(accounts, name)
		}
		sort.Strings(accounts)
		return nil, fmt.Errorf("message %q exists in multiple accounts (%s); add --account", messageID, strings.Join(accounts, ", "))
	}
	return messages[0], nil
}

func createAttachmentFile(path string, force bool) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("output file already exists: %s (use --force to overwrite)", path)
		}
		return nil, fmt.Errorf("create file: %w", err)
	}
	return f, nil
}

// fetchAttachmentPartViaBackend fetches msg's raw body through the account's
// backend and returns the requested attachment part's decoded bytes.
func fetchAttachmentPartViaBackend(ctx context.Context, account *config.AccountConfig, msg *store.Message, partID int) ([]byte, error) {
	b, err := backendfactory.New(account)
	if err != nil {
		return nil, fmt.Errorf("create backend: %w", err)
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
