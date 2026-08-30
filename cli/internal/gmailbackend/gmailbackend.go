// Package gmailbackend implements the provider-agnostic backend.Backend
// interface on the Gmail REST API, as a third Backend next to imapbackend and
// graphbackend. Gmail has no folders — a message carries multiple labels — so
// this backend syncs a single "All Mail" stream and reports each message's
// labels, which the engine maps to Durian tags. The sync engine, store, ingest
// and flag logic stay provider-neutral.
//
// OAuth: Gmail accounts already hold the https://mail.google.com/ scope, which
// is a Gmail REST API full-access scope, so the existing Google token works
// here without any additional consent.
//
// Cursor encoding: an opaque JSON token. While the initial full sync is still
// paging it carries the messages.list pageToken plus a historyId snapshot taken
// at the start of the sync; once paging completes it carries that snapshot as
// the resume point for the incremental history sync (users.history.list). An
// empty cursor starts a fresh full sync (safe — the store upserts by
// Message-ID); an expired historyId (404) restarts one.
package gmailbackend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/oauth"
	"github.com/julion2/durian/cli/internal/redact"
)

const (
	defaultBaseURL = "https://gmail.googleapis.com/gmail/v1"
	// tokenExpiryBuffer refreshes the access token slightly before it expires.
	tokenExpiryBuffer = 2 * time.Minute
	// RAW messages are base64url-encoded inside JSON; leave room for Gmail's
	// largest accepted messages plus encoding overhead while still bounding
	// every response allocation.
	maxJSONBytes = 128 << 20
	// allMailStream is the single synthetic folder the engine iterates: Gmail is
	// folderless, so all messages flow through one stream and their labels become
	// tags. "me" is Gmail's alias for the authenticated user in message queries.
	allMailStream = "ALL"

	// Gmail's reserved system labels that map to flag state rather than tags.
	labelUnread  = "UNREAD"
	labelStarred = "STARRED"
)

// errNotImplemented marks the write methods this read-focused backend does not
// yet provide. The engine only drives them once Gmail is routed onto it.
var errNotImplemented = errors.New("gmailbackend: not implemented")

// Backend implements backend.Backend on the Gmail REST API.
type Backend struct {
	account      *config.AccountConfig
	clientID     string
	clientSecret string

	httpClient *http.Client
	// baseURL is the Gmail API root without trailing slash. Defaults to
	// defaultBaseURL; tests point it at an httptest.Server.
	baseURL string
	// tokenFn returns a valid Google bearer token. Defaults to cachedToken;
	// tests override it.
	tokenFn func(ctx context.Context) (string, error)

	mu          sync.Mutex
	cachedToken *oauth.Token
	// labels maps Gmail label ids to Durian tag names (system labels via a fixed
	// table, user labels to their display name), reloaded each FetchMessages call.
	labels map[string]string
	// tagToID is the inverse of labels (Durian tag -> Gmail label id), for the
	// label-upload path. System labels win a tag collision so removing "inbox"
	// targets Gmail's INBOX, not a user label that happens to sanitize the same.
	tagToID map[string]string
}

var _ backend.SnapshotHydrator = (*Backend)(nil)

// Compile-time check that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)
var _ backend.LabelWriter = (*Backend)(nil)

// New creates a Gmail backend for the given Google OAuth account. The token is
// fetched lazily on first use, so construction never touches the network.
func New(account *config.AccountConfig) (*Backend, error) {
	if account.OAuth == nil || account.OAuth.Provider != "google" {
		return nil, fmt.Errorf("gmail backend requires a Google OAuth account, got %s", account.Email)
	}
	b := &Backend{
		account:      account,
		clientID:     account.OAuth.ClientID,
		clientSecret: account.OAuth.ClientSecret,
		baseURL:      defaultBaseURL,
	}
	b.httpClient = &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return validateAuthenticatedURL(b.baseURL, req.URL.String())
		},
	}
	b.tokenFn = b.cachedGoogleToken
	return b, nil
}

// cachedGoogleToken returns the cached Google access token, minting a fresh one
// via oauth.GetValidToken when none is cached or it is about to expire. The
// mail.google.com scope the token already carries is accepted by the Gmail API.
func (b *Backend) cachedGoogleToken(_ context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cachedToken != nil && !b.cachedToken.IsExpiredWithBuffer(tokenExpiryBuffer) {
		return b.cachedToken.AccessToken, nil
	}
	token, err := oauth.GetValidToken(b.account.GetAuthEmail(), b.clientID, b.clientSecret, "")
	if err != nil {
		return "", fmt.Errorf("failed to get Google token for %s: %w", b.account.Email, err)
	}
	b.cachedToken = token
	slog.Debug("Minted Google access token", "module", "GMAILBACKEND",
		"account", b.account.Name, "expiry", token.Expiry) // account config name, not the user's email (PII)
	return token.AccessToken, nil
}

// MARK: - HTTP client

// statusError is a non-2xx Gmail response, carrying the HTTP status so callers
// can react to specific codes (e.g. a 404 historyId that must restart a sync).
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("gmail request failed: status %d: %s", e.status, e.body)
}

func (e *statusError) SafeLogText() string {
	return fmt.Sprintf("gmail request failed: status %d: response body %s", e.status, redact.Placeholder)
}

var _ redact.SafeLogError = (*statusError)(nil)

// do executes one authenticated Gmail request with throttle handling: for
// idempotent HTTP methods it retries
// 429 and quota 403 (rateLimitExceeded / userRateLimitExceeded) honoring the
// Retry-After header, and retries any 5xx once, all with exponential backoff and
// respecting ctx. A non-retryable response (including a permission 403) is
// returned with its body intact so the caller can read the error.
func (b *Backend) do(ctx context.Context, method, reqURL string, body []byte) (*http.Response, error) {
	return b.doWithRetry(ctx, method, reqURL, body, method == http.MethodGet || method == http.MethodHead)
}

func (b *Backend) doWithRetry(ctx context.Context, method, reqURL string, body []byte, safeToRetry bool) (*http.Response, error) {
	const maxRetries = 3

	if err := validateAuthenticatedURL(b.baseURL, reqURL); err != nil {
		return nil, fmt.Errorf("refusing Gmail request URL: %w", err)
	}

	backoff := time.Second
	transientRetried := false
	for attempt := 0; ; attempt++ {
		token, err := b.tokenFn(ctx)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
		if err != nil {
			return nil, fmt.Errorf("failed to build gmail request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := b.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gmail request failed: %w", err)
		}

		switch {
		case safeToRetry && resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries:
			delay := retryAfter(resp, backoff)
			drainClose(resp)
			slog.Warn("Gmail throttled (429), backing off", "module", "GMAILBACKEND", "retry", attempt+1, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			backoff *= 2
		case safeToRetry && resp.StatusCode == http.StatusForbidden && attempt < maxRetries:
			// A 403 is either a quota error (retryable) or a real permission
			// error (not). Buffer the body to tell them apart without losing it.
			buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			drainClose(resp)
			if !isQuotaBody(buf) {
				resp.Body = io.NopCloser(bytes.NewReader(buf))
				return resp, nil
			}
			delay := retryAfter(resp, backoff)
			slog.Warn("Gmail quota (403), backing off", "module", "GMAILBACKEND", "retry", attempt+1, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			backoff *= 2
		case safeToRetry && resp.StatusCode >= http.StatusInternalServerError && !transientRetried:
			transientRetried = true
			drainClose(resp)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
		default:
			return resp, nil
		}
	}
}

// retryAfter returns the Retry-After header as a duration (delay-seconds form),
// or fallback when the header is absent or unparseable.
func retryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
}

// isQuotaBody reports whether a Gmail 403 body is a rate-limit / quota error
// (retryable) rather than a permission error.
func isQuotaBody(b []byte) bool {
	return bytes.Contains(b, []byte("rateLimitExceeded")) ||
		bytes.Contains(b, []byte("userRateLimitExceeded")) ||
		bytes.Contains(b, []byte("quotaExceeded"))
}

// doJSON executes a Gmail request and decodes the JSON response into out (out
// may be nil to discard the body).
func (b *Backend) doJSON(ctx context.Context, method, reqURL string, body any, out any) error {
	return b.doJSONWithRetry(ctx, method, reqURL, body, out, method == http.MethodGet || method == http.MethodHead)
}

func (b *Backend) doJSONWithRetry(ctx context.Context, method, reqURL string, body any, out any, safeToRetry bool) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}
	resp, err := b.doWithRetry(ctx, method, reqURL, payload, safeToRetry)
	if err != nil {
		return err
	}
	defer drainClose(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStatusError(resp)
	}
	if out == nil {
		return nil
	}
	if err := decodeJSONLimited(resp.Body, maxJSONBytes, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func validateAuthenticatedURL(baseURL, requestURL string) error {
	base, err := url.Parse(baseURL)
	if err != nil || !base.IsAbs() || base.User != nil {
		return errors.New("Gmail base URL is invalid")
	}
	target, err := url.Parse(requestURL)
	if err != nil || !target.IsAbs() || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("authenticated URL must be an absolute HTTP(S) URL without userinfo")
	}
	if !strings.EqualFold(base.Scheme, target.Scheme) ||
		!strings.EqualFold(base.Hostname(), target.Hostname()) || effectivePort(base) != effectivePort(target) {
		return errors.New("authenticated URL origin differs from Gmail API origin")
	}
	return nil
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func decodeJSONLimited(r io.Reader, limit int64, out any) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("Gmail JSON response exceeds %d bytes", limit)
	}
	return json.Unmarshal(data, out)
}

func newStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &statusError{status: resp.StatusCode, body: string(body)}
}

func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MARK: - Cursor

// gmailCursor is the opaque per-stream cursor: during the initial sync it holds
// the messages.list pageToken plus the start-of-sync historyId snapshot; once
// complete it holds only the historyId resume point.
type gmailCursor struct {
	PageToken string `json:"p,omitempty"`
	HistoryID string `json:"h,omitempty"`
	// Snapshot is the mailbox historyId captured at the start of the initial
	// full sync. It becomes the resume point when paging completes, so a change
	// made mid-sync (which gets a higher historyId) is not missed.
	Snapshot string `json:"s,omitempty"`
}

func decodeCursor(c backend.Cursor) gmailCursor {
	var gc gmailCursor
	if len(c) > 0 {
		_ = json.Unmarshal(c, &gc)
	}
	return gc
}

func encodeCursor(gc gmailCursor) backend.Cursor {
	b, _ := json.Marshal(gc)
	return b
}

// MARK: - Folders

// FetchFolders returns the single synthetic stream Gmail syncs through. Gmail is
// folderless; labels become tags, so there is one stream rather than a folder
// per label.
func (b *Backend) FetchFolders(_ context.Context) ([]backend.Folder, error) {
	return []backend.Folder{{
		Name:       allMailStream,
		Display:    "All Mail",
		Role:       backend.RoleAll,
		Selectable: true,
	}}, nil
}

// MARK: - Messages

// gmailMessageList is one page of users.messages.list.
type gmailMessageList struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	NextPageToken string `json:"nextPageToken"`
}

// gmailMessage is one users.messages.get?format=RAW response.
type gmailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	LabelIDs     []string `json:"labelIds"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"` // ms since epoch, as a string
	Raw          string   `json:"raw"`          // base64url RFC822
}

// FetchMessages returns the changes in the stream since cursor. With no
// historyId yet it runs the initial (paged) full sync via messages.list; once
// that completed the cursor carries a historyId and it runs the incremental
// history sync via history.list. An expired historyId (404) restarts a full sync.
func (b *Backend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	if err := b.loadLabels(ctx); err != nil {
		return backend.FetchResult{}, fmt.Errorf("load labels: %w", err)
	}
	gc := decodeCursor(cursor)
	if gc.HistoryID != "" {
		res, err := b.historySync(ctx, folder, gc, limit)
		if isNotFound(err) {
			slog.Info("Gmail historyId expired, reconciling remote IDs", "module", "GMAILBACKEND")
			return b.replacementSnapshot(ctx, folder)
		}
		return res, err
	}
	return b.initialList(ctx, folder, gc, limit)
}

// replacementSnapshot cheaply reconciles an expired history cursor from
// messages.list IDs only. The history snapshot is captured first, so changes
// concurrent with the listing are replayed by the next incremental sync.
func (b *Backend) replacementSnapshot(ctx context.Context, folder string) (backend.FetchResult, error) {
	snapshot, err := b.profileHistoryID(ctx)
	if err != nil {
		return backend.FetchResult{}, fmt.Errorf("snapshot historyId: %w", err)
	}
	q := url.Values{}
	q.Set("includeSpamTrash", "true")
	q.Set("maxResults", "500")
	var refs []backend.RemoteRef
	priorToken := ""
	for {
		var page gmailMessageList
		if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/messages?"+q.Encode(), nil, &page); err != nil {
			return backend.FetchResult{}, fmt.Errorf("list replacement snapshot: %w", err)
		}
		for _, message := range page.Messages {
			if message.ID != "" {
				refs = append(refs, backend.RemoteRef{Folder: folder, ID: message.ID})
			}
		}
		if page.NextPageToken == "" {
			break
		}
		if page.NextPageToken == priorToken {
			return backend.FetchResult{}, errors.New("Gmail replacement snapshot pagination made no progress")
		}
		priorToken = page.NextPageToken
		q.Set("pageToken", page.NextPageToken)
	}
	if len(refs) == 0 {
		// A false-empty authoritative snapshot would delete the account's
		// complete local read model. Gmail exposes no exact total with which to
		// distinguish that response from a genuinely empty mailbox, so prefer a
		// retry over irreversible local deletion.
		return backend.FetchResult{}, errors.New("Gmail replacement snapshot returned no messages")
	}
	return backend.FetchResult{
		Cursor:       encodeCursor(gmailCursor{HistoryID: snapshot}),
		FullSnapshot: true,
		Present:      refs,
	}, nil
}

// FetchSnapshotMessages hydrates only replacement-snapshot refs absent from
// Durian's local read model; the engine determines that set before calling.
func (b *Backend) FetchSnapshotMessages(ctx context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	if len(refs) == 0 {
		return backend.SnapshotBatch{}, nil
	}
	folder := refs[0].Folder
	batch := backend.SnapshotBatch{}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	messages, missing, err := b.fetchMany(ctx, folder, ids)
	batch.Messages = messages
	for _, id := range missing {
		batch.Missing = append(batch.Missing, backend.RemoteRef{Folder: folder, ID: id})
	}
	return batch, err
}

// FetchSnapshotMetadata refreshes flags and labels for replacement-snapshot
// refs already in Durian without downloading their raw MIME bodies.
func (b *Backend) FetchSnapshotMetadata(ctx context.Context, refs []backend.RemoteRef) (backend.SnapshotBatch, error) {
	messages := make([]backend.Message, len(refs))
	errs := make([]error, len(refs))
	missing := make([]bool, len(refs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	for i := range refs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return backend.SnapshotBatch{}, ctx.Err()
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			ref := refs[i]
			var gm gmailMessage
			endpoint := b.baseURL + "/users/me/messages/" + url.PathEscape(ref.ID) + "?format=MINIMAL"
			if err := b.doJSON(ctx, http.MethodGet, endpoint, nil, &gm); err != nil {
				if isNotFound(err) {
					missing[i] = true
				} else {
					errs[i] = err
				}
				return
			}
			messages[i] = backend.Message{
				Ref:   backend.RemoteRef{Folder: ref.Folder, ID: gm.ID},
				Flags: flagsFromLabels(gm.LabelIDs), Labels: b.resolveLabels(gm.LabelIDs),
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return backend.SnapshotBatch{}, fmt.Errorf("fetch snapshot metadata: %w", err)
		}
	}
	batch := backend.SnapshotBatch{Messages: messages[:0]}
	for i, message := range messages {
		if missing[i] {
			batch.Missing = append(batch.Missing, refs[i])
			continue
		}
		batch.Messages = append(batch.Messages, message)
	}
	return batch, nil
}

// profileHistoryID returns the mailbox's current historyId (users.getProfile),
// used to snapshot the start point of a full sync.
func (b *Backend) profileHistoryID(ctx context.Context) (string, error) {
	var p struct {
		HistoryID string `json:"historyId"`
	}
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/profile", nil, &p); err != nil {
		return "", err
	}
	return p.HistoryID, nil
}

// initialList runs the first full sync: messages.list paged with a pageToken,
// each id fetched raw. A historyId snapshot taken at the very start becomes the
// resume point once paging completes, so a change made mid-sync (which gets a
// higher historyId) is caught by the first incremental round rather than lost —
// capturing it from the fetched messages would miss changes whose id falls below
// the last page's maximum.
func (b *Backend) initialList(ctx context.Context, folder string, gc gmailCursor, limit int) (backend.FetchResult, error) {
	var result backend.FetchResult

	snapshot := gc.Snapshot
	if snapshot == "" {
		var err error
		if snapshot, err = b.profileHistoryID(ctx); err != nil {
			return result, fmt.Errorf("snapshot historyId: %w", err)
		}
	}

	q := url.Values{}
	q.Set("includeSpamTrash", "true") // Spam/Trash carry TRASH/SPAM labels -> trash/spam tags
	if limit > 0 {
		q.Set("maxResults", strconv.Itoa(limit))
	}
	if gc.PageToken != "" {
		q.Set("pageToken", gc.PageToken)
	}
	var page gmailMessageList
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/messages?"+q.Encode(), nil, &page); err != nil {
		return result, fmt.Errorf("list messages: %w", err)
	}

	ids := make([]string, len(page.Messages))
	for i, m := range page.Messages {
		ids[i] = m.ID
	}
	msgs, _, err := b.fetchMany(ctx, folder, ids)
	if err != nil {
		return result, err
	}
	result.Messages = msgs

	if page.NextPageToken != "" {
		result.Cursor = encodeCursor(gmailCursor{PageToken: page.NextPageToken, Snapshot: snapshot})
		result.HasMore = true
	} else {
		result.Cursor = encodeCursor(gmailCursor{HistoryID: snapshot})
	}
	return result, nil
}

// fetched bundles a converted message with its historyId (used to advance the
// cursor without threading it through backend.Message).
type fetched struct {
	message   backend.Message
	historyID uint64
}

func (b *Backend) fetchOne(ctx context.Context, folder, id string) (fetched, error) {
	var gm gmailMessage
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/messages/"+url.PathEscape(id)+"?format=RAW", nil, &gm); err != nil {
		return fetched{}, err
	}
	raw, err := decodeRawMIME(gm.Raw)
	if err != nil {
		return fetched{}, fmt.Errorf("decode raw MIME: %w", err)
	}
	hid, _ := strconv.ParseUint(gm.HistoryID, 10, 64)
	return fetched{
		message: backend.Message{
			Ref:          backend.RemoteRef{Folder: folder, ID: gm.ID},
			Raw:          raw,
			Flags:        flagsFromLabels(gm.LabelIDs),
			Labels:       b.resolveLabels(gm.LabelIDs),
			InternalDate: parseInternalDate(gm.InternalDate),
		},
		historyID: hid,
	}, nil
}

// fetchConcurrency bounds simultaneous message.get RAW downloads. Gmail caps a
// user at ~250 quota units/sec and messages.get costs 5 (~50 msgs/sec), so this
// is sized to saturate that ceiling — turning a serial full sync (minutes) into
// a quota-bound one — while do() backs off on the 429s that steady state hits.
const fetchConcurrency = 20

// fetchMany downloads the raw MIME for each id concurrently, preserving order.
// A 404 (message deleted since it was listed) is skipped; any other error fails
// the whole call so the caller does not advance the cursor past a lost message.
func (b *Backend) fetchMany(ctx context.Context, folder string, ids []string) ([]backend.Message, []string, error) {
	msgs := make([]backend.Message, len(ids))
	errs := make([]error, len(ids))
	missing := make([]bool, len(ids))

	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	for i := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			m, err := b.fetchOne(ctx, folder, ids[i])
			if err != nil {
				if isNotFound(err) {
					missing[i] = true
				} else {
					errs[i] = err // own slot: no shared write, no mutex
				}
				return // 404 -> leave msgs[i] zero, filtered out below
			}
			msgs[i] = m.message
		}(i)
	}
	wg.Wait()

	for _, e := range errs {
		if e != nil {
			return nil, nil, fmt.Errorf("fetch message: %w", e)
		}
	}
	// Drop skipped (zero) entries in place, preserving order.
	out := msgs[:0]
	var missingIDs []string
	for i, m := range msgs {
		if missing[i] {
			missingIDs = append(missingIDs, ids[i])
		} else {
			out = append(out, m)
		}
	}
	return out, missingIDs, nil
}

// gmailHistoryRef is a message reference inside a history record.
type gmailHistoryRef struct {
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
}

// gmailHistoryList is one page of users.history.list.
type gmailHistoryList struct {
	History []struct {
		MessagesAdded   []gmailHistoryRef `json:"messagesAdded"`
		MessagesDeleted []gmailHistoryRef `json:"messagesDeleted"`
		LabelsAdded     []gmailHistoryRef `json:"labelsAdded"`
		LabelsRemoved   []gmailHistoryRef `json:"labelsRemoved"`
	} `json:"history"`
	NextPageToken string `json:"nextPageToken"`
	HistoryID     string `json:"historyId"`
}

// historySync runs the incremental sync from gc.HistoryID via history.list. A
// message that was added or whose labels changed is re-fetched so its current
// flags/labels re-ingest; a deleted message becomes a Deletion. When both occur
// in the same window the deletion wins.
func (b *Backend) historySync(ctx context.Context, folder string, gc gmailCursor, limit int) (backend.FetchResult, error) {
	var result backend.FetchResult
	q := url.Values{}
	q.Set("startHistoryId", gc.HistoryID)
	// Bound the page: maxResults caps history RECORDS — a soft cap on bodies
	// (most records carry one message), enough to honor the engine's per-call
	// limit hint, with the remainder paged via nextPageToken.
	if limit > 0 {
		q.Set("maxResults", strconv.Itoa(limit))
	}
	if gc.PageToken != "" {
		q.Set("pageToken", gc.PageToken)
	}
	for _, t := range []string{"messageAdded", "messageDeleted", "labelAdded", "labelRemoved"} {
		q.Add("historyTypes", t)
	}
	var page gmailHistoryList
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/history?"+q.Encode(), nil, &page); err != nil {
		return result, err
	}

	// Collect changed (added or relabeled) and deleted ids, each in first-seen
	// order so the output is deterministic.
	var changedOrder, deletedOrder []string
	changed := make(map[string]bool)
	deleted := make(map[string]bool)
	addTo := func(order *[]string, seen map[string]bool, id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		*order = append(*order, id)
	}
	for _, h := range page.History {
		for _, a := range h.MessagesAdded {
			addTo(&changedOrder, changed, a.Message.ID)
		}
		for _, la := range h.LabelsAdded {
			addTo(&changedOrder, changed, la.Message.ID)
		}
		for _, lr := range h.LabelsRemoved {
			addTo(&changedOrder, changed, lr.Message.ID)
		}
		for _, d := range h.MessagesDeleted {
			addTo(&deletedOrder, deleted, d.Message.ID)
		}
	}

	var toFetch []string
	for _, id := range changedOrder {
		if !deleted[id] { // a deletion wins over an add/relabel in the same window
			toFetch = append(toFetch, id)
		}
	}
	msgs, _, err := b.fetchMany(ctx, folder, toFetch)
	if err != nil {
		return result, err
	}
	result.Messages = msgs
	for _, id := range deletedOrder {
		result.Deleted = append(result.Deleted, backend.Deletion{Ref: backend.RemoteRef{Folder: folder, ID: id}})
	}

	if page.NextPageToken != "" {
		result.Cursor = encodeCursor(gmailCursor{HistoryID: gc.HistoryID, PageToken: page.NextPageToken})
		result.HasMore = true
	} else {
		// history.list echoes the latest historyId; persist it as the next resume
		// point (fall back to the start id if the response omitted it).
		next := page.HistoryID
		if next == "" {
			next = gc.HistoryID
		}
		result.Cursor = encodeCursor(gmailCursor{HistoryID: next})
	}
	return result, nil
}

// isNotFound reports a 404 statusError: a single message that was deleted, or a
// history.list startHistoryId too old for Gmail to serve (must restart a sync).
func isNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// FetchBody streams the full RFC822 message for ref via messages.get?format=RAW.
func (b *Backend) FetchBody(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
	var gm gmailMessage
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/messages/"+url.PathEscape(ref.ID)+"?format=RAW", nil, &gm); err != nil {
		return fmt.Errorf("fetch body for %s: %w", ref.ID, err)
	}
	raw, err := decodeRawMIME(gm.Raw)
	if err != nil {
		return fmt.Errorf("decode raw MIME for %s: %w", ref.ID, err)
	}
	_, err = w.Write(raw)
	return err
}

// MARK: - Label / flag mapping

// flagsFromLabels derives neutral flag state from Gmail's reserved labels:
// a message is seen unless it carries UNREAD, and flagged when it carries
// STARRED.
func flagsFromLabels(labels []string) backend.Flags {
	f := backend.Flags{Seen: true}
	for _, l := range labels {
		switch l {
		case labelUnread:
			f.Seen = false
		case labelStarred:
			f.Flagged = true
		}
	}
	return f
}

// systemLabelTags maps Gmail's reserved system labels to Durian's canonical
// tags (IMPORTANT -> important mirrors the legacy IMAP \Important mapping).
// Labels not listed are intentionally not tagged: STARRED/UNREAD are flags, and
// CATEGORY_* / CHAT would only add noise.
var systemLabelTags = map[string]string{
	"INBOX":     "inbox",
	"SENT":      "sent",
	"DRAFT":     "draft",
	"TRASH":     "trash",
	"SPAM":      "spam",
	"IMPORTANT": "important",
}

// gmailLabel is one entry of users.labels.list.
type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "system" or "user"
}

// labelToTag maps one Gmail label to a Durian tag name, or "" when it should not
// become a tag. System labels use the fixed table; user labels use their display
// name via sanitizeTag (a nested "Parent/Child" keeps its slash).
func labelToTag(l gmailLabel) string {
	if l.ID == labelUnread || l.ID == labelStarred {
		return ""
	}
	if l.Type == "system" {
		return systemLabelTags[l.ID]
	}
	return sanitizeTag(l.Name)
}

// sanitizeTag normalizes a user label name to a tag: lowercased, trimmed, with
// inner spaces collapsed to dashes (parity with the IMAP label path, so the same
// label yields the same tag on either backend).
func sanitizeTag(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
}

// loadLabels (re)builds the label-id -> tag-name map from users.labels.list. It
// runs once per FetchMessages call so newly created labels resolve on the next
// sync rather than only after a restart.
func (b *Backend) loadLabels(ctx context.Context) error {
	var resp struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := b.doJSON(ctx, http.MethodGet, b.baseURL+"/users/me/labels", nil, &resp); err != nil {
		b.mu.Lock()
		haveCache := b.labels != nil
		b.mu.Unlock()
		if haveCache {
			// Labels change rarely, so reuse the last map rather than failing the
			// whole sync on a momentary labels.list outage.
			slog.Warn("labels.list failed, reusing cached labels", "module", "GMAILBACKEND", "err", err)
			return nil
		}
		return err
	}
	m := make(map[string]string, len(resp.Labels))
	rev := make(map[string]string, len(resp.Labels))
	for _, l := range resp.Labels {
		tag := labelToTag(l)
		if tag == "" {
			continue
		}
		m[l.ID] = tag
		// Prefer the system label id on a tag collision (a user label named e.g.
		// "Inbox" sanitizes to "inbox" too) so uploads target the reserved label.
		if _, exists := rev[tag]; !exists || l.Type == "system" {
			rev[tag] = l.ID
		}
	}
	b.mu.Lock()
	b.labels = m
	b.tagToID = rev
	b.mu.Unlock()
	return nil
}

// resolveLabels turns a message's label ids into Durian tag names, dropping the
// reserved flag labels (carried as flags) and any id with no tag mapping.
func (b *Backend) resolveLabels(ids []string) []string {
	b.mu.Lock()
	cache := b.labels
	b.mu.Unlock()

	var out []string
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		name, ok := cache[id]
		if !ok || seen[name] {
			continue // unmapped id, or a duplicate tag (e.g. a user "Inbox" label)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// decodeRawMIME decodes Gmail's URL-safe base64 raw body, tolerating both the
// padded and unpadded forms.
func decodeRawMIME(s string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// parseInternalDate converts Gmail's millisecond-epoch string to a time.Time.
func parseInternalDate(ms string) time.Time {
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

// MARK: - Capabilities

func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		// history.list reports label changes (including UNREAD/STARRED), so a
		// server-side flag change surfaces in the incremental stream — the engine
		// reconciles flags from it (via FetchFlags) instead of polling every message.
		FlagChangesInDelta: true,
		// Message.Labels carries each message's Gmail labels as tag names; the
		// engine mirrors them to tags instead of folder-role mapping.
		LabelsAreTags: true,
		// Gmail has no \Answered label, so the engine must not let a local
		// "replied" tag drive the flag three-way (it would ping-pong every sync).
		AnsweredUnsupported: true,
	}
}

func (b *Backend) Close() error { return nil }

// MARK: - Flags

// ApplyFlags maps neutral flag changes to Gmail label modifications: Seen is the
// absence of UNREAD, Flagged is STARRED. Gmail cannot represent Answered or the
// $Completed keyword — and a local "replied" tag DOES yield Answered=true, so the
// engine wiring MUST scope a Gmail account's three-way flag merge to the flags
// Gmail supports (Seen, Flagged). Otherwise the baseline advances past a flag
// Gmail never stores, and the next sync "downloads" it away, silently removing
// the tag. Here we apply only what Gmail can represent.
func (b *Backend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	var addLabels, removeLabels []string
	if add.Seen {
		removeLabels = append(removeLabels, labelUnread)
	}
	if remove.Seen {
		addLabels = append(addLabels, labelUnread)
	}
	if add.Flagged {
		addLabels = append(addLabels, labelStarred)
	}
	if remove.Flagged {
		removeLabels = append(removeLabels, labelStarred)
	}
	if len(addLabels) == 0 && len(removeLabels) == 0 {
		return nil
	}

	body := make(map[string][]string, 2)
	if len(addLabels) > 0 {
		body["addLabelIds"] = addLabels
	}
	if len(removeLabels) > 0 {
		body["removeLabelIds"] = removeLabels
	}
	if err := b.doJSONWithRetry(ctx, http.MethodPost,
		b.baseURL+"/users/me/messages/"+url.PathEscape(ref.ID)+"/modify", body, nil, true); err != nil {
		return fmt.Errorf("modify labels for %s: %w", ref.ID, err)
	}
	slog.Debug("Applied flags", "module", "GMAILBACKEND", "id", ref.ID, "add", addLabels, "remove", removeLabels) // encgrep:allow logs Gmail reserved label names (UNREAD/STARRED), not message content
	return nil
}

// FetchFlags returns current flag state per message from its labels. Gmail has
// no batch get, but FetchFlags is only called for the delta-scoped candidate set
// (FlagChangesInDelta), so per-message minimal gets are cheap. A message that
// cannot be resolved is simply absent from the map.
func (b *Backend) FetchFlags(ctx context.Context, _ string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	out := make(map[string]backend.Flags, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var gm struct {
			LabelIDs []string `json:"labelIds"`
		}
		if err := b.doJSON(ctx, http.MethodGet,
			b.baseURL+"/users/me/messages/"+url.PathEscape(ref.ID)+"?format=minimal", nil, &gm); err != nil {
			if isNotFound(err) {
				continue // message gone; leaving it out of the map is correct
			}
			// A systemic error (auth/quota/5xx) must fail the pass, not silently
			// report an incomplete flag set as success.
			return nil, fmt.Errorf("fetch labels for %s: %w", ref.ID, err)
		}
		out[ref.ID] = flagsFromLabels(gm.LabelIDs)
	}
	return out, nil
}

// MARK: - Not yet implemented (move/append/send/watch land with engine routing)

// Move is unused for Gmail: it is folderless, so the engine drives archive and
// delete through the label-upload path (ApplyLabels), not folder relocation.
func (b *Backend) Move(_ context.Context, _ backend.RemoteRef, _ string) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, errNotImplemented
}

// ensureLabels loads the label map on demand (an UploadOnly sync never calls
// FetchMessages, which is what normally populates it).
func (b *Backend) ensureLabels(ctx context.Context) error {
	b.mu.Lock()
	loaded := b.labels != nil
	b.mu.Unlock()
	if loaded {
		return nil
	}
	return b.loadLabels(ctx)
}

// LabelTags returns the tags that map to real Gmail labels (system + user), so
// the engine can distinguish an uploadable label change from a Durian-local
// tag. Implements backend.LabelWriter.
func (b *Backend) LabelTags(ctx context.Context) ([]string, error) {
	if err := b.ensureLabels(ctx); err != nil {
		return nil, fmt.Errorf("load labels: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tags := make([]string, 0, len(b.tagToID))
	for tag := range b.tagToID {
		tags = append(tags, tag)
	}
	return tags, nil
}

// ApplyLabels applies local tag changes to a Gmail message as one label
// modification: add/remove tags are resolved to Gmail label ids (system labels
// via the fixed table, user labels by display name) and tags with no label —
// Durian-local tags the caller may include — are skipped. Removing "inbox"
// archives; the engine adds a "trash" tag to delete. Implements
// backend.LabelWriter.
func (b *Backend) ApplyLabels(ctx context.Context, ref backend.RemoteRef, add, remove []string) error {
	if err := b.ensureLabels(ctx); err != nil {
		return fmt.Errorf("load labels: %w", err)
	}
	b.mu.Lock()
	rev := b.tagToID
	b.mu.Unlock()

	addIDs := resolveTagIDs(rev, add)
	removeIDs := resolveTagIDs(rev, remove)
	if len(addIDs) == 0 && len(removeIDs) == 0 {
		return nil // nothing Gmail-actionable (e.g. only Durian-local tags changed)
	}

	body := make(map[string][]string, 2)
	if len(addIDs) > 0 {
		body["addLabelIds"] = addIDs
	}
	if len(removeIDs) > 0 {
		body["removeLabelIds"] = removeIDs
	}
	if err := b.doJSONWithRetry(ctx, http.MethodPost,
		b.baseURL+"/users/me/messages/"+url.PathEscape(ref.ID)+"/modify", body, nil, true); err != nil {
		return fmt.Errorf("modify labels for %s: %w", ref.ID, err)
	}
	slog.Debug("Applied labels", "module", "GMAILBACKEND", "id", ref.ID, "add", addIDs, "remove", removeIDs) // encgrep:allow logs Gmail label ids, not message content
	return nil
}

// resolveTagIDs maps tags to their Gmail label ids via rev, dropping unmapped
// tags and de-duplicating ids (two tags could resolve to one label).
func resolveTagIDs(rev map[string]string, tags []string) []string {
	var ids []string
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		id, ok := rev[t]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func (b *Backend) Append(_ context.Context, _ string, _ backend.Flags, _ []byte) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, errNotImplemented
}

func (b *Backend) Send(_ context.Context, _ []byte) error { return errNotImplemented }

func (b *Backend) Watch(_ context.Context, _ string, _ func()) error { return errNotImplemented }
