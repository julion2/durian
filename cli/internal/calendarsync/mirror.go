// Remote mirror for the incremental download path: the last known state of
// each remote calendar, plus the opaque cursor that resumes the provider's
// change feed where the previous round stopped.
//
// Why a mirror exists at all. The planner needs the WHOLE remote calendar —
// the decision matrix asks "does this UID still exist remotely", conditional
// writes need the current etag, and the owner-RSVP sub-matrix reads the remote
// attendee list. A change feed only reports what MOVED, so it cannot answer
// any of those on its own. Keeping the last full picture and folding each
// round of changes into it turns an incremental feed back into the complete
// view the planner already knows how to work with — no rail, no classification
// and no test in twosync.go has to change.
//
// The mirror is a cache, never a source of truth: the vdir holds the converged
// state and the state file holds the baselines. Deleting the mirror costs one
// full download, nothing else, which is exactly what makes it safe to discard
// whenever the provider says its cursor is no longer usable.

package calendarsync

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CalendarMirror is the last known remote state of one calendar.
type CalendarMirror struct {
	// Cursor is the provider's opaque resume token (a Graph deltaLink, a
	// Google syncToken). Empty means "no incremental round is possible yet" —
	// the next fetch is a full one.
	Cursor string `json:"cursor,omitempty"`
	// ParamFingerprint identifies the query shape the Cursor is bound to.
	//
	// Providers disagree about how a cursor relates to its query, and both
	// directions break silently. Google requires every incremental call to
	// repeat the ORIGINAL parameters and answers a mismatch with a 400, not
	// the 410 that means "resync"; Graph encodes the parameters in the token
	// and rejects repeating them. Recording the fingerprint next to the cursor
	// makes a changed query — a new showDeleted default, a different event
	// type filter — force a clean full sync instead of a confusing error.
	ParamFingerprint string `json:"param_fingerprint,omitempty"`
	// Events is the remote event set keyed by iCalUID, series masters carrying
	// their exceptions.
	Events map[string]Event `json:"events"`
	// UpdatedAt is when the last round was folded in, for diagnostics.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// RemoteMirror is the whole per-account remote picture: one CalendarMirror per
// remote calendar id.
type RemoteMirror struct {
	Calendars map[string]CalendarMirror `json:"calendars"`
}

// newRemoteMirror returns an empty, fully initialized mirror.
func newRemoteMirror() *RemoteMirror {
	return &RemoteMirror{Calendars: make(map[string]CalendarMirror)}
}

// normalize initializes any nil maps (e.g. after unmarshaling a sparse file)
// so callers can index without nil checks.
func (m *RemoteMirror) normalize() {
	if m.Calendars == nil {
		m.Calendars = make(map[string]CalendarMirror)
	}
	for id, cal := range m.Calendars {
		if cal.Events == nil {
			cal.Events = make(map[string]Event)
			m.Calendars[id] = cal
		}
	}
}

// MARK: - Delta model

// OverrideChange is one modified or cancelled occurrence of a series, reported
// on its own because its master did not change.
//
// It is kept separate from a changed master rather than folded in by the
// provider, because an incremental round may deliver the occurrence WITHOUT
// its master — the series definition did not move, only that one date did. The
// master to apply it to is the one already in the mirror, which no provider
// call can produce.
type OverrideChange struct {
	// MasterID is the provider event id of the series the occurrence belongs
	// to.
	MasterID string
	// RecurrenceID is the ORIGINAL start of the occurrence, its identity
	// within the series.
	RecurrenceID time.Time
	// Cancelled marks the occurrence as removed from the series: it becomes an
	// exception date and any override for it is dropped.
	Cancelled bool
	// Event is the occurrence's new content; the zero value when Cancelled.
	Event Event
}

// DeltaResult is one round of an incremental fetch.
type DeltaResult struct {
	// ChangedMasters are new or changed master events (single events and
	// series definitions) to upsert wholesale.
	ChangedMasters []Event
	// ChangedOverrides are occurrence-level changes to fold into the master
	// they name.
	ChangedOverrides []OverrideChange
	// RemovedIDs are the provider event ids of deleted masters.
	RemovedIDs []string
	// Cursor is the token that resumes the feed after this round. It is empty
	// when the round did not settle (paging was interrupted), in which case
	// the caller must NOT record it — the next run has to start over rather
	// than skip the changes this round did not finish reporting.
	Cursor string
	// ParamFingerprint is the query shape Cursor is bound to.
	ParamFingerprint string
	// Reset reports that the provider could not continue from the cursor it
	// was given and started over. ChangedMasters is then the COMPLETE remote
	// set, so the mirror must be replaced rather than merged into — otherwise
	// events deleted while the cursor was dead would linger forever.
	Reset bool
}

// MARK: - Merge

// applyDelta folds one round of changes into the calendar mirror and returns
// the resulting remote event set.
//
// On Reset the previous events are discarded outright: a restarted feed
// reports what EXISTS, never what vanished, so merging would keep every event
// deleted while the cursor was unusable. On a normal round the events are
// merged, because the round only mentions what moved.
//
// The cursor is taken over only when the round actually settled. A round that
// stopped mid-paging has reported some changes but not all of them, and
// recording its cursor would declare the unreported ones already seen.
func (m *CalendarMirror) applyDelta(d DeltaResult, calendarName string) map[string]Event {
	if d.Reset || m.Events == nil {
		m.Events = make(map[string]Event, len(d.ChangedMasters))
	}

	// Index by provider event id so removals and occurrence changes, which
	// name events by id rather than by UID, can find their target.
	byID := make(map[string]string, len(m.Events))
	for uid, ev := range m.Events {
		byID[ev.ID] = uid
	}

	for _, ev := range d.ChangedMasters {
		if ev.ICalUID == "" {
			slog.Warn("Ignoring remote event without iCalUID", "module", "CALSYNC",
				"calendar", calendarName, "id", ev.ID)
			continue
		}
		// A changed master arrives with the exceptions the round delivered.
		// Anything the round did not mention is still valid, so the previously
		// known exceptions are carried over for occurrences the new copy says
		// nothing about.
		if prev, known := m.Events[ev.ICalUID]; known {
			ev = carryOverExceptions(prev, ev)
		}
		m.Events[ev.ICalUID] = ev
		byID[ev.ID] = ev.ICalUID
	}

	for _, oc := range d.ChangedOverrides {
		uid, known := byID[oc.MasterID]
		if !known {
			slog.Warn("Ignoring occurrence change without a known master", "module", "CALSYNC",
				"calendar", calendarName, "master", oc.MasterID)
			continue
		}
		master := m.Events[uid]
		applyOverrideChange(&master, oc)
		m.Events[uid] = master
	}

	for _, id := range d.RemovedIDs {
		uid, known := byID[id]
		if !known {
			// Already absent — a delete for something never mirrored, or a
			// replay of a round already folded in. Both are no-ops.
			continue
		}
		delete(m.Events, uid)
		delete(byID, id)
	}

	if d.Cursor != "" {
		m.Cursor = d.Cursor
		m.ParamFingerprint = d.ParamFingerprint
	}
	m.UpdatedAt = time.Now().UTC()

	out := make(map[string]Event, len(m.Events))
	for uid, ev := range m.Events {
		out[uid] = ev
	}
	return out
}

// carryOverExceptions returns next with the exceptions of prev restored for
// occurrences next does not mention.
//
// A provider that reports a changed series master does not necessarily repeat
// every exception of that series: a renamed weekly meeting comes back as one
// master with, at best, the exceptions the same round happened to touch.
// Overwriting wholesale would resurrect every previously cancelled date on the
// next rename. Exceptions next DOES carry win, because they are the newer
// information about those specific dates.
func carryOverExceptions(prev, next Event) Event {
	seen := make(map[int64]bool, len(next.ExceptionDates)+len(next.Overrides))
	for _, d := range next.ExceptionDates {
		seen[d.UTC().Unix()] = true
	}
	for _, o := range next.Overrides {
		seen[o.RecurrenceID.UTC().Unix()] = true
	}

	for _, d := range prev.ExceptionDates {
		if !seen[d.UTC().Unix()] {
			next.ExceptionDates = append(next.ExceptionDates, d)
		}
	}
	for _, o := range prev.Overrides {
		if !seen[o.RecurrenceID.UTC().Unix()] {
			next.Overrides = append(next.Overrides, o)
		}
	}
	sortExceptions(&next)
	return next
}

// applyOverrideChange folds one occurrence-level change into its master. A
// date is either cancelled or overridden, never both, so each side removes the
// other's entry for that RecurrenceID.
func applyOverrideChange(master *Event, oc OverrideChange) {
	key := oc.RecurrenceID.UTC().Unix()

	overrides := master.Overrides[:0:0]
	for _, o := range master.Overrides {
		if o.RecurrenceID.UTC().Unix() != key {
			overrides = append(overrides, o)
		}
	}
	exDates := master.ExceptionDates[:0:0]
	for _, d := range master.ExceptionDates {
		if d.UTC().Unix() != key {
			exDates = append(exDates, d)
		}
	}

	if oc.Cancelled {
		exDates = append(exDates, oc.RecurrenceID.UTC())
	} else {
		ev := oc.Event
		ev.RecurrenceID = oc.RecurrenceID.UTC()
		// An override is a deviation, never a series of its own.
		ev.Recurrence = nil
		ev.ExceptionDates = nil
		ev.Overrides = nil
		overrides = append(overrides, ev)
	}

	master.Overrides = overrides
	master.ExceptionDates = exDates
	sortExceptions(master)
}

// sortExceptions orders the exception dates and overrides of a master, so the
// .ics bytes it renders to — and with them the local hash the engine diffs
// on — do not depend on the order changes arrived in.
func sortExceptions(e *Event) {
	sort.Slice(e.ExceptionDates, func(i, j int) bool {
		return e.ExceptionDates[i].Before(e.ExceptionDates[j])
	})
	sort.Slice(e.Overrides, func(i, j int) bool {
		return e.Overrides[i].RecurrenceID.Before(e.Overrides[j].RecurrenceID)
	})
}

// MARK: - Store

// FileMirrorStore persists a RemoteMirror at
// <accountDir>/.durian-calsync-mirror.json, beside the sync state and with the
// same file handling (0700 dir, 0600 file, flock on a .lock sibling, atomic
// temp + rename). It sits at the vdir base, above the per-calendar collection
// subdirs, so khal and vdirsyncer ignore it.
type FileMirrorStore struct {
	dir string
}

// NewFileMirrorStore creates a mirror store inside dir (the account's vdir
// base directory).
func NewFileMirrorStore(dir string) *FileMirrorStore {
	return &FileMirrorStore{dir: dir}
}

// path returns the mirror file path inside the vdir directory.
func (f *FileMirrorStore) path() string {
	return filepath.Join(f.dir, ".durian-calsync-mirror.json")
}

// Load reads the remote mirror. A missing OR corrupted file yields an empty
// mirror: unlike the state file, nothing here is irreplaceable, so a bad
// mirror is simply dropped and the next sync refills it with a full download.
func (f *FileMirrorStore) Load() (*RemoteMirror, error) {
	lockFile, err := acquireFileLock(f.dir, f.path())
	if err != nil {
		return nil, err
	}
	defer releaseLock(lockFile)

	data, err := os.ReadFile(f.path())
	if err != nil {
		if os.IsNotExist(err) {
			return newRemoteMirror(), nil
		}
		return nil, fmt.Errorf("failed to read calendar mirror file: %w", err)
	}

	mirror := &RemoteMirror{}
	if err := json.Unmarshal(data, mirror); err != nil {
		slog.Warn("Unreadable calendar mirror, starting from a full sync", "module", "CALSYNC",
			"path", f.path(), "err", err)
		return newRemoteMirror(), nil
	}
	mirror.normalize()
	return mirror, nil
}

// Save persists the mirror atomically (temp file + rename) under the flock.
func (f *FileMirrorStore) Save(mirror *RemoteMirror) error {
	lockFile, err := acquireFileLock(f.dir, f.path())
	if err != nil {
		return err
	}
	defer releaseLock(lockFile)

	data, err := json.Marshal(mirror)
	if err != nil {
		return fmt.Errorf("failed to marshal calendar mirror: %w", err)
	}

	path := f.path()
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write calendar mirror file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename calendar mirror file: %w", err)
	}
	return nil
}
