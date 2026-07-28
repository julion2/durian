// Google Calendar event write operations — the provider side of the sync
// engine's upload direction (calendarsync.CalendarProvider): eventToGoogle
// builds the Calendar API event JSON from a locally parsed Event (the inverse
// of eventFromGoogle), and CreateEvent / UpdateEvent / DeleteEvent /
// RespondToEvent perform the POST / PATCH / DELETE calls through doRequest,
// so bearer auth and throttle retries apply exactly like on the read paths.
//
// Robustness rails baked in here (mirroring the Graph provider):
//   - UpdateEvent and DeleteEvent send the engine-planned etag as an If-Match
//     header; a 412 is wrapped as calendarsync.ErrPrecondition so the engine
//     skips that action instead of clobbering, and a 404/410 is wrapped as
//     calendarsync.ErrNotFound so the engine can fold "already gone" into
//     success.
//   - CreateEvent encodes the engine's idempotency key as the client-chosen
//     Google event id; a retried POST then answers 409, which is folded into
//     "fetch and return the existing event" — no duplicate, no second
//     invitation wave (rail R1).
//   - Attendees are only serialized when the engine explicitly asks for them
//     (role-gated in the engine: only for meetings the owner organizes) and
//     the organizer is NEVER uploaded — Google derives it from the calendar,
//     and durian must not invite on someone else's behalf.

package googlecalendar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

// classifyWriteError wraps a Calendar API write failure in the neutral
// sentinel the sync engine reacts to: 412 -> calendarsync.ErrPrecondition
// (If-Match guard tripped), 404/410 -> calendarsync.ErrNotFound (event
// already gone; Google answers 410 Gone for deleted events). Other errors
// pass through unchanged; the original error stays in the chain.
func classifyWriteError(err error) error {
	var se *statusError
	if !errors.As(err, &se) {
		return err
	}
	switch se.status {
	case http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %w", calendarsync.ErrPrecondition, err)
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: %w", calendarsync.ErrNotFound, err)
	}
	return err
}

// MARK: - Reverse mapping (neutral Event -> Calendar API resource)

// eventToGoogle builds the Calendar API event resource JSON for a create
// (POST) or update (PATCH) from a parsed local Event — the inverse of
// eventFromGoogle. All-day events use start/end date boundaries (exclusive
// end, as the model stores); because Google rejects an all-day event whose
// end is not after its start, such an end date is snapped to start + 1 day.
// Timed events are written as RFC3339 UTC dateTimes with timeZone "UTC". The
// recurrence key is always present — the RRULE line of a series, or an empty
// list so a PATCH clears the recurrence when the local file dropped its
// RRULE.
//
// includeAttendees adds the attendee list (see attendeesToGoogle); the engine
// only sets it for meetings the account owner organizes, because a
// create/update carrying attendees makes Google send invitation/update
// emails. organizer, id, etag, status and the conference fields are never
// emitted here (RSVPs go through RespondToEvent; Meet conferences are
// requested by CreateEvent via extra keys when the engine asks for one).
func eventToGoogle(ev calendar.Event, includeAttendees bool) map[string]any {
	var start, end map[string]string
	if ev.AllDay {
		startDay := calendar.DateOnly(ev.Start)
		endDay := calendar.DateOnly(ev.End)
		if !endDay.After(startDay) {
			endDay = startDay.AddDate(0, 0, 1)
		}
		start = map[string]string{"date": startDay.Format(calendar.GraphDateFormat)}
		end = map[string]string{"date": endDay.Format(calendar.GraphDateFormat)}
	} else {
		start = map[string]string{
			"dateTime": ev.Start.UTC().Format(time.RFC3339),
			"timeZone": "UTC",
		}
		end = map[string]string{
			"dateTime": ev.End.UTC().Format(time.RFC3339),
			"timeZone": "UTC",
		}
	}

	body := map[string]any{
		"summary":     ev.Subject,
		"location":    ev.Location,
		"description": ev.Description,
		"start":       start,
		"end":         end,
		"recurrence":  recurrenceToGoogle(ev.Recurrence, ev.ID),
	}
	if includeAttendees {
		body["attendees"] = attendeesToGoogle(ev.Attendees)
	}
	return body
}

// recurrenceToGoogle renders the neutral Recurrence as the Calendar API
// recurrence line list: a single RRULE line built via the shared rrule-go
// bridge (RRULE only — Google forbids a DTSTART line, the event start carries
// it). A nil recurrence yields an empty list, which clears the series on
// PATCH; a recurrence outside the supported mapping is dropped the same way,
// with a warning (mirroring the read path, which would have parsed it to nil
// too).
func recurrenceToGoogle(rec *calendar.Recurrence, id string) []string {
	if rec == nil {
		return []string{}
	}
	opt, err := calendar.RecurrenceToROption(*rec)
	if err != nil {
		slog.Warn("Dropping unmappable recurrence from upload", "module", "GOOGLECAL",
			"id", id, "pattern", rec.Pattern.Type, "err", err)
		return []string{}
	}
	return []string{"RRULE:" + opt.RRuleString()}
}

// attendeesToGoogle renders the attendee list as Calendar API attendee
// resources. Each attendee's known responseStatus is echoed back (inverse of
// responseFromGoogle) because Google REPLACES the attendee array on write —
// omitting the status would reset everyone to needsAction and wipe recorded
// RSVPs. The organizer is not special-cased: Google keeps its own organizer
// field, which is never uploaded.
func attendeesToGoogle(attendees []calendar.Attendee) []map[string]any {
	out := make([]map[string]any, 0, len(attendees))
	for _, a := range attendees {
		entry := map[string]any{
			"email":          a.Email,
			"responseStatus": responseToGoogle(a.Response),
		}
		if a.Name != "" {
			entry["displayName"] = a.Name
		}
		switch a.Type {
		case "optional":
			entry["optional"] = true
		case "resource":
			entry["resource"] = true
		}
		out = append(out, entry)
	}
	return out
}

// responseToGoogle maps the neutral attendee response vocabulary back onto
// the Calendar API responseStatus enum — the inverse of responseFromGoogle.
func responseToGoogle(s string) string {
	switch s {
	case "accepted", "declined":
		return s
	case "tentativelyAccepted":
		return "tentative"
	default: // "none", "", unknown
		return "needsAction"
	}
}

// sendUpdatesParam maps the engine's attendee gate onto the sendUpdates query
// parameter: attendee-carrying writes notify everyone, attendee-less writes
// notify no one.
func sendUpdatesParam(includeAttendees bool) string {
	if includeAttendees {
		return "all"
	}
	return "none"
}

// MARK: - Create idempotency

// googleEventIDCharset is the charset Google accepts for client-chosen event
// ids: base32hex lowercase (RFC 2938 section 3.1.2).
var googleEventIDCharset = regexp.MustCompile(`^[0-9a-v]+$`)

// validGoogleEventID reports whether id satisfies Google's constraints for
// client-chosen event ids: base32hex lowercase charset, 5-1024 characters.
func validGoogleEventID(id string) bool {
	return len(id) >= 5 && len(id) <= 1024 && googleEventIDCharset.MatchString(id)
}

// createEventID deterministically transforms the engine's idempotency key
// into a valid Google event id: a UUID (the engine's key shape) lowercased
// with the dashes stripped is 32 lowercase hex characters — already valid
// base32hex — and any key outside that charset falls back to its SHA-256 hex
// digest (64 hex characters, also valid). Both branches are deterministic,
// so a retried create derives the same id and Google's 409 dedupes it.
func createEventID(key string) string {
	id := strings.ToLower(strings.ReplaceAll(key, "-", ""))
	if validGoogleEventID(id) {
		return id
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// conferenceRequestID returns the conferenceData.createRequest.requestId: the
// idempotency-derived event id when one exists (so a retried create carries
// the same request id and Google dedupes the conference request too), or a
// random id when the engine sent no idempotency key.
func conferenceRequestID(eventID string) string {
	if eventID != "" {
		return eventID
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// MARK: - Write operations

// CreateEvent POSTs a new event into the calendar and returns the created
// event as Google rendered it — including the server-assigned iCalUID and
// etag the sync status needs. The body is serialized per opts:
// opts.IncludeAttendees adds the attendee list (invitations go out, and
// sendUpdates=all makes Google deliver them), opts.RequestOnlineMeeting adds
// a Meet conference create request (with conferenceDataVersion=1), and
// opts.IdempotencyKey becomes the client-chosen event id so a retried POST
// answers 409 — folded into fetching the already-created event instead of
// creating a duplicate (or sending a second invitation wave).
func (c *Client) CreateEvent(ctx context.Context, calendarID string, ev calendar.Event, opts calendarsync.CreateOptions) (calendar.Event, error) {
	body := eventToGoogle(ev, opts.IncludeAttendees)
	eventID := ""
	if opts.IdempotencyKey != "" {
		eventID = createEventID(opts.IdempotencyKey)
		body["id"] = eventID
	}

	q := url.Values{}
	q.Set("sendUpdates", sendUpdatesParam(opts.IncludeAttendees))
	if opts.RequestOnlineMeeting {
		q.Set("conferenceDataVersion", "1")
		body["conferenceData"] = map[string]any{
			"createRequest": map[string]any{
				"requestId":             conferenceRequestID(eventID),
				"conferenceSolutionKey": map[string]string{"type": "hangoutsMeet"},
			},
		}
	}
	reqURL := c.baseURL + "/calendars/" + url.PathEscape(calendarID) + "/events?" + q.Encode()

	var g googleEvent
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, nil, body, &g); err != nil {
		var se *statusError
		if eventID != "" && errors.As(err, &se) && se.status == http.StatusConflict {
			slog.Warn("Create answered 409 for idempotent id, fetching existing event",
				"module", "GOOGLECAL", "calendar", calendarID, "id", eventID)
			return c.GetEvent(ctx, calendarID, eventID)
		}
		return calendar.Event{}, fmt.Errorf("failed to create event in calendar %s: %w",
			calendarID, classifyWriteError(err))
	}
	created, ok := c.eventFromGoogle(g)
	if !ok {
		return calendar.Event{}, fmt.Errorf("failed to parse created event %s (calendar %s)", g.ID, calendarID)
	}
	slog.Debug("Created remote event", "module", "GOOGLECAL",
		"calendar", calendarID, "id", created.ID, "uid", created.ICalUID)
	return created, nil
}

// UpdateEvent PATCHes an existing event per spec. spec.ETag, when non-empty,
// is sent as an If-Match header so a remote edit that happened after the last
// read fails with calendarsync.ErrPrecondition instead of being overwritten.
// With spec.AttendeesOnly the PATCH body carries just the attendee list, so
// Google notifies only the added/removed attendees; otherwise the full body
// is sent (attendees included only when spec.IncludeAttendees).
func (c *Client) UpdateEvent(ctx context.Context, calendarID, eventID string, spec calendarsync.UpdateSpec) error {
	var body map[string]any
	if spec.AttendeesOnly {
		body = map[string]any{"attendees": attendeesToGoogle(spec.Event.Attendees)}
	} else {
		body = eventToGoogle(spec.Event, spec.IncludeAttendees)
	}

	q := url.Values{}
	q.Set("sendUpdates", sendUpdatesParam(spec.IncludeAttendees))
	reqURL := c.baseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID) + "?" + q.Encode()
	var headers map[string]string
	if spec.ETag != "" {
		headers = map[string]string{"If-Match": spec.ETag}
	}

	var g googleEvent
	if err := c.doJSONBody(ctx, http.MethodPatch, reqURL, headers, body, &g); err != nil {
		return fmt.Errorf("failed to update event %s: %w", eventID, classifyWriteError(err))
	}
	slog.Debug("Updated remote event", "module", "GOOGLECAL", "id", eventID, "etag", g.ETag)
	return nil
}

// DeleteEvent DELETEs an event. A 404/410 surfaces as
// calendarsync.ErrNotFound — the engine folds it into success, since the goal
// (event absent remotely) is reached either way; Google answers 410 Gone for
// an already-deleted event. etag, when non-empty, is sent as an If-Match
// header (see UpdateEvent); a 412 surfaces as calendarsync.ErrPrecondition,
// which the engine skips and re-plans.
func (c *Client) DeleteEvent(ctx context.Context, calendarID, eventID, etag string) error {
	reqURL := c.baseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID)
	var headers map[string]string
	if etag != "" {
		headers = map[string]string{"If-Match": etag}
	}

	if err := c.doRequest(ctx, http.MethodDelete, reqURL, headers, nil, nil); err != nil {
		return fmt.Errorf("failed to delete event %s: %w", eventID, classifyWriteError(err))
	}
	slog.Debug("Deleted remote event", "module", "GOOGLECAL", "id", eventID)
	return nil
}

// RespondToEvent records the owner's RSVP. Google has no dedicated RSVP
// endpoint: the owner's responseStatus lives in the attendees array, and a
// PATCH REPLACES that array — so the event is read first and the full
// attendee list echoed back with only the owner's entry changed (status, plus
// the comment when non-empty). With sendResponse the PATCH goes out with
// sendUpdates=all so the organizer is notified; otherwise sendUpdates=none.
// Responding with the same value again is harmless (same state recorded), so
// a re-planned RSVP after a partial failure cannot corrupt anything. resp
// must be Accepted, Declined or Tentative — None and Organizer have no
// Google action. When the owner is not in the attendee list the RSVP is a
// logged no-op (there is no entry to update). A 404/410 surfaces as
// calendarsync.ErrNotFound.
func (c *Client) RespondToEvent(ctx context.Context, calendarID, eventID string, resp calendar.OwnerResp, sendResponse bool, comment string) error {
	var status string
	switch resp {
	case calendar.OwnerRespAccepted:
		status = "accepted"
	case calendar.OwnerRespDeclined:
		status = "declined"
	case calendar.OwnerRespTentative:
		status = "tentative"
	default:
		return fmt.Errorf("cannot send RSVP for owner response state %q", resp)
	}

	eventURL := c.baseURL + "/calendars/" + url.PathEscape(calendarID) +
		"/events/" + url.PathEscape(eventID)
	var g googleEvent
	if err := c.doJSON(ctx, eventURL, nil, &g); err != nil {
		return fmt.Errorf("failed to read event %s for RSVP: %w", eventID, classifyWriteError(err))
	}

	ownerFound := false
	attendees := make([]map[string]any, 0, len(g.Attendees))
	for _, a := range g.Attendees {
		entry := map[string]any{
			"email":          a.Email,
			"responseStatus": a.ResponseStatus,
		}
		if a.DisplayName != "" {
			entry["displayName"] = a.DisplayName
		}
		if a.Optional {
			entry["optional"] = true
		}
		if a.Resource {
			entry["resource"] = true
		}
		if a.Self || (c.owner != "" && strings.EqualFold(a.Email, c.owner)) {
			ownerFound = true
			entry["responseStatus"] = status
			if comment != "" {
				entry["comment"] = comment
			}
		}
		attendees = append(attendees, entry)
	}
	if !ownerFound {
		slog.Warn("Owner not in attendee list, skipping RSVP", "module", "GOOGLECAL", "id", eventID)
		return nil
	}

	q := url.Values{}
	if sendResponse {
		q.Set("sendUpdates", "all")
	} else {
		q.Set("sendUpdates", "none")
	}
	body := map[string]any{"attendees": attendees}
	if err := c.doJSONBody(ctx, http.MethodPatch, eventURL+"?"+q.Encode(), nil, body, nil); err != nil {
		return fmt.Errorf("failed to RSVP %s to event %s: %w", status, eventID, classifyWriteError(err))
	}
	slog.Info("Sent RSVP", "module", "GOOGLECAL",
		"id", eventID, "response", status, "sendResponse", sendResponse)
	return nil
}
