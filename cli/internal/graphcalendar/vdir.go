package graphcalendar

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// prodID identifies durian as the generator in exported VCALENDARs.
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
func Export(ctx context.Context, c *Client, outDir string, from, to time.Time, include []string) (ExportStats, error) {
	var stats ExportStats

	calendars, err := c.ListCalendars(ctx)
	if err != nil {
		return stats, err
	}

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return stats, fmt.Errorf("failed to create export dir %s: %w", outDir, err)
	}

	for _, cal := range calendars {
		if !calendarIncluded(cal.Name, include) {
			slog.Debug("Skipping calendar not in include list", "module", "GRAPHCAL",
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

		events, err := c.FetchEvents(ctx, cal.ID, from, to)
		if err != nil {
			return stats, fmt.Errorf("failed to fetch events for calendar %s: %w", cal.Name, err)
		}
		for _, e := range events {
			// The per-occurrence Graph event id (not the iCalUID, which is
			// shared across a recurring series) keeps filenames unique.
			path := filepath.Join(calDir, sanitizeName(e.ID)+".ics")
			if err := WriteFileAtomic(path, []byte(EventToICS(e, prodID)), 0o600); err != nil {
				return stats, fmt.Errorf("failed to write %s: %w", path, err)
			}
			stats.Events++
		}
		stats.Calendars++

		slog.Debug("Exported calendar", "module", "GRAPHCAL",
			"calendar", cal.Name, "events", len(events), "dir", calDir)
	}

	slog.Info("Calendar export complete", "module", "GRAPHCAL",
		"calendars", stats.Calendars, "events", stats.Events, "dir", outDir)
	return stats, nil
}

// calendarIncluded reports whether a calendar display name passes the include
// filter: an empty filter admits every calendar, otherwise the name must match
// one entry case-insensitively.
func calendarIncluded(name string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	for _, want := range include {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

// WriteFileAtomic writes data to path via a temporary sibling and a rename,
// so a reader never observes a half-written file.
//
// The vdir is a shared surface: the GUI reads it through the HTTP API, the
// sync engine rewrites it in the background, and khal or vdirsyncer may walk
// it at any moment. A plain os.WriteFile truncates first and fills after, so
// every write opens a window in which a concurrent reader parses a truncated
// .ics — which the planner cannot distinguish from a corrupt file. rename(2)
// is atomic within a directory, so the file is either the old content or the
// new one.
//
// The temp file is created in the same directory (a rename across filesystems
// would fail) and carries the ".ics-tmp" suffix rather than ".ics", so the
// local scan ignores it even if a crash leaves one behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.ics-tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// sanitizeName makes a calendar name or event id safe as a single filesystem
// path component: path separators and other unsafe characters become '_', and
// leading dots are stripped so no hidden files appear.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" {
		return "unnamed"
	}
	return out
}
