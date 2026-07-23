// Package graphcalendar implements a one-way export of Microsoft Graph
// (Outlook) calendars into a vdir layout — one directory per calendar, one
// .ics file per event — that vdirsyncer / khal can consume. It is strictly
// read-only (Calendars.Read); there is no two-way sync.
//
// Events are fetched via the Graph calendarView endpoint, which expands
// recurring series into concrete instances within the requested window, so no
// RRULE handling is needed (see ics.go).
package graphcalendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/oauth"
)

const (
	// defaultBaseURL is the Graph API root, without a trailing slash.
	defaultBaseURL = "https://graph.microsoft.com/v1.0"

	// eventSelect is the field set requested on calendarView queries: just what
	// the .ics writer needs.
	eventSelect = "id,iCalUId,subject,start,end,isAllDay,location,bodyPreview,lastModifiedDateTime"

	// preferUTC asks Graph to return event start/end dateTimes in UTC, so
	// parsing never has to interpret Windows timezone names.
	preferUTC = `outlook.timezone="UTC"`

	// tokenExpiryBuffer refreshes the cached Graph token this long before its
	// actual expiry, so a request never starts with an about-to-expire token.
	tokenExpiryBuffer = 5 * time.Minute
)

// Client is a minimal, read-only Microsoft Graph calendar client.
type Client struct {
	account      *config.AccountConfig
	clientID     string
	clientSecret string
	tenant       string

	httpClient *http.Client
	// baseURL is the Graph API root without trailing slash. Defaults to
	// defaultBaseURL; tests point it at an httptest.Server.
	baseURL string
	// tokenFn returns a valid Graph bearer token. Defaults to the cached
	// oauth.GetGraphToken path (cachedGraphToken); tests override it.
	tokenFn func(ctx context.Context) (string, error)

	// mu guards cachedToken.
	mu          sync.Mutex
	cachedToken *oauth.Token
}

// New creates a Graph calendar client for the given Microsoft OAuth account.
// The Graph token is fetched lazily on first use, so construction never
// touches the network.
func New(account *config.AccountConfig) (*Client, error) {
	if account.OAuth == nil || account.OAuth.Provider != "microsoft" {
		return nil, fmt.Errorf("calendar export requires a Microsoft OAuth account, got %s", account.Email)
	}

	c := &Client{
		account:      account,
		clientID:     account.OAuth.ClientID,
		clientSecret: account.OAuth.ClientSecret,
		tenant:       account.OAuth.Tenant,
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		baseURL:      defaultBaseURL,
	}
	c.tokenFn = c.cachedGraphToken
	return c, nil
}

// MARK: - Token source

// cachedGraphToken returns the cached Graph access token, minting a fresh one
// via oauth.GetGraphToken when none is cached or it expires within
// tokenExpiryBuffer. The Graph token lives only in memory; oauth.GetGraphToken
// persists any rotated refresh token itself.
func (c *Client) cachedGraphToken(_ context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cachedToken != nil && !c.cachedToken.IsExpiredWithBuffer(tokenExpiryBuffer) {
		return c.cachedToken.AccessToken, nil
	}

	token, err := oauth.GetGraphToken(c.account.GetAuthEmail(), c.clientID, c.clientSecret, c.tenant)
	if err != nil {
		return "", fmt.Errorf("failed to get Graph token for %s: %w", c.account.Email, err)
	}
	c.cachedToken = token
	slog.Debug("Minted Graph access token", "module", "GRAPHCAL",
		"account", c.account.Email, "expiry", token.Expiry)
	return token.AccessToken, nil
}

// MARK: - HTTP client

// statusError is a non-2xx Graph response, carrying the HTTP status so callers
// can react to specific codes.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("graph request failed: status %d: %s", e.status, e.body)
}

// IsAuthError reports whether err looks like a Graph authentication or consent
// problem (expired/missing token, or a 401/403 from Graph), so the command can
// hint at re-running `durian auth login`.
func IsAuthError(err error) bool {
	if errors.Is(err, oauth.ErrTokenExpired) ||
		errors.Is(err, oauth.ErrTokenNotFound) ||
		errors.Is(err, oauth.ErrRefreshFailed) {
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusUnauthorized || se.status == http.StatusForbidden
	}
	return false
}

// doJSON executes one authenticated Graph GET with throttle handling — up to 3
// retries on 429 honoring Retry-After, and one retry with a short backoff on
// 503/504 — then decodes the JSON response into out. All waits respect ctx
// cancellation. Non-2xx responses become a statusError with a body snippet.
func (c *Client) doJSON(ctx context.Context, reqURL string, extraHeaders map[string]string, out any) error {
	const (
		maxThrottleRetries = 3
		transientBackoff   = 2 * time.Second
	)

	throttleRetries := 0
	transientRetried := false
	for {
		token, err := c.tokenFn(ctx)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return fmt.Errorf("failed to build graph request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("graph request failed: %w", err)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && throttleRetries < maxThrottleRetries:
			throttleRetries++
			delay := retryAfter(resp)
			drainClose(resp)
			slog.Warn("Graph throttled request, backing off", "module", "GRAPHCAL",
				"retry", throttleRetries, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return err
			}
		case (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) && !transientRetried:
			transientRetried = true
			drainClose(resp)
			slog.Warn("Graph transient error, retrying once", "module", "GRAPHCAL",
				"status", resp.StatusCode)
			if err := sleepCtx(ctx, transientBackoff); err != nil {
				return err
			}
		default:
			defer drainClose(resp)
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return newStatusError(resp)
			}
			if out == nil {
				return nil
			}
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return fmt.Errorf("failed to decode graph response: %w", err)
			}
			return nil
		}
	}
}

// retryAfter parses the Retry-After header (delay-seconds form) of a 429
// response, defaulting to one second when absent or unparseable.
func retryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Second
}

// newStatusError builds a statusError including a snippet of the error body.
func newStatusError(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &statusError{status: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
}

// drainClose drains (bounded) and closes a response body so the underlying
// connection can be reused.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MARK: - Calendars

// Calendar is one Outlook calendar.
type Calendar struct {
	ID   string
	Name string
	// HexColor is the calendar's "#RRGGBB" color, or "" when the calendar has
	// no explicit color (Graph reports "" or "auto").
	HexColor string
}

// calendarPage is one page of GET /me/calendars.
type calendarPage struct {
	Value []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		HexColor string `json:"hexColor"`
	} `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// ListCalendars returns all calendars of the account, following pagination.
func (c *Client) ListCalendars(ctx context.Context) ([]Calendar, error) {
	var calendars []Calendar
	pageURL := c.baseURL + "/me/calendars?$select=id,name,hexColor"
	for pageURL != "" {
		var page calendarPage
		if err := c.doJSON(ctx, pageURL, nil, &page); err != nil {
			return nil, fmt.Errorf("failed to list calendars: %w", err)
		}
		for _, cal := range page.Value {
			hexColor := cal.HexColor
			if strings.EqualFold(hexColor, "auto") {
				hexColor = ""
			}
			calendars = append(calendars, Calendar{ID: cal.ID, Name: cal.Name, HexColor: hexColor})
		}
		pageURL = page.NextLink
	}

	slog.Debug("Listed calendars", "module", "GRAPHCAL", "count", len(calendars))
	return calendars, nil
}

// MARK: - Events

// Event is one concrete event instance from a Graph calendarView. Recurring
// series arrive pre-expanded, so ID is unique per occurrence while ICalUID is
// shared across a series.
type Event struct {
	ID           string
	ICalUID      string
	Subject      string
	Location     string
	Description  string
	Start        time.Time
	End          time.Time
	AllDay       bool
	LastModified time.Time
}

// graphDateTime is Graph's {dateTime, timeZone} pair. With the preferUTC
// header, dateTime is already UTC.
type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// graphEvent is the subset of a Graph event resource we consume.
type graphEvent struct {
	ID       string        `json:"id"`
	ICalUID  string        `json:"iCalUId"`
	Subject  string        `json:"subject"`
	Start    graphDateTime `json:"start"`
	End      graphDateTime `json:"end"`
	IsAllDay bool          `json:"isAllDay"`
	Location struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	BodyPreview          string `json:"bodyPreview"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
}

// eventPage is one page of a calendarView query.
type eventPage struct {
	Value    []graphEvent `json:"value"`
	NextLink string       `json:"@odata.nextLink"`
}

// FetchEvents returns all event instances of the calendar within [from, to)
// via the Graph calendarView endpoint, which expands recurring series into
// concrete occurrences. The Prefer: outlook.timezone="UTC" header is sent on
// every page (including @odata.nextLink follow-ups) so start/end come back in
// UTC. For all-day events Graph reports midnight boundaries with an exclusive
// end date; both are kept as-is.
func (c *Client) FetchEvents(ctx context.Context, calendarID string, from, to time.Time) ([]Event, error) {
	headers := map[string]string{"Prefer": preferUTC}

	var events []Event
	pageURL := fmt.Sprintf("%s/me/calendars/%s/calendarView?startDateTime=%s&endDateTime=%s&$select=%s&$top=100",
		c.baseURL, url.PathEscape(calendarID),
		url.QueryEscape(from.UTC().Format(time.RFC3339)),
		url.QueryEscape(to.UTC().Format(time.RFC3339)),
		eventSelect)
	for pageURL != "" {
		var page eventPage
		if err := c.doJSON(ctx, pageURL, headers, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch calendar view for %s: %w", calendarID, err)
		}
		for _, ge := range page.Value {
			start, err := parseGraphDateTime(ge.Start.DateTime)
			if err != nil {
				slog.Warn("Skipping event with unparseable start", "module", "GRAPHCAL",
					"id", ge.ID, "value", ge.Start.DateTime, "err", err)
				continue
			}
			end, err := parseGraphDateTime(ge.End.DateTime)
			if err != nil {
				slog.Warn("Skipping event with unparseable end", "module", "GRAPHCAL",
					"id", ge.ID, "value", ge.End.DateTime, "err", err)
				continue
			}
			events = append(events, Event{
				ID:           ge.ID,
				ICalUID:      ge.ICalUID,
				Subject:      ge.Subject,
				Location:     ge.Location.DisplayName,
				Description:  ge.BodyPreview,
				Start:        start,
				End:          end,
				AllDay:       ge.IsAllDay,
				LastModified: parseGraphTimestamp(ge.LastModifiedDateTime),
			})
		}
		pageURL = page.NextLink
	}

	slog.Debug("Fetched calendar view", "module", "GRAPHCAL",
		"calendar", calendarID, "events", len(events))
	return events, nil
}

// parseGraphDateTime parses a Graph event dateTime like
// "2026-07-23T10:00:00.0000000" as UTC (the preferUTC header guarantees UTC).
// Go's time.Parse accepts the fractional seconds even though the layout does
// not mention them.
func parseGraphDateTime(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse graph dateTime %q: %w", s, err)
	}
	return t, nil
}

// parseGraphTimestamp parses an RFC3339 Graph timestamp (e.g.
// lastModifiedDateTime). An unparseable value yields the zero time rather than
// failing the event.
func parseGraphTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		slog.Warn("Failed to parse graph timestamp", "module", "GRAPHCAL", "value", s, "err", err)
		return time.Time{}
	}
	return t
}
