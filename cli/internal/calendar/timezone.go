package calendar

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // Calendar TZIDs must also work without a host zoneinfo installation.

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// CivilTime copies clock fields into a UTC container without converting them.
// The result represents a floating clock/date, not an instant.
func CivilTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func loadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if name == "Local" {
		return nil, fmt.Errorf("timezone must be an explicit IANA name, not Local")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unsupported timezone %q: %w", name, err)
	}
	return loc, nil
}

func (e Event) endZone() string {
	if e.EndTimeZone != "" {
		return e.EndTimeZone
	}
	return e.TimeZone
}

// ValidateTimeZones rejects ambiguous combinations instead of silently changing
// the meaning of a stored event. Unknown/custom VTIMEZONE rules are unsupported.
func (e Event) ValidateTimeZones() error {
	if e.AllDay && (e.Floating || e.TimeZone != "" || e.EndTimeZone != "") {
		return fmt.Errorf("all-day dates cannot have floating or timezone metadata")
	}
	if e.Floating && (e.TimeZone != "" || e.EndTimeZone != "") {
		return fmt.Errorf("floating times cannot have a timezone")
	}
	for _, zone := range []string{e.TimeZone, e.EndTimeZone} {
		if _, err := loadZone(zone); err != nil {
			return err
		}
	}
	return nil
}

// resolveCivil implements RFC 5545's first occurrence of a repeated clock time.
// valid=false marks a spring gap: recurrence generation must skip it WITHOUT
// consuming COUNT. For an explicit DATE-TIME, RFC 5545 instead uses the offset
// before the gap; that instant is returned alongside valid=false.
func resolveCivil(clock time.Time, loc *time.Location) (instant time.Time, valid bool) {
	clock = CivilTime(clock)
	guess := time.Date(clock.Year(), clock.Month(), clock.Day(), clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), loc)
	// Enumerate the offsets around this date using actual zone transitions,
	// including non-hour jumps and date-line moves (not a guessed DST offset).
	lo, hi := guess.Add(-48*time.Hour), guess.Add(48*time.Hour)
	var gap time.Time
	for probe := lo; !probe.After(hi); {
		_, offset := probe.In(loc).Zone()
		candidate := clock.Add(-time.Duration(offset) * time.Second).In(loc)
		wall := CivilTime(candidate)
		if wall.Equal(clock) && (!valid || candidate.Before(instant)) {
			instant, valid = candidate, true
		} else if wall.After(clock) && (gap.IsZero() || wall.Before(CivilTime(gap))) {
			gap = candidate
		}
		_, next := probe.In(loc).ZoneBounds()
		if next.IsZero() || !next.After(probe) || next.After(hi) {
			break
		}
		probe = next
	}
	if !valid {
		return gap, false
	}
	return instant, true
}

// timeProp retains floating clocks and zone identities. RFC local DATE-TIME
// cannot name the second fold; in that case use UTC plus a companion zone
// property on DTSTART/DTEND, so an untouched edit keeps the definite instant.
func timeProp(name string, t time.Time, allDay, floating bool, zone string) (*ical.Prop, error) {
	p := ical.NewProp(name)
	if allDay {
		p.SetDate(t.UTC())
	} else if floating {
		p.SetValueType(ical.ValueDateTime)
		p.Value = t.UTC().Format("20060102T150405")
	} else {
		loc, err := loadZone(zone)
		if err != nil {
			return nil, err
		}
		local := t.In(loc)
		first, _ := resolveCivil(local, loc)
		if !first.Equal(t) {
			local = t.UTC()
		}
		p.SetDateTime(local)
	}
	return p, nil
}

func readTimeProperty(p *ical.Prop) (t time.Time, zone string, floating bool, err error) {
	if p.ValueType() == ical.ValueDate {
		t, err = time.Parse("20060102", p.Value)
		return
	}
	zone = p.Params.Get(ical.ParamTimezoneID)
	if strings.HasSuffix(p.Value, "Z") {
		t, err = time.Parse("20060102T150405Z", p.Value)
		zone = ""
		return
	}
	floating = zone == ""
	loc, err := loadZone(zone)
	if err != nil {
		return time.Time{}, "", false, err
	}
	t, err = parseICalInstantIn(p.Value, loc)
	return
}

func recurrenceOptions(e Event) (*rrule.ROption, error) {
	loc, err := loadZone(e.TimeZone)
	if err != nil {
		return nil, err
	}
	if e.RecurrenceRule != "" {
		return rrule.StrToROptionInLocation(e.RecurrenceRule, loc)
	}
	if e.Recurrence == nil {
		return nil, fmt.Errorf("event has no recurrence")
	}
	rec := *e.Recurrence
	if rec.Range.TimeZone == "" {
		rec.Range.TimeZone = e.TimeZone
	}
	return RecurrenceToROption(rec)
}

func canonicalRule(e Event) string {
	if e.Recurrence == nil && e.RecurrenceRule == "" {
		return ""
	}
	opt, err := recurrenceOptions(e)
	if err != nil {
		return e.RecurrenceRule
	}
	if opt.Interval <= 1 {
		opt.Interval = 0
	}
	return opt.RRuleString()
}

func temporalContent(e Event) string {
	zone, endZone := e.TimeZone, e.endZone()
	if zone == "UTC" {
		zone = ""
	}
	if endZone == "UTC" {
		endZone = ""
	}
	rule := ""
	if e.RecurrenceRule != "" {
		structured := e
		structured.RecurrenceRule = ""
		if canonicalRule(e) != canonicalRule(structured) {
			rule = canonicalRule(e)
		}
	}
	if !e.Floating && zone == "" && endZone == "" && rule == "" {
		return ""
	}
	return fmt.Sprintf("time-v1|%s|%s|%t|%s", zone, endZone, e.Floating, rule)
}

// SameTimeSemantics compares the zone/kind and exact recurrence boundary, not
// the serialization form (a provider pattern versus the equivalent RRULE).
func SameTimeSemantics(a, b Event) bool {
	return temporalContent(a) == temporalContent(b) && canonicalRule(a) == canonicalRule(b)
}

func legacyTimeEvent(e Event) Event {
	if e.Recurrence != nil {
		copy := *e.Recurrence
		copy.Range.TimeZone = ""
		e.Recurrence = &copy
		// Google previously derived range dates from the UTC start/UNTIL.
		if e.RecurrenceRule != "" {
			if opt, err := rrule.StrToROption(e.RecurrenceRule); err == nil {
				if rec, err := ROptionToRecurrence(opt, e.Start.UTC()); err == nil {
					e.Recurrence = rec
				}
			}
		}
	}
	e.TimeZone, e.EndTimeZone, e.Floating, e.RecurrenceRule = "", "", false, ""
	overrides := make([]Event, len(e.Overrides))
	for i, o := range e.Overrides {
		overrides[i] = legacyTimeEvent(o)
	}
	e.Overrides = overrides
	return e
}

// RestoreLegacyTimeMetadata upgrades an old UTC-only local rendering without
// changing a user's pending edit to its instants/content. New explicit local
// TZIDs/floating clocks are never replaced. Called only for legacy sync state.
func RestoreLegacyTimeMetadata(local, remote Event) Event {
	if !local.AllDay && !local.Floating && local.TimeZone == "" && local.EndTimeZone == "" {
		local.TimeZone, local.EndTimeZone = remote.TimeZone, remote.EndTimeZone
		if local.Recurrence != nil && remote.Recurrence != nil {
			copy := *local.Recurrence
			copy.Range.TimeZone = remote.Recurrence.Range.TimeZone
			local.Recurrence = &copy
			// Restore exact provider rule only if the old local RRULE wasn't edited.
			old := legacyTimeEvent(remote)
			if canonicalRule(local) == canonicalRule(old) {
				local.RecurrenceRule = remote.RecurrenceRule
			}
		}
	}
	for i := range local.Overrides {
		for _, o := range remote.Overrides {
			if local.Overrides[i].RecurrenceID.Equal(o.RecurrenceID) {
				local.Overrides[i] = RestoreLegacyTimeMetadata(local.Overrides[i], o)
			}
		}
	}
	return local
}
