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
	"strings"
	"sync"
	"time"
)

const GoogleBase = "https://www.googleapis.com/calendar/v3"
const calendarDescription = "Durian isolated calendar integration tests. No attendees."

// EnsureGoogleCalendar is the only calendar-creation path. It never modifies
// or deletes an existing calendar, and fails closed on ambiguous names.
func EnsureGoogleCalendar(ctx context.Context, source *TokenSource) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	request := func(method, path string, body []byte, result any) error {
		token, err := source.Token(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, GoogleBase+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("construct calendar bootstrap request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("calendar bootstrap outcome unknown; inspect before retrying")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("calendar bootstrap HTTP %d", resp.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(result); err != nil {
			return fmt.Errorf("invalid calendar bootstrap response")
		}
		return nil
	}
	type metadata struct {
		ID          string `json:"id"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Primary     bool   `json:"primary"`
		AccessRole  string `json:"accessRole"`
	}
	var found []metadata
	page := ""
	for {
		var list struct {
			Items []metadata `json:"items"`
			Next  string     `json:"nextPageToken"`
		}
		if err := request(http.MethodGet, "/users/me/calendarList?maxResults=250&pageToken="+url.QueryEscape(page), nil, &list); err != nil {
			return "", err
		}
		for _, c := range list.Items {
			if c.Summary == "Durian Test" {
				found = append(found, c)
			}
		}
		page = list.Next
		if page == "" {
			break
		}
	}
	if len(found) > 1 {
		return "", fmt.Errorf("ambiguous Durian Test calendars; refusing writes")
	}
	if len(found) == 1 {
		c := found[0]
		if c.Primary || c.AccessRole != "owner" || c.Description != calendarDescription || !secondaryCalendar(c.ID) {
			return "", fmt.Errorf("existing Durian Test calendar lacks expected ownership marker")
		}
		return c.ID, nil
	}
	body, _ := json.Marshal(map[string]string{"summary": "Durian Test", "description": calendarDescription, "timeZone": "Europe/Berlin"})
	var created metadata
	// Deliberately no automatic retry: an unknown POST result could duplicate
	// calendars. A later run discovers the ownership marker through the list.
	if err := request(http.MethodPost, "/calendars", body, &created); err != nil {
		return "", err
	}
	if created.Summary != "Durian Test" || created.Description != calendarDescription || !secondaryCalendar(created.ID) {
		return "", fmt.Errorf("created calendar failed identity check; refusing events")
	}
	return created.ID, nil
}

func secondaryCalendar(id string) bool {
	return strings.HasSuffix(id, "@group.calendar.google.com") && !strings.ContainsAny(id, "/\\?#%")
}

var eventID = regexp.MustCompile(`^[a-v0-9]+$`)
var instanceSuffix = regexp.MustCompile(`^_[0-9]{8}(T[0-9]{6}Z)?$`)

const foldStartZone = "durian.fold.v1.startZone"
const foldEndZone = "durian.fold.v1.endZone"

// Only the production single-fold workaround is allowed, never arbitrary
// private/shared properties. Null deletion is a paired PATCH operation.
func googleFoldProperties(raw json.RawMessage, patch bool) bool {
	var props map[string]json.RawMessage
	var private map[string]json.RawMessage
	if json.Unmarshal(raw, &props) != nil || len(props) != 1 ||
		json.Unmarshal(props["private"], &private) != nil || len(private) != 2 {
		return false
	}
	start, hasStart := private[foldStartZone]
	end, hasEnd := private[foldEndZone]
	if !hasStart || !hasEnd {
		return false
	}
	if string(start) == "null" || string(end) == "null" {
		return patch && string(start) == "null" && string(end) == "null"
	}
	for _, rawZone := range []json.RawMessage{start, end} {
		var zone string
		if json.Unmarshal(rawZone, &zone) != nil || zone == "" || zone == "Local" {
			return false
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return false
		}
	}
	return true
}

// GoogleGuard checks every request before adding credentials. Even an
// incorrectly scoped provider method cannot write to primary/other calendars.
type GoogleGuard struct {
	mu                 sync.Mutex
	calendarID, prefix string
	source             *TokenSource
	transport          http.RoundTripper
	owned              map[string]bool
}

func NewGoogleGuard(calendarID, prefix string, source *TokenSource) (*GoogleGuard, error) {
	if !secondaryCalendar(calendarID) || !strings.HasPrefix(prefix, "Durian Test ") || len(prefix) < 25 || source == nil {
		return nil, fmt.Errorf("invalid isolated test scope")
	}
	return &GoogleGuard{calendarID: calendarID, prefix: prefix, source: source, transport: http.DefaultTransport, owned: map[string]bool{}}, nil
}

func (g *GoogleGuard) HTTPClient() *http.Client {
	return &http.Client{Transport: g, Timeout: 45 * time.Second, CheckRedirect: noRedirect}
}

func (g *GoogleGuard) owns(id string) bool {
	if g.owned[id] {
		return true
	}
	for master := range g.owned {
		if suffix, ok := strings.CutPrefix(id, master); ok && instanceSuffix.MatchString(suffix) {
			return true
		}
	}
	return false
}

func (g *GoogleGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	deny := func() (*http.Response, error) { return nil, fmt.Errorf("isolated calendar guard blocked request") }
	u := req.URL
	base := "/calendar/v3/calendars/" + g.calendarID + "/events"
	if u.Scheme != "https" || u.Host != "www.googleapis.com" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (req.Host != "" && req.Host != u.Host) {
		return deny()
	}
	id := ""
	if u.Path != base {
		var ok bool
		id, ok = strings.CutPrefix(u.Path, base+"/")
		if !ok || !g.owns(id) {
			return deny()
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || req.Header.Get("X-HTTP-Method-Override") != "" {
		return deny()
	}
	for key, vals := range query {
		switch key {
		case "sendUpdates", "maxResults", "singleEvents", "timeMin", "timeMax", "showDeleted", "pageToken", "orderBy":
			if len(vals) != 1 {
				return deny()
			}
		default:
			return deny()
		}
	}
	if req.Method != http.MethodGet {
		if query.Get("sendUpdates") != "none" {
			return deny()
		}
		switch req.Method {
		case http.MethodPost, http.MethodPatch:
			if (req.Method == http.MethodPost) != (id == "") || req.Body == nil {
				return deny()
			}
			body, err := io.ReadAll(io.LimitReader(req.Body, (1<<20)+1))
			if err != nil || len(body) > 1<<20 {
				return deny()
			}
			req.Body.Close()
			req.Body = io.NopCloser(bytes.NewReader(body))
			var data map[string]json.RawMessage
			if json.Unmarshal(body, &data) != nil {
				return deny()
			}
			// Allowlist, not just an attendee denylist: no conference, organizer,
			// email attachments, gadgets or future notification side channels.
			for key := range data {
				switch key {
				case "id", "summary", "description", "location", "start", "end", "recurrence":
				case "extendedProperties":
					if !googleFoldProperties(data[key], req.Method == http.MethodPatch) {
						return deny()
					}
				default:
					return deny()
				}
			}
			var summary string
			if json.Unmarshal(data["summary"], &summary) != nil || !strings.HasPrefix(summary, g.prefix) {
				return deny()
			}
			if req.Method == http.MethodPost {
				var createdID string
				if json.Unmarshal(data["id"], &createdID) != nil || len(createdID) < 5 || len(createdID) > 1024 || !eventID.MatchString(createdID) {
					return deny()
				}
				// The harness uses the prefixed title as its idempotency key.
				// Do not let a conflicting arbitrary ID enroll an unrelated event
				// for cleanup merely because POST returned 409.
				sum := sha256.Sum256([]byte(summary))
				if createdID != hex.EncodeToString(sum[:]) {
					return deny()
				}
				// Register before sending, so a lost POST answer remains eligible
				// for bounded cleanup using the deterministic idempotency key.
				g.owned[createdID] = true
			} else if _, present := data["id"]; present {
				return deny()
			}
			// Forward the checked tree, not duplicate-key JSON whose provider
			// interpretation could differ from encoding/json's last-key wins.
			var canonical map[string]any
			if json.Unmarshal(body, &canonical) != nil {
				return deny()
			}
			body, err = json.Marshal(canonical)
			if err != nil {
				return deny()
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		case http.MethodDelete:
			if id == "" || req.Body != nil {
				return deny()
			}
		default:
			return deny()
		}
	}
	token, err := g.source.Token(req.Context())
	if err != nil {
		return nil, err
	}
	copy := req.Clone(req.Context())
	copy.Header.Set("Authorization", "Bearer "+token)
	return g.transport.RoundTrip(copy)
}

func (g *GoogleGuard) CreatedIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.owned))
	for id := range g.owned {
		ids = append(ids, id)
	}
	return ids
}
