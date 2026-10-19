package demotest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var messageID = regexp.MustCompile(`^[A-Za-z0-9_+=-]{1,1000}$`)

type mailGuard struct {
	mu        sync.Mutex
	google    bool
	allowed   map[string]bool
	owner     string
	token     TokenFunc
	transport http.RoundTripper
	owned     map[string]bool
}

func newGuard(google bool, allowed map[string]bool, owner string, token TokenFunc, transport http.RoundTripper) *mailGuard {
	return &mailGuard{google: google, allowed: allowed, owner: owner, token: token, transport: transport, owned: map[string]bool{}}
}

func (g *mailGuard) client() *http.Client {
	return &http.Client{Transport: g, Timeout: 60 * time.Second, CheckRedirect: noRedirect}
}

func blocked() error {
	return fmt.Errorf("mail demo HTTP guard blocked unsafe or unsupported operation")
}

// RoundTrip is the final recipient/operation boundary, even for callers that
// bypass Sender. It serializes guard-owned operations and consumes draft send
// capabilities before transport, preventing replay after an uncertain outcome.
func (g *mailGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.Body != nil {
		defer req.Body.Close()
	}
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Fragment != "" ||
		req.URL.Opaque != "" || req.URL.RawPath != "" || (req.Host != "" && req.Host != req.URL.Host) ||
		req.Header.Get("X-HTTP-Method-Override") != "" || req.Header.Get("Content-Encoding") != "" {
		return nil, blocked()
	}
	base, host := "/v1.0/me/", "graph.microsoft.com"
	if g.google {
		base, host = "/gmail/v1/users/me/", "gmail.googleapis.com"
	}
	if req.URL.Host != host {
		return nil, blocked()
	}
	// FetchFlags is the sole batch exception: Graph's batch endpoint sits
	// outside /me, so validate the host above and every relative request below.
	// Never grant a general-purpose batch capability (including GET /users).
	if !g.google && req.URL.Path == "/v1.0/$batch" {
		if req.Method != http.MethodPost || req.URL.RawQuery != "" || req.Header.Get("Content-Type") != "application/json" {
			return nil, blocked()
		}
		body, err := readBody(req.Body, 32<<10)
		if err != nil || !flagBatch(body) {
			return nil, blocked()
		}
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		return g.exchange(clone)
	}
	if !strings.HasPrefix(req.URL.Path, base) {
		return nil, blocked()
	}
	path := strings.TrimPrefix(req.URL.Path, base)
	if req.Method == http.MethodGet && readable(path, g.google) {
		if !readQuery(req.URL.RawQuery, g.google) {
			return nil, blocked()
		}
		return g.exchange(req)
	}
	if req.URL.RawQuery != "" || req.Method != http.MethodPost && req.Method != http.MethodPatch {
		return nil, blocked()
	}
	if !(path == "sendMail" && !g.google) && !(strings.HasSuffix(path, "/send") && !g.google) && req.Header.Get("Content-Type") != "application/json" {
		return nil, blocked()
	}
	body, err := readBody(req.Body, maxPayload)
	if err != nil {
		return nil, blocked()
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	create := false
	if g.google {
		switch {
		case req.Method == http.MethodPost && path == "drafts":
			if !g.gmailRaw(body, true) {
				return nil, blocked()
			}
			create = true
		case req.Method == http.MethodPost && path == "messages/send":
			if !g.gmailRaw(body, false) {
				return nil, blocked()
			}
		case req.Method == http.MethodPost && path == "drafts/send":
			o, ok := object(body)
			id := textValue(o["id"])
			if !ok || !keys(o, "id") || !g.owned[id] || !g.currentDraft(req.Context(), id) {
				return nil, blocked()
			}
			delete(g.owned, id)
		default:
			return nil, blocked()
		}
	} else {
		parts := strings.Split(path, "/")
		switch {
		case strings.HasSuffix(path, "/attachments/createUploadSession"):
			return nil, fmt.Errorf("mail demo does not support Graph large attachment upload sessions")
		case req.Method == http.MethodPost && path == "messages":
			if !g.graphDraft(body) {
				return nil, blocked()
			}
			create = true
		case req.Method == http.MethodPost && path == "sendMail":
			// Only the canonical raw MIME form is supported, not JSON wrappers
			// or inferred-recipient reply/forward operations.
			raw, err := base64.StdEncoding.DecodeString(string(body))
			if req.Header.Get("Content-Type") != "text/plain" || err != nil || !g.mime(raw) {
				return nil, blocked()
			}
		case len(parts) == 3 && parts[0] == "messages" && g.owned[parts[1]] && req.Method == http.MethodPost && parts[2] == "send":
			if len(body) != 0 || !g.currentDraft(req.Context(), parts[1]) {
				return nil, blocked()
			}
			delete(g.owned, parts[1])
		case len(parts) == 3 && parts[0] == "messages" && g.owned[parts[1]] && req.Method == http.MethodPost && parts[2] == "attachments":
			if !fileAttachment(body) {
				return nil, blocked()
			}
		case len(parts) == 2 && parts[0] == "messages" && g.owned[parts[1]] && req.Method == http.MethodPatch:
			if !g.graphDraft(body) {
				return nil, blocked()
			}
		default:
			// Includes all move/reply/forward, upload sessions, /users,
			// rules, subscriptions and unknown operations. No host-wide grant.
			return nil, blocked()
		}
	}
	resp, err := g.exchange(req)
	if err != nil || !create || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	data, err := readBody(resp.Body, maxMetadata)
	if err != nil {
		return nil, fmt.Errorf("mail demo invalid draft creation response; inspect before retrying")
	}
	o, ok := object(data)
	id := textValue(o["id"])
	if !ok || !messageID.MatchString(id) || g.owned[id] {
		return nil, fmt.Errorf("mail demo invalid draft creation identity; inspect before retrying")
	}
	g.owned[id] = true
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

// flagBatch accepts only graphbackend.FetchFlags' exact read-only shape.
// Unknown/duplicate fields, bodies, headers, dependencies, extra projections,
// encoded paths and absolute URLs all fail closed, even in a mixed batch.
func flagBatch(data []byte) bool {
	o, ok := object(data)
	if !ok || !keys(o, "requests") {
		return false
	}
	var requests []json.RawMessage
	if json.Unmarshal(o["requests"], &requests) != nil || len(requests) == 0 || len(requests) > 20 {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range requests {
		r, ok := object(raw)
		id := textValue(r["id"])
		if !ok || !keys(r, "id", "method", "url") || len(id) == 0 || len(id) > 6 || seen[id] || textValue(r["method"]) != http.MethodGet {
			return false
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				return false
			}
		}
		seen[id] = true
		u := textValue(r["url"])
		const prefix, suffix = "/me/messages/", "?$select=id,isRead,flag"
		if !strings.HasPrefix(u, prefix) || !strings.HasSuffix(u, suffix) ||
			!messageID.MatchString(strings.TrimSuffix(strings.TrimPrefix(u, prefix), suffix)) {
			return false
		}
	}
	return true
}

func readable(path string, google bool) bool {
	p := strings.Split(path, "/")
	id := func(i int) bool { return i < len(p) && messageID.MatchString(p[i]) }
	if google {
		switch p[0] {
		case "profile", "history":
			return len(p) == 1
		case "labels":
			return len(p) == 1 || len(p) == 2 && id(1)
		case "messages", "drafts", "threads":
			return len(p) == 1 || len(p) == 2 && id(1) || p[0] == "messages" && len(p) == 4 && id(1) && p[2] == "attachments" && id(3)
		}
		return false
	}
	switch p[0] {
	case "messages":
		return len(p) == 1 || len(p) == 2 && id(1) || len(p) == 3 && id(1) && (p[2] == "$value" || p[2] == "attachments") ||
			len(p) == 4 && id(1) && p[2] == "attachments" && id(3)
	case "mailFolders":
		return len(p) == 1 || len(p) == 2 && id(1) || len(p) == 3 && id(1) && (p[2] == "messages" || p[2] == "childFolders") ||
			len(p) == 4 && id(1) && p[2] == "messages" && p[3] == "delta"
	}
	return false
}

func readQuery(raw string, google bool) bool {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return false
	}
	allowed := strings.Fields("$select $filter $top $skip $skiptoken $deltatoken $orderby $count $search")
	if google {
		allowed = strings.Fields("format metadataHeaders fields q maxResults pageToken labelIds includeSpamTrash startHistoryId historyTypes")
	}
	for key := range q {
		found := false
		for _, a := range allowed {
			found = found || key == a
		}
		if !found {
			return false
		}
	}
	return true
}

func readBody(body io.ReadCloser, max int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, blocked()
	}
	return data, nil
}

func (g *mailGuard) exchange(req *http.Request) (*http.Response, error) {
	token, err := g.token(req.Context())
	if err != nil || token == "" {
		return nil, fmt.Errorf("mail demo token unavailable (details withheld)")
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.transport.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("mail demo transport outcome unknown (details withheld); inspect before retrying")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Neither provider error snippets nor redirect destinations can escape
		// into outbox logs. A client cannot follow a redirect, even if its
		// CheckRedirect is replaced by a caller.
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader("mail demo provider details withheld"))
		resp.ContentLength = -1
		resp.Header.Del("Location")
	}
	return resp, nil
}

func (g *mailGuard) currentDraft(ctx context.Context, id string) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	endpoint := "https://graph.microsoft.com/v1.0/me/messages/" + url.PathEscape(id) + "?$select=id,isDraft,toRecipients,ccRecipients,bccRecipients"
	if g.google {
		endpoint = "https://gmail.googleapis.com/gmail/v1/users/me/drafts/" + url.PathEscape(id) + "?format=raw"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := g.exchange(req)
	if err != nil {
		return false
	}
	limit := int64(maxMetadata)
	if g.google {
		limit = maxPayload
	}
	data, err := readBody(resp.Body, limit)
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	o, ok := object(data)
	if !ok || textValue(o["id"]) != id {
		return false
	}
	if g.google {
		m, ok := object(o["message"])
		if !ok {
			return false
		}
		raw, err := base64.URLEncoding.DecodeString(textValue(m["raw"]))
		if err != nil {
			raw, err = base64.RawURLEncoding.DecodeString(textValue(m["raw"]))
		}
		return err == nil && g.mime(raw)
	}
	var draft bool
	if json.Unmarshal(o["isDraft"], &draft) != nil || !draft {
		return false
	}
	for _, key := range []string{"toRecipients", "ccRecipients", "bccRecipients"} {
		if _, ok := o[key]; !ok {
			return false
		}
	}
	return g.graphRecipients(o)
}
