// Public, snake_case data-transfer objects for surfacing calendar data to
// callers outside the sync engine — the CLI's --json output and the durian
// serve HTTP API (documented in openapi.yaml). The internal Event/Calendar
// types carry Microsoft-Graph-shaped JSON tags and bookkeeping fields
// (changeKey, type, …); these DTOs are the stable, minimal wire contract.

package graphcalendar

import "time"

// CalendarDTO is one calendar in an API/CLI listing.
type CalendarDTO struct {
	Name       string `json:"name"`
	Color      string `json:"color,omitempty"`
	EventCount int    `json:"event_count"`
}

// PersonDTO is a name/email pair (organizer).
type PersonDTO struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// AttendeeDTO is one meeting attendee.
type AttendeeDTO struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email"`
	Type     string `json:"type,omitempty"`
	Response string `json:"response,omitempty"`
}

// CalendarEvent is the wire shape of one event (or expanded occurrence). The
// summary fields are always set; the detail fields (organizer, attendees,
// description) are only filled when detail is requested — list/search emit the
// summary, the single-event lookup emits the full form.
type CalendarEvent struct {
	Calendar         string    `json:"calendar"`
	UID              string    `json:"uid"`
	Subject          string    `json:"subject"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	AllDay           bool      `json:"all_day"`
	Location         string    `json:"location,omitempty"`
	MyResponse       string    `json:"my_response,omitempty"`
	OnlineMeeting    bool      `json:"online_meeting,omitempty"`
	OnlineMeetingURL string    `json:"online_meeting_url,omitempty"`
	Recurring        bool      `json:"recurring,omitempty"`

	// Detail-only fields (omitted from list/search responses).
	Organizer   *PersonDTO    `json:"organizer,omitempty"`
	Attendees   []AttendeeDTO `json:"attendees,omitempty"`
	Description string        `json:"description,omitempty"`
}

// ToCalendarEvent projects an Event onto the wire DTO for the given calendar
// name. With detail=false only the summary fields are populated (organizer,
// attendees and description are left empty); with detail=true the full form is
// returned.
func ToCalendarEvent(calendarName string, e Event, detail bool) CalendarEvent {
	dto := CalendarEvent{
		Calendar:         calendarName,
		UID:              e.ICalUID,
		Subject:          e.Subject,
		Start:            e.Start.UTC(),
		End:              e.End.UTC(),
		AllDay:           e.AllDay,
		Location:         e.Location,
		MyResponse:       string(e.OwnerResponse),
		OnlineMeeting:    e.IsOnlineMeeting || e.OnlineMeetingURL != "",
		OnlineMeetingURL: e.OnlineMeetingURL,
		Recurring:        e.Recurrence != nil,
	}
	if !detail {
		return dto
	}
	if e.Organizer != nil {
		dto.Organizer = &PersonDTO{Name: e.Organizer.Name, Email: e.Organizer.Email}
	}
	for _, a := range e.Attendees {
		dto.Attendees = append(dto.Attendees, AttendeeDTO{
			Name: a.Name, Email: a.Email, Type: a.Type, Response: a.Response,
		})
	}
	dto.Description = e.Description
	return dto
}
