package googlecalendar

import (
	"fmt"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
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
	return nil
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
	return nil
}
