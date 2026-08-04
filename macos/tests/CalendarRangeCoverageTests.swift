@testable import durian_lib
import XCTest

final class CalendarRangeCoverageTests: XCTestCase {

    // MARK: - Helpers

    /// A fixed base so all offsets in the tests are deterministic.
    private let base = Date(timeIntervalSince1970: 1_780_000_000)

    private func interval(from: TimeInterval, to: TimeInterval) -> DateInterval {
        DateInterval(start: base.addingTimeInterval(from), end: base.addingTimeInterval(to))
    }

    private let day: TimeInterval = 86_400
    private let week: TimeInterval = 7 * 86_400

    // MARK: - augmented

    func testAugmentedDoublesWidthCentered() {
        let requested = interval(from: 0, to: week)
        let augmented = CalendarRangeCoverage.augmented(requested)
        // Half the width (3.5 days) on each side: total 2x, same center.
        XCTAssertEqual(augmented.start, base.addingTimeInterval(-week / 2))
        XCTAssertEqual(augmented.end, base.addingTimeInterval(week + week / 2))
        XCTAssertEqual(augmented.duration, 2 * requested.duration)
        XCTAssertEqual(
            augmented.start.timeIntervalSince1970 + augmented.end.timeIntervalSince1970,
            requested.start.timeIntervalSince1970 + requested.end.timeIntervalSince1970,
            accuracy: 0.001,
            "augmentation must keep the center (equal midpoints)"
        )
    }

    func testAugmentedOffsetsAreExact() {
        let requested = interval(from: 10 * day, to: 12 * day)
        let augmented = CalendarRangeCoverage.augmented(requested)
        // Width 2 days, so exactly 1 day is added on each side.
        XCTAssertEqual(augmented.start, base.addingTimeInterval(9 * day))
        XCTAssertEqual(augmented.end, base.addingTimeInterval(13 * day))
    }

    func testAugmentedZeroWidthIsIdentity() {
        let degenerate = interval(from: 3600, to: 3600)
        let augmented = CalendarRangeCoverage.augmented(degenerate)
        XCTAssertEqual(augmented, degenerate, "a zero-width window has nothing to widen by")
    }

    // MARK: - covers

    func testCoversRequestedFullyInside() {
        let loaded = interval(from: 0, to: 4 * week)
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: week, to: 2 * week)))
    }

    func testCoversFailsOnPartialOverlap() {
        let loaded = interval(from: 0, to: 2 * week)
        // Sticks out past the loaded end.
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: week, to: 3 * week)))
        // Sticks out before the loaded start.
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: -week, to: week)))
        // Fully disjoint.
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: 3 * week, to: 4 * week)))
    }

    func testCoversFailsOnNilLoaded() {
        XCTAssertFalse(CalendarRangeCoverage.covers(nil, interval(from: 0, to: week)))
    }

    func testCoversExactEqualRanges() {
        let range = interval(from: 0, to: week)
        XCTAssertTrue(CalendarRangeCoverage.covers(range, range))
    }

    func testCoversRequestedTouchingLoadedEdges() {
        // Both windows are half-open [start, end): the fetch for `loaded`
        // returned every event starting strictly before loaded.end, so a
        // requested window ending exactly at loaded.end (or starting exactly
        // at loaded.start) is still fully covered.
        let loaded = interval(from: 0, to: 4 * week)
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: 3 * week, to: 4 * week)))
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: 0, to: week)))
        // One second beyond either edge breaks coverage.
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: 3 * week, to: 4 * week + 1)))
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: -1, to: week)))
    }

    // MARK: - accumulate

    func testAccumulateNilCoverageTakesFetched() {
        let fetched = interval(from: 0, to: week)
        XCTAssertEqual(CalendarRangeCoverage.accumulate(nil, adding: fetched), fetched)
    }

    func testAccumulateOverlappingWidens() {
        let loaded = interval(from: 0, to: 2 * week)
        let fetched = interval(from: week, to: 3 * week)
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched),
                       interval(from: 0, to: 3 * week))
        // Widening backwards works the same.
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: interval(from: -week, to: week)),
                       interval(from: -week, to: 2 * week))
    }

    func testAccumulateTouchingCountsAsConnected() {
        // Both windows are half-open: [a,b) + [b,c) covers [a,c) gap-free.
        let loaded = interval(from: 0, to: week)
        let fetched = interval(from: week, to: 2 * week)
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched),
                       interval(from: 0, to: 2 * week))
    }

    func testAccumulateContainedFetchKeepsCoverage() {
        let loaded = interval(from: 0, to: 4 * week)
        let fetched = interval(from: week, to: 2 * week)
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched), loaded)
    }

    func testAccumulateDisjointResets() {
        let loaded = interval(from: 0, to: week)
        let fetched = interval(from: 3 * week, to: 4 * week)
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched), fetched,
                       "a disjoint fetch must not claim the never-fetched gap")
    }

    func testAccumulateCapResetsOversizedUnion() {
        let loaded = interval(from: 0, to: 3 * week)
        let fetched = interval(from: 2 * week, to: 5 * week)
        // The union would span 5 weeks; capped at 4 it resets to the new
        // window, bounding how far the band can grow.
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched, maxSpan: 4 * week),
                       fetched)
        // At exactly the cap the union survives.
        XCTAssertEqual(CalendarRangeCoverage.accumulate(loaded, adding: fetched, maxSpan: 5 * week),
                       interval(from: 0, to: 5 * week))
    }

    /// The Phase 2 gap this closes: stepping the padded 9-day week window
    /// forward escapes its own augmented band (that fetch stays), but the
    /// band UNIONS instead of moving — so stepping BACK over any previously
    /// visited week skips the network entirely.
    func testAccumulatedBandCoversRevisitedWeeks() {
        let week0 = interval(from: -day, to: week + day)
        var band = CalendarRangeCoverage.accumulate(nil, adding: CalendarRangeCoverage.augmented(week0))
        let week1 = interval(from: week - day, to: 2 * week + day)
        XCTAssertFalse(CalendarRangeCoverage.covers(band, week1),
                       "a full week step still escapes the half-width margin")
        band = CalendarRangeCoverage.accumulate(band, adding: CalendarRangeCoverage.augmented(week1))
        // Both the new week and the one stepped away from are now covered.
        XCTAssertTrue(CalendarRangeCoverage.covers(band, week1))
        XCTAssertTrue(CalendarRangeCoverage.covers(band, week0))
    }

    // MARK: - Coverage sequence (the navigation scenario)

    /// The skip/fetch decision sequence: load an augmented band around a
    /// two-week window W, then a one-week step stays covered (no fetch) while
    /// a two-week step escapes the band (fetch). The band's absorption is its
    /// half-width margin: shifts up to width/2 skip, larger ones refetch.
    func testSteppingSequenceSkipsInsideBandFetchesOutside() {
        let windowW = interval(from: 0, to: 2 * week)
        let loaded = CalendarRangeCoverage.augmented(windowW)
        XCTAssertEqual(loaded, interval(from: -week, to: 3 * week))

        // Adjacent week: shifted by exactly the half-width margin — still
        // fully inside the band (edge-touching counts, see covers) — skip.
        let oneWeekOut = interval(from: week, to: 3 * week)
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, oneWeekOut))
        // Backwards too.
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: -week, to: week)))

        // Two weeks out: escapes the band — fetch.
        let twoWeeksOut = interval(from: 2 * week, to: 4 * week)
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, twoWeeksOut))

        // After refetching for the escaped window, its own augmented band
        // covers it, still covers a step back toward W (the bands overlap),
        // but not a jump two more weeks out.
        let reloaded = CalendarRangeCoverage.augmented(twoWeeksOut)
        XCTAssertTrue(CalendarRangeCoverage.covers(reloaded, twoWeeksOut))
        XCTAssertTrue(CalendarRangeCoverage.covers(reloaded, oneWeekOut))
        XCTAssertFalse(CalendarRangeCoverage.covers(reloaded, interval(from: 4 * week, to: 6 * week)))
    }

    /// The manager's WEEK window is 9 days (padded week), so its augmented
    /// band reaches only 4.5 days past each edge — a full 7-day step escapes
    /// it and refetches. Documented here so the margin math stays honest:
    /// factor-2 absorbs shifts up to HALF the window width, not a whole one.
    func testPaddedWeekWindowStepEscapesItsOwnBand() {
        let weekW = interval(from: -day, to: week + day)
        let loaded = CalendarRangeCoverage.augmented(weekW)
        XCTAssertEqual(loaded, interval(from: -5.5 * day, to: 12.5 * day))
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: week - day, to: 2 * week + day)))
        // A sub-half-width slide stays covered.
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: 3 * day - day, to: 3 * day + week + day)))
    }

    /// The 30-day agenda window's band absorbs one- and two-week steps (its
    /// half-width margin is 15 days) but not a 16-day shift.
    func testAgendaWindowBandAbsorbsWeekSteps() {
        let agenda = interval(from: 0, to: 30 * day)
        let loaded = CalendarRangeCoverage.augmented(agenda)
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: week, to: 30 * day + week)))
        XCTAssertTrue(CalendarRangeCoverage.covers(loaded, interval(from: -2 * week, to: 30 * day - 2 * week)))
        XCTAssertFalse(CalendarRangeCoverage.covers(loaded, interval(from: 16 * day, to: 46 * day)))
    }
}
