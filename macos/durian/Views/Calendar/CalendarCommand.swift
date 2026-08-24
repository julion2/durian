//
//  CalendarCommand.swift
//  Durian
//
//  The grammar behind the calendar's `:` line.
//
//  Deliberately the same shape as the CLI's own vocabulary rather than a
//  natural-language guess: this app is configured in Pkl and driven by a Go
//  binary, so a command that reads like something you could have typed in a
//  terminal is the honest surface. It is also deterministic, which a
//  probabilistic parser is not — you can learn it once and then trust it.
//
//      :new Standup                       cursor day and time, one hour
//      :new 14:00 Standup                 cursor day at 14:00
//      :new 14:00 +90 Standup             ninety minutes
//      :new 14:00 +1h30 Standup           the same
//      :new 14:00 15:30 Standup           explicit end
//      :new tomorrow 9:00 Standup
//      :new 9:00 Standup :: with Ferdi    description after ::
//      :new 9:00 "Lunch on Tuesday"       quotes protect a title from parsing
//      :new 9:00 Standup -a Work Projects calendar by name prefix
//      :new 9:00 Standup -a me@example.com/Work
//
//      :modify 15:00                  keep date, title, and duration
//      :modify +90                    keep start; make it ninety minutes
//      :modify tomorrow 9:00 Standup  patch date, time, and title
//      :modify :: updated notes       patch notes only
//      :modify                         open the full editor
//
//      :today  :week  :month  :year  :agenda  :delete
//
//  The quoting escape is not a nicety. Every mature parser has one, because
//  without it a title containing a weekday or a time silently loses those
//  words to the date parser.
//

import Foundation

// MARK: - Result

enum CalendarCommand: Equatable {
    case none
    case create(start: Date, end: Date, title: String, notes: String?, calendarKey: String?)
    case modifySelected(CalendarEventPatch)
    case goToday
    case setView(CalendarViewMode)
    case editSelected
    case deleteSelected
    /// Parsed far enough to know it is wrong. The message is shown inline.
    case invalid(String)
}

/// The concrete changes produced by `:modify ...`. Nil text fields mean
/// "preserve"; an empty notes string means "clear". Start/end are concrete so
/// the preview and the write cannot resolve relative tokens differently.
struct CalendarEventPatch: Equatable {
    let start: Date
    let end: Date
    let title: String?
    let notes: String?
}

// MARK: - Parser

enum CalendarCommandParser {
    /// Parses a command line. `cursor` supplies every field the text leaves
    /// out — the day, and the time when none is typed.
    static func parse(_ raw: String, cursor: Date, calendars: [CalendarInfo],
                      selectedEvent: CalendarEvent? = nil,
                      calendar: Calendar = .current) -> CalendarCommand
    {
        var line = raw.trimmingCharacters(in: .whitespaces)
        if line.hasPrefix(":") { line.removeFirst() }
        guard !line.isEmpty else { return .none }

        let parts = line.split(separator: " ", maxSplits: 1, omittingEmptySubsequences: false)
        let verb = String(parts[0]).lowercased()
        let rest = parts.count > 1 ? String(parts[1]) : ""

        switch verb {
        case "new", "n":
            return parseNew(rest, cursor: cursor, calendars: calendars, calendar: calendar)
        case "today", "t":
            return withoutArguments(rest, command: .goToday, verb: verb)
        case "week", "w":
            return withoutArguments(rest, command: .setView(.week), verb: verb)
        case "month", "m":
            return withoutArguments(rest, command: .setView(.month), verb: verb)
        case "year", "y":
            return withoutArguments(rest, command: .setView(.year), verb: verb)
        case "agenda", "a":
            return withoutArguments(rest, command: .setView(.agenda), verb: verb)
        case "modify", "mod", "edit", "e":
            guard !rest.trimmingCharacters(in: .whitespaces).isEmpty else {
                return .editSelected
            }
            guard let selectedEvent else { return .invalid("Select an event first") }
            return parseModify(rest, event: selectedEvent, calendar: calendar)
        case "delete", "d":
            return withoutArguments(rest, command: .deleteSelected, verb: verb)
        default:
            return .invalid("Unknown command \(verb)")
        }
    }

    private static func withoutArguments(_ rest: String, command: CalendarCommand,
                                         verb: String) -> CalendarCommand
    {
        guard rest.trimmingCharacters(in: .whitespaces).isEmpty else {
            return .invalid("\(verb) takes no arguments")
        }
        return command
    }

    // MARK: - :new

    private static func parseNew(_ input: String, cursor: Date,
                                 calendars: [CalendarInfo], calendar: Calendar) -> CalendarCommand
    {
        var text = input

        // 1. `:: notes` — taken first so nothing after it is ever parsed as a
        //    date, however it reads.
        var notes: String?
        if let range = text.range(of: "::") {
            notes = String(text[range.upperBound...]).trimmingCharacters(in: .whitespaces)
            text = String(text[..<range.lowerBound])
            if notes?.isEmpty == true { notes = nil }
        }

        // 2. `-a name` — calendar by name prefix, case-insensitive. The flag
        //    consumes the rest of the command, so multi-word names work. When
        //    names collide across accounts, `account/name` disambiguates them.
        var calendarKey: String?
        if let flag = text.range(of: #"(?:^|\s)-a\s+(.+?)\s*$"#, options: .regularExpression) {
            let flagText = text[flag].trimmingCharacters(in: .whitespaces)
            let value = String(flagText.dropFirst(2)).trimmingCharacters(in: .whitespaces)
            let qualified = value.contains("/")
            let matches = calendars.filter {
                let candidate = qualified ? "\($0.account)/\($0.name)" : $0.name
                return candidate.lowercased().hasPrefix(value.lowercased())
            }
            guard !matches.isEmpty else { return .invalid("No calendar matching \(value)") }
            guard matches.count == 1, let match = matches.first else {
                return .invalid("Ambiguous calendar \(value); use account/name")
            }
            calendarKey = match.visibilityKey
            text.removeSubrange(flag)
        }

        // 3. A quoted span is the title verbatim, lifted out before any date
        //    parsing so a weekday inside it survives.
        var quotedTitle: String?
        if let open = text.firstIndex(of: "\""),
           let close = text.lastIndex(of: "\""), open < close
        {
            quotedTitle = String(text[text.index(after: open) ..< close])
            text.removeSubrange(open ... close)
        }

        // 4. Consume leading date/time/duration tokens; the rest is the title.
        var day = calendar.startOfDay(for: cursor)
        var startMinutes: Int?
        var endMinutes: Int?
        var durationMinutes: Int?

        var tokens = text.split(separator: " ").map(String.init)
        while let token = tokens.first {
            if let resolved = parseDayWord(token, relativeTo: cursor, calendar: calendar) {
                day = resolved
            } else if let minutes = parseDuration(token) {
                durationMinutes = minutes
            } else if let minutes = parseTime(token) {
                if startMinutes == nil { startMinutes = minutes } else { endMinutes = minutes }
            } else {
                break
            }
            tokens.removeFirst()
        }

        let title = quotedTitle ?? tokens.joined(separator: " ").trimmingCharacters(in: .whitespaces)
        guard !title.isEmpty else { return .invalid("Needs a title") }

        // The cursor supplies the time when none was typed — that is the whole
        // point of having a cursor.
        let cursorMinutes = calendar.component(.hour, from: cursor) * 60
            + calendar.component(.minute, from: cursor)
        let startMin = startMinutes ?? cursorMinutes
        var components = calendar.dateComponents([.year, .month, .day], from: day)
        components.hour = startMin / 60
        components.minute = startMin % 60
        guard let start = calendar.date(from: components) else { return .invalid("Invalid date") }

        let end: Date
        if let endMin = endMinutes {
            guard endMin > startMin else { return .invalid("Ends before it starts") }
            components.hour = endMin / 60
            components.minute = endMin % 60
            guard let explicitEnd = calendar.date(from: components) else {
                return .invalid("Invalid date")
            }
            end = explicitEnd
        } else {
            guard let durationEnd = calendar.date(byAdding: .minute,
                                                  value: durationMinutes ?? 60, to: start)
            else { return .invalid("Invalid date") }
            end = durationEnd
        }

        return .create(start: start, end: end, title: title, notes: notes, calendarKey: calendarKey)
    }

    // MARK: - :modify

    /// Parses the same leading date/time/duration vocabulary as `:new`, but as
    /// a patch against the selected event. Anything omitted stays unchanged.
    private static func parseModify(_ input: String, event: CalendarEvent,
                                    calendar: Calendar) -> CalendarCommand
    {
        var text = input

        // Unlike `:new`, an explicitly empty notes suffix means clear notes;
        // nil continues to mean the field was not mentioned.
        var notes: String?
        var mentionsNotes = false
        if let range = text.range(of: "::") {
            mentionsNotes = true
            notes = String(text[range.upperBound...]).trimmingCharacters(in: .whitespaces)
            text = String(text[..<range.lowerBound])
        }

        var quotedTitle: String?
        if let open = text.firstIndex(of: "\""),
           let close = text.lastIndex(of: "\""), open < close
        {
            quotedTitle = String(text[text.index(after: open) ..< close])
            text.removeSubrange(open ... close)
        }

        var day: Date?
        var startMinutes: Int?
        var endMinutes: Int?
        var durationMinutes: Int?
        var tokens = text.split(separator: " ").map(String.init)
        while let token = tokens.first {
            if let resolved = parseDayWord(token, relativeTo: event.start, calendar: calendar) {
                day = resolved
            } else if let minutes = parseDuration(token) {
                durationMinutes = minutes
            } else if let minutes = parseTime(token) {
                if startMinutes == nil { startMinutes = minutes } else { endMinutes = minutes }
            } else {
                break
            }
            tokens.removeFirst()
        }

        let unquotedTitle = tokens.joined(separator: " ").trimmingCharacters(in: .whitespaces)
        let title = quotedTitle ?? (unquotedTitle.isEmpty ? nil : unquotedTitle)
        guard day != nil || startMinutes != nil || endMinutes != nil || durationMinutes != nil
            || title != nil || mentionsNotes
        else { return .invalid("Needs a change") }

        if event.allDay && (startMinutes != nil || endMinutes != nil || durationMinutes != nil) {
            return .invalid("Use the editor to add a time to an all-day event")
        }

        let baseStart = event.recurring ? (event.seriesStart ?? event.start) : event.start
        let baseEnd = event.recurring ? (event.seriesEnd ?? event.end) : event.end
        let targetDay = day ?? calendar.startOfDay(for: baseStart)
        let baseStartMinutes = calendar.component(.hour, from: baseStart) * 60
            + calendar.component(.minute, from: baseStart)
        let targetStartMinutes = startMinutes ?? baseStartMinutes
        var components = calendar.dateComponents([.year, .month, .day], from: targetDay)
        components.hour = targetStartMinutes / 60
        components.minute = targetStartMinutes % 60
        guard let start = calendar.date(from: components) else { return .invalid("Invalid date") }

        let end: Date
        if let explicitEndMinutes = endMinutes {
            guard explicitEndMinutes > targetStartMinutes else {
                return .invalid("Ends before it starts")
            }
            components.hour = explicitEndMinutes / 60
            components.minute = explicitEndMinutes % 60
            guard let explicitEnd = calendar.date(from: components) else {
                return .invalid("Invalid date")
            }
            end = explicitEnd
        } else if let durationMinutes {
            guard let durationEnd = calendar.date(byAdding: .minute, value: durationMinutes, to: start)
            else { return .invalid("Invalid date") }
            end = durationEnd
        } else if event.allDay {
            // Preserve calendar-day length rather than elapsed seconds: a
            // one-day event crossing DST can be 23 or 25 hours but must still
            // end at local midnight after being moved.
            let baseStartDay = calendar.startOfDay(for: baseStart)
            let baseEndDay = calendar.startOfDay(for: baseEnd)
            let days = max(1, calendar.dateComponents([.day], from: baseStartDay,
                                                       to: baseEndDay).day ?? 1)
            guard let allDayEnd = calendar.date(byAdding: .day, value: days, to: start)
            else { return .invalid("Invalid date") }
            end = allDayEnd
        } else {
            end = start.addingTimeInterval(baseEnd.timeIntervalSince(baseStart))
        }

        return .modifySelected(CalendarEventPatch(
            start: start, end: end, title: title,
            notes: mentionsNotes ? (notes ?? "") : nil
        ))
    }

    // MARK: - Tokens

    /// `today`, `tomorrow`, or a weekday name — weekdays resolve FORWARD, the
    /// convention every calendar parser uses: "monday" in a calendar means the
    /// next one, never the one that already passed.
    static func parseDayWord(_ token: String, relativeTo cursor: Date,
                             calendar: Calendar = .current) -> Date?
    {
        let word = token.lowercased()
        let today = calendar.startOfDay(for: cursor)
        if word == "today" { return today }
        if word == "tomorrow" || word == "tmr" {
            return calendar.date(byAdding: .day, value: 1, to: today)
        }
        let symbols = ["sunday", "monday", "tuesday", "wednesday",
                       "thursday", "friday", "saturday"]
        guard let index = symbols.firstIndex(where: { $0.hasPrefix(word) }), word.count >= 2 else {
            return nil
        }
        let target = index + 1 // Calendar weekdays are 1-based
        let current = calendar.component(.weekday, from: today)
        let delta = (target - current + 7) % 7
        return calendar.date(byAdding: .day, value: delta == 0 ? 7 : delta, to: today)
    }

    /// `14:00`, `1400`, `14`, `2pm`, `2:30pm` -> minutes from midnight.
    static func parseTime(_ token: String) -> Int? {
        let text = token.lowercased()
        var body = text
        var pm = false
        var explicitMeridiem = false
        if body.hasSuffix("pm") { pm = true; explicitMeridiem = true; body.removeLast(2) }
        else if body.hasSuffix("am") { explicitMeridiem = true; body.removeLast(2) }

        var hour: Int
        var minute = 0
        if let colon = body.firstIndex(of: ":") {
            guard let h = Int(body[..<colon]), let m = Int(body[body.index(after: colon)...]) else {
                return nil
            }
            hour = h; minute = m
        } else if body.count == 4, let value = Int(body) {
            hour = value / 100; minute = value % 100
        } else if let value = Int(body), body.count <= 2 {
            hour = value
        } else {
            return nil
        }

        if explicitMeridiem {
            if pm && hour < 12 { hour += 12 }
            if !pm && hour == 12 { hour = 0 }
        }
        guard (0 ..< 24).contains(hour), (0 ..< 60).contains(minute) else { return nil }
        return hour * 60 + minute
    }

    /// `+90`, `+30m`, `+1h`, `+1h30`, `+1h30m` -> minutes.
    static func parseDuration(_ token: String) -> Int? {
        guard token.hasPrefix("+") else { return nil }
        var body = Substring(token.dropFirst().lowercased())
        guard !body.isEmpty else { return nil }

        var minutes = 0
        if let h = body.firstIndex(of: "h") {
            guard let hours = Int(body[..<h]) else { return nil }
            minutes += hours * 60
            body = body[body.index(after: h)...]
        }
        if body.hasSuffix("m") { body = body.dropLast() }
        if !body.isEmpty {
            guard let mins = Int(String(body)) else { return nil }
            minutes += mins
        }
        return minutes > 0 ? minutes : nil
    }
}
