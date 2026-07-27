//
//  CalendarMonthView.swift
//  Durian
//
//  A classic month grid: 6 weeks × 7 days, each cell showing the day number and
//  up to three colored event chips. Tapping a chip selects the event; tapping
//  an empty part of a day drills into that day's agenda.
//

import SwiftUI

struct CalendarMonthView: View {
    @ObservedObject var manager = CalendarManager.shared
    private let calendar = Calendar.current

    var body: some View {
        VStack(spacing: 0) {
            weekdayHeader
            Divider()
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
                    .font(.caption2).foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity)
            }
        }
        .padding(.vertical, 4)
    }

    private func dayCell(_ day: Date) -> some View {
        let inMonth = calendar.isDate(day, equalTo: manager.anchorDate, toGranularity: .month)
        let isToday = calendar.isDateInToday(day)
        let dayEvents = eventsByDay[calendar.startOfDay(for: day)] ?? []
        return VStack(alignment: .leading, spacing: 2) {
            Text("\(calendar.component(.day, from: day))")
                .font(.caption)
                .fontWeight(isToday ? .bold : .regular)
                .foregroundStyle(dayNumberColor(inMonth: inMonth, isToday: isToday))
                .frame(maxWidth: .infinity, alignment: .trailing)

            ForEach(dayEvents.prefix(3)) { event in
                monthChip(event)
            }
            if dayEvents.count > 3 {
                Text("+\(dayEvents.count - 3) more")
                    .font(.system(size: 9)).foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .padding(4)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .background(isToday ? Color.accentColor.opacity(0.08) : Color.clear)
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
        return HStack(spacing: 3) {
            Circle().fill(color(for: event)).frame(width: 5, height: 5)
            Text(event.displaySubject)
                .font(.system(size: 10)).lineLimit(1)
                .foregroundStyle(Color.Detail.textBody)
        }
        .padding(.horizontal, 3).padding(.vertical, 1)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(selected ? Color.primary.opacity(0.14) : Color.clear, in: RoundedRectangle(cornerRadius: 3))
        .contentShape(Rectangle())
        .onTapGesture { manager.selectedEventID = event.id }
    }

    // MARK: - Styling helpers

    private func dayNumberColor(inMonth: Bool, isToday: Bool) -> Color {
        if !inMonth { return Color.secondary.opacity(0.5) }
        if isToday { return Color.accentColor }
        return Color.Detail.textPrimary
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
}
