package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
)

// Local-first calendar write endpoints. They only edit the on-disk vdir (create
// or overwrite a .ics, or remove one) — NOTHING is sent to Outlook here. The
// change reaches the server on the next `durian calendar sync`, which runs the
// Stage-2 engine with its notification preview and confirmation. So no API write
// can email attendees on its own.

// calendarEventWrite is the PUT /calendars/event request body (snake_case).
// Attendees is a plain email list (each becomes a required attendee) and
// RequestOnlineMeeting asks the provider for an online meeting — both are
// honored on CREATE only; an update never wipes an existing meeting's
// attendee set or pending online-meeting request (see the merge below).
type calendarEventWrite struct {
	Account              string   `json:"account"`
	Calendar             string   `json:"calendar"`
	UID                  string   `json:"uid"`
	Subject              string   `json:"subject"`
	Start                string   `json:"start"`
	End                  string   `json:"end"`
	AllDay               bool     `json:"all_day"`
	Location             string   `json:"location"`
	Description          string   `json:"description"`
	Attendees            []string `json:"attendees"`
	RequestOnlineMeeting bool     `json:"request_online_meeting"`
}

// refuseReadOnly reports whether the named calendar is configured read-only
// and, if so, writes the HTTP error.
//
// WriteEventIn guards the create path, but an update, an RSVP and a delete all
// write the .ics file directly — a read-only calendar has to be refused at
// each of them, or "durian never edits this folder" would hold for exactly one
// of the four operations.
func refuseReadOnly(w http.ResponseWriter, cols []calendar.Collection, name string) bool {
	for _, col := range cols {
		// Match on the RESOLVED name: Collection.Name is empty for the
		// collections discovered under an account directory, so comparing it
		// directly would silently never match.
		if col.ReadOnly && strings.EqualFold(calendar.CollectionName(col), name) {
			http.Error(w, "calendar is read-only", http.StatusForbidden)
			return true
		}
	}
	return false
}

// CalendarPutEventHandler upserts a local .ics from the posted event, generating
// a UID when absent (create) or overwriting the existing file for a known UID
// (update). Writes the vdir only.
//
// Robustness rails:
//   - An all-day event whose end is less than 24h after the start is snapped to
//     end = start + 1 day (Graph rejects shorter all-day events; same guard as
//     the CLI `calendar new` and the sync write boundary).
//   - An update (known UID) merges over the existing event: organizer,
//     recurrence, the owner's RSVP, the online-meeting link and a pending
//     online-meeting request are always preserved, and the attendee list is
//     preserved unless the request carries a non-empty one — so a GUI edit of
//     subject/time can never silently strip a meeting's attendees (which the
//     next sync would push as an uninvite wave). request_online_meeting is
//     honored on create only.
//   - An update must target the calendar the event already lives in; moving an
//     event between calendars (a remote delete + re-invite) is rejected.
func (h *Handler) CalendarPutEventHandler(w http.ResponseWriter, r *http.Request) {
	var req calendarEventWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	cols, ok := h.resolveCalendarCollections(w, req.Account)
	if !ok {
		return
	}
	if req.Calendar == "" {
		http.Error(w, "missing required 'calendar'", http.StatusBadRequest)
		return
	}
	// A local calendar never reaches a provider, so attendees / online meetings
	// would silently go nowhere. Reject them rather than accept an invite that
	// can never be sent.
	if req.Account == config.LocalCalendarAccount && (len(req.Attendees) > 0 || req.RequestOnlineMeeting) {
		http.Error(w, "local calendars cannot have attendees or online meetings (they do not sync to a provider)", http.StatusBadRequest)
		return
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		http.Error(w, "invalid 'start' (need RFC3339, e.g. 2026-08-03T09:00:00Z)", http.StatusBadRequest)
		return
	}
	end, err := time.Parse(time.RFC3339, req.End)
	if err != nil {
		http.Error(w, "invalid 'end' (need RFC3339, e.g. 2026-08-03T10:00:00Z)", http.StatusBadRequest)
		return
	}
	if req.AllDay {
		// All-day events span whole days at midnight UTC boundaries; Graph
		// rejects anything shorter than 24h, so truncate to the calendar day
		// and snap a non-positive span up to a one-day event instead of
		// writing a local file the sync could never upload.
		start = start.UTC().Truncate(24 * time.Hour)
		end = end.UTC().Truncate(24 * time.Hour)
		if !end.After(start) {
			end = start.AddDate(0, 0, 1)
		}
	}
	if !end.After(start) {
		http.Error(w, "'end' must be after 'start'", http.StatusBadRequest)
		return
	}
	if req.UID == "" {
		req.UID = uuid.NewString()
	}

	ev := calendar.Event{
		ICalUID:              req.UID,
		Subject:              req.Subject,
		Start:                start.UTC(),
		End:                  end.UTC(),
		AllDay:               req.AllDay,
		Location:             req.Location,
		Description:          req.Description,
		RequestOnlineMeeting: req.RequestOnlineMeeting,
	}
	// Same attendee semantics as the CLI `calendar new --attendee`: every
	// entry is a required attendee; blanks are skipped, duplicates collapse,
	// and anything that does not look like an email is rejected.
	seen := make(map[string]bool, len(req.Attendees))
	for _, email := range req.Attendees {
		email = strings.TrimSpace(email)
		if email == "" || seen[strings.ToLower(email)] {
			continue
		}
		if !strings.Contains(email, "@") {
			http.Error(w, "invalid attendee: not an email address", http.StatusBadRequest)
			return
		}
		seen[strings.ToLower(email)] = true
		ev.Attendees = append(ev.Attendees, calendar.Attendee{Email: email, Type: "required"})
	}

	// Update path: the UID already exists in the vdir. Start from the STORED
	// event and overwrite only what the write schema actually carries, then
	// write back to the event's existing file (its name may predate the
	// UID-derived scheme).
	//
	// The direction matters. Copying a hand-written list of fields onto the
	// request would silently drop every field added to Event afterwards — and
	// dropping one here is not cosmetic: the rewritten .ics differs from the
	// stored one, which the content hash reads as a local edit, so the next
	// sync PATCHes the loss up to the server. Losing the series exceptions
	// resurrects cancelled occurrences and undoes moved ones; losing
	// OpaqueRecurrence collapses a series into a single appointment. Starting
	// from `existing` makes preservation the default and enumerates only what
	// the client may change.
	if path, existing, calName, resolveErr := calendar.ResolveEventIn(cols, req.UID, ""); resolveErr == nil && existing.ICalUID == req.UID {
		if !strings.EqualFold(calName, req.Calendar) {
			http.Error(w, "event belongs to a different calendar; moving events between calendars is not supported", http.StatusBadRequest)
			return
		}
		// A read-only calendar must refuse an update too (WriteEventIn only
		// guards create), or "durian never edits this folder" would hold for
		// create but not update.
		if refuseReadOnly(w, cols, calName) {
			return
		}
		merged := existing
		merged.Subject = ev.Subject
		merged.Start = ev.Start
		merged.End = ev.End
		merged.AllDay = ev.AllDay
		merged.Location = ev.Location
		merged.Description = ev.Description
		if len(ev.Attendees) > 0 {
			// The GUI edit form sends an empty list (attendee editing is
			// create-only for now); an empty list can never strip a meeting.
			merged.Attendees = ev.Attendees
		}
		// request_online_meeting is a create-time flag: an update never sets
		// it, and a still-pending marker (created locally, not yet synced)
		// survives an edit so the first sync still picks it up. It is carried
		// by `existing` and deliberately not overwritten here.
		ev = merged

		data, serErr := calendar.EventToICal(ev)
		if serErr != nil {
			slog.Error("Failed to serialize updated local event", "module", "API", "err", logSafe(serErr.Error()))
			http.Error(w, "failed to serialize event", http.StatusInternalServerError)
			return
		}
		if writeErr := calendar.WriteFileAtomic(path, data, 0o600); writeErr != nil {
			slog.Error("Failed to write local event", "module", "API", "err", logSafe(writeErr.Error()))
			http.Error(w, "failed to write event", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "event": calendar.ToCalendarEvent(calName, ev, true)})
		return
	}

	if _, err := calendar.WriteEventIn(cols, req.Calendar, ev); err != nil {
		slog.Error("Failed to write local event", "module", "API", "err", logSafe(err.Error()))
		status := http.StatusBadRequest
		if errors.Is(err, calendar.ErrReadOnly) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "event": calendar.ToCalendarEvent(req.Calendar, ev, true)})
}

// calendarRsvpRequest is the POST /calendars/rsvp request body (snake_case).
type calendarRsvpRequest struct {
	Account  string `json:"account"`
	Calendar string `json:"calendar"`
	Ref      string `json:"ref"`
	Response string `json:"response"`
}

// CalendarRsvpHandler sets the account owner's RSVP on a meeting by rewriting
// the owner's ATTENDEE PARTSTAT (and the canonical owner response) in the
// resolved local .ics. Strictly local-first: no provider call and no mail —
// the organizer only learns of the response on the next `durian calendar
// sync`, where an RSVP is a notifying action behind the preview/confirmation
// gate.
func (h *Handler) CalendarRsvpHandler(w http.ResponseWriter, r *http.Request) {
	var req calendarRsvpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	cols, ok := h.resolveCalendarCollections(w, req.Account)
	if !ok {
		return
	}
	if req.Ref == "" {
		http.Error(w, "missing required 'ref'", http.StatusBadRequest)
		return
	}
	resp, err := calendar.ParseRSVPVerb(req.Response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path, event, calName, err := calendar.ResolveEventIn(cols, req.Ref, req.Calendar)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if refuseReadOnly(w, cols, calName) {
		return
	}
	// Same rail as the CLI `calendar rsvp`: an organizer does not RSVP to
	// their own meeting.
	owner := calendar.CollectionOwner(cols, calName)
	if event.OwnerResponse == calendar.OwnerRespOrganizer || calendar.OwnerIsOrganizer(event, owner) {
		http.Error(w, "cannot RSVP: you are the organizer of this event", http.StatusBadRequest)
		return
	}
	if !calendar.SetOwnerResponse(&event, owner, resp) {
		http.Error(w, "not an attendee of this event", http.StatusBadRequest)
		return
	}
	data, err := calendar.EventToICal(event)
	if err != nil {
		slog.Error("Failed to serialize RSVP into local event", "module", "API", "err", err)
		http.Error(w, "failed to serialize event", http.StatusInternalServerError)
		return
	}
	// Atomic like every other write into the vdir: the sync engine scans this
	// directory concurrently, and a truncated .ics is indistinguishable from a
	// corrupt one — which suppresses local-deletion planning for the whole
	// calendar until the next run.
	if err := calendar.WriteFileAtomic(path, data, 0o600); err != nil {
		slog.Error("Failed to write local event", "module", "API", "err", err)
		http.Error(w, "failed to write event", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "event": calendar.ToCalendarEvent(calName, event, true)})
}

// CalendarDeleteEventHandler removes a local .ics by reference. The deletion is
// propagated to Outlook on the next sync.
func (h *Handler) CalendarDeleteEventHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols, ok := h.resolveCalendarCollections(w, q.Get("account"))
	if !ok {
		return
	}
	ref := q.Get("ref")
	if ref == "" {
		http.Error(w, "missing required 'ref' query parameter", http.StatusBadRequest)
		return
	}
	path, _, calName, err := calendar.ResolveEventIn(cols, ref, q.Get("calendar"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if refuseReadOnly(w, cols, calName) {
		return
	}
	// Move the file aside instead of unlinking it. The sync still reads the
	// event as locally deleted (the backup does not end in .ics, so the local
	// scan ignores it) and the remote deletion still waits for the interactive
	// `durian calendar sync` confirmation — but a mis-keyed delete in the GUI
	// no longer destroys an event that was never synced anywhere. Same rail as
	// the sync engine's own overwrite and conflict paths: local data is never
	// silently lost.
	backup := fmt.Sprintf("%s.deleted-%d", path, time.Now().Unix())
	if err := os.Rename(path, backup); err != nil {
		slog.Error("Failed to delete local event", "module", "API", "err", logSafe(err.Error()))
		http.Error(w, "failed to delete event", http.StatusInternalServerError)
		return
	}
	slog.Info("Deleted local event, kept a backup", "module", "API", "backup", logSafe(backup))
	writeJSON(w, map[string]any{"ok": true})
}

// logSafe strips control characters from a value that originates in a request
// before it reaches the log.
//
// The calendar write endpoints echo request-derived data into their errors —
// WriteLocalEvent names the requested calendar, ResolveLocalEvent the ref —
// and those errors get logged. A newline in a JSON calendar name would
// otherwise let a caller forge whole log lines (CodeQL go/log-injection). The
// server is localhost-only, but a log a reader cannot trust is worth less than
// the two lines it costs to keep it trustworthy.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, s)
}
