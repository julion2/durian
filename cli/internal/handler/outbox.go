package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	IdempotencyKey string             `json:"idempotency_key"`
	MessageID      string             `json:"message_id"`
	From           string             `json:"from"`
	To             []string           `json:"to"`
	CC             []string           `json:"cc"`
	BCC            []string           `json:"bcc"`
	Subject        string             `json:"subject"`
	Body           string             `json:"body"`
	IsHTML         bool               `json:"is_html"`
	InReplyTo      string             `json:"in_reply_to"`
	References     string             `json:"references"`
	Attachments    []OutboxAttachment `json:"attachments"`
	DelaySeconds   int                `json:"delay_seconds"`
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
	if draft.IdempotencyKey == "" || len(draft.IdempotencyKey) > 200 {
		http.Error(w, "Missing or invalid 'idempotency_key' field", http.StatusBadRequest)
		return
	}
	// The durable draft owns the provider-correlation ID before a worker can
	// claim it, so crash recovery can verify this exact message with the provider.
	draft.MessageID = mailsend.GenerateMessageID(draft.From)

	draftJSON, err := json.Marshal(draft)
	if err != nil {
		http.Error(w, "Failed to encode draft", http.StatusInternalServerError)
		return
	}

	var sendAfter int64
	if draft.DelaySeconds > 0 {
		sendAfter = time.Now().Unix() + int64(draft.DelaySeconds)
	}

	id, sendAfter, err := h.store.EnqueueIdempotent(string(draftJSON), sendAfter, draft.IdempotencyKey)
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
		ID                int64  `json:"id"`
		MessageID         string `json:"message_id"`
		Subject           string `json:"subject"`
		To                string `json:"to"`
		Attempts          int    `json:"attempts"`
		LastError         string `json:"last_error,omitempty"`
		CreatedAt         int64  `json:"created_at"`
		InFlight          bool   `json:"in_flight"`
		DeliveryConfirmed bool   `json:"delivery_confirmed"`
	}

	entries := make([]outboxEntry, 0, len(items))
	for _, item := range items {
		var draft OutboxDraft
		json.Unmarshal([]byte(item.DraftJSON), &draft)
		entries = append(entries, outboxEntry{
			ID:                item.ID,
			MessageID:         draft.MessageID,
			Subject:           draft.Subject,
			To:                strings.Join(draft.To, ", "),
			Attempts:          item.Attempts,
			LastError:         item.LastError,
			CreatedAt:         item.CreatedAt,
			InFlight:          item.InFlight,
			DeliveryConfirmed: item.DeliveryConfirmed,
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

	if err := h.store.DeletePendingOutboxItem(id); err != nil {
		slog.Error("Failed to delete outbox item", "module", "OUTBOX", "id", id, "err", err)
		switch {
		case errors.Is(err, store.ErrOutboxItemInFlight):
			http.Error(w, "Outbox item is already being sent", http.StatusConflict)
		case errors.Is(err, store.ErrOutboxItemNotFound):
			http.Error(w, "Outbox item not found", http.StatusNotFound)
		default:
			http.Error(w, "Failed to delete outbox item", http.StatusInternalServerError)
		}
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

// processQueue atomically claims and sends items until the queue is empty.
func (w *OutboxWorker) processQueue() {
	for {
		item, err := w.store.ClaimNextOutboxItem()
		if err != nil {
			slog.Error("Failed to claim outbox item", "module", "OUTBOX", "err", err)
			return
		}
		if item == nil {
			return // queue empty
		}

		if !w.sendItem(item) {
			return
		}
	}
}

// sendItem returns false when queue processing must stop because a failed send
// could not be moved out of the ready queue safely.
func (w *OutboxWorker) sendItem(item *store.OutboxItem) bool {
	var draft OutboxDraft
	if err := json.Unmarshal([]byte(item.DraftJSON), &draft); err != nil {
		slog.Error("Failed to unmarshal draft", "module", "OUTBOX", "id", item.ID, "err", err) // encgrep:allow word "draft" in message text, no draft value logged
		if transitionErr := w.store.MarkAttempted(item.ID, sanitizeOutboxError(err)); transitionErr != nil {
			slog.Error("Failed to record invalid outbox draft", "module", "OUTBOX", "id", item.ID, "err", transitionErr) // encgrep:allow word "draft" is static; outbox id and store error are operational metadata
			return false
		}
		return true
	}
	if draft.MessageID == "" {
		// Upgrade a legacy queued draft before any provider or credential work.
		draft.MessageID = mailsend.GenerateMessageID(draft.From)
		updated, err := json.Marshal(draft)
		if err != nil {
			slog.Error("Failed to persist outbox Message-ID", "module", "OUTBOX", "id", item.ID, "err", err)
			return false
		}
		if err := w.store.UpdateClaimedOutboxDraft(item.ID, string(updated)); err != nil {
			slog.Error("Failed to persist outbox Message-ID", "module", "OUTBOX", "id", item.ID, "err", err)
			return false
		}
	}

	// Look up account config by sender email
	account := w.findAccount(draft.From)
	if account == nil {
		errMsg := fmt.Sprintf("no account found for sender: %s", draft.From)
		slog.Error(errMsg, "module", "OUTBOX", "id", item.ID)
		if transitionErr := w.store.PoisonOutboxItem(item.ID, errMsg); transitionErr != nil {
			slog.Error("Failed to poison unrouteable outbox item", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
		w.broadcastStatus(item.ID, "failed", errMsg, draft.Subject, strings.Join(draft.To, ", "))
		return true
	}

	// Build the provider-neutral outgoing message. The Message-ID is fixed here
	// so the sent mail, the local Sent row and any Sent-folder copy share it.
	from := account.Email
	if account.DisplayName != "" {
		from = fmt.Sprintf("%s <%s>", account.DisplayName, account.Email)
	}
	msg := &mailsend.Message{
		MessageID:  draft.MessageID,
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
			if transitionErr := w.store.MarkAttempted(item.ID, safeMsg); transitionErr != nil {
				slog.Error("Failed to record invalid outbox attachment", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
				return false
			}
			// Drop the filename from the SSE broadcast too — it's
			// user-supplied content that may carry sensitive metadata.
			w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
			return true
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
		if transitionErr := w.store.MarkAttempted(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to record outbox sender setup failure", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "))
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	slog.Info("Sending outbox item", "module", "OUTBOX", "id", item.ID, "to", draft.To) // encgrep:allow recipient list is intentionally plaintext per ADR-0001 §3 (from/to/cc stay unencrypted for thread routing)
	send := func() error { return mailSender.Send(ctx, msg) }
	if durable, ok := mailSender.(mailsend.DurableSender); ok {
		send = func() error {
			return durable.SendAfterPersist(ctx, msg, func(messageID string) error {
				msg.MessageID = messageID
				draft.MessageID = messageID
				updated, err := json.Marshal(draft)
				if err != nil {
					return fmt.Errorf("encode provider Message-ID: %w", err)
				}
				if err := w.store.UpdateClaimedOutboxDraft(item.ID, string(updated)); err != nil {
					return fmt.Errorf("persist provider Message-ID: %w", err)
				}
				return nil
			})
		}
	}
	if err := send(); err != nil {
		return w.handleSendError(item, &draft, err)
	}

	// Record acceptance before deleting the claim. If either write or the
	// process fails, the durable confirmation prevents a not-delivered requeue
	// from duplicating a message the provider accepted.
	if err := w.store.MarkOutboxDeliveryConfirmed(item.ID, ""); err != nil {
		slog.Error("Sent outbox item could not be marked delivered; manual reconciliation required", "module", "OUTBOX", "id", item.ID, "err", err)
		return false
	}

	// Keep the confirmed claim—and therefore its full durable payload—until all
	// local post-delivery projection is complete. A crash or Sent filing failure
	// can never erase the only recoverable copy of an accepted SMTP message.
	filingErr := w.saveToLocalStore(account, msg, &draft)
	filingErr = errors.Join(filingErr, w.appendToSent(account, msg, mailSender.SavesSentCopy()))
	if filingErr != nil {
		const reason = "Message was delivered, but filing it in Sent requires manual remediation."
		if err := w.store.MarkOutboxDeliveryConfirmed(item.ID, reason); err != nil {
			slog.Error("Delivered outbox item could not retain filing state", "module", "OUTBOX", "id", item.ID, "err", err)
			return false
		}
		slog.Error("Delivery succeeded but Sent filing failed", "module", "OUTBOX", "id", item.ID)
		w.broadcastStatus(item.ID, "delivered_with_warning", reason, draft.Subject, strings.Join(draft.To, ", "))
		return true
	}
	if err := w.store.DeleteClaimedOutboxItem(item.ID); err != nil {
		slog.Error("Sent outbox item could not be deleted; manual reconciliation required", "module", "OUTBOX", "id", item.ID, "err", err)
		return false
	}
	slog.Info("Outbox item sent successfully", "module", "OUTBOX", "id", item.ID)
	w.broadcastStatus(item.ID, "sent", "", draft.Subject, strings.Join(draft.To, ", "))
	return true
}

func (w *OutboxWorker) handleSendError(item *store.OutboxItem, draft *OutboxDraft, err error) bool {
	// The Kind decides retry/reconciliation policy. Provider details remain in
	// the redacted log; DB and SSE receive only fixed or sanitized text.
	safeMsg := sanitizeOutboxError(err)
	status := "failed"
	switch mailsend.Classify(err) {
	case mailsend.KindNetwork:
		slog.Warn("Network error, will retry later", "module", "OUTBOX", "id", item.ID, "err", err)
		if transitionErr := w.store.DeferOutboxItem(item.ID, time.Now().Add(30*time.Second).Unix(), safeMsg); transitionErr != nil {
			slog.Error("Failed to defer outbox item", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
		return true
	case mailsend.KindAmbiguous:
		slog.Error("Delivery outcome is unknown; manual reconciliation required", "module", "OUTBOX", "id", item.ID, "err", err)
		safeMsg = "Delivery status is unknown. Verify the provider outcome before reconciling."
		status = "reconciliation_required"
		if transitionErr := w.store.MarkOutboxReconciliationRequired(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to preserve ambiguous outbox claim", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
	case mailsend.KindDeliveredWithWarning:
		slog.Error("Delivery succeeded with a post-delivery failure", "module", "OUTBOX", "id", item.ID, "err", err)
		safeMsg = "Message was delivered, but filing it in Sent requires manual remediation."
		status = "delivered_with_warning"
		if transitionErr := w.store.MarkOutboxDeliveryConfirmed(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to preserve delivered outbox claim", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
	case mailsend.KindPermanent:
		slog.Error("Send failed permanently", "module", "OUTBOX", "id", item.ID, "err", err)
		if transitionErr := w.store.PoisonOutboxItem(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to poison permanently failed outbox item", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
	default:
		slog.Error("Send failed", "module", "OUTBOX", "id", item.ID, "err", err)
		if transitionErr := w.store.MarkAttempted(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to record outbox send failure", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
	}
	w.broadcastStatus(item.ID, status, safeMsg, draft.Subject, strings.Join(draft.To, ", "))
	return true
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
// immediately without waiting for the next IMAP sync. A failure keeps the
// confirmed outbox claim and its payload available for manual remediation.
func (w *OutboxWorker) saveToLocalStore(account *config.AccountConfig, msg *mailsend.Message, draft *OutboxDraft) error {
	// Native JMAP sync uses provider-scoped Email ids as row identity. A local
	// Message-ID-only placeholder cannot be safely promoted and would remain as
	// a duplicate beside the server's Sent Email on the next sync.
	if account.UsesJMAPBackend() {
		return nil
	}
	messageID := strings.Trim(msg.MessageID, "<>")
	if messageID == "" {
		return errors.New("no Message-ID available for local Sent projection")
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
		BCCAddrs:    strings.Join(draft.BCC, ", "),
		InReplyTo:   draft.InReplyTo,
		Refs:        draft.References,
		Date:        now,
		CreatedAt:   now,
		Flags:       `\Seen`,
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
		return fmt.Errorf("save sent email to local store: %w", err)
	}
	if err := w.store.AddTag(storeMsg.ID, "sent"); err != nil {
		return fmt.Errorf("tag sent email: %w", err)
	}
	return nil
}

// appendToSent saves a copy to the IMAP Sent folder (skip for providers that auto-save).
func (w *OutboxWorker) appendToSent(account *config.AccountConfig, msg *mailsend.Message, savedServerSide bool) error {
	if savedServerSide {
		slog.Debug("Skipping Sent append for native provider", "module", "OUTBOX", "engine", account.EffectiveSyncEngine()) // encgrep:allow engine name is static configuration, not message content
		return nil
	}

	messageData, err := smtp.FromMessage(msg).Build()
	if err != nil {
		return fmt.Errorf("build message for Sent folder: %w", err)
	}

	conn := imapClient.NewClient(account)
	if err := conn.Connect(); err != nil {
		return fmt.Errorf("connect IMAP for Sent folder: %w", err)
	}
	defer conn.Close()

	if err := conn.Authenticate(); err != nil {
		return fmt.Errorf("authenticate IMAP for Sent folder: %w", err)
	}

	sentMailbox, err := conn.FindSentMailbox()
	if err != nil {
		return fmt.Errorf("find Sent mailbox: %w", err)
	}

	flags := []string{imap.SeenFlag}
	if _, err := conn.Append(sentMailbox, flags, time.Now(), messageData); err != nil {
		return fmt.Errorf("save to Sent folder: %w", err)
	}

	slog.Info("Saved to Sent folder", "module", "OUTBOX", "mailbox", sentMailbox) // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
	return nil
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
