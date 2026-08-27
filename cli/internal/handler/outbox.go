package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
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
	Kind         string             `json:"kind,omitempty"`
	Account      string             `json:"account,omitempty"`
	TargetID     string             `json:"target_message_id,omitempty"`
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

const (
	outboxKindReaction = "reaction"
	reactionSendDelay  = 10
)

type reactionRequest struct {
	Account string `json:"account"`
	Emoji   string `json:"emoji"`
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

// EnqueueReactionHandler handles POST /api/v1/messages/{message_id}/reactions.
// The client supplies only target account and emoji; all mail fields are
// derived from the exact stored message row.
func (h *Handler) EnqueueReactionHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request reactionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if h.cfg == nil {
		http.Error(w, "Mail configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	if !mailsend.IsReactionEmoji(request.Emoji) {
		http.Error(w, "Unsupported reaction emoji", http.StatusBadRequest)
		return
	}
	account, err := h.cfg.GetAccountByIdentifier(request.Account)
	if err != nil {
		http.Error(w, "Unknown account", http.StatusBadRequest)
		return
	}
	targetID := strings.Trim(mux.Vars(r)["message_id"], "<>")
	target, err := h.store.GetByMessageIDAndAccount(targetID, account.AccountIdentifier())
	if err != nil {
		http.Error(w, "Failed to resolve target message", http.StatusInternalServerError)
		return
	}
	if target == nil {
		http.Error(w, "Message not found for account", http.StatusNotFound)
		return
	}

	recipient := target.FromAddr
	if replyTo, err := h.store.GetHeader(target.ID, "reply-to"); err != nil {
		http.Error(w, "Failed to resolve Reply-To", http.StatusInternalServerError)
		return
	} else if strings.TrimSpace(replyTo) != "" {
		recipient = replyTo
	}
	parsedRecipients, err := mail.ParseAddressList(recipient)
	if err != nil || len(parsedRecipients) != 1 {
		http.Error(w, "Message has no single valid reply recipient", http.StatusBadRequest)
		return
	}
	recipient = parsedRecipients[0].String()

	references, err := mailsend.ReactionReferences(target.Refs, target.MessageID)
	if err != nil {
		http.Error(w, "Message has invalid threading headers", http.StatusBadRequest)
		return
	}
	draft := OutboxDraft{
		Kind:       outboxKindReaction,
		Account:    account.AccountIdentifier(),
		TargetID:   target.MessageID,
		From:       account.Email,
		To:         []string{recipient},
		Subject:    mailsend.ReplySubject(target.Subject),
		Body:       request.Emoji,
		InReplyTo:  target.MessageID,
		References: references,
	}

	items, err := h.store.ListOutbox()
	if err != nil {
		http.Error(w, "Failed to inspect outbox", http.StatusInternalServerError)
		return
	}
	for _, item := range items {
		var pending OutboxDraft
		if json.Unmarshal([]byte(item.DraftJSON), &pending) == nil &&
			pending.Kind == outboxKindReaction && pending.Account == draft.Account &&
			pending.TargetID == draft.TargetID && pending.Body == draft.Body {
			http.Error(w, "Reaction is already pending", http.StatusConflict)
			return
		}
	}

	draftJSON, err := json.Marshal(draft)
	if err != nil {
		http.Error(w, "Failed to encode reaction", http.StatusInternalServerError)
		return
	}
	sendAfter := time.Now().Unix() + reactionSendDelay
	id, err := h.store.Enqueue(string(draftJSON), sendAfter)
	if err != nil {
		http.Error(w, "Failed to enqueue reaction", http.StatusInternalServerError)
		return
	}
	slog.Info("Enqueued reaction", "module", "OUTBOX", "id", id, "account", draft.Account, "send_after", sendAfter)
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
		w.store.MarkAttempted(item.ID, sanitizeOutboxError(err))
		return true
	}

	// Reactions carry the exact target account; legacy compose payloads keep
	// resolving by sender address for backward compatibility.
	var account *config.AccountConfig
	if draft.Kind == outboxKindReaction {
		account, _ = w.cfg.GetAccountByIdentifier(draft.Account)
	} else {
		account = w.findAccount(draft.From)
	}
	if account == nil {
		errMsg := fmt.Sprintf("no account found for sender: %s", draft.From)
		slog.Error(errMsg, "module", "OUTBOX", "id", item.ID)
		w.store.PoisonOutboxItem(item.ID, errMsg)
		w.broadcastStatus(item.ID, "failed", errMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
		return true
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
	if draft.Kind == outboxKindReaction {
		var err error
		msg.RawMIME, err = mailsend.BuildReaction(msg, time.Now())
		if err != nil {
			safeMsg := sanitizeOutboxError(err)
			w.store.PoisonOutboxItem(item.ID, safeMsg)
			w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
			return true
		}
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
			w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
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
		w.store.MarkAttempted(item.ID, safeMsg)
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
		return true
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
			// Offline/timeout — don't count as an attempt, but move the item out
			// of the ready queue so processQueue cannot dequeue it in a tight loop.
			slog.Warn("Network error, will retry later", "module", "OUTBOX", "id", item.ID, "err", err)
			if deferErr := w.store.DeferOutboxItem(item.ID, time.Now().Add(30*time.Second).Unix(), safeMsg); deferErr != nil {
				slog.Error("Failed to defer outbox item", "module", "OUTBOX", "id", item.ID, "err", deferErr)
				return false
			}
			return true
		case mailsend.KindPermanent:
			slog.Error("Send failed permanently", "module", "OUTBOX", "id", item.ID, "err", err)
			w.store.PoisonOutboxItem(item.ID, safeMsg)
		default:
			slog.Error("Send failed", "module", "OUTBOX", "id", item.ID, "err", err)
			w.store.MarkAttempted(item.ID, safeMsg)
		}
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
		return true
	}

	// Success — delete from outbox
	slog.Info("Outbox item sent successfully", "module", "OUTBOX", "id", item.ID)
	w.store.DeleteOutboxItem(item.ID)

	// Save to local store so the Sent view shows the mail immediately.
	w.saveToLocalStore(account, msg, &draft)

	w.broadcastStatus(item.ID, "sent", "", draft.Subject, strings.Join(draft.To, ", "), draft.Kind)

	// Append to IMAP Sent folder (best-effort; providers that auto-save skip it).
	w.appendToSent(account, msg, mailSender.SavesSentCopy())
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
func (w *OutboxWorker) appendToSent(account *config.AccountConfig, msg *mailsend.Message, savedServerSide bool) {
	if savedServerSide {
		slog.Debug("Skipping Sent append for native provider", "module", "OUTBOX", "engine", account.EffectiveSyncEngine()) // encgrep:allow engine name is static configuration, not message content
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
func (w *OutboxWorker) broadcastStatus(itemID int64, status, errMsg, subject, to, kind string) {
	if w.eventHub == nil {
		return
	}
	w.eventHub.BroadcastOutbox(OutboxUpdateEvent{
		ItemID:  itemID,
		Status:  status,
		Error:   errMsg,
		Subject: subject,
		To:      to,
		Kind:    kind,
	})
}
