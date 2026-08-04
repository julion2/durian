// Package graphcalendar talks to Microsoft Graph (Outlook) calendars. It
// covers two paths:
//
//   - A one-way export into a vdir layout — one directory per calendar, one
//     .ics file per event — that vdirsyncer / khal can consume. Events come
//     from the Graph calendarView endpoint, which expands recurring series
//     into concrete instances within the requested window, so no RRULE
//     handling is needed there (see ics.go).
//   - The remote side of the two-way sync: Client implements
//     calendarsync.CalendarProvider — FetchMasterEvents retrieves
//     singleInstance and seriesMaster events (with their recurrence
//     definition and changeKey etag), write.go performs the event mutations,
//     and the calendar package's iCalendar round-trip converts events to/from
//     standalone iCalendar documents including RRULEs.
//
// The provider-neutral event model and the local vdir layer live in the
// calendar package; the sync engine itself lives in the calendarsync package.
package graphcalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
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
	// definition, the event type, the changeKey etag, and the meeting
	// metadata (attendees, organizer, the user's RSVP state, online-meeting
	// join info, cancellation flags), plus seriesMasterId/originalStart, which
	// tie a modified occurrence back to the series it deviates from and to the
	// date it replaces.
	masterEventSelect = "id,iCalUId,subject,body,bodyPreview,start,end,isAllDay,location,recurrence,type,changeKey,lastModifiedDateTime," +
		"attendees,organizer,responseStatus,isOnlineMeeting,onlineMeeting,onlineMeetingUrl,isCancelled,isOrganizer," +
		"seriesMasterId,originalStart"

	// preferUTC asks Graph to return event start/end dateTimes in UTC, so
	// parsing never has to interpret Windows timezone names.
	preferUTC = `outlook.timezone="UTC"`

	// preferMaster is the Prefer header for the two-way sync read/write paths:
	// UTC dateTimes plus immutable event ids, so the Graph ids recorded in the
	// sync status survive folder moves.
	preferMaster = preferUTC + `, IdType="ImmutableId"`

	// tokenExpiryBuffer refreshes the cached Graph token this long before its
	// actual expiry, so a request never starts with an about-to-expire token.
	tokenExpiryBuffer = 5 * time.Minute
)

// Client is a minimal Microsoft Graph calendar client: read paths for the
// vdir export and two-way sync download direction, plus the event write
// operations (create/update/delete, see write.go) for the upload direction.
// It implements calendarsync.CalendarProvider.
type Client struct {
	account      *config.AccountConfig
	clientID     string
	clientSecret string
	tenant       string

	// owner is the mailbox owner's email address (the account auth email),
	// used to recognize the owner's own attendee entry and to role-gate
	// attendee uploads. Tests set it directly.
	owner string

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
		owner:        account.GetAuthEmail(),
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		baseURL:      defaultBaseURL,
	}
	c.tokenFn = c.cachedGraphToken
	return c, nil
}

// Client satisfies the neutral provider seam of the sync engine.
var _ calendarsync.CalendarProvider = (*Client)(nil)

// NewWithToken creates a client bound to a fixed bearer token and base URL —
// no OAuth involved. Tests (this package's and the sync engine's harness) use
// it to drive the real client against a local fake server.
func NewWithToken(owner, baseURL, token string, httpClient *http.Client) *Client {
	return &Client{
		owner:      owner,
		httpClient: httpClient,
		baseURL:    baseURL,
		tokenFn: func(context.Context) (string, error) {
			return token, nil
		},
	}
}

// Owner returns the mailbox owner's email address (the account auth email).
func (c *Client) Owner() string {
	return c.owner
}

// IsAuthError implements the provider seam by delegating to the package-level
// IsAuthError.
func (c *Client) IsAuthError(err error) bool {
	return IsAuthError(err)
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

// Calendar is one Outlook calendar, in the neutral list shape of the sync
// engine (Graph reports "" or "auto" for a calendar without explicit color;
// both become an empty HexColor).
type Calendar = calendarsync.Calendar

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

// The neutral event model (Event, Attendee, Person, OwnerResp, Recurrence and
// the content hashes) lives in the provider-neutral calendar package.

// graphDateTime is Graph's {dateTime, timeZone} pair. With the preferUTC
// header, dateTime is already UTC.
type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// graphEmailAddress is Graph's {name, address} pair used inside attendees and
// organizer.
type graphEmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// graphResponseStatus is Graph's responseStatus resource: an RSVP enum plus a
// timestamp (ignored — it churns without content changes).
type graphResponseStatus struct {
	Response string `json:"response"`
}

// graphEvent is the subset of a Graph event resource we consume. Body,
// Recurrence, Type, ChangeKey and the meeting metadata are only present on
// /events (master) queries, never on calendarView queries.
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
	BodyPreview          string               `json:"bodyPreview"`
	Recurrence           *calendar.Recurrence `json:"recurrence"`
	Type                 string               `json:"type"`
	ChangeKey            string               `json:"changeKey"`
	LastModifiedDateTime string               `json:"lastModifiedDateTime"`
	// SeriesMasterID is the id of the series an occurrence or exception
	// belongs to; empty on singleInstance and seriesMaster events.
	SeriesMasterID string `json:"seriesMasterId"`
	// OriginalStart is the start the occurrence was scheduled for before it
	// was modified — the RECURRENCE-ID equivalent. Unlike start it does not
	// move with the exception, which is what makes it the stable key.
	OriginalStart string `json:"originalStart"`
	Attendees     []struct {
		Type         string              `json:"type"`
		Status       graphResponseStatus `json:"status"`
		EmailAddress graphEmailAddress   `json:"emailAddress"`
	} `json:"attendees"`
	Organizer *struct {
		EmailAddress graphEmailAddress `json:"emailAddress"`
	} `json:"organizer"`
	ResponseStatus  graphResponseStatus `json:"responseStatus"`
	IsOnlineMeeting bool                `json:"isOnlineMeeting"`
	OnlineMeeting   *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	// OnlineMeetingURL is the legacy join-link field, used as fallback when
	// onlineMeeting is null.
	OnlineMeetingURL string `json:"onlineMeetingUrl"`
	IsCancelled      bool   `json:"isCancelled"`
	IsOrganizer      bool   `json:"isOrganizer"`
}

// eventFromGraph converts one Graph event resource into an Event. It reports
// ok=false (with a warning logged) when the start or end timestamp is
// unparseable. The description prefers the full body content when Graph
// delivered a plain-text body, falling back to bodyPreview (always the case
// for calendarView queries, which do not select body).
func eventFromGraph(ge graphEvent) (calendar.Event, bool) {
	start, err := parseGraphDateTime(ge.Start.DateTime)
	if err != nil {
		slog.Warn("Skipping event with unparseable start", "module", "GRAPHCAL",
			"id", ge.ID, "value", ge.Start.DateTime, "err", err)
		return calendar.Event{}, false
	}
	end, err := parseGraphDateTime(ge.End.DateTime)
	if err != nil {
		slog.Warn("Skipping event with unparseable end", "module", "GRAPHCAL",
			"id", ge.ID, "value", ge.End.DateTime, "err", err)
		return calendar.Event{}, false
	}

	description := ge.BodyPreview
	if strings.EqualFold(ge.Body.ContentType, "text") && ge.Body.Content != "" {
		description = ge.Body.Content
	}

	var attendees []calendar.Attendee
	for _, a := range ge.Attendees {
		attendees = append(attendees, calendar.Attendee{
			Name:     a.EmailAddress.Name,
			Email:    a.EmailAddress.Address,
			Type:     a.Type,
			Response: a.Status.Response,
		})
	}
	var organizer *calendar.Person
	if ge.Organizer != nil {
		organizer = &calendar.Person{Name: ge.Organizer.EmailAddress.Name, Email: ge.Organizer.EmailAddress.Address}
	}
	onlineMeetingURL := ge.OnlineMeetingURL
	if ge.OnlineMeeting != nil && ge.OnlineMeeting.JoinURL != "" {
		onlineMeetingURL = ge.OnlineMeeting.JoinURL
	}

	return calendar.Event{
		ID:           ge.ID,
		ICalUID:      ge.ICalUID,
		Subject:      ge.Subject,
		Location:     ge.Location.DisplayName,
		Description:  description,
		Start:        start,
		End:          end,
		AllDay:       ge.IsAllDay,
		LastModified: parseGraphTimestamp(ge.LastModifiedDateTime),
		ETag:         ge.ChangeKey,
		Type:         ge.Type,
		Recurrence:   ge.Recurrence,

		Attendees:        attendees,
		Organizer:        organizer,
		IsOrganizer:      ge.IsOrganizer,
		IsCancelled:      ge.IsCancelled,
		IsOnlineMeeting:  ge.IsOnlineMeeting,
		OnlineMeetingURL: onlineMeetingURL,
		OwnerResponse:    calendar.OwnerRespFromGraph(ge.ResponseStatus.Response),
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
func (c *Client) FetchEvents(ctx context.Context, calendarID string, from, to time.Time) ([]calendar.Event, error) {
	headers := map[string]string{"Prefer": preferUTC}

	var events []calendar.Event
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

// FetchInstances implements the provider seam: the windowed, series-expanded
// calendarView fetch (see FetchEvents).
func (c *Client) FetchInstances(ctx context.Context, calendarID string, from, to time.Time) ([]calendar.Event, error) {
	return c.FetchEvents(ctx, calendarID, from, to)
}

// FetchMasterEvents returns all master events of the calendar via the Graph
// /events endpoint: singleInstance events and seriesMaster events carrying
// their Recurrence definition, each series master carrying its modified
// occurrences. The Prefer header (UTC dateTimes, immutable ids) is sent on
// every page (including @odata.nextLink follow-ups).
//
// An "exception" entry is a single occurrence that was changed — moved,
// renamed, given its own attendees. It is not a separate item to the sync
// engine but a deviation of its series, so it is folded into the master it
// names via seriesMasterId, keyed by originalStart. Exceptions are collected
// across ALL pages first, because /events gives no ordering guarantee that a
// master precedes its own exceptions.
//
// A plain "occurrence" entry stays skipped: it is the rule's own output,
// already reproduced by the local expansion, and storing it would duplicate
// every date of every series.
//
// KNOWN GAP — cancelled occurrences. Deleting one date of a Graph series
// removes it from /events entirely rather than leaving a tombstone, so a
// cancellation cannot be observed on this endpoint and the local calendar
// keeps rendering that date. Graph exposes the cancelled dates as the series
// master's cancelledOccurrences collection, but that property is only reliably
// documented on beta, and requesting an unsupported property in $select fails
// the whole query with a 400 — which would break every calendar, not just
// recurring ones. Closing this needs the endpoint verified against the live
// v1.0 API first. Google has no such gap: it returns cancelled instances as
// records.
func (c *Client) FetchMasterEvents(ctx context.Context, calendarID string) ([]calendar.Event, error) {
	headers := map[string]string{"Prefer": preferMaster}

	var events []calendar.Event
	// byID indexes into events by the Graph master id the exceptions name. It
	// stores positions, not pointers, so appending cannot leave a stale
	// reference behind.
	byID := make(map[string]int)
	var exceptions []graphEvent

	pageURL := fmt.Sprintf("%s/me/calendars/%s/events?$select=%s&$top=100",
		c.baseURL, url.PathEscape(calendarID), masterEventSelect)
	for pageURL != "" {
		var page eventPage
		if err := c.doJSON(ctx, pageURL, headers, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch master events for %s: %w", calendarID, err)
		}
		for _, ge := range page.Value {
			switch ge.Type {
			case "singleInstance", "seriesMaster":
				if ev, ok := eventFromGraph(ge); ok {
					byID[ge.ID] = len(events)
					events = append(events, ev)
				}
			case "exception":
				exceptions = append(exceptions, ge)
			default: // "occurrence" and anything unknown
				slog.Debug("Skipping non-master event", "module", "GRAPHCAL",
					"id", ge.ID, "type", ge.Type)
			}
		}
		pageURL = page.NextLink
	}

	attachExceptions(events, byID, exceptions)

	slog.Debug("Fetched master events", "module", "GRAPHCAL",
		"calendar", calendarID, "events", len(events), "exceptions", len(exceptions))
	return events, nil
}

// attachExceptions folds the collected exception events into their series
// masters as Overrides, keyed by originalStart and sorted by it — so the .ics
// bytes, and with them the local file hash the sync engine diffs on, do not
// depend on the order Graph happened to page them in.
//
// An exception whose master is absent is dropped with a warning: its series
// lives in a calendar the include filter excluded, or was deleted while this
// fetch was paging. Promoting it to a master of its own would upload a series
// collapsed to that single occurrence.
func attachExceptions(events []calendar.Event, byID map[string]int, exceptions []graphEvent) {
	touched := make(map[int]bool)
	for _, ge := range exceptions {
		idx, ok := byID[ge.SeriesMasterID]
		if !ok {
			slog.Warn("Dropping series exception without a master", "module", "GRAPHCAL",
				"id", ge.ID, "master", ge.SeriesMasterID)
			continue
		}
		original, err := parseGraphDateTime(ge.OriginalStart)
		if err != nil {
			slog.Warn("Dropping series exception without a usable original start",
				"module", "GRAPHCAL", "id", ge.ID, "value", ge.OriginalStart, "err", err)
			continue
		}
		ev, ok := eventFromGraph(ge)
		if !ok {
			continue
		}
		ev.RecurrenceID = original
		// An override is a deviation, never a series of its own.
		ev.Recurrence = nil
		ev.ExceptionDates = nil

		master := &events[idx]
		master.Overrides = append(master.Overrides, ev)
		touched[idx] = true
	}

	for idx := range touched {
		master := &events[idx]
		sort.Slice(master.Overrides, func(i, j int) bool {
			return master.Overrides[i].RecurrenceID.Before(master.Overrides[j].RecurrenceID)
		})
	}
}

// GetEvent returns one event by its Graph event id, requesting the same field
// set (masterEventSelect) and UTC preference as FetchMasterEvents — so the
// returned content is exactly what the next FetchMasterEvents will report for
// this event. The sync engine reads an event back through this after a
// create/update, because Graph normalizes events server-side right after a
// write, so the settled read-back — not the POST/PATCH response — is the
// canonical content the calendar.EventContentHash baseline must be computed
// from.
// calendarID is unused: Graph event ids are mailbox-global.
func (c *Client) GetEvent(ctx context.Context, calendarID, eventID string) (calendar.Event, error) {
	_ = calendarID
	reqURL := fmt.Sprintf("%s/me/events/%s?$select=%s",
		c.baseURL, url.PathEscape(eventID), masterEventSelect)

	var ge graphEvent
	if err := c.doJSON(ctx, reqURL, map[string]string{"Prefer": preferMaster}, &ge); err != nil {
		return calendar.Event{}, fmt.Errorf("failed to get event %s: %w", eventID, err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		return calendar.Event{}, fmt.Errorf("failed to parse event %s", eventID)
	}
	slog.Debug("Fetched event", "module", "GRAPHCAL", "id", ev.ID, "changeKey", ev.ETag)
	return ev, nil
}

// ParseEventJSON decodes one raw Graph event resource into the neutral Event,
// exactly like the fetch paths do (ok=false on undecodable/unparseable
// input). The sync-engine tests use it to compute the expected content-hash
// baselines for the fake Graph payloads they serve.
func ParseEventJSON(data []byte) (calendar.Event, bool) {
	var ge graphEvent
	if err := json.Unmarshal(data, &ge); err != nil {
		return calendar.Event{}, false
	}
	return eventFromGraph(ge)
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
