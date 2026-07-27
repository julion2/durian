package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/julion2/durian/cli/internal/graphcalendar"
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

	ev := graphcalendar.Event{
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
		ev.Attendees = append(ev.Attendees, graphcalendar.Attendee{
			Name: a.Name, Email: strings.TrimSpace(a.Email), Type: a.Type, Response: a.Response,
		})
	}

	// Update path: the UID already exists in the vdir. Merge over the existing
	// event so fields the write schema does not carry are never dropped, and
	// write back to the event's existing file (its name may predate the
	// UID-derived scheme).
	if path, existing, calName, resolveErr := graphcalendar.ResolveLocalEvent(dir, owner, req.UID, ""); resolveErr == nil && existing.ICalUID == req.UID {
		if !strings.EqualFold(calName, req.Calendar) {
			http.Error(w, "event belongs to a different calendar; moving events between calendars is not supported", http.StatusBadRequest)
			return
		}
		ev.Organizer = existing.Organizer
		ev.Recurrence = existing.Recurrence
		ev.OwnerResponse = existing.OwnerResponse
		ev.IsOnlineMeeting = existing.IsOnlineMeeting
		ev.OnlineMeetingURL = existing.OnlineMeetingURL
		if req.Attendees == nil {
			ev.Attendees = existing.Attendees
		}
		data, serErr := graphcalendar.EventToICal(ev)
		if serErr != nil {
			slog.Error("Failed to serialize updated local event", "module", "API", "err", serErr)
			http.Error(w, "failed to serialize event", http.StatusInternalServerError)
			return
		}
		if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
			slog.Error("Failed to write local event", "module", "API", "err", writeErr)
			http.Error(w, "failed to write event", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "event": graphcalendar.ToCalendarEvent(calName, ev, true)})
		return
	}

	if _, err := graphcalendar.WriteLocalEvent(dir, req.Calendar, ev); err != nil {
		slog.Error("Failed to write local event", "module", "API", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "event": graphcalendar.ToCalendarEvent(req.Calendar, ev, true)})
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
	path, _, _, err := graphcalendar.ResolveLocalEvent(dir, owner, ref, q.Get("calendar"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := os.Remove(path); err != nil {
		slog.Error("Failed to delete local event", "module", "API", "err", err)
		http.Error(w, "failed to delete event", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
