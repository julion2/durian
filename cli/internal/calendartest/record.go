package calendartest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Exchange intentionally has no headers, URL host, OAuth or raw error fields.
// Only allowlisted Calendar JSON is retained. Unknown fields are discarded.
type Exchange struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Query    url.Values      `json:"query"`
	IfMatch  string          `json:"if_match,omitempty"`
	Request  json.RawMessage `json:"request,omitempty"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response,omitempty"`
}

type Recording struct {
	Prefix    string     `json:"prefix"`
	Exchanges []Exchange `json:"exchanges"`
}

var address = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9.-]+`)

func anonymize(s string) string {
	return address.ReplaceAllStringFunc(s, func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return fmt.Sprintf("redacted-%x", sum[:8])
	})
}

func cleanJSON(body []byte) (json.RawMessage, error) {
	return cleanCalendarJSON(body, "")
}

func cleanCalendarJSON(body []byte, calendarID string) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, nil
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return nil, fmt.Errorf("recorder refuses non-JSON response")
	}
	var clean func(any) any
	clean = func(v any) any {
		switch v := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for key, field := range v {
				switch key {
				case "id", "iCalUID", "etag", "status", "summary", "description", "location", "start", "end", "dateTime", "date", "timeZone", "recurrence", "recurringEventId", "originalStartTime", "items", "kind", "sequence", "error", "code", "errors", "reason", "domain":
					if key == "recurrence" {
						if rec, ok := field.(map[string]any); ok {
							out[key] = cleanGraphRecurrence(rec)
							continue
						}
					}
					if key == "location" {
						if loc, ok := field.(map[string]any); ok {
							out[key] = cleanFields(loc, "displayName")
							continue
						}
					}
					out[key] = clean(field)
				case "value", "subject", "iCalUId", "@odata.etag", "changeKey", "transactionId", "type", "seriesMasterId", "isAllDay", "isCancelled", "originalStart", "originalStartTimeZone", "originalEndTimeZone", "cancelledOccurrences", "occurrenceId", "exceptionOccurrences":
					// Graph temporal/recurrence identity only, never organizer,
					// attendee, body HTML, token or emailAddress objects.
					out[key] = clean(field)
				case "extendedProperties":
					if props, ok := field.(map[string]any); ok {
						if private, ok := props["private"].(map[string]any); ok {
							out[key] = map[string]any{"private": cleanFields(private, foldStartZone, foldEndZone)}
						}
					}
				case "@odata.nextLink", "exceptionOccurrences@odata.nextLink":
					if link, ok := field.(string); ok {
						if safe := cleanGraphNextLink(link, calendarID); safe != "" {
							out[key] = safe
						}
					}
				case "nextPageToken", "nextSyncToken":
					// Cursors are opaque, non-reusable placeholders in replays.
					out[key] = cursor(fmt.Sprint(field))
				}
			}
			return out
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
			return v
		case string:
			return anonymize(v)
		default:
			return v
		}
	}
	return json.Marshal(clean(value))
}

// Context-specific scalar allowlists keep Graph recurrence/location and Google
// private metadata from turning into a general identity-bearing JSON escape.
func cleanFields(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if value, ok := m[key]; ok {
			switch value := value.(type) {
			case string:
				out[key] = anonymize(value)
			case nil, bool, float64:
				out[key] = value
			case []any:
				var values []string
				for _, v := range value {
					if s, ok := v.(string); ok {
						values = append(values, anonymize(s))
					}
				}
				out[key] = values
			}
		}
	}
	return out
}

func cleanGraphRecurrence(rec map[string]any) map[string]any {
	out := map[string]any{}
	if pattern, ok := rec["pattern"].(map[string]any); ok {
		out["pattern"] = cleanFields(pattern, "type", "interval", "month", "dayOfMonth", "daysOfWeek", "firstDayOfWeek", "index")
	}
	if span, ok := rec["range"].(map[string]any); ok {
		out["range"] = cleanFields(span, "type", "startDate", "endDate", "recurrenceTimeZone", "numberOfOccurrences")
	}
	return out
}

var graphRecordingPath = regexp.MustCompile(`^/v1\.0/me/(calendars/[A-Za-z0-9_+=-]+/(events|calendarView)|events/[A-Za-z0-9_+=-]+/(instances|exceptionOccurrences))$`)
var graphRecordingFields = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(,[A-Za-z][A-Za-z0-9]*)*$`)
var recordingNumber = regexp.MustCompile(`^[0-9]+$`)

func cleanGraphQuery(q url.Values) (url.Values, bool) {
	out := url.Values{}
	for key, values := range q {
		if len(values) != 1 {
			return nil, false
		}
		v := values[0]
		switch key {
		case "$skiptoken":
			v = cursor(v)
		case "$top", "$skip":
			if !recordingNumber.MatchString(v) {
				return nil, false
			}
		case "$select":
			if !graphRecordingFields.MatchString(v) {
				return nil, false
			}
		case "$expand":
			if v != "exceptionOccurrences" {
				return nil, false
			}
		case "startDateTime", "endDateTime":
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				return nil, false
			}
		default:
			return nil, false
		}
		out[key] = []string{v}
	}
	return out, true
}

func cleanGraphNextLink(raw, calendarID string) string {
	u, err := url.Parse(raw)
	if err != nil || !graphTrusted(u) || !graphRecordingPath.MatchString(u.Path) || u.RawPath != "" {
		return ""
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ""
	}
	q, ok := cleanGraphQuery(q)
	if !ok {
		return ""
	}
	// Calendar identifiers may themselves contain mailbox identity. Only the
	// explicitly pinned calendar can appear in a retained calendar-scoped link.
	if strings.HasPrefix(u.Path, "/v1.0/me/calendars/") {
		if calendarID == "" || !strings.HasPrefix(u.Path, "/v1.0/me/calendars/"+calendarID+"/") {
			return ""
		}
		u.Path = strings.Replace(u.Path, "/calendars/"+calendarID+"/", "/calendars/test-calendar/", 1)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func cursor(s string) string {
	if strings.HasPrefix(s, "redacted-cursor-") {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("redacted-cursor-%x", sum[:8])
}

func requestRecord(req *http.Request, calendarID string) (Exchange, error) {
	e := Exchange{Method: req.Method, Path: strings.ReplaceAll(req.URL.Path, calendarID, "test-calendar"), Query: req.URL.Query(), IfMatch: req.Header.Get("If-Match")}
	if req.URL.Host == "graph.microsoft.com" {
		var ok bool
		e.Query, ok = cleanGraphQuery(e.Query)
		if !ok {
			return e, fmt.Errorf("recorder refuses unsafe Graph query")
		}
	}
	if v := e.Query.Get("pageToken"); v != "" {
		e.Query.Set("pageToken", cursor(v))
	}
	if req.Body != nil {
		body, err := io.ReadAll(io.LimitReader(req.Body, (1<<20)+1))
		if err != nil || len(body) > 1<<20 {
			return e, fmt.Errorf("recording request exceeds limit")
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
		e.Request, err = cleanCalendarJSON(body, calendarID)
		if err != nil {
			return e, err
		}
	}
	return e, nil
}

type Recorder struct {
	mu         sync.Mutex
	CalendarID string
	Next       http.RoundTripper
	Source     *TokenSource
	Recording  Recording
}

func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := requestRecord(req, r.CalendarID)
	if err != nil {
		return nil, err
	}
	resp, err := r.Next.RoundTrip(req)
	if err != nil {
		r.Recording.Exchanges = append(r.Recording.Exchanges, e)
		return nil, fmt.Errorf("calendar test request failed (details withheld)")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	resp.Body.Close()
	if err != nil || len(body) > 8<<20 {
		return nil, fmt.Errorf("recording response exceeds limit")
	}
	e.Status = resp.StatusCode
	e.Response, err = cleanCalendarJSON(body, r.CalendarID)
	if err != nil {
		return nil, err
	}
	r.Recording.Exchanges = append(r.Recording.Exchanges, e)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (r *Recorder) Save(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.MarshalIndent(r.Recording, "", "  ")
	if err != nil {
		return err
	}
	if address.Match(data) {
		return fmt.Errorf("refusing recording with an email address")
	}
	if r.Source != nil && r.Source.containsSecret(data) {
		return fmt.Errorf("refusing recording containing credentials")
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

// Replay is network-free and consumes the same public-provider scenario,
// including writes and cleanup. Any changed request fails the replay.
type Replay struct {
	Recording Recording
	Index     int
}

func (r *Replay) RoundTrip(req *http.Request) (*http.Response, error) {
	got, err := requestRecord(req, "test-calendar")
	if err != nil {
		return nil, err
	}
	if r.Index >= len(r.Recording.Exchanges) {
		return nil, fmt.Errorf("replay exhausted")
	}
	want := r.Recording.Exchanges[r.Index]
	status, body := want.Status, want.Response
	want.Status, want.Response = 0, nil
	want.Request, err = cleanCalendarJSON(want.Request, "test-calendar")
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(got, want) {
		return nil, fmt.Errorf("replay request mismatch at exchange %d", r.Index)
	}
	r.Index++
	if status == 0 {
		return nil, fmt.Errorf("recorded transport failure")
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}
