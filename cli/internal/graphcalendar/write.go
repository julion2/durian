// Graph event write operations for the two-way sync upload direction:
// EventToGraphBody builds the Graph event JSON from a locally parsed Event
// (the inverse of eventFromGraph), and CreateEvent / UpdateEvent /
// DeleteEvent perform the POST / PATCH / DELETE calls through doRequest, so
// bearer auth and throttle retries apply exactly like on the read paths.
//
// iCalUId caveat: Graph assigns its own immutable iCalUId on POST and ignores
// any client-supplied UID, so a created event's UID never matches the UID of
// the local .ics that triggered the create. The sync engine handles this by
// rewriting the local file from the created event (see applyUploadCreate in
// twosync.go).

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
func EventToGraphBody(e Event) map[string]any {
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
	return body
}

// CreateEvent POSTs a new event into the calendar and returns the created
// event as Graph rendered it — including the server-assigned id, iCalUId and
// changeKey the sync status needs.
func (c *Client) CreateEvent(ctx context.Context, calendarID string, body any) (Event, error) {
	reqURL := fmt.Sprintf("%s/me/calendars/%s/events", c.baseURL, url.PathEscape(calendarID))

	var ge graphEvent
	if err := c.doJSONBody(ctx, http.MethodPost, reqURL, map[string]string{"Prefer": preferUTC}, body, &ge); err != nil {
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
// remote etag after the update).
func (c *Client) UpdateEvent(ctx context.Context, eventID string, body any) (string, error) {
	reqURL := c.baseURL + "/me/events/" + url.PathEscape(eventID)

	var resp struct {
		ChangeKey string `json:"changeKey"`
	}
	if err := c.doJSONBody(ctx, http.MethodPatch, reqURL, map[string]string{"Prefer": preferUTC}, body, &resp); err != nil {
		return "", fmt.Errorf("failed to update event %s: %w", eventID, err)
	}
	slog.Debug("Updated remote event", "module", "GRAPHCAL",
		"id", eventID, "changeKey", resp.ChangeKey)
	return resp.ChangeKey, nil
}

// DeleteEvent DELETEs an event. A 404 means the event is already gone and is
// treated as success — the goal (event absent remotely) is reached either way.
func (c *Client) DeleteEvent(ctx context.Context, eventID string) error {
	reqURL := c.baseURL + "/me/events/" + url.PathEscape(eventID)

	if err := c.doRequest(ctx, http.MethodDelete, reqURL, nil, nil, nil); err != nil {
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
