//
//  MiniMonthView.swift
//  Durian
//
//  Compact month navigator at the top of the calendar sidebar: a small
//  month/year header with prev/next chevrons, weekday initials, and
//  tappable day cells. Tapping a day re-anchors the main calendar view on
//  it; the chevrons only page the mini grid (browsing months does not move
//  the main view until a day is picked). Today gets the shared accent
//  badge, the anchored day a soft accent wash.
//

import SwiftUI

struct MiniMonthView: View {
    @ObservedObject var manager = CalendarManager.shared

    /// The month the grid shows. Starts on (and follows) the main view's
    /// anchor, but pages independently via the chevrons.
    @State private var displayedMonth: Date = CalendarManager.shared.anchorDate

    private var cal: Foundation.Calendar { .current }

    var body: some View {
        VStack(spacing: 6) {
            header
            weekdayRow
            dayGrid
        }
        .onChange(of: manager.anchorDate) { _, anchor in
            // The main view navigated (today / step / search): snap the mini
            // grid back onto the month the calendar actually shows.
            displayedMonth = anchor
        }
    }

    // MARK: - Header

    private var header: some View {
        HStack(spacing: 8) {
            Text(Self.monthFormatter.string(from: displayedMonth))
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(Color.Detail.textPrimary)
            Spacer()
            Button { stepMonth(-1) } label: {
                Image(systemName: "chevron.left")
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundStyle(Color.Detail.textSecondary)
            }
            .buttonStyle(.plain)
            .help("Previous month")
            Button { stepMonth(1) } label: {
                Image(systemName: "chevron.right")
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundStyle(Color.Detail.textSecondary)
            }
            .buttonStyle(.plain)
            .help("Next month")
        }
    }

    private func stepMonth(_ direction: Int) {
        displayedMonth = cal.date(byAdding: .month, value: direction, to: displayedMonth)
            ?? displayedMonth
    }

    // MARK: - Weekday initials

    private var weekdayRow: some View {
        HStack(spacing: 0) {
            ForEach(Array(weekdaySymbols.enumerated()), id: \.offset) { _, symbol in
                Text(symbol)
                    .font(.system(size: 9, weight: .medium))
                    .foregroundStyle(Color.Detail.textTertiary)
                    .frame(maxWidth: .infinity)
            }
        }
    }

    /// Locale weekday initials rotated so the row starts on the calendar's
    /// first weekday (Monday in most European locales, Sunday in the US).
    private var weekdaySymbols: [String] {
        let symbols = cal.veryShortWeekdaySymbols
        let first = cal.firstWeekday - 1
        return Array(symbols[first...] + symbols[..<first])
    }

    // MARK: - Day grid

    private var dayGrid: some View {
        LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 0), count: 7),
                  spacing: 2)
        {
            ForEach(Array(monthCells().enumerated()), id: \.offset) { _, day in
                if let day {
                    dayCell(day)
                } else {
                    Color.clear.frame(height: 20)
                }
            }
        }
    }

    private func dayCell(_ day: Date) -> some View {
        let isAnchor = cal.isDate(day, inSameDayAs: manager.anchorDate)
        let isToday = cal.isDateInToday(day)
        return Button {
            manager.anchorDate = day
            manager.refresh()
        } label: {
            // DayNumberBadge already renders today's filled accent circle;
            // the anchored (non-today) day gets a soft accent wash instead.
            DayNumberBadge(date: day)
                .background {
                    if isAnchor && !isToday {
                        Circle().fill(Color.accentColor.opacity(0.2))
                    }
                }
                .frame(maxWidth: .infinity)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    /// The displayed month's days, prefixed with nil placeholders so the
    /// first day lands in its weekday column. No adjacent-month spill in v1.
    private func monthCells() -> [Date?] {
        guard let interval = cal.dateInterval(of: .month, for: displayedMonth),
              let dayCount = cal.range(of: .day, in: .month, for: displayedMonth)?.count
        else { return [] }
        let first = interval.start
        let leading = (cal.component(.weekday, from: first) - cal.firstWeekday + 7) % 7
        var cells = [Date?](repeating: nil, count: leading)
        for offset in 0..<dayCount {
            cells.append(cal.date(byAdding: .day, value: offset, to: first))
        }
        return cells
    }

    private static let monthFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "MMMM yyyy"
        return f
    }()
}
