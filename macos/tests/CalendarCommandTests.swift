@testable import durian_lib
import XCTest

/// The `:` grammar. Pure parsing, so it is worth pinning precisely — a
/// command line whose behaviour drifts is worse than no command line.
final class CalendarCommandTests: XCTestCase {

    private let cal = Calendar.current

    /// Wed 2026-08-19, 14:30 — the cursor every case parses against.
    private var cursor: Date {
        var c = DateComponents()
        c.year = 2026; c.month = 8; c.day = 19; c.hour = 14; c.minute = 30
        return cal.date(from: c)!
    }

    private func parse(_ line: String, calendars: [CalendarInfo] = [],
                       event: CalendarEvent? = nil, calendar: Calendar? = nil) -> CalendarCommand
    {
        CalendarCommandParser.parse(line, cursor: cursor, calendars: calendars, selectedEvent: event,
                                    calendar: calendar ?? cal)
    }

    private var selectedEvent: CalendarEvent {
        CalendarEvent(uid: "selected", calendar: "Work", subject: "Standup",
                      start: cursor, end: cal.date(byAdding: .minute, value: 45, to: cursor)!,
                      description: "Original notes", account: "a@b.c")
    }

    /// Unwraps a `.create`, failing the test with a useful message otherwise.
    private func create(_ line: String, calendars: [CalendarInfo] = [],
                        file: StaticString = #filePath, line lineNo: UInt = #line)
        -> (start: Date, end: Date, title: String, notes: String?, key: String?)?
    {
        guard case .create(let s, let e, let t, let n, let k) = parse(line, calendars: calendars) else {
            XCTFail("expected a create for \(line)", file: file, line: lineNo)
            return nil
        }
        return (s, e, t, n, k)
    }

    // MARK: - Tokens

    func testParsesTimesInEveryAcceptedShape() {
        XCTAssertEqual(CalendarCommandParser.parseTime("14:00"), 14 * 60)
        XCTAssertEqual(CalendarCommandParser.parseTime("1400"), 14 * 60)
        XCTAssertEqual(CalendarCommandParser.parseTime("14"), 14 * 60)
        XCTAssertEqual(CalendarCommandParser.parseTime("2pm"), 14 * 60)
        XCTAssertEqual(CalendarCommandParser.parseTime("2:30pm"), 14 * 60 + 30)
        XCTAssertEqual(CalendarCommandParser.parseTime("12am"), 0)
        XCTAssertEqual(CalendarCommandParser.parseTime("12pm"), 12 * 60)
    }

    func testRejectsNonTimes() {
        XCTAssertNil(CalendarCommandParser.parseTime("Standup"))
        XCTAssertNil(CalendarCommandParser.parseTime("25"))
        XCTAssertNil(CalendarCommandParser.parseTime("14:75"))
        XCTAssertNil(CalendarCommandParser.parseTime(""))
    }

    func testParsesDurations() {
        XCTAssertEqual(CalendarCommandParser.parseDuration("+90"), 90)
        XCTAssertEqual(CalendarCommandParser.parseDuration("+30m"), 30)
        XCTAssertEqual(CalendarCommandParser.parseDuration("+1h"), 60)
        XCTAssertEqual(CalendarCommandParser.parseDuration("+1h30"), 90)
        XCTAssertEqual(CalendarCommandParser.parseDuration("+1h30m"), 90)
        XCTAssertNil(CalendarCommandParser.parseDuration("90"), "needs the leading +")
        XCTAssertNil(CalendarCommandParser.parseDuration("+"))
    }

    func testWeekdaysResolveForward() {
        // The cursor is a Wednesday; "monday" must mean the coming one, never
        // the one that already passed.
        let monday = CalendarCommandParser.parseDayWord("monday", relativeTo: cursor)
        XCTAssertNotNil(monday)
        XCTAssertGreaterThan(monday!, cursor)
        XCTAssertEqual(cal.component(.weekday, from: monday!), 2)

        // The cursor's own weekday means next week, not today.
        let wednesday = CalendarCommandParser.parseDayWord("wednesday", relativeTo: cursor)
        XCTAssertEqual(cal.dateComponents([.day], from: cursor, to: wednesday!).day, 6)
    }

    func testEnglishWeekdaysDoNotDependOnLocale() {
        var german = Calendar(identifier: .gregorian)
        german.locale = Locale(identifier: "de_DE")
        german.timeZone = cal.timeZone
        let monday = CalendarCommandParser.parseDayWord("monday", relativeTo: cursor,
                                                        calendar: german)
        XCTAssertEqual(german.component(.weekday, from: monday!), 2)
    }

    // MARK: - :new

    func testBareTitleUsesCursorTimeAndOneHour() {
        guard let r = create(":new Standup") else { return }
        XCTAssertEqual(r.title, "Standup")
        XCTAssertEqual(cal.component(.hour, from: r.start), 14)
        XCTAssertEqual(cal.component(.minute, from: r.start), 30)
        XCTAssertEqual(r.end.timeIntervalSince(r.start), 3600)
    }

    func testExplicitTimeKeepsCursorDay() {
        guard let r = create(":new 9:00 Standup") else { return }
        XCTAssertEqual(r.title, "Standup")
        XCTAssertEqual(cal.component(.day, from: r.start), 19)
        XCTAssertEqual(cal.component(.hour, from: r.start), 9)
    }

    func testDurationAndExplicitEndBothWork() {
        guard let a = create(":new 9:00 +90 Standup") else { return }
        XCTAssertEqual(a.end.timeIntervalSince(a.start), 90 * 60)

        guard let b = create(":new 9:00 10:30 Standup") else { return }
        XCTAssertEqual(b.end.timeIntervalSince(b.start), 90 * 60)
    }

    func testTomorrowShiftsTheDay() {
        guard let r = create(":new tomorrow 9:00 Standup") else { return }
        XCTAssertEqual(cal.component(.day, from: r.start), 20)
    }

    func testWallClockTimeSurvivesDSTTransition() {
        var berlin = Calendar(identifier: .gregorian)
        berlin.timeZone = TimeZone(identifier: "Europe/Berlin")!
        for day in [(month: 3, day: 29), (month: 10, day: 25)] {
            let cursor = berlin.date(from: DateComponents(year: 2026, month: day.month,
                                                           day: day.day, hour: 14, minute: 30))!
            guard case .create(let start, let end, _, _, _) = CalendarCommandParser.parse(
                ":new 9:00 Standup", cursor: cursor, calendars: [], calendar: berlin
            ) else {
                XCTFail("expected create"); return
            }
            XCTAssertEqual(berlin.component(.hour, from: start), 9)
            XCTAssertEqual(end.timeIntervalSince(start), 3600)
        }
    }

    func testNotesComeAfterDoubleColon() {
        guard let r = create(":new 9:00 Standup :: with Ferdi") else { return }
        XCTAssertEqual(r.title, "Standup")
        XCTAssertEqual(r.notes, "with Ferdi")
    }

    func testNotesAreNeverParsedAsDates() {
        // "tomorrow" inside the notes must not move the event.
        guard let r = create(":new 9:00 Standup :: tomorrow 3pm") else { return }
        XCTAssertEqual(cal.component(.day, from: r.start), 19)
        XCTAssertEqual(r.notes, "tomorrow 3pm")
    }

    func testQuotesProtectATitleFromTheDateParser() {
        // Without quotes the parser would eat "tuesday" out of the title.
        // This is the escape every mature grammar has.
        guard let r = create(#":new 9:00 "Lunch on tuesday""#) else { return }
        XCTAssertEqual(r.title, "Lunch on tuesday")
        XCTAssertEqual(cal.component(.day, from: r.start), 19)
    }

    func testTitlesKeepWordsAfterTheFirstNonDateToken() {
        guard let r = create(":new 9:00 Sync with 3 people") else { return }
        XCTAssertEqual(r.title, "Sync with 3 people")
    }

    func testCalendarFlagMatchesByPrefix() {
        let work = CalendarInfo(name: "Work", account: "a@b.c")
        let home = CalendarInfo(name: "Home", account: "a@b.c")
        guard let r = create(":new 9:00 Standup -a wo", calendars: [work, home]) else { return }
        XCTAssertEqual(r.title, "Standup")
        XCTAssertEqual(r.key, work.visibilityKey)
    }

    func testCalendarFlagSupportsMultiWordAndInternalDashA() {
        let calendar = CalendarInfo(name: "Sales-App Projects", account: "a@b.c")
        guard let r = create(":new 9:00 Standup -a Sales-App Projects",
                             calendars: [calendar]) else { return }
        XCTAssertEqual(r.title, "Standup")
        XCTAssertEqual(r.key, calendar.visibilityKey)
    }

    func testDuplicateCalendarNamesRequireAccount() {
        let first = CalendarInfo(name: "Work", account: "first@example.com")
        let second = CalendarInfo(name: "Work", account: "second@example.com")
        guard case .invalid = parse(":new 9:00 Standup -a Work", calendars: [first, second]) else {
            XCTFail("expected ambiguous calendar"); return
        }
        guard let r = create(":new 9:00 Standup -a second@example.com/Work",
                             calendars: [first, second]) else { return }
        XCTAssertEqual(r.key, second.visibilityKey)
    }

    func testUnknownCalendarIsRejected() {
        let work = CalendarInfo(name: "Work", account: "a@b.c")
        guard case .invalid = parse(":new 9:00 Standup -a zzz", calendars: [work]) else {
            XCTFail("expected invalid"); return
        }
    }

    // MARK: - Errors and other verbs

    func testModifyPatchesOnlySuppliedFields() {
        guard case .modifySelected(let time) = parse(":modify 9:00", event: selectedEvent) else {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(cal.component(.hour, from: time.start), 9)
        XCTAssertEqual(time.end.timeIntervalSince(time.start), 45 * 60)
        XCTAssertNil(time.title)
        XCTAssertNil(time.notes)

        guard case .modifySelected(let duration) = parse(":modify +90", event: selectedEvent) else {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(duration.start, selectedEvent.start)
        XCTAssertEqual(duration.end.timeIntervalSince(duration.start), 90 * 60)
    }

    func testModifyCanPatchDayTitleAndNotes() {
        guard case .modifySelected(let patch) = parse(
            ":modify tomorrow 9:00 Planning :: Updated notes", event: selectedEvent
        ) else {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(cal.component(.day, from: patch.start), 20)
        XCTAssertEqual(cal.component(.hour, from: patch.start), 9)
        XCTAssertEqual(patch.title, "Planning")
        XCTAssertEqual(patch.notes, "Updated notes")
    }

    func testModifyTitleOnlyAndEmptyNotes() {
        guard case .modifySelected(let title) = parse(":modify Planning", event: selectedEvent) else {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(title.start, selectedEvent.start)
        XCTAssertEqual(title.end, selectedEvent.end)
        XCTAssertEqual(title.title, "Planning")

        guard case .modifySelected(let notes) = parse(":modify ::", event: selectedEvent) else {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(notes.notes, "")
    }

    func testMovingAllDayEventPreservesCalendarDayAcrossDST() {
        var berlin = Calendar(identifier: .gregorian)
        berlin.timeZone = TimeZone(identifier: "Europe/Berlin")!
        let start = berlin.date(from: DateComponents(year: 2026, month: 3, day: 28))!
        let end = berlin.date(byAdding: .day, value: 1, to: start)!
        let event = CalendarEvent(uid: "all-day", calendar: "Work", subject: "Offsite",
                                  start: start, end: end, allDay: true, account: "a@b.c")
        guard case .modifySelected(let patch) = parse(":modify tomorrow", event: event,
                                                       calendar: berlin) else
        {
            XCTFail("expected modify"); return
        }
        XCTAssertEqual(berlin.component(.hour, from: patch.start), 0)
        XCTAssertEqual(berlin.component(.hour, from: patch.end), 0)
        XCTAssertEqual(berlin.dateComponents([.day], from: patch.start, to: patch.end).day, 1)
    }

    func testModifyArgumentsRequireASelection() {
        guard case .invalid = parse(":modify 9:00") else {
            XCTFail("expected invalid without a selected event"); return
        }
    }

    func testTitlelessAndBackwardsEventsAreRejected() {
        guard case .invalid = parse(":new 9:00") else {
            XCTFail("expected invalid for a missing title"); return
        }
        guard case .invalid = parse(":new 10:00 9:00 Standup") else {
            XCTFail("expected invalid for an end before the start"); return
        }
    }

    func testEmptyLineIsNoCommandAndGarbageIsInvalid() {
        XCTAssertEqual(parse(":"), .none)
        XCTAssertEqual(parse(""), .none)
        guard case .invalid = parse(":frobnicate") else {
            XCTFail("expected invalid"); return
        }
    }

    func testVerbsAndTheirShortForms() {
        XCTAssertEqual(parse(":today"), .goToday)
        XCTAssertEqual(parse(":t"), .goToday)
        XCTAssertEqual(parse(":week"), .setView(.week))
        XCTAssertEqual(parse(":m"), .setView(.month))
        XCTAssertEqual(parse(":year"), .setView(.year))
        XCTAssertEqual(parse(":agenda"), .setView(.agenda))
        XCTAssertEqual(parse(":modify"), .editSelected)
        XCTAssertEqual(parse(":mod"), .editSelected)
        XCTAssertEqual(parse(":edit"), .editSelected)
        XCTAssertEqual(parse(":delete"), .deleteSelected)
    }

    func testCommandsWithoutArgumentsRejectTrailingInput() {
        guard case .invalid = parse(":delete typo") else {
            XCTFail("delete must reject trailing input"); return
        }
        guard case .invalid = parse(":week tomorrow") else {
            XCTFail("week must reject trailing input"); return
        }
    }
}
