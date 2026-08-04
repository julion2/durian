package calendar

import "testing"

func TestParseRSVPVerb(t *testing.T) {
	cases := map[string]OwnerResp{
		// CLI verbs, case-insensitive.
		"accept":    OwnerRespAccepted,
		"Decline":   OwnerRespDeclined,
		"TENTATIVE": OwnerRespTentative,
		// Graph-style response values as the GUI sends them.
		"accepted":            OwnerRespAccepted,
		"declined":            OwnerRespDeclined,
		"tentativelyAccepted": OwnerRespTentative,
	}
	for in, want := range cases {
		got, err := ParseRSVPVerb(in)
		if err != nil || got != want {
			t.Errorf("ParseRSVPVerb(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"maybe", "", "organizer"} {
		if _, err := ParseRSVPVerb(in); err == nil {
			t.Errorf("ParseRSVPVerb(%q): want error", in)
		}
	}
}

func TestSetOwnerResponse(t *testing.T) {
	owner := "me@example.com"
	e := Event{Attendees: []Attendee{
		{Email: "other@example.com", Response: "accepted"},
		{Email: "ME@example.com", Response: "none"}, // owner, case-insensitive
	}}
	if !SetOwnerResponse(&e, owner, OwnerRespDeclined) {
		t.Fatal("SetOwnerResponse: want true (owner is an attendee)")
	}
	if e.OwnerResponse != OwnerRespDeclined {
		t.Errorf("OwnerResponse = %q, want declined", e.OwnerResponse)
	}
	if e.Attendees[1].Response != "declined" {
		t.Errorf("owner attendee Response = %q, want declined", e.Attendees[1].Response)
	}
	if e.Attendees[0].Response != "accepted" {
		t.Error("non-owner attendee response must not change")
	}

	// Tentative maps onto the Graph-style attendee response value.
	if !SetOwnerResponse(&e, owner, OwnerRespTentative) {
		t.Fatal("SetOwnerResponse tentative: want true")
	}
	if e.Attendees[1].Response != "tentativelyAccepted" {
		t.Errorf("owner attendee Response = %q, want tentativelyAccepted", e.Attendees[1].Response)
	}

	// Owner not among attendees.
	e2 := Event{Attendees: []Attendee{{Email: "x@example.com"}}}
	if SetOwnerResponse(&e2, owner, OwnerRespAccepted) {
		t.Error("SetOwnerResponse: want false when owner is not an attendee")
	}
	if e2.OwnerResponse != OwnerRespNone {
		t.Errorf("OwnerResponse = %q, want unchanged", e2.OwnerResponse)
	}
}
