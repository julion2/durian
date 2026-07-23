// Package graphcalendar talks to Microsoft Graph (Outlook) calendars. It
// covers two paths:
//
//   - A one-way export into a vdir layout — one directory per calendar, one
//     .ics file per event — that vdirsyncer / khal can consume. Events come
//     from the Graph calendarView endpoint, which expands recurring series
//     into concrete instances within the requested window, so no RRULE
//     handling is needed there (see ics.go).
//   - The foundation for two-way sync: FetchMasterEvents retrieves
//     singleInstance and seriesMaster events (with their recurrence
//     definition and changeKey etag), and ical_roundtrip.go converts them
//     to/from standalone iCalendar documents including RRULEs.
package graphcalendar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// masterEventSelect is the field set requested on /events queries (master
	// events for two-way sync): eventSelect plus the full body, the recurrence
	// definition, the event type, and the changeKey etag.
	masterEventSelect = "id,iCalUId,subject,body,bodyPreview,start,end,isAllDay,location,recurrence,type,changeKey,lastModifiedDateTime"

	// preferUTC asks Graph to return event start/end dateTimes in UTC, so
	// parsing never has to interpret Windows timezone names.
	preferUTC = `outlook.timezone="UTC"`

	// tokenExpiryBuffer refreshes the cached Graph token this long before its
	// actual expiry, so a request never starts with an about-to-expire token.
	tokenExpiryBuffer = 5 * time.Minute
)

// Client is a minimal Microsoft Graph calendar client: read paths for the
// vdir export and two-way sync download direction, plus the event write
// operations (create/update/delete, see write.go) for the upload direction.
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

// doJSON executes one authenticated Graph GET and decodes the JSON response
// into out. See doRequest for the retry behavior.
func (c *Client) doJSON(ctx context.Context, reqURL string, extraHeaders map[string]string, out any) error {
	return c.doRequest(ctx, http.MethodGet, reqURL, extraHeaders, nil, out)
}

// doJSONBody marshals in as the JSON request body and executes one
// authenticated Graph request (POST/PATCH) via doRequest, decoding the JSON
// response into out.
func (c *Client) doJSONBody(ctx context.Context, method, reqURL string, extraHeaders map[string]string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("failed to marshal graph request body: %w", err)
	}
	return c.doRequest(ctx, method, reqURL, extraHeaders, payload, out)
}

// doRequest executes one authenticated Graph request with throttle handling —
// up to 3 retries on 429 honoring Retry-After, and one retry with a short
// backoff on 503/504 — then decodes the JSON response into out (skipped when
// out is nil, e.g. for DELETE). body, when non-nil, is sent as an
// application/json request body (re-sent from the start on every retry). All
// waits respect ctx cancellation. Non-2xx responses become a statusError with
// a body snippet.
func (c *Client) doRequest(ctx context.Context, method, reqURL string, extraHeaders map[string]string, body []byte, out any) error {
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

		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
		if err != nil {
			return fmt.Errorf("failed to build graph request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
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

// Event is one Graph calendar event. Two fetch paths fill it:
//
//   - FetchEvents (calendarView) returns pre-expanded concrete instances, so
//     ID is unique per occurrence while ICalUID is shared across a series;
//     Type, ChangeKey and Recurrence stay empty.
//   - FetchMasterEvents (/events) returns singleInstance and seriesMaster
//     events; seriesMaster carries the series definition in Recurrence, and
//     ChangeKey/Type are populated for the two-way sync engine.
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

	// ChangeKey is the remote etag of the event; it changes on every remote
	// modification.
	ChangeKey string
	// Type is the Graph event type: "singleInstance", "seriesMaster",
	// "occurrence" or "exception". Empty for calendarView results.
	Type string
	// Recurrence is the series definition of a seriesMaster event; nil for
	// non-recurring events.
	Recurrence *Recurrence
}

// eventContentHash returns a SHA-256 hex digest over the MEANINGFUL content of
// an event — the fields a user actually edits — serialized deterministically.
// The two-way sync engine uses it as the remote-change signal: same content
// yields the same hash no matter which read path produced the Event
// (FetchMasterEvents, GetEvent, or a CreateEvent response).
//
// Volatile identity/bookkeeping fields are deliberately excluded: ChangeKey is
// NOT a stable etag — Graph rewrites it between a write and subsequent reads
// (and over time) without any content change — and LastModified churns with
// it. ID, ICalUID and Type are identity/shape, not content. Fields are joined
// with NUL separators (no meaningful field contains NUL) so adjacent values
// can never be confused; Description line endings are normalized to LF, and a
// Recurrence is canonicalized via its fixed-field JSON encoding.
func eventContentHash(e Event) string {
	var recurrence string
	if e.Recurrence != nil {
		data, err := json.Marshal(e.Recurrence)
		if err != nil {
			// Unreachable for a plain struct; keep a deterministic fallback
			// rather than failing the hash.
			slog.Warn("Failed to marshal recurrence for content hash", "module", "GRAPHCAL",
				"id", e.ID, "err", err)
			recurrence = fmt.Sprintf("%+v", *e.Recurrence)
		} else {
			recurrence = string(data)
		}
	}

	h := sha256.New()
	for _, field := range []string{
		e.Subject,
		e.Start.UTC().Format(time.RFC3339),
		e.End.UTC().Format(time.RFC3339),
		strconv.FormatBool(e.AllDay),
		e.Location,
		normalizeText(e.Description),
		recurrence,
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Recurrence mirrors the Graph patternedRecurrence resource: how a series
// repeats (Pattern) and over which span (Range).
type Recurrence struct {
	Pattern RecurrencePattern `json:"pattern"`
	Range   RecurrenceRange   `json:"range"`
}

// RecurrencePattern is the Graph recurrencePattern resource. Type is one of
// daily, weekly, absoluteMonthly, relativeMonthly, absoluteYearly,
// relativeYearly.
type RecurrencePattern struct {
	Type     string `json:"type"`
	Interval int    `json:"interval"`
	// DaysOfWeek holds lowercase day names ("monday", ...) for weekly and
	// relative patterns.
	DaysOfWeek []string `json:"daysOfWeek,omitempty"`
	// DayOfMonth applies to absoluteMonthly/absoluteYearly patterns.
	DayOfMonth int `json:"dayOfMonth,omitempty"`
	// Month (1-12) applies to yearly patterns.
	Month int `json:"month,omitempty"`
	// Index is the week ordinal for relative patterns: first, second, third,
	// fourth or last.
	Index          string `json:"index,omitempty"`
	FirstDayOfWeek string `json:"firstDayOfWeek,omitempty"`
}

// RecurrenceRange is the Graph recurrenceRange resource. Type is one of
// noEnd, endDate, numbered. Dates are "YYYY-MM-DD" strings as Graph sends
// them.
type RecurrenceRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate,omitempty"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
}

// graphDateTime is Graph's {dateTime, timeZone} pair. With the preferUTC
// header, dateTime is already UTC.
type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// graphEvent is the subset of a Graph event resource we consume. Body,
// Recurrence, Type and ChangeKey are only present on /events (master)
// queries, never on calendarView queries.
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
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	BodyPreview          string      `json:"bodyPreview"`
	Recurrence           *Recurrence `json:"recurrence"`
	Type                 string      `json:"type"`
	ChangeKey            string      `json:"changeKey"`
	LastModifiedDateTime string      `json:"lastModifiedDateTime"`
}

// eventFromGraph converts one Graph event resource into an Event. It reports
// ok=false (with a warning logged) when the start or end timestamp is
// unparseable. The description prefers the full body content when Graph
// delivered a plain-text body, falling back to bodyPreview (always the case
// for calendarView queries, which do not select body).
func eventFromGraph(ge graphEvent) (Event, bool) {
	start, err := parseGraphDateTime(ge.Start.DateTime)
	if err != nil {
		slog.Warn("Skipping event with unparseable start", "module", "GRAPHCAL",
			"id", ge.ID, "value", ge.Start.DateTime, "err", err)
		return Event{}, false
	}
	end, err := parseGraphDateTime(ge.End.DateTime)
	if err != nil {
		slog.Warn("Skipping event with unparseable end", "module", "GRAPHCAL",
			"id", ge.ID, "value", ge.End.DateTime, "err", err)
		return Event{}, false
	}

	description := ge.BodyPreview
	if strings.EqualFold(ge.Body.ContentType, "text") && ge.Body.Content != "" {
		description = ge.Body.Content
	}

	return Event{
		ID:           ge.ID,
		ICalUID:      ge.ICalUID,
		Subject:      ge.Subject,
		Location:     ge.Location.DisplayName,
		Description:  description,
		Start:        start,
		End:          end,
		AllDay:       ge.IsAllDay,
		LastModified: parseGraphTimestamp(ge.LastModifiedDateTime),
		ChangeKey:    ge.ChangeKey,
		Type:         ge.Type,
		Recurrence:   ge.Recurrence,
	}, true
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
			if ev, ok := eventFromGraph(ge); ok {
				events = append(events, ev)
			}
		}
		pageURL = page.NextLink
	}

	slog.Debug("Fetched calendar view", "module", "GRAPHCAL",
		"calendar", calendarID, "events", len(events))
	return events, nil
}

// FetchMasterEvents returns all master events of the calendar via the Graph
// /events endpoint: singleInstance events and seriesMaster events carrying
// their Recurrence definition. Expanded "occurrence" and "exception" entries
// are skipped — the two-way sync engine works on series definitions, not
// instances. The Prefer: outlook.timezone="UTC" header is sent on every page
// (including @odata.nextLink follow-ups) so start/end come back in UTC.
func (c *Client) FetchMasterEvents(ctx context.Context, calendarID string) ([]Event, error) {
	headers := map[string]string{"Prefer": preferUTC}

	var events []Event
	pageURL := fmt.Sprintf("%s/me/calendars/%s/events?$select=%s&$top=100",
		c.baseURL, url.PathEscape(calendarID), masterEventSelect)
	for pageURL != "" {
		var page eventPage
		if err := c.doJSON(ctx, pageURL, headers, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch master events for %s: %w", calendarID, err)
		}
		for _, ge := range page.Value {
			if ge.Type != "singleInstance" && ge.Type != "seriesMaster" {
				slog.Debug("Skipping non-master event", "module", "GRAPHCAL",
					"id", ge.ID, "type", ge.Type)
				continue
			}
			if ev, ok := eventFromGraph(ge); ok {
				events = append(events, ev)
			}
		}
		pageURL = page.NextLink
	}

	slog.Debug("Fetched master events", "module", "GRAPHCAL",
		"calendar", calendarID, "events", len(events))
	return events, nil
}

// GetEvent returns one event by its Graph event id, requesting the same field
// set (masterEventSelect) and UTC preference as FetchMasterEvents — so the
// returned content is exactly what the next FetchMasterEvents will report for
// this event. The sync engine reads an event back through this after a
// create/update, because Graph normalizes events server-side right after a
// write, so the settled read-back — not the POST/PATCH response — is the
// canonical content the eventContentHash baseline must be computed from.
func (c *Client) GetEvent(ctx context.Context, eventID string) (Event, error) {
	reqURL := fmt.Sprintf("%s/me/events/%s?$select=%s",
		c.baseURL, url.PathEscape(eventID), masterEventSelect)

	var ge graphEvent
	if err := c.doJSON(ctx, reqURL, map[string]string{"Prefer": preferUTC}, &ge); err != nil {
		return Event{}, fmt.Errorf("failed to get event %s: %w", eventID, err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		return Event{}, fmt.Errorf("failed to parse event %s", eventID)
	}
	slog.Debug("Fetched event", "module", "GRAPHCAL", "id", ev.ID, "changeKey", ev.ChangeKey)
	return ev, nil
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
