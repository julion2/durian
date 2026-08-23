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
        // Computed ONCE per render and passed down: the year view holds ~370
        // day cells, and deriving the marker inside each cell re-scanned the
        // whole event list per cell.
        let eventDays = daysWithEvents
        return ScrollView {
            LazyVGrid(
                columns: [GridItem(.adaptive(minimum: 170, maximum: 260), spacing: 20)],
                spacing: 20
            ) {
                ForEach(monthStarts, id: \.self) { monthStart in
                    miniMonth(monthStart, eventDays: eventDays)
                }
            }
            .padding()
        }
    }

    private func miniMonth(_ monthStart: Date, eventDays: Set<Date>) -> some View {
        let days = gridDays(monthStart)
        let isCurrentMonth = calendar.isDate(monthStart, equalTo: Date(), toGranularity: .month)
        return VStack(spacing: 4) {
            Text(Self.monthFormatter.string(from: monthStart))
                .font(.caption).fontWeight(.semibold)
                .foregroundStyle(isCurrentMonth ? ProfileManager.shared.resolvedAccentColor
                                                : Color.Detail.textPrimary)
                .frame(maxWidth: .infinity, alignment: .leading)

            LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 1), count: 7), spacing: 2) {
                ForEach(weekdayInitials.indices, id: \.self) { i in
                    Text(weekdayInitials[i])
                        .font(.system(size: 8, weight: .medium))
                        .foregroundStyle(Color.Detail.textTertiary)
                }
                ForEach(days, id: \.self) { day in
                    dayCell(day, monthStart: monthStart, eventDays: eventDays)
                }
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color.Detail.cardBackground))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(Color.Detail.border, lineWidth: 0.5))
        .contentShape(Rectangle())
        .onTapGesture {
            manager.anchorDate = monthStart
            manager.setViewMode(.month)
        }
    }

    @ViewBuilder
    private func dayCell(_ day: Date, monthStart: Date, eventDays: Set<Date>) -> some View {
        if calendar.isDate(day, equalTo: monthStart, toGranularity: .month) {
            let hasEvents = eventDays.contains(calendar.startOfDay(for: day))
            VStack(spacing: 1) {
                DayNumberBadge(date: day, size: .mini)
                Circle()
                    .fill(hasEvents ? ProfileManager.shared.resolvedAccentColor : Color.clear)
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

    /// The year overview only marks days, so a Set of start days is all it
    /// needs — no per-day event arrays.
    private var daysWithEvents: Set<Date> {
        Set(manager.events.map { calendar.startOfDay(for: $0.start) })
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
