@testable import durian_lib
import XCTest

/// Tests the pure event-visibility filter behind CalendarManager's projection
/// (window overlap composed with the hidden-calendar set) and the sidebar
/// persistence round-trip against a scratch UserDefaults suite.
@MainActor
final class CalendarVisibilityTests: XCTestCase {

    // MARK: - Helpers

    /// A fixed base so all offsets in the tests are deterministic.
    private let base = Date(timeIntervalSince1970: 1_780_000_000)

    private func makeEvent(uid: String, start: TimeInterval, durationMinutes: Double = 60,
                           calendar: String = "Calendar",
                           account: String = "user@example.com") -> CalendarEvent {
        let s = base.addingTimeInterval(start)
        return CalendarEvent(
            uid: uid, calendar: calendar, subject: "Event",
            start: s, end: s.addingTimeInterval(durationMinutes * 60),
            account: account
        )
    }

    private func window(from: TimeInterval, to: TimeInterval) -> DateInterval {
        DateInterval(start: base.addingTimeInterval(from), end: base.addingTimeInterval(to))
    }

    /// The account-scoped hidden-set key for a calendar (default account matches
    /// makeEvent's default), since visibility is keyed per (account, calendar).
    private func key(_ name: String, account: String = "user@example.com") -> String {
        CalendarInfo.key(account: account, name: name)
    }

    // MARK: - Hidden-calendar filter

    func testHiddenCalendarEventsExcluded() {
        let events = [
            makeEvent(uid: "w1", start: 0, calendar: "Work"),
            makeEvent(uid: "h1", start: 3600, calendar: "Home"),
            makeEvent(uid: "w2", start: 7200, calendar: "Work"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400), hidden: [key("Work")])
        XCTAssertEqual(visible.map(\.uid), ["h1"])
    }

    func testEmptyHiddenSetKeepsAllWithinWindow() {
        let events = [
            makeEvent(uid: "a", start: 0, calendar: "Work"),
            makeEvent(uid: "b", start: 3600, calendar: "Home"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400), hidden: [])
        XCTAssertEqual(visible.map(\.uid), ["a", "b"])
    }

    func testHiddenNameMatchesExactCalendarOnly() {
        let events = [
            makeEvent(uid: "a", start: 0, calendar: "Work"),
            makeEvent(uid: "b", start: 3600, calendar: "Workshops"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400), hidden: [key("Work")])
        XCTAssertEqual(visible.map(\.uid), ["b"], "hiding is by exact name, not prefix")
    }

    func testHidingIsScopedPerAccount() {
        // Two accounts each own a "Work" calendar; hiding one account's must
        // leave the other's events visible.
        let events = [
            makeEvent(uid: "a1", start: 0, calendar: "Work", account: "a@example.com"),
            makeEvent(uid: "b1", start: 3600, calendar: "Work", account: "b@example.com"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400),
            hidden: [key("Work", account: "a@example.com")])
        XCTAssertEqual(visible.map(\.uid), ["b1"],
                       "hiding one account's calendar must not hide another account's same-named one")
    }

    // MARK: - Window filter still composes

    func testWindowFilterStillApplies() {
        let events = [
            makeEvent(uid: "inside", start: 3600, calendar: "Work"),
            makeEvent(uid: "outside", start: 10 * 24 * 3600, calendar: "Work"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400), hidden: [])
        XCTAssertEqual(visible.map(\.uid), ["inside"],
                       "the overlap window must keep filtering with no calendars hidden")
    }

    func testWindowAndHiddenCompose() {
        let events = [
            makeEvent(uid: "visibleInside", start: 0, calendar: "Home"),
            makeEvent(uid: "hiddenInside", start: 3600, calendar: "Work"),
            makeEvent(uid: "visibleOutside", start: 10 * 24 * 3600, calendar: "Home"),
            makeEvent(uid: "hiddenOutside", start: 11 * 24 * 3600, calendar: "Work"),
        ]
        let visible = CalendarManager.visibleEvents(
            events, window: window(from: 0, to: 86_400), hidden: [key("Work")])
        XCTAssertEqual(visible.map(\.uid), ["visibleInside"])
    }

    func testWindowOverlapNotStartContainment() {
        // Starts before the window but ends inside: still visible (the week
        // grid renders it clamped) — hiding must not change the overlap rule.
        let spanning = makeEvent(uid: "span", start: -3600, durationMinutes: 120, calendar: "Work")
        XCTAssertEqual(
            CalendarManager.visibleEvents([spanning], window: window(from: 0, to: 86_400),
                                          hidden: []).map(\.uid),
            ["span"])
        XCTAssertTrue(
            CalendarManager.visibleEvents([spanning], window: window(from: 0, to: 86_400),
                                          hidden: [key("Work")]).isEmpty)
    }

    func testNilWindowShowsAllExceptHidden() {
        // Search mode projects with no window bound; only hiding filters.
        let events = [
            makeEvent(uid: "old", start: -30 * 24 * 3600, calendar: "Home"),
            makeEvent(uid: "hidden", start: 0, calendar: "Work"),
        ]
        let visible = CalendarManager.visibleEvents(events, window: nil, hidden: [key("Work")])
        XCTAssertEqual(visible.map(\.uid), ["old"])
    }

    // MARK: - Toggling in and out

    func testToggleInAndOut() {
        let events = [
            makeEvent(uid: "w", start: 0, calendar: "Work"),
            makeEvent(uid: "h", start: 3600, calendar: "Home"),
        ]
        let w = window(from: 0, to: 86_400)

        var hidden: Set<String> = []
        XCTAssertEqual(CalendarManager.visibleEvents(events, window: w, hidden: hidden).count, 2)

        hidden.insert(key("Work"))
        XCTAssertEqual(CalendarManager.visibleEvents(events, window: w, hidden: hidden).map(\.uid),
                       ["h"])

        hidden.remove(key("Work"))
        XCTAssertEqual(CalendarManager.visibleEvents(events, window: w, hidden: hidden).count, 2,
                       "re-showing a calendar restores its events")
    }

    // MARK: - Persistence round-trip

    func testHiddenCalendarsPersistenceRoundTrip() throws {
        let suiteName = "org.js-lab.durian.tests.calendar-visibility"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suiteName))
        defaults.removePersistentDomain(forName: suiteName)
        defer { defaults.removePersistentDomain(forName: suiteName) }

        XCTAssertTrue(CalendarManager.loadHiddenCalendars(from: defaults).isEmpty,
                      "no stored value decodes as the empty set")

        let hidden: Set<String> = ["Work", "Birthdays"]
        CalendarManager.saveHiddenCalendars(hidden, to: defaults)
        XCTAssertEqual(CalendarManager.loadHiddenCalendars(from: defaults), hidden)

        CalendarManager.saveHiddenCalendars([], to: defaults)
        XCTAssertTrue(CalendarManager.loadHiddenCalendars(from: defaults).isEmpty,
                      "saving the empty set clears the stored names")
    }
}
