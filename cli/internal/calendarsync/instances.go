package calendarsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

const instancesFile = ".durian-calsync-instances.json"

// This is a disposable rendering cache, not sync state. In particular, no
// generated occurrence ever enters the vdir, planner, or remote mirror.
type occurrenceCache struct {
	Version   int                            `json:"version"`
	Mailbox   string                         `json:"mailbox"`
	Calendars map[string]calendarOccurrences `json:"calendars"`
}

type calendarOccurrences struct {
	Name      string                       `json:"name"`
	Dir       string                       `json:"dir"`
	From      time.Time                    `json:"from"`
	To        time.Time                    `json:"to"`
	Generated time.Time                    `json:"generated"`
	Series    map[string]seriesOccurrences `json:"series"` // local master UID
}

type seriesOccurrences struct {
	Baseline ItemStatus       `json:"baseline"` // includes remote ID and raw local hash
	Events   []calendar.Event `json:"events"`
}

// loadOccurrences is deliberately read-only, including for corrupt/missing
// files. GETs must work offline and must never create locks or repair files.
func loadOccurrences(dir string) *occurrenceCache {
	var cache occurrenceCache
	data, err := os.ReadFile(filepath.Join(dir, instancesFile))
	if err != nil || json.Unmarshal(data, &cache) != nil || cache.Version != 1 || cache.Calendars == nil {
		return &occurrenceCache{Version: 1, Calendars: make(map[string]calendarOccurrences)}
	}
	return &cache
}

func recurring(e calendar.Event) bool {
	return e.Recurrence != nil || e.OpaqueRecurrence || e.RecurrenceRule != ""
}

func boundDir(dir string) bool {
	return dir != "" && dir != "." && dir != ".." && filepath.Base(dir) == dir
}

// RefreshOccurrences refreshes only the calendars in applied, after ApplyAll
// AND a successful state Save, while the caller still holds the account run
// lock. It never touches mirror cursors (including on targeted GUI syncs).
// Failures leave the last snapshot in place and cannot roll back sync writes.
// The bool reports a published cache change, for calendar_updated broadcasts.
func RefreshOccurrences(ctx context.Context, p CalendarProvider, accountDir string, state *SyncState, applied []CalendarPlan, now time.Time) (bool, error) {
	cache := loadOccurrences(accountDir)
	if cache.Mailbox != state.Mailbox {
		cache = &occurrenceCache{Version: 1, Mailbox: state.Mailbox, Calendars: make(map[string]calendarOccurrences)}
	}
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from, to := month.AddDate(0, -12, 0), month.AddDate(0, 18, 0)
	changed := false
	var failures []error
	seen := make(map[string]bool)
	for _, plan := range applied {
		id := plan.Calendar.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		status, exists := state.Calendars[id]
		if !exists || plan.Removed {
			if _, exists := cache.Calendars[id]; exists {
				delete(cache.Calendars, id)
				changed = true
			}
			continue
		}
		if state.Mailbox == "" || !boundDir(status.Dir) {
			continue // no trustworthy saved binding yet
		}
		items, unreadable, err := calendar.ScanLocalItems(filepath.Join(accountDir, status.Dir), p.Owner())
		if err != nil || len(unreadable) != 0 {
			failures = append(failures, fmt.Errorf("cannot capture local occurrence baseline"))
			continue
		}
		candidate := calendarOccurrences{Name: status.Name, Dir: status.Dir, From: from, To: to, Generated: now, Series: make(map[string]seriesOccurrences)}
		parents := make(map[string]string)
		for uid, baseline := range status.Items {
			if baseline.RemoteID != "" {
				parents[baseline.RemoteID] = uid
			}
			item, exists := items[uid]
			if exists && recurring(item.Event) && baseline.RemoteID != "" && baseline.RemoteHash != "" && baseline.LocalHash == item.Hash {
				candidate.Series[uid] = seriesOccurrences{Baseline: baseline}
			}
		}
		old, exists := cache.Calendars[id]
		if exists && old.Name == candidate.Name && old.Dir == candidate.Dir && old.From.Equal(from) && old.To.Equal(to) &&
			!old.Generated.IsZero() && !now.Before(old.Generated) && now.Sub(old.Generated) < 24*time.Hour && sameOccurrenceBaselines(old.Series, candidate.Series) {
			continue
		}
		if len(candidate.Series) > 0 {
			if err := fetchOccurrences(ctx, p, id, parents, &candidate); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		cache.Calendars[id] = candidate
		changed = true
	}
	if changed {
		data, err := json.Marshal(cache)
		if err != nil {
			return false, fmt.Errorf("marshal occurrence cache: %w", err)
		}
		if err := calendar.WriteFileAtomic(filepath.Join(accountDir, instancesFile), data, 0o600); err != nil {
			return false, fmt.Errorf("save occurrence cache: %w", err)
		}
	}
	return changed, errors.Join(failures...)
}

func sameOccurrenceBaselines(a, b map[string]seriesOccurrences) bool {
	if len(a) != len(b) {
		return false
	}
	for uid, series := range a {
		other, exists := b[uid]
		if !exists || series.Baseline != other.Baseline {
			return false
		}
	}
	return true
}

func fetchOccurrences(ctx context.Context, p CalendarProvider, id string, parents map[string]string, snapshot *calendarOccurrences) error {
	instances, err := p.FetchInstances(ctx, id, snapshot.From, snapshot.To)
	if err != nil {
		return fmt.Errorf("fetch provider occurrences: %w", err)
	}
	for _, instance := range instances {
		if instance.SeriesMasterID == "" && instance.Type == "singleInstance" {
			continue
		}
		uid, known := parents[instance.SeriesMasterID]
		if instance.SeriesMasterID == "" || !known {
			return fmt.Errorf("provider occurrence has missing or unknown parent")
		}
		series, eligible := snapshot.Series[uid]
		if !eligible {
			continue // a known parent with pending local edits, for example
		}
		if instance.Start.IsZero() || instance.End.Before(instance.Start) || instance.AllDay && !instance.End.After(instance.Start) {
			return fmt.Errorf("provider occurrence has invalid boundaries")
		}
		if instance.IsCancelled {
			continue
		}
		instance.ICalUID = uid // detail/edit/delete still address the local series
		instance.Recurrence, instance.Overrides, instance.ExceptionDates = nil, nil, nil
		instance.RecurrenceRule = ""
		instance.OccurrenceSource = "provider"
		series.Events = append(series.Events, instance)
		snapshot.Series[uid] = series
	}
	// A remote edit between the sync snapshot and calendarView must not attach
	// new instances to an old local master. Use full masters (with exceptions),
	// not GetEvent, whose shape need not include the series exception set.
	masters, err := p.FetchMasterEvents(ctx, id)
	if err != nil {
		return fmt.Errorf("verify occurrence masters: %w", err)
	}
	verified := make(map[string]bool)
	for _, master := range masters {
		uid := parents[master.ID]
		series, exists := snapshot.Series[uid]
		remoteUID := master.ICalUID
		if remoteUID == "" {
			remoteUID = master.ID
		}
		if exists && remoteUID == uid && recurring(master) && calendar.EventContentHash(master, p.Owner()) == series.Baseline.RemoteHash {
			verified[uid] = true
		}
	}
	rejected := 0
	for uid := range snapshot.Series {
		if !verified[uid] {
			delete(snapshot.Series, uid) // fail closed for this series, not its neighbors
			rejected++
		}
	}
	if rejected > 0 {
		slog.Warn("Provider masters changed during occurrence refresh; using local estimates", "module", "CALSYNC", "series", rejected)
	}
	return nil
}

// ReadOccurrences is the shared CLI/HTTP offline rendering path. Only a
// fully covered query with matching mailbox, calendar binding, entire saved
// item baseline, AND current raw ICS bytes can use provider expansion. Cache
// age alone does not invalidate an offline snapshot; sync refreshes it daily.
// Pending/new local edits use RFC expansion and are explicitly estimates for
// remote collections. Known local-only collections always remain RFC-native.
func ReadOccurrences(cal calendar.LocalCalendar, from, to time.Time) []calendar.Event {
	accountDir := filepath.Dir(cal.Dir)
	state, stateErr := NewFileStateStore(accountDir).LoadReadOnly()
	cache := loadOccurrences(accountDir)
	var snapshot calendarOccurrences
	var status CalendarStatus
	remote := !strings.EqualFold(cal.Account, "local") && (cal.Account != "" || cal.Owner != "" || stateErr == nil && len(state.Calendars) > 0)
	if remote && stateErr == nil && state.Mailbox != "" && cache.Mailbox == state.Mailbox && (cal.Owner == "" || strings.EqualFold(cal.Owner, state.Mailbox)) {
		for id, binding := range state.Calendars {
			if boundDir(binding.Dir) && binding.Dir == filepath.Base(cal.Dir) {
				status = binding
				snapshot = cache.Calendars[id]
				if snapshot.Dir != binding.Dir || snapshot.Name != binding.Name || snapshot.Generated.IsZero() || !snapshot.From.Before(snapshot.To) {
					snapshot = calendarOccurrences{}
				}
				break
			}
		}
	}
	// Re-scan instead of trusting an earlier ReadCollections: a concurrent
	// local delete must never be resurrected from either cache or old masters.
	items, _, scanErr := calendar.ScanLocalItems(cal.Dir, cal.Owner)
	events := cal.Events
	if scanErr == nil {
		events = nil
		for _, item := range items {
			events = append(events, item.Event)
		}
	}
	var out []calendar.Event
	for _, master := range events {
		if recurring(master) && remote {
			series, exists := snapshot.Series[master.ICalUID]
			baseline, tracked := status.Items[master.ICalUID]
			item := items[master.ICalUID]
			covered := !from.Before(snapshot.From) && !to.After(snapshot.To)
			if master.AllDay {
				covered = covered && !calendar.CivilTime(from).Before(snapshot.From) && !calendar.CivilTime(to).After(snapshot.To)
			}
			if scanErr == nil && exists && tracked && covered && baseline.RemoteID != "" && baseline == series.Baseline && item.Hash == baseline.LocalHash {
				for _, instance := range series.Events {
					if instance.SeriesMasterID != baseline.RemoteID || instance.ICalUID != master.ICalUID || instance.OccurrenceSource != "provider" {
						exists = false
						break
					}
				}
				if exists {
					for _, instance := range series.Events {
						out = append(out, calendar.ExpandOccurrences(instance, from, to)...)
					}
					continue // an empty provider result is meaningful (COUNT/gaps)
				}
			}
			for _, instance := range calendar.ExpandOccurrences(master, from, to) {
				instance.OccurrenceSource = "local-estimate"
				out = append(out, instance)
			}
			continue
		}
		out = append(out, calendar.ExpandOccurrences(master, from, to)...)
	}
	if stateErr == nil {
		col := calendar.Collection{Dir: cal.Dir, Account: cal.Account, Owner: cal.Owner}
		for i := range out {
			out[i] = overlayOrganizerRole(col, state, out[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// ReadOrganizerRole restores provider-confirmed organizer authority for a
// resolved local event. A secondary calendar's ORGANIZER need not be the
// account email. Only the saved mailbox/collection/UID binding is trusted;
// this read never repairs state or rewrites ICS, and is not a sync planner gate.
// path must name the resolved event file or its exact collection directory.
func ReadOrganizerRole(cols []calendar.Collection, path string, event calendar.Event) calendar.Event {
	for _, col := range cols {
		if filepath.Clean(col.Dir) != filepath.Clean(path) && filepath.Clean(col.Dir) != filepath.Dir(path) {
			continue
		}
		state, err := NewFileStateStore(filepath.Dir(filepath.Clean(col.Dir))).LoadReadOnly()
		if err == nil {
			return overlayOrganizerRole(col, state, event)
		}
		return event
	}
	return event
}

func overlayOrganizerRole(col calendar.Collection, state *SyncState, event calendar.Event) calendar.Event {
	if strings.EqualFold(col.Account, "local") || col.Owner == "" || state.Mailbox == "" ||
		!strings.EqualFold(col.Owner, state.Mailbox) || event.ICalUID == "" {
		return event
	}
	var binding *CalendarStatus
	for id, status := range state.Calendars {
		if !boundDir(status.Dir) || status.Dir != filepath.Base(filepath.Clean(col.Dir)) {
			continue
		}
		if binding != nil || id == "" {
			return event // ambiguous or missing remote collection identity
		}
		binding = &status
	}
	if binding == nil {
		return event
	}
	item, tracked := binding.Items[event.ICalUID]
	if !tracked || item.RemoteID == "" || item.OwnerResponse != calendar.OwnerRespOrganizer {
		return event
	}
	event.IsOrganizer = true
	// Do not replay saved RSVP baselines over local PARTSTAT: those responses
	// may be pending writes. Only recover the organizer role, not attendee RSVP.
	switch event.OwnerResponse {
	case calendar.OwnerRespAccepted, calendar.OwnerRespTentative, calendar.OwnerRespDeclined:
	default:
		event.OwnerResponse = calendar.OwnerRespOrganizer
	}
	return event
}
