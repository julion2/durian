package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/draft"
	"github.com/julion2/durian/cli/internal/smtp"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/spf13/cobra"
)

var (
	draftTo         string
	draftCC         string
	draftBCC        string
	draftSubject    string
	draftBody       string
	draftFrom       string
	draftAttach     []string
	draftHTML       bool
	draftReplace    string // Message-ID of draft to replace
	draftAccount    string // Account to use
	draftInReplyTo  string
	draftReferences string
)

var draftCmd = &cobra.Command{
	Use:   "draft",
	Short: "Manage email drafts",
	Long:  `Manage email drafts in the IMAP Drafts folder.`,
}

var draftSaveCmd = &cobra.Command{
	Use:   "save",
	Short: "Save a draft to IMAP Drafts folder",
	Long:  "Save a draft to IMAP. Use --replace to overwrite by Message-ID.",
	Example: `  durian draft save --account "user@example.com" --to "recipient@example.com" --subject "Hello" --body "Message"
  durian draft save --account "user@example.com" --to "..." --subject "..." --body "..." --replace "<old-message-id@host>"
  durian draft save --account "..." --to "..." --subject "..." --body "..." --attach file.pdf`,
	RunE: runDraftSave,
}

var draftDeleteCmd = &cobra.Command{
	Use:     "delete <message-id>",
	Short:   "Delete a draft from IMAP by Message-ID",
	Example: `  durian draft delete --account "user@example.com" "<message-id@host>"`,
	Args:    cobra.ExactArgs(1),
	RunE:    runDraftDelete,
}

func init() {
	// Save command flags
	draftSaveCmd.Flags().StringVar(&draftAccount, "account", "", "account email (required)")
	draftSaveCmd.Flags().StringVar(&draftFrom, "from", "", "from address (defaults to account email)")
	draftSaveCmd.Flags().StringVar(&draftTo, "to", "", "recipient email address(es), comma-separated")
	draftSaveCmd.Flags().StringVar(&draftCC, "cc", "", "CC recipient(s), comma-separated")
	draftSaveCmd.Flags().StringVar(&draftBCC, "bcc", "", "BCC recipient(s), comma-separated")
	draftSaveCmd.Flags().StringVar(&draftSubject, "subject", "", "email subject")
	draftSaveCmd.Flags().StringVar(&draftBody, "body", "", "email body")
	draftSaveCmd.Flags().StringSliceVar(&draftAttach, "attach", nil, "attach file(s)")
	draftSaveCmd.Flags().BoolVar(&draftHTML, "html", false, "body is HTML")
	draftSaveCmd.Flags().StringVar(&draftReplace, "replace", "", "Message-ID of draft to replace")
	draftSaveCmd.Flags().StringVar(&draftInReplyTo, "in-reply-to", "", "Message-ID of the message being replied to")
	draftSaveCmd.Flags().StringVar(&draftReferences, "references", "", "space-separated Message-IDs of the thread")
	draftSaveCmd.MarkFlagRequired("account")
	_ = draftSaveCmd.RegisterFlagCompletionFunc("account", completeAccounts)
	_ = draftSaveCmd.RegisterFlagCompletionFunc("from", completeAccounts)

	// Delete command flags
	draftDeleteCmd.Flags().StringVar(&draftAccount, "account", "", "account email (required)")
	draftDeleteCmd.MarkFlagRequired("account")
	_ = draftDeleteCmd.RegisterFlagCompletionFunc("account", completeAccounts)

	draftCmd.AddCommand(draftSaveCmd)
	draftCmd.AddCommand(draftDeleteCmd)
	rootCmd.AddCommand(draftCmd)
}

// draftResponse is the JSON output format
type draftResponse struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	UID       uint32 `json:"uid,omitempty"`
}

func runDraftSave(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return outputDraftError("no configuration loaded")
	}

	// Get account
	account, err := cfg.GetAccountByIdentifier(draftAccount)
	if err != nil {
		return outputDraftError(fmt.Sprintf("account not found: %s", draftAccount))
	}

	// Check IMAP config
	if account.IMAP.Host == "" {
		return outputDraftError(fmt.Sprintf("no IMAP host configured for %s", account.Email))
	}

	// Build from address — include display name if configured
	from := draftFrom
	if from == "" {
		if account.DisplayName != "" {
			from = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
		} else {
			from = account.Email
		}
	}

	// Parse recipients
	var toRecipients, ccRecipients, bccRecipients []string

	if draftTo != "" {
		toRecipients, err = smtp.ParseAddressList(draftTo)
		if err != nil {
			return outputDraftError(fmt.Sprintf("invalid To address: %v", err))
		}
	}

	if draftCC != "" {
		ccRecipients, err = smtp.ParseAddressList(draftCC)
		if err != nil {
			return outputDraftError(fmt.Sprintf("invalid CC address: %v", err))
		}
	}

	if draftBCC != "" {
		bccRecipients, err = smtp.ParseAddressList(draftBCC)
		if err != nil {
			return outputDraftError(fmt.Sprintf("invalid BCC address: %v", err))
		}
	}

	// Build message
	msg := &smtp.Message{
		From:       from,
		To:         toRecipients,
		CC:         ccRecipients,
		BCC:        bccRecipients,
		Subject:    draftSubject,
		Body:       draftBody,
		IsHTML:     draftHTML,
		InReplyTo:  draftInReplyTo,
		References: draftReferences,
	}

	// Load attachments
	var totalAttachmentSize int64
	for _, attachPath := range draftAttach {
		att, err := smtp.LoadAttachment(attachPath)
		if err != nil {
			return outputDraftError(fmt.Sprintf("failed to load attachment %s: %v", attachPath, err))
		}
		msg.Attachments = append(msg.Attachments, *att)
		totalAttachmentSize += int64(len(att.Data))
	}

	// Check attachment size limit
	if totalAttachmentSize > 0 {
		maxSize := account.GetMaxAttachmentSize()
		if totalAttachmentSize > maxSize {
			return outputDraftError(fmt.Sprintf("total attachment size (%s) exceeds limit (%s)",
				config.FormatSize(totalAttachmentSize), config.FormatSize(maxSize)))
		}
	}

	// Save draft
	service := draft.NewService(account)
	result, err := service.Save(msg, draftReplace)
	if err != nil {
		return outputDraftError(fmt.Sprintf("failed to save draft: %v", err))
	}

	// Save to local SQLite store so Drafts folder shows it immediately
	saveDraftToLocalStore(account, result.MessageID, msg)

	// Output success
	resp := draftResponse{
		OK:        true,
		MessageID: result.MessageID,
		UID:       result.UID,
	}
	return outputJSON(resp)
}

func runDraftDelete(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return outputDraftError("no configuration loaded")
	}

	messageID := args[0]

	// Get account
	account, err := cfg.GetAccountByIdentifier(draftAccount)
	if err != nil {
		return outputDraftError(fmt.Sprintf("account not found: %s", draftAccount))
	}

	// Check IMAP config
	if account.IMAP.Host == "" {
		return outputDraftError(fmt.Sprintf("no IMAP host configured for %s", account.Email))
	}

	// Delete draft from IMAP
	service := draft.NewService(account)
	if err := service.Delete(messageID); err != nil {
		return outputDraftError(fmt.Sprintf("failed to delete draft: %v", err))
	}

	// Also drop the local DB row so the draft tag disappears immediately.
	// `durian draft save` inserts the draft into the local store with tag:draft
	// so the GUI can show it without waiting for IMAP sync. Without this delete,
	// the row (and its tag) lives on after the IMAP draft is gone, and every
	// sent reply ends up with thread tags = sent,draft,... forever.
	deleteDraftFromLocalStore(account, messageID)

	// Output success
	resp := draftResponse{OK: true}
	return outputJSON(resp)
}

// deleteDraftFromLocalStore removes the local draft row mirroring the IMAP
// delete. Best-effort: errors are logged but do not affect the API result.
func deleteDraftFromLocalStore(account *config.AccountConfig, messageID string) {
	messageID = strings.Trim(messageID, "<>")
	if messageID == "" {
		return
	}

	db, err := store.Open(store.DefaultDBPath(), bootstrapKeyring())
	if err != nil {
		slog.Debug("Could not open store for draft delete", "module", "DRAFT", "err", err) // encgrep:allow word "draft" in message text, no draft value logged
		return
	}
	defer db.Close()

	if err := db.DeleteByMessageIDAndAccount(messageID, account.AccountIdentifier()); err != nil {
		slog.Debug("Local draft row not deleted", "module", "DRAFT", "message_id", messageID, "err", err) // encgrep:allow word "draft" in message text, no draft value logged
	}
}

// saveDraftToLocalStore inserts the draft into SQLite so the Drafts folder
// shows it immediately without waiting for the next IMAP sync.
func saveDraftToLocalStore(account *config.AccountConfig, messageID string, msg *smtp.Message) {
	messageID = strings.Trim(messageID, "<>")
	if messageID == "" {
		return
	}

	db, err := store.Open(store.DefaultDBPath(), bootstrapKeyring())
	if err != nil {
		slog.Debug("Could not open store for draft insert", "module", "DRAFT", "err", err) // encgrep:allow word "draft" in message text, no draft value logged
		return
	}
	defer db.Close()

	now := time.Now().Unix()
	fromAddr := account.Email
	if account.DisplayName != "" {
		fromAddr = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}

	storeMsg := &store.Message{
		MessageID:   messageID,
		Subject:     msg.Subject,
		FromAddr:    fromAddr,
		ToAddrs:     strings.Join(msg.To, ", "),
		CCAddrs:     strings.Join(msg.CC, ", "),
		InReplyTo:   msg.InReplyTo,
		Refs:        msg.References,
		Date:        now,
		CreatedAt:   now,
		Flags:       "Draft,Seen",
		FetchedBody: true,
		Account:     account.AccountIdentifier(),
	}
	if msg.IsHTML {
		storeMsg.BodyHTML = msg.Body
	} else {
		storeMsg.BodyText = msg.Body
	}

	if err := db.InsertMessage(storeMsg); err != nil {
		slog.Debug("Failed to save draft to local store", "module", "DRAFT", "err", err) // encgrep:allow word "draft" in message text, no draft value logged
		return
	}
	_ = db.AddTag(storeMsg.ID, "draft")
}

func outputDraftError(message string) error {
	resp := draftResponse{
		OK:    false,
		Error: message,
	}
	outputJSON(resp)
	return errors.New(message)
}

func outputJSON(v interface{}) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}
