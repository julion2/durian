// Aliases re-exporting the provider-neutral calendar model and local vdir
// layer, which moved to the calendar package. They keep every existing caller
// (handler, cmd) and the remaining Graph-specific files in this package
// compiling unchanged; callers migrate to importing calendar directly in a
// later step.

package graphcalendar

import "github.com/julion2/durian/cli/internal/calendar"

// MARK: - Model

type (
	// Event is one calendar event; see calendar.Event.
	Event = calendar.Event
	// Attendee is one meeting attendee; see calendar.Attendee.
	Attendee = calendar.Attendee
	// Person is a name/email pair; see calendar.Person.
	Person = calendar.Person
	// OwnerResp is the canonical owner-RSVP state; see calendar.OwnerResp.
	OwnerResp = calendar.OwnerResp
	// Recurrence mirrors the Graph patternedRecurrence resource.
	Recurrence = calendar.Recurrence
	// RecurrencePattern is the Graph recurrencePattern resource.
	RecurrencePattern = calendar.RecurrencePattern
	// RecurrenceRange is the Graph recurrenceRange resource.
	RecurrenceRange = calendar.RecurrenceRange
)

const (
	OwnerRespNone      = calendar.OwnerRespNone
	OwnerRespTentative = calendar.OwnerRespTentative
	OwnerRespAccepted  = calendar.OwnerRespAccepted
	OwnerRespDeclined  = calendar.OwnerRespDeclined
	OwnerRespOrganizer = calendar.OwnerRespOrganizer
)

// MARK: - iCalendar round-trip and vdir readers

type (
	// LocalCalendar is one calendar directory of the vdir.
	LocalCalendar = calendar.LocalCalendar
)

var (
	EventToICal       = calendar.EventToICal
	WriteFileAtomic   = calendar.WriteFileAtomic
	ICalToEvent       = calendar.ICalToEvent
	EventToICS        = calendar.EventToICS
	ReadCalendars     = calendar.ReadCalendars
	ExpandOccurrences = calendar.ExpandOccurrences
	ResolveLocalEvent = calendar.ResolveLocalEvent
	WriteLocalEvent   = calendar.WriteLocalEvent
	ParseWhen         = calendar.ParseWhen
	CalendarWindow    = calendar.CalendarWindow
	EventMatchesQuery = calendar.EventMatchesQuery
)

// MARK: - Wire DTOs

type (
	// CalendarDTO is one calendar in an API/CLI listing.
	CalendarDTO = calendar.CalendarDTO
	// PersonDTO is a name/email pair (organizer).
	PersonDTO = calendar.PersonDTO
	// AttendeeDTO is one meeting attendee.
	AttendeeDTO = calendar.AttendeeDTO
	// CalendarEvent is the wire shape of one event.
	CalendarEvent = calendar.CalendarEvent
)

// ToCalendarEvent projects an Event onto the wire DTO.
var ToCalendarEvent = calendar.ToCalendarEvent

// MARK: - Package-internal aliases

// localItem is one local .ics file found in the calendar dir.
type localItem = calendar.LocalItem

// graphDateFormat is the "YYYY-MM-DD" layout of Graph recurrenceRange dates.
const graphDateFormat = calendar.GraphDateFormat

var (
	ownerRespFromGraph   = calendar.OwnerRespFromGraph
	eventContentHash     = calendar.EventContentHash
	recurrenceJSON       = calendar.RecurrenceJSON
	attendeeSetHash      = calendar.AttendeeSetHash
	ownerIsOrganizer     = calendar.OwnerIsOrganizer
	countRecipients      = calendar.CountRecipients
	normalizeText        = calendar.NormalizeText
	recurrenceToROption  = calendar.RecurrenceToROption
	roptionToRecurrence  = calendar.ROptionToRecurrence
	attendeeRole         = calendar.AttendeeRole
	attendeePartStat     = calendar.AttendeePartStat
	attendeeTypeFromRole = calendar.AttendeeTypeFromRole
	responseFromPartStat = calendar.ResponseFromPartStat
	sanitizeName         = calendar.SanitizeName
	calendarIncluded     = calendar.CalendarIncluded
	dateOnly             = calendar.DateOnly
	scanLocalItems       = calendar.ScanLocalItems
	hashBytes            = calendar.HashBytes
)
