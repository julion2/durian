// Package googlecalendar talks to the Google Calendar API (v3) and implements
// the remote side of the two-way calendar sync: Client satisfies
// calendarsync.CalendarProvider. The read paths (calendar listing, master
// events with their recurrence definition and etag, windowed instance
// expansion, single-event read-back) live here; the write operations
// (create/update/delete/RSVP) live in write.go.
//
// The provider-neutral event model lives in the calendar package; the sync
// engine itself lives in the calendarsync package. Google's RRULE strings are
// bridged into the neutral Recurrence via rrule-go and
// calendar.ROptionToRecurrence.
package googlecalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teambition/rrule-go"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/oauth"
)

const (
	// defaultBaseURL is the Calendar API root, without a trailing slash.
	defaultBaseURL = "https://www.googleapis.com/calendar/v3"

	// pageSize is the maxResults value sent on list queries.
	pageSize = 250

	// tokenExpiryBuffer refreshes the cached access token this long before its
	// actual expiry, so a request never starts with an about-to-expire token.
	tokenExpiryBuffer = 5 * time.Minute

	// maxThrottleBackoff caps the exponential part of the throttle backoff, per
	// Google's documented usage-limit guidance.
	maxThrottleBackoff = 32 * time.Second

	// maxBackoffShift bounds the exponential shift. 2^5 s already reaches
	// maxThrottleBackoff, and anything past 2^33 s overflows time.Duration.
	maxBackoffShift = 5

	// maxRetryAfter bounds how long a server may make this process wait before
	// the request is failed instead. See clampRetryAfter.
	maxRetryAfter = 2 * time.Minute

	// jitterSpan is the random amount added on top of the exponential backoff
	// (Google documents up to one second), so concurrently throttled requests
	// do not all retry at the same instant.
	jitterSpan = time.Second
)

// Client is a minimal Google Calendar client: the read paths of the two-way
// sync download direction plus the event write operations (see write.go). It
// implements calendarsync.CalendarProvider.
type Client struct {
	account      *config.AccountConfig
	clientID     string
	clientSecret string

	// owner is the calendar owner's email address (the account auth email),
	// used to recognize the owner's own attendee entry and to role-gate
	// attendee uploads. Tests set it directly.
	owner string

	httpClient *http.Client
	// baseURL is the Calendar API root without trailing slash. Defaults to
	// defaultBaseURL; tests point it at an httptest.Server.
	baseURL string
	// tokenFn returns a valid bearer token. Defaults to the cached
	// oauth.GetValidToken path (cachedToken); tests override it.
	tokenFn func(ctx context.Context) (string, error)

	// mu guards cached.
	mu     sync.Mutex
	cached *oauth.Token
}

// Client satisfies the neutral provider seam of the sync engine.
var _ calendarsync.CalendarProvider = (*Client)(nil)

// New creates a Google Calendar client for the given Google OAuth account.
// Unlike Microsoft (one token per resource), Google issues a single access
// token covering all consented scopes, so the standard oauth.GetValidToken
// path serves the Calendar API too. The token is fetched lazily on first use,
// so construction never touches the network.
func New(account *config.AccountConfig) (*Client, error) {
	if account.OAuth == nil || account.OAuth.Provider != "google" {
		return nil, fmt.Errorf("google calendar sync requires a Google OAuth account, got %s", account.Email)
	}

	c := &Client{
		account:      account,
		clientID:     account.OAuth.ClientID,
		clientSecret: account.OAuth.ClientSecret,
		owner:        account.GetAuthEmail(),
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		baseURL:      defaultBaseURL,
	}
	c.tokenFn = c.cachedToken
	return c, nil
}

// NewWithToken creates a client bound to a fixed bearer token and base URL —
// no OAuth involved. Tests use it to drive the real client against a local
// fake server.
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

// Owner returns the calendar owner's email address (the account auth email).
func (c *Client) Owner() string {
	return c.owner
}

// IsAuthError implements the provider seam by delegating to the package-level
// IsAuthError.
func (c *Client) IsAuthError(err error) bool {
	return IsAuthError(err)
}

// MARK: - Token source

// cachedToken returns the cached access token, minting a fresh one via
// oauth.GetValidToken when none is cached or it expires within
// tokenExpiryBuffer. GetValidToken refreshes through the stored refresh token
// and persists any rotation itself; the access token is additionally cached
// here in memory to avoid a keychain read per request. The tenant argument is
// Microsoft-only and stays empty for Google.
func (c *Client) cachedToken(_ context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cached != nil && !c.cached.IsExpiredWithBuffer(tokenExpiryBuffer) {
		return c.cached.AccessToken, nil
	}

	token, err := oauth.GetValidToken(c.account.GetAuthEmail(), c.clientID, c.clientSecret, "")
	if err != nil {
		return "", fmt.Errorf("failed to get Google token for %s: %w", c.account.Email, err)
	}
	c.cached = token
	slog.Debug("Minted Google access token", "module", "GOOGLECAL", "expiry", token.Expiry)
	return token.AccessToken, nil
}

// MARK: - HTTP client

// statusError is a non-2xx Calendar API response, carrying the HTTP status so
// callers can react to specific codes.
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("google calendar request failed: status %d: %s", e.status, e.body)
}

// IsAuthError reports whether err looks like a Google authentication or
// consent problem (expired/missing token, a 401, or a 403 such as
// insufficientPermissions/accessNotConfigured), so the command can hint at
// re-running `durian auth login`. Google also uses 403 for rate limiting
// (rateLimitExceeded, userRateLimitExceeded, quotaExceeded); those are
// transient, not auth errors, and are excluded by their reason string.
func IsAuthError(err error) bool {
	if errors.Is(err, oauth.ErrTokenExpired) ||
		errors.Is(err, oauth.ErrTokenNotFound) ||
		errors.Is(err, oauth.ErrRefreshFailed) {
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.status {
		case http.StatusUnauthorized:
			return true
		case http.StatusForbidden:
			return !isRateLimited(se)
		}
	}
	return false
}

// isRateLimited reports whether a 403 is Google's throttle response rather
// than a permission problem. Google returns 429 for some quota breaches and
// 403 with a rateLimitExceeded / userRateLimitExceeded / quotaExceeded reason
// for others; both are transient and must be retried, not surfaced as an auth
// failure or a hard error.
func isRateLimited(se *statusError) bool {
	return se.status == http.StatusForbidden &&
		(strings.Contains(se.body, "ateLimitExceeded") ||
			strings.Contains(se.body, "quotaExceeded"))
}

// doJSON executes one authenticated Calendar API GET and decodes the JSON
// response into out. See doRequest for the retry behavior.
func (c *Client) doJSON(ctx context.Context, reqURL string, extraHeaders map[string]string, out any) error {
	return c.doRequest(ctx, http.MethodGet, reqURL, extraHeaders, nil, out)
}

// doJSONBody marshals in as the JSON request body and executes one
// authenticated Calendar API request (POST/PATCH) via doRequest, decoding the
// JSON response into out (skipped when out is nil).
func (c *Client) doJSONBody(ctx context.Context, method, reqURL string, extraHeaders map[string]string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("failed to marshal google calendar request body: %w", err)
	}
	return c.doRequest(ctx, method, reqURL, extraHeaders, payload, out)
}

// doRequest executes one authenticated Calendar API request with throttle
// handling — up to 3 retries on 429 honoring Retry-After, and one retry with
// a short backoff on 503/504 — then decodes the JSON response into out
// (skipped when out is nil, e.g. for DELETE). body, when non-nil, is sent as
// an application/json request body (re-sent from the start on every retry).
// All waits respect ctx cancellation. Non-2xx responses become a statusError
// with a body snippet.
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
			return fmt.Errorf("failed to build google calendar request: %w", err)
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
			return fmt.Errorf("google calendar request failed: %w", err)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && throttleRetries < maxThrottleRetries:
			throttleRetries++
			delay := throttleDelay(resp, throttleRetries)
			drainClose(resp)
			slog.Warn("Google Calendar throttled request, backing off", "module", "GOOGLECAL",
				"retry", throttleRetries, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return err
			}
		case resp.StatusCode == http.StatusForbidden && throttleRetries < maxThrottleRetries:
			// A 403 is usually a permission problem, but Google also uses it
			// for per-user rate limiting. Classify from the reason string
			// before deciding: throttles back off like a 429, everything else
			// falls through to the error path unretried.
			se := newStatusError(resp)
			drainClose(resp)
			var rateLimited bool
			if serr, ok := se.(*statusError); ok {
				rateLimited = isRateLimited(serr)
			}
			if !rateLimited {
				return se
			}
			throttleRetries++
			// A 403 throttle carries no Retry-After, so this is the pure
			// backoff case.
			delay := backoffWithJitter(throttleRetries)
			slog.Warn("Google Calendar rate-limited request (403), backing off", "module", "GOOGLECAL",
				"retry", throttleRetries, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return err
			}
		case (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) && !transientRetried:
			transientRetried = true
			drainClose(resp)
			slog.Warn("Google Calendar transient error, retrying once", "module", "GOOGLECAL",
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
				return fmt.Errorf("failed to decode google calendar response: %w", err)
			}
			return nil
		}
	}
}

// throttleDelay returns how long to wait before retrying a throttled request:
// the server's own Retry-After when it sent one, otherwise Google's documented
// truncated exponential backoff with jitter. attempt counts from 1.
func throttleDelay(resp *http.Response, attempt int) time.Duration {
	if d, ok := retryAfter(resp); ok {
		return d
	}
	return backoffWithJitter(attempt)
}

// backoffWithJitter implements the backoff Google documents for its usage
// limits: min(2^n seconds, cap) plus up to a second of jitter.
//
// The jitter is not decoration, though the reason is not within one account:
// PlanAll walks an account's calendars sequentially, so those requests cannot
// collide with each other. It is across accounts. serve runs one autosync
// goroutine per account, and Google meters its quota per OAuth PROJECT — every
// account of every durian install shares it. A fixed 2^n makes all of them
// retry on the same instant and rebuild the burst that caused the throttle.
func backoffWithJitter(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Clamp the SHIFT, not the result. time.Duration is int64 nanoseconds, so
	// 1<<34 seconds already exceeds its range and wraps to a negative value —
	// which sails past a "> maxThrottleBackoff" check and makes the caller
	// sleep for nothing, turning the backoff into a hot retry loop.
	if attempt > maxBackoffShift {
		attempt = maxBackoffShift
	}
	base := time.Duration(1<<attempt) * time.Second
	if base > maxThrottleBackoff {
		base = maxThrottleBackoff
	}
	if jitterSpan <= 0 {
		return base
	}
	return base + rand.N(jitterSpan)
}

// retryAfter parses the Retry-After header in BOTH forms RFC 9110 allows: a
// delay in seconds, and an HTTP-date. Reading only the numeric form silently
// falls back to a fixed one-second wait against a server that answered with a
// date — which retries far too early and earns another throttle.
//
// A date already in the past yields zero: the server named an instant that has
// come, so the request may go out immediately. The bool distinguishes "no
// usable header" from "wait exactly nothing".
func retryAfter(resp *http.Response) (time.Duration, bool) {
	s := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if s == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
		return clampRetryAfter(time.Duration(secs)*time.Second, s), true
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return clampRetryAfter(d, s), true
		}
		return 0, true
	}
	return 0, false
}

// clampRetryAfter bounds what the server may make this process wait.
//
// Honoring an hour-long Retry-After literally is worse than useless: the
// manual sync has no deadline of its own, so it would block that long holding
// the run lock — which also stalls the background loop — while its "backing
// off" message is invisible at the default log level. The background loop
// would merely hit its own timeout and report a context error instead of a
// throttle. Waiting the cap and letting the request fail surfaces the problem
// in seconds and leaves the retry to the next run.
func clampRetryAfter(d time.Duration, header string) time.Duration {
	if d <= maxRetryAfter {
		return d
	}
	slog.Warn("Retry-After exceeds the cap, waiting the cap instead", "module", "GOOGLECAL",
		"retryAfter", header, "cap", maxRetryAfter)
	return maxRetryAfter
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

// calendarListPage is one page of GET /users/me/calendarList.
type calendarListPage struct {
	Items []struct {
		ID              string `json:"id"`
		Summary         string `json:"summary"`
		BackgroundColor string `json:"backgroundColor"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ListCalendars returns all calendars of the account, following pagination.
// The calendarList backgroundColor is already a "#RRGGBB" value.
func (c *Client) ListCalendars(ctx context.Context) ([]calendarsync.Calendar, error) {
	base := c.baseURL + "/users/me/calendarList"
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(pageSize))

	var calendars []calendarsync.Calendar
	for {
		var page calendarListPage
		if err := c.doJSON(ctx, base+"?"+q.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("failed to list calendars: %w", err)
		}
		for _, cal := range page.Items {
			calendars = append(calendars, calendarsync.Calendar{
				ID:       cal.ID,
				Name:     cal.Summary,
				HexColor: cal.BackgroundColor,
			})
		}
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}

	slog.Debug("Listed calendars", "module", "GOOGLECAL", "count", len(calendars))
	return calendars, nil
}

// MARK: - Event decoding

// googleDateTime is the Calendar API {date, dateTime, timeZone} triple: date
// ("YYYY-MM-DD") for all-day events, dateTime (RFC3339 with offset) otherwise.
type googleDateTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// googleAttendee is one entry of the Calendar API attendees list.
type googleAttendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	Optional       bool   `json:"optional"`
	Resource       bool   `json:"resource"`
	Self           bool   `json:"self"`
	ResponseStatus string `json:"responseStatus"`
}

// googlePerson is the Calendar API organizer/creator shape.
type googlePerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Self        bool   `json:"self"`
}

// googleEvent is the subset of a Calendar API event resource we consume.
type googleEvent struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"`
	ETag        string         `json:"etag"`
	ICalUID     string         `json:"iCalUID"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Location    string         `json:"location"`
	Start       googleDateTime `json:"start"`
	End         googleDateTime `json:"end"`
	Updated     string         `json:"updated"`
	// Recurrence holds the raw RRULE/EXDATE/RDATE lines of a series master.
	Recurrence []string `json:"recurrence"`
	// RecurringEventID marks expanded or detached instances of a series: the
	// id of the series master they belong to.
	RecurringEventID string `json:"recurringEventId"`
	// OriginalStartTime is the start the instance was SCHEDULED for by the
	// rule, which stays its identity within the series even after it was
	// moved — the RECURRENCE-ID equivalent. Only set on instances. Cancelled
	// instances carry it while carrying no start/end at all.
	OriginalStartTime googleDateTime   `json:"originalStartTime"`
	Attendees         []googleAttendee `json:"attendees"`
	Organizer         *googlePerson    `json:"organizer"`
	// HangoutLink is the legacy join-link field, used as fallback when
	// conferenceData carries no video entry point.
	HangoutLink    string `json:"hangoutLink"`
	ConferenceData *struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
}

// eventFromGoogle converts one Calendar API event resource into a neutral
// calendar.Event. It reports ok=false (with a warning logged) when the start
// or end timestamp is missing or unparseable — cancelled tombstones, which
// carry no times at all, are filtered by the callers before mapping.
func (c *Client) eventFromGoogle(g googleEvent) (calendar.Event, bool) {
	start, allDay, err := parseGoogleTime(g.Start)
	if err != nil {
		slog.Warn("Skipping event with unparseable start", "module", "GOOGLECAL",
			"id", g.ID, "err", err)
		return calendar.Event{}, false
	}
	end, _, err := parseGoogleTime(g.End)
	if err != nil {
		slog.Warn("Skipping event with unparseable end", "module", "GOOGLECAL",
			"id", g.ID, "err", err)
		return calendar.Event{}, false
	}

	var attendees []calendar.Attendee
	ownerResponse := calendar.OwnerRespNone
	for _, a := range g.Attendees {
		attendee := calendar.Attendee{
			Name:     a.DisplayName,
			Email:    a.Email,
			Type:     attendeeTypeFromGoogle(a),
			Response: responseFromGoogle(a.ResponseStatus),
		}
		attendees = append(attendees, attendee)
		if a.Self || (c.owner != "" && strings.EqualFold(a.Email, c.owner)) {
			ownerResponse = calendar.OwnerRespFromGraph(attendee.Response)
		}
	}

	var organizer *calendar.Person
	isOrganizer := false
	if g.Organizer != nil {
		organizer = &calendar.Person{Name: g.Organizer.DisplayName, Email: g.Organizer.Email}
		isOrganizer = g.Organizer.Self ||
			(c.owner != "" && strings.EqualFold(g.Organizer.Email, c.owner))
	}
	if isOrganizer {
		ownerResponse = calendar.OwnerRespOrganizer
	}

	onlineMeetingURL := g.HangoutLink
	if g.ConferenceData != nil {
		for _, ep := range g.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" && ep.URI != "" {
				onlineMeetingURL = ep.URI
				break
			}
		}
	}

	// Mirror the Graph event-type vocabulary so downstream consumers see the
	// familiar shapes: a master with recurrence lines is a seriesMaster, an
	// item pointing at its series is an occurrence/exception equivalent.
	eventType := "singleInstance"
	switch {
	case len(g.Recurrence) > 0:
		eventType = "seriesMaster"
	case g.RecurringEventID != "":
		eventType = "occurrence"
	}

	recurrence, exDates, opaqueRecurrence := recurrenceFromGoogle(g.Recurrence, start, g.ID)

	return calendar.Event{
		ID:               g.ID,
		ICalUID:          g.ICalUID,
		Subject:          g.Summary,
		Location:         g.Location,
		Description:      g.Description,
		Start:            start,
		End:              end,
		AllDay:           allDay,
		LastModified:     parseGoogleTimestamp(g.Updated),
		ETag:             g.ETag,
		Type:             eventType,
		Recurrence:       recurrence,
		ExceptionDates:   exDates,
		OpaqueRecurrence: opaqueRecurrence,

		Attendees:        attendees,
		Organizer:        organizer,
		IsOrganizer:      isOrganizer,
		IsCancelled:      g.Status == "cancelled",
		IsOnlineMeeting:  onlineMeetingURL != "",
		OnlineMeetingURL: onlineMeetingURL,
		OwnerResponse:    ownerResponse,
	}, true
}

// parseGoogleTime parses one {date, dateTime} boundary: an all-day date
// ("YYYY-MM-DD", end-exclusive on the end boundary, kept as-is like the Graph
// path) becomes midnight UTC with allDay=true; a timed dateTime (RFC3339 with
// offset) is normalized to UTC.
func parseGoogleTime(dt googleDateTime) (t time.Time, allDay bool, err error) {
	if dt.Date != "" {
		t, err = time.ParseInLocation(calendar.GraphDateFormat, dt.Date, time.UTC)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("failed to parse google date %q: %w", dt.Date, err)
		}
		return t, true, nil
	}
	if dt.DateTime == "" {
		return time.Time{}, false, errors.New("event boundary has neither date nor dateTime")
	}
	t, err = time.Parse(time.RFC3339, dt.DateTime)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("failed to parse google dateTime %q: %w", dt.DateTime, err)
	}
	return t.UTC(), false, nil
}

// parseGoogleTimestamp parses an RFC3339 timestamp (e.g. updated). An
// unparseable value yields the zero time rather than failing the event.
func parseGoogleTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		slog.Warn("Failed to parse google timestamp", "module", "GOOGLECAL", "value", s, "err", err)
		return time.Time{}
	}
	return t.UTC()
}

// recurrenceFromGoogle bridges the raw recurrence lines of a series master
// into the neutral model: the RRULE line is parsed via rrule-go and mapped
// through calendar.ROptionToRecurrence (start provides the range start date),
// and every EXDATE line contributes the cancelled occurrences.
//
// EXDATE is part of the series definition, not a detail of it: dropping those
// lines leaves a rule that still produces occurrences the calendar no longer
// has, and the local expansion renders meetings that were cancelled months
// ago. RDATE (extra dates outside the rule) has no place in the neutral
// Recurrence yet and is still logged and dropped; unlike EXDATE it fails
// safe — a missing RDATE hides an event rather than inventing one.
func recurrenceFromGoogle(lines []string, start time.Time, id string) (rec *calendar.Recurrence, exDates []time.Time, opaque bool) {
	var rruleLine string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "RRULE"):
			if rruleLine != "" {
				slog.Warn("Ignoring additional RRULE line", "module", "GOOGLECAL", "id", id)
				continue
			}
			rruleLine = line
		case strings.HasPrefix(line, "EXDATE"):
			dates, err := calendar.ParseExDateLine(line)
			if err != nil {
				// The cancellations on this line are now unknown. Uploading a
				// recurrence rebuilt without them would revive the cancelled
				// occurrences on the server — and for a meeting they would
				// reappear in every attendee's calendar.
				slog.Warn("Keeping series with an unreadable EXDATE line opaque", "module", "GOOGLECAL",
					"id", id, "line", line, "err", err)
				opaque = true
				continue
			}
			exDates = append(exDates, dates...)
		default:
			// RDATE, EXRULE and anything else the neutral model cannot hold.
			// Unlike a missing RRULE this is not "no recurrence": the rule
			// exists and durian just cannot express all of it, so the upload
			// must not rewrite the recurrence from the part it understood.
			slog.Warn("Keeping series with an unsupported recurrence line opaque", "module", "GOOGLECAL",
				"id", id, "line", line)
			opaque = true
		}
	}
	if opaque {
		// A rule component was lost. Report whatever was parsed for display,
		// but never let the write paths reconstruct the recurrence from it.
		return nil, exDates, true
	}
	if rruleLine == "" {
		// Genuinely no rule: not opaque, just not a series.
		return nil, exDates, false
	}

	// From here on a rule EXISTS. Every failure below means durian cannot
	// express it, not that the event is non-recurring — so the event is marked
	// opaque and the write paths leave its recurrence alone.

	// StrToROption strips the "RRULE:" prefix itself and parses UNTIL in UTC.
	opt, err := rrule.StrToROption(rruleLine)
	if err != nil {
		slog.Warn("Keeping unparseable RRULE opaque", "module", "GOOGLECAL", "id", id, "err", err)
		return nil, exDates, true
	}
	rec, err = calendar.ROptionToRecurrence(opt, start)
	if err != nil {
		slog.Warn("Keeping unmappable RRULE opaque", "module", "GOOGLECAL", "id", id, "err", err)
		return nil, exDates, true
	}
	return rec, exDates, false
}

// attendeeTypeFromGoogle maps the optional/resource flags onto the neutral
// attendee type vocabulary ("required", "optional", "resource").
func attendeeTypeFromGoogle(a googleAttendee) string {
	switch {
	case a.Resource:
		return "resource"
	case a.Optional:
		return "optional"
	default:
		return "required"
	}
}

// responseFromGoogle maps a Calendar API responseStatus onto the neutral
// attendee response vocabulary (the Graph enum the model and the iCal
// round-trip use).
func responseFromGoogle(s string) string {
	switch s {
	case "accepted":
		return "accepted"
	case "declined":
		return "declined"
	case "tentative":
		return "tentativelyAccepted"
	default: // "needsAction", "", unknown
		return "none"
	}
}

// MARK: - Events

// eventsPage is one page of an events list query.
type eventsPage struct {
	Items         []googleEvent `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
}

// FetchMasterEvents returns all master events of the calendar (single events
// and series masters carrying their recurrence definition) via the events list
// with singleEvents=false, each series master carrying its exceptions.
//
// An instance — an item with a recurringEventId — is not a separate item to
// the sync engine but a deviation of the series it points at, so instances are
// folded into their master: a cancelled one becomes an ExceptionDate, a
// modified one an Override. They are collected across ALL pages before being
// attached, because the API gives no ordering guarantee that a master precedes
// its own instances.
//
// showDeleted=true is requested explicitly rather than relying on the default:
// with singleEvents=false the API returns cancelled instances either way, but
// the parameter set of this query is the one an incremental syncToken will be
// bound to, so it has to state everything it wants up front. A cancelled
// MASTER stays skipped — the engine detects that deletion by the master's
// absence from the returned set.
func (c *Client) FetchMasterEvents(ctx context.Context, calendarID string) ([]calendar.Event, error) {
	base := c.baseURL + "/calendars/" + url.PathEscape(calendarID) + "/events"
	// Same parameter set as the incremental round (see delta.go): a sync token
	// is only replayable against the query that minted it, so the two paths
	// must not be allowed to drift apart.
	q := baseQuery()

	var events []calendar.Event
	// byID indexes into events by the Google master id the instances point at.
	// It stores positions, not pointers, so appending to events cannot leave a
	// stale reference behind.
	byID := make(map[string]int)
	var instances []googleEvent

	for {
		var page eventsPage
		if err := c.doJSON(ctx, base+"?"+q.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch master events for %s: %w", calendarID, err)
		}
		for _, g := range page.Items {
			if g.RecurringEventID != "" {
				instances = append(instances, g)
				continue
			}
			if g.Status == "cancelled" {
				slog.Debug("Skipping cancelled master", "module", "GOOGLECAL", "id", g.ID)
				continue
			}
			if ev, ok := c.eventFromGoogle(g); ok {
				byID[g.ID] = len(events)
				events = append(events, ev)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}

	c.attachInstances(events, byID, instances)

	slog.Debug("Fetched master events", "module", "GOOGLECAL",
		"calendar", calendarID, "events", len(events), "instances", len(instances))
	return events, nil
}

// attachInstances folds the collected series instances into their masters:
// cancelled ones extend ExceptionDates, modified ones become Overrides keyed
// by their originalStartTime. Both lists end up sorted so the .ics bytes — and
// with them the local file hash the sync engine diffs on — do not depend on
// the order the API happened to page the instances in.
//
// An instance whose master is not in the set is dropped with a warning: that
// happens when the master lies in a calendar the include filter excluded, or
// when it was deleted while this fetch was paging. Either way there is nothing
// to attach it to, and inventing a master from an instance would upload a
// series that shrinks to that single occurrence.
func (c *Client) attachInstances(events []calendar.Event, byID map[string]int, instances []googleEvent) {
	touched := make(map[int]bool)
	for _, g := range instances {
		idx, ok := byID[g.RecurringEventID]
		if !ok {
			slog.Warn("Dropping series instance without a master", "module", "GOOGLECAL",
				"id", g.ID, "master", g.RecurringEventID)
			continue
		}
		original, _, err := parseGoogleTime(g.OriginalStartTime)
		if err != nil {
			slog.Warn("Dropping series instance without a usable original start",
				"module", "GOOGLECAL", "id", g.ID, "err", err)
			continue
		}
		master := &events[idx]
		touched[idx] = true

		if g.Status == "cancelled" {
			master.ExceptionDates = append(master.ExceptionDates, original)
			continue
		}
		ev, ok := c.eventFromGoogle(g)
		if !ok {
			continue
		}
		ev.RecurrenceID = original
		// An override is a deviation, never a series of its own.
		ev.Recurrence = nil
		ev.ExceptionDates = nil
		master.Overrides = append(master.Overrides, ev)
	}

	for idx := range touched {
		master := &events[idx]
		sort.Slice(master.ExceptionDates, func(i, j int) bool {
			return master.ExceptionDates[i].Before(master.ExceptionDates[j])
		})
		sort.Slice(master.Overrides, func(i, j int) bool {
			return master.Overrides[i].RecurrenceID.Before(master.Overrides[j].RecurrenceID)
		})
	}
}

// FetchInstances returns the concrete event instances within [from, to) via
// the events list with singleEvents=true, which expands recurring series into
// occurrences (the one-way export path). Cancelled tombstones are skipped.
func (c *Client) FetchInstances(ctx context.Context, calendarID string, from, to time.Time) ([]calendar.Event, error) {
	base := c.baseURL + "/calendars/" + url.PathEscape(calendarID) + "/events"
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(pageSize))
	q.Set("singleEvents", "true")
	q.Set("timeMin", from.UTC().Format(time.RFC3339))
	q.Set("timeMax", to.UTC().Format(time.RFC3339))

	var events []calendar.Event
	for {
		var page eventsPage
		if err := c.doJSON(ctx, base+"?"+q.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("failed to fetch instances for %s: %w", calendarID, err)
		}
		for _, g := range page.Items {
			if g.Status == "cancelled" {
				continue
			}
			if ev, ok := c.eventFromGoogle(g); ok {
				events = append(events, ev)
			}
		}
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}

	slog.Debug("Fetched instances", "module", "GOOGLECAL",
		"calendar", calendarID, "events", len(events))
	return events, nil
}

// GetEvent returns one event by its Google event id — the settled server-side
// content the engine reads back after a write. Google event ids are scoped to
// their calendar, so calendarID addresses the right collection.
func (c *Client) GetEvent(ctx context.Context, calendarID, eventID string) (calendar.Event, error) {
	reqURL := c.baseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID)

	var g googleEvent
	if err := c.doJSON(ctx, reqURL, nil, &g); err != nil {
		return calendar.Event{}, fmt.Errorf("failed to get event %s: %w", eventID, err)
	}
	ev, ok := c.eventFromGoogle(g)
	if !ok {
		return calendar.Event{}, fmt.Errorf("failed to parse event %s", eventID)
	}
	slog.Debug("Fetched event", "module", "GOOGLECAL", "id", ev.ID, "etag", ev.ETag)
	return ev, nil
}
