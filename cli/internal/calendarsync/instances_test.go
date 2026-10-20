package calendarsync_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

type occurrenceProvider struct {
	*fakeProvider
	instances  []Event
	err        error
	calls      int
	from, to   time.Time
	afterFetch func()
}

func (p *occurrenceProvider) FetchInstances(_ context.Context, _ string, from, to time.Time) ([]Event, error) {
	p.calls++
	p.from, p.to = from, to
	if p.afterFetch != nil {
		p.afterFetch()
	}
	return p.instances, p.err
}

func occurrenceTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func gapMaster() Event {
	e := remoteEvent("parent", "local-series", "Gap series")
	e.Start = occurrenceTime("2026-03-22T02:30:00+01:00")
	e.End = e.Start.Add(45 * time.Minute)
	e.TimeZone, e.EndTimeZone, e.Type = "Europe/Berlin", "Europe/Berlin", "seriesMaster"
	e.Recurrence = &calendar.Recurrence{
		Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1, DaysOfWeek: []string{"sunday"}},
		Range:   calendar.RecurrenceRange{Type: "numbered", StartDate: "2026-03-22", NumberOfOccurrences: 3, TimeZone: "Europe/Berlin"},
	}
	return e
}

func providerOccurrence(master Event, start string, duration time.Duration) Event {
	e := master
	e.ID, e.ICalUID = "instance-"+start, "provider-instance-specific-uid"
	e.SeriesMasterID, e.Type = master.ID, "occurrence"
	e.Start = occurrenceTime(start)
	e.End = e.Start.Add(duration)
	e.Recurrence = nil
	return e
}

func seedOccurrences(t *testing.T, masters, instances []Event) (*occurrenceProvider, string, *calendarsync.SyncState, []calendarsync.CalendarPlan) {
	t.Helper()
	p := &occurrenceProvider{fakeProvider: &fakeProvider{events: masters}, instances: instances}
	dir := t.TempDir()
	state := &calendarsync.SyncState{Mailbox: p.Owner()}
	plans, err := calendarsync.PlanAll(context.Background(), p, dir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), p, state, plans, calendarsync.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := calendarsync.NewFileStateStore(dir).Save(state); err != nil {
		t.Fatal(err)
	}
	return p, dir, state, plans
}

func occurrenceCalendar(t *testing.T, dir string) calendar.LocalCalendar {
	t.Helper()
	cols, err := calendar.CollectionsUnderFor(dir, "work", testOwnerEmail)
	if err != nil {
		t.Fatal(err)
	}
	cals, err := calendar.ReadCollections(cols)
	if err != nil || len(cals) != 1 {
		t.Fatalf("read calendars: %v (%d)", err, len(cals))
	}
	return cals[0]
}

func refreshOccurrences(t *testing.T, p *occurrenceProvider, dir string, state *calendarsync.SyncState, plans []calendarsync.CalendarPlan, now time.Time) bool {
	t.Helper()
	changed, err := calendarsync.RefreshOccurrences(context.Background(), p, dir, state, plans, now)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestProviderOccurrenceCacheOfflineAndCadence(t *testing.T) {
	master := gapMaster()
	instances := []Event{providerOccurrence(master, "2026-03-22T01:30:00Z", 45*time.Minute), providerOccurrence(master, "2026-04-05T00:30:00Z", 45*time.Minute)}
	p, dir, state, plans := seedOccurrences(t, []Event{master}, instances)
	now := occurrenceTime("2026-03-20T12:00:00Z")
	from, to := occurrenceTime("2026-03-01T00:00:00Z"), occurrenceTime("2026-05-01T00:00:00Z")
	cal := occurrenceCalendar(t, dir)
	if got := calendarsync.ReadOccurrences(cal, from, to); len(got) != 3 || got[0].OccurrenceSource != "local-estimate" {
		t.Fatalf("RFC fallback: %+v", got)
	}
	if refreshOccurrences(t, p, dir, state, nil, now) || p.calls != 0 {
		t.Fatal("refresh read a calendar outside the applied scope")
	}
	if !refreshOccurrences(t, p, dir, state, plans, now) {
		t.Fatal("missing cache did not refresh")
	}
	if !p.from.Equal(occurrenceTime("2025-03-01T00:00:00Z")) || !p.to.Equal(occurrenceTime("2027-09-01T00:00:00Z")) {
		t.Fatalf("unbounded or unaligned fetch: %s to %s", p.from, p.to)
	}
	path := filepath.Join(dir, ".durian-calsync-instances.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache permissions: %v %v", info, err)
	}
	got := calendarsync.ReadOccurrences(occurrenceCalendar(t, dir), from, to)
	if len(got) != 2 {
		t.Fatalf("provider gap must consume COUNT, got %d", len(got))
	}
	for _, e := range got {
		dto := calendar.ToCalendarEvent("Work", e, false)
		if dto.OccurrenceSource != "provider" || !dto.Recurring || dto.UID != master.ICalUID || e.Recurrence != nil {
			t.Fatalf("provider identity/source: %+v", dto)
		}
	}
	if refreshOccurrences(t, p, dir, state, plans, now.Add(time.Minute)) || p.calls != 1 {
		t.Fatal("minute autosync reread instances")
	}
	p.err = errors.New("offline")
	if got := calendarsync.ReadOccurrences(occurrenceCalendar(t, dir), from, to); len(got) != 2 || p.calls != 1 {
		t.Fatal("offline read used network or lost cache")
	}
	before, _ := os.ReadFile(path)
	if _, err := calendarsync.RefreshOccurrences(context.Background(), p, dir, state, plans, now.Add(24*time.Hour)); err == nil {
		t.Fatal("expected failed daily refresh")
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed refresh replaced valid snapshot")
	}
	p.err = nil
	refreshOccurrences(t, p, dir, state, plans, now.Add(24*time.Hour))
	refreshOccurrences(t, p, dir, state, plans, occurrenceTime("2026-04-01T00:00:00Z"))
	if p.calls != 4 || !p.from.Equal(occurrenceTime("2025-04-01T00:00:00Z")) {
		t.Fatal("daily/month rollover refresh missing")
	}
	items, _, err := calendar.ScanLocalItems(cal.Dir, p.Owner())
	if err != nil || len(items) != 1 {
		t.Fatal("generated instances entered ICS scan")
	}
	local := cal
	local.Account = "local"
	if got := calendarsync.ReadOccurrences(local, from, to); len(got) != 3 || got[0].OccurrenceSource != "" {
		t.Fatal("local-only calendar stopped being RFC-native")
	}
}

func TestProviderOccurrencePendingEditsDeleteAndUpload(t *testing.T) {
	master := gapMaster()
	single := remoteEvent("single", "single", "Ordinary")
	single.Start, single.End = master.Start, master.End
	p, dir, state, plans := seedOccurrences(t, []Event{master, single}, []Event{providerOccurrence(master, "2026-03-22T01:30:00Z", 45*time.Minute)})
	now := occurrenceTime("2026-03-20T12:00:00Z")
	refreshOccurrences(t, p, dir, state, plans, now)
	from, to := occurrenceTime("2026-03-01T00:00:00Z"), occurrenceTime("2026-05-01T00:00:00Z")
	cal := occurrenceCalendar(t, dir)
	master.Subject = "Local edit"
	path := writeLocalICS(t, cal.Dir, master)
	assertEstimate := func() {
		t.Helper()
		got := calendarsync.ReadOccurrences(cal, from, to)
		var recurringCount, singles int
		for _, e := range got {
			if e.ICalUID == master.ICalUID {
				recurringCount++
				if e.OccurrenceSource != "local-estimate" || e.Subject != master.Subject {
					t.Fatalf("stale cache eligible: %+v", e)
				}
			} else {
				singles++
				if e.OccurrenceSource != "" {
					t.Fatal("ordinary event marked estimated")
				}
			}
		}
		if recurringCount != 3 || singles != 1 {
			t.Fatalf("lost RFC fallback or ordinary event: %+v", got)
		}
	}
	assertEstimate()
	// Public sync uploads the edit and records a NEW saved baseline. Merely
	// matching that baseline to the file must not re-qualify the old cache.
	if _, err := calendarsync.SyncAll(context.Background(), p, dir, nil, state, calendarsync.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.updateSpecs) != 1 {
		t.Fatal("edit was not uploaded")
	}
	if err := calendarsync.NewFileStateStore(dir).Save(state); err != nil {
		t.Fatal(err)
	}
	assertEstimate()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got := calendarsync.ReadOccurrences(cal, from, to) // deliberately stale LocalCalendar
	if len(got) != 1 || got[0].ICalUID != single.ICalUID {
		t.Fatalf("deleted master resurrected: %+v", got)
	}
	master.ICalUID = "new-local-series"
	writeLocalICS(t, cal.Dir, master)
	got = calendarsync.ReadOccurrences(cal, from, to)
	if len(got) != 4 {
		t.Fatal("new local series lost")
	}
	for _, e := range got {
		if e.ICalUID == master.ICalUID && e.OccurrenceSource != "local-estimate" {
			t.Fatal("new local series treated as provider truth")
		}
	}
}

func TestProviderOccurrenceCacheFallbacks(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "out-of-range", "mailbox", "binding", "baseline", "raw-bytes"} {
		t.Run(mode, func(t *testing.T) {
			master := gapMaster()
			p, dir, state, plans := seedOccurrences(t, []Event{master}, []Event{providerOccurrence(master, "2026-03-22T01:30:00Z", 45*time.Minute)})
			refreshOccurrences(t, p, dir, state, plans, occurrenceTime("2026-03-20T12:00:00Z"))
			from, to := occurrenceTime("2026-03-01T00:00:00Z"), occurrenceTime("2026-05-01T00:00:00Z")
			path := filepath.Join(dir, ".durian-calsync-instances.json")
			switch mode {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "out-of-range":
				to = occurrenceTime("2028-01-01T00:00:00Z")
			case "raw-bytes":
				icsPath := filepath.Join(plans[0].Dir, calendar.EventFileName(master.ICalUID))
				data, err := os.ReadFile(icsPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(icsPath, append(data, '\r', '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			case "mailbox":
				state.Mailbox = "other@example.invalid"
			case "binding":
				c := state.Calendars["cal1"]
				c.Name = "Rebound"
				state.Calendars["cal1"] = c
			case "baseline":
				c := state.Calendars["cal1"]
				b := c.Items[master.ICalUID]
				b.OwnerResponse = calendar.OwnerRespAccepted
				c.Items[master.ICalUID] = b
			}
			if err := calendarsync.NewFileStateStore(dir).Save(state); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir(dir)
			got := calendarsync.ReadOccurrences(occurrenceCalendar(t, dir), from, to)
			if len(got) != 3 || got[0].OccurrenceSource != "local-estimate" {
				t.Fatalf("fallback %s: %+v", mode, got)
			}
			after, _ := os.ReadDir(dir)
			if len(before) != len(after) || p.calls != 1 {
				t.Fatal("read wrote files or used network")
			}
		})
	}
}

func TestProviderOccurrenceIdentityAndReadback(t *testing.T) {
	for _, mode := range []string{"missing-parent", "unknown-parent", "remote-edit"} {
		t.Run(mode, func(t *testing.T) {
			master := gapMaster()
			other := master
			other.ID, other.ICalUID = "other-parent", "other-series"
			instance := providerOccurrence(master, "2026-03-22T01:30:00Z", 45*time.Minute)
			if mode == "missing-parent" {
				instance.SeriesMasterID = ""
			}
			if mode == "unknown-parent" {
				instance.SeriesMasterID = "unknown"
			}
			p, dir, state, plans := seedOccurrences(t, []Event{master, other}, []Event{instance, providerOccurrence(other, "2026-03-22T01:30:00Z", 45*time.Minute)})
			if mode == "remote-edit" {
				p.afterFetch = func() { p.events[0].Subject = "Edited remotely during fetch" }
			}
			_, err := calendarsync.RefreshOccurrences(context.Background(), p, dir, state, plans, occurrenceTime("2026-03-20T12:00:00Z"))
			if mode != "remote-edit" && err == nil {
				t.Fatal("missing/unknown parent accepted by UID heuristic")
			}
			got := calendarsync.ReadOccurrences(occurrenceCalendar(t, dir), occurrenceTime("2026-03-01T00:00:00Z"), occurrenceTime("2026-05-01T00:00:00Z"))
			var estimated, provider int
			for _, e := range got {
				if e.OccurrenceSource == "provider" {
					provider++
				} else if e.OccurrenceSource == "local-estimate" {
					estimated++
				}
			}
			if mode == "remote-edit" {
				if estimated != 3 || provider != 1 {
					t.Fatalf("race affected neighboring series: %+v", got)
				}
			} else if estimated != 6 || provider != 0 {
				t.Fatal("bad parent did not fail snapshot closed")
			}
		})
	}
}

func TestProviderOccurrenceZeroDurationFoldAndCivilDates(t *testing.T) {
	master := gapMaster()
	day := master
	day.ID, day.ICalUID, day.AllDay = "day-parent", "day-series", true
	day.Start, day.End = occurrenceTime("2026-03-28T00:00:00Z"), occurrenceTime("2026-03-30T00:00:00Z")
	day.TimeZone, day.EndTimeZone = "", ""
	day.Recurrence = &calendar.Recurrence{Pattern: calendar.RecurrencePattern{Type: "weekly", Interval: 1}, Range: calendar.RecurrenceRange{Type: "numbered", StartDate: "2026-03-28", NumberOfOccurrences: 1}}
	travel := remoteEvent("travel", "travel", "Travel")
	travel.Start, travel.End = occurrenceTime("2026-03-28T18:00:00+01:00"), occurrenceTime("2026-03-28T21:00:00-04:00")
	travel.TimeZone, travel.EndTimeZone = "Europe/Berlin", "America/New_York"
	instances := []Event{providerOccurrence(master, "2026-03-29T01:30:00Z", 0), providerOccurrence(master, "2026-10-25T00:30:00Z", 105*time.Minute), providerOccurrence(day, "2026-03-28T00:00:00Z", 48*time.Hour)}
	p, dir, state, plans := seedOccurrences(t, []Event{master, day, travel}, instances)
	refreshOccurrences(t, p, dir, state, plans, occurrenceTime("2026-03-20T12:00:00Z"))
	cal := occurrenceCalendar(t, dir)
	zero := calendarsync.ReadOccurrences(cal, occurrenceTime("2026-03-29T01:30:00Z"), occurrenceTime("2026-03-29T01:31:00Z"))
	var found bool
	for _, e := range zero {
		if e.ICalUID == master.ICalUID {
			found = e.Start.Equal(e.End) && e.OccurrenceSource == "provider"
		}
	}
	if !found {
		t.Fatal("Graph zero-duration gap rejected at query start")
	}
	fold := calendarsync.ReadOccurrences(cal, occurrenceTime("2026-10-25T00:00:00Z"), occurrenceTime("2026-10-26T00:00:00Z"))
	if len(fold) != 1 || fold[0].End.Sub(fold[0].Start) != 105*time.Minute {
		t.Fatalf("Graph fold duration re-expanded: %+v", fold)
	}
	got := calendarsync.ReadOccurrences(cal, occurrenceTime("2026-03-28T00:00:00-07:00"), occurrenceTime("2026-03-29T00:00:00-07:00"))
	// The gap point also lies inside this offset query's absolute window.
	if len(got) != 3 {
		t.Fatalf("travel/civil date window changed: %+v", got)
	}
	for _, e := range got {
		if e.ICalUID == day.ICalUID && (!e.AllDay || !e.Start.Equal(day.Start) || !e.End.Equal(day.End)) {
			t.Fatal("all-day dates shifted")
		}
		if e.ICalUID == travel.ICalUID && (e.TimeZone != travel.TimeZone || e.EndTimeZone != travel.EndTimeZone || !e.Start.Equal(travel.Start) || !e.End.Equal(travel.End)) {
			t.Fatal("travel instants/zones changed")
		}
	}
}

func TestReadOrganizerRoleBindingAndPendingRSVP(t *testing.T) {
	for _, tc := range []struct {
		name, mailbox, dir, uid, remoteID, account, owner, response, stateFile string
		baseline                                                               calendar.OwnerResp
		wantOrganizer                                                          bool
	}{
		{"secondary organizer", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, true},
		{"case insensitive mailbox", "ME@EXAMPLE.COM", "Secondary", "uid", "remote", "work", "me@example.com", "", "valid", calendar.OwnerRespOrganizer, true},
		{"wrong mailbox", "other@example.com", "Secondary", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"missing mailbox", "", "Secondary", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"missing owner", testOwnerEmail, "Secondary", "uid", "remote", "work", "", "", "valid", calendar.OwnerRespOrganizer, false},
		{"wrong collection", testOwnerEmail, "Other", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"unsafe collection", testOwnerEmail, "../Secondary", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"missing collection", testOwnerEmail, "", "uid", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"wrong UID", testOwnerEmail, "Secondary", "different", "remote", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"missing remote ID", testOwnerEmail, "Secondary", "uid", "", "work", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"local only", testOwnerEmail, "Secondary", "uid", "remote", "local", testOwnerEmail, "", "valid", calendar.OwnerRespOrganizer, false},
		{"missing state", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "", "missing", calendar.OwnerRespOrganizer, false},
		{"corrupt state", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "", "corrupt", calendar.OwnerRespOrganizer, false},
		{"accepted attendee", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "accepted", "valid", calendar.OwnerRespAccepted, false},
		{"pending declined", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "declined", "valid", calendar.OwnerRespAccepted, false},
		{"pending accepted with organizer baseline", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "accepted", "valid", calendar.OwnerRespOrganizer, true},
		{"pending tentative with organizer baseline", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "tentativelyAccepted", "valid", calendar.OwnerRespOrganizer, true},
		{"pending declined with organizer baseline", testOwnerEmail, "Secondary", "uid", "remote", "work", testOwnerEmail, "declined", "valid", calendar.OwnerRespOrganizer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accountDir := t.TempDir()
			col := calendar.Collection{Dir: filepath.Join(accountDir, "Secondary"), Account: tc.account, Owner: tc.owner}
			if err := os.MkdirAll(col.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(accountDir, ".durian-calsync-state.json")
			state := &calendarsync.SyncState{Mailbox: tc.mailbox, Calendars: map[string]calendarsync.CalendarStatus{
				"secondary": {Name: "Renamed calendar", Dir: tc.dir, Items: map[string]calendarsync.ItemStatus{
					tc.uid: {RemoteID: tc.remoteID, OwnerResponse: tc.baseline},
				}},
			}}
			switch tc.stateFile {
			case "valid":
				if err := calendarsync.NewFileStateStore(accountDir).Save(state); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(statePath, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			event := calendar.Event{ICalUID: "uid", Subject: "Secondary", Start: occurrenceTime("2026-08-07T09:00:00Z"), End: occurrenceTime("2026-08-07T10:00:00Z"), Organizer: &calendar.Person{Email: "secondary@group.calendar.google.com"}}
			if tc.response != "" {
				event.Attendees = []calendar.Attendee{{Email: tc.owner, Response: tc.response, Type: "required"}}
			}
			data, err := calendar.EventToICal(event)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(col.Dir, "legacy-name.ics")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			parsed, err := calendar.ICalToEvent(data, col.Owner)
			if err != nil {
				t.Fatal(err)
			}
			want := parsed
			want.IsOrganizer = tc.wantOrganizer
			if tc.wantOrganizer && tc.response == "" {
				want.OwnerResponse = calendar.OwnerRespOrganizer
			}
			before, _ := os.ReadDir(accountDir)
			stateBefore, _ := os.ReadFile(statePath)
			for _, resolvedPath := range []string{path, col.Dir} {
				got := calendarsync.ReadOrganizerRole([]calendar.Collection{col}, resolvedPath, parsed)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("role for %q = %+v, want %+v", resolvedPath, got, want)
				}
			}
			outside := calendarsync.ReadOrganizerRole([]calendar.Collection{col}, filepath.Join(accountDir, "Other", "event.ics"), parsed)
			if !reflect.DeepEqual(outside, parsed) {
				t.Error("event outside collection was promoted")
			}
			cals, err := calendar.ReadCollections([]calendar.Collection{col})
			if err != nil || len(cals) != 1 {
				t.Fatalf("read collection: %v, %+v", err, cals)
			}
			got := calendarsync.ReadOccurrences(cals[0], event.Start, event.End)
			if len(got) != 1 || got[0].IsOrganizer != want.IsOrganizer || got[0].OwnerResponse != want.OwnerResponse {
				t.Errorf("summary role disagrees with detail: %+v, want %+v", got, want)
			}
			after, _ := os.ReadDir(accountDir)
			stateAfter, _ := os.ReadFile(statePath)
			icsAfter, _ := os.ReadFile(path)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(stateBefore, stateAfter) || !reflect.DeepEqual(data, icsAfter) {
				t.Error("offline role reads changed state or ICS")
			}
		})
	}
}

func TestOccurrenceCacheIsNotSyncState(t *testing.T) {
	master := gapMaster()
	p := &occurrenceProvider{fakeProvider: &fakeProvider{events: []Event{master}}}
	dir := t.TempDir()
	state := &calendarsync.SyncState{Mailbox: p.Owner()}
	plans, err := calendarsync.PlanAll(context.Background(), p, dir, nil, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := calendarsync.ApplyAll(context.Background(), p, state, plans, calendarsync.SyncOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".durian-calsync-instances.json")); !os.IsNotExist(err) || p.calls != 0 {
		t.Fatal("planning/dry-run fetched or wrote occurrence cache")
	}
	before, err := calendar.EventToICal(master)
	if err != nil {
		t.Fatal(err)
	}
	transient := master
	transient.SeriesMasterID, transient.OccurrenceSource = "parent", "provider"
	after, err := calendar.EventToICal(transient)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || calendar.EventContentHash(master, p.Owner()) != calendar.EventContentHash(transient, p.Owner()) || calendar.CoreContentHash(master, p.Owner()) != calendar.CoreContentHash(transient, p.Owner()) {
		t.Fatal("read-only occurrence metadata entered ICS/sync hashes")
	}
}
