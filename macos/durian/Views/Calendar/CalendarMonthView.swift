//
//  CalendarMonthView.swift
//  Durian
//
//  A classic month grid: 6 weeks × 7 days, each cell showing the day number and
//  up to three events in the shared pill style (EventPill.swift). Tapping a
//  pill selects the event; tapping an empty part of a day drills into that
//  day's agenda.
//

import SwiftUI

struct CalendarMonthView: View {
    @ObservedObject var manager = CalendarManager.shared
    private let calendar = Calendar.current

    var body: some View {
        VStack(spacing: 0) {
            weekdayHeader
            line(width: nil, height: 0.5)
            GeometryReader { geo in
                let rows = weeks
                let rowHeight = geo.size.height / CGFloat(max(rows.count, 1))
                VStack(spacing: 0) {
                    ForEach(rows.indices, id: \.self) { r in
                        HStack(spacing: 0) {
                            ForEach(rows[r], id: \.self) { day in
                                dayCell(day).frame(height: rowHeight)
                            }
                        }
                    }
                }
            }
        }
    }

    private var weekdayHeader: some View {
        HStack(spacing: 0) {
            ForEach(weekdaySymbols, id: \.self) { symbol in
                Text(symbol)
                    .font(.caption2).fontWeight(.semibold)
                    .foregroundStyle(Color.Detail.textSecondary)
                    .frame(maxWidth: .infinity)
            }
        }
        .padding(.vertical, 6)
    }

    private func dayCell(_ day: Date) -> some View {
        let inMonth = calendar.isDate(day, equalTo: manager.anchorDate, toGranularity: .month)
        let dayEvents = eventsByDay[calendar.startOfDay(for: day)] ?? []
        return VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 4) {
                DayNumberBadge(date: day)
                if calendar.component(.day, from: day) == 1 {
                    monthBoundaryLabel(day)
                }
                Spacer(minLength: 0)
            }

            ForEach(dayEvents.prefix(3)) { event in
                monthChip(event)
            }
            if dayEvents.count > 3 {
                Text("+\(dayEvents.count - 3) more")
                    .font(.system(size: 9, weight: .medium))
                    .foregroundStyle(Color.Detail.textTertiary)
                    .padding(.leading, 4)
            }
            Spacer(minLength: 0)
        }
        .padding(4)
        // Days spilling in from the neighbouring months stay visible but
        // clearly secondary.
        .opacity(inMonth ? 1 : 0.45)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .overlay(alignment: .bottom) { line(width: nil, height: 0.5) }
        .overlay(alignment: .trailing) { line(width: 0.5, height: nil) }
        .contentShape(Rectangle())
        .onTapGesture {
            manager.anchorDate = day
            manager.setViewMode(.agenda)
        }
    }

    private func monthChip(_ event: CalendarEvent) -> some View {
        let selected = manager.selectedEventID == event.id
        let variant: EventPillChrome.Variant = event.allDay ? .allDay : .timed
        // Show the start time right-aligned when the cell is wide enough for
        // the full title plus the time; otherwise fall back to title only.
        return ViewThatFits(in: .horizontal) {
            chipLabel(event, variant: variant, withTime: !event.allDay)
            chipLabel(event, variant: variant, withTime: false)
        }
        // The timed variant draws a 3pt accent bar on the leading edge; pad
        // past it so the title doesn't touch the bar.
        .padding(.leading, variant == .timed ? 7 : 5)
        .padding(.trailing, 5)
        .padding(.vertical, 2)
        .frame(maxWidth: .infinity, alignment: .leading)
        .eventPill(color(for: event), variant: variant, selected: selected, cornerRadius: 4)
        .contentShape(Rectangle())
        .onTapGesture { manager.selectedEventID = event.id }
    }

    @ViewBuilder
    private func chipLabel(_ event: CalendarEvent, variant: EventPillChrome.Variant,
                           withTime: Bool) -> some View {
        if withTime {
            HStack(spacing: 4) {
                Text(event.displaySubject)
                    .font(.system(size: 10, weight: .medium)).lineLimit(1)
                    .fixedSize()
                Spacer(minLength: 4)
                Text(Self.timeFormatter.string(from: event.start))
                    .font(.system(size: 9))
                    .foregroundStyle(Color.Detail.textSecondary)
                    .fixedSize()
            }
        } else {
            Text(event.displaySubject)
                .font(.system(size: 10, weight: .medium)).lineLimit(1)
        }
    }

    // MARK: - Styling helpers

    /// A compact inverted pill naming the month, shown on its first day so
    /// month boundaries stay readable inside the continuous six-week grid.
    private func monthBoundaryLabel(_ day: Date) -> some View {
        Text(Self.monthAbbrevFormatter.string(from: day))
            .font(.system(size: 9, weight: .semibold))
            .foregroundStyle(Color.Detail.cardBackground)
            .padding(.horizontal, 6).padding(.vertical, 2)
            .background(Capsule().fill(Color.Detail.textPrimary))
    }

    private func line(width: CGFloat?, height: CGFloat?) -> some View {
        Rectangle().fill(Color.Detail.border).frame(width: width, height: height)
    }

    private func color(for event: CalendarEvent) -> Color {
        manager.calendars.first { $0.name == event.calendar }?.color ?? .secondary
    }

    // MARK: - Grid computation

    private var weeks: [[Date]] {
        let days = monthGridDays()
        return stride(from: 0, to: days.count, by: 7).map { Array(days[$0 ..< min($0 + 7, days.count)]) }
    }

    private func monthGridDays() -> [Date] {
        guard let interval = calendar.dateInterval(of: .month, for: manager.anchorDate) else { return [] }
        let first = interval.start
        let weekday = calendar.component(.weekday, from: first)
        let offset = (weekday - calendar.firstWeekday + 7) % 7
        guard let gridStart = calendar.date(byAdding: .day, value: -offset, to: first) else { return [] }
        return (0 ..< 42).compactMap { calendar.date(byAdding: .day, value: $0, to: gridStart) }
    }

    private var eventsByDay: [Date: [CalendarEvent]] {
        Dictionary(grouping: manager.events) { calendar.startOfDay(for: $0.start) }
    }

    private var weekdaySymbols: [String] {
        let symbols = calendar.shortWeekdaySymbols
        let first = calendar.firstWeekday - 1
        return Array(symbols[first...] + symbols[..<first])
    }

    // MARK: - Formatters

    /// Fixed 24h "HH:mm", matching the week grid: a locale AM/PM suffix would
    /// eat most of a narrow month cell.
    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "HH:mm"; return f
    }()

    private static let monthAbbrevFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "MMM"; return f
    }()
}
