package graphcalendar

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// v1.0 event-get example 3 documents this exact select/expand combination.
// Verified against the isolated demo calendar: /events returned masters only;
// GET master returned full exceptions (including originalStart) and cancellation
// OIDs whose series component matched the requested immutable master ID.
// https://learn.microsoft.com/en-us/graph/api/event-get?view=graph-rest-1.0
const expandedMasterSelect = masterEventSelect + ",occurrenceId,exceptionOccurrences,cancelledOccurrences"

// sameGraphResource confines continuation URLs to the original mailbox and
// resource. Never send credentials to an arbitrary server-supplied nextLink.
func sameGraphResource(next, resource string) bool {
	n, err := url.Parse(next)
	if err != nil {
		return false
	}
	r, err := url.Parse(resource)
	return err == nil && n.Scheme == r.Scheme && n.Host == r.Host &&
		n.EscapedPath() == r.EscapedPath() && n.User == nil && n.Fragment == "" && n.Opaque == ""
}

func (c *Client) getSeriesMaster(ctx context.Context, id string) (calendar.Event, error) {
	resource := c.baseURL + c.mailboxPath() + "/events/" + url.PathEscape(id)
	query := url.Values{"$select": {expandedMasterSelect}, "$expand": {"exceptionOccurrences"}}
	headers := map[string]string{"Prefer": preferMaster}
	var ge graphEvent
	if err := c.doJSON(ctx, resource+"?"+query.Encode(), headers, &ge); err != nil {
		return calendar.Event{}, fmt.Errorf("failed to read expanded master: %w", err)
	}
	if ge.ID != id || ge.Type != "seriesMaster" || ge.Recurrence == nil ||
		ge.ExceptionOccurrences == nil || ge.CancelledOccurrences == nil {
		return calendar.Event{}, fmt.Errorf("incomplete expanded series master")
	}
	if ge.CancelledNextLink != "" || ge.NextLink != "" {
		return calendar.Event{}, fmt.Errorf("unsupported expanded master pagination")
	}
	exceptions := ge.ExceptionOccurrences
	seen := map[string]bool{}
	for next := ge.ExceptionsNextLink; next != ""; {
		// Only follow the bound relationship, not another master or mailbox.
		// Unknown continuation shapes fail closed rather than truncate a series.
		if seen[next] || !sameGraphResource(next, resource+"/exceptionOccurrences") {
			return calendar.Event{}, fmt.Errorf("invalid exception occurrences pagination")
		}
		seen[next] = true
		var page eventPage
		if err := c.doJSON(ctx, next, headers, &page); err != nil {
			return calendar.Event{}, fmt.Errorf("failed to page series exceptions: %w", err)
		}
		if page.Value == nil {
			return calendar.Event{}, fmt.Errorf("incomplete exception occurrences page")
		}
		exceptions = append(exceptions, page.Value...)
		next = page.NextLink
	}
	ev, ok := eventFromGraph(ge)
	if !ok {
		return calendar.Event{}, fmt.Errorf("failed to parse expanded series master")
	}
	loc, err := time.LoadLocation(ev.Recurrence.Range.TimeZone)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("invalid series recurrence timezone: %w", err)
	}
	byID := map[string]graphEvent{}
	byOriginal := map[time.Time]bool{}
	for _, exception := range exceptions {
		if exception.ID == "" || exception.ID == id || exception.SeriesMasterID != id || exception.Type != "exception" {
			return calendar.Event{}, fmt.Errorf("invalid series exception identity")
		}
		if previous, exists := byID[exception.ID]; exists {
			if !reflect.DeepEqual(previous, exception) {
				return calendar.Event{}, fmt.Errorf("inconsistent duplicate series exception")
			}
			continue
		}
		byID[exception.ID] = exception
		original, err := exceptionOriginal(ev, exception, loc)
		if err != nil {
			return calendar.Event{}, err
		}
		if byOriginal[original] {
			return calendar.Event{}, fmt.Errorf("conflicting series exceptions for the same original start")
		}
		byOriginal[original] = true
		override, ok := eventFromGraph(exception)
		if !ok {
			return calendar.Event{}, fmt.Errorf("failed to parse series exception")
		}
		if exception.IsCancelled {
			ev.ExceptionDates = append(ev.ExceptionDates, original)
			continue
		}
		override.RecurrenceID = original
		// Graph assigns an occurrence-specific iCalUId; an ICS sibling must
		// instead share the master's UID or readers treat it as another event.
		override.ICalUID = ev.ICalUID
		override.Recurrence, override.ExceptionDates, override.Overrides = nil, nil, nil
		ev.Overrides = append(ev.Overrides, override)
	}
	cancelled := map[time.Time]bool{}
	for _, original := range ev.ExceptionDates {
		cancelled[original] = true
	}
	for _, oid := range ge.CancelledOccurrences {
		original, err := occurrenceOriginal(ev, oid, loc)
		if err != nil {
			return calendar.Event{}, err
		}
		if byOriginal[original] && !cancelled[original] {
			return calendar.Event{}, fmt.Errorf("series occurrence is both modified and cancelled")
		}
		if !cancelled[original] {
			cancelled[original] = true
			ev.ExceptionDates = append(ev.ExceptionDates, original)
		}
	}
	sort.Slice(ev.Overrides, func(i, j int) bool { return ev.Overrides[i].RecurrenceID.Before(ev.Overrides[j].RecurrenceID) })
	sort.Slice(ev.ExceptionDates, func(i, j int) bool { return ev.ExceptionDates[i].Before(ev.ExceptionDates[j]) })
	return ev, nil
}

func exceptionOriginal(master calendar.Event, ge graphEvent, loc *time.Location) (time.Time, error) {
	if ge.OriginalStart == "" {
		if ge.OccurrenceID == "" {
			return time.Time{}, fmt.Errorf("series exception has no original occurrence identity")
		}
		return occurrenceOriginal(master, ge.OccurrenceID, loc)
	}
	// originalStart is a DateTimeOffset, not a bare local start/end boundary.
	original, err := time.Parse(time.RFC3339Nano, ge.OriginalStart)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid series exception original start")
	}
	if ge.OccurrenceID != "" {
		day, err := occurrenceDate(master.ID, ge.OccurrenceID)
		if err != nil {
			return time.Time{}, err
		}
		// The OID supplies a date, while originalStart supplies the exact
		// instant (even in a fold). Validate the date without guessing a clock.
		if !day.Equal(civilDate(original.In(loc))) {
			return time.Time{}, fmt.Errorf("series exception occurrence identities disagree")
		}
	}
	if master.AllDay {
		original = civilDate(original.In(loc))
	} else {
		original = original.UTC()
	}
	return original, nil
}

// OID.<series ID>.<YYYY-MM-DD> is documented in event-get example 3.
// Treat the series component as opaque: compare it verbatim to the master ID;
// do NOT base64-decode an event ID or guess an occurrence's original date from
// its (possibly moved) start. Cancellation dates are in the recurrence zone.
func occurrenceDate(masterID, oid string) (time.Time, error) {
	date, ok := strings.CutPrefix(oid, "OID."+masterID+".")
	day, err := time.Parse("2006-01-02", date)
	if !ok || err != nil || day.Format("2006-01-02") != date {
		return time.Time{}, fmt.Errorf("unknown or foreign series occurrence ID")
	}
	return day, nil
}

func occurrenceOriginal(master calendar.Event, oid string, loc *time.Location) (time.Time, error) {
	day, err := occurrenceDate(master.ID, oid)
	if err != nil {
		return time.Time{}, err
	}
	if master.AllDay {
		return day, nil
	}
	date := day.Format("2006-01-02")
	clock := master.Start.In(loc)
	original := time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), loc)
	if original.Format("2006-01-02T15:04:05.999999999") != date+"T"+clock.Format("15:04:05.999999999") {
		return time.Time{}, fmt.Errorf("cancelled occurrence falls in a timezone gap")
	}
	// A date-only ID cannot disambiguate a repeated clock. Refuse the read
	// rather than exclude the wrong instant. This does not change write policy.
	_, offset := original.Zone()
	start, end := original.ZoneBounds()
	var boundaries []time.Time
	if !start.IsZero() {
		boundaries = append(boundaries, start.Add(-time.Second))
	}
	if !end.IsZero() {
		boundaries = append(boundaries, end)
	}
	for _, boundary := range boundaries {
		_, otherOffset := boundary.In(loc).Zone()
		other := original.Add(time.Duration(offset-otherOffset) * time.Second)
		if !other.Equal(original) && other.Format("2006-01-02T15:04:05.999999999") == original.Format("2006-01-02T15:04:05.999999999") {
			return time.Time{}, fmt.Errorf("cancelled occurrence has an ambiguous original clock")
		}
	}
	return original.UTC(), nil
}
