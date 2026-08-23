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
                // Grouped ONCE per render and passed down: dayCell runs 42
                // times per body, and rebuilding the grouping dictionary in
                // each cell made one render cost 42 passes over all events.
                let byDay = eventsByDay
                VStack(spacing: 0) {
                    ForEach(rows.indices, id: \.self) { r in
                        HStack(spacing: 0) {
                            ForEach(rows[r], id: \.self) { day in
                                dayCell(day, events: byDay[calendar.startOfDay(for: day)] ?? [])
                                    .frame(height: rowHeight)
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

    private func dayCell(_ day: Date, events dayEvents: [CalendarEvent]) -> some View {
        let inMonth = calendar.isDate(day, equalTo: manager.anchorDate, toGranularity: .month)
        return VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 4) {
                DayNumberBadge(date: day)
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
        // Show the start time when the cell is wide enough for it plus the
        // title; otherwise fall back to title only. An all-day event has no
        // time to show, which is exactly what distinguishes it here — the
        // fill no longer does.
        return ViewThatFits(in: .horizontal) {
            chipLabel(event, withTime: !event.allDay)
            chipLabel(event, withTime: false)
        }
        .padding(.horizontal, 5)
        .padding(.vertical, 2)
        .frame(maxWidth: .infinity, alignment: .leading)
        .eventPill(color(for: event), selected: selected, cornerRadius: 5)
        .contentShape(Rectangle())
        .calendarEventInteractions(event)
    }

    @ViewBuilder
    private func chipLabel(_ event: CalendarEvent, withTime: Bool) -> some View {
        if withTime {
            HStack(spacing: 4) {
                Text(CalendarTimeFormat.compact(event.start))
                    .font(.system(size: 10, weight: .medium))
                    .opacity(0.6)
                    .fixedSize()
                Text(event.displaySubject)
                    .font(.system(size: 10, weight: .semibold)).lineLimit(1)
                    .fixedSize()
            }
        } else {
            Text(event.displaySubject)
                .font(.system(size: 10, weight: .semibold)).lineLimit(1)
        }
    }

    // MARK: - Styling helpers

    private func line(width: CGFloat?, height: CGFloat?) -> some View {
        Rectangle().fill(Color.Detail.border).frame(width: width, height: height)
    }

    private func color(for event: CalendarEvent) -> Color {
        manager.color(for: event)
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

}
