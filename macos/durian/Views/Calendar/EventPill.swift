//
//  EventPill.swift
//  Durian
//
//  The shared visual language for calendar events: a rounded pill filled
//  with a soft tint of the owning calendar's color and a saturated accent
//  bar on the leading edge, titles in the primary text color. All-day
//  events use a near-solid fill instead (no bar). Selection is a ring in
//  the primary color. The week grid, agenda (and later the month/year
//  views) all style events through this file so they cannot drift apart.
//

import SwiftUI

// MARK: - Pill chrome

/// The pill's background, accent bar, selection ring and default text color.
/// Purely visual: it adds no gestures and no hit-testable overlays, so hosts
/// can layer their own handles/gestures on top unchanged.
struct EventPillChrome: ViewModifier {
    enum Variant {
        /// Soft tint of the calendar color over an opaque surface, with a
        /// 3pt accent bar on the leading edge. Text defaults to primary.
        case timed
        /// Near-solid calendar color, no bar. Text defaults to white.
        case allDay
    }

    let color: Color
    let variant: Variant
    let selected: Bool
    /// Square off an edge a day boundary cut (a week-grid slice of a
    /// multi-day event), so the block reads as continuing into the
    /// neighbouring day rather than ending here.
    var squareTop = false
    var squareBottom = false
    var cornerRadius: CGFloat = 5

    private var shape: UnevenRoundedRectangle {
        UnevenRoundedRectangle(
            topLeadingRadius: squareTop ? 0 : cornerRadius,
            bottomLeadingRadius: squareBottom ? 0 : cornerRadius,
            bottomTrailingRadius: squareBottom ? 0 : cornerRadius,
            topTrailingRadius: squareTop ? 0 : cornerRadius
        )
    }

    func body(content: Content) -> some View {
        content
            .background { fill }
            // Selection stays in the calendar's own color family (a deeper fill
            // plus a hairline border in that color) instead of a high-contrast
            // black/white ring, which read as too heavy on the small pills.
            .overlay(shape.strokeBorder(selectionBorderColor,
                                        lineWidth: selected ? (variant == .timed ? 1.5 : 1) : 0))
            .foregroundStyle(variant == .allDay ? Color.white : Color.Detail.textPrimary)
    }

    /// The selected-state border: the calendar color for timed pills; a soft
    /// white edge for all-day pills (whose fill is already the solid color).
    private var selectionBorderColor: Color {
        variant == .allDay ? Color.white.opacity(0.7) : color
    }

    @ViewBuilder
    private var fill: some View {
        ZStack(alignment: .leading) {
            switch variant {
            case .timed:
                // Opaque surface first: the tint is translucent, and without
                // this the grid's hour lines would show through the block.
                shape.fill(Color.Detail.cardBackground)
                shape.fill(color.eventTint(selected: selected))
                // The accent bar widens a touch when selected, so selection
                // reads even where the border is subtle.
                Rectangle().fill(color).frame(width: selected ? 4 : 3)
            case .allDay:
                shape.fill(color.eventSolid(selected: selected))
            }
        }
        // Clips the accent bar to the pill's rounded (or squared) corners.
        .clipShape(shape)
    }
}

extension View {
    /// Applies the shared event-pill chrome around this content.
    func eventPill(_ color: Color, variant: EventPillChrome.Variant,
                   selected: Bool, squareTop: Bool = false, squareBottom: Bool = false,
                   cornerRadius: CGFloat = 5) -> some View {
        modifier(EventPillChrome(color: color, variant: variant, selected: selected,
                                 squareTop: squareTop, squareBottom: squareBottom,
                                 cornerRadius: cornerRadius))
    }
}

// MARK: - Grid label

/// The label stack inside a week-grid block: the start time small on top
/// (only when the block is tall enough to fit it) and the title under it.
struct EventPillGridLabel: View {
    let event: CalendarEvent
    let showsTime: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            if showsTime {
                Text(Self.timeFormatter.string(from: event.start))
                    .font(.system(size: 8, weight: .semibold))
                    .foregroundStyle(Color.Detail.textSecondary)
            }
            Text(event.displaySubject)
                .font(.system(size: 10)).fontWeight(.medium).lineLimit(2)
                .foregroundStyle(Color.Detail.textPrimary)
        }
    }

    /// Fixed 24h "HH:mm": inside a ~40pt lane a locale AM/PM suffix would
    /// eat most of the line.
    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "HH:mm"; return f
    }()
}

// MARK: - Day number badge

/// A day-of-month number for date headers: a filled accent badge with
/// contrasting text when the day is today, plain primary text otherwise.
struct DayNumberBadge: View {
    let date: Date

    var body: some View {
        let isToday = Calendar.current.isDateInToday(date)
        Text("\(Calendar.current.component(.day, from: date))")
            .font(.callout)
            .fontWeight(isToday ? .semibold : .regular)
            .foregroundStyle(isToday ? Color.white : Color.Detail.textPrimary)
            .frame(minWidth: 20, minHeight: 20)
            .background {
                if isToday {
                    Circle().fill(Color.accentColor)
                }
            }
    }
}
