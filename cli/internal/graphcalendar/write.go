// Graph event write operations for the two-way sync upload direction:
// EventToGraphBody builds the Graph event JSON from a locally parsed Event
// (the inverse of eventFromGraph), and CreateEvent / UpdateEvent /
// DeleteEvent / RespondToEvent perform the POST / PATCH / DELETE calls
// through doRequest, so bearer auth and throttle retries apply exactly like
// on the read paths.
//
// iCalUId caveat: Graph assigns its own immutable iCalUId on POST and ignores
// any client-supplied UID, so a created event's UID never matches the UID of
// the local .ics that triggered the create. The sync engine handles this by
// rewriting the local file from the created event (see applyUploadCreate in
// twosync.go).
//
// Robustness rails baked in here:
//   - UpdateEvent and DeleteEvent send the last-read changeKey as an If-Match
//     header, so a concurrently edited event yields a 412 instead of being
//     clobbered (the sync engine skips that action and re-plans next run).
//   - Attendees are only included when the caller explicitly asks for them
//     (role-gated in twosync.go: only for meetings the owner organizes) and
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
	"time"
)

// graphWriteDateFormat is the zone-less dateTime layout Graph expects in
// start/end bodies (the timeZone field carries the zone).
const graphWriteDateFormat = "2006-01-02T15:04:05"

// EventToGraphBody builds the Graph event resource JSON for a create (POST)
// or update (PATCH) from a parsed local Event. Start/end are written as
// zone-less UTC dateTimes with timeZone "UTC"; all-day events use the date at
// 00:00:00 (Graph requires midnight boundaries together with isAllDay). The
// recurrence key is always present — a Graph patternedRecurrence object for a
// series, or null so a PATCH clears the recurrence when the local file
// dropped its RRULE.
//
// includeAttendees adds the attendee list (see attendeesToGraph); callers
// must only set it for meetings the account owner organizes, because a
// create/update carrying attendees makes Graph send invitation/update emails.
// organizer, responseStatus, isCancelled and the online-meeting fields are
// never emitted here (RSVPs go through RespondToEvent; Teams meetings are
// requested by the create path via extra keys, see createFromLocal).
func EventToGraphBody(e Event, includeAttendees bool) map[string]any {
	formatDT := func(t time.Time) string {
		t = t.UTC()
		if e.AllDay {
			return t.Format(graphDateFormat) + "T00:00:00"
		}
		return t.Format(graphWriteDateFormat)
	}

	body := map[string]any{
		"subject":  e.Subject,
		"body":     map[string]string{"contentType": "text", "content": e.Description},
		"start":    map[string]string{"dateTime": formatDT(e.Start), "timeZone": "UTC"},
		"end":      map[string]string{"dateTime": formatDT(e.End), "timeZone": "UTC"},
		"isAllDay": e.AllDay,
		"location": map[string]string{"displayName": e.Location},
	}
	if e.Recurrence != nil {
		body["recurrence"] = e.Recurrence
	} else {
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
func attendeesToGraph(attendees []Attendee) []map[string]any {
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
// changeKey the sync status needs. Callers should include a client-generated
// transactionId in the body so a retried POST cannot create a duplicate event
// (and a second invitation wave); see createFromLocal.
func (c *Client) CreateEvent(ctx context.Context, calendarID string, body any) (Event, error) {
	reqURL := fmt.Sprintf("%s/me/calendars/%s/events", c.baseURL, url.PathEscape(calendarID))

	var ge graphEvent
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, map[string]string{"Prefer": preferMaster}, body, &ge); err != nil {
		return Event{}, fmt.Errorf("failed to create event in calendar %s: %w", calendarID, err)
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		return Event{}, fmt.Errorf("failed to parse created event %s (calendar %s)", ge.ID, calendarID)
	}
	slog.Debug("Created remote event", "module", "GRAPHCAL",
		"calendar", calendarID, "id", ev.ID, "uid", ev.ICalUID)
	return ev, nil
}

// UpdateEvent PATCHes an existing event and returns the new changeKey (the
// remote etag after the update). etag, when non-empty, is sent as an If-Match
// header so a remote edit that happened after the last read fails with 412
// instead of being overwritten.
func (c *Client) UpdateEvent(ctx context.Context, eventID, etag string, body any) (string, error) {
	reqURL := c.baseURL + "/me/events/" + url.PathEscape(eventID)
	headers := map[string]string{"Prefer": preferMaster}
	if etag != "" {
		headers["If-Match"] = etag
	}

	var resp struct {
		ChangeKey string `json:"changeKey"`
	}
	if err := c.doJSONBody(ctx, http.MethodPatch, reqURL, headers, body, &resp); err != nil {
		return "", fmt.Errorf("failed to update event %s: %w", eventID, err)
	}
	slog.Debug("Updated remote event", "module", "GRAPHCAL",
		"id", eventID, "changeKey", resp.ChangeKey)
	return resp.ChangeKey, nil
}

// DeleteEvent DELETEs an event. A 404 means the event is already gone and is
// treated as success — the goal (event absent remotely) is reached either
// way. etag, when non-empty, is sent as an If-Match header (see UpdateEvent);
// a 412 surfaces to the caller, which skips the action and re-plans.
func (c *Client) DeleteEvent(ctx context.Context, eventID, etag string) error {
	reqURL := c.baseURL + "/me/events/" + url.PathEscape(eventID)
	headers := map[string]string{"Prefer": preferMaster}
	if etag != "" {
		headers["If-Match"] = etag
	}

	if err := c.doRequest(ctx, http.MethodDelete, reqURL, headers, nil, nil); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			slog.Info("Remote event already gone on delete", "module", "GRAPHCAL", "id", eventID)
			return nil
		}
		return fmt.Errorf("failed to delete event %s: %w", eventID, err)
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
// Declined or Tentative — None and Organizer have no Graph action.
func (c *Client) RespondToEvent(ctx context.Context, eventID string, resp OwnerResp, sendResponse bool, comment string) error {
	var verb string
	switch resp {
	case OwnerRespAccepted:
		verb = "accept"
	case OwnerRespDeclined:
		verb = "decline"
	case OwnerRespTentative:
		verb = "tentativelyAccept"
	default:
		return fmt.Errorf("cannot send RSVP for owner response state %q", resp)
	}
	reqURL := fmt.Sprintf("%s/me/events/%s/%s", c.baseURL, url.PathEscape(eventID), verb)

	body := map[string]any{"comment": comment, "sendResponse": sendResponse}
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, nil, body, nil); err != nil {
		return fmt.Errorf("failed to %s event %s: %w", verb, eventID, err)
	}
	slog.Info("Sent RSVP", "module", "GRAPHCAL", "id", eventID, "verb", verb, "sendResponse", sendResponse)
	return nil
}
