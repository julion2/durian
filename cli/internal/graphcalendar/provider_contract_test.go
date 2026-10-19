package graphcalendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

func TestGraphRecordedTimezones(t *testing.T) {
	data, err := os.ReadFile("testdata/graph-timezones.json")
	if err != nil {
		t.Fatal(err)
	}
	var replay calendartest.Replay
	if err := json.Unmarshal(data, &replay.Recording); err != nil {
		t.Fatal("invalid sanitized recording")
	}
	t.Cleanup(func() {
		if replay.Index != len(replay.Recording.Exchanges) {
			t.Errorf("replayed %d of %d exchanges", replay.Index, len(replay.Recording.Exchanges))
		}
		t.Logf("replayed %d of %d exchanges without network", replay.Index, len(replay.Recording.Exchanges))
	})
	// No fallback transport, token source, or OAuth. Unexpected requests fail.
	c := NewWithToken("demo@example.invalid", calendartest.GraphBase, "offline", &http.Client{Transport: &replay})
	exerciseGraphTimezones(t, c, "test-calendar", replay.Recording.Prefix)
}

// Live and offline run the SAME public-provider contract. The only bypass is
// the explicitly named raw probes: production's ambiguous-time rejection must
// remain until real Graph evidence establishes a safe representation.
// Provider-specific gap/fold expectations remain distinct from local RFC
// expansion. The sync/read path must use provider occurrences, not estimates.
// The live guard adds false isReminderOn/isOnlineMeeting on POST, forces UTC
// plus ImmutableId on every request, and augments list $select with safety and
// series membership fields. Recorder sees provider requests and sanitized
// genuine responses, not these extra safety GETs or injected outbound fields.
func exerciseGraphTimezones(t *testing.T, c *Client, cal, prefix string) {
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	from, to := graphContractTime(t, "2026-01-01T00:00:00Z"), graphContractTime(t, "2027-01-01T00:00:00Z")
	createdCount, cleanedCount := 0, 0
	cleanup := func(t *testing.T, id string, deleted *bool) {
		t.Helper()
		createdCount++
		t.Cleanup(func() {
			if *deleted {
				cleanedCount++
				return
			}
			cleanupCtx, stop := context.WithTimeout(context.Background(), 45*time.Second)
			defer stop()
			if err := c.DeleteEvent(cleanupCtx, cal, id, "", false); err != nil {
				t.Error("owned-ID cleanup failed; inspect sanitized evidence")
				return
			}
			cleanedCount++
		})
	}
	create := func(t *testing.T, ev calendar.Event) calendar.Event {
		t.Helper()
		got, err := c.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: calendartest.GraphTransactionID(ev.Subject)})
		if err != nil {
			cancel() // Stop further mutations if a POST outcome is unverified.
			t.Fatal("create failed; inspect sanitized recording")
		}
		deleted := false
		cleanup(t, got.ID, &deleted)
		if got.ID == "" || got.Subject != ev.Subject || got.ETag == "" || len(got.Attendees) != 0 || got.IsOnlineMeeting {
			t.Fatal("create identity/safety contract failed")
		}
		return got
	}
	read := func(t *testing.T, id string) calendar.Event {
		t.Helper()
		got, err := c.GetEvent(ctx, cal, id)
		if err != nil {
			t.Fatal("read-back failed; inspect sanitized recording")
		}
		return got
	}
	for _, tc := range []struct {
		name, start, end, zone, endZone string
		allDay                          bool
	}{
		{"single-gap-crossing", "2026-03-29T01:30:00+01:00", "2026-03-29T03:30:00+02:00", "Europe/Berlin", "", false},
		{"single-travel-zones", "2026-03-28T18:00:00+01:00", "2026-03-28T21:00:00-04:00", "Europe/Berlin", "America/New_York", false},
		{"all-day-over-DST", "2026-03-28T00:00:00Z", "2026-03-30T00:00:00Z", "Europe/Berlin", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := calendar.Event{Subject: prefix + tc.name, Start: graphContractTime(t, tc.start), End: graphContractTime(t, tc.end), TimeZone: tc.zone, EndTimeZone: tc.endZone, AllDay: tc.allDay}
			created := create(t, want)
			checkGraphContractEvent(t, read(t, created.ID), want)
			again, err := c.CreateEvent(ctx, cal, want, calendarsync.CreateOptions{IdempotencyKey: calendartest.GraphTransactionID(want.Subject)})
			if err != nil || again.ID != created.ID {
				if err == nil && again.ID != "" {
					deleted := false
					cleanup(t, again.ID, &deleted)
				}
				t.Error("idempotent create did not return same event")
			}
			// Graph can advance changeKey without a content change, including
			// after an idempotent POST. Read the current version before editing;
			// never treat the original POST's key as guaranteed fresh.
			beforeEdit := read(t, created.ID)
			checkGraphContractEvent(t, beforeEdit, want)
			before := want
			want.Subject += " edited"
			if tc.allDay {
				want.Start = want.Start.AddDate(0, 0, 1)
				want.End = want.End.AddDate(0, 0, 1)
			} else {
				want.Start = want.Start.Add(15 * time.Minute)
				want.End = want.End.Add(15 * time.Minute)
			}
			oldKey := graphContractEdit(t, ctx, c, cal, created.ID, before, want, beforeEdit)
			current := read(t, created.ID)
			checkGraphContractEvent(t, current, want)
			stale := want
			stale.Subject += " STALE MUST NOT WIN"
			if err := c.UpdateEvent(ctx, cal, created.ID, calendarsync.UpdateSpec{Event: stale, ETag: oldKey}); !errors.Is(err, calendarsync.ErrPrecondition) {
				t.Error("stale changeKey did not reject edit with ErrPrecondition")
			}
			checkGraphContractEvent(t, read(t, created.ID), want)
		})
	}
	t.Run("stale-delete-read-back", func(t *testing.T) {
		ev := calendar.Event{Subject: prefix + "stale-delete", Start: from.Add(time.Hour), End: from.Add(2 * time.Hour), TimeZone: "UTC"}
		got, err := c.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: calendartest.GraphTransactionID(ev.Subject)})
		if err != nil {
			cancel()
			t.Fatal("create failed")
		}
		deleted := false
		cleanup(t, got.ID, &deleted)
		got = read(t, got.ID)
		checkGraphContractEvent(t, got, ev)
		before := ev
		ev.Subject += " edited"
		oldKey := graphContractEdit(t, ctx, c, cal, got.ID, before, ev, got)
		err = c.DeleteEvent(ctx, cal, got.ID, oldKey, false)
		if err == nil {
			deleted = true
		}
		if !errors.Is(err, calendarsync.ErrPrecondition) {
			t.Error("stale changeKey did not reject DELETE with ErrPrecondition")
		}
		// A failed read is evidence too: do not hide an erroneously successful DELETE.
		current, err := c.GetEvent(ctx, cal, got.ID)
		if err != nil {
			t.Error("stale DELETE read-back event missing or unsafe")
			return
		}
		checkGraphContractEvent(t, current, ev)
	})
	for _, tc := range []struct {
		name, zone, start, moved string
		want                     []string
		providerStarts           []string
		providerDurations        []time.Duration
	}{
		{"Berlin-spring-move-cancel", "Europe/Berlin", "2026-03-22T09:00:00+01:00", "2026-03-30T12:00:00Z", []string{"2026-03-22T08:00:00Z", "2026-03-29T07:00:00Z", "2026-04-05T07:00:00Z", "2026-04-12T07:00:00Z"}, nil, nil},
		{"Berlin-autumn-move-cancel", "Europe/Berlin", "2026-10-18T09:00:00+02:00", "2026-10-26T12:00:00Z", []string{"2026-10-18T07:00:00Z", "2026-10-25T08:00:00Z", "2026-11-01T08:00:00Z", "2026-11-08T08:00:00Z"}, nil, nil},
		{"New-York-spring-move-cancel", "America/New_York", "2026-03-01T09:00:00-05:00", "2026-03-09T12:00:00Z", []string{"2026-03-01T14:00:00Z", "2026-03-08T13:00:00Z", "2026-03-15T13:00:00Z", "2026-03-22T13:00:00Z"}, nil, nil},
		// Genuine Graph recordings establish a different expansion from RFC
		// 5545: the gap consumes COUNT and yields a zero-duration instance;
		// the fold preserves endpoint clocks rather than elapsed duration.
		// Keep the independent RFC expectations for local/ICS below.
		{"Berlin-gap-series", "Europe/Berlin", "2026-03-22T02:30:00+01:00", "", []string{"2026-03-22T01:30:00Z", "2026-04-05T00:30:00Z", "2026-04-12T00:30:00Z"}, []string{"2026-03-22T01:30:00Z", "2026-03-29T01:30:00Z", "2026-04-05T00:30:00Z"}, []time.Duration{45 * time.Minute, 0, 45 * time.Minute}},
		{"Berlin-fold-series", "Europe/Berlin", "2026-10-18T02:30:00+02:00", "", []string{"2026-10-18T00:30:00Z", "2026-10-25T00:30:00Z", "2026-11-01T01:30:00Z"}, []string{"2026-10-18T00:30:00Z", "2026-10-25T00:30:00Z", "2026-11-01T01:30:00Z"}, []time.Duration{45 * time.Minute, 105 * time.Minute, 45 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := graphContractTime(t, tc.start)
			dir := t.TempDir()
			ev := calendar.Event{Subject: prefix + tc.name, Start: start, End: start.Add(45 * time.Minute), TimeZone: tc.zone,
				Recurrence: &calendar.Recurrence{Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"sunday"}}, Range: calendar.RecurrenceRange{Type: "numbered", StartDate: start.Format("2006-01-02"), TimeZone: tc.zone, NumberOfOccurrences: len(tc.want)}}}
			created := create(t, ev)
			checkGraphContractEvent(t, read(t, created.ID), ev)
			instances := graphContractInstances(t, ctx, c, cal, ev.Subject, from, to)
			if tc.providerStarts == nil {
				checkGraphContractTimes(t, "Graph", instances, tc.want)
			} else {
				checkGraphContractWindow(t, "Graph provider-specific expansion", instances, tc.providerStarts, tc.providerDurations)
			}
			checkGraphContractLocal(t, ctx, c, cal, created.ID, from, to, tc.want)
			providerStarts := tc.providerStarts
			if providerStarts == nil {
				providerStarts = tc.want
			}
			checkGraphContractSynced(t, ctx, c, cal, dir, from, to, providerStarts, tc.providerDurations)
			if tc.moved == "" {
				return
			}
			if len(instances) != len(tc.want) {
				t.Fatal("cannot safely identify mutation targets")
			}
			// calendarView intentionally does not select changeKey; GetEvent supplies it.
			moved := read(t, instances[1].ID)
			moved.Start = graphContractTime(t, tc.moved)
			moved.End = moved.Start.Add(45 * time.Minute)
			moved.Subject += " moved"
			if err := c.UpdateEvent(ctx, cal, moved.ID, calendarsync.UpdateSpec{Event: moved, ETag: moved.ETag}); err != nil {
				t.Fatal("move occurrence failed; inspect sanitized recording")
			}
			checkGraphContractEvent(t, read(t, moved.ID), moved)
			movedWant := append([]string(nil), tc.want...)
			movedWant[1] = tc.moved
			sort.Strings(movedWant)
			checkGraphContractTimes(t, "Graph after move", graphContractInstances(t, ctx, c, cal, ev.Subject, from, to), movedWant)
			checkGraphContractLocal(t, ctx, c, cal, created.ID, from, to, movedWant)
			checkGraphContractSynced(t, ctx, c, cal, dir, from, to, movedWant, nil)
			removed := read(t, instances[len(instances)-1].ID)
			if err := c.DeleteEvent(ctx, cal, removed.ID, removed.ETag, false); err != nil {
				t.Fatal("cancel occurrence failed; inspect sanitized recording")
			}
			want := append([]string(nil), movedWant[:len(movedWant)-1]...)
			checkGraphContractTimes(t, "Graph after cancel", graphContractInstances(t, ctx, c, cal, ev.Subject, from, to), want)
			checkGraphContractLocal(t, ctx, c, cal, created.ID, from, to, want)
			checkGraphContractSynced(t, ctx, c, cal, dir, from, to, want, nil)
		})
	}
	for _, tc := range []struct{ name, start, end, wantStart, wantEnd string }{
		{"raw-first-fold", "2026-10-25T02:15:00+02:00", "2026-10-25T02:45:00+02:00", "2026-10-25T00:15:00Z", "2026-10-25T00:45:00Z"},
		{"raw-second-fold", "2026-10-25T02:15:00+01:00", "2026-10-25T02:45:00+01:00", "2026-10-25T01:15:00Z", "2026-10-25T01:45:00Z"},
		{"raw-fold-crossing", "2026-10-25T02:30:00+02:00", "2026-10-25T02:00:00+01:00", "2026-10-25T00:30:00Z", "2026-10-25T01:00:00Z"},
		{"raw-spring-gap", "2026-03-29T02:30:00", "2026-03-29T03:15:00", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject := prefix + tc.name
			body := map[string]any{"subject": subject, "transactionId": calendartest.GraphTransactionID(subject), "start": map[string]string{"dateTime": tc.start, "timeZone": "Europe/Berlin"}, "end": map[string]string{"dateTime": tc.end, "timeZone": "Europe/Berlin"}}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal("encode probe")
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, calendartest.GraphBase+"/me/calendars/"+url.PathEscape(cal)+"/events", bytes.NewReader(data))
			if err != nil {
				t.Fatal("build probe")
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.httpClient.Do(req)
			if err != nil {
				cancel()
				t.Fatal("guarded raw probe failed; inspect sanitized recording")
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				if tc.wantStart == "" && resp.StatusCode == http.StatusBadRequest {
					t.Log("nonexistent spring endpoint rejected with HTTP 400")
					return
				}
				t.Fatalf("raw probe HTTP %d; inspect sanitized recording", resp.StatusCode)
			}
			var identity struct {
				ID string `json:"id"`
			}
			if json.NewDecoder(resp.Body).Decode(&identity) != nil || identity.ID == "" {
				t.Fatal("raw probe identity missing")
			}
			deleted := false
			cleanup(t, identity.ID, &deleted)
			got := read(t, identity.ID)
			t.Logf("raw probe resolved UTC %s / %s", got.Start.UTC().Format(time.RFC3339), got.End.UTC().Format(time.RFC3339))
			if tc.wantStart == "" {
				t.Error("Graph accepted nonexistent spring-gap endpoint rather than rejecting it")
				return
			}
			// These are characterization probes, NOT an accepted production
			// representation: Graph keeps the instant but loses original zones.
			// The separate validation control must continue rejecting fold writes.
			checkGraphContractEvent(t, got, calendar.Event{Subject: subject, Start: graphContractTime(t, tc.wantStart), End: graphContractTime(t, tc.wantEnd), TimeZone: "UTC"})
		})
	}
	t.Run("ambiguous-write-rejected", func(t *testing.T) {
		noIO := &graphContractNoIO{}
		validationClient := NewWithToken("", calendartest.GraphBase, "unused", &http.Client{Transport: noIO})
		for _, value := range []string{"2026-10-25T02:15:00+02:00", "2026-10-25T02:15:00+01:00"} {
			ev := calendar.Event{Subject: prefix + "ambiguous", Start: graphContractTime(t, value), TimeZone: "Europe/Berlin"}
			ev.End = ev.Start.Add(30 * time.Minute)
			_, createErr := validationClient.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: calendartest.GraphTransactionID(ev.Subject)})
			updateErr := validationClient.UpdateEvent(ctx, cal, "never-created", calendarsync.UpdateSpec{Event: ev})
			for _, err := range []error{createErr, updateErr} {
				if err == nil || !strings.Contains(err.Error(), "ambiguous calendar time") {
					t.Error("ambiguous endpoint was not rejected before I/O")
				}
			}
		}
		if noIO.calls != 0 {
			t.Error("ambiguous write attempted I/O")
		}
	})
	t.Run("floating-write-rejected", func(t *testing.T) {
		noIO := &graphContractNoIO{}
		validationClient := NewWithToken("", calendartest.GraphBase, "unused", &http.Client{Transport: noIO})
		ev := calendar.Event{Subject: prefix + "floating", Floating: true, Start: from.Add(time.Hour), End: from.Add(2 * time.Hour)}
		if _, err := validationClient.CreateEvent(ctx, cal, ev, calendarsync.CreateOptions{IdempotencyKey: calendartest.GraphTransactionID(ev.Subject)}); err == nil {
			t.Error("floating write silently converted to instant")
		}
		if noIO.calls != 0 {
			t.Error("floating write attempted I/O")
		}
	})
	t.Run("cleanup-read-back", func(t *testing.T) {
		if got := graphContractInstances(t, ctx, c, cal, prefix, from, to); len(got) != 0 {
			t.Errorf("cleanup left %d owned instances", len(got))
		}
		t.Logf("created %d master/single IDs; cleaned %d; no listing/prefix deletions", createdCount, cleanedCount)
		if cleanedCount != createdCount {
			t.Error("not all created IDs cleaned")
		}
	})
}

// Validation controls must never create remote objects even if validation
// regresses; they assert both the public error and zero outbound requests.
type graphContractNoIO struct{ calls int }

func (n *graphContractNoIO) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("validation control attempted I/O")
}

func graphContractTime(t *testing.T, value string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal("invalid expected time")
	}
	return got
}

// Graph may advance changeKey asynchronously after POST, even between GET
// and PATCH. A 412 is a correct rejection, not permission to overwrite. This
// setup helper re-reads and compares the scenario's entire editable content
// before at most two re-planned attempts; an actual change aborts the test.
// The separate stale-write/delete assertions never retry.
func graphContractEdit(t *testing.T, ctx context.Context, c *Client, cal, id string, before, after, current calendar.Event) string {
	t.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		checkGraphContractEvent(t, current, before)
		if t.Failed() || current.Description != before.Description || current.Location != before.Location ||
			current.IsCancelled || !reflect.DeepEqual(current.Recurrence, before.Recurrence) || current.ETag == "" {
			t.Fatal("edit precondition content changed; refusing to refresh the key")
		}
		err := c.UpdateEvent(ctx, cal, id, calendarsync.UpdateSpec{Event: after, ETag: current.ETag})
		if err == nil {
			return current.ETag
		}
		if !errors.Is(err, calendarsync.ErrPrecondition) || attempt == 2 {
			t.Fatal("conditional edit failed; inspect sanitized recording")
		}
		t.Log("Graph rejected a setup edit with 412; re-read and compare unchanged content before re-planning")
		time.Sleep(500 * time.Millisecond)
		current, err = c.GetEvent(ctx, cal, id)
		if err != nil {
			t.Fatal("conditional edit re-read failed")
		}
	}
	return "" // every terminal branch above returns or fails the test
}

func checkGraphContractEvent(t *testing.T, got, want calendar.Event) {
	t.Helper()
	if !got.Start.Equal(want.Start) || !got.End.Equal(want.End) || got.AllDay != want.AllDay || got.Subject != want.Subject {
		t.Errorf("read-back content/time differs: start=%s end=%s allDay=%v; want start=%s end=%s", got.Start, got.End, got.AllDay, want.Start, want.End)
	}
	endZone := want.EndTimeZone
	if endZone == "" {
		endZone = want.TimeZone
	}
	if !want.AllDay && (got.TimeZone != want.TimeZone || got.EndTimeZone != endZone) {
		t.Errorf("read-back zones %q/%q, want %q/%q", got.TimeZone, got.EndTimeZone, want.TimeZone, endZone)
	}
	if len(got.Attendees) != 0 || got.IsOnlineMeeting {
		t.Error("unexpected attendees or online meeting")
	}
}

func graphContractInstances(t *testing.T, ctx context.Context, c *Client, cal, subject string, from, to time.Time) []calendar.Event {
	t.Helper()
	all, err := c.FetchInstances(ctx, cal, from, to)
	if err != nil {
		t.Fatal("fetch instances failed; inspect sanitized recording")
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

func checkGraphContractTimes(t *testing.T, label string, events []calendar.Event, want []string) {
	t.Helper()
	durations := make([]time.Duration, len(want))
	for i := range durations {
		durations[i] = 45 * time.Minute
	}
	checkGraphContractWindow(t, label, events, want, durations)
}

func checkGraphContractWindow(t *testing.T, label string, events []calendar.Event, want []string, durations []time.Duration) {
	t.Helper()
	var got []string
	for i, ev := range events {
		got = append(got, ev.Start.UTC().Format(time.RFC3339))
		if i < len(durations) && ev.End.Sub(ev.Start) != durations[i] {
			t.Errorf("%s occurrence %d duration=%s, want %s", label, i, ev.End.Sub(ev.Start), durations[i])
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s starts=%v; want %v", label, got, want)
	}
}

func checkGraphContractLocal(t *testing.T, ctx context.Context, c *Client, cal, id string, from, to time.Time, want []string) {
	t.Helper()
	masters, err := c.FetchMasterEvents(ctx, cal)
	if err != nil {
		t.Fatal("fetch masters failed; inspect sanitized recording")
	}
	for _, master := range masters {
		if master.ID != id {
			continue
		}
		checkGraphContractTimes(t, "local expansion", calendar.ExpandOccurrences(master, from, to), want)
		ics, err := calendar.EventToICal(master)
		if err != nil {
			t.Fatal("iCalendar encoding failed")
		}
		parsed, err := calendar.ICalToEvent(ics, "")
		if err != nil {
			t.Fatal("iCalendar decoding failed")
		}
		checkGraphContractTimes(t, "iCalendar round-trip", calendar.ExpandOccurrences(parsed, from, to), want)
		return
	}
	t.Fatal("series master missing")
}

func checkGraphContractSynced(t *testing.T, ctx context.Context, c *Client, cal, dir string, from, to time.Time, want []string, durations []time.Duration) {
	t.Helper()
	events, err := calendartest.SyncOccurrences(ctx, c, cal, dir, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if durations == nil {
		checkGraphContractTimes(t, "synced provider occurrences", events, want)
	} else {
		checkGraphContractWindow(t, "synced provider occurrences", events, want, durations)
	}
	for _, event := range events {
		if event.OccurrenceSource != "provider" || !calendar.ToCalendarEvent("Durian Test", event, false).Recurring {
			t.Error("synced recurrence did not retain provider source/series identity")
		}
	}
}
