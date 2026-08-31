package jmapbackend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/redact"
)

const (
	coreCapability       = "urn:ietf:params:jmap:core"
	mailCapability       = "urn:ietf:params:jmap:mail"
	submissionCapability = "urn:ietf:params:jmap:submission"
	maxSessionBytes      = 4 << 20
	maxMethodBytes       = 64 << 20
	maxMessageBytes      = 100 << 20
	maxJSONSafeInteger   = int64(1<<53 - 1)
	maxClientAPIRequests = 16
	maxClientUploads     = 4
)

type session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]sessionAccount  `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
}

type sessionAccount struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

type coreLimits struct {
	MaxSizeUpload         int64
	MaxConcurrentUpload   int
	MaxSizeRequest        int64
	MaxConcurrentRequests int
	MaxCallsInRequest     int
	MaxObjectsInGet       int
	MaxObjectsInSet       int
	CollationAlgorithms   []string
}

func (l *coreLimits) UnmarshalJSON(data []byte) error {
	var wire struct {
		MaxSizeUpload         *int64    `json:"maxSizeUpload"`
		MaxConcurrentUpload   *int64    `json:"maxConcurrentUpload"`
		MaxSizeRequest        *int64    `json:"maxSizeRequest"`
		MaxConcurrentRequests *int64    `json:"maxConcurrentRequests"`
		MaxCallsInRequest     *int64    `json:"maxCallsInRequest"`
		MaxObjectsInGet       *int64    `json:"maxObjectsInGet"`
		MaxObjectsInSet       *int64    `json:"maxObjectsInSet"`
		CollationAlgorithms   *[]string `json:"collationAlgorithms"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	limits := []struct {
		name  string
		value *int64
	}{
		{"maxSizeUpload", wire.MaxSizeUpload},
		{"maxConcurrentUpload", wire.MaxConcurrentUpload},
		{"maxSizeRequest", wire.MaxSizeRequest},
		{"maxConcurrentRequests", wire.MaxConcurrentRequests},
		{"maxCallsInRequest", wire.MaxCallsInRequest},
		{"maxObjectsInGet", wire.MaxObjectsInGet},
		{"maxObjectsInSet", wire.MaxObjectsInSet},
	}
	for _, limit := range limits {
		if limit.value == nil {
			return fmt.Errorf("missing required %s", limit.name)
		}
		if *limit.value < 0 || *limit.value > maxJSONSafeInteger {
			return fmt.Errorf("%s is outside the JMAP UnsignedInt range", limit.name)
		}
	}
	if wire.CollationAlgorithms == nil {
		return errors.New("missing required collationAlgorithms")
	}
	l.MaxSizeUpload = *wire.MaxSizeUpload
	l.MaxConcurrentUpload = int(*wire.MaxConcurrentUpload)
	l.MaxSizeRequest = *wire.MaxSizeRequest
	l.MaxConcurrentRequests = int(*wire.MaxConcurrentRequests)
	l.MaxCallsInRequest = int(*wire.MaxCallsInRequest)
	l.MaxObjectsInGet = int(*wire.MaxObjectsInGet)
	l.MaxObjectsInSet = int(*wire.MaxObjectsInSet)
	l.CollationAlgorithms = *wire.CollationAlgorithms
	return nil
}

type credential struct {
	mode     string
	username string
	secret   string
}

type client struct {
	httpClient *http.Client
	sessionURL string
	credential credential
	session    session
	accountID  string
	limits     coreLimits
	apiSem     chan struct{}
	uploadSem  chan struct{}
}

type methodEnvelope struct {
	Using       []string        `json:"using"`
	MethodCalls [][]interface{} `json:"methodCalls"`
}

type methodResponseEnvelope struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
	SessionState    string            `json:"sessionState"`
}

type methodResponseTarget struct {
	name string
	out  interface{}
	seen bool
}

type methodError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e *methodError) Error() string {
	if e.Description == "" {
		return "JMAP method error: " + e.Type
	}
	return fmt.Sprintf("JMAP method error %s: %s", e.Type, e.Description)
}

func (e *methodError) SafeLogText() string {
	return "JMAP method error: provider details " + redact.Placeholder
}

var _ redact.SafeLogError = (*methodError)(nil)

type statusError struct {
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("JMAP request failed: status %d: %s", e.Status, e.Body)
}

func (e *statusError) SafeLogText() string {
	return fmt.Sprintf("JMAP request failed: status %d: response body %s", e.Status, redact.Placeholder)
}

var _ redact.SafeLogError = (*statusError)(nil)

func (c *client) discover(ctx context.Context) error {
	resp, err := c.doHTTP(ctx, http.MethodGet, c.sessionURL, nil, "", true)
	if err != nil {
		return fmt.Errorf("discover JMAP session: %w", err)
	}
	defer resp.Body.Close()
	if err := decodeJSONLimited(resp.Body, maxSessionBytes, &c.session); err != nil {
		return fmt.Errorf("decode JMAP session: %w", err)
	}
	if _, ok := c.session.Capabilities[coreCapability]; !ok {
		return errors.New("JMAP server does not advertise the core capability")
	}
	if err := json.Unmarshal(c.session.Capabilities[coreCapability], &c.limits); err != nil {
		return fmt.Errorf("decode JMAP core limits: %w", err)
	}
	if c.limits.MaxCallsInRequest == 0 || c.limits.MaxObjectsInGet == 0 ||
		c.limits.MaxConcurrentRequests == 0 || c.limits.MaxSizeRequest == 0 {
		return errors.New("JMAP core capability does not permit API reads")
	}
	c.apiSem = make(chan struct{}, min(c.limits.MaxConcurrentRequests, maxClientAPIRequests))
	if c.limits.MaxConcurrentUpload > 0 {
		c.uploadSem = make(chan struct{}, min(c.limits.MaxConcurrentUpload, maxClientUploads))
	}
	accountID := c.session.PrimaryAccounts[mailCapability]
	account, primaryOK := c.session.Accounts[accountID]
	_, primaryHasMail := account.AccountCapabilities[mailCapability]
	if accountID == "" || !primaryOK || !primaryHasMail || account.IsReadOnly {
		var candidates []string
		for id, account := range c.session.Accounts {
			if _, ok := account.AccountCapabilities[mailCapability]; ok && !account.IsReadOnly {
				candidates = append(candidates, id)
			}
		}
		sort.Strings(candidates)
		accountID = ""
		if len(candidates) > 0 {
			accountID = candidates[0]
		}
	}
	if accountID == "" {
		return errors.New("JMAP session has no writable mail account")
	}
	if strings.TrimSpace(c.session.APIURL) == "" {
		return errors.New("JMAP session has no apiUrl")
	}
	c.accountID = accountID
	c.session.APIURL = resolveURL(c.sessionURL, c.session.APIURL)
	c.session.DownloadURL = resolveURL(c.sessionURL, c.session.DownloadURL)
	c.session.UploadURL = resolveURL(c.sessionURL, c.session.UploadURL)
	c.session.EventSourceURL = resolveURL(c.sessionURL, c.session.EventSourceURL)
	for name, endpoint := range map[string]string{
		"apiUrl": c.session.APIURL, "downloadUrl": c.session.DownloadURL,
		"uploadUrl": c.session.UploadURL, "eventSourceUrl": c.session.EventSourceURL,
	} {
		if endpoint != "" {
			if err := validateJMAPURL(endpoint); err != nil {
				return fmt.Errorf("invalid JMAP %s: %w", name, err)
			}
		}
	}
	return nil
}

func resolveURL(base, ref string) string {
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.IsAbs() {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	return b.ResolveReference(u).String()
}

func (c *client) call(ctx context.Context, using []string, method string, args, out interface{}, additional ...*methodResponseTarget) error {
	if c.apiSem != nil && strings.HasSuffix(method, "/set") && c.limits.MaxObjectsInSet == 0 {
		return fmt.Errorf("JMAP core capability does not permit %s operations", method)
	}
	body, err := json.Marshal(methodEnvelope{
		Using:       using,
		MethodCalls: [][]interface{}{{method, args, "0"}},
	})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	requestLimit := int64(maxMethodBytes)
	if c.limits.MaxSizeRequest > 0 && c.limits.MaxSizeRequest < requestLimit {
		requestLimit = c.limits.MaxSizeRequest
	}
	if int64(len(body)) > requestLimit {
		return fmt.Errorf("%s request is %d bytes, exceeds JMAP maxSizeRequest %d", method, len(body), requestLimit)
	}
	release, err := acquire(ctx, c.apiSem)
	if err != nil {
		return err
	}
	defer release()
	safeToRetry := strings.HasSuffix(method, "/get") || strings.HasSuffix(method, "/query") || strings.HasSuffix(method, "/changes")
	resp, err := c.doHTTP(ctx, http.MethodPost, c.session.APIURL, body, "application/json", safeToRetry)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	var envelope methodResponseEnvelope
	if err := decodeJSONLimited(resp.Body, maxMethodBytes, &envelope); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if len(envelope.MethodResponses) == 0 {
		return fmt.Errorf("%s returned no method responses", method)
	}
	var responseErr *methodError
	var protocolErr error
	recordProtocolError := func(err error) {
		if protocolErr == nil {
			protocolErr = err
		}
	}
	found := false
	for _, raw := range envelope.MethodResponses {
		var tuple []json.RawMessage
		if err := json.Unmarshal(raw, &tuple); err != nil {
			recordProtocolError(fmt.Errorf("decode %s method response: %w", method, err))
			continue
		}
		if len(tuple) != 3 {
			recordProtocolError(fmt.Errorf("decode %s method response: tuple has %d elements, want 3", method, len(tuple)))
			continue
		}
		var callID string
		if err := json.Unmarshal(tuple[2], &callID); err != nil {
			recordProtocolError(fmt.Errorf("decode %s response call ID: %w", method, err))
			continue
		}
		if callID != "0" {
			recordProtocolError(fmt.Errorf("JMAP %s response has call ID %q, want %q", method, callID, "0"))
			continue
		}
		var responseName string
		if err := json.Unmarshal(tuple[0], &responseName); err != nil {
			recordProtocolError(fmt.Errorf("decode %s response name: %w", method, err))
			continue
		}
		if responseName == "error" {
			var methodErr methodError
			if err := decodeMethodPayload(tuple[1], &methodErr); err != nil {
				recordProtocolError(fmt.Errorf("decode %s method error: %w", method, err))
				continue
			}
			if methodErr.Type == "" {
				recordProtocolError(fmt.Errorf("decode %s method error: missing type", method))
				continue
			}
			responseErr = &methodErr
			continue
		}
		if responseName == method {
			if found {
				recordProtocolError(fmt.Errorf("JMAP response included duplicate %s results", method))
				continue
			}
			if err := decodeMethodPayload(tuple[1], out); err != nil {
				recordProtocolError(fmt.Errorf("decode %s result: %w", method, err))
				continue
			}
			found = true
			continue
		}
		for _, target := range additional {
			if responseName != target.name {
				continue
			}
			if target.seen {
				recordProtocolError(fmt.Errorf("JMAP response included duplicate %s results", responseName))
				break
			}
			if err := decodeMethodPayload(tuple[1], target.out); err != nil {
				recordProtocolError(fmt.Errorf("decode %s result: %w", responseName, err))
				break
			}
			target.seen = true
			break
		}
	}
	if responseErr != nil {
		return responseErr
	}
	if protocolErr != nil {
		return protocolErr
	}
	if !found {
		return fmt.Errorf("JMAP response did not include %s", method)
	}
	return nil
}

func decodeMethodPayload(raw json.RawMessage, out interface{}) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("payload is not a non-null JSON object")
	}
	if out == nil {
		return nil
	}
	target := reflect.ValueOf(out)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return errors.New("result target is not a non-nil pointer")
	}
	decoded := reflect.New(target.Elem().Type())
	if err := json.Unmarshal(raw, decoded.Interface()); err != nil {
		return err
	}
	target.Elem().Set(decoded.Elem())
	return nil
}

func (c *client) doHTTP(ctx context.Context, method, requestURL string, body []byte, contentType string, safeToRetry bool) (*http.Response, error) {
	if err := validateJMAPURL(requestURL); err != nil {
		return nil, err
	}
	const maxRetries = 3
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
		if err != nil {
			return nil, fmt.Errorf("build JMAP request: %w", err)
		}
		c.authorize(req)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("execute JMAP request: %w", err)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if safeToRetry && attempt < maxRetries && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			delay := time.Duration(1<<attempt) * time.Second
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
			slog.Warn("JMAP request throttled or unavailable, backing off", "module", "JMAPBACKEND",
				"status", resp.StatusCode, "retry", attempt+1, "delay", delay)
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		return nil, &statusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
	}
}

func (c *client) authorize(req *http.Request) {
	if c.credential.mode == "bearer" {
		req.Header.Set("Authorization", "Bearer "+c.credential.secret)
		return
	}
	req.SetBasicAuth(c.credential.username, c.credential.secret)
}

func validateJMAPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("JMAP endpoint must be an absolute HTTP(S) URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("unencrypted JMAP endpoints are only allowed on loopback")
	}
	return nil
}

func validateJMAPRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if err := validateJMAPURL(req.URL.String()); err != nil {
		return err
	}
	if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
		return errors.New("JMAP redirect origin differs from request origin")
	}
	return nil
}
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
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

func decodeJSONLimited(r io.Reader, limit int64, out interface{}) error {
	data, err := readLimited(r, limit, "JMAP JSON response")
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func readLimited(r io.Reader, limit int64, description string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", description, limit)
	}
	return data, nil
}

func (c *client) upload(ctx context.Context, data []byte, contentType string) (string, error) {
	if c.session.UploadURL == "" {
		return "", errors.New("JMAP session has no uploadUrl")
	}
	if c.apiSem != nil && (c.limits.MaxSizeUpload == 0 || c.limits.MaxConcurrentUpload == 0) {
		return "", errors.New("JMAP core capability does not permit uploads")
	}
	if c.limits.MaxSizeUpload > 0 && int64(len(data)) > c.limits.MaxSizeUpload {
		return "", fmt.Errorf("JMAP upload is %d bytes, exceeds maxSizeUpload %d", len(data), c.limits.MaxSizeUpload)
	}
	release, err := acquire(ctx, c.uploadSem)
	if err != nil {
		return "", err
	}
	defer release()
	u := expandTemplate(c.session.UploadURL, map[string]string{"accountId": c.accountID})
	resp, err := c.doHTTP(ctx, http.MethodPost, u, data, contentType, false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		BlobID string `json:"blobId"`
	}
	if err := decodeJSONLimited(resp.Body, maxSessionBytes, &result); err != nil {
		return "", fmt.Errorf("decode JMAP upload response: %w", err)
	}
	if result.BlobID == "" {
		return "", errors.New("JMAP upload returned no blobId")
	}
	return result.BlobID, nil
}

func acquire(ctx context.Context, sem chan struct{}) (func(), error) {
	if sem == nil {
		return func() {}, nil
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *client) maxObjectsInGet(fallback int) int {
	if fallback <= 0 {
		fallback = 1
	}
	if c.limits.MaxObjectsInGet > 0 && c.limits.MaxObjectsInGet < fallback {
		return c.limits.MaxObjectsInGet
	}
	return fallback
}

func (c *client) download(ctx context.Context, blobID, name, mediaType string) (io.ReadCloser, error) {
	if c.session.DownloadURL == "" {
		return nil, errors.New("JMAP session has no downloadUrl")
	}
	u := expandTemplate(c.session.DownloadURL, map[string]string{
		"accountId": c.accountID,
		"blobId":    blobID,
		"name":      name,
		"type":      mediaType,
	})
	resp, err := c.doHTTP(ctx, http.MethodGet, u, nil, "", true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func expandTemplate(template string, values map[string]string) string {
	for key, value := range values {
		template = strings.ReplaceAll(template, "{"+key+"}", url.PathEscape(value))
	}
	return template
}

func (c *client) watch(ctx context.Context, onChange func()) error {
	if c.session.EventSourceURL == "" {
		return errors.New("JMAP session has no eventSourceUrl")
	}
	u, err := url.Parse(c.session.EventSourceURL)
	if err != nil {
		return fmt.Errorf("parse JMAP eventSourceUrl: %w", err)
	}
	q := u.Query()
	q.Set("types", "Email")
	q.Set("closeafter", "no")
	q.Set("ping", "30")
	u.RawQuery = q.Encode()

	eventClient := *c.httpClient
	eventClient.Timeout = 0
	var lastEventID string
	retryAttempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		c.authorize(req)
		req.Header.Set("Accept", "text/event-stream")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := eventClient.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			lastEventID, err = c.consumeEvents(ctx, resp.Body, lastEventID, onChange)
			_ = resp.Body.Close()
		} else if resp != nil {
			responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return &statusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			retryAttempt = 0
		} else if retryAttempt < 6 {
			retryAttempt++
		}
		delay := time.Duration(1<<retryAttempt) * time.Second
		if delay > time.Minute {
			delay = time.Minute
		}
		if resp != nil {
			if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
		}
		slog.Warn("JMAP EventSource disconnected, reconnecting", "module", "JMAPBACKEND",
			"retry", retryAttempt, "delay", delay)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *client) consumeEvents(ctx context.Context, r io.Reader, lastID string, onChange func()) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var data strings.Builder
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return lastID, err
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			lastID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if stateChangeIncludesEmail(data.String(), c.accountID) {
				onChange()
			}
			data.Reset()
		}
	}
	return lastID, scanner.Err()
}

func stateChangeIncludesEmail(data, accountID string) bool {
	if data == "" {
		return false
	}
	var event struct {
		Type    string                       `json:"@type"`
		Changed map[string]map[string]string `json:"changed"`
	}
	if json.Unmarshal([]byte(data), &event) != nil || event.Type != "StateChange" {
		return false
	}
	_, ok := event.Changed[accountID]["Email"]
	return ok
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
