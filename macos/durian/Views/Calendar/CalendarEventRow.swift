//
//  CalendarEventRow.swift
//  Durian
//
//  One agenda row: the shared event pill in its compact form — calendar
//  color as accent bar + soft tint, subject on the left, right-aligned
//  time — with a colored dot carrying the RSVP state (dashed ring for
//  tentative, dimmed for declined). Selection is the pill's ring.
//

import SwiftUI

struct CalendarEventRow: View {
    let event: CalendarEvent
    let isSelected: Bool

    @ObservedObject private var manager = CalendarManager.shared

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            calendarDot
                .frame(width: 9, height: 9)
                .padding(.top, 4)

            VStack(alignment: .leading, spacing: 2) {
                Text(event.displaySubject)
                    .font(.headline)
                    .lineLimit(1)
                    .strikethrough(isDeclined, color: .secondary)
                    .foregroundStyle(subjectColor)
                    .opacity(isTentative ? 0.8 : 1.0)

                if hasSubline {
                    HStack(spacing: 6) {
                        if let location = event.location, !location.isEmpty {
                            Text(location)
                                .font(.caption)
                                .lineLimit(1)
                                .foregroundStyle(Color.Detail.textSecondary)
                        }
                        ForEach(markers, id: \.self) { marker in
                            Text(marker)
                                .font(.caption2)
                                .foregroundStyle(Color.Detail.textTertiary)
                        }
                    }
                }
            }

            Spacer(minLength: 8)

            Text(timeText)
                .font(.caption)
                .monospacedDigit()
                .foregroundStyle(Color.Detail.textSecondary)
                .padding(.top, 3)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 12)
        .eventPill(calendarColor, variant: .timed, selected: isSelected, cornerRadius: 6)
        .padding(.horizontal, 8)
        // The agenda list stacks rows with zero spacing; without this the
        // tinted pills would touch and read as one slab.
        .padding(.vertical, 1.5)
    }

    private var isTentative: Bool { event.myResponse == "tentativelyAccepted" }
    private var isDeclined: Bool { event.myResponse == "declined" }
    private var hasSubline: Bool {
        !(event.location ?? "").isEmpty || !markers.isEmpty
    }

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
        isDeclined ? .secondary : Color.Detail.textPrimary
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
