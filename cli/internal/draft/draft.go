// Package draft provides functionality for managing email drafts via IMAP
package draft

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/emersion/go-imap"

	"github.com/julion2/durian/cli/internal/config"
	imapClient "github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/smtp"
)

// Service handles draft operations
type Service struct {
	account *config.AccountConfig
}

// NewService creates a new draft service for the given account
func NewService(account *config.AccountConfig) *Service {
	return &Service{
		account: account,
	}
}

// SaveResult contains the result of a draft save operation
type SaveResult struct {
	MessageID string `json:"message_id"`
	UID       uint32 `json:"uid"`
}

// Save saves a draft to the IMAP Drafts folder
// If replaceMessageID is provided, the old draft will be deleted first
func (s *Service) Save(msg *smtp.Message, replaceMessageID string) (*SaveResult, error) {
	// Build the RFC822 message
	messageData, err := msg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build message: %w", err)
	}

	// Extract Message-ID from the built message
	messageID := extractMessageID(messageData)

	// Connect to IMAP
	client := imapClient.NewClient(s.account)
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to IMAP: %w", err)
	}
	defer client.Close()

	if err := client.Authenticate(); err != nil {
		return nil, fmt.Errorf("failed to authenticate: %w", err)
	}

	// Find the Drafts mailbox
	draftsMailbox, err := client.FindDraftsMailbox()
	if err != nil {
		return nil, fmt.Errorf("failed to find Drafts mailbox: %w", err)
	}

	// If replacing an existing draft, delete it first
	if replaceMessageID != "" {
		if err := s.deleteByMessageID(client, draftsMailbox, replaceMessageID); err != nil {
			// Log but don't fail - the old draft might not exist anymore
			slog.Warn("Failed to delete old draft",
				"module", "DRAFT", "message_id", replaceMessageID, "err", err)
		}
	}

	// Append the new draft with \Draft and \Seen flags
	flags := []string{imap.DraftFlag, imap.SeenFlag}
	uid, err := client.Append(draftsMailbox, flags, time.Now(), messageData)
	if err != nil {
		return nil, fmt.Errorf("failed to append draft: %w", err)
	}

	return &SaveResult{
		MessageID: messageID,
		UID:       uid,
	}, nil
}

// Delete removes a draft from the IMAP server by its Message-ID
func (s *Service) Delete(messageID string) error {
	// Connect to IMAP
	client := imapClient.NewClient(s.account)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to IMAP: %w", err)
	}
	defer client.Close()

	if err := client.Authenticate(); err != nil {
		return fmt.Errorf("failed to authenticate: %w", err)
	}

	// Find the Drafts mailbox
	draftsMailbox, err := client.FindDraftsMailbox()
	if err != nil {
		return fmt.Errorf("failed to find Drafts mailbox: %w", err)
	}

	return s.deleteByMessageID(client, draftsMailbox, messageID)
}

// deleteByMessageID deletes a message by its Message-ID from a specific mailbox
func (s *Service) deleteByMessageID(client *imapClient.Client, mailbox, messageID string) error {
	// Select the mailbox
	_, err := client.SelectMailbox(mailbox)
	if err != nil {
		return fmt.Errorf("failed to select mailbox %s: %w", mailbox, err)
	}

	// Search for the message by Message-ID
	uid, err := client.SearchByMessageID(messageID)
	if err != nil {
		return fmt.Errorf("failed to search for message: %w", err)
	}

	if uid == 0 {
		return fmt.Errorf("message not found: %s", messageID)
	}

	// Delete the message
	if err := client.Delete(uid); err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}

	return nil
}

// extractMessageID extracts the Message-ID from raw email data
func extractMessageID(data []byte) string {
	// Simple extraction - look for Message-ID header
	// Format: Message-ID: <xxx@yyy>
	lines := string(data)
	start := 0
	for {
		idx := indexOf(lines[start:], "Message-ID:")
		if idx == -1 {
			idx = indexOf(lines[start:], "Message-Id:")
		}
		if idx == -1 {
			break
		}
		start += idx

		// Find the end of the line
		lineEnd := indexOf(lines[start:], "\r\n")
		if lineEnd == -1 {
			lineEnd = indexOf(lines[start:], "\n")
		}
		if lineEnd == -1 {
			break
		}

		line := lines[start : start+lineEnd]
		// Extract the ID part
		colonIdx := indexOf(line, ":")
		if colonIdx != -1 {
			id := line[colonIdx+1:]
			// Trim whitespace and brackets
			id = trimSpace(id)
			id = trimBrackets(id)
			return id
		}
		start += lineEnd
	}
	return ""
}

// Helper functions to avoid importing strings package for simple operations
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		return s[1 : len(s)-1]
	}
	return s
}
