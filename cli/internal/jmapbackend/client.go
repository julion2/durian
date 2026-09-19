package jmapbackend

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	maxEventBytes        = 1 << 20
	minEventRetry        = time.Second
	maxEventRetry        = 5 * time.Minute
	eventHeaderTimeout   = 30 * time.Second
	eventIdleTimeout     = 90 * time.Second
	maxJSONSafeInteger   = int64(1<<53 - 1)
	maxClientAPIRequests = 16
	maxClientUploads     = 4
)

type session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]sessionAccount  `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Username        *string                    `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`
}

type sessionAccount struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

func (s *session) UnmarshalJSON(data []byte) error {
	var wire struct {
		Capabilities    *map[string]json.RawMessage `json:"capabilities"`
		Accounts        *map[string]sessionAccount  `json:"accounts"`
		PrimaryAccounts *map[string]string          `json:"primaryAccounts"`
		Username        *string                     `json:"username"`
		APIURL          *string                     `json:"apiUrl"`
		DownloadURL     *string                     `json:"downloadUrl"`
		UploadURL       *string                     `json:"uploadUrl"`
		EventSourceURL  *string                     `json:"eventSourceUrl"`
		State           *string                     `json:"state"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	requiredObjects := []struct {
		name  string
		value any
	}{
		{"capabilities", wire.Capabilities},
		{"accounts", wire.Accounts},
		{"primaryAccounts", wire.PrimaryAccounts},
	}
	for _, field := range requiredObjects {
		if reflect.ValueOf(field.value).IsNil() {
			return fmt.Errorf("missing required %s", field.name)
		}
	}
	requiredStrings := []struct {
		name  string
		value *string
	}{
		{"username", wire.Username},
		{"apiUrl", wire.APIURL},
		{"downloadUrl", wire.DownloadURL},
		{"uploadUrl", wire.UploadURL},
		{"eventSourceUrl", wire.EventSourceURL},
		{"state", wire.State},
	}
	for _, field := range requiredStrings {
		if field.value == nil {
			return fmt.Errorf("missing required %s", field.name)
		}
	}
	if err := validateCapabilityObjects("capabilities", *wire.Capabilities); err != nil {
		return err
	}
	for id, account := range *wire.Accounts {
		if !validJMAPID(id) {
			return fmt.Errorf("accounts contains invalid account id %q", id)
		}
		if err := validateCapabilityObjects("accountCapabilities", account.AccountCapabilities); err != nil {
			return fmt.Errorf("account %q: %w", id, err)
		}
	}
	for capability, id := range *wire.PrimaryAccounts {
		account, ok := (*wire.Accounts)[id]
		if capability == "" || !validJMAPID(id) || !ok {
			return fmt.Errorf("primaryAccounts contains invalid account %q for capability %q", id, capability)
		}
		if _, ok := account.AccountCapabilities[capability]; !ok {
			return fmt.Errorf("primary account %q does not advertise capability %q", id, capability)
		}
	}
	*s = session{
		Capabilities:    *wire.Capabilities,
		Accounts:        *wire.Accounts,
		PrimaryAccounts: *wire.PrimaryAccounts,
		Username:        wire.Username,
		APIURL:          *wire.APIURL,
		DownloadURL:     *wire.DownloadURL,
		UploadURL:       *wire.UploadURL,
		EventSourceURL:  *wire.EventSourceURL,
		State:           *wire.State,
	}
	return nil
}

func (a *sessionAccount) UnmarshalJSON(data []byte) error {
	var wire struct {
		Name                *string                     `json:"name"`
		IsPersonal          *bool                       `json:"isPersonal"`
		IsReadOnly          *bool                       `json:"isReadOnly"`
		AccountCapabilities *map[string]json.RawMessage `json:"accountCapabilities"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Name == nil || wire.IsPersonal == nil || wire.IsReadOnly == nil || wire.AccountCapabilities == nil {
		return errors.New("missing required name, isPersonal, isReadOnly, or accountCapabilities")
	}
	*a = sessionAccount{
		Name:                *wire.Name,
		IsPersonal:          *wire.IsPersonal,
		IsReadOnly:          *wire.IsReadOnly,
		AccountCapabilities: *wire.AccountCapabilities,
	}
	return nil
}

func validateCapabilityObjects(name string, capabilities map[string]json.RawMessage) error {
	for capability, raw := range capabilities {
		var object map[string]json.RawMessage
		if capability == "" || json.Unmarshal(raw, &object) != nil || object == nil {
			return fmt.Errorf("%s contains an invalid object for capability %q", name, capability)
		}
	}
	return nil
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
	httpClient   *http.Client
	sessionURL   string
	credential   credential
	session      session
	accountID    string
	accountScope string
	limits       coreLimits
	apiSem       *requestLimiter
	uploadSem    *requestLimiter
	sessionStale atomic.Bool
}

type requestLimiter struct {
	mu       sync.Mutex
	limit    int
	inFlight int
	changed  chan struct{}
}

type accountLimiters struct {
	api    requestLimiter
	upload requestLimiter
}

var jmapAccountLimiters sync.Map

func sharedAccountLimiters(apiURL, accountID string) *accountLimiters {
	key := apiURL + "\x00" + accountID
	limiters, _ := jmapAccountLimiters.LoadOrStore(key, &accountLimiters{})
	return limiters.(*accountLimiters)
}

func (l *requestLimiter) configure(limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.changed == nil {
		l.changed = make(chan struct{})
	}
	// Multiple clients discover the same account independently. Keep the most
	// restrictive advertised value so their aggregate can never exceed either
	// session's account limit if discovery responses change mid-process.
	if l.limit == 0 || limit < l.limit {
		l.limit = limit
		close(l.changed)
		l.changed = make(chan struct{})
	}
}

func (l *requestLimiter) acquire(ctx context.Context) (func(), error) {
	for {
		l.mu.Lock()
		if l.inFlight < l.limit {
			l.inFlight++
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				l.inFlight--
				close(l.changed)
				l.changed = make(chan struct{})
				l.mu.Unlock()
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

type methodEnvelope struct {
	Using       []string        `json:"using"`
	MethodCalls [][]interface{} `json:"methodCalls"`
}

type methodResponseEnvelope struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
	SessionState    *string           `json:"sessionState"`
}

type methodResponseTarget struct {
	name string
	out  interface{}
	seen bool
}

var errJMAPLocalPermanent = errors.New("permanent local JMAP failure")

type protocolError struct {
	err              error
	primaryAmbiguous bool
}

func (e *protocolError) Error() string { return e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

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
		accountID = ""
		if len(candidates) == 1 {
			accountID = candidates[0]
		} else if len(candidates) > 1 {
			return errors.New("JMAP session has multiple writable mail accounts and no valid primary account")
		}
	}
	if accountID == "" {
		return errors.New("JMAP session has no writable mail account")
	}
	for name, endpoint := range map[string]string{
		"apiUrl": c.session.APIURL, "downloadUrl": c.session.DownloadURL,
		"uploadUrl": c.session.UploadURL, "eventSourceUrl": c.session.EventSourceURL,
	} {
		if strings.TrimSpace(endpoint) == "" {
			return fmt.Errorf("JMAP session has no %s", name)
		}
	}
	for name, template := range map[string]struct {
		value     string
		variables []string
	}{
		"downloadUrl":    {c.session.DownloadURL, []string{"accountId", "blobId", "type", "name"}},
		"uploadUrl":      {c.session.UploadURL, []string{"accountId"}},
		"eventSourceUrl": {c.session.EventSourceURL, []string{"types", "closeafter", "ping"}},
	} {
		if err := requireTemplateVariables(template.value, template.variables...); err != nil {
			return fmt.Errorf("invalid JMAP %s: %w", name, err)
		}
	}
	principal := *c.session.Username
	if principal == "" {
		principal = c.credential.username
		if principal == "" {
			return errors.New("JMAP session username is empty and no configured principal is available")
		}
	}
	c.accountID = accountID
	c.accountScope = providerAccountScope(c.sessionURL, principal, accountID)
	c.session.APIURL = resolveURL(c.sessionURL, c.session.APIURL)
	c.session.DownloadURL = resolveURL(c.sessionURL, c.session.DownloadURL)
	c.session.UploadURL = resolveURL(c.sessionURL, c.session.UploadURL)
	c.session.EventSourceURL = resolveURL(c.sessionURL, c.session.EventSourceURL)
	for name, endpoint := range map[string]string{
		"apiUrl": c.session.APIURL, "downloadUrl": c.session.DownloadURL,
		"uploadUrl": c.session.UploadURL, "eventSourceUrl": c.session.EventSourceURL,
	} {
		if err := validateJMAPURL(endpoint); err != nil {
			return fmt.Errorf("invalid JMAP %s: %w", name, err)
		}
	}
	limiters := sharedAccountLimiters(c.session.APIURL, c.accountID)
	limiters.api.configure(min(c.limits.MaxConcurrentRequests, maxClientAPIRequests))
	c.apiSem = &limiters.api
	if c.limits.MaxConcurrentUpload > 0 {
		limiters.upload.configure(min(c.limits.MaxConcurrentUpload, maxClientUploads))
		c.uploadSem = &limiters.upload
	}
	c.sessionStale.Store(false)
	return nil
}

func requireTemplateVariables(template string, variables ...string) error {
	for _, variable := range variables {
		if !strings.Contains(template, "{"+variable+"}") {
			return fmt.Errorf("URI template must contain {%s}", variable)
		}
	}
	return nil
}

// providerAccountScope binds provider object IDs and sync cursors to the
// authenticated JMAP account that issued them. Email.id and state values are
// only unique within one account, while a Durian account alias is mutable.
func providerAccountScope(sessionURL, username, accountID string) string {
	service := strings.TrimSpace(sessionURL)
	if parsed, err := url.Parse(sessionURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Fragment = ""
		service = parsed.String()
	}
	sum := sha256.Sum256([]byte("jmap\x00" + service + "\x00" + username + "\x00" + accountID))
	return hex.EncodeToString(sum[:])
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
		return fmt.Errorf("%w: JMAP core capability does not permit %s operations", errJMAPLocalPermanent, method)
	}
	body, err := json.Marshal(methodEnvelope{
		Using:       using,
		MethodCalls: [][]interface{}{{method, args, "0"}},
	})
	if err != nil {
		return fmt.Errorf("%w: encode %s request: %v", errJMAPLocalPermanent, method, err)
	}
	requestLimit := int64(maxMethodBytes)
	if c.limits.MaxSizeRequest > 0 && c.limits.MaxSizeRequest < requestLimit {
		requestLimit = c.limits.MaxSizeRequest
	}
	if int64(len(body)) > requestLimit {
		return fmt.Errorf("%w: %s request is %d bytes, exceeds JMAP maxSizeRequest %d", errJMAPLocalPermanent, method, len(body), requestLimit)
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
	if envelope.SessionState == nil {
		return &protocolError{err: fmt.Errorf("%s response omitted required sessionState", method), primaryAmbiguous: true}
	}
	if *envelope.SessionState != c.session.State {
		c.sessionStale.Store(true)
		return &protocolError{
			err:              fmt.Errorf("JMAP Session changed during %s; rediscovery is required", method),
			primaryAmbiguous: true,
		}
	}
	if len(envelope.MethodResponses) == 0 {
		return fmt.Errorf("%s returned no method responses", method)
	}
	var responseErr *methodError
	var protocolErr error
	primaryAmbiguous := false
	recordProtocolError := func(err error, primary bool) {
		if protocolErr == nil {
			protocolErr = err
		}
		primaryAmbiguous = primaryAmbiguous || primary
	}
	found := false
	for _, raw := range envelope.MethodResponses {
		var tuple []json.RawMessage
		if err := json.Unmarshal(raw, &tuple); err != nil {
			recordProtocolError(fmt.Errorf("decode %s method response: %w", method, err), false)
			continue
		}
		if len(tuple) != 3 {
			recordProtocolError(fmt.Errorf("decode %s method response: tuple has %d elements, want 3", method, len(tuple)), false)
			continue
		}
		var callID string
		if err := json.Unmarshal(tuple[2], &callID); err != nil {
			recordProtocolError(fmt.Errorf("decode %s response call ID: %w", method, err), false)
			continue
		}
		if callID != "0" {
			recordProtocolError(fmt.Errorf("JMAP %s response has call ID %q, want %q", method, callID, "0"), false)
			continue
		}
		var responseName string
		if err := json.Unmarshal(tuple[0], &responseName); err != nil {
			recordProtocolError(fmt.Errorf("decode %s response name: %w", method, err), false)
			continue
		}
		if responseName == "error" {
			var methodErr methodError
			if err := decodeMethodPayload(tuple[1], &methodErr); err != nil {
				recordProtocolError(fmt.Errorf("decode %s method error: %w", method, err), true)
				continue
			}
			if methodErr.Type == "" {
				recordProtocolError(fmt.Errorf("decode %s method error: missing type", method), true)
				continue
			}
			additionalSeen := false
			for _, target := range additional {
				additionalSeen = additionalSeen || target.seen
			}
			if responseErr != nil || found || additionalSeen {
				recordProtocolError(fmt.Errorf("JMAP response included contradictory primary results for %s", method), true)
			}
			responseErr = &methodErr
			continue
		}
		if responseName == method {
			if found || responseErr != nil {
				recordProtocolError(fmt.Errorf("JMAP response included contradictory primary results for %s", method), true)
				continue
			}
			if err := decodeMethodPayload(tuple[1], out); err != nil {
				recordProtocolError(fmt.Errorf("decode %s result: %w", method, err), true)
				continue
			}
			found = true
			continue
		}
		matchedAdditional := false
		for _, target := range additional {
			if responseName != target.name {
				continue
			}
			matchedAdditional = true
			if responseErr != nil {
				recordProtocolError(fmt.Errorf("JMAP response included an implicit %s result with a primary error", responseName), true)
			}
			if target.seen {
				recordProtocolError(fmt.Errorf("JMAP response included duplicate %s results", responseName), false)
				break
			}
			if err := decodeMethodPayload(tuple[1], target.out); err != nil {
				recordProtocolError(fmt.Errorf("decode %s result: %w", responseName, err), false)
				break
			}
			target.seen = true
			break
		}
		if !matchedAdditional {
			recordProtocolError(fmt.Errorf("JMAP response included unexpected %s result", responseName), false)
		}
	}
	if protocolErr != nil {
		return &protocolError{err: protocolErr, primaryAmbiguous: primaryAmbiguous}
	}
	if responseErr != nil {
		return responseErr
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
			if retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				delay = retryAfter
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

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds >= uint64(maxEventRetry/time.Second) {
			return maxEventRetry, true
		}
		return max(time.Duration(seconds)*time.Second, minEventRetry), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(time.Until(when), minEventRetry), maxEventRetry), true
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
		return "", fmt.Errorf("%w: JMAP session has no uploadUrl", errJMAPLocalPermanent)
	}
	if c.apiSem != nil && (c.limits.MaxSizeUpload == 0 || c.limits.MaxConcurrentUpload == 0) {
		return "", fmt.Errorf("%w: JMAP core capability does not permit uploads", errJMAPLocalPermanent)
	}
	if c.limits.MaxSizeUpload > 0 && int64(len(data)) > c.limits.MaxSizeUpload {
		return "", fmt.Errorf("%w: JMAP upload is %d bytes, exceeds maxSizeUpload %d", errJMAPLocalPermanent, len(data), c.limits.MaxSizeUpload)
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
		AccountID *string `json:"accountId"`
		BlobID    *string `json:"blobId"`
		Type      *string `json:"type"`
		Size      *int64  `json:"size"`
	}
	if err := decodeJSONLimited(resp.Body, maxSessionBytes, &result); err != nil {
		return "", fmt.Errorf("decode JMAP upload response: %w", err)
	}
	if result.AccountID == nil || *result.AccountID != c.accountID {
		return "", errors.New("JMAP upload omitted required matching accountId")
	}
	if result.BlobID == nil || !validJMAPID(*result.BlobID) {
		return "", errors.New("JMAP upload omitted required valid blobId")
	}
	wantType, wantParams, wantErr := mime.ParseMediaType(contentType)
	gotType, gotParams, gotErr := "", map[string]string(nil), error(nil)
	if result.Type != nil {
		gotType, gotParams, gotErr = mime.ParseMediaType(*result.Type)
	}
	paramsMatch := len(gotParams) == len(wantParams)
	for name, want := range wantParams {
		got, ok := gotParams[name]
		if !ok || (name == "charset" && !strings.EqualFold(got, want)) || (name != "charset" && got != want) {
			paramsMatch = false
			break
		}
	}
	if result.Type == nil || wantErr != nil || gotErr != nil ||
		!strings.EqualFold(gotType, wantType) || !paramsMatch {
		return "", errors.New("JMAP upload omitted required matching type")
	}
	if result.Size == nil || *result.Size != int64(len(data)) {
		return "", errors.New("JMAP upload omitted required matching size")
	}
	return *result.BlobID, nil
}

func acquire(ctx context.Context, sem *requestLimiter) (func(), error) {
	if sem == nil {
		return func() {}, nil
	}
	return sem.acquire(ctx)
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
	template := c.session.EventSourceURL
	u, err := url.Parse(expandTemplate(template, map[string]string{
		"types": "Email", "closeafter": "no", "ping": "30",
	}))
	if err != nil {
		return fmt.Errorf("parse JMAP eventSourceUrl: %w", err)
	}
	q := u.Query()
	if !strings.Contains(template, "{types}") {
		q.Set("types", "Email")
	}
	if !strings.Contains(template, "{closeafter}") {
		q.Set("closeafter", "no")
	}
	if !strings.Contains(template, "{ping}") {
		q.Set("ping", "30")
	}
	u.RawQuery = q.Encode()

	eventClient, err := newEventHTTPClient(c.httpClient, eventHeaderTimeout)
	if err != nil {
		return err
	}
	var lastEventID string
	var serverRetry time.Duration
	var serverRetrySet bool
	retryAttempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		requestCtx, cancelRequest := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			cancelRequest()
			return err
		}
		c.authorize(req)
		req.Header.Set("Accept", "text/event-stream")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		resp, err := eventClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusNoContent {
			_ = resp.Body.Close()
			cancelRequest()
			return nil
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			if mediaErr := validateEventStreamContentType(resp.Header.Get("Content-Type")); mediaErr != nil {
				err = mediaErr
			} else {
				var retry time.Duration
				var retrySet bool
				lastEventID, retry, retrySet, err = c.consumeEventsWithIdleTimeout(ctx, cancelRequest, resp.Body, lastEventID, onChange, eventIdleTimeout)
				if retrySet {
					serverRetry = retry
					serverRetrySet = true
				}
			}
			_ = resp.Body.Close()
		} else if resp != nil {
			responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				cancelRequest()
				return &statusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(responseBody))}
			}
		}
		cancelRequest()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if resp != nil && resp.StatusCode != http.StatusOK && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return fmt.Errorf("JMAP EventSource returned unsupported status %d", resp.StatusCode)
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
		if serverRetrySet {
			delay = serverRetry
		}
		if resp != nil {
			if retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				delay = retryAfter
			}
		}
		slog.Warn("JMAP EventSource disconnected, reconnecting", "module", "JMAPBACKEND",
			"retry", retryAttempt, "delay", delay)
		if err := sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func newEventHTTPClient(base *http.Client, headerTimeout time.Duration) (*http.Client, error) {
	if base == nil {
		return nil, errors.New("JMAP EventSource requires an HTTP client")
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	baseTransport, ok := transport.(*http.Transport)
	if !ok {
		return nil, errors.New("JMAP EventSource HTTP transport does not support a bounded response-header timeout")
	}
	boundedTransport := baseTransport.Clone()
	if boundedTransport.ResponseHeaderTimeout == 0 || boundedTransport.ResponseHeaderTimeout > headerTimeout {
		boundedTransport.ResponseHeaderTimeout = headerTimeout
	}
	eventClient := *base
	eventClient.Transport = boundedTransport
	eventClient.Timeout = 0
	return &eventClient, nil
}

func validateEventStreamContentType(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return fmt.Errorf("JMAP EventSource returned invalid Content-Type %q", contentType)
	}
	return nil
}

var errEventStreamIdle = errors.New("JMAP EventSource exceeded its read-idle timeout")

type eventConsumeResult struct {
	lastID   string
	retry    time.Duration
	retrySet bool
	err      error
}

type activityReader struct {
	reader   io.Reader
	activity chan<- struct{}
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (c *client) consumeEventsWithIdleTimeout(
	ctx context.Context,
	cancelRequest context.CancelFunc,
	body io.ReadCloser,
	lastID string,
	onChange func(),
	idleTimeout time.Duration,
) (string, time.Duration, bool, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activity := make(chan struct{}, 1)
	result := make(chan eventConsumeResult, 1)
	go func() {
		id, retry, retrySet, err := c.consumeEvents(streamCtx, activityReader{reader: body, activity: activity}, lastID, onChange)
		result <- eventConsumeResult{lastID: id, retry: retry, retrySet: retrySet, err: err}
	}()

	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case consumed := <-result:
			return consumed.lastID, consumed.retry, consumed.retrySet, consumed.err
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleTimeout)
		case <-timer.C:
			cancel()
			cancelRequest()
			_ = body.Close()
			select {
			case consumed := <-result:
				return consumed.lastID, consumed.retry, consumed.retrySet, errEventStreamIdle
			default:
				return lastID, 0, false, errEventStreamIdle
			}
		case <-ctx.Done():
			cancel()
			cancelRequest()
			_ = body.Close()
			select {
			case consumed := <-result:
				return consumed.lastID, consumed.retry, consumed.retrySet, ctx.Err()
			default:
				return lastID, 0, false, ctx.Err()
			}
		}
	}
}

func (c *client) consumeEvents(ctx context.Context, r io.Reader, lastID string, onChange func()) (string, time.Duration, bool, error) {
	scanner := bufio.NewScanner(r)
	scanner.Split(splitEventStreamLines)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var data strings.Builder
	pendingID := lastID
	idSeen := false
	var retry time.Duration
	retrySet := false
	firstLine := true
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return lastID, retry, retrySet, err
		}
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			payload := strings.TrimSuffix(data.String(), "\n")
			if stateChangeIncludesEmail(payload, c.accountID) {
				onChange()
			}
			if idSeen {
				lastID = pendingID
				idSeen = false
			}
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				pendingID = value
				idSeen = true
			}
		case "data":
			if data.Len()+len(value)+1 > maxEventBytes {
				return lastID, retry, retrySet, fmt.Errorf("JMAP EventSource event exceeds %d bytes", maxEventBytes)
			}
			data.WriteString(value)
			data.WriteByte('\n')
		case "retry":
			if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
				milliseconds, err := strconv.ParseUint(value, 10, 64)
				if err == nil && milliseconds <= uint64((1<<63-1)/int64(time.Millisecond)) {
					retry = min(max(time.Duration(milliseconds)*time.Millisecond, minEventRetry), maxEventRetry)
					retrySet = true
				}
			}
		}
	}
	return lastID, retry, retrySet, scanner.Err()
}

func splitEventStreamLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, char := range data {
		switch char {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
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
