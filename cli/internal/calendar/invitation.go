package calendar

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/emersion/go-ical"
)

// Invitation is the iCalendar part of a mail (an iTIP message): what the
// sender asks for and the event it is about.
type Invitation struct {
	// Method is the iTIP method, upper case: REQUEST for an invitation or
	// an update, CANCEL, REPLY for an attendee's answer, and so on. Empty
	// when the part names none.
	Method string        `json:"method,omitempty"`
	Event  CalendarEvent `json:"event"`
	// Occurrence is true when the mail is about one occurrence of a series
	// only (an update or cancellation of that date); Event is then that
	// occurrence, and answering the series would answer every date.
	Occurrence bool `json:"occurrence,omitempty"`
}

// ParseInvitation reads the iCalendar part of a mail. accountEmail is the
// address the mail reached, to read that attendee's own response.
func ParseInvitation(data []byte, accountEmail string) (*Invitation, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to decode invitation: %w", err)
	}
	inv := &Invitation{}
	if method := cal.Props.Get(ical.PropMethod); method != nil {
		inv.Method = strings.ToUpper(strings.TrimSpace(method.Value))
	}

	event, err := ICalToEvent(data, accountEmail)
	if err != nil {
		// An update to one occurrence carries only that occurrence: VEVENTs
		// with a RECURRENCE-ID and no series, which ICalToEvent rejects.
		events := cal.Events()
		if len(events) == 0 {
			return nil, err
		}
		for i := range events {
			if events[i].Props.Get(ical.PropRecurrenceID) == nil {
				return nil, err
			}
		}
		if event, err = eventFromComponent(&events[0], accountEmail); err != nil {
			return nil, err
		}
		inv.Occurrence = true
	}
	inv.Event = ToCalendarEvent("", event, true)
	return inv, nil
}
