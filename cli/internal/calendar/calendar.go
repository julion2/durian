// Package calendar holds the provider-neutral calendar model and local vdir
// layer: the Event/Attendee/Person types, the content hashes the sync engine
// diffs with, the iCalendar round-trip (ical_roundtrip.go, ics.go), the
// offline vdir readers (read_local.go, local_scan.go) and the wire DTOs
// (dto.go). It talks to the filesystem only — never to a remote calendar API;
// provider clients (e.g. the Microsoft Graph client in graphcalendar) build
// on top of it.
package calendar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Event is one calendar event. Two fetch paths fill it:
//
//   - FetchEvents (calendarView) returns pre-expanded concrete instances, so
//     ID is unique per occurrence while ICalUID is shared across a series;
//     Type, ETag and Recurrence stay empty.
//   - FetchMasterEvents (/events) returns singleInstance and seriesMaster
//     events; seriesMaster carries the series definition in Recurrence, and
//     ETag/Type are populated for the two-way sync engine.
type Event struct {
	ID           string
	ICalUID      string
	Subject      string
	Location     string
	Description  string
	Start        time.Time
	End          time.Time
	AllDay       bool
	LastModified time.Time

	// ETag is the remote etag of the event (Graph calls it changeKey); it
	// changes on every remote modification.
	ETag string
	// Type is the Graph event type: "singleInstance", "seriesMaster",
	// "occurrence" or "exception". Empty for calendarView results.
	Type string
	// Recurrence is the series definition of a seriesMaster event; nil for
	// non-recurring events.
	Recurrence *Recurrence

	// Meeting metadata (Stage 2: read/write). Attendees and the owner's RSVP
	// are parsed on both read paths (Graph and local iCal) and — role-gated
	// on the owner being the organizer — uploaded again, so durian acts as a
	// full scheduling client. calendarView results leave the fields zero
	// (eventSelect does not request them).

	// Attendees is the meeting's attendee list; empty for plain appointments.
	Attendees []Attendee
	// Organizer is the meeting organizer; nil for plain appointments.
	Organizer *Person
	// IsOrganizer reports whether the account owner organizes this meeting.
	// Only set on the Graph read path; local files identify the organizer via
	// Organizer/owner email comparison instead.
	IsOrganizer bool
	// IsCancelled reports whether the meeting has been cancelled remotely.
	IsCancelled bool
	// IsOnlineMeeting reports whether the event is an online meeting.
	IsOnlineMeeting bool
	// OnlineMeetingURL is the join link (Teams etc.): onlineMeeting.joinUrl,
	// falling back to the legacy onlineMeetingUrl field. Empty when the event
	// is not an online meeting.
	OnlineMeetingURL string
	// OwnerResponse is the account owner's RSVP to this meeting, canonical
	// across both read paths (Graph responseStatus, iCal owner PARTSTAT).
	// Deliberately excluded from EventContentHash — an owner RSVP is handled
	// by the dedicated ActionRsvp three-way diff, never as a content change.
	OwnerResponse OwnerResp
	// RequestOnlineMeeting is the local-only X-DURIAN-CREATE-TEAMS-MEETING
	// marker: the create path requests a Teams online meeting for this event.
	// Excluded from every hash and never round-tripped back into the .ics.
	RequestOnlineMeeting bool
}

// OwnerResp is the canonical owner-RSVP state of an event, mapped from both
// Graph's responseStatus.response enum and iCal PARTSTAT values. The zero
// value OwnerRespNone means "not (yet) responded": Graph's "none" and
// "notResponded" and iCal's NEEDS-ACTION all collapse to it.
type OwnerResp string

const (
	OwnerRespNone      OwnerResp = ""
	OwnerRespTentative OwnerResp = "tentative"
	OwnerRespAccepted  OwnerResp = "accepted"
	OwnerRespDeclined  OwnerResp = "declined"
	OwnerRespOrganizer OwnerResp = "organizer"
)

// OwnerRespFromGraph maps Graph's responseStatus.response enum to the
// canonical OwnerResp.
func OwnerRespFromGraph(s string) OwnerResp {
	switch s {
	case "tentativelyAccepted":
		return OwnerRespTentative
	case "accepted":
		return OwnerRespAccepted
	case "declined":
		return OwnerRespDeclined
	case "organizer":
		return OwnerRespOrganizer
	default: // "none", "notResponded", "", unknown
		return OwnerRespNone
	}
}

// Attendee is one meeting attendee as Graph reports it. Type is the Graph
// attendee type: "required", "optional" or "resource". Response is the
// attendee's RSVP, same enum as Event.MyResponse.
type Attendee struct {
	Name     string
	Email    string
	Type     string
	Response string
}

// Person is a name/email pair, e.g. the meeting organizer.
type Person struct {
	Name  string
	Email string
}

// EventContentHash returns a SHA-256 hex digest over the MEANINGFUL content of
// an event — the fields a user actually edits — serialized deterministically.
// The two-way sync engine uses it as the remote-change signal: same content
// yields the same hash no matter which read path produced the Event
// (FetchMasterEvents, GetEvent, or a CreateEvent response).
//
// Volatile identity/bookkeeping fields are deliberately excluded: ETag is
// NOT a stable etag — Graph rewrites it between a write and subsequent reads
// (and over time) without any content change — and LastModified churns with
// it. ID, ICalUID and Type are identity/shape, not content. Fields are joined
// with NUL separators (no meaningful field contains NUL) so adjacent values
// can never be confused; Description line endings are normalized to LF, and a
// Recurrence is canonicalized via its fixed-field JSON encoding.
//
// The meeting metadata (attendees, organizer, cancellation, join link) is
// part of the hash too, so a server-side change — another attendee RSVPs, an
// attendee is added or removed, a meeting is cancelled, a join link changes —
// is detected as a remote change and re-downloaded. Attendees are
// canonicalized as a SORTED list of "email|type|response" entries so the hash
// does not depend on Graph's ordering.
//
// The OWNER'S own state is deliberately excluded: neither OwnerResponse nor
// the owner's own attendee entry (matched by ownerEmail, case-insensitive on
// the address) is hashed, so an owner RSVP — locally or in Outlook — never
// registers as a content change. Owner RSVPs are handled by the dedicated
// ActionRsvp three-way diff instead (see twosync.go).
func EventContentHash(e Event, ownerEmail string) string {
	return contentHash(e, ownerEmail, true)
}

// CoreContentHash is EventContentHash with the attendee RESPONSES excluded:
// each attendee contributes only "email|type", so another attendee accepting
// or declining does not move the hash, while every user-editable field —
// subject, times, all-day flag, location, description, recurrence, the
// attendee SET (adds/removes), organizer, online-meeting fields and
// cancellation — still does. The owner's own attendee entry stays excluded
// exactly like in EventContentHash.
//
// The two-way sync engine records it as the ItemStatus.CoreHash baseline and
// diffs both sides against it for the CONFLICT decision: attendee responses
// are orthogonal to the editable core, so a remote RSVP must refresh the
// local rendering (EventContentHash moves -> download) without ever turning a
// concurrent local core edit into a conflict.
func CoreContentHash(e Event, ownerEmail string) string {
	return contentHash(e, ownerEmail, false)
}

// contentHash is the shared deterministic serialization behind
// EventContentHash and CoreContentHash; withResponses controls whether an
// attendee entry carries the response ("email|type|response") or not
// ("email|type").
func contentHash(e Event, ownerEmail string, withResponses bool) string {
	attendees := make([]string, 0, len(e.Attendees))
	for _, a := range e.Attendees {
		if ownerEmail != "" && strings.EqualFold(a.Email, ownerEmail) {
			continue
		}
		entry := a.Email + "|" + a.Type
		if withResponses {
			entry += "|" + a.Response
		}
		attendees = append(attendees, entry)
	}
	sort.Strings(attendees)

	var organizerEmail string
	if e.Organizer != nil {
		organizerEmail = e.Organizer.Email
	}

	h := sha256.New()
	for _, field := range []string{
		e.Subject,
		e.Start.UTC().Format(time.RFC3339),
		e.End.UTC().Format(time.RFC3339),
		strconv.FormatBool(e.AllDay),
		e.Location,
		NormalizeText(e.Description),
		RecurrenceJSON(e.Recurrence, e.ID),
		strings.Join(attendees, "\n"),
		organizerEmail,
		strconv.FormatBool(e.IsOnlineMeeting),
		e.OnlineMeetingURL,
		strconv.FormatBool(e.IsCancelled),
	} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecurrenceJSON canonicalizes a Recurrence as its fixed-field JSON encoding
// ("" for nil), for hashing and equality checks. id is only used in the
// (unreachable for plain structs) marshal-failure warning.
func RecurrenceJSON(rec *Recurrence, id string) string {
	if rec == nil {
		return ""
	}
	data, err := json.Marshal(rec)
	if err != nil {
		// Keep a deterministic fallback rather than failing the hash.
		slog.Warn("Failed to marshal recurrence for content hash", "module", "GRAPHCAL",
			"id", id, "err", err)
		return fmt.Sprintf("%+v", *rec)
	}
	return string(data)
}

// AttendeeSetHash returns a SHA-256 hex digest of the attendee SET — sorted
// "email|type" entries, responses excluded — used as the ItemStatus baseline
// that detects attendee adds/removes as real scheduling changes. The digest
// of an empty list is still a non-empty string, so "" can mean "unknown
// baseline" (pre-Stage-2 status files).
func AttendeeSetHash(attendees []Attendee) string {
	entries := make([]string, 0, len(attendees))
	for _, a := range attendees {
		entries = append(entries, strings.ToLower(a.Email)+"|"+a.Type)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(sum[:])
}

// OwnerIsOrganizer reports whether the account owner organizes this remote
// event: Graph's IsOrganizer flag, or the organizer email matching the owner.
func OwnerIsOrganizer(e Event, owner string) bool {
	if e.IsOrganizer {
		return true
	}
	return e.Organizer != nil && owner != "" && strings.EqualFold(e.Organizer.Email, owner)
}

// CountRecipients counts the attendees other than the owner — the recipients
// of an invitation/update/cancellation for this event.
func CountRecipients(attendees []Attendee, owner string) int {
	n := 0
	for _, a := range attendees {
		if owner != "" && strings.EqualFold(a.Email, owner) {
			continue
		}
		n++
	}
	return n
}

// Recurrence mirrors the Graph patternedRecurrence resource: how a series
// repeats (Pattern) and over which span (Range).
type Recurrence struct {
	Pattern RecurrencePattern `json:"pattern"`
	Range   RecurrenceRange   `json:"range"`
}

// RecurrencePattern is the Graph recurrencePattern resource. Type is one of
// daily, weekly, absoluteMonthly, relativeMonthly, absoluteYearly,
// relativeYearly.
type RecurrencePattern struct {
	Type     string `json:"type"`
	Interval int    `json:"interval"`
	// DaysOfWeek holds lowercase day names ("monday", ...) for weekly and
	// relative patterns.
	DaysOfWeek []string `json:"daysOfWeek,omitempty"`
	// DayOfMonth applies to absoluteMonthly/absoluteYearly patterns.
	DayOfMonth int `json:"dayOfMonth,omitempty"`
	// Month (1-12) applies to yearly patterns.
	Month int `json:"month,omitempty"`
	// Index is the week ordinal for relative patterns: first, second, third,
	// fourth or last.
	Index          string `json:"index,omitempty"`
	FirstDayOfWeek string `json:"firstDayOfWeek,omitempty"`
}

// RecurrenceRange is the Graph recurrenceRange resource. Type is one of
// noEnd, endDate, numbered. Dates are "YYYY-MM-DD" strings as Graph sends
// them.
type RecurrenceRange struct {
	Type                string `json:"type"`
	StartDate           string `json:"startDate,omitempty"`
	EndDate             string `json:"endDate,omitempty"`
	NumberOfOccurrences int    `json:"numberOfOccurrences,omitempty"`
}

// SanitizeName makes a calendar name or event id safe as a single filesystem
// path component: path separators and other unsafe characters become '_', and
// leading dots are stripped so no hidden files appear.

// WriteFileAtomic writes data to path via a temporary sibling and a rename,
// so a reader never observes a half-written file.
//
// The vdir is a shared surface: the GUI reads it through the HTTP API, the
// sync engine rewrites it in the background, and khal or vdirsyncer may walk
// it at any moment. A plain os.WriteFile truncates first and fills after, so
// every write opens a window in which a concurrent reader parses a truncated
// .ics — which the planner cannot distinguish from a corrupt file. rename(2)
// is atomic within a directory, so the file is either the old content or the
// new one.
//
// The temp file is created in the same directory (a rename across filesystems
// would fail) and carries the ".ics-tmp" suffix rather than ".ics", so the
// local scan ignores it even if a crash leaves one behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.ics-tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// SanitizeName makes a calendar name or event id safe as a single filesystem
// path component: path separators and other unsafe characters become '_', and
// leading dots are stripped so no hidden files appear.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" {
		return "unnamed"
	}
	return out
}

// CalendarIncluded reports whether a calendar display name passes the include
// filter: an empty filter admits every calendar, otherwise the name must match
// one entry case-insensitively.
func CalendarIncluded(name string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	for _, want := range include {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}
