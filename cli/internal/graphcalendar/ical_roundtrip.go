// iCalendar round-trip for the two-way sync engine: master events (see
// FetchMasterEvents) are serialized to standalone .ics documents and parsed
// back, including RRULE <-> Graph recurrence mapping. The hand-rolled one-way
// EventToICS in ics.go stays untouched for the vdir export path; this file is
// the go-ical based counterpart that must survive a parse round-trip.
//
// Recurrence mapping (Graph pattern -> RRULE):
//
//	daily            -> FREQ=DAILY
//	weekly           -> FREQ=WEEKLY;BYDAY=<daysOfWeek>
//	absoluteMonthly  -> FREQ=MONTHLY;BYMONTHDAY=<dayOfMonth>
//	relativeMonthly  -> FREQ=MONTHLY;BYDAY=<daysOfWeek>;BYSETPOS=<index> (best effort)
//	absoluteYearly   -> FREQ=YEARLY;BYMONTH=<month>;BYMONTHDAY=<dayOfMonth>
//	relativeYearly   -> FREQ=YEARLY;BYMONTH=<month>;BYDAY=<daysOfWeek>;BYSETPOS=<index> (best effort)
//
// with INTERVAL when > 1, and range endDate -> UNTIL (end of day UTC),
// numbered -> COUNT, noEnd -> neither. The relative patterns map the Graph
// index (first..fourth, last) to BYSETPOS 1..4 / -1; an unmappable pattern
// yields no RRULE plus a warning rather than a wrong rule. The parse
// direction inverts the same table; an RRULE outside it leaves Recurrence nil
// with a warning.

package graphcalendar

import (
	"bytes"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// icalProdID identifies durian-generated iCalendar documents.
const icalProdID = "-//durian//graphcalendar//EN"

// propTeamsMeetingURL is the Microsoft X-prop carrying the Teams join link;
// khal and other clients look for it alongside URL.
const propTeamsMeetingURL = "X-MICROSOFT-SKYPETEAMSMEETINGURL"

// graphDateFormat is the "YYYY-MM-DD" layout of Graph recurrenceRange dates.
const graphDateFormat = "2006-01-02"

// MARK: - Serialize

// EventToICal renders one event as a VCALENDAR with a single VEVENT using
// go-ical (CRLF output). Timed events are written as UTC date-times, all-day
// events as VALUE=DATE with Graph's exclusive DTEND kept as-is. A seriesMaster
// Recurrence becomes an RRULE per the mapping documented in the file header;
// an unmappable recurrence is dropped with a warning so the event itself still
// round-trips.
//
// Read-only meeting metadata is surfaced as standard properties: ORGANIZER
// (with CN), one ATTENDEE per attendee (CN/ROLE/PARTSTAT/RSVP, sorted by email
// so the output is deterministic), URL plus X-MICROSOFT-SKYPETEAMSMEETINGURL
// for the online-meeting join link, and STATUS:CANCELLED for cancelled
// meetings. These are display-only — the upload path never sends them back to
// Graph (see EventToGraphBody).
func EventToICal(e Event) ([]byte, error) {
	uid := e.ICalUID
	if uid == "" {
		uid = e.ID
	}
	stamp := e.LastModified
	if stamp.IsZero() {
		stamp = time.Now()
	}

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, stamp.UTC())
	if !e.LastModified.IsZero() {
		ev.Props.SetDateTime(ical.PropLastModified, e.LastModified.UTC())
	}
	if e.Subject != "" {
		ev.Props.SetText(ical.PropSummary, normalizeText(e.Subject))
	}
	if e.Location != "" {
		ev.Props.SetText(ical.PropLocation, normalizeText(e.Location))
	}
	if e.Description != "" {
		ev.Props.SetText(ical.PropDescription, normalizeText(e.Description))
	}
	if e.AllDay {
		ev.Props.SetDate(ical.PropDateTimeStart, e.Start.UTC())
		ev.Props.SetDate(ical.PropDateTimeEnd, e.End.UTC())
	} else {
		ev.Props.SetDateTime(ical.PropDateTimeStart, e.Start.UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, e.End.UTC())
	}
	if e.Recurrence != nil {
		opt, err := recurrenceToROption(*e.Recurrence)
		if err != nil {
			slog.Warn("Dropping unmappable recurrence from ICS", "module", "GRAPHCAL",
				"id", e.ID, "pattern", e.Recurrence.Pattern.Type, "err", err)
		} else {
			ev.Props.SetRecurrenceRule(opt)
		}
	}

	if e.Organizer != nil && e.Organizer.Email != "" {
		ev.Props.Set(calAddressProp(ical.PropOrganizer, e.Organizer.Name, e.Organizer.Email))
	}
	for _, a := range sortedAttendees(e.Attendees) {
		if a.Email == "" {
			continue
		}
		prop := calAddressProp(ical.PropAttendee, a.Name, a.Email)
		prop.Params.Set(ical.ParamRole, attendeeRole(a.Type))
		prop.Params.Set(ical.ParamParticipationStatus, attendeePartStat(a.Response))
		prop.Params.Set(ical.ParamRSVP, "TRUE")
		ev.Props.Add(prop)
	}
	if e.OnlineMeetingURL != "" {
		urlProp := ical.NewProp(ical.PropURL)
		urlProp.Value = e.OnlineMeetingURL
		ev.Props.Set(urlProp)
		// khal and other clients pick the Teams join link up from the
		// Microsoft X-prop as well.
		teamsProp := ical.NewProp(propTeamsMeetingURL)
		teamsProp.Value = e.OnlineMeetingURL
		ev.Props.Set(teamsProp)
	}
	if e.IsCancelled {
		ev.Props.SetText(ical.PropStatus, string(ical.EventCancelled))
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, icalProdID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, ev.Component)

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("failed to encode event %s as iCal: %w", e.ID, err)
	}
	return buf.Bytes(), nil
}

// normalizeText collapses CR/CRLF to LF; go-ical escapes LF itself but would
// reject a raw CR at encode time.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// calAddressProp builds a CAL-ADDRESS property (ORGANIZER/ATTENDEE): a
// mailto: URI value with an optional CN parameter.
func calAddressProp(name, cn, email string) *ical.Prop {
	prop := ical.NewProp(name)
	if cn != "" {
		prop.Params.Set(ical.ParamCommonName, sanitizeParamValue(cn))
	}
	prop.Value = "mailto:" + email
	return prop
}

// sanitizeParamValue makes a display name safe as an iCalendar parameter
// value: go-ical quotes values containing ";:," itself but rejects double
// quotes and CR/LF at encode time, so those are replaced.
func sanitizeParamValue(s string) string {
	s = strings.ReplaceAll(normalizeText(s), "\n", " ")
	return strings.ReplaceAll(s, `"`, "'")
}

// sortedAttendees returns a copy of the attendee list sorted by email, so the
// emitted ATTENDEE lines are deterministic regardless of Graph's ordering.
func sortedAttendees(attendees []Attendee) []Attendee {
	out := make([]Attendee, len(attendees))
	copy(out, attendees)
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// attendeeRole maps a Graph attendee type to an iCalendar ROLE.
func attendeeRole(attendeeType string) string {
	switch attendeeType {
	case "optional":
		return "OPT-PARTICIPANT"
	case "resource":
		return "NON-PARTICIPANT"
	default: // "required" and anything unknown
		return "REQ-PARTICIPANT"
	}
}

// attendeePartStat maps a Graph response status to an iCalendar PARTSTAT. The
// organizer implicitly attends, so "organizer" maps to ACCEPTED.
func attendeePartStat(response string) string {
	switch response {
	case "accepted", "organizer":
		return "ACCEPTED"
	case "declined":
		return "DECLINED"
	case "tentativelyAccepted":
		return "TENTATIVE"
	default: // "none", "notResponded" and anything unknown
		return "NEEDS-ACTION"
	}
}

// MARK: - Parse

// ICalToEvent parses a VCALENDAR containing a single VEVENT (the inverse of
// EventToICal) into an Event. A VALUE=DATE DTSTART marks the event all-day;
// all times are interpreted in UTC. An RRULE is mapped back into a Graph
// Recurrence; an RRULE outside the supported mapping leaves Recurrence nil
// with a warning. ID/ChangeKey/Type are not part of the iCal representation
// and stay empty.
//
// The read-only meeting metadata (ATTENDEE/ORGANIZER/URL/STATUS, see
// EventToICal) is deliberately NOT parsed back: the upload path ignores those
// fields anyway (Stage 1 never sends them to Graph), and several Graph enums
// collapse to one PARTSTAT ("none" and "notResponded" both map to
// NEEDS-ACTION), so a reparse could not reproduce the downloaded event
// faithfully. The zero values keep local edits from ever carrying meeting
// state.
func ICalToEvent(data []byte) (Event, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return Event{}, fmt.Errorf("failed to decode iCal: %w", err)
	}
	events := cal.Events()
	if len(events) == 0 {
		return Event{}, fmt.Errorf("failed to parse iCal: no VEVENT found")
	}
	if len(events) > 1 {
		return Event{}, fmt.Errorf("failed to parse iCal: expected one VEVENT, got %d", len(events))
	}
	ev := events[0]

	uid, err := ev.Props.Text(ical.PropUID)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal UID: %w", err)
	}
	subject, err := ev.Props.Text(ical.PropSummary)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal SUMMARY: %w", err)
	}
	location, err := ev.Props.Text(ical.PropLocation)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal LOCATION: %w", err)
	}
	description, err := ev.Props.Text(ical.PropDescription)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal DESCRIPTION: %w", err)
	}

	startProp := ev.Props.Get(ical.PropDateTimeStart)
	if startProp == nil {
		return Event{}, fmt.Errorf("failed to parse iCal: VEVENT has no DTSTART")
	}
	allDay := startProp.ValueType() == ical.ValueDate
	start, err := startProp.DateTime(time.UTC)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal DTSTART: %w", err)
	}
	// DateTimeEnd falls back to DTSTART+DURATION, or one day for DATE starts.
	end, err := ev.DateTimeEnd(time.UTC)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal DTEND: %w", err)
	}
	lastModified, err := ev.Props.DateTime(ical.PropLastModified, time.UTC)
	if err != nil {
		return Event{}, fmt.Errorf("failed to parse iCal LAST-MODIFIED: %w", err)
	}

	var recurrence *Recurrence
	if opt, err := ev.Props.RecurrenceRule(); err != nil {
		slog.Warn("Ignoring unparseable RRULE", "module", "GRAPHCAL", "uid", uid, "err", err)
	} else if opt != nil {
		recurrence, err = roptionToRecurrence(opt, start)
		if err != nil {
			slog.Warn("Ignoring unmappable RRULE", "module", "GRAPHCAL", "uid", uid, "err", err)
			recurrence = nil
		}
	}

	return Event{
		ICalUID:      uid,
		Subject:      subject,
		Location:     location,
		Description:  description,
		Start:        start.UTC(),
		End:          end.UTC(),
		AllDay:       allDay,
		LastModified: lastModified.UTC(),
		Recurrence:   recurrence,
	}, nil
}

// MARK: - Recurrence mapping

// graphDayToRRule maps Graph lowercase day names to rrule weekdays.
var graphDayToRRule = map[string]rrule.Weekday{
	"monday":    rrule.MO,
	"tuesday":   rrule.TU,
	"wednesday": rrule.WE,
	"thursday":  rrule.TH,
	"friday":    rrule.FR,
	"saturday":  rrule.SA,
	"sunday":    rrule.SU,
}

// rruleDayToGraph is the inverse of graphDayToRRule, keyed by
// rrule.Weekday.Day() (0 = Monday).
var rruleDayToGraph = [7]string{
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
}

// graphIndexToSetPos maps the Graph week ordinal of relative patterns to a
// BYSETPOS value.
var graphIndexToSetPos = map[string]int{
	"first":  1,
	"second": 2,
	"third":  3,
	"fourth": 4,
	"last":   -1,
}

// recurrenceToROption builds an rrule.ROption from a Graph recurrence per the
// mapping documented in the file header. It fails (instead of guessing) on
// pattern or range types outside that mapping.
func recurrenceToROption(rec Recurrence) (*rrule.ROption, error) {
	opt := &rrule.ROption{}
	if rec.Pattern.Interval > 1 {
		opt.Interval = rec.Pattern.Interval
	}

	days, err := graphDaysToRRule(rec.Pattern.DaysOfWeek)
	if err != nil {
		return nil, err
	}

	switch rec.Pattern.Type {
	case "daily":
		opt.Freq = rrule.DAILY
	case "weekly":
		opt.Freq = rrule.WEEKLY
		opt.Byweekday = days
	case "absoluteMonthly":
		opt.Freq = rrule.MONTHLY
		opt.Bymonthday = []int{rec.Pattern.DayOfMonth}
	case "relativeMonthly":
		opt.Freq = rrule.MONTHLY
		if err := setRelative(opt, days, rec.Pattern.Index); err != nil {
			return nil, err
		}
	case "absoluteYearly":
		opt.Freq = rrule.YEARLY
		opt.Bymonth = []int{rec.Pattern.Month}
		opt.Bymonthday = []int{rec.Pattern.DayOfMonth}
	case "relativeYearly":
		opt.Freq = rrule.YEARLY
		opt.Bymonth = []int{rec.Pattern.Month}
		if err := setRelative(opt, days, rec.Pattern.Index); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported recurrence pattern type %q", rec.Pattern.Type)
	}

	switch rec.Range.Type {
	case "endDate":
		endDate, err := time.ParseInLocation(graphDateFormat, rec.Range.EndDate, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("failed to parse recurrence end date %q: %w", rec.Range.EndDate, err)
		}
		// Graph's endDate is the inclusive last day; UNTIL is an inclusive
		// instant, so use the end of that day to cover timed occurrences.
		opt.Until = endDate.Add(24*time.Hour - time.Second)
	case "numbered":
		opt.Count = rec.Range.NumberOfOccurrences
	case "noEnd", "":
		// No UNTIL/COUNT.
	default:
		return nil, fmt.Errorf("unsupported recurrence range type %q", rec.Range.Type)
	}

	return opt, nil
}

// setRelative applies the nth-weekday part of a relativeMonthly/relativeYearly
// pattern as BYDAY + BYSETPOS.
func setRelative(opt *rrule.ROption, days []rrule.Weekday, index string) error {
	if len(days) == 0 {
		return fmt.Errorf("relative recurrence pattern without daysOfWeek")
	}
	setPos, ok := graphIndexToSetPos[strings.ToLower(index)]
	if !ok {
		return fmt.Errorf("unsupported recurrence index %q", index)
	}
	opt.Byweekday = days
	opt.Bysetpos = []int{setPos}
	return nil
}

// graphDaysToRRule converts Graph day names to rrule weekdays.
func graphDaysToRRule(days []string) ([]rrule.Weekday, error) {
	if len(days) == 0 {
		return nil, nil
	}
	out := make([]rrule.Weekday, 0, len(days))
	for _, d := range days {
		wd, ok := graphDayToRRule[strings.ToLower(d)]
		if !ok {
			return nil, fmt.Errorf("unsupported day of week %q", d)
		}
		out = append(out, wd)
	}
	return out, nil
}

// roptionToRecurrence maps a parsed RRULE back into a Graph recurrence — the
// inverse of recurrenceToROption. start (DTSTART) provides range.startDate,
// which Graph requires on the series master. RRULEs outside the supported
// mapping (other frequencies, multiple BYMONTHDAY/BYSETPOS values, nth-weekday
// BYDAY offsets beyond first..fourth/last, ...) yield an error; the caller
// treats that as "no recurrence" with a warning.
func roptionToRecurrence(opt *rrule.ROption, start time.Time) (*Recurrence, error) {
	rec := &Recurrence{}

	interval := opt.Interval
	if interval <= 0 {
		interval = 1
	}
	rec.Pattern.Interval = interval

	days, setPos, err := rruleDaysToGraph(opt.Byweekday, opt.Bysetpos)
	if err != nil {
		return nil, err
	}

	switch opt.Freq {
	case rrule.DAILY:
		rec.Pattern.Type = "daily"
	case rrule.WEEKLY:
		rec.Pattern.Type = "weekly"
		rec.Pattern.DaysOfWeek = days
	case rrule.MONTHLY:
		switch {
		case len(opt.Bymonthday) == 1 && len(days) == 0:
			rec.Pattern.Type = "absoluteMonthly"
			rec.Pattern.DayOfMonth = opt.Bymonthday[0]
		case len(opt.Bymonthday) == 0 && len(days) > 0 && setPos != 0:
			rec.Pattern.Type = "relativeMonthly"
			rec.Pattern.DaysOfWeek = days
			rec.Pattern.Index, err = setPosToGraphIndex(setPos)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported MONTHLY rrule shape (BYMONTHDAY=%v BYDAY=%v BYSETPOS=%v)",
				opt.Bymonthday, opt.Byweekday, opt.Bysetpos)
		}
	case rrule.YEARLY:
		if len(opt.Bymonth) != 1 {
			return nil, fmt.Errorf("unsupported YEARLY rrule without single BYMONTH (BYMONTH=%v)", opt.Bymonth)
		}
		rec.Pattern.Month = opt.Bymonth[0]
		switch {
		case len(opt.Bymonthday) == 1 && len(days) == 0:
			rec.Pattern.Type = "absoluteYearly"
			rec.Pattern.DayOfMonth = opt.Bymonthday[0]
		case len(opt.Bymonthday) == 0 && len(days) > 0 && setPos != 0:
			rec.Pattern.Type = "relativeYearly"
			rec.Pattern.DaysOfWeek = days
			rec.Pattern.Index, err = setPosToGraphIndex(setPos)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported YEARLY rrule shape (BYMONTHDAY=%v BYDAY=%v BYSETPOS=%v)",
				opt.Bymonthday, opt.Byweekday, opt.Bysetpos)
		}
	default:
		return nil, fmt.Errorf("unsupported rrule frequency %v", opt.Freq)
	}

	switch {
	case !opt.Until.IsZero() && opt.Count > 0:
		return nil, fmt.Errorf("unsupported rrule with both UNTIL and COUNT")
	case !opt.Until.IsZero():
		rec.Range.Type = "endDate"
		rec.Range.EndDate = opt.Until.UTC().Format(graphDateFormat)
	case opt.Count > 0:
		rec.Range.Type = "numbered"
		rec.Range.NumberOfOccurrences = opt.Count
	default:
		rec.Range.Type = "noEnd"
	}
	rec.Range.StartDate = start.UTC().Format(graphDateFormat)

	return rec, nil
}

// rruleDaysToGraph converts BYDAY entries back to Graph day names and derives
// the single BYSETPOS-style ordinal, accepting either an explicit BYSETPOS or
// an nth-weekday offset embedded in BYDAY (e.g. 2MO). setPos is 0 when the
// rule has no ordinal.
func rruleDaysToGraph(days []rrule.Weekday, bysetpos []int) ([]string, int, error) {
	if len(bysetpos) > 1 {
		return nil, 0, fmt.Errorf("unsupported rrule with multiple BYSETPOS values %v", bysetpos)
	}
	setPos := 0
	if len(bysetpos) == 1 {
		setPos = bysetpos[0]
	}

	var names []string
	for _, wd := range days {
		if n := wd.N(); n != 0 {
			if setPos != 0 && setPos != n {
				return nil, 0, fmt.Errorf("unsupported rrule with conflicting BYDAY offset %d and BYSETPOS %d", n, setPos)
			}
			setPos = n
		}
		names = append(names, rruleDayToGraph[wd.Day()])
	}
	return names, setPos, nil
}

// setPosToGraphIndex maps a BYSETPOS ordinal to the Graph index enum.
func setPosToGraphIndex(setPos int) (string, error) {
	for index, pos := range graphIndexToSetPos {
		if pos == setPos {
			return index, nil
		}
	}
	return "", fmt.Errorf("unsupported BYSETPOS value %d", setPos)
}
