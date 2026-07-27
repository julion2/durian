//
//  CalendarEventDetailView.swift
//  Durian
//
//  Detail pane for one calendar event: time, location, organizer, attendees
//  with their RSVP status, online-meeting join link and description.
//

import SwiftUI

struct CalendarEventDetailView: View {
    let event: CalendarEvent

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                Text(event.displaySubject)
                    .font(.title2).fontWeight(.semibold)
                    .foregroundStyle(Color.Detail.textPrimary)

                VStack(alignment: .leading, spacing: 8) {
                    field("When", whenText)
                    if let location = event.location, !location.isEmpty {
                        field("Location", location)
                    }
                    if let organizer = event.organizer {
                        field("Organizer", organizer.displayName)
                    }
                    if let response = myResponseLabel {
                        field("My status", response)
                    }
                    if event.recurring {
                        field("Repeats", "Recurring event")
                    }
                }

                rsvpControls

                if let urlString = event.onlineMeetingURL, let url = URL(string: urlString) {
                    Link(destination: url) {
                        Label("Join online meeting", systemImage: "video.fill")
                    }
                    .buttonStyle(.borderedProminent)
                    .controlSize(.small)
                }

                if !event.attendees.isEmpty {
                    Divider()
                    VStack(alignment: .leading, spacing: 6) {
                        Text("Attendees (\(event.attendees.count))")
                            .font(.caption).fontWeight(.semibold).foregroundStyle(.secondary)
                        ForEach(event.attendees) { attendee in
                            HStack(spacing: 8) {
                                Text(attendee.responseLabel)
                                    .font(.caption2)
                                    .foregroundStyle(.secondary)
                                    .frame(width: 66, alignment: .leading)
                                Text(attendee.displayName)
                                    .font(.callout)
                                    .foregroundStyle(Color.Detail.textBody)
                                Spacer(minLength: 0)
                            }
                        }
                    }
                }

                if let description = event.description, !description.isEmpty {
                    Divider()
                    Text("Description")
                        .font(.caption).fontWeight(.semibold).foregroundStyle(.secondary)
                    Text(description.trimmingCharacters(in: .whitespacesAndNewlines))
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                        .textSelection(.enabled)
                }

                Spacer(minLength: 0)
            }
            .padding(20)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    // MARK: - RSVP

    /// Shown for meetings the account owner attends (not organizes). The current
    /// response is highlighted. Wired to a stub for now — see requestRSVP.
    private var isOrganizer: Bool { event.myResponse == "organizer" }
    private var showRSVP: Bool { !event.attendees.isEmpty && !isOrganizer }

    @ViewBuilder
    private var rsvpControls: some View {
        if showRSVP {
            HStack(spacing: 8) {
                rsvpButton("Accept", target: "accepted", systemImage: "checkmark")
                rsvpButton("Tentative", target: "tentativelyAccepted", systemImage: "questionmark")
                rsvpButton("Decline", target: "declined", systemImage: "xmark")
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

    // MARK: - Fields

    private func field(_ label: String, _ value: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Text(label)
                .font(.callout)
                .foregroundStyle(.secondary)
                .frame(width: 84, alignment: .leading)
            Text(value)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .textSelection(.enabled)
        }
    }

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
