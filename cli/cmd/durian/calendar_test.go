package main

import (
	"testing"

	"github.com/julion2/durian/cli/internal/graphcalendar"
)

func TestParseRsvpVerb(t *testing.T) {
	cases := map[string]graphcalendar.OwnerResp{
		"accept":    graphcalendar.OwnerRespAccepted,
		"Decline":   graphcalendar.OwnerRespDeclined,
		"TENTATIVE": graphcalendar.OwnerRespTentative,
	}
	for in, want := range cases {
		got, err := parseRsvpVerb(in)
		if err != nil || got != want {
			t.Errorf("parseRsvpVerb(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseRsvpVerb("maybe"); err == nil {
		t.Error("parseRsvpVerb(maybe): want error")
	}
}

func TestSetOwnerResponse(t *testing.T) {
	owner := "me@example.com"
	e := graphcalendar.Event{Attendees: []graphcalendar.Attendee{
		{Email: "other@example.com", Response: "accepted"},
		{Email: "ME@example.com", Response: "none"}, // owner, case-insensitive
	}}
	if !setOwnerResponse(&e, owner, graphcalendar.OwnerRespDeclined) {
		t.Fatal("setOwnerResponse: want true (owner is an attendee)")
	}
	if e.OwnerResponse != graphcalendar.OwnerRespDeclined {
		t.Errorf("OwnerResponse = %q, want declined", e.OwnerResponse)
	}
	if e.Attendees[1].Response != "declined" {
		t.Errorf("owner attendee Response = %q, want declined", e.Attendees[1].Response)
	}
	if e.Attendees[0].Response != "accepted" {
		t.Error("non-owner attendee response must not change")
	}

	// Owner not among attendees.
	e2 := graphcalendar.Event{Attendees: []graphcalendar.Attendee{{Email: "x@example.com"}}}
	if setOwnerResponse(&e2, owner, graphcalendar.OwnerRespAccepted) {
		t.Error("setOwnerResponse: want false when owner is not an attendee")
	}
}

func TestParseHexColor(t *testing.T) {
	if r, g, b, ok := parseHexColor("#FF8000"); !ok || r != 255 || g != 128 || b != 0 {
		t.Errorf("parseHexColor(#FF8000) = %d,%d,%d,%v", r, g, b, ok)
	}
	if _, _, _, ok := parseHexColor("auto"); ok {
		t.Error("parseHexColor(auto): want ok=false")
	}
	if _, _, _, ok := parseHexColor(""); ok {
		t.Error("parseHexColor(empty): want ok=false")
	}
}

func TestRecurrenceSummary(t *testing.T) {
	e := graphcalendar.Event{Recurrence: &graphcalendar.Recurrence{
		Pattern: graphcalendar.RecurrencePattern{Type: "weekly", Interval: 2, DaysOfWeek: []string{"monday", "wednesday"}},
		Range:   graphcalendar.RecurrenceRange{Type: "numbered", NumberOfOccurrences: 10},
	}}
	got := recurrenceSummary(e)
	if got == "" {
		t.Fatal("recurrenceSummary empty for a recurring event")
	}
	if recurrenceSummary(graphcalendar.Event{}) != "" {
		t.Error("recurrenceSummary: want empty for non-recurring event")
	}
}
