package calendartest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ExistingGoogleCalendar verifies an exact, pre-provisioned secondary calendar.
// Unlike EnsureGoogleCalendar it never creates a calendar or discovers by name.
func ExistingGoogleCalendar(ctx context.Context, source *TokenSource, id string) error {
	if source == nil || source.provider != Google || !secondaryCalendar(id) {
		return fmt.Errorf("invalid pinned Google calendar")
	}
	// calendarList entry includes the authenticated user's accessRole.
	raw := "https://www.googleapis.com/calendar/v3/users/me/calendarList/" + url.PathEscape(id)
	token, err := source.Token(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pinned Google metadata read failed")
	}
	defer resp.Body.Close()
	var c struct {
		ID, Summary, Description, AccessRole string
		Primary                              bool
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, graphLimit)).Decode(&c) != nil || c.ID != id || c.Summary != "Durian Test" || c.Description != calendarDescription || c.Primary || c.AccessRole != "owner" {
		return fmt.Errorf("pinned Google calendar failed ownership verification")
	}
	return nil
}

// ExistingGraphCalendar verifies exact metadata only, with no creation fallback.
func ExistingGraphCalendar(ctx context.Context, source *TokenSource, id string) (*GraphCalendarPin, error) {
	if source == nil || source.provider != Microsoft || source.email == "" || !graphID.MatchString(id) {
		return nil, fmt.Errorf("invalid pinned Graph calendar")
	}
	var c graphMetadata
	if err := graphJSON(ctx, source, http.DefaultTransport, GraphBase+"/me/calendars/"+url.PathEscape(id)+"?$select="+graphCalendarSelect, &c); err != nil || !c.isolated() || c.ID != id || !strings.EqualFold(c.Owner.Address, source.email) {
		return nil, fmt.Errorf("pinned Graph calendar failed ownership verification")
	}
	return &GraphCalendarPin{id: id, source: source, verified: true, owner: c.Owner.Address}, nil
}
