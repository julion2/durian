package calendarsync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// prodID identifies durian as the generator in exported VCALENDARs. The
// historical value is kept so re-exports over existing vdirs stay
// byte-identical.
const prodID = "-//durian//graphcalendar//EN"

// ExportStats reports what one Export run wrote.
type ExportStats struct {
	Calendars int
	Events    int
}

// Export writes the calendars of the account as a vdir under outDir (the
// account-level directory, i.e. <vdir_path>/<account-dir>): one directory per
// calendar — with a `displayname` file holding the calendar name and, when the
// calendar has one, a `color` file holding its "#RRGGBB" hex color, per
// vdirsyncer/khal convention — and one .ics file per event instance within
// [from, to). When include is non-empty, only calendars whose display name
// matches an entry (case-insensitive) are exported. Existing files are
// overwritten, so re-exports are idempotent.
func Export(ctx context.Context, p CalendarProvider, outDir string, from, to time.Time, include []string) (ExportStats, error) {
	var stats ExportStats

	calendars, err := p.ListCalendars(ctx)
	if err != nil {
		return stats, err
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return stats, fmt.Errorf("failed to create export dir %s: %w", outDir, err)
	}

	for _, cal := range calendars {
		if !calendarIncluded(cal.Name, include) {
			slog.Debug("Skipping calendar not in include list", "module", "CALSYNC",
				"calendar", cal.Name)
			continue
		}

		calDir := filepath.Join(outDir, sanitizeName(cal.Name))
		if err := os.MkdirAll(calDir, 0o700); err != nil {
			return stats, fmt.Errorf("failed to create calendar dir %s: %w", calDir, err)
		}
		if err := WriteFileAtomic(filepath.Join(calDir, "displayname"), []byte(cal.Name+"\n"), 0o600); err != nil {
			return stats, fmt.Errorf("failed to write displayname for %s: %w", cal.Name, err)
		}
		if cal.HexColor != "" {
			if err := WriteFileAtomic(filepath.Join(calDir, "color"), []byte(cal.HexColor+"\n"), 0o600); err != nil {
				return stats, fmt.Errorf("failed to write color for %s: %w", cal.Name, err)
			}
		}

		events, err := p.FetchInstances(ctx, cal.ID, from, to)
		if err != nil {
			return stats, fmt.Errorf("failed to fetch events for calendar %s: %w", cal.Name, err)
		}
		for _, e := range events {
			// The per-occurrence provider event id (not the iCalUID, which is
			// shared across a recurring series) keeps filenames unique.
			path := filepath.Join(calDir, sanitizeName(e.ID)+".ics")
			if err := WriteFileAtomic(path, []byte(EventToICS(e, prodID)), 0o600); err != nil {
				return stats, fmt.Errorf("failed to write %s: %w", path, err)
			}
			stats.Events++
		}
		stats.Calendars++

		slog.Debug("Exported calendar", "module", "CALSYNC",
			"calendar", cal.Name, "events", len(events), "dir", calDir)
	}

	slog.Info("Calendar export complete", "module", "CALSYNC",
		"calendars", stats.Calendars, "events", stats.Events, "dir", outDir)
	return stats, nil
}
