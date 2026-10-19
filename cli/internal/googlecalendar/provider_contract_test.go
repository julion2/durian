package googlecalendar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/calendartest"
)

func TestGoogleRecordedTimezones(t *testing.T) {
	data, err := os.ReadFile("testdata/google-timezones.json")
	if err != nil {
		t.Fatal(err)
	}
	var replay calendartest.Replay
	if err := json.Unmarshal(data, &replay.Recording); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if replay.Index != len(replay.Recording.Exchanges) {
			t.Errorf("replayed %d of %d exchanges", replay.Index, len(replay.Recording.Exchanges))
		}
		t.Logf("replayed %d of %d exchanges without network", replay.Index, len(replay.Recording.Exchanges))
	})
	c := NewWithToken("demo@example.invalid", calendartest.GoogleBase, "offline", &http.Client{Transport: &replay})
	exerciseProviderTimezones(t, c, "test-calendar", replay.Recording.Prefix)
}

// The exact same public-provider contract runs against Google and the saved
// responses. Expected instants are independently specified, not derived from
// the recurrence implementation under test.
func exerciseProviderTimezones(t *testing.T, c *Client, cal, prefix string) {
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	from, to := contractTime(t, "2026-01-01T00:00:00Z"), contractTime(t, "2027-01-01T00:00:00Z")
	create := func(t *testing.T, ev calendar.Event, name string) calendar.Event {
		t.Helper()
		opts := calendarsync.CreateOptions{IdempotencyKey: prefix + name}
		// Register cleanup BEFORE POST, including a lost create answer. This
		// deletes only this run's deterministic ID, not every matching title.
		id := createEventID(opts.IdempotencyKey)
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			err := c.DeleteEvent(cleanupCtx, cal, id, "", false)
			if err != nil && !errors.Is(err, calendarsync.ErrNotFound) {
				t.Error("owned-event cleanup failed; inspect redacted recording")
			}
		})
		got, err := c.CreateEvent(ctx, cal, ev, opts)
		if err != nil {
			t.Fatal("create failed; inspect redacted recording")
		}
		if got.ID != id || got.Subject != ev.Subject || got.ETag == "" || len(got.Attendees) != 0 {
			t.Fatal("create identity/content contract failed")
		}
		return got
	}
	read := func(t *testing.T, id string) calendar.Event {
		t.Helper()
		got, err := c.GetEvent(ctx, cal, id)
		if err != nil {
			t.Fatal("read-back failed; inspect redacted recording")
		}
		return got
	}
	for _, tc := range []struct {
		name, start, end, zone, endZone string
		allDay                          bool
	}{
		{"single-gap-crossing", "2026-03-29T01:30:00+01:00", "2026-03-29T03:30:00+02:00", "Europe/Berlin", "", false},
		{"single-first-fold", "2026-10-25T02:15:00+02:00", "2026-10-25T02:45:00+02:00", "Europe/Berlin", "", false},
		{"single-second-fold", "2026-10-25T02:15:00+01:00", "2026-10-25T02:45:00+01:00", "Europe/Berlin", "", false},
		{"single-fold-crossing-and-clear", "2026-10-25T02:30:00+02:00", "2026-10-25T02:00:00+01:00", "Europe/Berlin", "", false},
		{"single-travel-zones", "2026-03-28T18:00:00+01:00", "2026-03-28T21:00:00-04:00", "Europe/Berlin", "America/New_York", false},
		{"all-day-over-DST", "2026-03-28T00:00:00Z", "2026-03-30T00:00:00Z", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := calendar.Event{Subject: prefix + tc.name, Start: contractTime(t, tc.start), End: contractTime(t, tc.end), TimeZone: tc.zone, EndTimeZone: tc.endZone, AllDay: tc.allDay}
			created := create(t, want, tc.name)
			checkContractEvent(t, read(t, created.ID), want)
			again, err := c.CreateEvent(ctx, cal, want, calendarsync.CreateOptions{IdempotencyKey: prefix + tc.name})
			if err != nil || again.ID != created.ID {
				t.Fatal("idempotent create did not return the same event")
			}
			want.Subject += " edited"
			if tc.allDay {
				want.Start = want.Start.AddDate(0, 0, 1)
				want.End = want.End.AddDate(0, 0, 1)
			} else {
				want.Start = want.Start.Add(15 * time.Minute)
				want.End = want.End.Add(15 * time.Minute)
			}
			if err := c.UpdateEvent(ctx, cal, created.ID, calendarsync.UpdateSpec{Event: want, ETag: created.ETag}); err != nil {
				t.Fatal("single edit failed; inspect redacted recording")
			}
			checkContractEvent(t, read(t, created.ID), want)
			if err := c.UpdateEvent(ctx, cal, created.ID, calendarsync.UpdateSpec{Event: want, ETag: created.ETag}); !errors.Is(err, calendarsync.ErrPrecondition) {
				t.Error("stale ETag did not reject edit")
			}
			if tc.name == "single-fold-crossing-and-clear" {
				if created.End.Sub(created.Start) != 30*time.Minute {
					t.Error("fold crossing changed the 30-minute real duration")
				}
				current := read(t, created.ID)
				want.Start = contractTime(t, "2026-10-26T10:00:00-04:00")
				want.End = want.Start.Add(30 * time.Minute)
				want.TimeZone, want.EndTimeZone = "America/New_York", "Europe/London"
				want.Subject += " outside fold"
				if err := c.UpdateEvent(ctx, cal, created.ID, calendarsync.UpdateSpec{Event: want, ETag: current.ETag}); err != nil {
					t.Fatal("leaving fold failed; inspect redacted recording")
				}
				// Both zones differ from the old metadata. If PATCH does not
				// actually delete the pair, GetEvent restores the stale zones.
				checkContractEvent(t, read(t, created.ID), want)
			}
		})
	}
	for _, tc := range []struct {
		name, zone, start, rule, exdate, moved string
		want                                   []string
	}{
		{"Berlin-spring-EXDATE-move-cancel", "Europe/Berlin", "2026-03-22T09:00:00+01:00", "FREQ=WEEKLY;COUNT=5", "2026-03-29T07:00:00Z", "2026-04-06T12:00:00Z", []string{"2026-03-22T08:00:00Z", "2026-04-05T07:00:00Z", "2026-04-12T07:00:00Z", "2026-04-19T07:00:00Z"}},
		{"Berlin-autumn-EXDATE-move-cancel", "Europe/Berlin", "2026-10-18T09:00:00+02:00", "FREQ=WEEKLY;COUNT=4", "2026-10-25T08:00:00Z", "2026-11-02T12:00:00Z", []string{"2026-10-18T07:00:00Z", "2026-11-01T08:00:00Z", "2026-11-08T08:00:00Z"}},
		{"New-York-spring-EXDATE-move-cancel", "America/New_York", "2026-03-01T09:00:00-05:00", "FREQ=WEEKLY;COUNT=4", "2026-03-08T13:00:00Z", "2026-03-16T12:00:00Z", []string{"2026-03-01T14:00:00Z", "2026-03-15T13:00:00Z", "2026-03-22T13:00:00Z"}},
		{"Berlin-gap-series", "Europe/Berlin", "2026-03-22T02:30:00+01:00", "FREQ=WEEKLY;COUNT=3", "", "", []string{"2026-03-22T01:30:00Z", "2026-04-05T00:30:00Z", "2026-04-12T00:30:00Z"}},
		{"Berlin-fold-series", "Europe/Berlin", "2026-10-18T02:30:00+02:00", "FREQ=WEEKLY;COUNT=3", "", "", []string{"2026-10-18T00:30:00Z", "2026-10-25T00:30:00Z", "2026-11-01T01:30:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := contractTime(t, tc.start)
			dir := t.TempDir()
			ev := calendar.Event{Subject: prefix + tc.name, Start: start, End: start.Add(45 * time.Minute), TimeZone: tc.zone, RecurrenceRule: tc.rule}
			if tc.exdate != "" {
				ev.ExceptionDates = []time.Time{contractTime(t, tc.exdate)}
			}
			created := create(t, ev, tc.name)
			checkContractEvent(t, read(t, created.ID), ev)
			instances := contractInstances(t, ctx, c, cal, ev.Subject, from, to)
			googleWant := tc.want
			if tc.name == "Berlin-gap-series" {
				// Google normalizes the nonexistent March 29 02:30 Berlin
				// to 03:30 (01:30Z), consuming COUNT. RFC 5545/local/ICS
				// instead skip that wall time WITHOUT consuming COUNT.
				googleWant = []string{"2026-03-22T01:30:00Z", "2026-03-29T01:30:00Z", "2026-04-05T00:30:00Z"}
			}
			checkContractTimes(t, "Google", instances, googleWant)
			checkContractLocal(t, ctx, c, cal, created.ID, from, to, tc.want)
			checkContractSynced(t, ctx, c, cal, dir, from, to, googleWant)
			if tc.moved == "" {
				return
			}
			if len(instances) != len(tc.want) {
				t.Fatal("cannot safely identify mutation targets")
			}
			moved, removed := instances[1], instances[len(instances)-1]
			moved.Start = contractTime(t, tc.moved)
			moved.End = moved.Start.Add(45 * time.Minute)
			moved.Subject += " moved"
			if err := c.UpdateEvent(ctx, cal, moved.ID, calendarsync.UpdateSpec{Event: moved, ETag: moved.ETag}); err != nil {
				t.Fatal("move occurrence failed; inspect redacted recording")
			}
			checkContractEvent(t, read(t, moved.ID), moved)
			// Editing one exception can change sibling instance ETags. Fetch
			// the current version instead of accidentally testing a stale delete.
			removed = read(t, removed.ID)
			if err := c.DeleteEvent(ctx, cal, removed.ID, removed.ETag, false); err != nil {
				t.Fatal("cancel occurrence failed; inspect redacted recording")
			}
			want := append([]string(nil), tc.want[:len(tc.want)-1]...)
			want[1] = tc.moved
			sort.Strings(want)
			checkContractTimes(t, "Google after exceptions", contractInstances(t, ctx, c, cal, ev.Subject, from, to), want)
			checkContractLocal(t, ctx, c, cal, created.ID, from, to, want)
			checkContractSynced(t, ctx, c, cal, dir, from, to, want)
		})
	}
	t.Run("ambiguous-series-write-rejected", func(t *testing.T) {
		for _, start := range []string{"2026-10-25T02:15:00+02:00", "2026-10-25T02:15:00+01:00"} {
			ev := calendar.Event{Subject: prefix + "ambiguous series", Start: contractTime(t, start), TimeZone: "Europe/Berlin", RecurrenceRule: "FREQ=WEEKLY;COUNT=3"}
			ev.End = ev.Start.Add(30 * time.Minute)
			_, createErr := c.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: ev.Subject})
			updateErr := c.UpdateEvent(ctx, cal, createEventID(ev.Subject), calendarsync.UpdateSpec{Event: ev})
			for _, err := range []error{createErr, updateErr} {
				if err == nil || !strings.Contains(err.Error(), "ambiguous recurring calendar endpoints") {
					t.Error("ambiguous series was not rejected by provider validation before I/O")
				}
			}
		}
	})
	t.Run("floating-write-rejected", func(t *testing.T) {
		ev := calendar.Event{Subject: prefix + "floating", Floating: true, Start: from.Add(time.Hour), End: from.Add(2 * time.Hour)}
		if _, err := c.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: prefix + "floating"}); err == nil {
			t.Fatal("floating write silently converted to an instant")
		}
	})
	// Subtest cleanup has already removed all created masters. Read-back must
	// show no surviving event/occurrence from this run; never delete by listing.
	t.Run("cleanup-read-back", func(t *testing.T) {
		if got := contractInstances(t, ctx, c, cal, prefix, from, to); len(got) != 0 {
			t.Errorf("cleanup left %d owned instances", len(got))
		}
	})
}

func contractTime(t *testing.T, value string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func checkContractEvent(t *testing.T, got, want calendar.Event) {
	t.Helper()
	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.AllDay != want.AllDay || got.Subject != want.Subject {
		t.Errorf("read-back content/time differs: start=%s end=%s allDay=%v", got.Start, got.End, got.AllDay)
	}
	endZone := want.EndTimeZone
	if endZone == "" {
		endZone = want.TimeZone
	}
	if !want.AllDay && (got.TimeZone != want.TimeZone || got.EndTimeZone != endZone) {
		t.Errorf("read-back zones %q/%q, want %q/%q", got.TimeZone, got.EndTimeZone, want.TimeZone, endZone)
	}
	if len(got.Attendees) != 0 {
		t.Error("unexpected attendees")
	}
}

func contractInstances(t *testing.T, ctx context.Context, c *Client, cal, subject string, from, to time.Time) []calendar.Event {
	t.Helper()
	all, err := c.FetchInstances(ctx, cal, from, to)
	if err != nil {
		t.Fatal("fetch instances failed; inspect redacted recording")
	}
	var found []calendar.Event
	for _, ev := range all {
		if strings.HasPrefix(ev.Subject, subject) {
			found = append(found, ev)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Start.Before(found[j].Start) })
	return found
}

func checkContractTimes(t *testing.T, label string, events []calendar.Event, want []string) {
	t.Helper()
	var got []string
	for _, ev := range events {
		got = append(got, ev.Start.UTC().Format(time.RFC3339))
		if ev.End.Sub(ev.Start) != 45*time.Minute {
			t.Errorf("%s occurrence duration=%s, want 45m", label, ev.End.Sub(ev.Start))
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s starts=%v; want %v", label, got, want)
	}
}

func checkContractLocal(t *testing.T, ctx context.Context, c *Client, cal, id string, from, to time.Time, want []string) {
	t.Helper()
	masters, err := c.FetchMasterEvents(ctx, cal)
	if err != nil {
		t.Fatal("fetch masters failed; inspect redacted recording")
	}
	for _, master := range masters {
		if master.ID != id {
			continue
		}
		checkContractTimes(t, "local expansion", calendar.ExpandOccurrences(master, from, to), want)
		ics, err := calendar.EventToICal(master)
		if err != nil {
			t.Fatal("iCalendar encoding failed")
		}
		parsed, err := calendar.ICalToEvent(ics, "")
		if err != nil {
			t.Fatal("iCalendar decoding failed")
		}
		checkContractTimes(t, "iCalendar round-trip", calendar.ExpandOccurrences(parsed, from, to), want)
		return
	}
	t.Fatal("series master missing")
}

func checkContractSynced(t *testing.T, ctx context.Context, c *Client, cal, dir string, from, to time.Time, want []string) {
	t.Helper()
	events, err := calendartest.SyncOccurrences(ctx, c, cal, dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	checkContractTimes(t, "synced provider occurrences", events, want)
	for _, event := range events {
		if event.OccurrenceSource != "provider" || !calendar.ToCalendarEvent("Durian Test", event, false).Recurring {
			t.Error("synced recurrence did not retain provider source/series identity")
		}
	}
}
