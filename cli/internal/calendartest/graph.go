package calendartest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const GraphBase = "https://graph.microsoft.com/v1.0"
const graphPrefer = `outlook.timezone="UTC", IdType="ImmutableId"`

// Outlook personal accounts reject isSharedWithMe in v1.0 $select (HTTP 400).
// Ownership is established by canShare (creator-only) AND the demo identity,
// not by treating an absent shared flag as proof of ownership.
const graphCalendarSelect = "id,name,isDefaultCalendar,canEdit,canShare,owner"
const graphSafetySelect = "id,subject,transactionId,type,seriesMasterId,attendees,isOrganizer,isOnlineMeeting,onlineMeeting,onlineMeetingUrl,isReminderOn,changeKey,start,end"
const graphLimit = 2 << 20

var graphID = regexp.MustCompile(`^[A-Za-z0-9_+=-]{1,1000}$`)
var graphPrefix = regexp.MustCompile(`^Durian Test [0-9a-f]{32} $`)

// GraphCalendarPin is an in-memory capability, bound to the bootstrap token
// source. It cannot be constructed from a display name by callers.
type GraphCalendarPin struct {
	id       string
	source   *TokenSource
	verified bool
	owner    string
}

func (p *GraphCalendarPin) ID() string { return p.id }

type graphMetadata struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Default  *bool  `json:"isDefaultCalendar"`
	Shared   *bool  `json:"isSharedWithMe"`
	CanEdit  *bool  `json:"canEdit"`
	CanShare *bool  `json:"canShare"`
	Owner    struct {
		Address string `json:"address"`
	} `json:"owner"`
}

func (c graphMetadata) isolated() bool {
	return graphID.MatchString(c.ID) && c.Name == "Durian Test" &&
		c.Default != nil && !*c.Default && (c.Shared == nil || !*c.Shared) &&
		c.CanEdit != nil && *c.CanEdit && c.CanShare != nil && *c.CanShare && c.Owner.Address != ""
}

// EnsureGraphCalendar lists metadata only and creates exactly "Durian Test"
// when absent. No calendar is ever deleted. A successful POST plus matching
// fresh ownership metadata proves new-calendar provenance without another
// human attestation. Reuse requires a unique exact metadata match owned by
// the consent-verified DURIAN_MS_TEST_EMAIL demo identity. verifiedID, when
// supplied, is an additional ID constraint, not a human-attestation gate.
// Missing identity on reuse fails closed; first creation does not require
// EMAIL because provenance comes from this token's own /me/calendars POST.
// No credentials or owner addresses are logged here.
//
// Graph has no Google-style description marker. canShare identifies the
// creator, not whether the calendar has been shared OUT; the latter needs the
// dedicated demo account's exclusive test-calendar convention. These metadata
// checks cannot prove absence of outgoing shares/publication. Keep the demo
// mailbox quiescent (no other clients changing sharing, moving events or adding
// attendees) throughout the test. This guard sends no invitations or reminders.
// Docs: https://learn.microsoft.com/en-us/graph/api/resources/calendar?view=graph-rest-1.0
// https://learn.microsoft.com/en-us/graph/api/user-post-calendars?view=graph-rest-1.0
func EnsureGraphCalendar(ctx context.Context, source *TokenSource, verifiedID string) (*GraphCalendarPin, error) {
	if source == nil || source.provider != Microsoft || (verifiedID != "" && !graphID.MatchString(verifiedID)) {
		return nil, fmt.Errorf("invalid Graph bootstrap scope")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var found []graphMetadata
	next := GraphBase + "/me/calendars?$select=" + graphCalendarSelect
	seen := map[string]bool{}
	for next != "" {
		if len(seen) >= 100 || seen[next] || !graphPageURL(next, "/v1.0/me/calendars") {
			return nil, fmt.Errorf("unsafe Graph calendar metadata paging")
		}
		seen[next] = true
		var page struct {
			Value []graphMetadata `json:"value"`
			Next  string          `json:"@odata.nextLink"`
		}
		if err := graphJSON(ctx, source, http.DefaultTransport, next, &page); err != nil {
			return nil, err
		}
		if page.Value == nil {
			return nil, fmt.Errorf("invalid Graph calendar metadata list; refusing writes")
		}
		for _, c := range page.Value {
			if strings.EqualFold(c.Name, "Durian Test") {
				found = append(found, c)
			}
		}
		next = page.Next
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("ambiguous Durian Test calendars; refusing writes")
	}
	if len(found) == 1 {
		c := found[0]
		if !c.isolated() || source.email == "" || !strings.EqualFold(c.Owner.Address, source.email) || (verifiedID != "" && c.ID != verifiedID) {
			return nil, fmt.Errorf("existing Graph calendar lacks unique demo identity and API ownership verification")
		}
		// Do not rely only on a possibly stale calendar-list entry.
		var current graphMetadata
		if err := graphJSON(ctx, source, http.DefaultTransport, GraphBase+"/me/calendars/"+url.PathEscape(c.ID)+"?$select="+graphCalendarSelect, &current); err != nil || !current.isolated() || current.ID != c.ID || !strings.EqualFold(current.Owner.Address, source.email) {
			return nil, fmt.Errorf("existing Graph calendar ownership changed or unverifiable")
		}
		return &GraphCalendarPin{id: c.ID, source: source, verified: true, owner: current.Owner.Address}, nil
	}
	if verifiedID != "" {
		return nil, fmt.Errorf("verified Graph calendar missing; refusing replacement")
	}
	resp, err := graphExchange(ctx, source, http.DefaultTransport, http.MethodPost, GraphBase+"/me/calendars", nil, []byte(`{"name":"Durian Test"}`))
	if err != nil {
		return nil, fmt.Errorf("Graph calendar POST outcome unknown; inspect manually before retrying")
	}
	defer resp.Body.Close()
	var created graphMetadata
	if resp.StatusCode != http.StatusCreated || json.NewDecoder(resp.Body).Decode(&created) != nil || !graphID.MatchString(created.ID) || created.Name != "Durian Test" {
		return nil, fmt.Errorf("Graph calendar POST unverified; retain and inspect manually")
	}
	// Creation responses may omit isDefaultCalendar; explicitly read metadata.
	var verified graphMetadata
	if err := graphJSON(ctx, source, http.DefaultTransport, GraphBase+"/me/calendars/"+url.PathEscape(created.ID)+"?$select="+graphCalendarSelect, &verified); err != nil || !verified.isolated() || verified.ID != created.ID || (source.email != "" && !strings.EqualFold(verified.Owner.Address, source.email)) {
		return nil, fmt.Errorf("created Graph calendar unverified; retain and inspect manually")
	}
	return &GraphCalendarPin{id: verified.ID, source: source, verified: true, owner: verified.Owner.Address}, nil
}

// GraphGuard protects the real provider's mailbox-global Get/Update/Delete
// seam, not just its calendar-scoped POST. Only successful creates in THIS run
// enroll masters; instances enroll only through an owned master's relationship.
// Every global access rechecks event safety and actual calendar membership
// (navigation ID, or exact immutable ID in the pinned calendar's collection).
// No move, RSVP, attachment, calendar delete or notification API is exposed.
//
// Membership is a separate GET, not an atomic Graph authorization boundary.
// Mutations retain an explicit caller If-Match; otherwise we supply the freshly
// read @odata.etag. Neither promises atomic membership+mutation or folder-move
// preconditions (and Graph may reject or ignore the caller's precondition).
// The quiescent-mailbox requirement on EnsureGraphCalendar is essential; this
// helper is NOT safe against another writer racing these checks.
// Docs: https://learn.microsoft.com/en-us/graph/api/resources/event?view=graph-rest-1.0
// https://learn.microsoft.com/en-us/graph/api/calendar-post-events?view=graph-rest-1.0
type GraphGuard struct {
	mu        sync.Mutex
	pin       *GraphCalendarPin
	prefix    string
	transport http.RoundTripper
	owned     map[string]string // id -> series master (empty for own masters)
	posts     map[string][32]byte
	unknown   map[string]bool
	readable  map[string]bool // observed in pinned collection; NEVER mutation ownership
}

// NewGraphGuard requires a fresh 128-bit random hex run nonce in prefix:
// "Durian Test <32 lowercase hex digits> ". Never reuse it across runs.
func NewGraphGuard(pin *GraphCalendarPin, prefix string) (*GraphGuard, error) {
	if pin == nil || pin.source == nil || !pin.verified || pin.owner == "" || !graphID.MatchString(pin.id) || !graphPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("invalid isolated Graph scope")
	}
	return &GraphGuard{pin: pin, prefix: prefix, transport: http.DefaultTransport,
		owned: map[string]string{}, posts: map[string][32]byte{}, unknown: map[string]bool{}, readable: map[string]bool{}}, nil
}

func (g *GraphGuard) HTTPClient() *http.Client {
	return &http.Client{Transport: g, Timeout: 45 * time.Second, CheckRedirect: noRedirect}
}

// GraphTransactionID deterministically binds production CreateOptions'
// IdempotencyKey to the full, run-prefixed subject. Reuse it only for retries
// with the identical body. A lost POST never authorizes list-based cleanup.
func GraphTransactionID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])
}

func (g *GraphGuard) CreatedIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.owned))
	for id := range g.owned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// UnknownTransactions reports POSTs that might have committed but have no
// verified ID. Retain them for manual inspection, or retry the exact same POST
// through this guard. Never sweep events by subject/prefix/transaction alone.
func (g *GraphGuard) UnknownTransactions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var ids []string
	for id := range g.unknown {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func graphTrusted(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Host == "graph.microsoft.com" && u.User == nil && u.Fragment == "" && u.Opaque == ""
}

func graphPageURL(raw, path string) bool {
	u, err := url.Parse(raw)
	if err != nil || !graphTrusted(u) || u.Path != path {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for k, v := range q {
		if len(v) != 1 {
			return false
		}
		switch k {
		case "$select", "$top", "$skiptoken", "$skip", "startDateTime", "endDateTime":
		case "$expand":
			id := strings.TrimPrefix(path, "/v1.0/me/events/")
			if v[0] != "exceptionOccurrences" || !strings.HasPrefix(path, "/v1.0/me/events/") || !graphID.MatchString(id) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// graphExchange buffers bounded responses, strips caller-controlled headers,
// rejects redirects even on the trusted host. Bounded HTTP error responses are
// returned intact for the provider/recorder to observe 400/412 behavior; never
// put their bodies in adapter errors or logs. Unit tests intercept all I/O.
func graphExchange(ctx context.Context, source *TokenSource, transport http.RoundTripper, method, raw string, headers http.Header, body []byte) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil || !graphTrusted(u) {
		return nil, fmt.Errorf("untrusted Graph endpoint")
	}
	token, err := source.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("Graph test token unavailable (details withheld)")
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid Graph request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", graphPrefer)
	if headers.Get("If-Match") != "" {
		req.Header.Set("If-Match", headers.Get("If-Match"))
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Graph test request failed (details withheld)")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, graphLimit+1))
	if err != nil || len(data) > graphLimit {
		return nil, fmt.Errorf("Graph test response rejected (HTTP %d)", resp.StatusCode)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

func graphJSON(ctx context.Context, source *TokenSource, transport http.RoundTripper, raw string, out any) error {
	resp, err := graphExchange(ctx, source, transport, http.MethodGet, raw, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(out) != nil {
		return fmt.Errorf("invalid Graph test JSON (HTTP %d)", resp.StatusCode)
	}
	return nil
}

type graphSafeEvent struct {
	ID            string          `json:"id"`
	Subject       string          `json:"subject"`
	TransactionID string          `json:"transactionId"`
	Type          string          `json:"type"`
	Master        string          `json:"seriesMasterId"`
	Attendees     json.RawMessage `json:"attendees"`
	Organizer     *bool           `json:"isOrganizer"`
	Online        *bool           `json:"isOnlineMeeting"`
	Meeting       json.RawMessage `json:"onlineMeeting"`
	MeetingURL    string          `json:"onlineMeetingUrl"`
	Reminder      *bool           `json:"isReminderOn"`
	ChangeKey     string          `json:"changeKey"`
	ETag          string          `json:"@odata.etag"`
	Start         struct {
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	} `json:"start"`
	End struct {
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	} `json:"end"`
}

func (g *GraphGuard) safe(e graphSafeEvent) bool {
	var attendees []json.RawMessage
	return graphID.MatchString(e.ID) && strings.HasPrefix(e.Subject, g.prefix) &&
		json.Unmarshal(e.Attendees, &attendees) == nil && string(e.Attendees) != "null" && len(attendees) == 0 && e.Organizer != nil && *e.Organizer &&
		e.Online != nil && !*e.Online && (len(e.Meeting) == 0 || string(e.Meeting) == "null") && e.MeetingURL == "" && e.Reminder != nil && !*e.Reminder
}

func (g *GraphGuard) membership(ctx context.Context, event graphSafeEvent) error {
	var c struct {
		ID string `json:"id"`
	}
	// calendar is the read-only navigation relationship containing the event.
	// Never follow a returned @odata.navigationLink or associationLink.
	err := graphJSON(ctx, g.pin.source, g.transport, GraphBase+"/me/events/"+url.PathEscape(event.ID)+"/calendar?$select=id", &c)
	if err != nil {
		return fmt.Errorf("Graph event calendar membership read failed: %w", err)
	}
	if c.ID == g.pin.id {
		return nil
	}
	// Real Outlook personal accounts can return the PRIMARY calendar from
	// event/calendar even for an event present in another calendar's collection.
	// Do NOT compare names or accept that wrong relationship. Require the exact
	// already-known immutable ID in the pinned calendar's own collection instead.
	// This is membership verification, never enrollment or listing-based cleanup.
	path := "/v1.0/me/calendars/" + g.pin.id + "/events"
	query := url.Values{"$select": {"id"}, "$top": {"100"}}
	if event.Type == "occurrence" || event.Type == "exception" {
		start, e1 := time.Parse("2006-01-02T15:04:05", event.Start.DateTime)
		end, e2 := time.Parse("2006-01-02T15:04:05", event.End.DateTime)
		if e1 != nil || e2 != nil || event.Start.TimeZone != "UTC" || event.End.TimeZone != "UTC" || end.Before(start) || end.Sub(start) > 31*24*time.Hour {
			return fmt.Errorf("Graph instance membership lacks bounded UTC endpoints")
		}
		path = "/v1.0/me/calendars/" + g.pin.id + "/calendarView"
		query.Set("startDateTime", start.Add(-time.Second).Format(time.RFC3339))
		query.Set("endDateTime", end.Add(time.Second).Format(time.RFC3339))
	}
	next := "https://graph.microsoft.com" + path + "?" + query.Encode()
	seen := map[string]bool{}
	for next != "" {
		if len(seen) >= 100 || seen[next] || !graphPageURL(next, path) {
			return fmt.Errorf("unsafe Graph membership paging")
		}
		seen[next] = true
		var page struct {
			Value []struct {
				ID string `json:"id"`
			} `json:"value"`
			Next string `json:"@odata.nextLink"`
		}
		if err := graphJSON(ctx, g.pin.source, g.transport, next, &page); err != nil {
			return err
		}
		for _, candidate := range page.Value {
			if candidate.ID == event.ID {
				return nil
			}
		}
		next = page.Next
	}
	return fmt.Errorf("Graph event no longer belongs to pinned calendar")
}

func (g *GraphGuard) checkEvent(ctx context.Context, id string) (graphSafeEvent, error) {
	var e graphSafeEvent
	if _, ok := g.owned[id]; !ok {
		return e, fmt.Errorf("Graph event not created in this run")
	}
	err := graphJSON(ctx, g.pin.source, g.transport, GraphBase+"/me/events/"+url.PathEscape(id)+"?$select="+graphSafetySelect, &e)
	if err != nil || e.ID != id || !g.safe(e) || e.Master != g.owned[id] {
		return e, fmt.Errorf("Graph event failed current safety check")
	}
	if err := g.membership(ctx, e); err != nil {
		return e, err
	}
	return e, nil
}

// graphWriteBody is a recursive allowlist. In particular location accepts
// displayName only (not locationEmailAddress), and recurrence cannot smuggle
// arbitrary recipient-bearing fields. Extend this deliberately, not by adding
// an attendees denylist: even attendees:[] is forbidden in this first layer.
func graphWriteBody(body []byte, prefix string, create bool) (string, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || len(m) == 0 {
		return "", fmt.Errorf("invalid Graph event body")
	}
	var subject, tx string
	for k, v := range m {
		valid := false
		switch k {
		case "subject":
			valid = json.Unmarshal(v, &subject) == nil && strings.HasPrefix(subject, prefix)
		case "transactionId":
			valid = create && json.Unmarshal(v, &tx) == nil
		case "body":
			valid = graphObject(v, map[string]string{"contentType": "string", "content": "string"})
			var b struct {
				ContentType string `json:"contentType"`
			}
			valid = valid && json.Unmarshal(v, &b) == nil && b.ContentType == "text"
		case "location":
			valid = graphObject(v, map[string]string{"displayName": "string"})
		case "start", "end":
			valid = graphObject(v, map[string]string{"dateTime": "string", "timeZone": "string"})
		case "isAllDay":
			var b bool
			valid = string(v) != "null" && json.Unmarshal(v, &b) == nil
		case "recurrence":
			var rec map[string]json.RawMessage
			valid = string(v) == "null"
			if !valid && json.Unmarshal(v, &rec) == nil && len(rec) == 2 {
				valid = graphObject(rec["pattern"], map[string]string{"type": "string", "interval": "number", "month": "number", "dayOfMonth": "number", "daysOfWeek": "strings", "firstDayOfWeek": "string", "index": "string"}) &&
					graphObject(rec["range"], map[string]string{"type": "string", "startDate": "string", "endDate": "string", "recurrenceTimeZone": "string", "numberOfOccurrences": "number"})
			}
		}
		if !valid {
			return "", fmt.Errorf("Graph event body blocked by no-recipient policy")
		}
	}
	if create && (subject == "" || tx != GraphTransactionID(subject)) {
		return "", fmt.Errorf("Graph create requires run-bound transactionId")
	}
	return tx, nil
}

func graphObject(raw json.RawMessage, allowed map[string]string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	for k, v := range m {
		if string(v) == "null" {
			return false
		}
		switch allowed[k] {
		case "string":
			var s string
			if json.Unmarshal(v, &s) != nil {
				return false
			}
		case "number":
			var n int
			if json.Unmarshal(v, &n) != nil {
				return false
			}
		case "strings":
			var s []string
			if json.Unmarshal(v, &s) != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (g *GraphGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	deny := func() (*http.Response, error) { return nil, fmt.Errorf("isolated Graph guard blocked request") }
	u := req.URL
	if !graphTrusted(u) || (req.Host != "" && req.Host != u.Host) || req.Header.Get("X-HTTP-Method-Override") != "" || req.Context().Err() != nil {
		return deny()
	}
	base := "/v1.0/me/calendars/" + g.pin.id
	collection := u.Path == base+"/events" || u.Path == base+"/calendarView"
	id, instances := "", false
	exceptionsPage := false
	if !collection {
		var ok bool
		id, ok = strings.CutPrefix(u.Path, "/v1.0/me/events/")
		if !ok {
			return deny()
		}
		id, exceptionsPage = strings.CutSuffix(id, "/exceptionOccurrences")
		if exceptionsPage && req.Method != http.MethodGet {
			return deny()
		}
		if !exceptionsPage {
			id, instances = strings.CutSuffix(id, "/instances")
		}
		if !graphID.MatchString(id) {
			return deny()
		}
		if _, ok := g.owned[id]; !ok && (req.Method != http.MethodGet || !g.readable[id]) {
			return deny()
		}
	}
	if !graphPageURL(u.String(), u.Path) {
		return deny()
	}
	query := u.Query()
	if req.Method != http.MethodGet && len(query) != 0 {
		return deny()
	}
	var body []byte
	tx := ""
	if req.Method == http.MethodPost || req.Method == http.MethodPatch {
		if req.Body == nil {
			return deny()
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, (1<<20)+1))
		req.Body.Close()
		if err != nil || len(body) > 1<<20 {
			return deny()
		}
		if req.Method == http.MethodPost && u.Path != base+"/events" {
			return deny()
		}
		if req.Method == http.MethodPatch && (id == "" || instances) {
			return deny()
		}
		tx, err = graphWriteBody(body, g.prefix, req.Method == http.MethodPost)
		if err != nil {
			return deny()
		}
		// Send exactly the decoded tree we checked, not duplicate-key JSON
		// whose interpretation might differ between Graph and encoding/json.
		var canonical map[string]any
		if json.Unmarshal(body, &canonical) != nil {
			return deny()
		}
		if req.Method == http.MethodPost {
			// Do not inherit mailbox reminder/automatic conferencing defaults.
			// Callers cannot set these fields; the guard supplies only false.
			canonical["isOnlineMeeting"] = false
			canonical["isReminderOn"] = false
		}
		body, err = json.Marshal(canonical)
		if err != nil {
			return deny()
		}
	} else if req.Body != nil || (req.Method != http.MethodGet && (req.Method != http.MethodDelete || id == "" || instances)) {
		return deny()
	}
	headers := http.Header{}
	if id != "" {
		var e graphSafeEvent
		var err error
		if _, owned := g.owned[id]; owned {
			e, err = g.checkEvent(req.Context(), id)
		} else {
			// Production enriches existing series with mailbox-global reads.
			// A prior pinned collection read plus FRESH membership permits only
			// GET; it never enrolls this ID (or its children) for mutation.
			err = graphJSON(req.Context(), g.pin.source, g.transport, GraphBase+"/me/events/"+url.PathEscape(id)+"?$select="+graphSafetySelect, &e)
			if err == nil && e.ID != id {
				err = fmt.Errorf("Graph read identity mismatch")
			}
			if err == nil {
				err = g.membership(req.Context(), e)
			}
		}
		if err != nil {
			return nil, err
		}
		if (instances || exceptionsPage) && (e.Type != "seriesMaster" || g.owned[id] != "") {
			return deny()
		}
		if query.Get("$expand") != "" && (req.Method != http.MethodGet || instances || e.Type != "seriesMaster" || g.owned[id] != "") {
			return deny()
		}
		if req.Method != http.MethodGet {
			match := req.Header.Get("If-Match")
			if match == "" {
				match = e.ETag
			}
			if match == "" {
				return deny()
			}
			// Preserve even stale changeKey-style caller tokens verbatim:
			// only the real provider can establish their HTTP semantics.
			headers.Set("If-Match", match)
		}
	}
	if req.Method != http.MethodGet {
		var c graphMetadata
		if err := graphJSON(req.Context(), g.pin.source, g.transport, GraphBase+"/me/calendars/"+url.PathEscape(g.pin.id)+"?$select="+graphCalendarSelect, &c); err != nil || c.ID != g.pin.id || !c.isolated() || !strings.EqualFold(c.Owner.Address, g.pin.owner) {
			return deny()
		}
	}
	if tx != "" {
		sum := sha256.Sum256(body)
		if prior, exists := g.posts[tx]; exists && prior != sum {
			return deny()
		}
		g.posts[tx], g.unknown[tx] = sum, true
	}
	if req.Method == http.MethodGet && (collection || instances) {
		// FetchInstances normally asks only for UTC, and omits seriesMasterId.
		// Normalize ALL pages to immutable IDs and select the binding fields.
		fields := map[string]bool{}
		var selected []string
		for _, field := range strings.Split(query.Get("$select")+","+graphSafetySelect, ",") {
			if field != "" && !fields[field] {
				fields[field] = true
				selected = append(selected, field)
			}
		}
		query.Set("$select", strings.Join(selected, ","))
	}
	copyURL := *u
	copyURL.RawQuery = query.Encode()
	resp, err := graphExchange(req.Context(), g.pin.source, g.transport, req.Method, copyURL.String(), headers, body)
	if err != nil {
		if tx != "" {
			return nil, fmt.Errorf("Graph POST outcome unknown; retained, no cleanup ID authorized")
		}
		return nil, err
	}
	data, _ := io.ReadAll(resp.Body) // already bounded by graphExchange
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A definite invalid-input rejection cannot have created an event.
		// Other failures remain unknown; never authorize cleanup from an error.
		if tx != "" && resp.StatusCode == http.StatusBadRequest {
			delete(g.unknown, tx)
		}
		return resp, nil
	}
	if tx != "" {
		var e graphSafeEvent
		if resp.StatusCode != http.StatusCreated || json.Unmarshal(data, &e) != nil || !g.safe(e) || e.Master != "" || (e.Type != "singleInstance" && e.Type != "seriesMaster") || e.TransactionID != tx || GraphTransactionID(e.Subject) != tx {
			resp.Body.Close()
			return nil, fmt.Errorf("Graph POST unverified; retained, no cleanup ID authorized")
		}
		if err := g.membership(req.Context(), e); err != nil {
			resp.Body.Close()
			return nil, err
		}
		g.owned[e.ID] = ""
		delete(g.unknown, tx)
	}
	if req.Method == http.MethodGet && (collection || instances) {
		var page struct {
			Value []graphSafeEvent `json:"value"`
			Next  string           `json:"@odata.nextLink"`
		}
		if json.Unmarshal(data, &page) != nil || (page.Next != "" && !graphPageURL(page.Next, u.Path)) {
			resp.Body.Close()
			return deny()
		}
		for _, e := range page.Value {
			if collection && graphID.MatchString(e.ID) {
				g.readable[e.ID] = true
			}
			if e.Master == "" || e.Master == e.ID || !g.safe(e) || (e.Type != "occurrence" && e.Type != "exception") {
				continue
			}
			if master, ok := g.owned[e.Master]; !ok || master != "" || (instances && e.Master != id) {
				continue
			}
			if master, exists := g.owned[e.ID]; exists && master != e.Master {
				resp.Body.Close()
				return deny()
			}
			if err := g.membership(req.Context(), e); err != nil {
				resp.Body.Close()
				return nil, err
			}
			// checkEvent revalidates this binding before every operation.
			g.owned[e.ID] = e.Master
		}
	}
	return resp, nil
}
