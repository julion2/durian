@testable import durian_lib
import XCTest

/// TimeGeometry.moveTarget: the absolute-position landing cell of a move
/// drag (pointer minus grab offset -> day column + snapped start minute).
final class WeekDragTargetingTests: XCTestCase {

    /// The week view's real dimensions: 44pt/hour, ~120pt columns after a
    /// 48pt time axis (matches TimeGeometryTests).
    private let geo = TimeGeometry(hourHeight: 44, dayWidth: 120, timeColumnWidth: 48)

    /// The grid-space origin of a full-width block at (day, minute).
    private func origin(day: Int, minute: Int) -> CGPoint {
        CGPoint(x: geo.x(forDayIndex: day), y: geo.y(forMinutes: minute))
    }

    // MARK: - Grab-offset invariance

    /// The core fix: the landing cell is a function of the block's proposed
    /// position, so the same block displacement lands on the same cell no
    /// matter where inside the block the drag started.
    func testLandingIndependentOfGrabPoint() {
        let o = origin(day: 2, minute: 9 * 60)
        // One column right, +30 minutes of block displacement.
        let displacement = CGSize(width: 120, height: 22)
        for grab in [CGSize(width: 5, height: 3), CGSize(width: 100, height: 40)] {
            let start = CGPoint(x: o.x + grab.width, y: o.y + grab.height)
            let location = CGPoint(x: start.x + displacement.width, y: start.y + displacement.height)
            let target = geo.moveTarget(location: location, grabOffset: grab, durationMinutes: 60)
            XCTAssertEqual(target, TimeGeometry.MoveTarget(dayIndex: 3, startMinute: 9 * 60 + 30),
                           "grab offset \(grab) must not change the landing cell")
        }
    }

    /// An unaligned start (9:10) snaps from the block's ABSOLUTE position:
    /// with no movement it clicks to the nearest slot of where it IS.
    func testSubSlotStartSnapsFromAbsolutePosition() {
        let o = origin(day: 1, minute: 9 * 60 + 10)
        let grab = CGSize(width: 30, height: 10)
        let target = geo.moveTarget(location: CGPoint(x: o.x + grab.width, y: o.y + grab.height),
                                    grabOffset: grab, durationMinutes: 60)
        XCTAssertEqual(target, TimeGeometry.MoveTarget(dayIndex: 1, startMinute: 9 * 60 + 15))
    }

    /// The one-slot-off bug the absolute targeting removes: an event at 9:10
    /// dragged down 22pt (30min of travel) visually sits at 9:40, whose
    /// nearest slot is 9:45. The old translation math added a snapped 30min
    /// delta to 9:10 and landed at 9:40 — off the grid the preview showed.
    func testDropMatchesVisualPositionNotTranslation() {
        let o = origin(day: 0, minute: 9 * 60 + 10)
        let grab = CGSize(width: 10, height: 5)
        let location = CGPoint(x: o.x + grab.width, y: o.y + grab.height + 22)
        let target = geo.moveTarget(location: location, grabOffset: grab, durationMinutes: 60)
        XCTAssertEqual(target.startMinute, 9 * 60 + 45)
    }

    // MARK: - Day columns

    func testDayIndexFollowsProposedOrigin() {
        let o = origin(day: 4, minute: 12 * 60)
        let grab = CGSize(width: 60, height: 10)
        // Displace the block two columns left.
        let location = CGPoint(x: o.x + grab.width - 240, y: o.y + grab.height)
        let target = geo.moveTarget(location: location, grabOffset: grab, durationMinutes: 30)
        XCTAssertEqual(target.dayIndex, 2)
        XCTAssertEqual(target.startMinute, 12 * 60)
    }

    func testDayIndexClampsToWeek() {
        // Left of the time axis clamps to the first column.
        XCTAssertEqual(geo.moveTarget(location: CGPoint(x: 0, y: 100), grabOffset: .zero,
                                      durationMinutes: 30).dayIndex, 0)
        // Beyond the last column clamps to the seventh.
        XCTAssertEqual(geo.moveTarget(location: CGPoint(x: 48 + 7 * 120 + 300, y: 100), grabOffset: .zero,
                                      durationMinutes: 30).dayIndex, 6)
    }

    // MARK: - Vertical clamping

    func testStartClampsSoBlockStaysInDay() {
        // Dragged past the bottom of the grid: a 2h event starts at 22:00 at
        // the latest so [start, start + duration] stays inside the day.
        let bottom = geo.moveTarget(location: CGPoint(x: 100, y: geo.totalHeight + 50),
                                    grabOffset: .zero, durationMinutes: 120)
        XCTAssertEqual(bottom.startMinute, 22 * 60)
        // Dragged above the top: clamps to midnight.
        let top = geo.moveTarget(location: CGPoint(x: 100, y: -50),
                                 grabOffset: .zero, durationMinutes: 120)
        XCTAssertEqual(top.startMinute, 0)
    }

    func testOverlongDurationPinsToTop() {
        let target = geo.moveTarget(location: CGPoint(x: 100, y: 500), grabOffset: .zero,
                                    durationMinutes: 2000)
        XCTAssertEqual(target.startMinute, 0,
                       "a duration wider than the grid clamps to the top, not to an inverted range")
    }

    func testWorkingHoursGridClampsToItsOwnBounds() {
        let working = TimeGeometry(hourHeight: 44, dayWidth: 120, timeColumnWidth: 48,
                                   startHour: 8, endHour: 20)
        let top = working.moveTarget(location: CGPoint(x: 100, y: -10), grabOffset: .zero,
                                     durationMinutes: 60)
        XCTAssertEqual(top.startMinute, 8 * 60)
        let bottom = working.moveTarget(location: CGPoint(x: 100, y: working.totalHeight + 10),
                                        grabOffset: .zero, durationMinutes: 60)
        XCTAssertEqual(bottom.startMinute, 19 * 60)
    }
}
