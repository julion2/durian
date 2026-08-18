package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// NewMailEvent is broadcast over SSE when new mail arrives after an IDLE-triggered sync.
type NewMailEvent struct {
	Account  string        `json:"account"`
	TotalNew int           `json:"total_new"`
	Messages []NewMailInfo `json:"messages"`
}

// NewMailInfo describes a single new message within a NewMailEvent.
type NewMailInfo struct {
	ThreadID string `json:"thread_id"`
	Subject  string `json:"subject"`
	From     string `json:"from"`
	Snippet  string `json:"snippet"`
}

// OutboxUpdateEvent is broadcast over SSE when an outbox item changes status.
type OutboxUpdateEvent struct {
	ItemID  int64  `json:"item_id"`
	Status  string `json:"status"` // "sent", "failed", "queued"
	Error   string `json:"error,omitempty"`
	Subject string `json:"subject,omitempty"`
	To      string `json:"to,omitempty"`
}

// CalendarUpdatedEvent is broadcast over SSE after a background calendar
// autosync changed anything (events downloaded or pruned locally, or —
// in safe upload mode — non-notifying local changes pushed upstream), so the
// GUI can refresh its calendar views. It carries only the account identifier
// and counts — never event content.
type CalendarUpdatedEvent struct {
	Account    string `json:"account"`
	Downloaded int    `json:"downloaded"`
	Pruned     int    `json:"pruned"`
	Uploaded   int    `json:"uploaded"`
}

// EventHub is a fan-out broadcaster for SSE events.
// It implements http.Handler and serves as the /api/v1/events endpoint.
type EventHub struct {
	mu          sync.Mutex
	subscribers map[chan []byte]struct{}
}

// NewEventHub creates a ready-to-use EventHub.
func NewEventHub() *EventHub {
	return &EventHub{
		subscribers: make(map[chan []byte]struct{}),
	}
}

// Subscribe registers a new SSE client and returns its event channel.
func (h *EventHub) Subscribe() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// HasClients reports whether any SSE client is currently connected. The GUI
// holds this stream open for its entire lifetime, so it doubles as the
// daemon's "is anyone watching?" signal — used to pick a fast or a battery-
// friendly sync cadence without the daemon knowing anything about windows.
func (h *EventHub) HasClients() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers) > 0
}

// Unsubscribe removes a client channel and closes it.
func (h *EventHub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
	close(ch)
}

// Broadcast serialises a NewMailEvent to SSE format and sends it to all
// connected clients. Slow clients whose buffers are full are skipped (logged).
func (h *EventHub) Broadcast(event NewMailEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("Failed to marshal event", "module", "EVENTS", "err", err)
		return
	}

	msg := fmt.Appendf(nil, "event: new_mail\ndata: %s\n\n", data)

	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subscribers {
		select {
		case ch <- msg:
		default:
			slog.Warn("Dropped event for slow subscriber", "module", "EVENTS")
		}
	}
}

// BroadcastOutbox serialises an OutboxUpdateEvent and sends it to all SSE clients.
func (h *EventHub) BroadcastOutbox(event OutboxUpdateEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("Failed to marshal outbox event", "module", "EVENTS", "err", err)
		return
	}

	msg := fmt.Appendf(nil, "event: outbox_update\ndata: %s\n\n", data)

	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subscribers {
		select {
		case ch <- msg:
		default:
			slog.Warn("Dropped outbox event for slow subscriber", "module", "EVENTS")
		}
	}
}

// BroadcastCalendar serialises a CalendarUpdatedEvent and sends it to all SSE
// clients as a "calendar_updated" event.
func (h *EventHub) BroadcastCalendar(event CalendarUpdatedEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("Failed to marshal calendar event", "module", "EVENTS", "err", err)
		return
	}

	msg := fmt.Appendf(nil, "event: calendar_updated\ndata: %s\n\n", data)

	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.subscribers {
		select {
		case ch <- msg:
		default:
			slog.Warn("Dropped calendar event for slow subscriber", "module", "EVENTS")
		}
	}
}

// ServeHTTP implements http.Handler — the SSE endpoint.
func (h *EventHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.Subscribe()
	defer h.Unsubscribe(ch)

	slog.Info("SSE client connected", "module", "EVENTS", "remote", r.RemoteAddr)
	defer slog.Info("SSE client disconnected", "module", "EVENTS", "remote", r.RemoteAddr)

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
