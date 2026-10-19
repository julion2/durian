package googlecalendar

import (
	"fmt"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// Fold metadata v1 is a pair: both original, effective IANA endpoint zones
// accompany a single timed appointment encoded with UTC endpoints. It never
// changes the instants parsed from Google's explicit RFC3339 offsets. A missing
// pair, invalid zone, or use on a date/series must abort the read round, not
// silently drop the event from a snapshot or guess a zone.
const (
	foldStartZoneKey = "durian.fold.v1.startZone"
	foldEndZoneKey   = "durian.fold.v1.endZone"
)

func googleZone(zone string) (string, error) {
	if zone == "" {
		zone = "UTC"
	}
	if zone == "Local" {
		return "", fmt.Errorf("unsupported Google calendar timezone %q", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("unsupported Google calendar timezone %q: %w", zone, err)
	}
	return zone, nil
}

// Abort a snapshot/change-feed round for an unsupported zone instead of
// silently dropping an existing event (which sync could interpret as deleted).
func validateGoogleZones(g googleEvent) error {
	for _, dt := range []googleDateTime{g.Start, g.End, g.OriginalStartTime} {
		if dt.Date != "" {
			continue // Date-only events have no timezone.
		}
		if _, err := googleZone(dt.TimeZone); err != nil {
			return err
		}
		if dt.DateTime != "" && dt.TimeZone == "" {
			if _, err := time.Parse(time.RFC3339, dt.DateTime); err != nil {
				return fmt.Errorf("Google dateTime %q requires an offset or IANA timeZone", dt.DateTime)
			}
		}
	}
	return validateFoldZones(g)
}

func validateFoldZones(g googleEvent) error {
	private := g.ExtendedProperties.Private
	startZone, hasStart := private[foldStartZoneKey]
	endZone, hasEnd := private[foldEndZoneKey]
	if !hasStart && !hasEnd {
		return nil
	}
	if !hasStart || !hasEnd || startZone == "" || endZone == "" {
		return fmt.Errorf("invalid Google fold metadata: both original endpoint zones are required")
	}
	for _, zone := range []string{startZone, endZone} {
		if _, err := googleZone(zone); err != nil {
			return fmt.Errorf("invalid Google fold metadata: %w", err)
		}
	}
	// Google tombstones may retain properties but omit both endpoints.
	if g.Status == "cancelled" {
		return nil
	}
	if len(g.Recurrence) != 0 || g.RecurringEventID != "" {
		return fmt.Errorf("invalid Google fold metadata: recurring events are not supported")
	}
	for _, dt := range []googleDateTime{g.Start, g.End} {
		if dt.Date != "" {
			return fmt.Errorf("invalid Google fold metadata: date-only endpoints are not supported")
		}
		if _, err := time.Parse(time.RFC3339, dt.DateTime); err != nil {
			return fmt.Errorf("invalid Google fold metadata: endpoint requires an explicit RFC3339 offset: %w", err)
		}
	}
	return nil
}

// googleWriteZones resolves the model's UTC default and end-zone inheritance.
// Call only after validateWriteEvent has checked the zone identifiers.
func googleWriteZones(ev calendar.Event) (string, string) {
	start, _ := googleZone(ev.TimeZone)
	end := ev.EndTimeZone
	if end == "" {
		end = start
	}
	return start, end
}

// googleFoldTime detects BOTH occurrences of a repeated civil time, using
// transition offsets rather than assuming every autumn rollback is one hour.
func googleFoldTime(t time.Time, zone string) bool {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false // Validation at the public write boundary reports this.
	}
	local := t.In(loc)
	_, offset := local.Zone()
	start, end := local.ZoneBounds()
	if !start.IsZero() {
		_, previous := start.Add(-time.Nanosecond).In(loc).Zone()
		if rollback := previous - offset; rollback > 0 && t.Before(start.Add(time.Duration(rollback)*time.Second)) {
			return true // Second occurrence, after the rollback.
		}
	}
	if !end.IsZero() {
		_, next := end.In(loc).Zone()
		if rollback := offset - next; rollback > 0 && !t.Before(end.Add(-time.Duration(rollback)*time.Second)) {
			return true // First occurrence, before the rollback.
		}
	}
	return false
}

func googleFoldEvent(ev calendar.Event) bool {
	if ev.AllDay {
		return false
	}
	startZone, endZone := googleWriteZones(ev)
	return googleFoldTime(ev.Start, startZone) || googleFoldTime(ev.End, endZone)
}

func validateWriteEvent(ev calendar.Event) error {
	if ev.Floating {
		return fmt.Errorf("floating calendar events cannot be represented by Google Calendar")
	}
	for _, zone := range []string{ev.TimeZone, ev.EndTimeZone} {
		if _, err := googleZone(zone); err != nil {
			return err
		}
	}
	if ev.Recurrence != nil {
		if _, err := googleZone(ev.Recurrence.Range.TimeZone); err != nil {
			return err
		}
	}
	// A UTC series would change its civil-time recurrence semantics. Detached
	// instances are also excluded until their original-start identity and Google
	// inheritance behavior have a separately verified write contract.
	if googleFoldEvent(ev) && (ev.Recurrence != nil || ev.RecurrenceRule != "" || ev.OpaqueRecurrence ||
		!ev.RecurrenceID.IsZero() || ev.Type == "occurrence" || ev.Type == "exception") {
		return fmt.Errorf("ambiguous recurring calendar endpoints cannot be represented safely by Google Calendar")
	}
	return nil
}
