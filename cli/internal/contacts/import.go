package contacts

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/julion2/durian/cli/internal/store"
)

var multiSpaceRe = regexp.MustCompile(`\s{2,}`)

// ImportFromStore imports contacts by scanning From, To, and CC addresses
// in the email store.
func ImportFromStore(emailDB *store.DB) ([]Contact, error) {
	// Get sender addresses with counts (SQL GROUP BY)
	senderCounts, err := emailDB.GetSenderCounts()
	if err != nil {
		return nil, fmt.Errorf("query senders: %w", err)
	}

	now := time.Now()
	seen := make(map[string]bool)

	var senderContacts []Contact
	for addr, count := range senderCounts {
		email, name := parseAddress(addr)
		if email == "" || !isValidEmail(email) {
			continue
		}
		email = strings.ToLower(email)
		if seen[email] {
			continue
		}
		seen[email] = true
		senderContacts = append(senderContacts, Contact{
			ID:         uuid.New().String(),
			Email:      email,
			Name:       name,
			UsageCount: count,
			Source:     SourceImported,
			CreatedAt:  now,
		})
	}

	// Get recipient addresses (To, CC fields — comma-separated)
	recipientStrings, err := emailDB.GetRecipientAddresses()
	if err != nil {
		return nil, fmt.Errorf("query recipients: %w", err)
	}

	recipientCounts := make(map[string]int)
	recipientNames := make(map[string]string)
	for _, addrList := range recipientStrings {
		for _, addr := range strings.Split(addrList, ",") {
			email, name := parseAddress(addr)
			if email == "" || !isValidEmail(email) {
				continue
			}
			email = strings.ToLower(email)
			recipientCounts[email]++
			if name != "" && recipientNames[email] == "" {
				recipientNames[email] = name
			}
		}
	}

	var recipientContacts []Contact
	for email, count := range recipientCounts {
		recipientContacts = append(recipientContacts, Contact{
			ID:         uuid.New().String(),
			Email:      email,
			Name:       recipientNames[email],
			UsageCount: count,
			Source:     SourceImported,
			CreatedAt:  now,
		})
	}

	return mergeContacts(senderContacts, recipientContacts), nil
}

// parseAddress extracts email and name from an address string
// Handles formats:
// - "Name <email@example.com>"
// - "<email@example.com>"
// - "email@example.com"
// - "email@example.com (Name)"
func parseAddress(addr string) (email, name string) {
	addr = strings.TrimSpace(addr)

	// Format: "Name <email>"
	if idx := strings.LastIndex(addr, "<"); idx != -1 {
		if end := strings.LastIndex(addr, ">"); end > idx {
			email = strings.TrimSpace(addr[idx+1 : end])
			name = strings.TrimSpace(addr[:idx])
			// Remove surrounding quotes from name
			name = strings.Trim(name, `"'`)
			// Discard name if it looks like an email address (garbage before <>)
			if strings.Contains(name, "@") {
				name = ""
			}
			name = multiSpaceRe.ReplaceAllString(name, " ")
			return
		}
	}

	// Format: "email (Name)"
	if idx := strings.LastIndex(addr, "("); idx != -1 {
		if end := strings.LastIndex(addr, ")"); end > idx {
			name = strings.TrimSpace(addr[idx+1 : end])
			email = strings.TrimSpace(addr[:idx])
			return
		}
	}

	// Plain email
	email = addr
	return
}

// isValidEmail does basic email validation
func isValidEmail(email string) bool {
	pattern := `^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`
	matched, _ := regexp.MatchString(pattern, email)
	return matched
}

// mergeContacts merges two contact lists, preferring entries with names
// and combining usage counts
func mergeContacts(existing, new []Contact) []Contact {
	byEmail := make(map[string]*Contact)

	// Index existing contacts
	for i := range existing {
		byEmail[existing[i].Email] = &existing[i]
	}

	// Merge new contacts
	for _, c := range new {
		if existing, ok := byEmail[c.Email]; ok {
			// Update name if we didn't have one
			if existing.Name == "" && c.Name != "" {
				existing.Name = c.Name
			}
			// Add usage counts together (sender + recipient counts)
			existing.UsageCount += c.UsageCount
		} else {
			byEmail[c.Email] = &Contact{
				ID:         c.ID,
				Email:      c.Email,
				Name:       c.Name,
				UsageCount: c.UsageCount,
				Source:     c.Source,
				CreatedAt:  c.CreatedAt,
			}
		}
	}

	// Convert back to slice
	result := make([]Contact, 0, len(byEmail))
	for _, c := range byEmail {
		result = append(result, *c)
	}

	return result
}
