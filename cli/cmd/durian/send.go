package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/auth"
	"github.com/julion2/durian/cli/internal/config"
	imapClient "github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/smtp"
	"github.com/spf13/cobra"
)

var (
	sendTo         string
	sendCC         string
	sendBCC        string
	sendSubject    string
	sendBody       string
	sendFrom       string
	sendAttach     []string
	sendBodyFile   string
	sendHTML       bool
	sendForce      bool
	sendInReplyTo  string
	sendReferences string
)

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send an email via SMTP",
	Long:  "Send an email via SMTP. Omit --body to compose in $EDITOR.",
	Example: `  durian send --to "recipient@example.com" --subject "Hello" --body "Message"
  durian send --to "main@example.com" --cc "copy@example.com" --bcc "hidden@example.com" --subject "Hello" --body "Message"
  durian send --to "..." --subject "..." --body "..." --attach file.pdf --attach file2.jpg
  durian send --to "..." --subject "..." --body-file message.txt
  durian send --to "..." --subject "Newsletter" --body-file newsletter.html --html
  durian send --to "recipient@example.com" --subject "Hello"
  durian send --from gmail --to "..." --subject "..." --body "..."`,
	RunE: runSend,
}

func init() {
	sendCmd.Flags().StringVar(&sendTo, "to", "", "recipient email address(es), comma-separated")
	sendCmd.Flags().StringVar(&sendCC, "cc", "", "CC recipient(s), comma-separated")
	sendCmd.Flags().StringVar(&sendBCC, "bcc", "", "BCC recipient(s), comma-separated")
	sendCmd.Flags().StringVar(&sendSubject, "subject", "", "email subject")
	sendCmd.Flags().StringVar(&sendBody, "body", "", "email body")
	sendCmd.Flags().StringVar(&sendFrom, "from", "", "sender account (alias, name, or email; uses default if not specified)")
	sendCmd.Flags().StringSliceVar(&sendAttach, "attach", nil, "attach file(s), can be specified multiple times")
	sendCmd.Flags().StringVar(&sendBodyFile, "body-file", "", "read body from file (cannot use with --body)")
	sendCmd.Flags().BoolVar(&sendHTML, "html", false, "send body as HTML")
	sendCmd.Flags().BoolVar(&sendForce, "force", false, "send even if attachments exceed size limit")
	sendCmd.Flags().StringVar(&sendInReplyTo, "in-reply-to", "", "Message-ID of the message being replied to")
	sendCmd.Flags().StringVar(&sendReferences, "references", "", "space-separated Message-IDs of the thread")
	_ = sendCmd.RegisterFlagCompletionFunc("from", completeAccounts)

	rootCmd.AddCommand(sendCmd)
}

func runSend(cmd *cobra.Command, args []string) error {
	cfg := GetConfig()
	if cfg == nil {
		return errors.New("no configuration loaded")
	}

	// Get sender account
	var account *config.AccountConfig
	var err error

	if sendFrom != "" {
		account, err = cfg.GetAccountByIdentifier(sendFrom)
		if err != nil {
			return fmt.Errorf("account not found: %s\nAvailable accounts: %s", sendFrom, cfg.ListAccountIdentifiers())
		}
	} else {
		account, err = cfg.GetDefaultAccount()
		if err != nil {
			return fmt.Errorf("no default account configured\nUse --from to specify an account or set default=true in config.pkl")
		}
	}

	// Check SMTP config
	if account.SMTP.Host == "" {
		return fmt.Errorf("no SMTP host configured for %s", account.Email)
	}

	// Get To address (prompt if not provided)
	to := sendTo
	if to == "" {
		to, err = prompt("To: ")
		if err != nil {
			return err
		}
	}
	if to == "" {
		return errors.New("at least one recipient required")
	}

	// Parse recipients
	recipients, err := smtp.ParseAddressList(to)
	if err != nil {
		return err
	}

	// Get Subject (prompt if not provided)
	subject := sendSubject
	if subject == "" {
		subject, err = prompt("Subject: ")
		if err != nil {
			return err
		}
	}

	// Validate body flags
	if sendBody != "" && sendBodyFile != "" {
		return errors.New("cannot use both --body and --body-file")
	}

	// Get Body
	var body string
	if sendBodyFile != "" {
		// Read body from file
		data, err := os.ReadFile(sendBodyFile)
		if err != nil {
			return fmt.Errorf("failed to read body file: %w", err)
		}
		body = string(data)
	} else if sendBody != "" {
		body = sendBody
	} else {
		// Open editor for interactive mode
		body, err = openEditor(to, subject)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("empty message body, aborting")
	}

	// Parse CC recipients
	var ccRecipients []string
	if sendCC != "" {
		ccRecipients, err = smtp.ParseAddressList(sendCC)
		if err != nil {
			return fmt.Errorf("invalid CC address: %w", err)
		}
	}

	// Parse BCC recipients
	var bccRecipients []string
	if sendBCC != "" {
		bccRecipients, err = smtp.ParseAddressList(sendBCC)
		if err != nil {
			return fmt.Errorf("invalid BCC address: %w", err)
		}
	}

	// Build message — include display name in From if configured
	from := account.Email
	if account.DisplayName != "" {
		from = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}

	msg := &smtp.Message{
		From:       from,
		To:         recipients,
		CC:         ccRecipients,
		BCC:        bccRecipients,
		Subject:    subject,
		Body:       body,
		IsHTML:     sendHTML,
		InReplyTo:  sendInReplyTo,
		References: sendReferences,
	}

	// Load attachments if specified
	var totalAttachmentSize int64
	for _, attachPath := range sendAttach {
		att, err := smtp.LoadAttachment(attachPath)
		if err != nil {
			return err
		}
		msg.Attachments = append(msg.Attachments, *att)
		totalAttachmentSize += int64(len(att.Data))
		fmt.Fprintf(os.Stderr, "Attaching: %s (%s, %s)\n", att.Filename, att.MIMEType, config.FormatSize(int64(len(att.Data))))
	}

	// Check attachment size limit
	if totalAttachmentSize > 0 {
		maxSize := account.GetMaxAttachmentSize()
		if totalAttachmentSize > maxSize {
			if sendForce {
				fmt.Fprintf(os.Stderr, "Warning: total attachment size (%s) exceeds limit (%s)\n",
					config.FormatSize(totalAttachmentSize), config.FormatSize(maxSize))
			} else {
				return fmt.Errorf("total attachment size (%s) exceeds limit (%s)\nUse --force to send anyway",
					config.FormatSize(totalAttachmentSize), config.FormatSize(maxSize))
			}
		}
	}

	// Get authentication
	smtpAuth, err := auth.GetSMTPAuth(account)
	if err != nil {
		return err
	}

	// Send
	fmt.Fprintf(os.Stderr, "Connecting to %s:%d...\n", account.SMTP.Host, account.SMTP.Port)

	client := smtp.NewClient(account.SMTP.Host, account.SMTP.Port, smtpAuth)
	if err := client.Send(msg); err != nil {
		return fmt.Errorf("failed to send email: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✓ Email sent successfully to %s\n", to)

	// Save a copy to the IMAP Sent folder
	// Skip for providers that auto-save sent mail (Google, Microsoft)
	if account.OAuth != nil && (account.OAuth.Provider == "google" || account.OAuth.Provider == "microsoft") {
		fmt.Fprintf(os.Stderr, "✓ %s saves sent mail automatically\n", account.OAuth.Provider)
		return nil
	}

	messageData, err := msg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to build message for Sent folder: %v\n", err)
		return nil
	}

	imapConn := imapClient.NewClient(account)
	if err := imapConn.Connect(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to connect to IMAP for Sent folder: %v\n", err)
		return nil
	}
	defer imapConn.Close()

	if err := imapConn.Authenticate(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to authenticate IMAP for Sent folder: %v\n", err)
		return nil
	}

	sentMailbox, err := imapConn.FindSentMailbox()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not find Sent mailbox: %v\n", err)
		return nil
	}

	flags := []string{imap.SeenFlag}
	if _, err := imapConn.Append(sentMailbox, flags, time.Now(), messageData); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save to Sent folder: %v\n", err)
		return nil
	}

	fmt.Fprintf(os.Stderr, "✓ Saved to %s\n", sentMailbox)
	return nil
}

// prompt displays a prompt and reads a line of input
func prompt(message string) (string, error) {
	fmt.Fprint(os.Stderr, message)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// openEditor opens $EDITOR for the user to write the email body.
//
// ADR-0001 audit #254.3: the editor file holds the plaintext body, so it
// must not land in /tmp where any local user can read it and where editor
// artefacts (vim .swp, crashed sessions) linger across reboots. The file
// is placed under $XDG_CACHE_HOME/durian (or ~/.cache/durian) with the
// containing directory restricted to 0700 — matching the imap state-manager
// convention in the same cache root.
func openEditor(to, subject string) (string, error) {
	cacheRoot := os.Getenv("XDG_CACHE_HOME")
	if cacheRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for editor tempfile: %w", err)
		}
		cacheRoot = filepath.Join(home, ".cache")
	}
	cacheDir := filepath.Join(cacheRoot, "durian")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("create editor cache dir: %w", err)
	}
	tmpfile, err := os.CreateTemp(cacheDir, "compose-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpfile.Name()
	defer os.Remove(tmpPath)

	// Write template
	template := fmt.Sprintf(`
# Write your message above this line.
# Lines starting with # will be ignored.
# To: %s
# Subject: %s
# 
# Save and close the editor to send, or delete all text to cancel.
`, to, subject)

	if _, err := tmpfile.WriteString(template); err != nil {
		tmpfile.Close()
		return "", fmt.Errorf("failed to write template: %w", err)
	}
	tmpfile.Close()

	// Determine editor
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		// Try common editors
		for _, e := range []string{"vim", "nano", "vi"} {
			if _, err := exec.LookPath(e); err == nil {
				editor = e
				break
			}
		}
	}
	if editor == "" {
		return "", errors.New("no editor found. Set $EDITOR environment variable")
	}

	// Open editor
	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor failed: %w", err)
	}

	// Read result
	file, err := os.Open(tmpPath)
	if err != nil {
		return "", fmt.Errorf("failed to read edited file: %w", err)
	}
	defer file.Close()

	body, err := smtp.ReadBody(file)
	if err != nil {
		return "", fmt.Errorf("failed to parse body: %w", err)
	}

	return body, nil
}
