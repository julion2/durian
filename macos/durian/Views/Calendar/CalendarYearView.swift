//
//  CalendarYearView.swift
//  Durian
//
//  Year overview: twelve mini-months in an adaptive grid. Days with events get
//  a dot; tapping a month drills into the month view.
//

import SwiftUI

struct CalendarYearView: View {
    @ObservedObject var manager = CalendarManager.shared
    private let calendar = Calendar.current

    var body: some View {
        ScrollView {
            LazyVGrid(
                columns: [GridItem(.adaptive(minimum: 170, maximum: 260), spacing: 20)],
                spacing: 20
            ) {
                ForEach(monthStarts, id: \.self) { monthStart in
                    miniMonth(monthStart)
                }
            }
            .padding()
        }
    }

    private func miniMonth(_ monthStart: Date) -> some View {
        let days = gridDays(monthStart)
        return VStack(spacing: 4) {
            Text(Self.monthFormatter.string(from: monthStart))
                .font(.caption).fontWeight(.semibold)
                .frame(maxWidth: .infinity, alignment: .leading)

            LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 1), count: 7), spacing: 2) {
                ForEach(weekdayInitials.indices, id: \.self) { i in
                    Text(weekdayInitials[i]).font(.system(size: 8)).foregroundStyle(.secondary)
                }
                ForEach(days, id: \.self) { day in
                    dayCell(day, monthStart: monthStart)
                }
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color(nsColor: .controlBackgroundColor)))
        .contentShape(Rectangle())
        .onTapGesture {
            manager.anchorDate = monthStart
            manager.setViewMode(.month)
        }
    }

    @ViewBuilder
    private func dayCell(_ day: Date, monthStart: Date) -> some View {
        if calendar.isDate(day, equalTo: monthStart, toGranularity: .month) {
            let isToday = calendar.isDateInToday(day)
            let hasEvents = !(eventsByDay[calendar.startOfDay(for: day)] ?? []).isEmpty
            VStack(spacing: 1) {
                Text("\(calendar.component(.day, from: day))")
                    .font(.system(size: 9))
                    .fontWeight(isToday ? .bold : .regular)
                    .foregroundStyle(isToday ? Color.accentColor : Color.Detail.textPrimary)
                Circle()
                    .fill(hasEvents ? Color.accentColor : Color.clear)
                    .frame(width: 3, height: 3)
            }
            .frame(height: 20)
        } else {
            Color.clear.frame(height: 20)
        }
    }

    // MARK: - Data

    private var monthStarts: [Date] {
        guard let yearStart = calendar.dateInterval(of: .year, for: manager.anchorDate)?.start else { return [] }
        return (0 ..< 12).compactMap { calendar.date(byAdding: .month, value: $0, to: yearStart) }
    }

    private func gridDays(_ monthStart: Date) -> [Date] {
        let weekday = calendar.component(.weekday, from: monthStart)
        let offset = (weekday - calendar.firstWeekday + 7) % 7
        let daysInMonth = calendar.range(of: .day, in: .month, for: monthStart)?.count ?? 30
        let cells = Int((Double(offset + daysInMonth) / 7).rounded(.up)) * 7
        guard let gridStart = calendar.date(byAdding: .day, value: -offset, to: monthStart) else { return [] }
        return (0 ..< cells).compactMap { calendar.date(byAdding: .day, value: $0, to: gridStart) }
    }

    private var eventsByDay: [Date: [CalendarEvent]] {
        Dictionary(grouping: manager.events) { calendar.startOfDay(for: $0.start) }
    }

    private var weekdayInitials: [String] {
        let symbols = calendar.veryShortWeekdaySymbols
        let first = calendar.firstWeekday - 1
        return Array(symbols[first...] + symbols[..<first])
    }

    private static let monthFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "MMMM"; return f
    }()
}
