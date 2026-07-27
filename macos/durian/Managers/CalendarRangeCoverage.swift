//
//  CalendarRangeCoverage.swift
//  Durian
//
//  Pure range math for the calendar's prefetch/skip decision: fetches cover
//  an AUGMENTED window (twice the requested width, centered), and while a
//  newly requested window stays inside the last augmented fetch the manager
//  skips the network entirely — a hysteresis band: the requested range is
//  widened by factor 2.0 and the backend is only re-queried when the union
//  escapes the previously augmented range.
//
//  No UI, no state — free static functions so they unit-test in isolation.
//

import Foundation

enum CalendarRangeCoverage {

    /// Widens `requested` by half its duration on each side:
    ///
    ///     d         = requested.duration
    ///     augmented = [requested.start - d/2, requested.end + d/2)
    ///
    /// so the result is exactly twice as wide (d/2 + d + d/2 = 2d) and
    /// centered on the requested window — the factor-2.0 augmentation. A
    /// degenerate zero-width interval augments to itself.
    static func augmented(_ requested: DateInterval) -> DateInterval {
        let half = requested.duration / 2
        return DateInterval(
            start: requested.start.addingTimeInterval(-half),
            end: requested.end.addingTimeInterval(half)
        )
    }

    /// True iff `requested` lies fully inside `loaded`:
    ///
    ///     loaded.start <= requested.start && requested.end <= loaded.end
    ///
    /// A nil `loaded` (nothing fetched yet, or search mode) never covers.
    /// Edges: both windows are half-open [start, end) — the fetch returned
    /// every event starting strictly before `loaded.end` — so a requested
    /// window whose end EQUALS the loaded end is still fully covered, as is
    /// one starting exactly at the loaded start. Only strictly exceeding an
    /// edge breaks coverage.
    static func covers(_ loaded: DateInterval?, _ requested: DateInterval) -> Bool {
        guard let loaded else { return false }
        return loaded.start <= requested.start && requested.end <= loaded.end
    }

    /// Merges a completed fetch's augmented window into the existing
    /// coverage. Overlapping (or touching — both windows are half-open, so
    /// [a,b) + [b,c) covers [a,c) gap-free) windows UNION: the band widens
    /// as navigation steps through adjacent periods, so revisiting any of
    /// them skips the network. A disjoint fetch resets coverage to just the
    /// new window (the union would also claim the never-fetched gap between
    /// the two), and a union that would exceed `maxSpan` resets too,
    /// bounding how far the store's band — and the events it retains — can
    /// grow.
    static func accumulate(_ loaded: DateInterval?, adding fetched: DateInterval,
                           maxSpan: TimeInterval = 180 * 86_400) -> DateInterval {
        guard let loaded else { return fetched }
        guard loaded.start <= fetched.end && fetched.start <= loaded.end else { return fetched }
        let union = DateInterval(start: min(loaded.start, fetched.start),
                                 end: max(loaded.end, fetched.end))
        return union.duration <= maxSpan ? union : fetched
    }
}
