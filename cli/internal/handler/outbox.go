package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/gorilla/mux"

	"github.com/julion2/durian/cli/internal/backend"
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
	IdempotencyKey string `json:"idempotency_key"`
	MessageID      string `json:"message_id"`
	// Kind is "reaction" for an RFC 9078 emoji reply, which is built as
	// canonical MIME and submitted unchanged. Empty means normal compose.
	Kind string `json:"kind,omitempty"`
	// Account and TargetID name the exact stored row a reaction answers, so
	// the worker resolves the sending account without matching on From.
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
	// IdempotencyKey identifies one user action, exactly as compose does.
	// Retrying a request after a lost response reuses it; reacting again after
	// an Undo is a new action and must carry a new key. Older clients omit it
	// and fall back to a content-derived key.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
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

// EnqueueReactionHandler handles POST /api/v1/messages/{message_id}/reactions.
// The client supplies only the target message and emoji; the sending account,
// reply recipient and threading metadata are derived from that exact stored
// row, which is addressed by the opaque local identifier the thread view
// returns.
func (h *Handler) EnqueueReactionHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request reactionRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeReactionError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if h.cfg == nil {
		writeReactionError(w, http.StatusServiceUnavailable, "Mail configuration unavailable")
		return
	}
	if !mailsend.IsReactionEmoji(request.Emoji) {
		writeReactionError(w, http.StatusBadRequest, "Unsupported reaction emoji")
		return
	}
	target, err := h.store.GetByIdentifier(strings.Trim(mux.Vars(r)["message_id"], "<>"))
	if err != nil {
		writeReactionError(w, http.StatusBadRequest, "Failed to resolve target message")
		return
	}
	if target == nil {
		writeReactionError(w, http.StatusNotFound, "Message not found")
		return
	}
	// The stored row owns the account: it decides which mailbox the reply
	// threads into and which credentials send it. An explicit account is only
	// accepted when it names that same account.
	account, err := h.cfg.GetAccountByIdentifier(target.Account)
	if err != nil {
		writeReactionError(w, http.StatusBadRequest, "Unknown account")
		return
	}
	if request.Account != "" {
		requested, err := h.cfg.GetAccountByIdentifier(request.Account)
		if err != nil || requested.AccountIdentifier() != account.AccountIdentifier() {
			writeReactionError(w, http.StatusNotFound, "Message not found for account")
			return
		}
	}
	replyToIndexed, err := h.store.HasHeader(target.ID, "reply-to")
	if err != nil {
		writeReactionError(w, http.StatusInternalServerError, "Failed to inspect Reply-To status")
		return
	}
	if !replyToIndexed {
		if err := h.resolveReactionHeaders(r.Context(), target, account); err != nil {
			// The provider error is deliberately absent: it can echo the
			// fetched message, and no header value may reach a log.
			slog.Warn("Failed to resolve reaction headers on demand", "module", "OUTBOX", "account", account.AccountIdentifier()) // encgrep:allow account identifier (config name); no header value or provider text is logged
			if errors.Is(err, errReactionHeadersUnfetchable) {
				writeReactionError(w, http.StatusConflict, "Reply-To status is unavailable for this message")
				return
			}
			writeReactionError(w, http.StatusBadGateway, "Failed to fetch the message's Reply-To from the mail server")
			return
		}
	}

	recipient := target.FromAddr
	if replyTo, err := h.store.GetHeader(target.ID, "reply-to"); err != nil {
		writeReactionError(w, http.StatusInternalServerError, "Failed to resolve Reply-To")
		return
	} else if strings.TrimSpace(replyTo) != "" {
		recipient = replyTo
	}
	parsedRecipients, err := mail.ParseAddressList(recipient)
	if err != nil || len(parsedRecipients) != 1 {
		writeReactionError(w, http.StatusBadRequest, "Message has no single valid reply recipient")
		return
	}
	recipient = parsedRecipients[0].String()

	references, err := mailsend.ReactionReferences(target.Refs, target.MessageID)
	if err != nil {
		writeReactionError(w, http.StatusBadRequest, "Message has invalid threading headers")
		return
	}
	draft := OutboxDraft{
		Kind:     outboxKindReaction,
		Account:  account.AccountIdentifier(),
		TargetID: target.MessageID,
		// Repeating one reaction request, including after a lost HTTP response,
		// returns the queued row and its original schedule instead of sending
		// the emoji twice.
		IdempotencyKey: reactionIdempotencyKey(request, account.AccountIdentifier(), target.ID),
		MessageID:      mailsend.GenerateMessageID(account.Email),
		From:           account.Email,
		To:             []string{recipient},
		Subject:        mailsend.ReplySubject(target.Subject),
		Body:           request.Emoji,
		InReplyTo:      target.MessageID,
		References:     references,
	}

	draftJSON, err := json.Marshal(draft)
	if err != nil {
		writeReactionError(w, http.StatusInternalServerError, "Failed to encode reaction")
		return
	}
	id, sendAfter, err := h.store.EnqueueIdempotent(string(draftJSON), time.Now().Unix()+reactionSendDelay, draft.IdempotencyKey)
	if err != nil {
		slog.Error("Failed to enqueue reaction", "module", "OUTBOX", "err", err)
		writeReactionError(w, http.StatusInternalServerError, "Failed to enqueue reaction")
		return
	}
	slog.Info("Enqueued reaction", "module", "OUTBOX", "id", id, "account", draft.Account, "send_after", sendAfter) // encgrep:allow account identifier (config name); no emoji, recipient or subject is logged
	writeJSON(w, map[string]any{"ok": true, "id": id, "send_after": sendAfter, "recipient": recipient})
}

// errReactionHeadersUnfetchable reports that this row carries no provider
// handle the header fetch could use. Only legacy IMAP rows land here, and the
// IMAP syncer backfills their markers on its own schedule.
var errReactionHeadersUnfetchable = errors.New("message has no provider handle for a header fetch")

// reactionHeaderFetchTimeout bounds the one provider roundtrip an old message
// pays on its first reaction. A user is waiting on the click, so this is far
// shorter than the attachment path's 60s download budget.
const reactionHeaderFetchTimeout = 10 * time.Second

// resolveReactionHeaders fetches the target message through its account's
// backend and writes the Reply-To and Content-Disposition markers for that
// row, exactly as syncengine.Ingest does for freshly synced messages —
// including empty values, which record that the header was inspected and
// absent. Rows synced before the reaction feature existed have no markers and
// no backfill will ever produce them on the provider-neutral engine, so the
// first reaction resolves them here instead of refusing.
func (h *Handler) resolveReactionHeaders(ctx context.Context, target *store.Message, account *config.AccountConfig) error {
	if target.RemoteRef == "" {
		return errReactionHeadersUnfetchable
	}
	ctx, cancel := context.WithTimeout(ctx, reactionHeaderFetchTimeout)
	defer cancel()

	b, err := h.mailBackend(account)
	if err != nil {
		return fmt.Errorf("create backend: %w", err)
	}
	defer b.Close()

	var buf bytes.Buffer
	ref := backend.RemoteRef{Folder: target.Mailbox, ID: target.RemoteRef, MessageID: target.MessageID}
	if err := b.FetchBody(ctx, ref, &buf); err != nil {
		return fmt.Errorf("fetch body: %w", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("parse message: %w", err)
	}
	for _, name := range []string{"Reply-To", "Content-Disposition"} {
		if err := h.store.InsertHeader(target.ID, strings.ToLower(name), parsed.Header.Get(name)); err != nil {
			return fmt.Errorf("insert header %q: %w", name, err)
		}
	}
	return nil
}

// reactionIdempotencyKey identifies one logical reaction. A client-supplied key
// scopes it to a single user action, so reacting again after an Undo enqueues a
// new send. Without one, the key falls back to the target row and emoji, which
// makes a lost response safe but permanently tombstones that combination. The
// row id, not the RFC Message-ID, is the target identity: two provider objects
// may legally share a Message-ID, and each must remain separately reactable.
func reactionIdempotencyKey(request reactionRequest, account string, targetRowID int64) string {
	identity := request.IdempotencyKey
	if identity == "" {
		identity = "derived\x00" + request.Emoji
	}
	digest := sha256.Sum256([]byte(account + "\x00" + strconv.FormatInt(targetRowID, 10) + "\x00" + identity))
	return fmt.Sprintf("reaction-v1-%x", digest)
}

// writeReactionError returns a machine-readable body so the GUI can show the
// server's reason — a failed Reply-To fetch in particular is worth retrying —
// rather than a bare status code.
func writeReactionError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": message}); err != nil {
		slog.Error("Failed to encode reaction error response", "module", "OUTBOX", "err", err)
	}
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
		if transitionErr := w.store.PoisonOutboxItem(item.ID, errMsg); transitionErr != nil {
			slog.Error("Failed to poison unrouteable outbox item", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
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
	if draft.Kind == outboxKindReaction {
		var err error
		msg.RawMIME, err = mailsend.BuildReaction(msg, time.Now())
		if err != nil {
			safeMsg := sanitizeOutboxError(err)
			slog.Error("Failed to build reaction MIME", "module", "OUTBOX", "id", item.ID, "err", safeMsg)
			if transitionErr := w.store.PoisonOutboxItem(item.ID, safeMsg); transitionErr != nil {
				slog.Error("Failed to poison unbuildable reaction", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
				return false
			}
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
			if transitionErr := w.store.MarkAttempted(item.ID, safeMsg); transitionErr != nil {
				slog.Error("Failed to record invalid outbox attachment", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
				return false
			}
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
		if transitionErr := w.store.MarkAttempted(item.ID, safeMsg); transitionErr != nil {
			slog.Error("Failed to record outbox sender setup failure", "module", "OUTBOX", "id", item.ID, "err", transitionErr)
			return false
		}
		w.broadcastStatus(item.ID, "failed", safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
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
		w.broadcastStatus(item.ID, "delivered_with_warning", reason, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
		return true
	}
	if err := w.store.DeleteClaimedOutboxItem(item.ID); err != nil {
		slog.Error("Sent outbox item could not be deleted; manual reconciliation required", "module", "OUTBOX", "id", item.ID, "err", err)
		return false
	}
	slog.Info("Outbox item sent successfully", "module", "OUTBOX", "id", item.ID)
	w.broadcastStatus(item.ID, "sent", "", draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
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
	w.broadcastStatus(item.ID, status, safeMsg, draft.Subject, strings.Join(draft.To, ", "), draft.Kind)
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
	// An empty row records that Reply-To was inspected and absent. This keeps
	// reactions to locally sent normal messages eligible without guessing.
	if err := w.store.InsertHeader(storeMsg.ID, "reply-to", ""); err != nil {
		slog.Warn("Failed to mark sent message Reply-To status", "module", "OUTBOX", "err", err)
	}
	contentDisposition := ""
	if draft.Kind == outboxKindReaction {
		contentDisposition = "reaction"
	}
	if err := w.store.InsertHeader(storeMsg.ID, "content-disposition", contentDisposition); err != nil {
		slog.Warn("Failed to mark sent message Content-Disposition", "module", "OUTBOX", "err", err)
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
