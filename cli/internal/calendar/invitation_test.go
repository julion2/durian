package calendar

import (
	"strings"
	"testing"
	"time"
)

// invite builds an iMIP body the way a mail client sends one.
func invite(method string, vevents ...string) []byte {
	doc := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Mail//EN\r\n"
	if method != "" {
		doc += "METHOD:" + method + "\r\n"
	}
	for _, v := range vevents {
		doc += "BEGIN:VEVENT\r\n" + v + "END:VEVENT\r\n"
	}
	return []byte(doc + "END:VCALENDAR\r\n")
}

const planning = "UID:planning-2026@example.com\r\n" +
	"DTSTAMP:20261001T080000Z\r\n" +
	"DTSTART;TZID=Europe/Berlin:20261020T100000\r\n" +
	"DTEND;TZID=Europe/Berlin:20261020T110000\r\n" +
	"SUMMARY:Quarterly planning\r\n" +
	"LOCATION:Room 4\r\n" +
	"ORGANIZER;CN=Mara:mailto:mara@example.com\r\n" +
	"ATTENDEE;CN=Mara;PARTSTAT=ACCEPTED;ROLE=REQ-PARTICIPANT:mailto:mara@example.com\r\n" +
	"ATTENDEE;CN=Me;PARTSTAT=NEEDS-ACTION;ROLE=REQ-PARTICIPANT:mailto:Me@Example.com\r\n"

func TestParseInvitation_Request(t *testing.T) {
	inv, err := ParseInvitation(invite("REQUEST", planning), "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e := inv.Event
	if inv.Method != "REQUEST" || inv.Occurrence {
		t.Errorf("method %q occurrence %v, want REQUEST for the whole event", inv.Method, inv.Occurrence)
	}
	if e.UID != "planning-2026@example.com" || e.Subject != "Quarterly planning" || e.Location != "Room 4" {
		t.Errorf("event = %+v", e)
	}
	if want := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC); !e.Start.Equal(want) {
		t.Errorf("start = %v, want %v (10:00 in Berlin)", e.Start, want)
	}
	if e.Organizer == nil || e.Organizer.Email != "mara@example.com" || len(e.Attendees) != 2 {
		t.Errorf("organizer %+v, %d attendees; want Mara and two attendees", e.Organizer, len(e.Attendees))
	}
	if e.MyResponse == string(OwnerRespAccepted) || e.MyResponse == string(OwnerRespOrganizer) {
		t.Errorf("my response = %q, want unanswered", e.MyResponse)
	}
}

// The recipient's own response is found whatever case the address has.
func TestParseInvitation_MyResponse(t *testing.T) {
	answered := strings.Replace(planning, "PARTSTAT=NEEDS-ACTION", "PARTSTAT=ACCEPTED", 1)
	inv, err := ParseInvitation(invite("REQUEST", answered), "ME@example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Event.MyResponse != string(OwnerRespAccepted) {
		t.Errorf("my response = %q, want %q", inv.Event.MyResponse, OwnerRespAccepted)
	}
}

// Moving one date of a series sends that occurrence alone.
func TestParseInvitation_OneOccurrence(t *testing.T) {
	moved := strings.Replace(planning, "DTSTART;TZID=Europe/Berlin:20261020T100000",
		"RECURRENCE-ID;TZID=Europe/Berlin:20261020T100000\r\nDTSTART;TZID=Europe/Berlin:20261021T140000", 1)
	moved = strings.Replace(moved, "DTEND;TZID=Europe/Berlin:20261020T110000", "DTEND;TZID=Europe/Berlin:20261021T150000", 1)
	inv, err := ParseInvitation(invite("REQUEST", moved), "me@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Occurrence {
		t.Error("occurrence = false, want true: only this date is meant")
	}
	if want := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC); !inv.Event.Start.Equal(want) || inv.Event.UID != "planning-2026@example.com" {
		t.Errorf("event %s at %v, want the series' UID at %v", inv.Event.UID, inv.Event.Start, want)
	}
}

func TestParseInvitation_MethodIsNormalized(t *testing.T) {
	for _, c := range []struct{ method, want string }{{"cancel", "CANCEL"}, {" Reply ", "REPLY"}, {"", ""}} {
		inv, err := ParseInvitation(invite(c.method, planning), "me@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if inv.Method != c.want {
			t.Errorf("METHOD %q read as %q, want %q", c.method, inv.Method, c.want)
		}
	}
}

func TestParseInvitation_Unreadable(t *testing.T) {
	series := strings.Replace(planning, "SUMMARY:", "RRULE:FREQ=WEEKLY\r\nSUMMARY:", 1)
	twoMasters := invite("REQUEST", series, strings.Replace(planning, "planning-2026", "other", 1))
	occurrence := strings.Replace(planning, "SUMMARY:", "RECURRENCE-ID:20261020T080000Z\r\nSUMMARY:", 1)
	for name, data := range map[string][]byte{
		"not iCalendar": []byte("hello"),
		"no event":      invite("REQUEST"),
		"two series":    twoMasters,
		// not one occurrence alone: the series it belongs to is unreadable
		"occurrence before two series": invite("REQUEST", occurrence, series, strings.Replace(planning, "planning-2026", "other", 1)),
	} {
		if inv, err := ParseInvitation(data, "me@example.com"); err == nil {
			t.Errorf("%s: parsed as %+v, want an error", name, inv)
		}
	}
}
