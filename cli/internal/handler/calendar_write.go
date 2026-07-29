package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/julion2/durian/cli/internal/calendar"
)

// Local-first calendar write endpoints. They only edit the on-disk vdir (create
// or overwrite a .ics, or remove one) — NOTHING is sent to Outlook here. The
// change reaches the server on the next `durian calendar sync`, which runs the
// Stage-2 engine with its notification preview and confirmation. So no API write
// can email attendees on its own.

// calendarEventWrite is the PUT /calendars/event request body (snake_case).
type calendarEventWrite struct {
	Account     string `json:"account"`
	Calendar    string `json:"calendar"`
	UID         string `json:"uid"`
	Subject     string `json:"subject"`
	Start       string `json:"start"`
	End         string `json:"end"`
	AllDay      bool   `json:"all_day"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Attendees   []struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Type     string `json:"type"`
		Response string `json:"response"`
	} `json:"attendees"`
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
//     recurrence, the owner's RSVP and the online-meeting link are always
//     preserved, and the attendee list is preserved unless the request carries
//     one — so a GUI edit of subject/time can never silently strip a meeting's
//     attendees (which the next sync would push as an uninvite wave).
//   - An update must target the calendar the event already lives in; moving an
//     event between calendars (a remote delete + re-invite) is rejected.
func (h *Handler) CalendarPutEventHandler(w http.ResponseWriter, r *http.Request) {
	var req calendarEventWrite
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	dir, owner, ok := h.resolveCalendarAccount(w, req.Account)
	if !ok {
		return
	}
	if req.Calendar == "" {
		http.Error(w, "missing required 'calendar'", http.StatusBadRequest)
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
		ICalUID:     req.UID,
		Subject:     req.Subject,
		Start:       start.UTC(),
		End:         end.UTC(),
		AllDay:      req.AllDay,
		Location:    req.Location,
		Description: req.Description,
	}
	for _, a := range req.Attendees {
		if strings.TrimSpace(a.Email) == "" {
			continue
		}
		ev.Attendees = append(ev.Attendees, calendar.Attendee{
			Name: a.Name, Email: strings.TrimSpace(a.Email), Type: a.Type, Response: a.Response,
		})
	}

	// Update path: the UID already exists in the vdir. Merge over the existing
	// event so fields the write schema does not carry are never dropped, and
	// write back to the event's existing file (its name may predate the
	// UID-derived scheme).
	if path, existing, calName, resolveErr := calendar.ResolveLocalEvent(dir, owner, req.UID, ""); resolveErr == nil && existing.ICalUID == req.UID {
		if !strings.EqualFold(calName, req.Calendar) {
			http.Error(w, "event belongs to a different calendar; moving events between calendars is not supported", http.StatusBadRequest)
			return
		}
		ev.Organizer = existing.Organizer
		ev.Recurrence = existing.Recurrence
		ev.OwnerResponse = existing.OwnerResponse
		ev.IsOnlineMeeting = existing.IsOnlineMeeting
		ev.OnlineMeetingURL = existing.OnlineMeetingURL
		// A meeting cancelled remotely stays cancelled. Dropping the flag
		// would rewrite the file without STATUS:CANCELLED, which the content
		// hash counts as a local edit — so the next sync would PATCH the
		// cancelled event back to life against the provider.
		ev.IsCancelled = existing.IsCancelled
		if req.Attendees == nil {
			ev.Attendees = existing.Attendees
		}
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

	if _, err := calendar.WriteLocalEvent(dir, req.Calendar, ev); err != nil {
		slog.Error("Failed to write local event", "module", "API", "err", logSafe(err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	dir, owner, ok := h.resolveCalendarAccount(w, req.Account)
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
	path, event, calName, err := calendar.ResolveLocalEvent(dir, owner, req.Ref, req.Calendar)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Same rail as the CLI `calendar rsvp`: an organizer does not RSVP to
	// their own meeting.
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
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
	dir, owner, ok := h.resolveCalendarAccount(w, q.Get("account"))
	if !ok {
		return
	}
	ref := q.Get("ref")
	if ref == "" {
		http.Error(w, "missing required 'ref' query parameter", http.StatusBadRequest)
		return
	}
	path, _, _, err := calendar.ResolveLocalEvent(dir, owner, ref, q.Get("calendar"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
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
