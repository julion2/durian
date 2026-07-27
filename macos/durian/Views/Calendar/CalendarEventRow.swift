//
//  CalendarEventRow.swift
//  Durian
//
//  One agenda row: time, subject, a colored dot for the owning calendar, and
//  dim markers (online / RSVP). Selection highlight matches the mail list.
//

import SwiftUI

struct CalendarEventRow: View {
    let event: CalendarEvent
    let isSelected: Bool

    @ObservedObject private var manager = CalendarManager.shared
    @ObservedObject private var profileManager = ProfileManager.shared

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            calendarDot
                .frame(width: 9, height: 9)
                .padding(.top, 4)

            VStack(alignment: .leading, spacing: 2) {
                Text(event.displaySubject)
                    .font(.headline)
                    .lineLimit(1)
                    .strikethrough(isDeclined, color: isSelected ? .white : .secondary)
                    .foregroundStyle(subjectColor)
                    .opacity(isTentative ? 0.8 : 1.0)

                HStack(spacing: 6) {
                    Text(timeText)
                        .font(.caption)
                        .foregroundStyle(isSelected ? Color.white.opacity(0.85) : .secondary)
                    if let location = event.location, !location.isEmpty {
                        Text("· \(location)")
                            .font(.caption)
                            .lineLimit(1)
                            .foregroundStyle(isSelected ? Color.white.opacity(0.85) : .secondary)
                    }
                    ForEach(markers, id: \.self) { marker in
                        Text(marker)
                            .font(.caption2)
                            .foregroundStyle(isSelected ? Color.white.opacity(0.75) : Color.secondary)
                    }
                }
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 12)
        .background(
            RoundedRectangle(cornerRadius: 6, style: .continuous)
                .fill(isSelected ? profileManager.resolvedAccentColor : Color.clear)
        )
        .padding(.horizontal, 8)
    }

    private var isTentative: Bool { event.myResponse == "tentativelyAccepted" }
    private var isDeclined: Bool { event.myResponse == "declined" }

    /// A dashed ring for a tentative RSVP, a dimmed dot for a declined one,
    /// otherwise a filled dot in the calendar's color.
    @ViewBuilder
    private var calendarDot: some View {
        if isTentative {
            Circle().strokeBorder(calendarColor, style: StrokeStyle(lineWidth: 2, dash: [2, 2]))
        } else {
            Circle().fill(isDeclined ? calendarColor.opacity(0.4) : calendarColor)
        }
    }

    private var subjectColor: Color {
        if isSelected { return .white }
        if isDeclined { return .secondary }
        return Color.Detail.textPrimary
    }

    private var calendarColor: Color {
        manager.calendars.first { $0.name == event.calendar }?.color ?? .secondary
    }

    private var timeText: String {
        if event.allDay { return "all-day" }
        return Self.timeFormatter.string(from: event.start)
    }

    private var markers: [String] {
        var out: [String] = []
        if event.onlineMeeting { out.append("online") }
        switch event.myResponse ?? "" {
        case "accepted": out.append("accepted")
        case "declined": out.append("declined")
        case "tentativelyAccepted": out.append("tentative")
        default: break
        }
        return out
    }

    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter()
        f.timeStyle = .short
        f.dateStyle = .none
        return f
    }()
}
