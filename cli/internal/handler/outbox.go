package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/auth"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/encoding"
	imapClient "github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/smtp"
	"github.com/julion2/durian/cli/internal/store"
)

// OutboxDraft is the JSON payload for enqueuing an email to the outbox.
type OutboxDraft struct {
	From         string             `json:"from"`
	To           []string           `json:"to"`
	CC           []string           `json:"cc"`
	BCC          []string           `json:"bcc"`
	Subject      string             `json:"subject"`
	Body         string             `json:"body"`
	IsHTML       bool               `json:"is_html"`
	InReplyTo    string             `json:"in_reply_to"`
	References   string             `json:"references"`
	Attachments  []OutboxAttachment `json:"attachments"`
	DelaySeconds int                `json:"delay_seconds"`
}

// OutboxAttachment represents a base64-encoded attachment in the outbox payload.
type OutboxAttachment struct {
	Filename   string `json:"filename"`
	MIMEType   string `json:"mime_type"`
	DataBase64 string `json:"data_base64"`
}

// MARK: - HTTP Handlers

// EnqueueOutboxHandler handles POST /api/v1/outbox/send.
func (h *Handler) EnqueueOutboxHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20) // 50 MB (attachments)
	var draft OutboxDraft
	if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if draft.From == "" {
		http.Error(w, "Missing 'from' field", http.StatusBadRequest)
		return
	}
	if len(draft.To) == 0 {
		http.Error(w, "Missing 'to' field", http.StatusBadRequest)
		return
	}

	draftJSON, err := json.Marshal(draft)
	if err != nil {
		http.Error(w, "Failed to encode draft", http.StatusInternalServerError)
		return
	}

	var sendAfter int64
	if draft.DelaySeconds > 0 {
		sendAfter = time.Now().Unix() + int64(draft.DelaySeconds)
	}

	id, err := h.store.Enqueue(string(draftJSON), sendAfter)
	if err != nil {
		slog.Error("Failed to enqueue outbox item", "module", "OUTBOX", "err", err)
		http.Error(w, "Failed to enqueue", http.StatusInternalServerError)
		return
	}

	// ADR-0001 §6 redaction: do not log recipient list, subject or body content.
	slog.Info("Enqueued outbox item", "module", "OUTBOX", "id", id, "recipient_count", len(draft.To), "is_html", draft.IsHTML, "body_len", len(draft.Body), "send_after", sendAfter) // encgrep:allow body_len + draft.To are length/count, not content
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id, "send_after": sendAfter})
}

// ListOutboxHandler handles GET /api/v1/outbox.
func (h *Handler) ListOutboxHandler(w http.ResponseWriter, r *http.Request) {
	items, err := h.store.ListOutbox()
	if err != nil {
		slog.Error("Failed to list outbox", "module", "OUTBOX", "err", err)
		http.Error(w, "Failed to list outbox", http.StatusInternalServerError)
		return
	}

	type outboxEntry struct {
		ID        int64  `json:"id"`
		Subject   string `json:"subject"`
		To        string `json:"to"`
		Attempts  int    `json:"attempts"`
		LastError string `json:"last_error,omitempty"`
		CreatedAt int64  `json:"created_at"`
	}

	entries := make([]outboxEntry, 0, len(items))
	for _, item := range items {
		var draft OutboxDraft
		json.Unmarshal([]byte(item.DraftJSON), &draft)
		entries = append(entries, outboxEntry{
			ID:        item.ID,
			Subject:   draft.Subject,
			To:        strings.Join(draft.To, ", "),
			Attempts:  item.Attempts,
			LastError: item.LastError,
			CreatedAt: item.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// DeleteOutboxHandler handles DELETE /api/v1/outbox/{id}.
func (h *Handler) DeleteOutboxHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, err := strconv.ParseInt(vars["id"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid outbox item ID", http.StatusBadRequest)
		return
	}

	if err := h.store.DeleteOutboxItem(id); err != nil {
		slog.Error("Failed to delete outbox item", "module", "OUTBOX", "id", id, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	slog.Info("Deleted outbox item", "module", "OUTBOX", "id", id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// MARK: - Background Worker

// OutboxWorker processes the outbox queue in the background.
type OutboxWorker struct {
	store    *store.DB
	cfg      *config.Config
	eventHub *EventHub
}

// NewOutboxWorker creates a new outbox background worker.
func NewOutboxWorker(db *store.DB, cfg *config.Config, hub *EventHub) *OutboxWorker {
	return &OutboxWorker{store: db, cfg: cfg, eventHub: hub}
}

// Start runs the outbox processing loop until ctx is cancelled.
func (w *OutboxWorker) Start(ctx context.Context) {
	slog.Info("Outbox worker started", "module", "OUTBOX")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Outbox worker stopped", "module", "OUTBOX")
			return
		case <-ticker.C:
			w.processQueue()
		}
	}
}

// processQueue dequeues and sends items until the queue is empty.
func (w *OutboxWorker) processQueue() {
	for {
		item, err := w.store.Dequeue()
		if err != nil {
			slog.Error("Failed to dequeue outbox item", "module", "OUTBOX", "err", err)
			return
		}
		if item == nil {
			return // queue empty
		}

		w.sendItem(item)
	}
}

func (w *OutboxWorker) sendItem(item *store.OutboxItem) {
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(item.DraftJSON), &draft); err != nil {
		slog.Error("Failed to unmarshal draft", "module", "OUTBOX", "id", item.ID, "err", err) // encgrep:allow word "draft" in message text, no draft value logged
		w.store.MarkAttempted(item.ID, sanitizeOutboxError(err))
		return
	}

	// Look up account config by sender email
	account := w.findAccount(draft.From)
	if account == nil {
		errMsg := fmt.Sprintf("no account found for sender: %s", draft.From)
		slog.Error(errMsg, "module", "OUTBOX", "id", item.ID)
		w.store.PoisonOutboxItem(item.ID, errMsg)
		w.broadcastStatus(item.ID, "failed", errMsg, draft.Subject, strings.Join(draft.To, ", "))
		return
	}

	// Get SMTP auth
	smtpAuth, err := auth.GetSMTPAuth(account)
	if err != nil {
		slog.Error("Auth failed for outbox item", "module", "OUTBOX", "id", item.ID, "err", err)
		safeMsg := "auth: " + sanitizeOutboxError(err)
		w.store.MarkAttempted(item.ID, safeMsg)
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
		return
	}

	// Build smtp.Message
	from := account.Email
	if account.DisplayName != "" {
		from = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}

	msg := &smtp.Message{
		From:       from,
		To:         draft.To,
		CC:         draft.CC,
		BCC:        draft.BCC,
		Subject:    draft.Subject,
		Body:       draft.Body,
		IsHTML:     draft.IsHTML,
		InReplyTo:  draft.InReplyTo,
		References: draft.References,
	}

	// Decode base64 attachments
	for _, att := range draft.Attachments {
		data, err := base64.StdEncoding.DecodeString(att.DataBase64)
		if err != nil {
			slog.Error("Failed to decode attachment", "module", "OUTBOX", "id", item.ID, "filename", att.Filename, "err", err)
			safeMsg := "attachment decode: " + sanitizeOutboxError(err)
			w.store.MarkAttempted(item.ID, safeMsg)
			// Drop the filename from the SSE broadcast too — it's
			// user-supplied content that may carry sensitive metadata.
			w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
			return
		}
		msg.Attachments = append(msg.Attachments, smtp.Attachment{
			Filename: att.Filename,
			Data:     data,
			MIMEType: att.MIMEType,
		})
	}

	// Send via SMTP
	slog.Info("Sending outbox item", "module", "OUTBOX", "id", item.ID, "to", draft.To) // encgrep:allow recipient list is intentionally plaintext per ADR-0001 §3 (from/to/cc stay unencrypted for thread routing)
	client := smtp.NewClient(account.SMTP.Host, account.SMTP.Port, smtpAuth)
	if err := client.Send(msg); err != nil {
		// Distinguish network errors (offline/timeout) from SMTP errors (server rejected)
		if isNetworkError(err) {
			// Network error — don't count as an attempt, will retry silently
			slog.Warn("Network error, will retry later", "module", "OUTBOX", "id", item.ID, "err", err)
			return
		}

		smtpErr := smtp.ParseSMTPError(err)
		slog.Error("SMTP send failed", "module", "OUTBOX", "id", item.ID, "err", err)

		// ADR-0001 audit medium: sanitize before DB-persist + SSE so
		// the server-supplied response body (Gmail/O365 echo To: /
		// Subject: fragments in 5xx) never reaches the GUI.
		safeMsg := sanitizeOutboxError(err)
		if smtpErr != nil && smtpErr.IsPermanent() {
			// 5xx permanent error — poison immediately
			w.store.PoisonOutboxItem(item.ID, safeMsg)
		} else {
			w.store.MarkAttempted(item.ID, safeMsg)
		}
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
		return
	}

	// Success — delete from outbox
	slog.Info("Outbox item sent successfully", "module", "OUTBOX", "id", item.ID)
	w.store.DeleteOutboxItem(item.ID)

	// Save to local SQLite store so Sent folder shows the email immediately
	// (without waiting for next IMAP sync).
	w.saveToLocalStore(account, msg, &draft)

	w.broadcastStatus(item.ID, "sent", "", draft.Subject, strings.Join(draft.To, ", "))

	// Append to IMAP Sent folder (best-effort, same logic as send.go)
	w.appendToSent(account, msg)
}

// findAccount looks up the account config matching the sender email or display name format.
func (w *OutboxWorker) findAccount(from string) *config.AccountConfig {
	// Extract email from "Display Name <email>" format
	email := from
	if idx := strings.Index(from, "<"); idx != -1 {
		end := strings.Index(from, ">")
		if end > idx {
			email = from[idx+1 : end]
		}
	}
	email = strings.TrimSpace(email)

	for i := range w.cfg.Accounts {
		if strings.EqualFold(w.cfg.Accounts[i].Email, email) {
			return &w.cfg.Accounts[i]
		}
	}
	return nil
}

// saveToLocalStore inserts the sent email into SQLite so the GUI can show it
// immediately without waiting for the next IMAP sync. Best-effort: errors are
// logged but do not affect the send result.
func (w *OutboxWorker) saveToLocalStore(account *config.AccountConfig, msg *smtp.Message, draft *OutboxDraft) {
	messageID := strings.Trim(msg.GeneratedMessageID, "<>")
	if messageID == "" {
		slog.Warn("No Message-ID available, skipping local store insert", "module", "OUTBOX")
		return
	}

	now := time.Now().Unix()
	fromAddr := account.Email
	if account.DisplayName != "" {
		fromAddr = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}
	storeMsg := &store.Message{
		MessageID:   messageID,
		Subject:     draft.Subject,
		FromAddr:    fromAddr,
		ToAddrs:     strings.Join(draft.To, ", "),
		CCAddrs:     strings.Join(draft.CC, ", "),
		InReplyTo:   draft.InReplyTo,
		Refs:        draft.References,
		Date:        now,
		CreatedAt:   now,
		Flags:       "Seen",
		FetchedBody: true,
		Account:     account.AccountIdentifier(),
	}
	if draft.IsHTML {
		storeMsg.BodyHTML = draft.Body
		storeMsg.BodyText = encoding.HTMLToText(draft.Body)
	} else {
		storeMsg.BodyText = draft.Body
	}

	if err := w.store.InsertMessage(storeMsg); err != nil {
		slog.Warn("Failed to save sent email to local store", "module", "OUTBOX", "err", err) // encgrep:allow message text, no PII attr
		return
	}
	if err := w.store.AddTag(storeMsg.ID, "sent"); err != nil {
		slog.Warn("Failed to tag sent email", "module", "OUTBOX", "err", err) // encgrep:allow message text, no PII attr
	}
}

// appendToSent saves a copy to the IMAP Sent folder (skip for providers that auto-save).
func (w *OutboxWorker) appendToSent(account *config.AccountConfig, msg *smtp.Message) {
	if account.OAuth != nil && (account.OAuth.Provider == "google" || account.OAuth.Provider == "microsoft") {
		slog.Debug("Skipping Sent append", "module", "OUTBOX", "provider", account.OAuth.Provider) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	messageData, err := msg.Build()
	if err != nil {
		slog.Warn("Failed to build message for Sent folder", "module", "OUTBOX", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	conn := imapClient.NewClient(account)
	if err := conn.Connect(); err != nil {
		slog.Warn("Failed to connect IMAP for Sent folder", "module", "OUTBOX", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}
	defer conn.Close()

	if err := conn.Authenticate(); err != nil {
		slog.Warn("Failed to authenticate IMAP for Sent folder", "module", "OUTBOX", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	sentMailbox, err := conn.FindSentMailbox()
	if err != nil {
		slog.Warn("Could not find Sent mailbox", "module", "OUTBOX", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	flags := []string{imap.SeenFlag}
	if _, err := conn.Append(sentMailbox, flags, time.Now(), messageData); err != nil {
		slog.Warn("Failed to save to Sent folder", "module", "OUTBOX", "err", err) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		return
	}

	slog.Info("Saved to Sent folder", "module", "OUTBOX", "mailbox", sentMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
}

// isNetworkError returns true if the error is a connection/DNS/timeout failure
// (i.e. we never reached the SMTP server). These should not count as send attempts.
func isNetworkError(err error) bool {
	// net.Error covers dial timeouts, connection refused, DNS failures
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Also catch wrapped connection errors from smtp.Client.Send
	msg := err.Error()
	return strings.Contains(msg, "failed to connect") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable")
}

// broadcastStatus sends an outbox_update SSE event.
func (w *OutboxWorker) broadcastStatus(itemID int64, status, errMsg, subject, to string) {
	if w.eventHub == nil {
		return
	}
	w.eventHub.BroadcastOutbox(OutboxUpdateEvent{
		ItemID:  itemID,
		Status:  status,
		Error:   errMsg,
		Subject: subject,
		To:      to,
	})
}
