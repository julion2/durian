//
//  CalendarEventDetailView.swift
//  Durian
//
//  Detail pane for one calendar event: time, location, organizer, attendees
//  with their RSVP status, online-meeting join link and description.
//  Presented as grouped rounded cards on the pane background.
//

import SwiftUI

struct CalendarEventDetailView: View {
    let event: CalendarEvent

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                header

                whenCard

                if let location = event.location, !location.isEmpty {
                    card("Location") {
                        Text(location)
                            .font(.callout)
                            .foregroundStyle(Color.Detail.textBody)
                            .textSelection(.enabled)
                    }
                }

                calendarCard

                if event.organizer != nil || !event.attendees.isEmpty {
                    peopleCard
                }

                rsvpCard

                if let urlString = event.onlineMeetingURL, let url = URL(string: urlString) {
                    Link(destination: url) {
                        Label("Join online meeting", systemImage: "video.fill")
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.regular)
                }

                if let description = event.description, !description.isEmpty {
                    card("Description") {
                        Text(description.trimmingCharacters(in: .whitespacesAndNewlines))
                            .font(.callout)
                            .foregroundStyle(Color.Detail.textBody)
                            .textSelection(.enabled)
                    }
                }

                actionRow

                Spacer(minLength: 0)
            }
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(Color(nsColor: .windowBackgroundColor))
    }

    // MARK: - Header

    private var header: some View {
        HStack(alignment: .top, spacing: 10) {
            RoundedRectangle(cornerRadius: 2)
                .fill(calendarColor)
                .frame(width: 4)
                .frame(maxHeight: 24)
                .padding(.top, 3)
            Text(event.displaySubject)
                .font(.title2).fontWeight(.semibold)
                .foregroundStyle(Color.Detail.textPrimary)
                .textSelection(.enabled)
        }
        .padding(.bottom, 4)
    }

    // MARK: - Cards

    private var whenCard: some View {
        card("When") {
            Text(whenText)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .textSelection(.enabled)
            if event.recurring {
                Label("Recurring event", systemImage: "repeat")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
        }
    }

    private var calendarCard: some View {
        card("Calendar") {
            HStack(spacing: 8) {
                Circle()
                    .fill(calendarColor)
                    .frame(width: 10, height: 10)
                Text(event.calendar)
                    .font(.callout)
                    .foregroundStyle(Color.Detail.textBody)
            }
        }
    }

    private var peopleCard: some View {
        card(peopleTitle) {
            VStack(alignment: .leading, spacing: 10) {
                if let organizer = event.organizer {
                    HStack(spacing: 8) {
                        avatar(organizer.displayName)
                        Text(organizer.displayName)
                            .font(.callout)
                            .foregroundStyle(Color.Detail.textBody)
                            .lineLimit(1)
                        Spacer(minLength: 8)
                        Text("Organizer")
                            .font(.caption2)
                            .foregroundStyle(Color.Detail.textSecondary)
                    }
                }
                ForEach(displayAttendees) { attendee in
                    HStack(spacing: 8) {
                        avatar(attendee.displayName)
                        Text(attendee.displayName)
                            .font(.callout)
                            .foregroundStyle(Color.Detail.textBody)
                            .lineLimit(1)
                        Spacer(minLength: 8)
                        statusBadge(attendee.response, label: attendee.responseLabel)
                    }
                }
            }
        }
    }

    /// Attendees without the organizer: some providers (e.g. Google) list the
    /// organizer as a self-attendee, which would otherwise show them twice —
    /// once in the organizer row and once here.
    private var displayAttendees: [CalendarAttendee] {
        guard let org = event.organizer?.email, !org.isEmpty else { return event.attendees }
        return event.attendees.filter { $0.email.caseInsensitiveCompare(org) != .orderedSame }
    }

    private var peopleTitle: String {
        displayAttendees.isEmpty ? "People" : "People (\(displayAttendees.count))"
    }

    // MARK: - RSVP

    /// Shown for meetings the account owner attends (not organizes). The current
    /// response is highlighted. The buttons save the response locally via
    /// CalendarManager.requestRSVP; the organizer is notified on the next sync.
    private var isOrganizer: Bool { event.myResponse == "organizer" }
    private var showRSVP: Bool { !event.attendees.isEmpty && !isOrganizer }

    @ViewBuilder
    private var rsvpCard: some View {
        if showRSVP || myResponseLabel != nil {
            card("My status") {
                if let response = myResponseLabel {
                    Text(response)
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                }
                if showRSVP {
                    HStack(spacing: 8) {
                        rsvpButton("Accept", target: "accepted", systemImage: "checkmark")
                        rsvpButton("Tentative", target: "tentativelyAccepted", systemImage: "questionmark")
                        rsvpButton("Decline", target: "declined", systemImage: "xmark")
                    }
                }
            }
        }
    }

    private func rsvpButton(_ title: String, target: String, systemImage: String) -> some View {
        let isCurrent = event.myResponse == target
        return Button {
            CalendarManager.shared.requestRSVP(event, response: target)
        } label: {
            Label(title, systemImage: systemImage)
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
        .tint(isCurrent ? .accentColor : .secondary)
    }

    // MARK: - Actions

    private var actionRow: some View {
        HStack(spacing: 8) {
            Button {
                CalendarManager.shared.beginEdit()
            } label: {
                Label("Edit", systemImage: "pencil")
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
            Spacer()
            Button(role: .destructive) {
                CalendarManager.shared.deleteSelected()
            } label: {
                Label("Delete", systemImage: "trash")
            }
            .buttonStyle(.bordered)
            .controlSize(.small)
            .tint(.red)
        }
        .padding(.top, 4)
    }

    // MARK: - Styling helpers

    /// A grouped rounded card: hairline border over the card surface, with an
    /// optional caption heading above the content.
    private func card<Content: View>(_ title: String? = nil,
                                     @ViewBuilder content: () -> Content) -> some View
    {
        VStack(alignment: .leading, spacing: 8) {
            if let title {
                Text(title)
                    .font(.caption).fontWeight(.semibold)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
            content()
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.Detail.cardBackground)
        .clipShape(RoundedRectangle(cornerRadius: 11))
        .overlay(RoundedRectangle(cornerRadius: 11).stroke(Color.Detail.border, lineWidth: 1))
    }

    /// A small initials circle for organizer/attendee rows.
    private func avatar(_ name: String) -> some View {
        Text(initials(name))
            .font(.system(size: 10, weight: .semibold))
            .foregroundStyle(Color.Detail.textSecondary)
            .frame(width: 26, height: 26)
            .background(Circle().fill(Color.Detail.buttonBackground))
            .overlay(Circle().stroke(Color.Detail.border, lineWidth: 1))
    }

    private func initials(_ name: String) -> String {
        let parts = name
            .split(whereSeparator: { $0 == " " || $0 == "." || $0 == "@" })
            .prefix(2)
            .compactMap { $0.first }
        guard !parts.isEmpty else { return "?" }
        return String(parts).uppercased()
    }

    /// PARTSTAT glyph + label for an attendee row's trailing edge.
    private func statusBadge(_ response: String?, label: String) -> some View {
        let (glyph, color) = statusGlyph(response)
        return HStack(spacing: 4) {
            Image(systemName: glyph)
                .font(.caption2)
                .foregroundStyle(color)
            Text(label)
                .font(.caption2)
                .foregroundStyle(Color.Detail.textSecondary)
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

    /// The event's calendar color (from the synced calendar list), used as a
    /// small accent — never as a large fill.
    private var calendarColor: Color {
        CalendarManager.shared.calendars
            .first { $0.name == event.calendar }?
            .color ?? .secondary
    }

    // MARK: - Formatting

    private var whenText: String {
        if event.allDay {
            return "\(Self.dayFormatter.string(from: event.start)) (all-day)"
        }
        let sameDay = Calendar.current.isDate(event.start, inSameDayAs: event.end)
        let day = Self.dayFormatter.string(from: event.start)
        let startTime = Self.timeFormatter.string(from: event.start)
        let endTime = Self.timeFormatter.string(from: event.end)
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

    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter()
        f.timeStyle = .short
        f.dateStyle = .none
        return f
    }()
}
