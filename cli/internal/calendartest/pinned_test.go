package calendartest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExistingPinsNeverDiscoverOrCreate(t *testing.T) {
	const googleID = "test@group.calendar.google.com"
	googleMetadata := `{"id":"` + googleID + `","summary":"Durian Test","description":"` + calendarDescription + `","accessRole":"owner","primary":false}`
	for _, tc := range []struct {
		name, google, graph string
		allowed             bool
	}{
		{"exact", googleMetadata, graphTestMetadata, true},
		{"google-wrong-id", strings.Replace(googleMetadata, googleID, "other@group.calendar.google.com", 1), graphTestMetadata, false},
		{"google-primary", strings.Replace(googleMetadata, `"primary":false`, `"primary":true`, 1), graphTestMetadata, false},
		{"google-marker", strings.Replace(googleMetadata, calendarDescription, "missing", 1), graphTestMetadata, false},
		{"graph-wrong-id", googleMetadata, strings.Replace(graphTestMetadata, "test-calendar", "other-calendar", 1), false},
		{"graph-default", googleMetadata, strings.Replace(graphTestMetadata, `"isDefaultCalendar":false`, `"isDefaultCalendar":true`, 1), false},
		{"graph-owner", googleMetadata, strings.Replace(graphTestMetadata, "demo@example.test", "other@example.test", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graphTestDefault(t, graphTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("pin verification attempted mutation")
				}
				switch r.URL.Path {
				case "/calendar/v3/users/me/calendarList/" + googleID:
					return graphTestResponse(r, 200, tc.google), nil
				case "/v1.0/me/calendars/test-calendar":
					return graphTestResponse(r, 200, tc.graph), nil
				default:
					t.Fatal("pin verification attempted discovery or other scope")
					return nil, nil
				}
			}))
			g := &TokenSource{provider: Google, accessToken: "fake", expires: time.Now().Add(time.Hour)}
			googleErr := ExistingGoogleCalendar(context.Background(), g, googleID)
			_, graphErr := ExistingGraphCalendar(context.Background(), graphTestSource(), "test-calendar")
			if (googleErr == nil && graphErr == nil) != tc.allowed {
				t.Fatal("pin verification ignored exact ownership metadata")
			}
		})
	}
}

func TestGraphScopedExistingReadsDoNotEnrollWrites(t *testing.T) {
	g, f := graphFixture(t)
	// An earlier run's series can be downloaded/enriched, never adopted as
	// write ownership. Even a moved ID previously seen in the list is denied.
	ev := map[string]any{"id": "old-series", "type": "seriesMaster", "subject": "Durian Test previous run", "attendees": []any{}, "isOrganizer": true, "isOnlineMeeting": false, "isReminderOn": false, "@odata.etag": "old"}
	f.events["old-series"], f.calendars["old-series"] = ev, "test-calendar"
	f.values = []map[string]any{ev}
	graphTestDo(t, g, "GET", GraphBase+"/me/events/old-series", "", false)
	graphTestDo(t, g, "GET", GraphBase+"/me/calendars/test-calendar/events", "", true)
	graphTestDo(t, g, "GET", GraphBase+"/me/events/old-series?$expand=exceptionOccurrences", "", true)
	graphTestDo(t, g, "PATCH", GraphBase+"/me/events/old-series", `{"subject":"`+graphTestPrefix+`changed"}`, false)
	graphTestDo(t, g, "DELETE", GraphBase+"/me/events/old-series", "", false)
	graphTestDo(t, g, "GET", GraphBase+"/me/calendars/other/events", "", false)
	f.calendars["old-series"] = "other-calendar"
	graphTestDo(t, g, "GET", GraphBase+"/me/events/old-series?$expand=exceptionOccurrences", "", false)
	if len(g.CreatedIDs()) != 0 || f.writes != 0 {
		t.Fatal("read-only snapshot enrolled mutation ownership")
	}
}
