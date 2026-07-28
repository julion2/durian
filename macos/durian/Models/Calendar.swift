//
//  Calendar.swift
//  Durian
//
//  Domain models for the calendar UI, mapped from the CalendarBackend wire
//  structs (which mirror openapi.yaml). Read-only.
//

import Foundation
import SwiftUI

// MARK: - Calendar

struct CalendarInfo: Identifiable, Hashable {
    // Identity is account+name: two accounts can each have a calendar of the
    // same name, and they must not collapse into one another.
    var id: String { CalendarInfo.key(account: account, name: name) }
    let name: String
    let colorHex: String?
    let eventCount: Int
    /// The account this calendar belongs to (set by CalendarManager) — needed
    /// to write events into it and to scope its visibility.
    var account: String = ""

    /// Stable per-(account, calendar) key used for identity and the
    /// hidden-calendars set. The separator cannot appear in an email/name.
    static func key(account: String, name: String) -> String {
        account + "\n" + name
    }

    var visibilityKey: String { CalendarInfo.key(account: account, name: name) }

    /// The calendar's color, or a neutral gray when it has none.
    var color: Color {
        if let hex = colorHex, !hex.isEmpty { return Color(hex: hex) }
        return .secondary
    }

    init(from wire: CalendarWire) {
        name = wire.name
        colorHex = wire.color
        eventCount = wire.event_count
    }
}

// MARK: - People

struct CalendarPerson: Hashable {
    let name: String?
    let email: String

    var displayName: String {
        if let name, !name.isEmpty, name.caseInsensitiveCompare(email) != .orderedSame {
            return name
        }
        return email
    }

    init(from wire: CalendarPersonWire) {
        name = wire.name
        email = wire.email
    }
}

struct CalendarAttendee: Identifiable, Hashable {
    var id: String { email }
    let name: String?
    let email: String
    let type: String?
    let response: String?

    var displayName: String {
        if let name, !name.isEmpty, name.caseInsensitiveCompare(email) != .orderedSame {
            return name
        }
        return email
    }

    /// Short human RSVP label for the attendee row.
    var responseLabel: String {
        switch response ?? "" {
        case "accepted", "organizer": return "accepted"
        case "declined": return "declined"
        case "tentativelyAccepted": return "tentative"
        default: return "no reply"
        }
    }

    init(from wire: CalendarAttendeeWire) {
        name = wire.name
        email = wire.email
        type = wire.type
        response = wire.response
    }
}

// MARK: - Event identity

/// Stable identity for a calendar event. For a non-recurring event the
/// identity is (account, calendar, uid) and `occurrence` is nil — moving or
/// resizing the event does NOT change its identity, so SwiftUI diffs it as an
/// update instead of remove+insert and the selection survives edits.
/// Occurrences of a recurring series share a uid, so they carry their start
/// as `occurrence` to disambiguate; a series' occurrence times only change
/// via series edits, which refetch anyway.
struct EventID: Hashable, Codable {
    let account: String
    let calendar: String
    let uid: String
    let occurrence: Date?

    init(account: String, calendar: String, uid: String, occurrence: Date?) {
        self.account = account
        self.calendar = calendar
        self.uid = uid
        self.occurrence = occurrence
    }

    init(_ event: CalendarEvent) {
        account = event.account
        calendar = event.calendar
        uid = event.uid
        occurrence = event.recurring ? event.start : nil
    }
}

extension EventID: Comparable {
    /// Deterministic ordering so equal-start events keep a stable sort key
    /// (used as the final tie-break in the store projection and lane layout).
    static func < (lhs: EventID, rhs: EventID) -> Bool {
        if lhs.account != rhs.account { return lhs.account < rhs.account }
        if lhs.calendar != rhs.calendar { return lhs.calendar < rhs.calendar }
        if lhs.uid != rhs.uid { return lhs.uid < rhs.uid }
        return (lhs.occurrence ?? .distantPast) < (rhs.occurrence ?? .distantPast)
    }
}

// MARK: - Event

struct CalendarEvent: Identifiable, Hashable {
    var id: EventID { EventID(self) }
    let uid: String
    let calendar: String
    let subject: String
    var start: Date
    var end: Date
    let allDay: Bool
    let location: String?
    let myResponse: String?
    let onlineMeeting: Bool
    let onlineMeetingURL: String?
    let recurring: Bool
    let organizer: CalendarPerson?
    let attendees: [CalendarAttendee]
    let description: String?
    /// The account this event belongs to (set by CalendarManager, not from the
    /// wire) — needed to fetch its full detail.
    var account: String = ""
    /// For a recurring event's detail, the series MASTER start/end as the API
    /// reported them, kept by CalendarManager.loadDetail before start/end are
    /// replaced with the selected occurrence's times. Editing a series must
    /// write the master times, not the occurrence's (see CalendarEventDraft).
    var seriesStart: Date?
    var seriesEnd: Date?

    var displaySubject: String { subject.isEmpty ? "(no subject)" : subject }

    /// Direct construction, used for optimistic local updates (an edited copy
    /// of a stored event before the server round-trip) and for tests. The wire
    /// init below stays the only decode path.
    init(uid: String, calendar: String, subject: String, start: Date, end: Date,
         allDay: Bool = false, location: String? = nil, myResponse: String? = nil,
         onlineMeeting: Bool = false, onlineMeetingURL: String? = nil,
         recurring: Bool = false, organizer: CalendarPerson? = nil,
         attendees: [CalendarAttendee] = [], description: String? = nil,
         account: String = "", seriesStart: Date? = nil, seriesEnd: Date? = nil) {
        self.uid = uid
        self.calendar = calendar
        self.subject = subject
        self.start = start
        self.end = end
        self.allDay = allDay
        self.location = location
        self.myResponse = myResponse
        self.onlineMeeting = onlineMeeting
        self.onlineMeetingURL = onlineMeetingURL
        self.recurring = recurring
        self.organizer = organizer
        self.attendees = attendees
        self.description = description
        self.account = account
        self.seriesStart = seriesStart
        self.seriesEnd = seriesEnd
    }

    /// Fails when the start/end timestamps cannot be parsed.
    init?(from wire: CalendarEventWire) {
        guard let s = Self.parseDate(wire.start), let e = Self.parseDate(wire.end) else {
            return nil
        }
        uid = wire.uid
        calendar = wire.calendar
        subject = wire.subject
        allDay = wire.all_day
        // The API reports all-day boundaries as midnight UTC. The views group
        // and format in LOCAL time, so map the UTC calendar day onto the local
        // calendar day — otherwise an all-day event shows on the wrong day for
        // any zone west of UTC (and at odd hours in the edit form everywhere).
        if wire.all_day {
            start = Self.localMidnight(fromUTCDay: s)
            end = Self.localMidnight(fromUTCDay: e)
        } else {
            start = s
            end = e
        }
        location = wire.location
        myResponse = wire.my_response
        onlineMeeting = wire.online_meeting ?? false
        onlineMeetingURL = wire.online_meeting_url
        recurring = wire.recurring ?? false
        organizer = wire.organizer.map(CalendarPerson.init(from:))
        attendees = (wire.attendees ?? []).map(CalendarAttendee.init(from:))
        description = wire.description
    }

    private static func parseDate(_ string: String) -> Date? {
        let iso = ISO8601DateFormatter()
        iso.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = iso.date(from: string) { return d }
        iso.formatOptions = [.withInternetDateTime]
        return iso.date(from: string)
    }

    // MARK: - All-day date mapping (UTC calendar day <-> local calendar day)

    private static let utcCalendar: Foundation.Calendar = {
        var cal = Foundation.Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC") ?? .current
        return cal
    }()

    /// Local midnight of the same year/month/day the date has in UTC.
    static func localMidnight(fromUTCDay date: Date) -> Date {
        let comps = utcCalendar.dateComponents([.year, .month, .day], from: date)
        return Foundation.Calendar.current.date(from: comps) ?? date
    }

    /// UTC midnight of the same year/month/day the date has in local time —
    /// the inverse of localMidnight, used when writing all-day payloads.
    static func utcMidnight(fromLocalDay date: Date) -> Date {
        let comps = Foundation.Calendar.current.dateComponents([.year, .month, .day], from: date)
        return utcCalendar.date(from: comps) ?? date
    }
}

// MARK: - Editable draft

/// A mutable event for the create/edit form. uid is empty for a new event.
struct CalendarEventDraft: Identifiable {
    let id = UUID()
    var uid: String
    var account: String
    var calendar: String
    var subject: String
    var start: Date
    var end: Date
    var allDay: Bool
    var location: String
    var description: String
    /// The edited event is a recurring series: the form warns that saving
    /// changes the whole series, and the draft times are the series master's.
    var recurring: Bool = false

    var isNew: Bool { uid.isEmpty }

    /// A draft pre-filled from an existing event for editing. For a recurring
    /// event the SERIES MASTER times are used (falling back to the occurrence
    /// when the detail fetch has not resolved them) — writing an occurrence's
    /// date onto the master would silently shift the entire series.
    init(from event: CalendarEvent) {
        uid = event.uid
        account = event.account
        calendar = event.calendar
        subject = event.subject
        recurring = event.recurring
        if event.recurring {
            start = event.seriesStart ?? event.start
            end = event.seriesEnd ?? event.end
        } else {
            start = event.start
            end = event.end
        }
        allDay = event.allDay
        location = event.location ?? ""
        description = event.description ?? ""
    }

    /// A blank draft for a new event in a calendar at a given time.
    init(account: String, calendar: String, start: Date, end: Date) {
        uid = ""
        self.account = account
        self.calendar = calendar
        subject = ""
        self.start = start
        self.end = end
        allDay = false
        location = ""
        description = ""
    }

    /// The RFC3339 write payload for the API. All-day drafts are sent as UTC
    /// midnight of the LOCAL calendar day the pickers show (the inverse of the
    /// read mapping in CalendarEvent), with the end snapped to at least one
    /// full day — the shape Graph requires.
    func toWrite() -> CalendarEventWrite {
        var s = start
        var e = end
        if allDay {
            s = CalendarEvent.utcMidnight(fromLocalDay: start)
            e = CalendarEvent.utcMidnight(fromLocalDay: end)
            if e <= s {
                e = s.addingTimeInterval(24 * 60 * 60)
            }
        }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return CalendarEventWrite(
            account: account, calendar: calendar, uid: uid, subject: subject,
            start: f.string(from: s), end: f.string(from: e),
            all_day: allDay, location: location, description: description
        )
    }
}
