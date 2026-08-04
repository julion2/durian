package calendar

import (
	"testing"
	"time"
)

// cloneAttendees deep-copies an attendee list so per-case mutations cannot
// leak through the shared backing array.
func cloneAttendees(attendees []Attendee) []Attendee {
	out := make([]Attendee, len(attendees))
	copy(out, attendees)
	return out
}

func TestCoreContentHashIgnoresAttendeeResponses(t *testing.T) {
	const owner = "me@example.com"
	base := Event{
		Subject:     "Design review",
		Location:    "Teams",
		Description: "please review the drafts",
		Start:       time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC),
		Attendees: []Attendee{
			{Name: "Me", Email: "me@example.com", Type: "required", Response: "accepted"},
			{Name: "Alice", Email: "alice@example.com", Type: "required", Response: "none"},
			{Name: "Bob", Email: "bob@example.com", Type: "optional", Response: "declined"},
		},
		Organizer: &Person{Name: "Org", Email: "organizer@example.com"},
	}

	// An attendee RESPONSE change must not move the core hash — but it must
	// still move the full content hash, which drives the download refresh.
	responded := base
	responded.Attendees = cloneAttendees(base.Attendees)
	responded.Attendees[1].Response = "accepted" // Alice: none -> accepted
	if CoreContentHash(responded, owner) != CoreContentHash(base, owner) {
		t.Error("core hash must ignore attendee response changes")
	}
	if EventContentHash(responded, owner) == EventContentHash(base, owner) {
		t.Error("full content hash must still detect attendee response changes")
	}

	// The attendee SET is core: an add, a remove, or a type change moves it.
	added := base
	added.Attendees = append(cloneAttendees(base.Attendees),
		Attendee{Name: "Carol", Email: "carol@example.com", Type: "required", Response: "none"})
	if CoreContentHash(added, owner) == CoreContentHash(base, owner) {
		t.Error("core hash must detect an added attendee")
	}
	removed := base
	removed.Attendees = cloneAttendees(base.Attendees[:2])
	if CoreContentHash(removed, owner) == CoreContentHash(base, owner) {
		t.Error("core hash must detect a removed attendee")
	}
	retyped := base
	retyped.Attendees = cloneAttendees(base.Attendees)
	retyped.Attendees[2].Type = "required" // Bob: optional -> required
	if CoreContentHash(retyped, owner) == CoreContentHash(base, owner) {
		t.Error("core hash must detect an attendee type change")
	}

	// Every user-editable core field moves the hash.
	for name, mutate := range map[string]func(*Event){
		"Subject":  func(e *Event) { e.Subject = "Design review v2" },
		"Start":    func(e *Event) { e.Start = e.Start.Add(time.Hour) },
		"End":      func(e *Event) { e.End = e.End.Add(time.Hour) },
		"AllDay":   func(e *Event) { e.AllDay = true },
		"Location": func(e *Event) { e.Location = "Elsewhere" },
		"Description": func(e *Event) {
			e.Description = "new agenda"
		},
		"Recurrence": func(e *Event) {
			e.Recurrence = &Recurrence{
				Pattern: RecurrencePattern{Type: "daily", Interval: 1},
				Range:   RecurrenceRange{Type: "noEnd", StartDate: "2026-07-23"},
			}
		},
		"Organizer":        func(e *Event) { e.Organizer = &Person{Name: "Other", Email: "other@example.com"} },
		"IsOnlineMeeting":  func(e *Event) { e.IsOnlineMeeting = true },
		"OnlineMeetingURL": func(e *Event) { e.OnlineMeetingURL = "https://example.com/join" },
		"IsCancelled":      func(e *Event) { e.IsCancelled = true },
	} {
		changed := base
		mutate(&changed)
		if CoreContentHash(changed, owner) == CoreContentHash(base, owner) {
			t.Errorf("core hash must change when %s changes", name)
		}
	}

	// The owner's own attendee entry is excluded: flipping the owner's
	// response — or dropping the owner's entry entirely — never moves the
	// core hash.
	ownerFlipped := base
	ownerFlipped.Attendees = cloneAttendees(base.Attendees)
	ownerFlipped.Attendees[0].Response = "declined"
	if CoreContentHash(ownerFlipped, owner) != CoreContentHash(base, owner) {
		t.Error("core hash must exclude the owner's own response")
	}
	ownerGone := base
	ownerGone.Attendees = cloneAttendees(base.Attendees[1:])
	if CoreContentHash(ownerGone, owner) != CoreContentHash(base, owner) {
		t.Error("core hash must exclude the owner's own attendee entry")
	}
}
