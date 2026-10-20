package demotest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/calendartest"
	"github.com/julion2/durian/cli/internal/googlecalendar"
	"github.com/julion2/durian/cli/internal/graphcalendar"
)

func TestRecordedMonthlyCalendarThroughSync(t *testing.T) {
	for _, provider := range []string{"google", "graph"} {
		t.Run(provider, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + provider + "-monthly-sync.json.gz")
			if err != nil {
				t.Fatal(err)
			}
			z, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			var replay calendartest.Replay
			if err := json.NewDecoder(z).Decode(&replay.Recording); err != nil {
				t.Fatal(err)
			}
			httpClient := &http.Client{Transport: &replay}
			var p calendarsync.CalendarProvider = googlecalendar.NewWithToken("demo@example.invalid", calendartest.GoogleBase, "offline", httpClient)
			if provider == "graph" {
				p = graphcalendar.NewWithToken("demo@example.invalid", calendartest.GraphBase, "offline", httpClient)
			}
			t.Cleanup(func() {
				if replay.Index != len(replay.Recording.Exchanges) {
					t.Fatalf("consumed %d of %d exchanges", replay.Index, len(replay.Recording.Exchanges))
				}
			})
			exerciseMonthlyCalendarSync(t, p, "test-calendar", replay.Recording.Prefix, provider == "graph")
		})
	}
}

// Inspired by Thunderbird negative-ordinal/BYSETPOS scenarios; independently
// authored ICS and calendar dates, exercised through Durian's actual planner.
func exerciseMonthlyCalendarSync(t *testing.T, p calendarsync.CalendarProvider, cal, prefix string, graph bool) {
	t.Helper()
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	subject := prefix + "last-Friday"
	key := subject
	cleanupID := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	if graph {
		key = calendartest.GraphTransactionID(subject)
		cleanupID = ""
	}
	t.Cleanup(func() {
		if cleanupID == "" {
			return // Unknown Graph POST outcomes must never trigger listing cleanup.
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := p.DeleteEvent(ctx, cal, cleanupID, "", false); err != nil {
			t.Error("owned monthly-event cleanup failed; inspect recording")
		}
	})
	start := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	ev := calendar.Event{ICalUID: "monthly-authored@example.invalid", Subject: subject,
		Start: start, End: start.Add(time.Hour), TimeZone: "UTC",
		RecurrenceRule: "FREQ=MONTHLY;INTERVAL=1;COUNT=4;BYDAY=FR;BYSETPOS=-1"}
	dir := t.TempDir()
	id, err := calendartest.UploadAuthoredAppointment(ctx, p, cal, dir, key, ev)
	if id != "" {
		cleanupID = id
	}
	if err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	want := []string{"2026-10-30T10:00:00Z", "2026-11-27T10:00:00Z", "2026-12-25T10:00:00Z", "2027-01-29T10:00:00Z"}
	check := func(events []calendar.Event, cached bool) {
		t.Helper()
		var got []string
		for _, e := range events {
			if e.Subject != subject {
				continue // The pinned calendar also holds other intentional demo data.
			}
			got = append(got, e.Start.UTC().Format(time.RFC3339))
			if e.End.Sub(e.Start) != time.Hour || (cached && e.OccurrenceSource != "provider") {
				t.Error("monthly occurrence duration or synced source lost")
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("monthly instances=%v want=%v", got, want)
		}
	}
	instances, err := p.FetchInstances(ctx, cal, from, to)
	if err != nil {
		t.Fatal("monthly provider instance read failed")
	}
	check(instances, false)
	for i := 0; i < 2; i++ {
		// This reloads the persisted sync state on each pass. The second pass
		// must converge, not recreate the series or fall back to local estimates.
		synced, err := calendartest.SyncOccurrences(ctx, p, cal, dir, from, to)
		if err != nil {
			t.Fatal(err)
		}
		check(synced, true)
	}

	// Neither provider uploads authored detached VEVENTs through a master
	// create. Fail closed, rather than accepting a partial series and replacing
	// the local file with a read-back that silently drops the moved occurrence.
	blocked := ev
	blocked.ICalUID = "monthly-moved@example.invalid"
	blocked.Subject = prefix + "moved-override"
	blocked.LastModified = start
	blocked.Overrides = []calendar.Event{{Subject: blocked.Subject, TimeZone: "UTC",
		LastModified: start,
		RecurrenceID: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC),
		Start:        time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 12, 1, 11, 0, 0, 0, time.UTC)}}
	blockedKey := blocked.Subject
	if graph {
		blockedKey = calendartest.GraphTransactionID(blocked.Subject)
	}
	blockedDir := t.TempDir()
	blockedID, err := calendartest.UploadAuthoredAppointment(ctx, p, cal, blockedDir, blockedKey, blocked)
	if blockedID != "" {
		// A regression that creates a partial series must still clean its exact ID.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := p.DeleteEvent(ctx, cal, blockedID, "", false); err != nil {
				t.Error("partial-series cleanup failed; inspect recording")
			}
		})
	}
	if err == nil || blockedID != "" {
		t.Fatal("authored override must not report success or create a partial remote series")
	}
	wantICS, err := calendar.EventToICal(blocked)
	if err != nil {
		t.Fatal(err)
	}
	actualICS, err := os.ReadFile(filepath.Join(blockedDir, "Durian Test", calendar.EventFileName(blocked.ICalUID)))
	if err != nil || !bytes.Equal(actualICS, wantICS) {
		t.Fatal("rejected upload lost authored ICS content")
	}
	state, err := calendarsync.NewFileStateStore(blockedDir).Load()
	if err != nil || len(state.Calendars[cal].Items) != 0 {
		t.Fatal("rejected upload recorded false convergence")
	}
	masters, err := p.FetchMasterEvents(ctx, cal)
	if err != nil {
		t.Fatal("verify rejected series was not created")
	}
	for _, master := range masters {
		if master.Subject == blocked.Subject {
			t.Fatal("provider contains a partial series after rejected upload")
		}
	}
}
