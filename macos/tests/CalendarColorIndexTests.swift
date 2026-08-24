@testable import durian_lib
import SwiftUI
import XCTest

/// Tests CalendarManager's calendar-color index: colors must resolve per
/// (account, name) — two accounts commonly both own a calendar called
/// "Calendar", and a name-only match colors one account's events with the
/// other account's calendar color.
@MainActor
final class CalendarColorIndexTests: XCTestCase {

    private let base = Date(timeIntervalSince1970: 1_780_000_000)

    private func makeEvent(calendar: String, account: String) -> CalendarEvent {
        CalendarEvent(uid: "e1", calendar: calendar, subject: "Event",
                      start: base, end: base.addingTimeInterval(3600),
                      account: account)
    }

    override func tearDown() {
        // The manager is a singleton; leave no calendar list behind for other
        // tests in this process.
        CalendarManager.shared.calendars = []
        super.tearDown()
    }

    func testSameNameResolvesPerAccount() {
        CalendarManager.shared.calendars = [
            CalendarInfo(name: "Calendar", colorHex: "ff0000", account: "a@example.com"),
            CalendarInfo(name: "Calendar", colorHex: "00ff00", account: "b@example.com"),
        ]
        XCTAssertEqual(
            CalendarManager.shared.color(for: makeEvent(calendar: "Calendar", account: "b@example.com")),
            Color(hex: "00ff00")
        )
        XCTAssertEqual(
            CalendarManager.shared.color(for: makeEvent(calendar: "Calendar", account: "a@example.com")),
            Color(hex: "ff0000")
        )
    }

    func testNameFallbackWhenAccountHasNoExactMatch() {
        CalendarManager.shared.calendars = [
            CalendarInfo(name: "Work", colorHex: "0000ff", account: "a@example.com")
        ]
        // The event's account has no calendar entry (list still loading):
        // fall back to the name match instead of dropping to gray.
        XCTAssertEqual(
            CalendarManager.shared.color(for: makeEvent(calendar: "Work", account: "b@example.com")),
            Color(hex: "0000ff")
        )
    }

    func testUnknownCalendarFallsBackToSecondary() {
        CalendarManager.shared.calendars = [
            CalendarInfo(name: "Work", colorHex: "0000ff", account: "a@example.com")
        ]
        XCTAssertEqual(
            CalendarManager.shared.color(for: makeEvent(calendar: "Nope", account: "a@example.com")),
            Color.secondary
        )
    }
}
