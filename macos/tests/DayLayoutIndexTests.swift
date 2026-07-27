@testable import durian_lib
import XCTest

final class DayLayoutIndexTests: XCTestCase {

    // MARK: - Helpers

    /// A fixed UTC calendar so wall-clock minute math is deterministic
    /// regardless of the machine's zone.
    private static let utc: Foundation.Calendar = {
        var cal = Foundation.Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "UTC")!
        return cal
    }()

    /// A UTC midnight (1_780_012_800 = 20602 * 86400).
    private let dayStart = Date(timeIntervalSince1970: 1_780_012_800)
    private var dayEnd: Date { dayStart.addingTimeInterval(86_400) }

    /// An event positioned in minutes relative to dayStart (negative =
    /// starts the previous day).
    private func event(_ uid: String, startMinute: Double, durationMinutes: Double,
                       allDay: Bool = false) -> CalendarEvent {
        let s = dayStart.addingTimeInterval(startMinute * 60)
        return CalendarEvent(uid: uid, calendar: "Cal", subject: uid,
                             start: s, end: s.addingTimeInterval(durationMinutes * 60),
                             allDay: allDay, account: "a@example.com")
    }

    private func layout(_ events: [CalendarEvent], maxVisibleLanes: Int = 4) -> DayLayoutIndex {
        DayLayoutIndex(events: events, dayStart: dayStart, dayEnd: dayEnd,
                       maxVisibleLanes: maxVisibleLanes, calendar: Self.utc)
    }

    // MARK: - Lanes

    func testSingleEventTakesLaneZero() {
        let l = layout([event("a", startMinute: 540, durationMinutes: 60)])
        XCTAssertEqual(l.visible.count, 1)
        XCTAssertTrue(l.overflow.isEmpty)
        let block = l.visible[0]
        XCTAssertEqual(block.lane, 0)
        XCTAssertEqual(block.laneCount, 1)
        XCTAssertEqual(block.startMinute, 540)
        XCTAssertEqual(block.endMinute, 600)
        XCTAssertFalse(block.continuesBefore)
        XCTAssertFalse(block.continuesAfter)
    }

    func testTwoOverlappingEventsUseTwoLanes() {
        let l = layout([
            event("a", startMinute: 540, durationMinutes: 60),
            event("b", startMinute: 570, durationMinutes: 60),
        ])
        XCTAssertEqual(l.visible.count, 2)
        XCTAssertEqual(Set(l.visible.map(\.lane)), [0, 1])
        XCTAssertTrue(l.visible.allSatisfy { $0.laneCount == 2 })
    }

    func testSequentialEventsReuseLaneZero() {
        let l = layout([
            event("a", startMinute: 540, durationMinutes: 60),
            event("b", startMinute: 600, durationMinutes: 60),
        ])
        XCTAssertEqual(l.visible.count, 2)
        XCTAssertTrue(l.visible.allSatisfy { $0.lane == 0 && $0.laneCount == 1 },
                      "non-overlapping events form separate clusters, both full-width")
    }

    func testEqualStartsLongerEventTakesLeftmostLane() {
        let l = layout([
            event("short", startMinute: 540, durationMinutes: 30),
            event("long", startMinute: 540, durationMinutes: 120),
        ])
        XCTAssertEqual(l.visible.first { $0.event.uid == "long" }?.lane, 0)
        XCTAssertEqual(l.visible.first { $0.event.uid == "short" }?.lane, 1)
    }

    // MARK: - Overflow

    func testOverflowCollapsesSurplusLanesIntoPlusX() {
        let concurrent = ["a", "b", "c", "d"].map { event($0, startMinute: 540, durationMinutes: 120) }
        let l = layout(concurrent, maxVisibleLanes: 2)
        XCTAssertEqual(l.visible.count, 1, "only lane 0 stays visible; the last lane becomes +X")
        XCTAssertEqual(l.visible[0].laneCount, 2)
        XCTAssertEqual(l.overflow.count, 1)
        let over = l.overflow[0]
        XCTAssertEqual(over.events.count, 3)
        XCTAssertEqual(over.lane, 1)
        XCTAssertEqual(over.laneCount, 2)
        XCTAssertEqual(over.startMinute, 540)
        XCTAssertEqual(over.endMinute, 660)
    }

    func testNoOverflowWhenLanesFit() {
        let concurrent = ["a", "b", "c"].map { event($0, startMinute: 540, durationMinutes: 60) }
        let l = layout(concurrent, maxVisibleLanes: 3)
        XCTAssertEqual(l.visible.count, 3)
        XCTAssertTrue(l.overflow.isEmpty)
    }

    // MARK: - Minimum visual height

    func testZeroDurationEventKeepsMinimumExtent() {
        let l = layout([event("a", startMinute: 540, durationMinutes: 0)])
        XCTAssertEqual(l.visible[0].startMinute, 540)
        XCTAssertEqual(l.visible[0].endMinute, 555, "a block is at least 15 minutes tall")
    }

    // MARK: - Multi-day clamping

    func testInteriorDayClampsToFullDay() {
        // Starts the previous day, ends the next day: this day shows the
        // full 24h slice, cut at both edges.
        let l = layout([event("span", startMinute: -600, durationMinutes: 3 * 1440)])
        XCTAssertEqual(l.visible.count, 1)
        let block = l.visible[0]
        XCTAssertEqual(block.startMinute, 0)
        XCTAssertEqual(block.endMinute, 1440)
        XCTAssertTrue(block.continuesBefore)
        XCTAssertTrue(block.continuesAfter)
    }

    func testFirstDayClampsAtMidnight() {
        // 22:00 for 4 hours: today shows [22:00, 24:00), continuing after.
        let l = layout([event("a", startMinute: 1320, durationMinutes: 240)])
        let block = l.visible[0]
        XCTAssertEqual(block.startMinute, 1320)
        XCTAssertEqual(block.endMinute, 1440)
        XCTAssertFalse(block.continuesBefore)
        XCTAssertTrue(block.continuesAfter)
    }

    func testLastDayClampsAtStart() {
        // Started yesterday 22:00, ends today 02:00: today shows [0, 02:00).
        let l = layout([event("a", startMinute: -120, durationMinutes: 240)])
        XCTAssertEqual(l.visible.count, 1, "an event starting the previous day is still a member")
        let block = l.visible[0]
        XCTAssertEqual(block.startMinute, 0)
        XCTAssertEqual(block.endMinute, 120)
        XCTAssertTrue(block.continuesBefore)
        XCTAssertFalse(block.continuesAfter)
    }

    func testEventEndingExactlyAtMidnightFillsToBottom() {
        let l = layout([event("a", startMinute: 1380, durationMinutes: 60)])
        let block = l.visible[0]
        XCTAssertEqual(block.endMinute, 1440, "an end at exactly dayEnd is minute 1440, not wall-clock 0")
        XCTAssertFalse(block.continuesAfter)
    }

    func testClampedSpillOverlapsWithTodaysEvents() {
        // Yesterday's spill [0, 120) overlaps a 01:00 event: two lanes.
        let l = layout([
            event("spill", startMinute: -120, durationMinutes: 240),
            event("early", startMinute: 60, durationMinutes: 60),
        ])
        XCTAssertEqual(l.visible.count, 2)
        XCTAssertEqual(Set(l.visible.map(\.lane)), [0, 1])
    }

    // MARK: - Membership

    func testMembershipExcludesNeighbourDays() {
        let l = layout([
            event("yesterday", startMinute: -120, durationMinutes: 60),
            event("ends-at-day-start", startMinute: -60, durationMinutes: 60),
            event("starts-at-day-end", startMinute: 1440, durationMinutes: 60),
            event("today", startMinute: 540, durationMinutes: 60),
        ])
        XCTAssertEqual(l.visible.map(\.event.uid), ["today"],
                       "half-open [dayStart, dayEnd): boundary-touching neighbours are excluded")
    }

    func testZeroDurationAtDayStartIsMember() {
        let l = layout([event("a", startMinute: 0, durationMinutes: 0)])
        XCTAssertEqual(l.visible.count, 1)
    }

    func testAllDayEventsAreIgnored() {
        let l = layout([
            event("all-day", startMinute: 0, durationMinutes: 1440, allDay: true),
            event("timed", startMinute: 540, durationMinutes: 60),
        ])
        XCTAssertEqual(l.visible.map(\.event.uid), ["timed"],
                       "all-day events belong to the header row, not the time grid")
    }
}
