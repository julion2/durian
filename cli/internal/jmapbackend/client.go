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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	coreCapability       = "urn:ietf:params:jmap:core"
	mailCapability       = "urn:ietf:params:jmap:mail"
	submissionCapability = "urn:ietf:params:jmap:submission"
	maxSessionBytes      = 4 << 20
	maxMethodBytes       = 64 << 20
	maxMessageBytes      = 100 << 20
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
}

type methodEnvelope struct {
	Using       []string        `json:"using"`
	MethodCalls [][]interface{} `json:"methodCalls"`
}

type methodResponseEnvelope struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
	SessionState    string            `json:"sessionState"`
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

type statusError struct {
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("JMAP request failed: status %d: %s", e.Status, e.Body)
}

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

func (c *client) call(ctx context.Context, using []string, method string, args, out interface{}) error {
	body, err := json.Marshal(methodEnvelope{
		Using:       using,
		MethodCalls: [][]interface{}{{method, args, "0"}},
	})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
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
	for _, raw := range envelope.MethodResponses {
		var tuple []json.RawMessage
		if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) < 2 {
			return fmt.Errorf("decode %s method response: %w", method, err)
		}
		var responseName string
		if err := json.Unmarshal(tuple[0], &responseName); err != nil {
			return fmt.Errorf("decode %s response name: %w", method, err)
		}
		if responseName == "error" {
			var methodErr methodError
			if err := json.Unmarshal(tuple[1], &methodErr); err != nil {
				return fmt.Errorf("decode %s method error: %w", method, err)
			}
			responseErr = &methodErr
			continue
		}
		if responseName != method {
			// EmailSubmission/set may produce an additional implicit Email/set
			// response for onSuccessUpdateEmail. Select the response belonging to
			// the requested method rather than rejecting the valid side effect.
			continue
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(tuple[1], out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}
	if responseErr != nil {
		return responseErr
	}
	return fmt.Errorf("JMAP response did not include %s", method)
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
