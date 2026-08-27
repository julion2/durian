package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/mail"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/backendfactory"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncengine"
)

// Probe cadence for engine-driven accounts.
//
// Microsoft Graph offers a desktop client no usable push transport: change
// notifications are delivered only to webhooks (public HTTPS endpoint), Event
// Hubs or Event Grid, and the endpoint-less socket transport Graph does expose
// covers driveItem/list resources, not mail. Polling is therefore the only
// option for a client that hosts no public endpoint.
//
// The intervals below are chosen against two published numbers. Graph's own
// change-notification latency for messages averages under a minute (max three),
// so a 30-second probe is not meaningfully slower than "real" push. And the
// Outlook throttling budget is 10,000 requests per 10 minutes per mailbox per
// app, with at most 4 concurrent: one inbox sync every 30s plus a full-mailbox
// pass every 5 minutes costs well under 1% of it, leaving room for the user's
// own reads on top.
const (
	// inboxIntervalActive is the inbox-only cadence while a GUI is attached.
	inboxIntervalActive = 30 * time.Second
	// inboxIntervalIdle is the inbox-only cadence with no GUI attached: mail
	// still arrives for the next launch, at a fraction of the battery cost.
	inboxIntervalIdle = 2 * time.Minute
	// fullIntervalActive is the all-folders cadence while a GUI is attached.
	// Non-inbox folders change almost only through the user's own actions,
	// which the app already applies locally.
	fullIntervalActive = 5 * time.Minute
	// fullIntervalIdle is the all-folders cadence with no GUI attached.
	fullIntervalIdle = 15 * time.Minute
	// probeJitter is the fraction each interval is randomly stretched or
	// shrunk by, so multiple accounts never align into synchronized bursts.
	probeJitter = 0.2
	// maxProbeBackoff caps the exponential backoff after repeated failures.
	maxProbeBackoff = 30 * time.Minute
	// A state-expiry replacement must finish atomically before its cursor can
	// advance. JMAP enumerates it in bounded pages, so recovery gets more time
	// than an ordinary pass without holding an account lock for an hour.
	syncTimeout         = 5 * time.Minute
	recoverySyncTimeout = 15 * time.Minute
	// pushReconnectBase is the initial delay before rebuilding a push backend
	// after its long-lived connection ends unexpectedly.
	pushReconnectBase = time.Second
	// maxPushReconnectBackoff prevents a broken push endpoint from being
	// hammered while the independent slow poll remains available as recovery.
	maxPushReconnectBackoff = time.Minute
)

// EngineWatcher keeps sync-engine accounts up to date inside `durian serve`.
//
// The IMAP IDLE watcher cannot serve these accounts: it runs the legacy IMAP
// syncer, which would ingest into IMAP-named mailboxes in parallel with the
// engine's provider-named ones and split the store into two incompatible views
// of one account. Before this existed, Graph accounts had no automated sync in
// the daemon at all — they moved only when the user ran `durian sync` by hand.
//
// Each account gets two loops: a fast inbox-only pass (arrival latency) and a
// slower full-mailbox pass (everything else, plus flag and folder-move
// reconciliation). Both funnel through one per-account mutex, so the two never
// overlap on the same store rows.
type EngineWatcher struct {
	hub            *EventHub
	store          *store.DB
	filterRules    []config.RuleConfig
	groups         map[string]config.GroupEntry
	indexedHeaders []string

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	triggers map[string]chan struct{}
}

// NewEngineWatcher creates an EngineWatcher. It mirrors NewWatcherManager's
// arguments so `durian serve` can wire both from the same config.
func NewEngineWatcher(hub *EventHub, st *store.DB, rules []config.RuleConfig, groups map[string]config.GroupEntry, indexedHeaders []string) *EngineWatcher {
	return &EngineWatcher{
		hub:            hub,
		store:          st,
		filterRules:    rules,
		groups:         groups,
		indexedHeaders: indexedHeaders,
		locks:          make(map[string]*sync.Mutex),
		triggers:       make(map[string]chan struct{}),
	}
}

// Start launches the watch loops for every account and blocks until ctx is
// done. Accounts are independent: one failing account never stops another.
func (w *EngineWatcher) Start(ctx context.Context, accounts []*config.AccountConfig) {
	w.RegisterAccounts(accounts)
	var wg sync.WaitGroup
	for i, account := range accounts {
		// Stagger account start-up so N accounts do not all fire their first
		// probe in the same second.
		offset := time.Duration(i) * 3 * time.Second
		w.mu.Lock()
		trigger := w.triggers[account.AccountIdentifier()]
		w.mu.Unlock()
		wg.Add(3)
		go func(a *config.AccountConfig, ch <-chan struct{}) {
			defer wg.Done()
			w.triggerLoop(ctx, a, ch)
		}(account, trigger)
		if w.pushEnabled(account) {
			go func(a *config.AccountConfig, off time.Duration) {
				defer wg.Done()
				w.pushLoop(ctx, a, off)
			}(account, offset)
			go func(a *config.AccountConfig, off time.Duration) {
				defer wg.Done()
				// Keep a slow full poll as recovery for a dropped push connection.
				w.loop(ctx, a, off+15*time.Second, false)
			}(account, offset)
			continue
		}
		go func(a *config.AccountConfig, off time.Duration) {
			defer wg.Done()
			w.loop(ctx, a, off, true)
		}(account, offset)
		go func(a *config.AccountConfig, off time.Duration) {
			defer wg.Done()
			// Offset the full pass from the inbox pass so they rarely collide
			// on the per-account lock.
			w.loop(ctx, a, off+15*time.Second, false)
		}(account, offset)
	}
	slog.Info("Started engine watchers", "module", "ENGINEWATCH", "accounts", len(accounts), // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		"inbox_interval_active", inboxIntervalActive, "full_interval_active", fullIntervalActive)
	wg.Wait()
}

// RegisterAccounts installs trigger queues synchronously, before the HTTP
// handler starts accepting mutations. Start calls it too for non-daemon users.
func (w *EngineWatcher) RegisterAccounts(accounts []*config.AccountConfig) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, account := range accounts {
		id := account.AccountIdentifier()
		if w.triggers[id] == nil {
			w.triggers[id] = make(chan struct{}, 1)
		}
	}
}

// TriggerSync schedules a coalesced upload-only engine pass for account.
func (w *EngineWatcher) TriggerSync(account string) {
	w.mu.Lock()
	trigger := w.triggers[account]
	w.mu.Unlock()
	if trigger == nil {
		return
	}
	select {
	case trigger <- struct{}{}:
	default:
	}
}

func (w *EngineWatcher) triggerLoop(ctx context.Context, account *config.AccountConfig, trigger <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			if err := w.syncAccountMode(ctx, account, false, syncengine.UploadOnly); err != nil {
				slog.Warn("Mutation-triggered engine sync failed", "module", "ENGINEWATCH", "account", account.AccountIdentifier()) // encgrep:allow account identifier; provider errors may be credential-tainted
			}
		}
	}
}

func (w *EngineWatcher) pushEnabled(account *config.AccountConfig) bool {
	return backendfactory.PushWatch(account)
}

// pushLoop converts provider push notifications into serialized full engine
// syncs. The buffered signal coalesces bursts while a sync is already running;
// JMAP state cursors make one pass sufficient to consume every queued change.
func (w *EngineWatcher) pushLoop(ctx context.Context, account *config.AccountConfig, startDelay time.Duration) {
	w.pushLoopWith(ctx, account, startDelay, backendfactory.New, w.syncAccount)
}

func (w *EngineWatcher) pushLoopWith(
	ctx context.Context,
	account *config.AccountConfig,
	startDelay time.Duration,
	newBackend func(*config.AccountConfig) (backend.Backend, error),
	syncAccount func(context.Context, *config.AccountConfig, bool) error,
) {
	if !sleepCtx(ctx, startDelay) {
		return
	}
	failures := 0
	inboxOnly := backendfactory.PushInboxOnly(account)
	for {
		// Establish a baseline before every (re)connection. A provider change
		// while push was disconnected is then covered before listening resumes.
		// Run this before constructing the watch backend: IMAP constructors open
		// their connection eagerly, and some servers reject a second concurrent
		// connection from the recovery sync.
		if err := syncAccount(ctx, account, inboxOnly); err != nil {
			slog.Warn("Push-backend recovery sync failed", "module", "ENGINEWATCH", "account", account.AccountIdentifier()) // encgrep:allow account identifier; provider errors may be credential-tainted
		}

		b, err := newBackend(account)
		if err != nil {
			failures++
			slog.Warn("Push backend creation failed", "module", "ENGINEWATCH", "account", account.AccountIdentifier()) // encgrep:allow account identifier; provider errors may be credential-tainted
			if !sleepCtx(ctx, pushReconnectDelay(failures)) {
				return
			}
			continue
		}

		changes := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- b.Watch(ctx, "", func() {
				select {
				case changes <- struct{}{}:
				default:
				}
			})
		}()
		disconnected := false
		for !disconnected {
			select {
			case <-ctx.Done():
				// Watch owns protocol goroutines and must finish before Close runs.
				<-done
				_ = b.Close()
				return
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("Push watch stopped; reconnecting", "module", "ENGINEWATCH", "account", account.AccountIdentifier()) // encgrep:allow account identifier; provider errors may be credential-tainted
				}
				disconnected = true
			case <-changes:
				failures = 0
				if err := syncAccount(ctx, account, inboxOnly); err != nil {
					slog.Warn("Push-triggered sync failed", "module", "ENGINEWATCH", "account", account.AccountIdentifier()) // encgrep:allow account identifier; provider errors may be credential-tainted
				}
			}
		}
		_ = b.Close()
		failures++
		if !sleepCtx(ctx, pushReconnectDelay(failures)) {
			return
		}
	}
}

func pushReconnectDelay(failures int) time.Duration {
	delay := pushReconnectBase
	for i := 1; i < failures && delay < maxPushReconnectBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxPushReconnectBackoff)
}

// loop runs one account's probe cycle. inboxOnly picks the fast inbox pass over
// the slow full-mailbox pass; startDelay staggers the first run.
func (w *EngineWatcher) loop(ctx context.Context, account *config.AccountConfig, startDelay time.Duration, inboxOnly bool) {
	if !sleepCtx(ctx, startDelay) {
		return
	}
	failures := 0
	for {
		if err := w.syncAccount(ctx, account, inboxOnly); err != nil {
			failures++
			slog.Warn("Engine watch sync failed", "module", "ENGINEWATCH", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"account", account.AccountIdentifier(), "inbox_only", inboxOnly,
				"failures", failures, "err", err)
		} else {
			failures = 0
		}
		if !sleepCtx(ctx, w.nextInterval(inboxOnly, failures)) {
			return
		}
	}
}

// nextInterval returns the wait before the next probe: the tier for this pass
// (active vs idle, inbox vs full), doubled once per consecutive failure up to
// maxProbeBackoff, then jittered.
//
// "Active" means at least one SSE client is connected — the GUI holds that
// stream open for its whole lifetime, so it is an exact proxy for "the user
// can actually see new mail arrive" without the daemon knowing anything about
// windows or focus.
func (w *EngineWatcher) nextInterval(inboxOnly bool, failures int) time.Duration {
	active := w.hub.HasClients()
	var base time.Duration
	switch {
	case inboxOnly && active:
		base = inboxIntervalActive
	case inboxOnly:
		base = inboxIntervalIdle
	case active:
		base = fullIntervalActive
	default:
		base = fullIntervalIdle
	}
	for i := 0; i < failures && base < maxProbeBackoff; i++ {
		base *= 2
	}
	if base > maxProbeBackoff {
		base = maxProbeBackoff
	}
	return jitter(base)
}

// jitter spreads d by ±probeJitter so concurrent loops drift apart instead of
// converging on the same instant.
func jitter(d time.Duration) time.Duration {
	spread := float64(d) * probeJitter
	return time.Duration(float64(d) + (rand.Float64()*2-1)*spread) //nolint:gosec // cadence spreading, not security
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// accountLock returns the per-account mutex serialising that account's syncs.
func (w *EngineWatcher) accountLock(account string) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()
	mu, ok := w.locks[account]
	if !ok {
		mu = &sync.Mutex{}
		w.locks[account] = mu
	}
	return mu
}

// syncAccount runs one engine pass and broadcasts any new inbox arrivals.
func (w *EngineWatcher) syncAccount(ctx context.Context, account *config.AccountConfig, inboxOnly bool) error {
	return w.syncAccountMode(ctx, account, inboxOnly, syncengine.Bidirectional)
}

func (w *EngineWatcher) syncAccountMode(ctx context.Context, account *config.AccountConfig, inboxOnly bool, mode syncengine.Mode) error {
	mu := w.accountLock(account.AccountIdentifier())
	mu.Lock()
	defer mu.Unlock()

	b, err := backendfactory.New(account)
	if err != nil {
		return fmt.Errorf("connect backend: %w", err)
	}
	defer b.Close()

	folders := watchedFolders(inboxOnly, b.Capabilities())

	suffix := backendfactory.CursorSuffix(account)
	var cursors syncengine.CursorStore = syncengine.NewFileCursorStore(account.AccountIdentifier())
	if suffix != "" {
		cursors = syncengine.NewFileCursorStoreWithSuffix(account.AccountIdentifier(), suffix)
	}
	engine := syncengine.New(syncengine.Options{
		Store:      w.store,
		Cursors:    cursors,
		Account:    account.AccountIdentifier(),
		BatchLimit: account.GetIMAPBatchSize(),
		// Keep provider-engine semantics aligned with `durian sync`: an explicit
		// zero means full local-first sync. Daemon passes retain the 5000-message
		// safety cap so a large initial sync yields before its watchdog.
		MaxPerFolder:    account.GetIMAPMaxMessages(),
		Folders:         folders,
		Mode:            mode,
		Timeout:         syncTimeout,
		RecoveryTimeout: recoverySyncTimeout,
		Ingest: syncengine.IngestOptions{
			Account:        account.AccountIdentifier(),
			FilterRules:    w.filterRules,
			Groups:         w.groups,
			IndexedHeaders: w.indexedHeaders,
		},
	})

	res, err := engine.Sync(ctx, b)
	if err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("engine sync completed with %d folder errors: %w", len(res.Errors), res.Errors[0])
	}
	if res.New > 0 || res.Deleted > 0 || res.Moved > 0 {
		slog.Info("Engine watch sync applied changes", "module", "ENGINEWATCH", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
			"account", account.AccountIdentifier(), "new", res.New,
			"deleted", res.Deleted, "moved", res.Moved, "inbox_only", inboxOnly)
	}
	w.broadcastNewMail(account, res.NewMessageIdentifiers)
	return nil
}

func watchedFolders(inboxOnly bool, capabilities backend.Capabilities) []string {
	// Matched against the folder's role, so this stays correct on a Graph
	// mailbox whose inbox is an opaque id displayed in another language.
	// Label-stream backends expose one account-wide RoleAll stream; arrivals are
	// filtered by their inbox label after ingestion instead.
	if inboxOnly && !capabilities.LabelsAreTags {
		return []string{"INBOX"}
	}
	return nil
}

// broadcastNewMail turns the engine's new inbox arrivals into a NewMailEvent.
// Unlike the IMAP watcher's UIDNEXT diffing, this works for any backend: the
// engine already knows which messages it created this run.
func (w *EngineWatcher) broadcastNewMail(account *config.AccountConfig, identifiers []string) {
	if len(identifiers) == 0 {
		return
	}
	messages := newMailInfos(w.store, account.Email, identifiers)
	if len(messages) == 0 {
		return
	}
	slog.Info("Broadcasting new messages", "module", "ENGINEWATCH", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
		"account", account.Email, "count", len(messages))
	w.hub.Broadcast(NewMailEvent{
		Account:  account.Email,
		TotalNew: len(messages),
		Messages: messages,
	})
}

// newMailInfos resolves exact local identifiers or legacy Message-IDs to the
// payload a NewMailEvent carries,
// skipping anything the store cannot resolve. Shared by both watchers: the
// IMAP one arrives here from a UID search, the engine one from the ingest
// result, and from this point on a new message is a new message.
func newMailInfos(st *store.DB, account string, identifiers []string) []NewMailInfo {
	messages := make([]NewMailInfo, 0, len(identifiers))
	for _, identifier := range identifiers {
		msg, err := st.GetByIdentifier(identifier)
		if err != nil {
			slog.Error("Store lookup failed", "module", "WATCH", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"account", account, "message_id", identifier, "err", err)
			continue
		}
		if msg == nil {
			slog.Debug("Message not yet in store", "module", "WATCH", // encgrep:allow wrapper-protected slog key per redact.SensitiveSlogKeys
				"account", account, "message_id", identifier)
			continue
		}
		from := msg.FromAddr
		if addr, err := mail.ParseAddress(msg.FromAddr); err == nil && addr.Name != "" {
			from = addr.Name
		}
		messages = append(messages, NewMailInfo{
			ThreadID: msg.ThreadID,
			Subject:  msg.Subject,
			From:     from,
			Snippet:  cleanSnippet(msg.BodyText, 150),
		})
		// ADR-0001 §6 redaction: thread/id observability without leaking from/subject.
		slog.Info("New mail", "module", "WATCH", "account", account, "thread", msg.ThreadID) // encgrep:allow account email plaintext per ADR-0001 §3
	}
	return messages
}
