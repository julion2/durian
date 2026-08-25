@testable import durian_lib
import XCTest

final class TimeGeometryTests: XCTestCase {

    /// The week view's real dimensions: 44pt/hour, ~120pt columns after a
    /// 48pt time axis.
    private let geo = TimeGeometry(hourHeight: 44, dayWidth: 120, timeColumnWidth: 48)

    // MARK: - y <-> minutes

    func testYForMinutes() {
        XCTAssertEqual(geo.y(forMinutes: 0), 0)
        XCTAssertEqual(geo.y(forMinutes: 60), 44)
        XCTAssertEqual(geo.y(forMinutes: 90), 66)
        XCTAssertEqual(geo.y(forMinutes: 24 * 60), geo.totalHeight)
    }

    func testMinutesAtYRoundTrips() {
        for minutes in [0, 1, 15, 59, 60, 437, 719, 1439, 1440] {
            XCTAssertEqual(geo.minutes(atY: geo.y(forMinutes: minutes)), minutes,
                           "y<->minutes must round-trip at \(minutes)")
        }
    }

    func testStartHourShiftsOrigin() {
        let working = TimeGeometry(hourHeight: 44, dayWidth: 120, timeColumnWidth: 48,
                                   startHour: 8, endHour: 20)
        XCTAssertEqual(working.y(forMinutes: 8 * 60), 0, "the grid's first hour sits at y = 0")
        XCTAssertEqual(working.minutes(atY: 0), 8 * 60)
        XCTAssertEqual(working.minutes(atY: working.y(forMinutes: 10 * 60 + 30)), 10 * 60 + 30)
    }

    func testTotalHeight() {
        XCTAssertEqual(geo.totalHeight, 24 * 44)
        let working = TimeGeometry(hourHeight: 44, dayWidth: 120, timeColumnWidth: 48,
                                   startHour: 8, endHour: 20)
        XCTAssertEqual(working.totalHeight, 12 * 44)
    }

    func testHeightForMinutes() {
        XCTAssertEqual(geo.height(forMinutes: 30), 22)
        XCTAssertEqual(geo.height(forMinutes: 90), 66)
        XCTAssertEqual(geo.height(forMinutes: -30), -22, "signed deltas convert symmetrically")
    }

    // MARK: - Snapping

    func testSnapRoundsToGrid() {
        XCTAssertEqual(geo.snap(minutes: 0), 0)
        XCTAssertEqual(geo.snap(minutes: 7), 0)
        XCTAssertEqual(geo.snap(minutes: 8), 15, "7.5 is the boundary; 8 rounds up")
        XCTAssertEqual(geo.snap(minutes: 15), 15)
        XCTAssertEqual(geo.snap(minutes: 22), 15)
        XCTAssertEqual(geo.snap(minutes: 23), 30)
    }

    func testSnapNegativesAreSymmetric() {
        XCTAssertEqual(geo.snap(minutes: -7), 0)
        XCTAssertEqual(geo.snap(minutes: -8), -15, "halves round away from zero in both directions")
        XCTAssertEqual(geo.snap(minutes: -22), -15)
        XCTAssertEqual(geo.snap(minutes: -23), -30)
    }

    func testSnappedMinuteDeltaFromPoints() {
        // 44pt = one hour; 11pt = 15 minutes exactly.
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: 0), 0)
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: 44), 60)
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: 11), 15)
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: 5), 0, "5pt is ~6.8min -> snaps to 0")
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: 6), 15, "6pt is ~8.2min -> snaps to 15")
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: -44), -60)
        XCTAssertEqual(geo.snappedMinuteDelta(fromPoints: -11), -15)
    }

    func testSelectionRangeSnapsInEitherDirection() {
        XCTAssertEqual(
            geo.selectionRange(fromY: geo.y(forMinutes: 9 * 60 + 8),
                               toY: geo.y(forMinutes: 10 * 60 + 22)),
            TimeGeometry.SelectionRange(startMinute: 9 * 60 + 15, endMinute: 10 * 60 + 15)
        )
        XCTAssertEqual(
            geo.selectionRange(fromY: geo.y(forMinutes: 10 * 60 + 22),
                               toY: geo.y(forMinutes: 9 * 60 + 8)),
            TimeGeometry.SelectionRange(startMinute: 9 * 60 + 15, endMinute: 10 * 60 + 15)
        )
    }

    func testSelectionRangeUsesOneSlotForClickSizedDrag() {
        XCTAssertEqual(
            geo.selectionRange(fromY: geo.y(forMinutes: 9 * 60),
                               toY: geo.y(forMinutes: 9 * 60 + 2)),
            TimeGeometry.SelectionRange(startMinute: 9 * 60, endMinute: 9 * 60 + 15)
        )
    }

    func testSelectionRangeClampsToGridEdges() {
        XCTAssertEqual(
            geo.selectionRange(fromY: -100, toY: geo.y(forMinutes: 30)),
            TimeGeometry.SelectionRange(startMinute: 0, endMinute: 30)
        )
        XCTAssertEqual(
            geo.selectionRange(fromY: geo.totalHeight + 100, toY: geo.totalHeight + 200),
            TimeGeometry.SelectionRange(startMinute: 23 * 60 + 45, endMinute: 24 * 60)
        )
    }

    // MARK: - Day columns

    func testXForDayIndex() {
        XCTAssertEqual(geo.x(forDayIndex: 0), 48, "day 0 starts right of the time axis")
        XCTAssertEqual(geo.x(forDayIndex: 3), 48 + 3 * 120)
    }

    func testDayIndexAtX() {
        XCTAssertEqual(geo.dayIndex(atX: 48), 0)
        XCTAssertEqual(geo.dayIndex(atX: 167.9), 0)
        XCTAssertEqual(geo.dayIndex(atX: 168), 1)
        XCTAssertEqual(geo.dayIndex(atX: 48 + 6 * 120 + 1), 6)
        XCTAssertEqual(geo.dayIndex(atX: 0), -1, "left of the time axis is out of range, not clamped")
    }

    func testDayIndexXRoundTrips() {
        for index in 0 ..< 7 {
            XCTAssertEqual(geo.dayIndex(atX: geo.x(forDayIndex: index)), index)
        }
    }

    func testDayDeltaFromPoints() {
        XCTAssertEqual(geo.dayDelta(fromPoints: 0), 0)
        XCTAssertEqual(geo.dayDelta(fromPoints: 59), 0)
        XCTAssertEqual(geo.dayDelta(fromPoints: 60), 1, "half a column snaps to the next day")
        XCTAssertEqual(geo.dayDelta(fromPoints: 130), 1)
        XCTAssertEqual(geo.dayDelta(fromPoints: 190), 2)
        XCTAssertEqual(geo.dayDelta(fromPoints: -60), -1)
        XCTAssertEqual(geo.dayDelta(fromPoints: -59), 0)
    }
}
