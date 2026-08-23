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

/// Visible bounds for each rendered event. CalendarView resolves the selected
/// anchor in its own coordinate space so the peek can stay beside its source.
struct CalendarEventAnchorPreferenceKey: PreferenceKey {
    static let defaultValue: [EventID: Anchor<CGRect>] = [:]

    static func reduce(value: inout [EventID: Anchor<CGRect>],
                       nextValue: () -> [EventID: Anchor<CGRect>])
    {
        value.merge(nextValue(), uniquingKeysWith: { _, latest in latest })
    }
}

struct CalendarEventRow: View {
    let event: CalendarEvent
    let isSelected: Bool

    @ObservedObject private var manager = CalendarManager.shared

    var body: some View {
        // Everything inherits the block's ink from the chrome; the secondary
        // tiers step down by opacity rather than by a second color, so a row
        // stays legible whatever hue its calendar has.
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Text(timeText)
                .font(.system(size: 12, weight: .medium))
                .monospacedDigit()
                .opacity(0.65)
                .frame(width: 52, alignment: .leading)

            VStack(alignment: .leading, spacing: 2) {
                Text(event.displaySubject)
                    .font(.system(size: 13, weight: .semibold))
                    .lineLimit(1)
                    .strikethrough(isDeclined)

                if hasSubline {
                    HStack(spacing: 6) {
                        if let location = event.location, !location.isEmpty {
                            Text(location)
                                .lineLimit(1)
                        }
                        ForEach(markers, id: \.self) { marker in
                            Text(marker)
                        }
                    }
                    .font(.system(size: 11))
                    .opacity(0.65)
                }
            }

            Spacer(minLength: 8)
        }
        // A declined event stays readable but clearly settled.
        .opacity(isDeclined ? 0.5 : (isTentative ? 0.8 : 1.0))
        .padding(.vertical, 7)
        .padding(.horizontal, 12)
        .eventPill(calendarColor, selected: isSelected)
        .padding(.horizontal, 8)
        // The agenda list stacks rows with zero spacing; without this the
        // blocks would touch and read as one slab.
        .padding(.vertical, 1.5)
    }

    private var isTentative: Bool { event.myResponse == "tentativelyAccepted" }
    private var isDeclined: Bool { event.myResponse == "declined" }
    private var hasSubline: Bool {
        !(event.location ?? "").isEmpty || !markers.isEmpty
    }

    private var calendarColor: Color {
        manager.color(for: event)
    }

    private var timeText: String {
        event.allDay ? "All-day" : CalendarTimeFormat.time(event.start)
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
}

// MARK: - Mouse interaction

/// Shared desktop interaction for every concrete event surface: click selects,
/// double-click modifies, and the context menu exposes the same non-visible
/// actions for pointer users. Keeping this in one modifier prevents agenda,
/// month, timed and all-day events from drifting into different workflows.
private struct CalendarEventInteractionModifier: ViewModifier {
    let event: CalendarEvent
    @ObservedObject private var manager = CalendarManager.shared

    func body(content: Content) -> some View {
        content
            .anchorPreference(key: CalendarEventAnchorPreferenceKey.self, value: .bounds) {
                [event.id: $0]
            }
            .onTapGesture { manager.select(event) }
            .highPriorityGesture(
                TapGesture(count: 2).onEnded {
                    manager.select(event)
                    manager.beginEdit()
                }
            )
            .contextMenu {
                Button("Modify Event") {
                    manager.select(event)
                    manager.beginEdit()
                }
                Divider()
                Button("Delete Event", role: .destructive) {
                    manager.select(event)
                    manager.deleteSelected()
                }
            }
            .accessibilityAction(named: "Modify Event") {
                manager.select(event)
                manager.beginEdit()
            }
    }
}

extension View {
    func calendarEventInteractions(_ event: CalendarEvent) -> some View {
        modifier(CalendarEventInteractionModifier(event: event))
    }
}
