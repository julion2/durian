package jmapbackend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	coreCapability       = "urn:ietf:params:jmap:core"
	mailCapability       = "urn:ietf:params:jmap:mail"
	submissionCapability = "urn:ietf:params:jmap:submission"
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
	resp, err := c.doHTTP(ctx, http.MethodGet, c.sessionURL, nil, "")
	if err != nil {
		return fmt.Errorf("discover JMAP session: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&c.session); err != nil {
		return fmt.Errorf("decode JMAP session: %w", err)
	}
	if _, ok := c.session.Capabilities[coreCapability]; !ok {
		return errors.New("JMAP server does not advertise the core capability")
	}
	accountID := c.session.PrimaryAccounts[mailCapability]
	if accountID == "" {
		for id, account := range c.session.Accounts {
			if _, ok := account.AccountCapabilities[mailCapability]; ok && !account.IsReadOnly {
				accountID = id
				break
			}
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
	resp, err := c.doHTTP(ctx, http.MethodPost, c.session.APIURL, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope methodResponseEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
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

func (c *client) doHTTP(ctx context.Context, method, requestURL string, body []byte, contentType string) (*http.Response, error) {
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
		if attempt < maxRetries && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			delay := time.Duration(1<<attempt) * time.Second
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
				delay = time.Duration(seconds) * time.Second
			}
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

func (c *client) upload(ctx context.Context, data []byte, contentType string) (string, error) {
	if c.session.UploadURL == "" {
		return "", errors.New("JMAP session has no uploadUrl")
	}
	u := expandTemplate(c.session.UploadURL, map[string]string{"accountId": c.accountID})
	resp, err := c.doHTTP(ctx, http.MethodPost, u, data, contentType)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		BlobID string `json:"blobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
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
	resp, err := c.doHTTP(ctx, http.MethodGet, u, nil, "")
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

	var lastEventID string
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
		resp, err := c.httpClient.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			lastEventID, err = c.consumeEvents(ctx, resp.Body, lastEventID, onChange)
			_ = resp.Body.Close()
		} else if resp != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (c *client) consumeEvents(ctx context.Context, r io.Reader, lastID string, onChange func()) (string, error) {
	scanner := bufio.NewScanner(r)
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
