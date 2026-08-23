//
//  CalendarEchoLine.swift
//  Durian
//
//  The single line under the grid that always says where the cursor is and
//  what it is on.
//
//  This is what replaced the detail pane. A pane spent a quarter of the
//  window permanently to show a handful of facts about one event; this shows
//  the same headline facts in 24pt, and full detail is one keystroke away
//  instead of always-on. The `[2/3]` counter is the part a pane could not do
//  at all: when several events are stacked under the cursor, it says which
//  one H/L has landed on — position and colour cannot separate them.
//

import SwiftUI

struct CalendarEchoLine: View {
    @ObservedObject var manager = CalendarManager.shared

    var body: some View {
        HStack(spacing: 0) {
            // Fixed-width, monospaced digits: this segment re-renders on every
            // j/k step, and proportional digits would make the rest of the
            // line jitter sideways while scrubbing.
            Text(cursorText)
                .monospacedDigit()
                .foregroundStyle(Color.Detail.textSecondary)

            if let event = currentEvent {
                if stack.count > 1 {
                    separator
                    Text("\(stackIndex + 1)/\(stack.count)")
                        .monospacedDigit()
                        .foregroundStyle(Color.Detail.textTertiary)
                }

                separator

                Circle()
                    .fill(color(for: event))
                    .frame(width: 7, height: 7)
                    .padding(.trailing, 6)

                Text(event.displaySubject)
                    .fontWeight(.medium)
                    .foregroundStyle(Color.Detail.textPrimary)
                    .lineLimit(1)
                    .truncationMode(.tail)

                separator
                Text(rangeText(event))
                    .monospacedDigit()
                    .foregroundStyle(Color.Detail.textSecondary)

                if let location = event.location, !location.isEmpty {
                    separator
                    Text(location)
                        .foregroundStyle(Color.Detail.textTertiary)
                        .lineLimit(1)
                        .truncationMode(.tail)
                }
            } else {
                separator
                Text("Free")
                    .foregroundStyle(Color.Detail.textTertiary)
            }

            Spacer(minLength: 8)

            // The one hint that keeps the command line discoverable. Keyboard
            // speed is worthless if the binding cannot be found, and this is
            // the primary action of the whole view.
            Text(":")
                .fontWeight(.semibold)
                .foregroundStyle(Color.Detail.textTertiary)
            Text("command")
                .foregroundStyle(Color.Detail.textTertiary)
                .padding(.leading, 3)
        }
        .font(.system(size: 11))
        .padding(.horizontal, 12)
        .frame(height: 24)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color(nsColor: .windowBackgroundColor))
        .accessibilityElement(children: .combine)
    }

    private var separator: some View {
        Text("·")
            .foregroundStyle(Color.Detail.textTertiary)
            .padding(.horizontal, 6)
    }

    // MARK: - Content

    private var stack: [CalendarEvent] { manager.eventsAtCursor }

    private var currentEvent: CalendarEvent? {
        if let id = manager.selectedEventID, let hit = stack.first(where: { $0.id == id }) {
            return hit
        }
        return stack.first
    }

    private var stackIndex: Int {
        guard let id = currentEvent?.id else { return 0 }
        return stack.firstIndex { $0.id == id } ?? 0
    }

    private var cursorText: String {
        "\(Self.dayFormatter.string(from: manager.cursorDate)) \(CalendarTimeFormat.time(manager.cursorDate))"
    }

    private func rangeText(_ event: CalendarEvent) -> String {
        event.allDay
            ? "All-day"
            : "\(CalendarTimeFormat.time(event.start))–\(CalendarTimeFormat.time(event.end))"
    }

    private func color(for event: CalendarEvent) -> Color {
        manager.color(for: event)
    }

    private static let dayFormatter: DateFormatter = {
        let f = DateFormatter()
        f.setLocalizedDateFormatFromTemplate("EEE d MMM")
        return f
    }()
}
