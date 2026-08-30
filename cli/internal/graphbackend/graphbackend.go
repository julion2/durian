// Package graphbackend implements the provider-agnostic backend.Backend
// interface on the Microsoft Graph REST API. It is the second Backend
// implementation next to imapbackend: the sync engine, store, ingest and flag
// logic stay unchanged and provider-neutral.
//
// Cursor encoding: the per-folder backend.Cursor is the opaque Graph delta URL
// — the @odata.nextLink while a delta round is still paging, or the
// @odata.deltaLink once the round is complete. An empty cursor starts a fresh
// delta round (full resync of the folder).
//
// Phase 2c part 1 implements the read/sync core (folders, delta messages, raw
// MIME bodies); part 2 adds the sync-critical writes (flags, native move).
// Append, send and a real watcher return errNotImplemented until they land.
package graphbackend

import (
	"bytes"
	"context"
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
	// defaultBaseURL is the Graph API root, without a trailing slash.
	defaultBaseURL = "https://graph.microsoft.com/v1.0"

	// deltaSelect is the field set requested on delta queries: just enough
	// metadata to build flags/labels/dates — the body always comes from a
	// separate raw MIME ($value) fetch.
	deltaSelect = "id,internetMessageId,isRead,flag,categories,receivedDateTime"

	// tokenExpiryBuffer refreshes the cached Graph token this long before its
	// actual expiry, so a request never starts with an about-to-expire token.
	tokenExpiryBuffer = 5 * time.Minute
	maxJSONBytes      = 16 << 20
)

// errNotImplemented marks the methods that have not landed yet (append, send,
// real watcher).
var errNotImplemented = errors.New("graphbackend: not implemented yet")

// wellKnownRoles pairs Graph well-known folder names with their backend.Role
// in a fixed order, so role resolution is deterministic.
var wellKnownRoles = []struct {
	wellKnown string
	role      backend.Role
}{
	{"inbox", backend.RoleInbox},
	{"sentitems", backend.RoleSent},
	{"drafts", backend.RoleDrafts},
	{"deleteditems", backend.RoleTrash},
	{"junkemail", backend.RoleJunk},
	{"archive", backend.RoleArchive},
}

// Backend implements backend.Backend on the Microsoft Graph REST API.
type Backend struct {
	account      *config.AccountConfig
	clientID     string
	clientSecret string
	tenant       string
	flagRetries  flagRetryPolicy

	httpClient *http.Client
	// baseURL is the Graph API root without trailing slash. Defaults to
	// defaultBaseURL; tests point it at an httptest.Server.
	baseURL string
	// mailbox is the URL segment identifying which mailbox to operate on:
	// "/me" for the token owner's own mailbox, or "/users/{address}" for a
	// shared/delegated mailbox accessed with the delegating user's token.
	mailbox string
	// tokenFn returns a valid Graph bearer token. Defaults to the cached
	// oauth.GetGraphToken path (cachedGraphToken); tests override it.
	tokenFn func(ctx context.Context) (string, error)

	// mu guards cachedToken (backends may be used from watch + sync goroutines).
	mu          sync.Mutex
	cachedToken *oauth.Token
}

// Compile-time check that Backend satisfies the interface.
var _ backend.Backend = (*Backend)(nil)

// New creates a Graph backend for the given Microsoft OAuth account. The
// Graph token is fetched lazily on first use, so construction never touches
// the network.
func New(account *config.AccountConfig) (*Backend, error) {
	if account.OAuth == nil || account.OAuth.Provider != "microsoft" {
		return nil, fmt.Errorf("graph backend requires a Microsoft OAuth account, got %s", account.Email)
	}

	// A delegated/shared mailbox is reached at /users/{address} with the
	// delegating user's token; the token owner's own mailbox uses /me.
	mailbox := "/me"
	if account.IsDelegatedMailbox() {
		mailbox = "/users/" + account.Email
	}

	b := &Backend{
		account:      account,
		clientID:     account.OAuth.ClientID,
		clientSecret: account.OAuth.ClientSecret,
		tenant:       account.OAuth.Tenant,
		baseURL:      defaultBaseURL,
		mailbox:      mailbox,
		flagRetries:  defaultFlagRetryPolicy(),
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
	b.tokenFn = b.cachedGraphToken
	return b, nil
}

// MARK: - Token source

// cachedGraphToken returns the cached Graph access token, minting a fresh one
// via oauth.GetGraphToken when none is cached or it expires within
// tokenExpiryBuffer. The Graph token lives only in memory; oauth.GetGraphToken
// persists any rotated refresh token itself.
func (b *Backend) cachedGraphToken(_ context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cachedToken != nil && !b.cachedToken.IsExpiredWithBuffer(tokenExpiryBuffer) {
		return b.cachedToken.AccessToken, nil
	}

	token, err := oauth.GetGraphToken(b.account.GetAuthEmail(), b.clientID, b.clientSecret, b.tenant)
	if err != nil {
		return "", fmt.Errorf("failed to get Graph token for %s: %w", b.account.Email, err)
	}
	b.cachedToken = token
	slog.Debug("Minted Graph access token", "module", "GRAPHBACKEND",
		"account", b.account.Email, "expiry", token.Expiry)
	return token.AccessToken, nil
}

// MARK: - HTTP client

// statusError is a non-2xx Graph response, carrying the HTTP status so callers
// can react to specific codes (e.g. tolerate 404 on well-known folder lookups).
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("graph request failed: status %d: %s", e.status, e.body)
}

func (e *statusError) SafeLogText() string {
	return fmt.Sprintf("graph request failed: status %d: response body %s", e.status, redact.Placeholder)
}

var _ redact.SafeLogError = (*statusError)(nil)

// do executes one authenticated Graph request with throttle handling. GET/HEAD
// requests retry on 429 and once on any 5xx; mutation requests are never
// replayed because the first response may have been lost after the server
// committed it. All waits respect ctx cancellation.
func (b *Backend) do(ctx context.Context, method, reqURL string, body []byte) (*http.Response, error) {
	safeToRetry := method == http.MethodGet || method == http.MethodHead
	return b.doWithRetry(ctx, method, reqURL, body, safeToRetry)
}

func (b *Backend) doWithRetry(ctx context.Context, method, reqURL string, body []byte, safeToRetry bool) (*http.Response, error) {
	const (
		maxThrottleRetries = 3
		transientBackoff   = 2 * time.Second
	)

	if err := validateAuthenticatedURL(b.baseURL, reqURL); err != nil {
		return nil, fmt.Errorf("refusing graph request URL: %w", err)
	}

	throttleRetries := 0
	transientRetried := false
	for {
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
			return nil, fmt.Errorf("failed to build graph request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := b.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("graph request failed: %w", err)
		}

		switch {
		case safeToRetry && resp.StatusCode == http.StatusTooManyRequests && throttleRetries < maxThrottleRetries:
			throttleRetries++
			delay := retryAfter(resp)
			drainClose(resp)
			slog.Warn("Graph throttled request, backing off", "module", "GRAPHBACKEND",
				"retry", throttleRetries, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		case safeToRetry && resp.StatusCode >= http.StatusInternalServerError && !transientRetried:
			transientRetried = true
			drainClose(resp)
			slog.Warn("Graph transient error, retrying once", "module", "GRAPHBACKEND",
				"status", resp.StatusCode)
			if err := sleepCtx(ctx, transientBackoff); err != nil {
				return nil, err
			}
		default:
			return resp, nil
		}
	}
}

// doJSON executes a Graph request with an optional JSON body and decodes the
// JSON response into out (out may be nil to discard the response).
func (b *Backend) doJSON(ctx context.Context, method, reqURL string, body any, out any) error {
	return b.doJSONWithRetry(ctx, method, reqURL, body, out, method == http.MethodGet || method == http.MethodHead)
}

func (b *Backend) doJSONWithRetry(ctx context.Context, method, reqURL string, body any, out any, safeToRetry bool) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal graph request body: %w", err)
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
		return fmt.Errorf("failed to decode graph response: %w", err)
	}
	return nil
}

func validateAuthenticatedURL(baseURL, requestURL string) error {
	base, err := url.Parse(baseURL)
	if err != nil || !base.IsAbs() || base.User != nil {
		return errors.New("graph base URL is invalid")
	}
	target, err := url.Parse(requestURL)
	if err != nil || !target.IsAbs() || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return errors.New("authenticated URL must be an absolute HTTP(S) URL without userinfo")
	}
	if !strings.EqualFold(base.Scheme, target.Scheme) ||
		!strings.EqualFold(base.Hostname(), target.Hostname()) || effectivePort(base) != effectivePort(target) {
		return errors.New("authenticated URL origin differs from Graph API origin")
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
		return fmt.Errorf("graph JSON response exceeds %d bytes", limit)
	}
	return json.Unmarshal(data, out)
}

// doRaw executes a GET and streams the raw response body (e.g. RFC822 MIME
// from a $value endpoint) to w.
func (b *Backend) doRaw(ctx context.Context, reqURL string, w io.Writer) error {
	resp, err := b.do(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	defer drainClose(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newStatusError(resp)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("failed to stream graph response: %w", err)
	}
	return nil
}

// retryAfter parses the Retry-After header (delay-seconds form) of a 429
// response, defaulting to one second when absent or unparseable.
func retryAfter(resp *http.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Second
}

// newStatusError builds a statusError including a snippet of the error body.
func newStatusError(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &statusError{status: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
}

// isSyncStateExpired reports whether err is a Graph 410 SyncStateNotFound — the
// signal that a delta token has aged out and the delta must be restarted from
// scratch rather than resumed.
func isSyncStateExpired(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	return se.status == http.StatusGone && strings.Contains(se.body, "SyncStateNotFound")
}

// drainClose drains (bounded) and closes a response body so the underlying
// connection can be reused.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// sleepCtx sleeps for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MARK: - Folders

// graphFolder is the subset of a Graph mailFolder resource we consume.
type graphFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// folderPage is one page of GET /me/mailFolders.
type folderPage struct {
	Value    []graphFolder `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

// FetchFolders returns all mail folders with their special-use roles. Roles
// are resolved by looking up each Graph well-known folder's real id first and
// matching ids against the full folder list; every Graph folder can hold
// messages, so Selectable is always true. Folder.Name is the Graph folder id,
// Display the human-readable name.
func (b *Backend) FetchFolders(ctx context.Context) ([]backend.Folder, error) {
	roleByID, err := b.wellKnownRoleIDs(ctx)
	if err != nil {
		return nil, err
	}

	var folders []backend.Folder
	pageURL := b.baseURL + b.mailbox + "/mailFolders?$top=100&$select=id,displayName"
	for pageURL != "" {
		var page folderPage
		if err := b.doJSON(ctx, http.MethodGet, pageURL, nil, &page); err != nil {
			return nil, fmt.Errorf("failed to list mail folders: %w", err)
		}
		for _, f := range page.Value {
			folders = append(folders, backend.Folder{
				Name:       f.ID,
				Display:    f.DisplayName,
				Role:       roleByID[f.ID],
				Selectable: true,
			})
		}
		pageURL = page.NextLink
	}

	slog.Debug("Fetched folders", "module", "GRAPHBACKEND", "count", len(folders)) // encgrep:allow logs folder count only, not message content
	return folders, nil
}

// wellKnownRoleIDs resolves each Graph well-known folder name to its real
// folder id so roles can be assigned by id match. Folders absent from the
// mailbox (404) are skipped; a folder keeps the first role that claims it.
func (b *Backend) wellKnownRoleIDs(ctx context.Context) (map[string]backend.Role, error) {
	roleByID := make(map[string]backend.Role, len(wellKnownRoles))
	for _, m := range wellKnownRoles {
		var folder graphFolder
		reqURL := fmt.Sprintf("%s%s/mailFolders/%s?$select=id", b.baseURL, b.mailbox, m.wellKnown)
		if err := b.doJSON(ctx, http.MethodGet, reqURL, nil, &folder); err != nil {
			var se *statusError
			if errors.As(err, &se) && se.status == http.StatusNotFound {
				continue // Well-known folder not present in this mailbox
			}
			return nil, fmt.Errorf("failed to resolve well-known folder %s: %w", m.wellKnown, err)
		}
		if _, taken := roleByID[folder.ID]; !taken {
			roleByID[folder.ID] = m.role
		}
	}
	return roleByID, nil
}

// MARK: - Messages

// graphFlag is the followupFlag object on a Graph message.
type graphFlag struct {
	FlagStatus string `json:"flagStatus"` // notFlagged | flagged | complete
}

// deltaItem is one entry in a delta page: either a new/changed message or,
// when Removed is present, a deletion carrying only the id.
type deltaItem struct {
	ID                string          `json:"id"`
	InternetMessageID string          `json:"internetMessageId"`
	IsRead            bool            `json:"isRead"`
	Flag              *graphFlag      `json:"flag"`
	Categories        []string        `json:"categories"`
	ReceivedDateTime  string          `json:"receivedDateTime"`
	Removed           json.RawMessage `json:"@removed"`
}

// deltaPage is one page of a messages delta query.
type deltaPage struct {
	Value     []deltaItem `json:"value"`
	NextLink  string      `json:"@odata.nextLink"`
	DeltaLink string      `json:"@odata.deltaLink"`
}

type graphCursor struct {
	URL             string `json:"url"`
	FullReplacement bool   `json:"fullReplacement,omitempty"`
}

func decodeGraphCursor(cursor backend.Cursor) graphCursor {
	if len(cursor) == 0 {
		return graphCursor{}
	}
	var state graphCursor
	if cursor[0] == '{' && json.Unmarshal(cursor, &state) == nil && state.URL != "" {
		return state
	}
	return graphCursor{URL: string(cursor)}
}

func encodeGraphCursor(state graphCursor) backend.Cursor {
	if !state.FullReplacement {
		return backend.Cursor(state.URL)
	}
	encoded, _ := json.Marshal(state)
	return encoded
}

// FetchMessages returns the changes in folder since cursor via the Graph delta
// query. One call consumes exactly one delta page — Graph controls the page
// size, so limit is only a soft hint and is not enforced here. Each changed
// message triggers a separate raw MIME ($value) fetch, because the delta JSON
// has no RFC822 body. A transient body failure aborts the page so its cursor is
// not advanced; an explicit 404 is treated as a concurrent deletion and a 403
// as permanently inaccessible content. HasMore reports a pending
// @odata.nextLink; the returned cursor is that nextLink, or the @odata.deltaLink
// once the delta round is complete.
func (b *Backend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	_ = limit // Soft hint only: Graph fixes the delta page size server-side.
	var result backend.FetchResult

	freshURL := fmt.Sprintf("%s%s/mailFolders/%s/messages/delta?$select=%s",
		b.baseURL, b.mailbox, url.PathEscape(folder), deltaSelect)
	state := decodeGraphCursor(cursor)
	pageURL := state.URL
	if pageURL == "" {
		pageURL = freshURL
	}

	var page deltaPage
	if err := b.doJSON(ctx, http.MethodGet, pageURL, nil, &page); err != nil {
		// An expired delta token returns 410 SyncStateNotFound; Graph's contract
		// is to discard it and restart the delta from scratch. Retry once from a
		// fresh delta URL so the folder resyncs (upsert dedups, so it is safe)
		// instead of the whole account sync aborting.
		if pageURL != freshURL && isSyncStateExpired(err) {
			slog.Info("Graph delta token expired, restarting full delta for folder", "module", "GRAPHBACKEND", "folder", folder) // encgrep:allow folder name is operational sync metadata, not message content
			state.FullReplacement = true
			page = deltaPage{}
			if err := b.doJSON(ctx, http.MethodGet, freshURL, nil, &page); err != nil {
				return result, fmt.Errorf("failed to restart delta for %s: %w", folder, err)
			}
		} else {
			return result, fmt.Errorf("failed to fetch delta page for %s: %w", folder, err)
		}
	}

	// Split deletions (handled inline) from content items, whose raw MIME we
	// fetch concurrently below — one serial $value roundtrip per message was the
	// dominant cost of a sync.
	var content []deltaItem
	for _, item := range page.Value {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if len(item.Removed) > 0 {
			// A removed delta entry usually carries only the id; MessageID may
			// therefore be empty and the engine falls back to same-run tracking.
			result.Deleted = append(result.Deleted, backend.Deletion{
				Ref:       backend.RemoteRef{Folder: folder, ID: item.ID},
				MessageID: trimAngles(item.InternetMessageID),
			})
			continue
		}
		content = append(content, item)
	}
	messages, missing, unavailable, err := b.fetchBodies(ctx, folder, content)
	if err != nil {
		return backend.FetchResult{}, err
	}
	result.Messages = messages
	for _, id := range missing {
		result.Deleted = append(result.Deleted, backend.Deletion{Ref: backend.RemoteRef{Folder: folder, ID: id}})
	}
	if state.FullReplacement {
		result.FullSnapshot = true
		missingSet := make(map[string]struct{}, len(missing))
		for _, id := range missing {
			missingSet[id] = struct{}{}
		}
		result.Present = make([]backend.RemoteRef, 0, len(content)-len(missingSet))
		for _, item := range content {
			if _, gone := missingSet[item.ID]; !gone {
				result.Present = append(result.Present, backend.RemoteRef{Folder: folder, ID: item.ID})
			}
		}
		for _, id := range unavailable {
			result.Unavailable = append(result.Unavailable, backend.RemoteRef{Folder: folder, ID: id})
		}
	}

	if page.NextLink != "" {
		result.Cursor = encodeGraphCursor(graphCursor{URL: page.NextLink, FullReplacement: state.FullReplacement})
		result.HasMore = true
	} else {
		result.Cursor = backend.Cursor(page.DeltaLink)
	}

	slog.Debug("Fetched delta page", "module", "GRAPHBACKEND", "folder", folder, // encgrep:allow folder name is operational sync metadata, not message content
		"new", len(result.Messages), "deleted", len(result.Deleted), "has_more", result.HasMore)
	return result, nil
}

// fetchConcurrency bounds the number of simultaneous $value downloads per delta
// page: enough to hide per-request latency, low enough to stay under Graph's
// per-app throttle ceiling (do() still backs off on a 429).
const fetchConcurrency = 6

// fetchBodies downloads raw MIME concurrently. Explicit 404s are reported as
// missing refs. A permanent per-item 403 is unavailable; other failures hold
// the page cursor so transient outages cannot silently lose mail.
func (b *Backend) fetchBodies(ctx context.Context, folder string, items []deltaItem) ([]backend.Message, []string, []string, error) {
	msgs := make([]backend.Message, len(items)) // indexed by item; failures stay zero
	errs := make([]error, len(items))
	missing := make([]bool, len(items))
	unavailable := make([]bool, len(items))

	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	for i := range items {
		// Acquire a slot, or bail out early if the sync was cancelled.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, nil, nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			msg, err := b.fetchOne(ctx, folder, items[i])
			if err != nil {
				var statusErr *statusError
				if errors.As(err, &statusErr) {
					switch statusErr.status {
					case http.StatusNotFound:
						missing[i] = true
					case http.StatusForbidden:
						// Some protected Graph items expose metadata but deny
						// $value permanently. Keep an existing local copy during a
						// snapshot and let the delta advance past this one item.
						slog.Warn("Graph message body is not accessible, skipping item", "module", "GRAPHBACKEND", // encgrep:allow folder/id are remote operational metadata, not message content
							"folder", folder, "id", items[i].ID, "status", statusErr.status)
						unavailable[i] = true
					default:
						errs[i] = err
					}
				} else {
					errs[i] = err
				}
				return
			}
			msgs[i] = msg // own slot: no shared write, no mutex needed
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to fetch raw MIME: %w", err)
		}
	}
	var missingIDs []string
	var unavailableIDs []string
	for i, gone := range missing {
		if gone {
			missingIDs = append(missingIDs, items[i].ID)
		} else if unavailable[i] {
			unavailableIDs = append(unavailableIDs, items[i].ID)
		}
	}
	return filterFetched(msgs), missingIDs, unavailableIDs, nil
}

// filterFetched drops skipped (zero) entries in place, preserving order.
func filterFetched(msgs []backend.Message) []backend.Message {
	out := msgs[:0]
	for _, m := range msgs {
		if m.Ref.ID != "" {
			out = append(out, m)
		}
	}
	return out
}

// fetchOne downloads the raw MIME for a single delta item and builds its Message.
func (b *Backend) fetchOne(ctx context.Context, folder string, item deltaItem) (backend.Message, error) {
	var raw bytes.Buffer
	rawURL := b.baseURL + b.mailbox + "/messages/" + url.PathEscape(item.ID) + "/$value"
	if err := b.doRaw(ctx, rawURL, &raw); err != nil {
		return backend.Message{}, err
	}
	return backend.Message{
		MessageID:    trimAngles(item.InternetMessageID),
		Ref:          backend.RemoteRef{Folder: folder, ID: item.ID},
		Raw:          raw.Bytes(),
		Flags:        flagsFromGraph(item.IsRead, item.Flag),
		Labels:       item.Categories,
		InternalDate: parseGraphTime(item.ReceivedDateTime),
	}, nil
}

// FetchBody streams the full RFC822 message for ref to w via the $value
// endpoint. Graph message ids are mailbox-global, so ref.Folder is not needed
// for the lookup.
func (b *Backend) FetchBody(ctx context.Context, ref backend.RemoteRef, w io.Writer) error {
	reqURL := b.baseURL + b.mailbox + "/messages/" + url.PathEscape(ref.ID) + "/$value"
	if err := b.doRaw(ctx, reqURL, w); err != nil {
		return fmt.Errorf("failed to fetch body for %s in %s: %w", ref.ID, ref.Folder, err)
	}
	return nil
}

// MARK: - Flags

// batchLimit is Graph's maximum number of sub-requests per $batch call.
const (
	batchLimit                = 20
	maxFlagSubresponseRetries = 3
	flagRetryBudget           = 7 * time.Second
)

type flagRetryPolicy struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func defaultFlagRetryPolicy() flagRetryPolicy {
	return flagRetryPolicy{now: time.Now, sleep: sleepCtx}
}

// batchRequest is one sub-request of a Graph $batch call. URL is a
// Graph-relative path (starting with the mailbox segment, e.g. /me/...), not a
// full URL.
type batchRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	URL    string `json:"url"`
}

// batchResponseItem is one sub-response of a Graph $batch call, carrying the
// sub-request's id and its own HTTP status.
type batchResponseItem struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Body    json.RawMessage   `json:"body"`
	Headers map[string]string `json:"headers"`
}

// flagFields is the flag subset of a Graph message resource.
type flagFields struct {
	IsRead bool       `json:"isRead"`
	Flag   *graphFlag `json:"flag"`
}

// FetchFlags reads the current server flag state for refs via Graph JSON
// batching: refs are chunked into $batch calls of at most batchLimit GET
// sub-requests each. Messages Graph can no longer resolve (404, e.g. moved or
// deleted since the last delta) are simply absent from the returned map, per
// the Backend contract. Graph message ids are mailbox-global, so folder is
// only used for error context.
func (b *Backend) FetchFlags(ctx context.Context, folder string, refs []backend.RemoteRef) (map[string]backend.Flags, error) {
	flags := make(map[string]backend.Flags, len(refs))
	requests := make([]batchRequest, len(refs))
	refIDByRequestID := make(map[string]string, len(refs))
	for i, ref := range refs {
		requestID := strconv.Itoa(i)
		requests[i] = batchRequest{
			ID:     requestID,
			Method: http.MethodGet,
			URL:    b.mailbox + "/messages/" + url.PathEscape(ref.ID) + "?$select=id,isRead,flag",
		}
		refIDByRequestID[requestID] = ref.ID
	}

	policy := b.flagRetries
	pending := requests
	forbidden := 0
	slept := time.Duration(0)
	for attempt := 0; ; attempt++ {
		transient := make([]batchRequest, 0)
		var retryAfterDelay time.Duration
		outerThrottled := false
		for start := 0; start < len(pending); start += batchLimit {
			chunk := pending[start:min(start+batchLimit, len(pending))]
			chunkTransient, chunkForbidden, chunkRetryAfter, chunkOuterThrottled, err := b.fetchFlagBatch(ctx, folder, chunk, refIDByRequestID, flags, policy.now)
			if err != nil {
				return nil, err
			}
			transient = append(transient, chunkTransient...)
			forbidden += chunkForbidden
			outerThrottled = outerThrottled || chunkOuterThrottled
			if chunkRetryAfter > retryAfterDelay {
				retryAfterDelay = chunkRetryAfter
			}
		}

		if len(transient) == 0 {
			var partialErr error
			if forbidden > 0 {
				partialErr = fmt.Errorf("%w: Graph denied flag access for %d message(s) in %s",
					backend.ErrPartialFlags, forbidden, folder)
			}
			slog.Debug("Fetched flags", "module", "GRAPHBACKEND", "folder", folder, // encgrep:allow logs folder name and flag counts, not flag values or message content
				"requested", len(refs), "resolved", len(flags))
			return flags, partialErr
		}
		if attempt >= maxFlagSubresponseRetries || slept >= flagRetryBudget {
			if outerThrottled {
				// An outer $batch throttle provides no per-message result. Do not
				// expose flags from other chunks as a usable partial response: the
				// sync engine would otherwise advance past every message in this
				// chunk. A systemic error keeps the folder cursor for a later pass.
				return nil, fmt.Errorf("Graph flag batch remains throttled after %d retries in %s", attempt, folder)
			}
			return flags, fmt.Errorf("%w: Graph flags remain unavailable after %d retries in %s", backend.ErrPartialFlags, attempt, folder)
		}

		delay := time.Second << attempt
		if retryAfterDelay > 0 {
			delay = retryAfterDelay
		}
		if remaining := flagRetryBudget - slept; delay > remaining {
			delay = remaining
		}
		slog.Warn("Graph batch subrequests throttled, backing off", "module", "GRAPHBACKEND",
			"folder", folder, "retry", attempt+1, "delay", delay, "unresolved", len(transient))
		if err := policy.sleep(ctx, delay); err != nil {
			return nil, err
		}
		slept += delay
		pending = transient
	}
}

func (b *Backend) fetchFlagBatch(
	ctx context.Context,
	folder string,
	requests []batchRequest,
	refIDByRequestID map[string]string,
	flags map[string]backend.Flags,
	now func() time.Time,
) ([]batchRequest, int, time.Duration, bool, error) {
	var envelope struct {
		Responses []batchResponseItem `json:"responses"`
	}
	payload, err := json.Marshal(map[string]any{"requests": requests})
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("failed to marshal flag batch in %s: %w", folder, err)
	}
	// Disable the generic HTTP retry loop here. Outer 429/5xx responses join
	// the same global rounds as transient subresponses below, so every sleep
	// across every chunk is charged to the one flagRetryBudget.
	resp, err := b.doWithRetry(ctx, http.MethodPost, b.baseURL+"/$batch", payload, false)
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("failed to batch-fetch flags in %s: %w", folder, err)
	}
	defer drainClose(resp)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		delay, _ := batchRetryAfter(map[string]string{"Retry-After": resp.Header.Get("Retry-After")}, now())
		return requests, 0, delay, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, 0, false, fmt.Errorf("failed to batch-fetch flags in %s: %w", folder, newStatusError(resp))
	}
	if err := decodeJSONLimited(resp.Body, maxJSONBytes, &envelope); err != nil {
		return nil, 0, 0, false, fmt.Errorf("failed to decode flag batch in %s: %w", folder, err)
	}

	seen := make(map[string]struct{}, len(requests))
	transient := make([]batchRequest, 0)
	var retryAfterDelay time.Duration
	forbidden := 0
	for _, item := range envelope.Responses {
		refID, ok := refIDByRequestID[item.ID]
		if !ok {
			return nil, 0, 0, false, fmt.Errorf("batch flag fetch returned unknown request id %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		switch {
		case item.Status >= 200 && item.Status < 300:
			var msg flagFields
			if err := json.Unmarshal(item.Body, &msg); err != nil {
				return nil, 0, 0, false, fmt.Errorf("failed to decode batched flags for %s: %w", refID, err)
			}
			flags[refID] = flagsFromGraph(msg.IsRead, msg.Flag)
		case item.Status == http.StatusNotFound:
			// Message moved or disappeared after the delta; absence is the
			// FetchFlags contract for a dead ref.
		case item.Status == http.StatusForbidden:
			// Unresolved, NOT dead: unlike the 404 above, absence here means
			// "we were not allowed to read it", so the caller must be able to
			// tell the two apart. Recorded and surfaced as ErrPartialFlags so
			// the engine falls back to what the delta carried instead of
			// treating the omission as a deletion.
			forbidden++
			slog.Warn("Graph flags are not accessible for message, skipping item", "module", "GRAPHBACKEND", // encgrep:allow folder/id are remote operational metadata, not message content
				"folder", folder, "id", refID, "status", item.Status)
		case item.Status == http.StatusTooManyRequests || item.Status >= http.StatusInternalServerError:
			transient = append(transient, batchRequest{
				ID: item.ID, Method: http.MethodGet,
				URL: b.mailbox + "/messages/" + url.PathEscape(refID) + "?$select=id,isRead,flag",
			})
			if delay, ok := batchRetryAfter(item.Headers, now()); ok && delay > retryAfterDelay {
				retryAfterDelay = delay
			}
		default:
			return nil, 0, 0, false, fmt.Errorf("batch flag fetch for %s failed with status %d", refID, item.Status)
		}
	}
	if len(seen) != len(requests) {
		return nil, 0, 0, false, fmt.Errorf("batch flag fetch returned %d of %d responses", len(seen), len(requests))
	}
	return transient, forbidden, retryAfterDelay, false, nil
}

func batchRetryAfter(headers map[string]string, now time.Time) (time.Duration, bool) {
	for name, value := range headers {
		if !strings.EqualFold(name, "Retry-After") {
			continue
		}
		value = strings.TrimSpace(value)
		if seconds, err := strconv.Atoi(value); err == nil {
			if seconds <= 0 {
				return 0, false
			}
			if seconds >= int(flagRetryBudget/time.Second) {
				return flagRetryBudget, true
			}
			return time.Duration(seconds) * time.Second, true
		}
		if retryAt, err := http.ParseTime(value); err == nil {
			delay := retryAt.Sub(now)
			if delay <= 0 {
				return 0, false
			}
			return min(delay, flagRetryBudget), true
		}
		return 0, false
	}
	return 0, false
}

// ApplyFlags pushes flag changes to Graph by PATCHing only the message fields
// that actually change: Seen maps to isRead, Flagged/Completed to the
// followupFlag flagStatus (flagged / complete; removing Flagged resets to
// notFlagged), mirroring flagsFromGraph. Graph has no equivalent of IMAP
// \Answered, and \Deleted is expressed as a move to Deleted Items rather than
// a flag, so both are ignored here. When nothing translates to a Graph field,
// no request is made.
func (b *Backend) ApplyFlags(ctx context.Context, ref backend.RemoteRef, add, remove backend.Flags) error {
	body := make(map[string]any, 2)
	switch {
	case add.Seen:
		body["isRead"] = true
	case remove.Seen:
		body["isRead"] = false
	}
	switch {
	case add.Completed:
		body["flag"] = graphFlag{FlagStatus: "complete"}
	case add.Flagged:
		body["flag"] = graphFlag{FlagStatus: "flagged"}
	case remove.Flagged:
		body["flag"] = graphFlag{FlagStatus: "notFlagged"}
	}
	if len(body) == 0 {
		return nil
	}

	reqURL := b.baseURL + b.mailbox + "/messages/" + url.PathEscape(ref.ID)
	if err := b.doJSON(ctx, http.MethodPatch, reqURL, body, nil); err != nil {
		return fmt.Errorf("failed to apply flags to %s in %s: %w", ref.ID, ref.Folder, err)
	}
	slog.Debug("Applied flags", "module", "GRAPHBACKEND", "id", ref.ID, "add", add, "remove", remove) // encgrep:allow logs IMAP/Graph flag names (\Seen etc.), not an ADR-0001 encrypted column
	return nil
}

// MARK: - Move

// Move relocates ref into destFolder via Graph's native atomic move. Graph
// assigns the moved message a new id, so the returned RemoteRef carries the id
// from the move response, not ref.ID.
//
// A 404 means the source id is dead — the message was moved or deleted by
// another client since the last delta, and Graph renumbers on move, so the old
// id will never resolve again. That is reported as backend.ErrRefGone rather
// than a plain failure, so the engine can reconcile locally instead of
// retrying the same doomed move on every sync.
func (b *Backend) Move(ctx context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	var moved struct {
		ID string `json:"id"`
	}
	reqURL := b.baseURL + b.mailbox + "/messages/" + url.PathEscape(ref.ID) + "/move"
	if err := b.doJSON(ctx, http.MethodPost, reqURL,
		map[string]string{"destinationId": destFolder}, &moved); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			return backend.RemoteRef{}, fmt.Errorf("failed to move %s to %s: %w: %w",
				ref.ID, destFolder, backend.ErrRefGone, err)
		}
		return backend.RemoteRef{}, fmt.Errorf("failed to move %s to %s: %w", ref.ID, destFolder, err)
	}
	if moved.ID == "" {
		return backend.RemoteRef{}, fmt.Errorf("graph move of %s to %s returned no message id", ref.ID, destFolder)
	}

	slog.Debug("Moved message", "module", "GRAPHBACKEND",
		"from", ref.Folder, "to", destFolder, "new_id", moved.ID)
	return backend.RemoteRef{Folder: destFolder, ID: moved.ID}, nil
}

// MARK: - Append / Send (not implemented)

// Append is not implemented yet; draft upload arrives in part 2.
func (b *Backend) Append(_ context.Context, _ string, _ backend.Flags, _ []byte) (backend.RemoteRef, error) {
	return backend.RemoteRef{}, errNotImplemented
}

// Send is not implemented: outbound mail stays on the SMTP path during
// coexistence with the IMAP backend.
func (b *Backend) Send(_ context.Context, _ []byte) error {
	return errNotImplemented
}

// MARK: - Watch / Capabilities / Close

// Watch never signals: Microsoft Graph has no push transport a local desktop
// client can receive. Change notifications are delivered only to a webhook
// (which needs a public HTTPS endpoint), Azure Event Hubs or Event Grid, and
// the endpoint-less socket transport Graph does offer is scoped to
// driveItem/list resources, not mail.
//
// So this blocks until ctx is done and reports PushWatch: false, which is the
// honest answer rather than a placeholder. Callers that see PushWatch: false
// are expected to drive the account on a cadence instead (handler.EngineWatcher
// does exactly that, running the delta-based sync engine every 30s on the
// inbox). Revisit if Graph ever extends socket subscriptions to messages.
func (b *Backend) Watch(ctx context.Context, _ string, _ func()) error {
	<-ctx.Done()
	return ctx.Err()
}

// Capabilities reports Graph delta behavior and the lack of a local push
// notification transport (see Watch).
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		PushWatch:          false,
		FlagChangesInDelta: true,
		// Graph has no answered flag on the message resource. ApplyFlags
		// translates only isRead and flagStatus, and flagsFromGraph can never
		// report Answered, so a local "replied" tag would be uploaded into
		// nothing, recorded in the baseline, and then removed on the next sync
		// when the server reports the message as un-answered. Declaring this
		// keeps Answered out of the three-way merge, which is what the code
		// here has always actually supported.
		AnsweredUnsupported: true,
	}
}

// Close releases the backend's resources. The Graph backend is stateless HTTP,
// so there is nothing to tear down.
func (b *Backend) Close() error {
	return nil
}

// MARK: - Helpers

// flagsFromGraph maps Graph message state to the neutral backend.Flags. The
// Graph message resource has no equivalent of IMAP \Answered, and deletions
// arrive as @removed delta entries rather than a \Deleted flag, so Answered
// and Deleted stay false.
func flagsFromGraph(isRead bool, flag *graphFlag) backend.Flags {
	f := backend.Flags{Seen: isRead}
	if flag != nil {
		f.Flagged = flag.FlagStatus == "flagged"
		f.Completed = flag.FlagStatus == "complete"
	}
	return f
}

// trimAngles strips the surrounding angle brackets from an RFC822 Message-ID.
func trimAngles(messageID string) string {
	return strings.Trim(messageID, "<>")
}

// parseGraphTime parses a Graph receivedDateTime (RFC3339). An unparseable
// value yields the zero time rather than failing the message.
func parseGraphTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		slog.Warn("Failed to parse receivedDateTime", "module", "GRAPHBACKEND", "value", s, "err", err)
		return time.Time{}
	}
	return t
}
