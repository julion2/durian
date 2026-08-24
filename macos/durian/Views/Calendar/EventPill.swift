//
//  EventPill.swift
//  Durian
//
//  The shared visual language for calendar events: a rounded block filled
//  with a pale wash of the owning calendar's hue (see Color.eventFill),
//  carrying text in that same hue's ink. The week grid,
//  agenda, month and year views all style events through this file so they
//  cannot drift apart.
//
//  The contrast in the calendar is meant to sit BETWEEN the grid and the
//  blocks, not inside a block: the grid is hairlines on the window surface
//  and the blocks are the only tinted thing on screen. The wash and matching
//  ink already carry the calendar identity, so a separate accent bar would
//  answer a question already answered.
//

import SwiftUI

// MARK: - Pill chrome

/// The block's fill, ink and selection ring. Purely visual: it adds no
/// gestures and no hit-testable overlays, so hosts can layer their own
/// handles/gestures on top unchanged.
struct EventPillChrome: ViewModifier {
    let color: Color
    let selected: Bool
    /// Square off an edge a day boundary cut (a week-grid slice of a
    /// multi-day event), so the block reads as continuing into the
    /// neighbouring day rather than ending here.
    var squareTop = false
    var squareBottom = false
    var cornerRadius: CGFloat = 8

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
            .background { shape.fill(color.eventFill()) }
            .overlay { if selected { selectionOverlay } }
            .foregroundStyle(color.eventInk())
    }

    /// The one place selection is drawn — every calendar surface routes
    /// through here, so this IS the calendar's selection language.
    ///
    /// Deliberately NEUTRAL, not the accent. Two reasons. The ring sits on a
    /// boundary with two neighbours — the block fill inside and the grid
    /// outside — and has to clear 3:1 against both; the accent comes from the
    /// user's profile and can be any hue, so it cannot guarantee that. And an
    /// accent ring would put the cursor on the same channel as the calendar
    /// colours it sits among, where a neutral one is the only thing on screen
    /// that is not a hue.
    ///
    /// strokeBorder insets by the line width, so the ring is drawn inside the
    /// block: a week block can be 16pt tall with no room to grow, and
    /// neighbouring lanes sit 1pt apart, so an outset ring would run into the
    /// block next to it.
    @ViewBuilder
    private var selectionOverlay: some View {
        shape.strokeBorder(Color.Detail.cursor, lineWidth: 2)
        shape.inset(by: 2).strokeBorder(Color.Detail.cardBackground, lineWidth: 1)
    }
}

extension View {
    /// Applies the shared event-block chrome around this content.
    func eventPill(_ color: Color, selected: Bool,
                   squareTop: Bool = false, squareBottom: Bool = false,
                   cornerRadius: CGFloat = 8) -> some View
    {
        modifier(EventPillChrome(color: color, selected: selected,
                                 squareTop: squareTop, squareBottom: squareBottom,
                                 cornerRadius: cornerRadius))
    }
}

// MARK: - Grid label

/// The label stack inside a week-grid block: the start time small and dimmed
/// on top (only when the block is tall enough to fit it) and the title under
/// it. Both inherit the block's ink from the chrome; the time is separated by
/// weight and opacity rather than by a second color.
struct EventPillGridLabel: View {
    let event: CalendarEvent
    let showsTime: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 1) {
            if showsTime {
                Text(CalendarTimeFormat.compact(event.start))
                    .font(.system(size: 9, weight: .medium))
                    .opacity(0.6)
            }
            Text(event.displaySubject)
                .font(.system(size: 11, weight: .semibold))
                .lineLimit(2)
        }
    }
}

// MARK: - Day number badge

/// A day-of-month number for date headers: a filled accent badge with
/// contrasting text when the day is today, plain primary text otherwise.
struct DayNumberBadge: View {
    /// Date headers (week/month/mini-month) use `.regular`; the year
    /// overview's twelve mini-months use `.mini`. Both come from here so the
    /// "today" badge cannot drift between them.
    enum Size {
        case regular, mini

        var font: Font {
            switch self {
            case .regular: return .callout
            case .mini: return .system(size: 9)
            }
        }

        var diameter: CGFloat {
            switch self {
            case .regular: return 22
            case .mini: return 14
            }
        }
    }

    let date: Date
    var size: Size = .regular

    var body: some View {
        let isToday = Calendar.current.isDateInToday(date)
        let accent = ProfileManager.shared.resolvedAccentColor
        Text("\(Calendar.current.component(.day, from: date))")
            .font(size.font)
            .fontWeight(isToday ? .semibold : .regular)
            .foregroundStyle(isToday ? accent.contrastingForeground() : Color.Detail.textPrimary)
            .frame(minWidth: size.diameter, minHeight: size.diameter)
            .background {
                if isToday {
                    Circle().fill(accent)
                }
            }
    }
}
