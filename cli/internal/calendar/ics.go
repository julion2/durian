package calendar

import (
	"strings"
	"time"
	"unicode/utf8"
)

// iCalendar (RFC 5545) time formats.
const (
	icsDateTimeUTC = "20060102T150405Z"
	icsDate        = "20060102"
)

// EventToICS renders one event as a complete VCALENDAR with a single VEVENT,
// RFC 5545-correct: CRLF line endings, text escaping, and folding of lines
// longer than 75 octets.
//
// Recurring events are exported as expanded instances (the calendarView
// endpoint pre-expands series), so no RRULE is ever emitted — the export
// window yields one .ics per occurrence instead.
func EventToICS(e Event, prodID string) string {
	uid := e.ICalUID
	if uid == "" {
		uid = e.ID
	}
	stamp := e.LastModified
	if stamp.IsZero() {
		stamp = time.Now()
	}

	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:" + prodID,
		"BEGIN:VEVENT",
		"UID:" + escapeText(uid),
		"DTSTAMP:" + stamp.UTC().Format(icsDateTimeUTC),
	}
	if !e.LastModified.IsZero() {
		lines = append(lines, "LAST-MODIFIED:"+e.LastModified.UTC().Format(icsDateTimeUTC))
	}
	if e.AllDay {
		// Graph all-day events are midnight boundaries with an exclusive end
		// date, which matches DTEND;VALUE=DATE semantics exactly.
		lines = append(lines,
			"DTSTART;VALUE=DATE:"+e.Start.UTC().Format(icsDate),
			"DTEND;VALUE=DATE:"+e.End.UTC().Format(icsDate))
	} else {
		lines = append(lines,
			"DTSTART:"+e.Start.UTC().Format(icsDateTimeUTC),
			"DTEND:"+e.End.UTC().Format(icsDateTimeUTC))
	}
	if e.Subject != "" {
		lines = append(lines, "SUMMARY:"+escapeText(e.Subject))
	}
	if e.Location != "" {
		lines = append(lines, "LOCATION:"+escapeText(e.Location))
	}
	if e.Description != "" {
		lines = append(lines, "DESCRIPTION:"+escapeText(e.Description))
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")

	var b strings.Builder
	for _, line := range lines {
		b.WriteString(foldLine(line))
		b.WriteString("\r\n")
	}
	return b.String()
}

// escapeText escapes a TEXT property value per RFC 5545 section 3.3.11:
// backslash, semicolon and comma are backslash-escaped, and newlines become
// the literal sequence \n.
func escapeText(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, ";", `\;`)
	s = strings.ReplaceAll(s, ",", `\,`)
	return s
}

// foldLine folds a content line longer than 75 octets per RFC 5545 section
// 3.1: continuation lines start with a single space and every physical line
// stays within 75 octets. Folds never split a UTF-8 rune.
func foldLine(line string) string {
	const (
		firstLimit = 75 // Octets on the first physical line
		contLimit  = 74 // Octets after the leading space on continuations
	)
	if len(line) <= firstLimit {
		return line
	}

	var b strings.Builder
	limit := firstLimit
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 {
			cut = limit // Defensive: never loop on pathological input
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		limit = contLimit
	}
	b.WriteString(line)
	return b.String()
}
