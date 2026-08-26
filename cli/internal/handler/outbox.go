package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/encoding"
	imapClient "github.com/julion2/durian/cli/internal/imap"
	"github.com/julion2/durian/cli/internal/mailsend"
	"github.com/julion2/durian/cli/internal/sender"
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

	// Build the provider-neutral outgoing message. The Message-ID is fixed here
	// so the sent mail, the local Sent row and any Sent-folder copy share it.
	from := account.Email
	if account.DisplayName != "" {
		from = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}
	msg := &mailsend.Message{
		MessageID:  mailsend.GenerateMessageID(from),
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
		msg.Attachments = append(msg.Attachments, mailsend.Attachment{
			Filename: att.Filename,
			MIMEType: att.MIMEType,
			Data:     data,
		})
	}

	// Resolve the account's configured SMTP, Graph, Gmail, or JMAP transport.
	mailSender, err := sender.For(account)
	if err != nil {
		// Transport setup resolves keychain credentials, so sanitize before the
		// log sink: never let a raw error from that path reach a logging call.
		safeMsg := "auth: " + sanitizeOutboxError(err)
		slog.Error("Sender setup failed for outbox item", "module", "OUTBOX", "id", item.ID, "err", safeMsg)
		w.store.MarkAttempted(item.ID, safeMsg)
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	slog.Info("Sending outbox item", "module", "OUTBOX", "id", item.ID, "to", draft.To) // encgrep:allow recipient list is intentionally plaintext per ADR-0001 §3 (from/to/cc stay unencrypted for thread routing)
	if err := mailSender.Send(ctx, msg); err != nil {
		// The Kind decides retry policy; sanitize before DB/SSE so a
		// server-echoed 5xx body never reaches the GUI.
		safeMsg := sanitizeOutboxError(err)
		switch mailsend.Classify(err) {
		case mailsend.KindNetwork:
			// Offline/timeout — don't count as an attempt, retry silently.
			slog.Warn("Network error, will retry later", "module", "OUTBOX", "id", item.ID, "err", err)
			return
		case mailsend.KindPermanent:
			slog.Error("Send failed permanently", "module", "OUTBOX", "id", item.ID, "err", err)
			w.store.PoisonOutboxItem(item.ID, safeMsg)
		default:
			slog.Error("Send failed", "module", "OUTBOX", "id", item.ID, "err", err)
			w.store.MarkAttempted(item.ID, safeMsg)
		}
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
		return
	}

	// Success — delete from outbox
	slog.Info("Outbox item sent successfully", "module", "OUTBOX", "id", item.ID)
	w.store.DeleteOutboxItem(item.ID)

	// Save to local store so the Sent view shows the mail immediately.
	w.saveToLocalStore(account, msg, &draft)

	w.broadcastStatus(item.ID, "sent", "", draft.Subject, strings.Join(draft.To, ", "))

	// Append to IMAP Sent folder (best-effort; providers that auto-save skip it).
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
func (w *OutboxWorker) saveToLocalStore(account *config.AccountConfig, msg *mailsend.Message, draft *OutboxDraft) {
	messageID := strings.Trim(msg.MessageID, "<>")
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
func (w *OutboxWorker) appendToSent(account *config.AccountConfig, msg *mailsend.Message) {
	if account.UsesGraphBackend() || account.UsesGmailBackend() || account.UsesJMAPBackend() {
		slog.Debug("Skipping Sent append for native provider", "module", "OUTBOX", "engine", account.EffectiveSyncEngine())
		return
	}

	messageData, err := smtp.FromMessage(msg).Build()
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
