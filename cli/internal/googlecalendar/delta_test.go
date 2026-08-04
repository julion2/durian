package googlecalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// forbiddenWithSyncToken are the parameters Google rejects alongside a
// syncToken with a 400 — the classic incremental-sync trap, and one that reads
// as a client bug rather than an expired token.
var forbiddenWithSyncToken = []string{
	"timeMin", "timeMax", "updatedMin", "orderBy", "q", "iCalUID",
	"privateExtendedProperty", "sharedExtendedProperty",
}

// assertQueryShape checks the invariants every round has to hold: the pinned
// parameter set, and nothing Google forbids next to a syncToken.
func assertQueryShape(t *testing.T, q url.Values) {
	t.Helper()
	if got := q.Get("singleEvents"); got != "false" {
		t.Errorf("singleEvents = %q, want \"false\"", got)
	}
	if got := q.Get("showDeleted"); got != "true" {
		t.Errorf("showDeleted = %q, want \"true\"", got)
	}
	if q.Get("syncToken") == "" {
		return
	}
	for _, param := range forbiddenWithSyncToken {
		if q.Get(param) != "" {
			t.Errorf("incremental round sent %s=%q, which Google answers with 400",
				param, q.Get(param))
		}
	}
}

func TestFetchMasterEventsDeltaFullRound(t *testing.T) {
	const page = `{
		"items": [
			{"id": "id-a", "status": "confirmed", "iCalUID": "uid-a", "summary": "A",
			 "start": {"dateTime": "2026-08-03T09:00:00Z"}, "end": {"dateTime": "2026-08-03T10:00:00Z"}}
		],
		"nextSyncToken": "tok-1"
	}`
	var sawSyncToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		assertQueryShape(t, r.URL.Query())
		sawSyncToken = r.URL.Query().Get("syncToken")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "")
	if err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}
	if sawSyncToken != "" {
		t.Errorf("full round sent syncToken=%q", sawSyncToken)
	}
	if !result.Reset {
		t.Error("a round without a cursor must report Reset: its items ARE the whole calendar")
	}
	if result.Cursor != "tok-1" {
		t.Errorf("cursor = %q, want tok-1", result.Cursor)
	}
	if len(result.ChangedMasters) != 1 || result.ChangedMasters[0].ICalUID != "uid-a" {
		t.Errorf("changed masters = %+v", result.ChangedMasters)
	}
	if result.ParamFingerprint != deltaParamFingerprint {
		t.Errorf("fingerprint = %q, want %q", result.ParamFingerprint, deltaParamFingerprint)
	}
}

func TestFetchMasterEventsDeltaIncrementalRound(t *testing.T) {
	const page = `{
		"items": [
			{"id": "id-a", "status": "confirmed", "iCalUID": "uid-a", "summary": "A changed",
			 "start": {"dateTime": "2026-08-03T09:00:00Z"}, "end": {"dateTime": "2026-08-03T10:00:00Z"}},
			{"id": "id-gone", "status": "cancelled"}
		],
		"nextSyncToken": "tok-2"
	}`
	var sawSyncToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		assertQueryShape(t, r.URL.Query())
		sawSyncToken = r.URL.Query().Get("syncToken")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-1")
	if err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}
	if sawSyncToken != "tok-1" {
		t.Errorf("syncToken = %q, want tok-1", sawSyncToken)
	}
	if result.Reset {
		t.Error("an incremental round must not report Reset: it only mentions what moved")
	}
	if len(result.ChangedMasters) != 1 {
		t.Errorf("changed masters = %+v", result.ChangedMasters)
	}
	// The tombstone is the whole point: a change feed never mentions what is
	// merely absent, so without it a deletion stays invisible forever.
	if len(result.RemovedIDs) != 1 || result.RemovedIDs[0] != "id-gone" {
		t.Errorf("removed ids = %v, want [id-gone]", result.RemovedIDs)
	}
	if result.Cursor != "tok-2" {
		t.Errorf("cursor = %q, want tok-2", result.Cursor)
	}
}

// The sync token appears on the LAST page only.
func TestFetchMasterEventsDeltaPagesToTheToken(t *testing.T) {
	const page1 = `{
		"items": [
			{"id": "id-a", "status": "confirmed", "iCalUID": "uid-a", "summary": "A",
			 "start": {"dateTime": "2026-08-03T09:00:00Z"}, "end": {"dateTime": "2026-08-03T10:00:00Z"}}
		],
		"nextPageToken": "page2"
	}`
	const page2 = `{
		"items": [
			{"id": "id-b", "status": "confirmed", "iCalUID": "uid-b", "summary": "B",
			 "start": {"dateTime": "2026-08-04T09:00:00Z"}, "end": {"dateTime": "2026-08-04T10:00:00Z"}}
		],
		"nextSyncToken": "tok-final"
	}`
	var pageTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		assertQueryShape(t, r.URL.Query())
		token := r.URL.Query().Get("pageToken")
		pageTokens = append(pageTokens, token)
		w.Header().Set("Content-Type", "application/json")
		if token == "" {
			_, _ = w.Write([]byte(page1))
			return
		}
		_, _ = w.Write([]byte(page2))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-1")
	if err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}
	if len(pageTokens) != 2 || pageTokens[1] != "page2" {
		t.Errorf("page tokens = %q, want an empty first token then page2", pageTokens)
	}
	if len(result.ChangedMasters) != 2 {
		t.Errorf("changed masters = %+v, want both pages", result.ChangedMasters)
	}
	if result.Cursor != "tok-final" {
		t.Errorf("cursor = %q, want the token from the last page", result.Cursor)
	}
}

// Google invalidates tokens for reasons a client cannot avoid — plain expiry,
// but also any ACL change, which makes this routine on shared calendars. It
// must resolve itself, not surface as a sync failure.
func TestFetchMasterEventsDeltaRecoversFromExpiredToken(t *testing.T) {
	const fullPage = `{
		"items": [
			{"id": "id-a", "status": "confirmed", "iCalUID": "uid-a", "summary": "A",
			 "start": {"dateTime": "2026-08-03T09:00:00Z"}, "end": {"dateTime": "2026-08-03T10:00:00Z"}}
		],
		"nextSyncToken": "tok-fresh"
	}`
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("syncToken")
		calls = append(calls, token)
		if token != "" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"fullSyncRequired"}],"code":410}}`))
			return
		}
		assertQueryShape(t, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-stale")
	if err != nil {
		t.Fatalf("an expired token must recover, got: %v", err)
	}
	if len(calls) != 2 || calls[0] != "tok-stale" || calls[1] != "" {
		t.Errorf("calls = %v, want the stale token then a full round", calls)
	}
	if !result.Reset {
		t.Error("the recovery round must report Reset so the mirror is replaced, not merged")
	}
	if result.Cursor != "tok-fresh" {
		t.Errorf("cursor = %q, want the freshly minted one", result.Cursor)
	}
}

// A 400 means the parameters did not match the ones the token was minted with.
// Retrying it as a full sync would hide a client bug that then repeats forever.
func TestFetchMasterEventsDeltaDoesNotSwallowBadRequest(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid combination of query parameters"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-1"); err == nil {
		t.Fatal("a 400 was swallowed; a wrong parameter set would never be noticed")
	}
	if calls != 1 {
		t.Errorf("made %d calls, want exactly 1 (no full-sync retry)", calls)
	}
}

// An instance whose master is NOT part of the round travels as an occurrence
// change, because only the mirror holds the master it belongs to.
func TestFetchMasterEventsDeltaReportsLoneInstanceAsOverride(t *testing.T) {
	const page = `{
		"items": [
			{"id": "id-series_20260817T090000Z", "status": "confirmed",
			 "iCalUID": "uid-series@google.com", "summary": "Standup (moved)",
			 "recurringEventId": "id-series",
			 "originalStartTime": {"dateTime": "2026-08-17T09:00:00Z"},
			 "start": {"dateTime": "2026-08-18T14:00:00Z"},
			 "end": {"dateTime": "2026-08-18T15:00:00Z"}},
			{"id": "id-series_20260810T090000Z", "status": "cancelled",
			 "recurringEventId": "id-series",
			 "originalStartTime": {"dateTime": "2026-08-10T09:00:00Z"}}
		],
		"nextSyncToken": "tok-2"
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-1")
	if err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}
	if len(result.ChangedMasters) != 0 {
		t.Errorf("changed masters = %+v, want none: the series itself did not move", result.ChangedMasters)
	}
	if len(result.ChangedOverrides) != 2 {
		t.Fatalf("occurrence changes = %+v, want both instances", result.ChangedOverrides)
	}

	byRecurrence := map[time.Time]calendarsync.OverrideChange{}
	for _, oc := range result.ChangedOverrides {
		byRecurrence[oc.RecurrenceID.UTC()] = oc
	}
	moved := byRecurrence[time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)]
	if moved.MasterID != "id-series" || moved.Cancelled ||
		!moved.Event.Start.Equal(time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("moved occurrence = %+v", moved)
	}
	cancelled := byRecurrence[time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)]
	if !cancelled.Cancelled || cancelled.MasterID != "id-series" {
		t.Errorf("cancelled occurrence = %+v", cancelled)
	}
}

// When the master arrives in the SAME round it is folded in directly. On a
// full round that is not cosmetic: ChangedMasters is the complete remote set
// the engine replaces the mirror with, so an occurrence left outside it would
// be applied to a mirror that is about to be discarded.
func TestFetchMasterEventsDeltaFoldsInstancesOfMastersInTheSameRound(t *testing.T) {
	const page = `{
		"items": [
			{"id": "id-series_20260817T090000Z", "status": "confirmed",
			 "iCalUID": "uid-series@google.com", "summary": "Standup (moved)",
			 "recurringEventId": "id-series",
			 "originalStartTime": {"dateTime": "2026-08-17T09:00:00Z"},
			 "start": {"dateTime": "2026-08-18T14:00:00Z"},
			 "end": {"dateTime": "2026-08-18T15:00:00Z"}},
			{"id": "id-series", "status": "confirmed", "iCalUID": "uid-series@google.com",
			 "summary": "Standup",
			 "start": {"dateTime": "2026-08-03T09:00:00Z"},
			 "end": {"dateTime": "2026-08-03T10:00:00Z"},
			 "recurrence": ["RRULE:FREQ=WEEKLY;BYDAY=MO"]}
		],
		"nextSyncToken": "tok-1"
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result, err := testClient(srv).FetchMasterEventsDelta(context.Background(), "cal-primary", "")
	if err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}
	if len(result.ChangedOverrides) != 0 {
		t.Errorf("occurrence changes = %+v, want them folded into the master", result.ChangedOverrides)
	}
	if len(result.ChangedMasters) != 1 {
		t.Fatalf("changed masters = %+v", result.ChangedMasters)
	}
	master := result.ChangedMasters[0]
	if len(master.Overrides) != 1 ||
		!master.Overrides[0].RecurrenceID.Equal(time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("master overrides = %+v", master.Overrides)
	}
}

// The full and the incremental round must send the SAME parameters: a token is
// only replayable against the query that minted it.
func TestFullAndDeltaRoundsShareTheQueryShape(t *testing.T) {
	var fullQuery, deltaQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/calendars/cal-primary/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("syncToken") == "" {
			fullQuery = q
		} else {
			deltaQuery = q
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": [], "nextSyncToken": "tok"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := testClient(srv)
	if _, err := client.FetchMasterEvents(context.Background(), "cal-primary"); err != nil {
		t.Fatalf("FetchMasterEvents: %v", err)
	}
	if _, err := client.FetchMasterEventsDelta(context.Background(), "cal-primary", "tok-1"); err != nil {
		t.Fatalf("FetchMasterEventsDelta: %v", err)
	}

	for _, param := range []string{"singleEvents", "showDeleted", "maxResults"} {
		if fullQuery.Get(param) != deltaQuery.Get(param) {
			t.Errorf("%s differs between rounds: full=%q delta=%q — the token would be rejected",
				param, fullQuery.Get(param), deltaQuery.Get(param))
		}
	}
}
