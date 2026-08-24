//
//  CalendarTimeFormat.swift
//  Durian
//
//  One clock for the whole calendar.
//
//  The grid, the month chips and the time axis used to hardcode 24-hour
//  "HH:mm" (to keep an AM/PM suffix from eating a narrow column) while the
//  agenda rows and the detail pane formatted by locale. On a 12-hour locale
//  that put two different clocks on one screen — "2:30 PM" in the agenda next
//  to "14:30" in the month cell. Everything now goes through the skeletons
//  below, which resolve to 24-hour or 12-hour per locale but stay consistent
//  with each other.
//

import Foundation

enum CalendarTimeFormat {
    /// Hour and minute: "14:30" on a 24-hour locale, "2:30 PM" on a 12-hour
    /// one. Used by grid blocks, month chips, agenda rows and the detail
    /// pane alike.
    static func time(_ date: Date) -> String {
        timeFormatter.string(from: date)
    }

    /// The same clock, shortened for tight surfaces: a whole hour drops its
    /// ":00" ("14", "2 PM"), because in a ~40pt grid lane the minutes of an
    /// on-the-hour event are the least useful glyphs on the block.
    static func compact(_ date: Date) -> String {
        let minute = Calendar.current.component(.minute, from: date)
        return minute == 0 ? hourFormatter.string(from: date) : timeFormatter.string(from: date)
    }

    /// An hour label for the week grid's vertical axis: "14" / "2 PM".
    static func axisHour(_ hour: Int) -> String {
        var comps = DateComponents()
        comps.hour = hour
        comps.minute = 0
        guard let date = Calendar.current.date(from: comps) else { return "\(hour)" }
        return hourFormatter.string(from: date)
    }

    // MARK: - Formatters

    /// "j" is the skeleton for "hour in the locale's preferred cycle", so
    /// this is the one place the 12h/24h decision is made.
    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter()
        f.locale = .current
        f.dateFormat = DateFormatter.dateFormat(fromTemplate: "jmm", options: 0,
                                                locale: .current) ?? "HH:mm"
        return f
    }()

    private static let hourFormatter: DateFormatter = {
        let f = DateFormatter()
        f.locale = .current
        f.dateFormat = DateFormatter.dateFormat(fromTemplate: "j", options: 0,
                                                locale: .current) ?? "HH"
        return f
    }()
}
