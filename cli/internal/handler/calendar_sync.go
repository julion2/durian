package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// CalendarEventSyncer applies the pending sync action for exactly one event.
// durian serve supplies the provider-backed implementation; the HTTP package
// owns only request validation and response mapping.
type CalendarEventSyncer interface {
	SyncCalendarEvent(ctx context.Context, account, calendar, uid, operation string) (bool, error)
}

// Errors returned by CalendarEventSyncer that have stable HTTP meanings.
var (
	ErrCalendarSyncBusy       = errors.New("another calendar sync is running")
	ErrCalendarSyncConflict   = errors.New("calendar event changed on both sides")
	ErrCalendarSyncBadRequest = errors.New("calendar sync request is invalid")
	ErrCalendarSyncNotFound   = errors.New("calendar sync account not found")
	ErrCalendarSyncDisabled   = errors.New("calendar sync is disabled for this account")
)

type calendarEventSyncRequest struct {
	Account   string `json:"account"`
	Calendar  string `json:"calendar"`
	UID       string `json:"uid"`
	Operation string `json:"operation"`
}

// CalendarSyncEventHandler synchronizes the one event named by the explicit
// GUI action. It never approves or applies other pending events in the account.
func (h *Handler) CalendarSyncEventHandler(w http.ResponseWriter, r *http.Request) {
	var req calendarEventSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Account == "" || req.Calendar == "" || req.UID == "" ||
		(req.Operation != "save" && req.Operation != "rsvp" && req.Operation != "delete") {
		http.Error(w, "'account', 'calendar', 'uid', and a valid 'operation' are required", http.StatusBadRequest)
		return
	}
	if h.calendarSyncer == nil {
		http.Error(w, "calendar sync unavailable", http.StatusServiceUnavailable)
		return
	}
	applied, err := h.calendarSyncer.SyncCalendarEvent(r.Context(), req.Account, req.Calendar, req.UID, req.Operation)
	if err != nil {
		switch {
		case errors.Is(err, ErrCalendarSyncBadRequest):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrCalendarSyncNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrCalendarSyncBusy), errors.Is(err, ErrCalendarSyncConflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrCalendarSyncDisabled):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			slog.Error("Failed to sync calendar event", "module", "API", "err", logSafe(err.Error()))
			http.Error(w, "failed to sync calendar event", http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, map[string]any{"ok": true, "applied": applied})
}
