// One-way Export test, moved here with the engine: it drives Export through
// the real Graph provider against a fake calendarView server (see
// twosync_test.go for the shared testClient helper).

package calendarsync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

func TestExport(t *testing.T) {
	calendarView := func(events ...map[string]any) string {
		body, err := json.Marshal(map[string]any{"value": events})
		if err != nil {
			t.Fatalf("marshal calendarView: %v", err)
		}
		return string(body)
	}
	event := func(id, subject, start, end string) map[string]any {
		return map[string]any{
			"id":                   id,
			"iCalUId":              "ical-" + id,
			"subject":              subject,
			"start":                map[string]string{"dateTime": start, "timeZone": "UTC"},
			"end":                  map[string]string{"dateTime": end, "timeZone": "UTC"},
			"isAllDay":             false,
			"location":             map[string]string{"displayName": "HQ"},
			"bodyPreview":          "agenda",
			"lastModifiedDateTime": "2026-07-20T09:00:00Z",
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value": [
			{"id": "cal1", "name": "Work/Team", "hexColor": "#0b8043"},
			{"id": "cal2", "name": "Personal"}
		]}`)
	})
	mux.HandleFunc("/me/calendars/cal1/calendarView", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, calendarView(
			event("ev/1", "Planning", "2026-07-23T10:00:00.0000000", "2026-07-23T11:00:00.0000000"),
			event("ev2", "Review", "2026-07-24T14:00:00.0000000", "2026-07-24T15:00:00.0000000"),
		))
	})
	mux.HandleFunc("/me/calendars/cal2/calendarView", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, calendarView(
			event("ev3", "Dentist", "2026-07-25T09:00:00.0000000", "2026-07-25T09:30:00.0000000"),
		))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	outDir := t.TempDir()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	stats, err := calendarsync.Export(context.Background(), c, outDir, from, to, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if stats.Calendars != 2 || stats.Events != 3 {
		t.Errorf("stats = %+v, want 2 calendars / 3 events", stats)
	}

	// Directory per calendar, with the path separator sanitized.
	for _, dir := range []string{"Work_Team", "Personal"} {
		info, err := os.Stat(filepath.Join(outDir, dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("expected calendar dir %s: err=%v", dir, err)
		}
	}

	// displayname carries the original (unsanitized) calendar name.
	displayname, err := os.ReadFile(filepath.Join(outDir, "Work_Team", "displayname"))
	if err != nil {
		t.Fatalf("read displayname: %v", err)
	}
	if got := strings.TrimSpace(string(displayname)); got != "Work/Team" {
		t.Errorf("displayname = %q, want Work/Team", got)
	}

	// One .ics per event, sanitized event id as filename.
	for _, f := range []struct{ dir, name, uid string }{
		{"Work_Team", "ev_1.ics", "UID:ical-ev/1"},
		{"Work_Team", "ev2.ics", "UID:ical-ev2"},
		{"Personal", "ev3.ics", "UID:ical-ev3"},
	} {
		body, err := os.ReadFile(filepath.Join(outDir, f.dir, f.name))
		if err != nil {
			t.Fatalf("read %s/%s: %v", f.dir, f.name, err)
		}
		ics := string(body)
		for _, want := range []string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", f.uid, "END:VEVENT", "END:VCALENDAR"} {
			if !strings.Contains(ics, want) {
				t.Errorf("%s/%s missing %q:\n%s", f.dir, f.name, want, ics)
			}
		}
	}

	// color file: written for a calendar with a hexColor, absent otherwise.
	if got, err := os.ReadFile(filepath.Join(outDir, "Work_Team", "color")); err != nil {
		t.Errorf("expected color file for Work/Team: %v", err)
	} else if strings.TrimSpace(string(got)) != "#0b8043" {
		t.Errorf("color = %q, want #0b8043", strings.TrimSpace(string(got)))
	}
	if _, err := os.Stat(filepath.Join(outDir, "Personal", "color")); !os.IsNotExist(err) {
		t.Errorf("Personal has no hexColor; color file should be absent (err=%v)", err)
	}

	// include filter: only the named calendar is exported.
	incDir := t.TempDir()
	incStats, err := calendarsync.Export(context.Background(), c, incDir, from, to, []string{"Personal"})
	if err != nil {
		t.Fatalf("Export with include: %v", err)
	}
	if incStats.Calendars != 1 || incStats.Events != 1 {
		t.Errorf("include stats = %+v, want 1 calendar / 1 event", incStats)
	}
	if _, err := os.Stat(filepath.Join(incDir, "Personal")); err != nil {
		t.Errorf("included calendar not exported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(incDir, "Work_Team")); !os.IsNotExist(err) {
		t.Errorf("Work_Team not in include list but was exported (err=%v)", err)
	}
}
