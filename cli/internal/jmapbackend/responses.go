package jmapbackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type jmapMailbox struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ParentID      string `json:"parentId"`
	Role          string `json:"role"`
	IsSubscribed  *bool  `json:"isSubscribed"`
	parentPresent bool
	rolePresent   bool
}

func (m *jmapMailbox) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		ParentID     json.RawMessage `json:"parentId"`
		Role         json.RawMessage `json:"role"`
		IsSubscribed *bool           `json:"isSubscribed"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.ID = wire.ID
	m.Name = wire.Name
	m.IsSubscribed = wire.IsSubscribed
	m.parentPresent = wire.ParentID != nil
	m.rolePresent = wire.Role != nil
	if len(wire.ParentID) > 0 && string(wire.ParentID) != "null" {
		if err := json.Unmarshal(wire.ParentID, &m.ParentID); err != nil {
			return err
		}
	}
	if len(wire.Role) > 0 && string(wire.Role) != "null" {
		if err := json.Unmarshal(wire.Role, &m.Role); err != nil {
			return err
		}
	}
	return nil
}

type jmapEmail struct {
	ID         string          `json:"id"`
	BlobID     string          `json:"blobId"`
	ThreadID   string          `json:"threadId"`
	MailboxIDs map[string]bool `json:"mailboxIds"`
	Keywords   map[string]bool `json:"keywords"`
	ReceivedAt string          `json:"receivedAt"`
	MessageID  []string        `json:"messageId"`
}

type jmapQueryPage struct {
	AccountID           *string   `json:"accountId"`
	QueryState          *string   `json:"queryState"`
	CanCalculateChanges *bool     `json:"canCalculateChanges"`
	Position            *int      `json:"position"`
	IDs                 *[]string `json:"ids"`
	Total               *int      `json:"total"`
}

func (p jmapQueryPage) validate(accountID string) error {
	if p.AccountID == nil || *p.AccountID != accountID {
		return errors.New("JMAP Email/query omitted required matching accountId")
	}
	if p.QueryState == nil {
		return errors.New("JMAP Email/query omitted required queryState")
	}
	if p.CanCalculateChanges == nil {
		return errors.New("JMAP Email/query omitted required canCalculateChanges")
	}
	if p.Position == nil || *p.Position < 0 {
		return errors.New("JMAP Email/query omitted required valid position")
	}
	if p.IDs == nil {
		return errors.New("JMAP Email/query omitted required ids")
	}
	seen := make(map[string]struct{}, len(*p.IDs))
	for _, id := range *p.IDs {
		if !validJMAPID(id) {
			return errors.New("JMAP Email/query returned an invalid id")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("JMAP Email/query returned duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	if p.Total == nil || *p.Total < 0 {
		return errors.New("JMAP Email/query omitted required valid total")
	}
	return nil
}

type jmapChangesPage struct {
	AccountID      *string   `json:"accountId"`
	OldState       *string   `json:"oldState"`
	NewState       *string   `json:"newState"`
	HasMoreChanges *bool     `json:"hasMoreChanges"`
	Created        *[]string `json:"created"`
	Updated        *[]string `json:"updated"`
	Destroyed      *[]string `json:"destroyed"`
}

func (p jmapChangesPage) validate(accountID, oldState string) error {
	if p.AccountID == nil || *p.AccountID != accountID {
		return errors.New("JMAP Email/changes omitted required matching accountId")
	}
	if p.OldState == nil || *p.OldState != oldState {
		return errors.New("JMAP Email/changes omitted required matching oldState")
	}
	if p.NewState == nil {
		return errors.New("JMAP Email/changes omitted required newState")
	}
	if p.HasMoreChanges == nil {
		return errors.New("JMAP Email/changes omitted required hasMoreChanges")
	}
	if p.Created == nil || p.Updated == nil || p.Destroyed == nil {
		return errors.New("JMAP Email/changes omitted required change arrays")
	}

	for _, ids := range [][]string{*p.Created, *p.Updated, *p.Destroyed} {
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if !validJMAPID(id) {
				return errors.New("JMAP Email/changes returned an invalid id")
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("JMAP Email/changes returned duplicate id %q", id)
			}
			seen[id] = struct{}{}
		}
	}
	if *p.HasMoreChanges && *p.NewState == oldState {
		return errors.New("JMAP Email/changes hasMoreChanges page made no progress")
	}
	return nil
}

type jmapGetPage struct {
	AccountID *string      `json:"accountId"`
	State     *string      `json:"state"`
	List      *[]jmapEmail `json:"list"`
	NotFound  *[]string    `json:"notFound"`
}

func (p jmapGetPage) validate(accountID string, requested []string) error {
	if p.AccountID == nil || *p.AccountID != accountID {
		return errors.New("JMAP Email/get omitted required matching accountId")
	}
	if p.State == nil {
		return errors.New("JMAP Email/get omitted required state")
	}
	if p.List == nil || p.NotFound == nil {
		return errors.New("JMAP Email/get omitted required result arrays")
	}

	wanted := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		wanted[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	validateID := func(id string) error {
		if _, requested := wanted[id]; !requested {
			return fmt.Errorf("JMAP Email/get returned unexpected id %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("JMAP Email/get returned duplicate id %q", id)
		}
		seen[id] = struct{}{}
		return nil
	}
	for _, email := range *p.List {
		if err := validateID(email.ID); err != nil {
			return err
		}
		if !validJMAPID(email.BlobID) || !validJMAPID(email.ThreadID) || len(email.MailboxIDs) == 0 || email.Keywords == nil {
			return fmt.Errorf("JMAP Email/get returned incomplete object %q", email.ID)
		}
		for id, present := range email.MailboxIDs {
			if !present || !validJMAPID(id) {
				return fmt.Errorf("JMAP Email/get returned invalid mailbox membership for %q", email.ID)
			}
		}
		for keyword, present := range email.Keywords {
			if !present || !validJMAPKeyword(keyword) {
				return fmt.Errorf("JMAP Email/get returned invalid keyword for %q", email.ID)
			}
		}
		if _, err := time.Parse(time.RFC3339, email.ReceivedAt); err != nil || !strings.HasSuffix(email.ReceivedAt, "Z") {
			return fmt.Errorf("JMAP Email/get returned invalid receivedAt for %q", email.ID)
		}
	}
	for _, id := range *p.NotFound {
		if err := validateID(id); err != nil {
			return err
		}
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("JMAP Email/get returned %d of %d requested ids", len(seen), len(wanted))
	}
	return nil
}

type jmapMailboxGetPage struct {
	AccountID *string        `json:"accountId"`
	State     *string        `json:"state"`
	List      *[]jmapMailbox `json:"list"`
	NotFound  *[]string      `json:"notFound"`
}

func (p jmapMailboxGetPage) validate(accountID string) error {
	if p.AccountID == nil || *p.AccountID != accountID {
		return errors.New("JMAP Mailbox/get omitted required matching accountId")
	}
	if p.State == nil {
		return errors.New("JMAP Mailbox/get omitted required state")
	}
	if p.List == nil || p.NotFound == nil || len(*p.NotFound) != 0 {
		return errors.New("JMAP Mailbox/get omitted required list or empty notFound")
	}

	mailboxes := make(map[string]jmapMailbox, len(*p.List))
	roles := make(map[string]struct{})
	for _, mailbox := range *p.List {
		if !validJMAPID(mailbox.ID) || mailbox.Name == "" || mailbox.IsSubscribed == nil || !mailbox.parentPresent || !mailbox.rolePresent {
			return errors.New("JMAP Mailbox/get returned an incomplete mailbox")
		}
		if _, duplicate := mailboxes[mailbox.ID]; duplicate {
			return fmt.Errorf("JMAP Mailbox/get returned duplicate id %q", mailbox.ID)
		}
		if mailbox.ParentID != "" && !validJMAPID(mailbox.ParentID) {
			return fmt.Errorf("JMAP Mailbox/get returned invalid parentId for %q", mailbox.ID)
		}
		role := strings.ToLower(mailbox.Role)
		if mailbox.Role != role {
			return fmt.Errorf("JMAP Mailbox/get returned invalid role %q", mailbox.Role)
		}
		if role != "" {
			if _, duplicate := roles[role]; duplicate {
				return fmt.Errorf("JMAP Mailbox/get returned duplicate role %q", role)
			}
			roles[role] = struct{}{}
		}
		mailboxes[mailbox.ID] = mailbox
	}
	for id, mailbox := range mailboxes {
		if mailbox.ParentID != "" {
			if _, ok := mailboxes[mailbox.ParentID]; !ok {
				return fmt.Errorf("JMAP Mailbox/get returned unknown parentId for %q", id)
			}
		}
		seen := map[string]struct{}{id: {}}
		for parentID := mailbox.ParentID; parentID != ""; parentID = mailboxes[parentID].ParentID {
			if _, cycle := seen[parentID]; cycle {
				return fmt.Errorf("JMAP Mailbox/get returned a parent cycle involving %q", id)
			}
			seen[parentID] = struct{}{}
		}
	}
	return nil
}
