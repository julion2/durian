package graphcalendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// MARK: - FetchMasterEvents

func TestFetchMasterEvents(t *testing.T) {
	var preferHeaders []string
	var srv *httptest.Server

	masterEvent := func(id, typ string) string {
		return fmt.Sprintf(`{
			"id": %q,
			"iCalUId": "ical-%s",
			"subject": "Event %s",
			"body": {"contentType": "text", "content": "full body %s"},
			"bodyPreview": "preview %s",
			"start": {"dateTime": "2026-07-23T10:00:00.0000000", "timeZone": "UTC"},
			"end": {"dateTime": "2026-07-23T11:00:00.0000000", "timeZone": "UTC"},
			"isAllDay": false,
			"location": {"displayName": "HQ"},
			"type": %q,
			"changeKey": "ck-%s",
			"lastModifiedDateTime": "2026-07-20T09:00:00Z"
		}`, id, id, id, id, id, typ, id)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/me/calendars/cal1/events", func(w http.ResponseWriter, r *http.Request) {
		preferHeaders = append(preferHeaders, r.Header.Get("Prefer"))
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer test-token", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			// Page 2: a seriesMaster with a weekly recurrence, and an
			// occurrence that must be skipped.
			fmt.Fprintf(w, `{"value": [
				{
					"id": "master1",
					"iCalUId": "ical-master1",
					"subject": "Weekly sync",
					"body": {"contentType": "html", "content": "<p>hi</p>"},
					"bodyPreview": "hi",
					"start": {"dateTime": "2026-07-20T09:00:00.0000000", "timeZone": "UTC"},
					"end": {"dateTime": "2026-07-20T09:30:00.0000000", "timeZone": "UTC"},
					"isAllDay": false,
					"location": {"displayName": "Zoom"},
					"type": "seriesMaster",
					"changeKey": "ck-master1",
					"recurrence": {
						"pattern": {
							"type": "weekly",
							"interval": 2,
							"daysOfWeek": ["monday", "wednesday"],
							"firstDayOfWeek": "sunday"
						},
						"range": {
							"type": "endDate",
							"startDate": "2026-07-20",
							"endDate": "2026-11-02"
						}
					},
					"lastModifiedDateTime": "2026-07-21T10:00:00Z"
				},
				%s
			]}`, masterEvent("occ1", "occurrence"))
			return
		}
		fmt.Fprintf(w, `{"value": [%s], "@odata.nextLink": %q}`,
			masterEvent("single1", "singleInstance"),
			srv.URL+"/me/calendars/cal1/events?page=2")
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(srv)
	events, err := c.FetchMasterEvents(context.Background(), "cal1")
	if err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}

	if len(preferHeaders) != 2 {
		t.Fatalf("expected 2 pages fetched, got %d", len(preferHeaders))
	}
	for i, h := range preferHeaders {
		if h != `outlook.timezone="UTC", IdType="ImmutableId"` {
			t.Errorf("page %d Prefer header = %q, want outlook.timezone UTC + immutable ids", i+1, h)
		}
	}

	// The occurrence must be skipped: one singleInstance + one seriesMaster.
	if len(events) != 2 {
		t.Fatalf("expected 2 events (occurrence skipped), got %d: %+v", len(events), events)
	}

	single := events[0]
	if single.ID != "single1" || single.Type != "singleInstance" || single.ETag != "ck-single1" {
		t.Errorf("single fields mismatched: %+v", single)
	}
	if single.Description != "full body single1" {
		t.Errorf("single.Description = %q, want full text body preferred over preview", single.Description)
	}
	if single.Recurrence != nil {
		t.Errorf("single.Recurrence = %+v, want nil", single.Recurrence)
	}

	master := events[1]
	if master.ID != "master1" || master.Type != "seriesMaster" || master.ETag != "ck-master1" {
		t.Errorf("master fields mismatched: %+v", master)
	}
	// HTML body: fall back to bodyPreview.
	if master.Description != "hi" {
		t.Errorf("master.Description = %q, want bodyPreview fallback for html body", master.Description)
	}
	if master.Recurrence == nil {
		t.Fatalf("master.Recurrence = nil, want weekly recurrence")
	}
	rec := master.Recurrence
	if rec.Pattern.Type != "weekly" || rec.Pattern.Interval != 2 {
		t.Errorf("master recurrence pattern = %+v, want weekly interval 2", rec.Pattern)
	}
	if want := []string{"monday", "wednesday"}; !reflect.DeepEqual(rec.Pattern.DaysOfWeek, want) {
		t.Errorf("master recurrence days = %v, want %v", rec.Pattern.DaysOfWeek, want)
	}
	if rec.Range.Type != "endDate" || rec.Range.EndDate != "2026-11-02" {
		t.Errorf("master recurrence range = %+v, want endDate 2026-11-02", rec.Range)
	}
	if want := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC); !master.Start.Equal(want) {
		t.Errorf("master.Start = %v, want %v", master.Start, want)
	}
}
