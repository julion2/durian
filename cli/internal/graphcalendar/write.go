// Graph event write operations — the provider side of the sync engine's
// upload direction (calendarsync.CalendarProvider): EventToGraphBody builds
// the Graph event JSON from a locally parsed Event (the inverse of
// eventFromGraph), and CreateEvent / UpdateEvent / DeleteEvent /
// RespondToEvent perform the POST / PATCH / DELETE calls through doRequest,
// so bearer auth and throttle retries apply exactly like on the read paths.
//
// iCalUId caveat: Graph assigns its own immutable iCalUId on POST and ignores
// any client-supplied UID, so a created event's UID never matches the UID of
// the local .ics that triggered the create. The sync engine handles this by
// rewriting the local file from the created event (see createFromLocal in
// calendarsync).
//
// Robustness rails baked in here:
//   - UpdateEvent and DeleteEvent send the engine-planned etag as an If-Match
//     header; a Graph 412 is wrapped as calendarsync.ErrPrecondition so the
//     engine skips that action instead of clobbering, and a 404/410 is
//     wrapped as calendarsync.ErrNotFound so the engine can fold "already
//     gone" into success.
//   - Attendees are only serialized when the engine explicitly asks for them
//     (role-gated in the engine: only for meetings the owner organizes) and
//     the organizer is NEVER uploaded — Graph derives it from the mailbox,
//     and durian must not invite on someone else's behalf.

package graphcalendar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

// graphWriteDateFormat is the zone-less dateTime layout Graph expects in
// start/end bodies (the timeZone field carries the zone).
const graphWriteDateFormat = "2006-01-02T15:04:05"

// classifyWriteError wraps a Graph write failure in the neutral sentinel the
// sync engine reacts to: 412 -> calendarsync.ErrPrecondition (If-Match guard
// tripped), 404/410 -> calendarsync.ErrNotFound (event already gone). Other
// errors pass through unchanged; the original error stays in the chain.
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

// EventToGraphBody builds the Graph event resource JSON for a create (POST)
// or update (PATCH) from a parsed local Event. Start/end are written as
// zone-less UTC dateTimes with timeZone "UTC"; all-day events use the date at
// 00:00:00 (Graph requires midnight boundaries together with isAllDay), and —
// because Graph rejects an all-day event spanning less than one full day —
// an all-day end date that is not after the start date is snapped to
// start + 1 day, so a bad-duration all-day event can never reach Graph. The
// recurrence key is always present — a Graph patternedRecurrence object for a
// series, or null so a PATCH clears the recurrence when the local file
// dropped its RRULE.
//
// includeAttendees adds the attendee list (see attendeesToGraph); the engine
// only sets it for meetings the account owner organizes, because a
// create/update carrying attendees makes Graph send invitation/update emails.
// organizer, responseStatus, isCancelled and the online-meeting fields are
// never emitted here (RSVPs go through RespondToEvent; Teams meetings are
// requested by CreateEvent via extra keys when the engine asks for one).
func EventToGraphBody(e calendar.Event, includeAttendees bool) map[string]any {
	var startDT, endDT string
	if e.AllDay {
		// Midnight date boundaries; Graph rejects an all-day event shorter
		// than 24h, so an end date not after the start date snaps to the
		// next day (a one-day event) instead of producing a Graph 400.
		startDay := calendar.DateOnly(e.Start)
		endDay := calendar.DateOnly(e.End)
		if !endDay.After(startDay) {
			endDay = startDay.AddDate(0, 0, 1)
		}
		startDT = startDay.Format(calendar.GraphDateFormat) + "T00:00:00"
		endDT = endDay.Format(calendar.GraphDateFormat) + "T00:00:00"
	} else {
		startDT = e.Start.UTC().Format(graphWriteDateFormat)
		endDT = e.End.UTC().Format(graphWriteDateFormat)
	}

	body := map[string]any{
		"subject":  e.Subject,
		"body":     map[string]string{"contentType": "text", "content": e.Description},
		"start":    map[string]string{"dateTime": startDT, "timeZone": "UTC"},
		"end":      map[string]string{"dateTime": endDT, "timeZone": "UTC"},
		"isAllDay": e.AllDay,
		"location": map[string]string{"displayName": e.Location},
	}
	switch {
	case e.OpaqueRecurrence:
		// The remote series has a rule durian could not read back. Sending
		// recurrence: null would DELETE that rule and collapse the series into
		// a single appointment — with a matching changeKey, so no precondition
		// rail would catch it. Omitting the key leaves the rule alone.
		slog.Warn("Omitting recurrence from upload: remote rule is not representable",
			"module", "GRAPHCAL", "id", e.ID, "uid", e.ICalUID)
	case e.Recurrence != nil:
		body["recurrence"] = e.Recurrence
	default:
		body["recurrence"] = nil
	}
	if includeAttendees {
		body["attendees"] = attendeesToGraph(e.Attendees)
	}
	return body
}

// attendeesToGraph renders the attendee list as Graph attendee resources:
// email address, display name and type only. Per-attendee responses are never
// uploaded — Graph owns the RSVP state of other attendees.
func attendeesToGraph(attendees []calendar.Attendee) []map[string]any {
	out := make([]map[string]any, 0, len(attendees))
	for _, a := range attendees {
		out = append(out, map[string]any{
			"type":         a.Type,
			"emailAddress": map[string]string{"name": a.Name, "address": a.Email},
		})
	}
	return out
}

// CreateEvent POSTs a new event into the calendar and returns the created
// event as Graph rendered it — including the server-assigned id, iCalUId and
// changeKey the sync status needs. The body is serialized per opts:
// opts.IncludeAttendees adds the attendee list (invitations go out),
// opts.RequestOnlineMeeting adds the Teams online-meeting keys, and
// opts.IdempotencyKey travels as the Graph transactionId so a retried POST
// cannot create a duplicate event (or a second invitation wave).
func (c *Client) CreateEvent(ctx context.Context, calendarID string, ev calendar.Event, opts calendarsync.CreateOptions) (calendar.Event, error) {
	body := EventToGraphBody(ev, opts.IncludeAttendees)
	if opts.RequestOnlineMeeting {
		body["isOnlineMeeting"] = true
		body["onlineMeetingProvider"] = "teamsForBusiness"
	}
	if opts.IdempotencyKey != "" {
		body["transactionId"] = opts.IdempotencyKey
	}
	reqURL := fmt.Sprintf("%s%s/calendars/%s/events", c.baseURL, c.mailboxPath(), url.PathEscape(calendarID))

	var ge graphEvent
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, map[string]string{"Prefer": preferMaster}, body, &ge); err != nil {
		return calendar.Event{}, fmt.Errorf("failed to create event in calendar %s: %w", calendarID, classifyWriteError(err))
	}
	created, ok := eventFromGraph(ge)
	if !ok {
		return calendar.Event{}, fmt.Errorf("failed to parse created event %s (calendar %s)", ge.ID, calendarID)
	}
	slog.Debug("Created remote event", "module", "GRAPHCAL",
		"calendar", calendarID, "id", created.ID, "uid", created.ICalUID)
	return created, nil
}

// UpdateEvent PATCHes an existing event per spec. spec.ETag, when non-empty,
// is sent as an If-Match header so a remote edit that happened after the last
// read fails with calendarsync.ErrPrecondition instead of being overwritten.
// With spec.AttendeesOnly the PATCH body carries just the attendee list, so
// Graph notifies only the added/removed attendees; otherwise the full body is
// sent (attendees included only when spec.IncludeAttendees). calendarID is
// unused: Graph event ids are mailbox-global.
func (c *Client) UpdateEvent(ctx context.Context, calendarID, eventID string, spec calendarsync.UpdateSpec) error {
	_ = calendarID
	var body map[string]any
	if spec.AttendeesOnly {
		body = map[string]any{"attendees": attendeesToGraph(spec.Event.Attendees)}
	} else {
		body = EventToGraphBody(spec.Event, spec.IncludeAttendees)
	}

	reqURL := c.baseURL + c.mailboxPath() + "/events/" + url.PathEscape(eventID)
	headers := map[string]string{"Prefer": preferMaster}
	if spec.ETag != "" {
		headers["If-Match"] = spec.ETag
	}

	var resp struct {
		ChangeKey string `json:"changeKey"`
	}
	if err := c.doJSONBody(ctx, http.MethodPatch, reqURL, headers, body, &resp); err != nil {
		return fmt.Errorf("failed to update event %s: %w", eventID, classifyWriteError(err))
	}
	slog.Debug("Updated remote event", "module", "GRAPHCAL",
		"id", eventID, "changeKey", resp.ChangeKey)
	return nil
}

// DeleteEvent DELETEs an event. A 404/410 surfaces as
// calendarsync.ErrNotFound — the engine folds it into success, since the goal
// (event absent remotely) is reached either way. etag, when non-empty, is
// sent as an If-Match header (see UpdateEvent); a 412 surfaces as
// calendarsync.ErrPrecondition, which the engine skips and re-plans.
// calendarID is unused: Graph event ids are mailbox-global. notify is unused
// too: Graph decides for itself whether to mail the attendees — deleting a
// meeting the owner organizes always cancels it for everyone, and there is no
// wire flag to opt out. The engine still states its intent so the Google
// provider, which does need to be told, gets it.
func (c *Client) DeleteEvent(ctx context.Context, calendarID, eventID, etag string, notify bool) error {
	_, _ = calendarID, notify
	reqURL := c.baseURL + c.mailboxPath() + "/events/" + url.PathEscape(eventID)
	headers := map[string]string{"Prefer": preferMaster}
	if etag != "" {
		headers["If-Match"] = etag
	}

	if err := c.doRequest(ctx, http.MethodDelete, reqURL, headers, nil, nil); err != nil {
		return fmt.Errorf("failed to delete event %s: %w", eventID, classifyWriteError(err))
	}
	slog.Debug("Deleted remote event", "module", "GRAPHCAL", "id", eventID)
	return nil
}

// RespondToEvent posts the owner's RSVP for a meeting: POST
// /me/events/{id}/{accept|decline|tentativelyAccept}. With sendResponse the
// organizer receives a response email; comment, when non-empty, is included
// in it. Graph answers 202 with an empty body. Responding with the same value
// again is harmless (Graph records the same state), so a re-planned RSVP
// after a partial failure cannot corrupt anything. resp must be Accepted,
// Declined or Tentative — None and Organizer have no Graph action. A 404/410
// surfaces as calendarsync.ErrNotFound. calendarID is unused: Graph event ids
// are mailbox-global.
func (c *Client) RespondToEvent(ctx context.Context, calendarID, eventID string, resp calendar.OwnerResp, sendResponse bool, comment string) error {
	_ = calendarID
	var verb string
	switch resp {
	case calendar.OwnerRespAccepted:
		verb = "accept"
	case calendar.OwnerRespDeclined:
		verb = "decline"
	case calendar.OwnerRespTentative:
		verb = "tentativelyAccept"
	default:
		return fmt.Errorf("cannot send RSVP for owner response state %q", resp)
	}
	reqURL := fmt.Sprintf("%s%s/events/%s/%s", c.baseURL, c.mailboxPath(), url.PathEscape(eventID), verb)

	body := map[string]any{"comment": comment, "sendResponse": sendResponse}
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, nil, body, nil); err != nil {
		return fmt.Errorf("failed to %s event %s: %w", verb, eventID, classifyWriteError(err))
	}
	slog.Info("Sent RSVP", "module", "GRAPHCAL", "id", eventID, "verb", verb, "sendResponse", sendResponse)
	return nil
}
