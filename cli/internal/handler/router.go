package handler

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
)

// NewRouter registers the complete production HTTP contract. Authentication is
// installed separately so in-process tests can use the same routes.
func NewRouter(h *Handler, eventHub *EventHub, version, commit string) *mux.Router {
	r := mux.NewRouter()
	r.UseEncodedPath()
	r.HandleFunc("/api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": version, "commit": commit})
	}).Methods("GET")
	r.HandleFunc("/api/v1/search", h.SearchHandler).Methods("GET")
	r.HandleFunc("/api/v1/search/count", h.SearchCountHandler).Methods("GET")
	r.HandleFunc("/api/v1/tags", h.ListTagsHandler).Methods("GET")
	r.HandleFunc("/api/v1/settings", h.SettingsHandler).Methods("GET")
	r.HandleFunc("/api/v1/compose-settings", h.ComposeSettingsHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}", h.ShowThreadHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}/tags", h.TagThreadHandler).Methods("POST")
	r.HandleFunc("/api/v1/message/body", h.ShowMessageBodyHandler).Methods("GET")
	r.HandleFunc("/api/v1/messages/{message_id}/attachments/{part_id}", h.DownloadAttachmentHandler).Methods("GET")
	r.HandleFunc("/api/v1/groups", h.ListGroupsHandler).Methods("GET")
	r.HandleFunc("/api/v1/profiles", h.ProfilesHandler).Methods("GET")
	r.HandleFunc("/api/v1/keymaps", h.KeymapsHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/search", h.SearchContactsHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/usage", h.IncrementContactUsageHandler).Methods("POST")
	r.HandleFunc("/api/v1/contacts", h.ListContactsHandler).Methods("GET")
	if eventHub != nil {
		r.Handle("/api/v1/events", eventHub).Methods("GET")
	}

	// More specific calendar paths precede /calendars.
	r.HandleFunc("/api/v1/calendars/events", h.CalendarEventsHandler).Methods("GET")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarEventHandler).Methods("GET")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarPutEventHandler).Methods("PUT")
	r.HandleFunc("/api/v1/calendars/event", h.CalendarDeleteEventHandler).Methods("DELETE")
	r.HandleFunc("/api/v1/calendars/rsvp", h.CalendarRsvpHandler).Methods("POST")
	r.HandleFunc("/api/v1/calendars/sync/event", h.CalendarSyncEventHandler).Methods("POST")
	r.HandleFunc("/api/v1/calendars", h.CalendarsHandler).Methods("GET")
	r.HandleFunc("/api/v1/outbox/send", h.EnqueueOutboxHandler).Methods("POST")
	r.HandleFunc("/api/v1/messages/{message_id}/reactions", h.EnqueueReactionHandler).Methods("POST")
	r.HandleFunc("/api/v1/outbox", h.ListOutboxHandler).Methods("GET")
	r.HandleFunc("/api/v1/outbox/{id}", h.DeleteOutboxHandler).Methods("DELETE")
	r.HandleFunc("/api/v1/local-drafts", h.ListLocalDraftsHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.SaveLocalDraftHandler).Methods("PUT")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.GetLocalDraftHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.DeleteLocalDraftHandler).Methods("DELETE")
	return r
}

// LoopbackAuth is serve's DNS-rebinding host check and constant-time bearer
// check. The listener must also bind loopback. An empty token preserves serve's
// explicit --no-auth mode; test-only session servers must supply a random token.
func LoopbackAuth(port int, token string) mux.MiddlewareFunc {
	allowedHost := fmt.Sprintf("localhost:%d", port)
	allowedIP := fmt.Sprintf("127.0.0.1:%d", port)
	expected := []byte("Bearer " + token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != allowedHost && r.Host != allowedIP && r.Host != "localhost" && r.Host != "127.0.0.1" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expected) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
