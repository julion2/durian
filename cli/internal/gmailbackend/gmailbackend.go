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
// paging it carries the messages.list pageToken; once paging completes it
// carries the mailbox historyId, the resume point for incremental history sync.
// An empty cursor starts a fresh full sync (safe — the store upserts by
// Message-ID). Incremental history sync is not wired here yet; a historyId
// cursor currently restarts a full sync.
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
	"sync"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/oauth"
)

const (
	defaultBaseURL = "https://gmail.googleapis.com/gmail/v1"
	// tokenExpiryBuffer refreshes the access token slightly before it expires.
	tokenExpiryBuffer = 2 * time.Minute
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
}

// Compile-time check that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)

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
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		baseURL:      defaultBaseURL,
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

// do executes one authenticated Gmail request with throttle handling: it retries
// 429 and quota 403 (rateLimitExceeded / userRateLimitExceeded) honoring the
// Retry-After header, and retries 503 once, all with exponential backoff and
// respecting ctx. A non-retryable response (including a permission 403) is
// returned with its body intact so the caller can read the error.
func (b *Backend) do(ctx context.Context, method, reqURL string, body []byte) (*http.Response, error) {
	const maxRetries = 3

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
		case resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries:
			delay := retryAfter(resp, backoff)
			drainClose(resp)
			slog.Warn("Gmail throttled (429), backing off", "module", "GMAILBACKEND", "retry", attempt+1, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
			backoff *= 2
		case resp.StatusCode == http.StatusForbidden && attempt < maxRetries:
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
		case resp.StatusCode == http.StatusServiceUnavailable && !transientRetried:
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
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
	}
	resp, err := b.do(ctx, method, reqURL, payload)
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
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
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

// gmailCursor is the opaque per-stream cursor: a pageToken mid initial sync, or
// a historyId once the initial sync completed.
type gmailCursor struct {
	PageToken string `json:"p,omitempty"`
	HistoryID string `json:"h,omitempty"`
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

// FetchMessages lists messages and fetches each one's raw MIME. It implements
// the initial (paged) full sync; incremental history sync lands separately. A
// historyId cursor therefore restarts a full sync for now, which is safe: the
// store upserts by (Message-ID, account).
func (b *Backend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	var result backend.FetchResult
	gc := decodeCursor(cursor)
	if gc.HistoryID != "" {
		slog.Debug("Gmail historyId cursor: restarting full sync (incremental not yet wired)", "module", "GMAILBACKEND")
		gc = gmailCursor{}
	}

	q := url.Values{}
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

	var maxHistoryID uint64
	for _, m := range page.Messages {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		msg, err := b.fetchOne(ctx, folder, m.ID)
		if err != nil {
			slog.Warn("Failed to fetch message, skipping", "module", "GMAILBACKEND", "id", m.ID, "err", err)
			continue
		}
		result.Messages = append(result.Messages, msg.message)
		if msg.historyID > maxHistoryID {
			maxHistoryID = msg.historyID
		}
	}

	if page.NextPageToken != "" {
		result.Cursor = encodeCursor(gmailCursor{PageToken: page.NextPageToken})
		result.HasMore = true
	} else {
		// Initial sync complete: the highest historyId seen is the resume point
		// for the incremental history sync.
		hid := ""
		if maxHistoryID > 0 {
			hid = strconv.FormatUint(maxHistoryID, 10)
		}
		result.Cursor = encodeCursor(gmailCursor{HistoryID: hid})
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
			Labels:       tagLabels(gm.LabelIDs),
			InternalDate: parseInternalDate(gm.InternalDate),
		},
		historyID: hid,
	}, nil
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

// tagLabels returns the labels that should become tags — every label except the
// reserved flag labels, which flagsFromLabels already carries. Resolving Gmail's
// opaque user-label ids to their display names is left to the engine mapping.
func tagLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == labelUnread || l == labelStarred {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil
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
		// Gmail auto-saves a Sent copy, so Durian must not append its own.
		ServerSideSent: true,
	}
}

func (b *Backend) Close() error { return nil }

// MARK: - Not yet implemented (write path lands with engine routing)

func (b *Backend) ApplyFlags(_ context.Context, _ backend.RemoteRef, _, _ backend.Flags) error {
	return errNotImplemented
}

func (b *Backend) FetchFlags(_ context.Context, _ string, _ []backend.RemoteRef) (map[string]backend.Flags, error) {
	return nil, errNotImplemented
}

func (b *Backend) Move(_ context.Context, _ backend.RemoteRef, _ string) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, errNotImplemented
}

func (b *Backend) Append(_ context.Context, _ string, _ backend.Flags, _ []byte) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, errNotImplemented
}

func (b *Backend) Send(_ context.Context, _ []byte) error { return errNotImplemented }

func (b *Backend) Watch(_ context.Context, _ string, _ func()) error { return errNotImplemented }
