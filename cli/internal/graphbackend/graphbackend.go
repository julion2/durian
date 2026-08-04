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
		httpClient:   &http.Client{Timeout: 60 * time.Second},
		baseURL:      defaultBaseURL,
		mailbox:      mailbox,
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

// do executes one authenticated Graph request with throttle handling: up to 3
// retries on 429 honoring the Retry-After header, and one retry with a short
// backoff on 503/504. All waits respect ctx cancellation. The caller owns the
// returned response body (including non-2xx responses).
func (b *Backend) do(ctx context.Context, method, reqURL string, body []byte) (*http.Response, error) {
	const (
		maxThrottleRetries = 3
		transientBackoff   = 2 * time.Second
	)

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
		case resp.StatusCode == http.StatusTooManyRequests && throttleRetries < maxThrottleRetries:
			throttleRetries++
			delay := retryAfter(resp)
			drainClose(resp)
			slog.Warn("Graph throttled request, backing off", "module", "GRAPHBACKEND",
				"retry", throttleRetries, "delay", delay)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		case (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout) && !transientRetried:
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
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal graph request body: %w", err)
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
		return fmt.Errorf("failed to decode graph response: %w", err)
	}
	return nil
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

// FetchMessages returns the changes in folder since cursor via the Graph delta
// query. One call consumes exactly one delta page — Graph controls the page
// size, so limit is only a soft hint and is not enforced here. Each changed
// message triggers a separate raw MIME ($value) fetch, because the delta JSON
// has no RFC822 body; a message whose body fetch fails is skipped with a
// warning rather than aborting the batch. HasMore reports a pending
// @odata.nextLink; the returned cursor is that nextLink, or the
// @odata.deltaLink once the delta round is complete.
func (b *Backend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	_ = limit // Soft hint only: Graph fixes the delta page size server-side.
	var result backend.FetchResult

	freshURL := fmt.Sprintf("%s%s/mailFolders/%s/messages/delta?$select=%s",
		b.baseURL, b.mailbox, url.PathEscape(folder), deltaSelect)
	pageURL := string(cursor)
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
	result.Messages = b.fetchBodies(ctx, folder, content)

	if page.NextLink != "" {
		result.Cursor = backend.Cursor(page.NextLink)
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

// fetchBodies downloads the raw MIME for each content item concurrently and
// returns the successfully fetched messages. A per-message fetch failure skips
// only that message (logged), never the whole page.
func (b *Backend) fetchBodies(ctx context.Context, folder string, items []deltaItem) []backend.Message {
	msgs := make([]backend.Message, len(items)) // indexed by item; failures stay zero

	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchConcurrency)
	for i := range items {
		// Acquire a slot, or bail out early if the sync was cancelled.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return filterFetched(msgs)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			msg, err := b.fetchOne(ctx, folder, items[i])
			if err != nil {
				slog.Warn("Failed to fetch raw MIME, skipping message", "module", "GRAPHBACKEND",
					"folder", folder, "id", items[i].ID, "err", err)
				return
			}
			msgs[i] = msg // own slot: no shared write, no mutex needed
		}(i)
	}
	wg.Wait()
	return filterFetched(msgs)
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
const batchLimit = 20

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
	ID     string          `json:"id"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
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
	for start := 0; start < len(refs); start += batchLimit {
		chunk := refs[start:min(start+batchLimit, len(refs))]

		// Sub-request ids are the chunk indices, mapped back to ref ids on the
		// way out — Graph does not guarantee response ordering.
		requests := make([]batchRequest, len(chunk))
		refIDByRequestID := make(map[string]string, len(chunk))
		for i, ref := range chunk {
			requestID := strconv.Itoa(i)
			requests[i] = batchRequest{
				ID:     requestID,
				Method: http.MethodGet,
				URL:    b.mailbox + "/messages/" + url.PathEscape(ref.ID) + "?$select=id,isRead,flag",
			}
			refIDByRequestID[requestID] = ref.ID
		}

		var envelope struct {
			Responses []batchResponseItem `json:"responses"`
		}
		if err := b.doJSON(ctx, http.MethodPost, b.baseURL+"/$batch",
			map[string]any{"requests": requests}, &envelope); err != nil {
			return nil, fmt.Errorf("failed to batch-fetch flags in %s: %w", folder, err)
		}

		for _, item := range envelope.Responses {
			refID, ok := refIDByRequestID[item.ID]
			if !ok {
				slog.Warn("Ignoring batch sub-response with unknown id", "module", "GRAPHBACKEND",
					"folder", folder, "request_id", item.ID)
				continue
			}
			if item.Status < 200 || item.Status >= 300 {
				// 404 = message gone since the last delta: expected, stays absent
				// from the map. Other statuses are unexpected but also just skip
				// the one message rather than failing the whole reconciliation.
				if item.Status != http.StatusNotFound {
					slog.Warn("Batch flag fetch failed for message, skipping", "module", "GRAPHBACKEND",
						"folder", folder, "id", refID, "status", item.Status)
				}
				continue
			}
			var msg flagFields
			if err := json.Unmarshal(item.Body, &msg); err != nil {
				return nil, fmt.Errorf("failed to decode batched flags for %s: %w", refID, err)
			}
			flags[refID] = flagsFromGraph(msg.IsRead, msg.Flag)
		}
	}

	slog.Debug("Fetched flags", "module", "GRAPHBACKEND", "folder", folder, // encgrep:allow logs folder name and flag counts, not flag values or message content
		"requested", len(refs), "resolved", len(flags))
	return flags, nil
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
func (b *Backend) Move(ctx context.Context, ref backend.RemoteRef, destFolder string) (backend.RemoteRef, error) {
	var moved struct {
		ID string `json:"id"`
	}
	reqURL := b.baseURL + b.mailbox + "/messages/" + url.PathEscape(ref.ID) + "/move"
	if err := b.doJSON(ctx, http.MethodPost, reqURL,
		map[string]string{"destinationId": destFolder}, &moved); err != nil {
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

// Watch is a no-op watcher for now: it blocks until ctx is done and never
// signals a change, so the sync engine falls back to its normal poll cadence.
// A real delta-poll (or webhook) watcher arrives in part 2.
func (b *Backend) Watch(ctx context.Context, _ string, _ func()) error {
	<-ctx.Done()
	return ctx.Err()
}

// Capabilities reports Graph backend behavior: M365 auto-saves sent mail
// server-side, moves are native atomic operations, and (until the part-2
// watcher lands) there is no push notification support.
func (b *Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		ServerSideSent:     true,
		NativeMove:         true,
		PushWatch:          false,
		FlagChangesInDelta: true,
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
