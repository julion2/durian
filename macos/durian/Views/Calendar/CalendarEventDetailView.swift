//
//  CalendarEventDetailView.swift
//  Durian
//
//  Detail pane for one calendar event: time, location, organizer, attendees
//  with their RSVP status, online-meeting join link and description.
//
//  Laid out flush rather than as a stack of bordered cards. The pane's facts
//  are mostly one line each — a date, a place, a calendar name — and a card
//  around a single line adds a border, a radius and a heading to group
//  something that proximity already groups. What is left is a leading glyph
//  per row, which names the kind of fact without spending a text label on it.
//

import SwiftUI

struct CalendarEventDetailView: View {
    let event: CalendarEvent

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header

            Divider().padding(.vertical, 14)

            VStack(alignment: .leading, spacing: 10) {
                if let location = event.location, !location.isEmpty {
                    factRow("mappin.and.ellipse", location)
                }
                factRow("calendar", event.calendar, dot: calendarColor)
                if event.recurring {
                    factRow("repeat", "Repeats")
                }
            }

            if let urlString = event.onlineMeetingURL, let url = URL(string: urlString) {
                Link(destination: url) {
                    Label("Join online meeting", systemImage: "video.fill")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)
                .tint(ProfileManager.shared.resolvedAccentColor)
                .padding(.top, 16)
            }

            if event.organizer != nil || !displayAttendees.isEmpty {
                section("People") { people }
            }

            rsvpSection

            if let description = event.description, !description.isEmpty {
                section("Notes") {
                    Text(description.trimmingCharacters(in: .whitespacesAndNewlines))
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
        .padding(18)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .windowBackgroundColor))
    }

    // MARK: - Header

    /// Title plus the one fact important enough to sit with it. Everything
    /// else is a row below; the time is what you came here to read.
    private var header: some View {
        HStack(alignment: .top, spacing: 10) {
            RoundedRectangle(cornerRadius: 2)
                .fill(calendarColor)
                .frame(width: 4)
                .frame(maxHeight: .infinity)

            VStack(alignment: .leading, spacing: 4) {
                Text(event.displaySubject)
                    .font(.title3).fontWeight(.semibold)
                    .foregroundStyle(Color.Detail.textPrimary)
                    .textSelection(.enabled)
                Text(whenText)
                    .font(.callout)
                    .foregroundStyle(Color.Detail.textSecondary)
                    .textSelection(.enabled)
            }
        }
        .fixedSize(horizontal: false, vertical: true)
    }

    // MARK: - Rows

    /// One fact: a glyph naming its kind, then the value. The glyph replaces
    /// the caption heading a card would have needed.
    private func factRow(_ symbol: String, _ text: String, dot: Color? = nil) -> some View {
        HStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 12))
                .foregroundStyle(Color.Detail.textTertiary)
                .frame(width: 16)
            if let dot {
                Circle().fill(dot).frame(width: 8, height: 8)
            }
            Text(text)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .textSelection(.enabled)
            Spacer(minLength: 0)
        }
    }

    /// A titled group. Headings survive where a run of rows genuinely needs
    /// naming — a list of faces, a block of prose — and nowhere else.
    private func section<Content: View>(_ title: String,
                                        @ViewBuilder content: () -> Content) -> some View
    {
        VStack(alignment: .leading, spacing: 8) {
            Text(title)
                .font(.caption).fontWeight(.semibold)
                .foregroundStyle(Color.Detail.textTertiary)
            content()
        }
        .padding(.top, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var people: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let organizer = event.organizer {
                personRow(name: organizer.displayName, email: organizer.email) {
                    Text("Organizer")
                        .font(.caption2)
                        .foregroundStyle(Color.Detail.textTertiary)
                }
            }
            ForEach(displayAttendees) { attendee in
                personRow(name: attendee.displayName, email: attendee.email) {
                    statusBadge(attendee.response, label: attendee.responseLabel)
                }
            }
        }
    }

    private func personRow<Trailing: View>(name: String, email: String,
                                           @ViewBuilder trailing: () -> Trailing) -> some View
    {
        HStack(spacing: 8) {
            AvatarView(name: name, email: email, size: 26)
            Text(name)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .lineLimit(1)
            Spacer(minLength: 8)
            trailing()
        }
    }

    // MARK: - RSVP

    /// Shown for meetings the account owner attends (not organizes). The current
    /// response is highlighted. Clicking a response saves it locally and sends
    /// that one event's RSVP immediately.
    private var isOrganizer: Bool { event.myResponse == "organizer" }
    private var showRSVP: Bool { !event.attendees.isEmpty && !isOrganizer }

    @ViewBuilder
    private var rsvpSection: some View {
        if showRSVP {
            section("My status") {
                HStack(spacing: 8) {
                    rsvpButton("Accept", target: "accepted", systemImage: "checkmark")
                    rsvpButton("Tentative", target: "tentativelyAccepted", systemImage: "questionmark")
                    rsvpButton("Decline", target: "declined", systemImage: "xmark")
                }
            }
        } else if let response = myResponseLabel {
            factRow(statusGlyph(event.myResponse).0, response)
                .padding(.top, 10)
        }
    }

    /// The response already saved reads as filled; the other two as outlines,
    /// so the card answers "what did I say" before it offers "say something
    /// else".
    @ViewBuilder
    private func rsvpButton(_ title: String, target: String, systemImage: String) -> some View {
        let isCurrent = event.myResponse == target
        let action = { CalendarManager.shared.requestRSVP(event, response: target) }
        if isCurrent {
            Button(action: action) { Label(title, systemImage: systemImage) }
                .buttonStyle(.borderedProminent)
                .controlSize(.small)
                .tint(ProfileManager.shared.resolvedAccentColor)
        } else {
            Button(action: action) { Label(title, systemImage: systemImage) }
                .buttonStyle(.bordered)
                .controlSize(.small)
        }
    }

    // MARK: - Styling helpers

    /// PARTSTAT glyph + label for an attendee row's trailing edge.
    private func statusBadge(_ response: String?, label: String) -> some View {
        let (glyph, color) = statusGlyph(response)
        return HStack(spacing: 4) {
            Image(systemName: glyph)
                .font(.caption2)
                .foregroundStyle(color)
            Text(label)
                .font(.caption2)
                .foregroundStyle(Color.Detail.textTertiary)
        }
    }

    private func statusGlyph(_ response: String?) -> (String, Color) {
        switch response ?? "" {
        case "accepted", "organizer": return ("checkmark.circle.fill", .green)
        case "declined": return ("xmark.circle.fill", .red)
        case "tentativelyAccepted": return ("questionmark.circle.fill", .orange)
        default: return ("circle.dotted", Color.Detail.textTertiary)
        }
    }

    /// Attendees without the organizer: some providers (e.g. Google) list the
    /// organizer as a self-attendee, which would otherwise show them twice —
    /// once in the organizer row and once here.
    private var displayAttendees: [CalendarAttendee] {
        guard let org = event.organizer?.email, !org.isEmpty else { return event.attendees }
        return event.attendees.filter { $0.email.caseInsensitiveCompare(org) != .orderedSame }
    }

    /// The event's calendar color (from the synced calendar list), used as a
    /// small accent — never as a large fill.
    private var calendarColor: Color {
        CalendarManager.shared.color(for: event)
    }

    // MARK: - Formatting

    private var whenText: String {
        if event.allDay {
            return "\(Self.dayFormatter.string(from: event.start)) · All-day"
        }
        let sameDay = Calendar.current.isDate(event.start, inSameDayAs: event.end)
        let day = Self.dayFormatter.string(from: event.start)
        let startTime = CalendarTimeFormat.time(event.start)
        let endTime = CalendarTimeFormat.time(event.end)
        if sameDay {
            return "\(day) · \(startTime) – \(endTime)"
        }
        return "\(day) \(startTime) – \(Self.dayFormatter.string(from: event.end)) \(endTime)"
    }

    private var myResponseLabel: String? {
        switch event.myResponse ?? "" {
        case "accepted": return "Accepted"
        case "declined": return "Declined"
        case "tentativelyAccepted": return "Tentative"
        case "organizer": return "Organizer"
        case "notResponded", "none", "": return nil
        default: return event.myResponse
        }
    }

    private static let dayFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "EEEE, d MMMM yyyy"
        return f
    }()
}
