package handler

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/config"
)

// Read-only calendar endpoints. They serve the locally synced vdir (populated
// by `durian calendar sync`) — no Microsoft Graph call, no token — projecting
// events through the calendar package's snake_case DTOs (see openapi.yaml). Writes are
// intentionally not exposed: the API can never trigger a sync or send mail.

// defaultCalendarWindow is the [today, today+7d) span used by the events
// endpoint when no from/to is given.
const defaultCalendarWindow = 7 * 24 * time.Hour

// resolveCalendarAccount maps the ?account= parameter to its local vdir dir and
// owner email. On any problem it writes the HTTP error and returns ok=false.
func (h *Handler) resolveCalendarAccount(w http.ResponseWriter, account string) (dir, owner string, ok bool) {
	if h.cfg == nil {
		http.Error(w, "config not loaded", http.StatusServiceUnavailable)
		return "", "", false
	}
	if account == "" {
		http.Error(w, "missing required 'account' query parameter", http.StatusBadRequest)
		return "", "", false
	}
	acct, err := h.cfg.GetAccountByIdentifier(account)
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return "", "", false
	}
	dir = filepath.Join(config.CalendarBaseDir(h.cfg, ""), acct.CalendarDir())
	return dir, acct.GetAuthEmail(), true
}

// resolveCalendarCollections maps the ?account= parameter to the calendar
// collections it addresses, plus the owner email used to recognize the owner's
// own RSVP. On any problem it writes the HTTP error and returns ok=false.
//
// The reserved identifier "local" resolves to the calendars configured under
// calendar.local instead of an account directory. They have no owner: nobody
// is invited to an event that never leaves the disk, so there is no RSVP to
// recognize.
func (h *Handler) resolveCalendarCollections(w http.ResponseWriter, account string) (cols []calendar.Collection, ok bool) {
	if h.cfg == nil {
		http.Error(w, "config not loaded", http.StatusServiceUnavailable)
		return nil, false
	}
	if account == "" {
		http.Error(w, "missing required 'account' query parameter", http.StatusBadRequest)
		return nil, false
	}
	if strings.EqualFold(account, config.LocalCalendarAccount) {
		return localCollections(h.cfg), true
	}

	dir, owner, ok := h.resolveCalendarAccount(w, account)
	if !ok {
		return nil, false
	}
	cols, err := calendar.CollectionsUnderFor(dir, account, owner)
	if err != nil {
		slog.Error("Failed to enumerate local calendars", "module", "API", "err", logSafe(err.Error()))
		http.Error(w, "failed to read calendars", http.StatusInternalServerError)
		return nil, false
	}
	return cols, true
}

// localCollections projects the configured local calendars onto the neutral
// collection type.
func localCollections(cfg *config.Config) []calendar.Collection {
	local := cfg.LocalCalendars()
	cols := make([]calendar.Collection, 0, len(local))
	for _, lc := range local {
		cols = append(cols, calendar.Collection{
			Dir:      lc.Path,
			Name:     lc.Name,
			HexColor: lc.Color,
			ReadOnly: lc.ReadOnly,
			Account:  config.LocalCalendarAccount,
		})
	}
	return cols
}

// CalendarsHandler serves GET /api/v1/calendars?account= — the account's
// calendars with their color and event count.
func (h *Handler) CalendarsHandler(w http.ResponseWriter, r *http.Request) {
	cols, ok := h.resolveCalendarCollections(w, r.URL.Query().Get("account"))
	if !ok {
		return
	}
	calendars, err := calendar.ReadCollections(cols)
	if err != nil {
		slog.Error("Failed to read local calendars", "module", "API", "err", err)
		http.Error(w, "failed to read calendars", http.StatusInternalServerError)
		return
	}
	for _, bad := range calendar.InspectCollections(cols, calendars) {
		slog.Warn("Local calendar path points at a vdir base, not a collection",
			"module", "API", "calendar", calendar.CollectionName(bad.Collection),
			"path", bad.Collection.Dir, "collections", bad.SubCollections)
	}
	out := make([]calendar.CalendarDTO, 0, len(calendars))
	for _, c := range calendars {
		out = append(out, calendar.CalendarDTO{Name: c.Name, Color: c.HexColor, EventCount: len(c.Events)})
	}
	writeJSON(w, map[string]any{"ok": true, "calendars": out})
}

// CalendarEventsHandler serves GET /api/v1/calendars/events?account=[&from&to&calendar&q].
// Without q it returns the events (recurrences expanded) starting in the
// [from, to) window, defaulting to the next 7 days. With q it returns a
// full-text search across all events (the window is ignored).
func (h *Handler) CalendarEventsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cols, ok := h.resolveCalendarCollections(w, q.Get("account"))
	if !ok {
		return
	}
	calFilter := q.Get("calendar")

	calendars, err := calendar.ReadCollections(cols)
	if err != nil {
		slog.Error("Failed to read local calendars", "module", "API", "err", err)
		http.Error(w, "failed to read calendars", http.StatusInternalServerError)
		return
	}

	events := []calendar.CalendarEvent{}
	if query := strings.TrimSpace(q.Get("q")); query != "" {
		lower := strings.ToLower(query)
		for _, cal := range calendars {
			if calFilter != "" && !strings.EqualFold(cal.Name, calFilter) {
				continue
			}
			for _, e := range cal.Events {
				if calendar.EventMatchesQuery(e, lower) {
					events = append(events, calendar.ToCalendarEvent(cal.Name, e, false))
				}
			}
		}
	} else {
		from, to, err := calendar.CalendarWindow(q.Get("from"), q.Get("to"), defaultCalendarWindow, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, cal := range calendars {
			if calFilter != "" && !strings.EqualFold(cal.Name, calFilter) {
				continue
			}
			for _, e := range cal.Events {
				for _, occ := range calendar.ExpandOccurrences(e, from, to) {
					events = append(events, calendar.ToCalendarEvent(cal.Name, occ, false))
				}
			}
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Start.Before(events[j].Start) })
	writeJSON(w, map[string]any{"ok": true, "events": events})
}

// CalendarEventHandler serves GET /api/v1/calendars/event?account=&ref=[&calendar].
// ref matches an event by iCalUID (exact or prefix) or a unique subject
// substring; the full (detail) event is returned, or 404 when none/ambiguous.
func (h *Handler) CalendarEventHandler(w http.ResponseWriter, r *http.Request) {
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
	_, e, calName, err := calendar.ResolveEventIn(cols, ref, q.Get("calendar"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "event": calendar.ToCalendarEvent(calName, e, true)})
}
