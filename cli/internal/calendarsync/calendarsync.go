// Package calendarsync is the provider-neutral two-way calendar sync engine
// (vdirsyncer model): it diffs a remote calendar against a local vdir of .ics
// files and a persisted per-item status, plans the actions that converge both
// sides, and applies them — including the Stage-2 scheduling safety rails
// (create idempotency, no-clobber conditional writes, the never-re-invite
// refusal, the organizer role gate, scoped attendee updates, and the
// notification preview that mirrors exactly what Apply does).
//
// The engine talks to the remote calendar exclusively through the
// CalendarProvider interface; provider packages (e.g. the Microsoft Graph
// client in graphcalendar) implement it. The neutral event model and the
// local vdir layer live in the calendar package.
package calendarsync

import (
	"context"
	"errors"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
)

// MARK: - Provider error sentinels

var (
	// ErrPrecondition marks a conditional remote write rejected because the
	// remote item changed after it was last read (HTTP 412 / etag mismatch).
	// Providers must wrap their transport-level equivalent so the engine can
	// skip the action instead of clobbering (rail R2).
	ErrPrecondition = errors.New("remote precondition failed")
	// ErrNotFound marks a remote item that no longer exists (HTTP 404/410).
	// Providers must wrap their transport-level equivalent; the engine folds
	// it into success on deletes and declines (the goal — item absent — is
	// reached either way).
	ErrNotFound = errors.New("remote item not found")
)

// MARK: - Provider types

// Calendar is one remote calendar as the provider lists it.
type Calendar struct {
	ID   string
	Name string
	// HexColor is the calendar's "#RRGGBB" color, or "" when the calendar has
	// no explicit color.
	HexColor string
}

// Notification policy across the seam
//
// "Upload the attendee list" and "tell the attendees about it" are two
// different decisions, and providers disagree about whether they are coupled:
//
//   - Graph decides on its own. Any write to an event the owner organizes
//     with attendees mails them; the client cannot ask for silence, and does
//     not have to ask for mail either.
//   - Google decides from the sendUpdates query parameter, defaulting to
//     "none". Silence is what you get unless you ask.
//
// So the engine states its intent explicitly — CreateOptions/UpdateSpec carry
// NotifyAttendees, and DeleteEvent takes a notify argument — and each provider
// maps it onto its own model (Graph: ignore, and document that it does).
// Deriving the notification from IncludeAttendees instead would be silently
// wrong on Google for every write that changes the time or subject without
// touching the attendee set: nobody would be told the meeting moved.

// CreateOptions tunes one provider CreateEvent call. The engine computes
// every field; the provider serializes them into its wire format.
type CreateOptions struct {
	// IncludeAttendees uploads the event's attendee list. The engine sets it
	// only for meetings the account owner organizes (role gate); providers
	// must not add attendees on their own.
	IncludeAttendees bool
	// NotifyAttendees asks the provider to send invitations for this create.
	// See the notification policy note above.
	NotifyAttendees bool
	// RequestOnlineMeeting asks the provider to attach an online meeting
	// (e.g. Teams) to the created event — the local one-shot marker.
	RequestOnlineMeeting bool
	// IdempotencyKey is a client-generated unique key the provider should
	// pass through for server-side create deduplication (rail R1: a retried
	// create must never produce a duplicate event or a second invitation
	// wave). Providers without such a mechanism may ignore it.
	IdempotencyKey string
}

// UpdateSpec describes one provider UpdateEvent call. The engine computes
// every field; the provider chooses the wire shape.
type UpdateSpec struct {
	// Event is the full desired event content.
	Event calendar.Event
	// IncludeAttendees uploads the attendee list (role-gated by the engine,
	// see CreateOptions.IncludeAttendees).
	IncludeAttendees bool
	// NotifyAttendees asks the provider to tell the attendees about this
	// update. See the notification policy note above.
	NotifyAttendees bool
	// AttendeesOnly restricts the update to the attendee set: the engine sets
	// it when ONLY the attendees changed, so the provider can send a scoped
	// patch that reaches just the added/removed attendees.
	AttendeesOnly bool
	// ETag is the remote etag read at planning time, sent as the write
	// precondition; a mismatch must surface as ErrPrecondition.
	ETag string
}

// CalendarProvider is the remote side of the sync engine. The calendarID
// parameter on the per-event methods addresses the calendar the event lives
// in — providers with mailbox-global event ids may ignore it, providers with
// per-calendar ids need it.
type CalendarProvider interface {
	// Owner returns the account owner's email address, used to recognize the
	// owner's own attendee entry and to role-gate attendee uploads.
	Owner() string
	// ListCalendars returns all calendars of the account.
	ListCalendars(ctx context.Context) ([]Calendar, error)
	// FetchMasterEvents returns the master events of the calendar (single
	// events and series definitions, not expanded occurrences).
	FetchMasterEvents(ctx context.Context, calendarID string) ([]calendar.Event, error)
	// FetchInstances returns the concrete event instances within [from, to),
	// with recurring series expanded (the one-way Export path).
	FetchInstances(ctx context.Context, calendarID string, from, to time.Time) ([]calendar.Event, error)
	// GetEvent returns one event by its provider event id — the settled
	// server-side content the engine reads back after a write.
	GetEvent(ctx context.Context, calendarID, eventID string) (calendar.Event, error)
	// CreateEvent creates a new event and returns it as the provider rendered
	// it, including the server-assigned id, UID and etag.
	CreateEvent(ctx context.Context, calendarID string, ev calendar.Event, opts CreateOptions) (calendar.Event, error)
	// UpdateEvent updates an existing event per spec, conditional on
	// spec.ETag (ErrPrecondition on mismatch).
	UpdateEvent(ctx context.Context, calendarID, eventID string, spec UpdateSpec) error
	// DeleteEvent deletes an event, conditional on etag when non-empty
	// (ErrPrecondition on mismatch, ErrNotFound when already gone). With
	// notify the attendees receive the cancellation; see the notification
	// policy note above.
	DeleteEvent(ctx context.Context, calendarID, eventID, etag string, notify bool) error
	// RespondToEvent records the owner's RSVP for a meeting; with
	// sendResponse the organizer is notified (comment included when
	// non-empty). resp must be Accepted, Declined or Tentative.
	RespondToEvent(ctx context.Context, calendarID, eventID string, resp calendar.OwnerResp, sendResponse bool, comment string) error
	// IsAuthError reports whether err is an authentication/consent problem —
	// the engine aborts the run so the command can print the auth hint.
	IsAuthError(err error) bool
}

// MARK: - Neutral-model shorthand

// Aliases keeping the engine sources (moved here from the Graph package)
// textually close to their original form; the definitions live in the
// provider-neutral calendar package.

type (
	// Event is one calendar event; see calendar.Event.
	Event = calendar.Event
	// Attendee is one meeting attendee; see calendar.Attendee.
	Attendee = calendar.Attendee
	// OwnerResp is the canonical owner-RSVP state; see calendar.OwnerResp.
	OwnerResp = calendar.OwnerResp
	// Recurrence is the series definition; see calendar.Recurrence.
	Recurrence = calendar.Recurrence

	// localItem is one local .ics file found in the calendar dir.
	localItem = calendar.LocalItem
)

const (
	OwnerRespNone      = calendar.OwnerRespNone
	OwnerRespTentative = calendar.OwnerRespTentative
	OwnerRespAccepted  = calendar.OwnerRespAccepted
	OwnerRespDeclined  = calendar.OwnerRespDeclined
	OwnerRespOrganizer = calendar.OwnerRespOrganizer
)

// dateOnlyFormat is the "YYYY-MM-DD" layout of recurrence-range dates.
const dateOnlyFormat = calendar.GraphDateFormat

var (
	EventToICal = calendar.EventToICal
	EventToICS  = calendar.EventToICS

	// WriteFileAtomic is the shared temp+rename vdir writer; see calendar.
	WriteFileAtomic = calendar.WriteFileAtomic

	eventContentHash     = calendar.EventContentHash
	coreContentHash      = calendar.CoreContentHash
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
	scanLocalItems       = calendar.ScanLocalItems
	hashBytes            = calendar.HashBytes
)
