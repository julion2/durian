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

package calendar

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

// propCreateTeamsMeeting is the durian marker property a user adds to a local
// .ics to request a Teams online meeting on CREATE. It is honored one-shot:
// EventToICal never re-emits it, so the post-create rewrite of the local file
// drops it and the next sync is a no-op.
const propCreateTeamsMeeting = "X-DURIAN-CREATE-TEAMS-MEETING"

// propOpaqueRecurrence marks an event whose remote recurrence rule durian
// could not map (see Event.OpaqueRecurrence). It has to survive the .ics
// round-trip: the upload path reads it back off the local file, and without it
// a user editing that file would silently clear the series remotely.
const propOpaqueRecurrence = "X-DURIAN-OPAQUE-RECURRENCE"

// GraphDateFormat is the "YYYY-MM-DD" layout of Graph recurrenceRange dates.
const GraphDateFormat = "2006-01-02"

// MARK: - Serialize

// EventToICal renders one event as a VCALENDAR with a single VEVENT using
// go-ical (CRLF output). Timed events are written as UTC date-times, all-day
// events as VALUE=DATE with Graph's exclusive DTEND kept as-is. A seriesMaster
// Recurrence becomes an RRULE per the mapping documented in the file header;
// an unmappable recurrence is dropped with a warning so the event itself still
// round-trips.
//
// Meeting metadata is surfaced as standard properties: ORGANIZER (with CN),
// one ATTENDEE per attendee (CN/ROLE/PARTSTAT/RSVP, sorted by email so the
// output is deterministic), URL plus X-MICROSOFT-SKYPETEAMSMEETINGURL for the
// online-meeting join link, and STATUS:CANCELLED for cancelled meetings.
// ATTENDEE and ORGANIZER round-trip through ICalToEvent; URL and STATUS are
// display-only. The X-DURIAN-CREATE-TEAMS-MEETING marker is emitted only for a
// still-pending request (RequestOnlineMeeting), so `calendar new --teams`
// reaches the first sync; it is one-shot because the post-create read-back
// rewrites the file from the settled remote event, which carries no marker.
func EventToICal(e Event) ([]byte, error) {
	uid := e.ICalUID
	if uid == "" {
		uid = e.ID
	}

	master, err := eventComponent(e, uid)
	if err != nil {
		return nil, err
	}
	// A cancelled occurrence is a date the RRULE still produces but the series
	// no longer has. One EXDATE line per instant (rather than one comma-joined
	// line) keeps the output stable under reordering and is what go-ical's own
	// rule-set reader expects.
	for _, d := range sortedTimes(e.ExceptionDates) {
		exdate := ical.NewProp(ical.PropExceptionDates)
		if e.AllDay {
			exdate.SetValueType(ical.ValueDate)
			exdate.Value = d.UTC().Format("20060102")
		} else {
			exdate.SetValueType(ical.ValueDateTime)
			exdate.Value = d.UTC().Format("20060102T150405Z")
		}
		master.Props.Add(exdate)
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, icalProdID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Children = append(cal.Children, master.Component)

	// Modified occurrences follow the master as sibling VEVENTs sharing its
	// UID, each pinned to the occurrence it replaces by RECURRENCE-ID. Order
	// is by RecurrenceID so the file bytes — and therefore the LocalHash the
	// sync engine diffs on — do not depend on provider ordering.
	for _, o := range sortedOverrides(e.Overrides) {
		if o.RecurrenceID.IsZero() {
			slog.Warn("Dropping series override without recurrence id", "module", "CALENDAR",
				"uid", uid, "subject", o.Subject)
			continue
		}
		comp, err := eventComponent(o, uid)
		if err != nil {
			return nil, err
		}
		recID := ical.NewProp(ical.PropRecurrenceID)
		if o.AllDay {
			recID.SetValueType(ical.ValueDate)
			recID.Value = o.RecurrenceID.UTC().Format("20060102")
		} else {
			recID.SetValueType(ical.ValueDateTime)
			recID.Value = o.RecurrenceID.UTC().Format("20060102T150405Z")
		}
		comp.Props.Set(recID)
		cal.Children = append(cal.Children, comp.Component)
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("failed to encode event %s as iCal: %w", e.ID, err)
	}
	return buf.Bytes(), nil
}

// sortedTimes returns a copy of ts sorted ascending.
func sortedTimes(ts []time.Time) []time.Time {
	out := make([]time.Time, len(ts))
	copy(out, ts)
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// sortedOverrides returns a copy of overrides sorted by RecurrenceID.
func sortedOverrides(overrides []Event) []Event {
	out := make([]Event, len(overrides))
	copy(out, overrides)
	sort.Slice(out, func(i, j int) bool { return out[i].RecurrenceID.Before(out[j].RecurrenceID) })
	return out
}

// eventComponent renders one Event as a VEVENT under the given UID — the body
// shared by the series master and each of its overrides. It writes everything
// except the series-level EXDATE lines and the override-level RECURRENCE-ID,
// which EventToICal adds around it.
func eventComponent(e Event, uid string) (*ical.Event, error) {
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
		ev.Props.SetText(ical.PropSummary, NormalizeText(e.Subject))
	}
	if e.Location != "" {
		ev.Props.SetText(ical.PropLocation, NormalizeText(e.Location))
	}
	if e.Description != "" {
		ev.Props.SetText(ical.PropDescription, NormalizeText(e.Description))
	}
	if e.AllDay {
		ev.Props.SetDate(ical.PropDateTimeStart, e.Start.UTC())
		ev.Props.SetDate(ical.PropDateTimeEnd, e.End.UTC())
	} else {
		ev.Props.SetDateTime(ical.PropDateTimeStart, e.Start.UTC())
		ev.Props.SetDateTime(ical.PropDateTimeEnd, e.End.UTC())
	}
	// A recurrence that cannot be rendered as an RRULE leaves the file without
	// one, and a file without an RRULE parses back as "not a series" — which
	// on the next upload would clear the series remotely. Carry the marker so
	// the write paths know the difference between "no rule" and "a rule this
	// document could not hold".
	opaque := e.OpaqueRecurrence
	if e.Recurrence != nil {
		opt, err := RecurrenceToROption(*e.Recurrence)
		if err != nil {
			slog.Warn("Recurrence not representable as RRULE, marking event opaque",
				"module", "CALENDAR", "id", e.ID, "pattern", e.Recurrence.Pattern.Type, "err", err)
			opaque = true
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
		prop.Params.Set(ical.ParamRole, AttendeeRole(a.Type))
		prop.Params.Set(ical.ParamParticipationStatus, AttendeePartStat(a.Response))
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
	// A pending "create as online meeting" request carries the marker so the
	// first sync's create picks it up. It is one-shot in practice: after a
	// successful create the engine rewrites the local file from the settled
	// remote event, which has no marker (only the resolved join URL).
	if e.RequestOnlineMeeting {
		marker := ical.NewProp(propCreateTeamsMeeting)
		marker.Value = "TRUE"
		ev.Props.Set(marker)
	}
	if opaque {
		marker := ical.NewProp(propOpaqueRecurrence)
		marker.Value = "TRUE"
		ev.Props.Set(marker)
	}
	if e.IsCancelled {
		ev.Props.SetText(ical.PropStatus, string(ical.EventCancelled))
	}

	return ev, nil
}

// NormalizeText collapses CR/CRLF to LF; go-ical escapes LF itself but would
// reject a raw CR at encode time.
func NormalizeText(s string) string {
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
	s = strings.ReplaceAll(NormalizeText(s), "\n", " ")
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

// AttendeeRole maps a Graph attendee type to an iCalendar ROLE.
func AttendeeRole(attendeeType string) string {
	switch attendeeType {
	case "optional":
		return "OPT-PARTICIPANT"
	case "resource":
		return "NON-PARTICIPANT"
	default: // "required" and anything unknown
		return "REQ-PARTICIPANT"
	}
}

// AttendeePartStat maps a Graph response status to an iCalendar PARTSTAT. The
// organizer implicitly attends, so "organizer" maps to ACCEPTED.
func AttendeePartStat(response string) string {
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

// ICalToEvent parses a VCALENDAR (the inverse of EventToICal) into an Event. A
// VALUE=DATE DTSTART marks the event all-day; all times are interpreted in
// UTC. An RRULE is mapped back into a Graph Recurrence; an RRULE outside the
// supported mapping leaves Recurrence nil with a warning. ID/ETag/Type are not
// part of the iCal representation and stay empty.
//
// A recurring series is one document with several VEVENTs sharing the UID: the
// master (no RECURRENCE-ID) plus one per modified occurrence. The master's
// EXDATE lines become ExceptionDates, the RECURRENCE-ID siblings become
// Overrides — so the whole series stays a single Event, a single .ics file and
// a single ItemStatus. A document with only overrides and no master is
// rejected: without the master there is no series definition to hang them on,
// and treating the first override as the master would silently rewrite the
// series to that one occurrence on the next upload.
//
// Meeting metadata is parsed back: every ATTENDEE line becomes an Attendee
// (name from CN, type from ROLE, response from PARTSTAT — lossy where Graph
// enums collapse: "none"/"notResponded" both render as NEEDS-ACTION and parse
// back as "none") and ORGANIZER becomes Organizer. accountEmail identifies
// the owner: OwnerResponse comes from the owner's own ATTENDEE line (matched
// case-insensitively on the address), or OwnerRespOrganizer when the
// ORGANIZER is the owner. The online-meeting join link is parsed from URL (or
// the Teams X-prop) into OnlineMeetingURL/IsOnlineMeeting for DISPLAY ONLY (it
// never feeds sync change detection and is never uploaded); STATUS is not
// parsed back. The X-DURIAN-CREATE-TEAMS-MEETING:TRUE marker sets
// RequestOnlineMeeting (honored on create only, see createFromLocal).
func ICalToEvent(data []byte, accountEmail string) (Event, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return Event{}, fmt.Errorf("failed to decode iCal: %w", err)
	}
	events := cal.Events()
	if len(events) == 0 {
		return Event{}, fmt.Errorf("failed to parse iCal: no VEVENT found")
	}

	var master *ical.Event
	var overrideComps []*ical.Event
	for i := range events {
		if events[i].Props.Get(ical.PropRecurrenceID) != nil {
			overrideComps = append(overrideComps, &events[i])
			continue
		}
		if master != nil {
			return Event{}, fmt.Errorf("failed to parse iCal: expected one VEVENT without RECURRENCE-ID, got several")
		}
		master = &events[i]
	}
	if master == nil {
		return Event{}, fmt.Errorf("failed to parse iCal: %d VEVENT(s), all with RECURRENCE-ID, no series master", len(events))
	}

	event, err := eventFromComponent(master, accountEmail)
	if err != nil {
		return Event{}, err
	}
	for _, prop := range master.Props.Values(ical.PropExceptionDates) {
		for _, instant := range strings.Split(prop.Value, ",") {
			t, err := parseICalInstant(strings.TrimSpace(instant))
			if err != nil {
				slog.Warn("Ignoring unparseable EXDATE", "module", "CALENDAR",
					"uid", event.ICalUID, "value", instant, "err", err)
				continue
			}
			event.ExceptionDates = append(event.ExceptionDates, t)
		}
	}
	for _, comp := range overrideComps {
		override, err := eventFromComponent(comp, accountEmail)
		if err != nil {
			return Event{}, err
		}
		prop := comp.Props.Get(ical.PropRecurrenceID)
		recID, err := parseICalInstant(strings.TrimSpace(prop.Value))
		if err != nil {
			slog.Warn("Ignoring override with unparseable RECURRENCE-ID", "module", "CALENDAR",
				"uid", event.ICalUID, "value", prop.Value, "err", err)
			continue
		}
		override.RecurrenceID = recID
		event.Overrides = append(event.Overrides, override)
	}
	event.ExceptionDates = sortedTimes(event.ExceptionDates)
	event.Overrides = sortedOverrides(event.Overrides)
	return event, nil
}

// parseICalInstant parses an EXDATE/RECURRENCE-ID value in either the DATE
// ("20260817") or the UTC DATE-TIME ("20260817T090000Z") form. A local
// DATE-TIME without the trailing Z is read as UTC, matching how every other
// timestamp in this package is interpreted.
func parseICalInstant(s string) (time.Time, error) {
	return parseICalInstantIn(s, time.UTC)
}

// parseICalInstantIn is parseICalInstant with an explicit location for the
// floating (no trailing Z) DATE-TIME form, so a TZID-qualified value can be
// resolved in the zone its property named.
func parseICalInstantIn(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.ParseInLocation("20060102T150405Z", s, time.UTC); err == nil {
		return t.UTC(), nil
	}
	for _, layout := range []string{"20060102T150405", "20060102"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized iCalendar instant %q", s)
}

// ParseExDateLine parses a raw EXDATE content line as the Google Calendar API
// hands it out in an event's recurrence array — "EXDATE;TZID=Europe/Zurich:
// 20260817T090000", "EXDATE;VALUE=DATE:20260817" or a comma-separated list of
// either — into the UTC instants it names.
//
// The TZID parameter is honored because Google emits the exception dates in
// the series' own zone while the rest of durian's model is UTC: reading
// "20260817T090000" as UTC would place the exception an offset away from the
// occurrence it is meant to cancel, and the cancellation would silently miss.
// An unknown zone falls back to UTC with a warning rather than dropping the
// line — a mistimed exception is recoverable, a lost one is not.
func ParseExDateLine(line string) ([]time.Time, error) {
	_, params, value, ok := splitContentLine(line)
	if !ok {
		return nil, fmt.Errorf("malformed EXDATE line %q", line)
	}

	loc := time.UTC
	if tzid := params["TZID"]; tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc = l
		} else {
			slog.Warn("Unknown EXDATE TZID, reading the value as UTC", "module", "CALENDAR",
				"tzid", tzid, "err", err)
		}
	}

	var out []time.Time
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		t, err := parseICalInstantIn(raw, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("EXDATE line %q names no date", line)
	}
	return out, nil
}

// splitContentLine splits an iCalendar content line into its property name,
// its parameters (upper-cased keys, unquoted values) and its value. It cuts at
// the FIRST colon that is not inside a quoted parameter value, since a quoted
// parameter may legally contain one.
func splitContentLine(line string) (name string, params map[string]string, value string, ok bool) {
	inQuotes := false
	colon := -1
	for i, r := range line {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ':' && !inQuotes:
			colon = i
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return "", nil, "", false
	}

	head, value := line[:colon], line[colon+1:]
	parts := strings.Split(head, ";")
	params = make(map[string]string, len(parts)-1)
	for _, p := range parts[1:] {
		key, val, found := strings.Cut(p, "=")
		if !found {
			continue
		}
		params[strings.ToUpper(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return strings.ToUpper(strings.TrimSpace(parts[0])), params, value, true
}

// eventFromComponent parses one VEVENT into an Event — the body shared by the
// series master and each override. Series-level EXDATE and the override-level
// RECURRENCE-ID are handled by ICalToEvent around it.
func eventFromComponent(ev *ical.Event, accountEmail string) (Event, error) {
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
		recurrence, err = ROptionToRecurrence(opt, start)
		if err != nil {
			slog.Warn("Ignoring unmappable RRULE", "module", "GRAPHCAL", "uid", uid, "err", err)
			recurrence = nil
		}
	}

	var attendees []Attendee
	for _, prop := range ev.Props[ical.PropAttendee] {
		address := mailtoAddress(prop.Value)
		if address == "" {
			slog.Warn("Ignoring ATTENDEE without mailto address", "module", "GRAPHCAL", "uid", uid)
			continue
		}
		attendees = append(attendees, Attendee{
			Name:     prop.Params.Get(ical.ParamCommonName),
			Email:    address,
			Type:     AttendeeTypeFromRole(prop.Params.Get(ical.ParamRole)),
			Response: ResponseFromPartStat(prop.Params.Get(ical.ParamParticipationStatus)),
		})
	}
	var organizer *Person
	if prop := ev.Props.Get(ical.PropOrganizer); prop != nil {
		if address := mailtoAddress(prop.Value); address != "" {
			organizer = &Person{Name: prop.Params.Get(ical.ParamCommonName), Email: address}
		}
	}
	ownerResponse := OwnerRespNone
	for _, a := range attendees {
		if accountEmail != "" && strings.EqualFold(a.Email, accountEmail) {
			ownerResponse = OwnerRespFromGraph(a.Response)
		}
	}
	if organizer != nil && accountEmail != "" && strings.EqualFold(organizer.Email, accountEmail) {
		ownerResponse = OwnerRespOrganizer
	}

	requestOnlineMeeting := false
	if prop := ev.Props.Get(propCreateTeamsMeeting); prop != nil &&
		strings.EqualFold(strings.TrimSpace(prop.Value), "TRUE") {
		requestOnlineMeeting = true
	}
	opaqueRecurrence := false
	if prop := ev.Props.Get(propOpaqueRecurrence); prop != nil &&
		strings.EqualFold(strings.TrimSpace(prop.Value), "TRUE") {
		opaqueRecurrence = true
	}

	// Online-meeting join link, DISPLAY-ONLY: recovered from URL (falling back
	// to the Teams X-prop) purely so list/show can surface it. It never affects
	// sync change detection — that runs on the file-byte LocalHash and the
	// remote-event RemoteHash, coreContentEqual/localEventMatchesRemote exclude
	// online-meeting fields, and in the CoreHash conflict baseline a link that
	// fails to round-trip can only read as "core changed", i.e. keep the
	// conservative conflict classification — and EventToGraphBody never
	// uploads it.
	onlineMeetingURL := ""
	if prop := ev.Props.Get(ical.PropURL); prop != nil {
		onlineMeetingURL = strings.TrimSpace(prop.Value)
	}
	if onlineMeetingURL == "" {
		if prop := ev.Props.Get(propTeamsMeetingURL); prop != nil {
			onlineMeetingURL = strings.TrimSpace(prop.Value)
		}
	}

	// STATUS:CANCELLED round-trips: EventToICal writes it for a cancelled
	// meeting, so the parse has to read it back. Without this the flag is
	// write-only — every editor that re-serializes a parsed event (the GUI
	// write handler, the RSVP path) silently drops the cancellation, which
	// registers as a local content change and makes the next sync patch the
	// cancelled meeting back to life.
	isCancelled := false
	if prop := ev.Props.Get(ical.PropStatus); prop != nil {
		isCancelled = strings.EqualFold(strings.TrimSpace(prop.Value), string(ical.EventCancelled))
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

		OpaqueRecurrence:     opaqueRecurrence,
		Attendees:            attendees,
		Organizer:            organizer,
		OwnerResponse:        ownerResponse,
		IsOnlineMeeting:      onlineMeetingURL != "",
		OnlineMeetingURL:     onlineMeetingURL,
		RequestOnlineMeeting: requestOnlineMeeting,
		IsCancelled:          isCancelled,
	}, nil
}

// mailtoAddress extracts the email address from a CAL-ADDRESS value
// ("mailto:user@host", scheme case-insensitive); "" when it is no mailto URI.
func mailtoAddress(value string) string {
	const scheme = "mailto:"
	if len(value) <= len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return ""
	}
	return value[len(scheme):]
}

// AttendeeTypeFromRole maps an iCalendar ROLE back to the Graph attendee
// type — the inverse of AttendeeRole.
func AttendeeTypeFromRole(role string) string {
	switch strings.ToUpper(role) {
	case "OPT-PARTICIPANT":
		return "optional"
	case "NON-PARTICIPANT":
		return "resource"
	default: // REQ-PARTICIPANT, CHAIR, absent, unknown
		return "required"
	}
}

// ResponseFromPartStat maps an iCalendar PARTSTAT back to the Graph response
// enum — the (lossy) inverse of AttendeePartStat: NEEDS-ACTION covers both
// "none" and "notResponded" and parses back as "none"; "organizer" rendered
// as ACCEPTED parses back as "accepted".
func ResponseFromPartStat(partStat string) string {
	switch strings.ToUpper(partStat) {
	case "ACCEPTED":
		return "accepted"
	case "DECLINED":
		return "declined"
	case "TENTATIVE":
		return "tentativelyAccepted"
	default: // NEEDS-ACTION, absent, unknown
		return "none"
	}
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

// RecurrenceToROption builds an rrule.ROption from a Graph recurrence per the
// mapping documented in the file header. It fails (instead of guessing) on
// pattern or range types outside that mapping.
func RecurrenceToROption(rec Recurrence) (*rrule.ROption, error) {
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
		endDate, err := time.ParseInLocation(GraphDateFormat, rec.Range.EndDate, time.UTC)
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

// ROptionToRecurrence maps a parsed RRULE back into a Graph recurrence — the
// inverse of RecurrenceToROption. start (DTSTART) provides range.startDate,
// which Graph requires on the series master. RRULEs outside the supported
// mapping (other frequencies, multiple BYMONTHDAY/BYSETPOS values, nth-weekday
// BYDAY offsets beyond first..fourth/last, ...) yield an error; the caller
// treats that as "no recurrence" with a warning.
func ROptionToRecurrence(opt *rrule.ROption, start time.Time) (*Recurrence, error) {
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
		rec.Range.EndDate = opt.Until.UTC().Format(GraphDateFormat)
	case opt.Count > 0:
		rec.Range.Type = "numbered"
		rec.Range.NumberOfOccurrences = opt.Count
	default:
		rec.Range.Type = "noEnd"
	}
	rec.Range.StartDate = start.UTC().Format(GraphDateFormat)

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
