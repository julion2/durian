package graphcalendar

import (
	"fmt"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

func civilDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ianaZone accepts IANA IDs and the complete CLDR Windows global-default
// mapping. Custom Outlook zones have no portable rules: never guess UTC.
func ianaZone(zone string) (string, error) {
	if zone == "" || zone == "UTC" {
		return "UTC", nil
	}
	if mapped, ok := windowsZones[zone]; ok {
		zone = mapped
	}
	if zone == "Local" {
		return "", fmt.Errorf("unsupported calendar timezone %q", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", fmt.Errorf("unsupported calendar timezone %q: %w", zone, err)
	}
	return zone, nil
}

func graphEventZones(ge graphEvent) (start, end string, rec *calendar.Recurrence, err error) {
	start = ge.OriginalStartTimeZone
	if start == "" {
		start = ge.Start.TimeZone
	}
	end = ge.OriginalEndTimeZone
	if end == "" {
		end = ge.OriginalStartTimeZone
		if end == "" {
			end = ge.End.TimeZone
		}
	}
	if end == "" {
		end = start
	}
	start, err = ianaZone(start)
	if err != nil {
		return
	}
	end, err = ianaZone(end)
	if err != nil {
		return
	}
	// Validate response zones as well: a bare boundary uses these, not the
	// original zones. Unknown zones must abort snapshots, not drop events.
	for _, zone := range []string{ge.Start.TimeZone, ge.End.TimeZone} {
		if _, err = ianaZone(zone); err != nil {
			return
		}
	}
	if ge.Recurrence != nil {
		copy := *ge.Recurrence
		rec = &copy
		zone := rec.Range.TimeZone
		if zone == "" {
			zone = start
		}
		rec.Range.TimeZone, err = ianaZone(zone)
	}
	return
}

func validateWriteEvent(ev calendar.Event) error {
	if ev.Floating {
		return fmt.Errorf("floating calendar events cannot be represented by Microsoft Graph")
	}
	for _, zone := range []string{ev.TimeZone, ev.EndTimeZone} {
		if _, err := ianaZone(zone); err != nil {
			return err
		}
	}
	if ev.Recurrence != nil {
		if _, err := ianaZone(ev.Recurrence.Range.TimeZone); err != nil {
			return err
		}
	}
	if !ev.AllDay {
		endZone := ev.EndTimeZone
		if endZone == "" {
			endZone = ev.TimeZone
		}
		for _, endpoint := range []struct {
			time time.Time
			zone string
		}{{ev.Start, ev.TimeZone}, {ev.End, endZone}} {
			zone, _ := ianaZone(endpoint.zone)
			loc, _ := time.LoadLocation(zone)
			local := endpoint.time.In(loc)
			_, offset := local.Zone()
			start, end := local.ZoneBounds()
			// Graph documents a zone-less dateTime plus timeZone, not an
			// offset-disambiguated fold. Refuse both repeated clock intervals
			// instead of assuming which instant its server will pick.
			if !start.IsZero() {
				_, before := start.Add(-time.Second).In(loc).Zone()
				if before > offset && local.Before(start.Add(time.Duration(before-offset)*time.Second)) {
					return fmt.Errorf("ambiguous calendar time %s in %s cannot be safely written to Microsoft Graph", local.Format(time.RFC3339), zone)
				}
			}
			if !end.IsZero() {
				_, after := end.In(loc).Zone()
				if after < offset && !local.Before(end.Add(-time.Duration(offset-after)*time.Second)) {
					return fmt.Errorf("ambiguous calendar time %s in %s cannot be safely written to Microsoft Graph", local.Format(time.RFC3339), zone)
				}
			}
		}
	}
	return nil
}
