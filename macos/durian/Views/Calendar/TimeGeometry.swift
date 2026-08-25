//
//  TimeGeometry.swift
//  Durian
//
//  The single source of pixel<->time truth for the week grid. Block
//  placement, the live drag/resize previews and the committed drop all go
//  through one TimeGeometry instance, so what a drag shows and what it
//  commits can never disagree (the conversions used to be written out four
//  times in CalendarWeekView and could drift apart).
//
//  Pure minutes<->points math only: the view converts Date<->minutes with
//  Calendar; this type never touches dates.
//

import Foundation

struct TimeGeometry: Equatable {
    var hourHeight: CGFloat
    var dayWidth: CGFloat
    var timeColumnWidth: CGFloat
    var startHour: Int = 0
    var endHour: Int = 24
    var snapMinutes: Int = 15

    /// The full height of the time grid.
    var totalHeight: CGFloat {
        CGFloat(endHour - startHour) * hourHeight
    }

    // MARK: - Vertical (time axis)

    /// The y position of a minute-of-day within the grid.
    func y(forMinutes minutes: Int) -> CGFloat {
        CGFloat(minutes - startHour * 60) / 60 * hourHeight
    }

    /// The minute-of-day at a y position — the inverse of y(forMinutes:),
    /// rounded to the nearest minute so the pair round-trips exactly.
    func minutes(atY y: CGFloat) -> Int {
        startHour * 60 + Int((Double(y) / Double(hourHeight) * 60).rounded())
    }

    /// The rendered height of a duration. Also valid for signed deltas
    /// (a negative minute delta yields a negative offset).
    func height(forMinutes minutes: Int) -> CGFloat {
        CGFloat(minutes) / 60 * hourHeight
    }

    /// Rounds a minute value to the snap grid (nearest, halves away from
    /// zero — so a drag snaps symmetrically in both directions).
    func snap(minutes: Int) -> Int {
        Int((Double(minutes) / Double(snapMinutes)).rounded()) * snapMinutes
    }

    /// A vertical drag translation in points -> the snapped minute delta it
    /// means. THE conversion move, resize and their previews share.
    func snappedMinuteDelta(fromPoints dy: CGFloat) -> Int {
        let rawMinutes = Double(dy) * 60 / Double(hourHeight)
        return Int((rawMinutes / Double(snapMinutes)).rounded()) * snapMinutes
    }

    /// A y position as a snapped, in-grid wall-clock minute. The upper bound
    /// is inclusive so a drag can end exactly at midnight.
    func snappedMinute(atY y: CGFloat) -> Int {
        min(max(snap(minutes: minutes(atY: y)), startHour * 60), endHour * 60)
    }

    struct SelectionRange: Equatable {
        var startMinute: Int
        var endMinute: Int
    }

    /// The snapped time range selected by dragging between two vertical grid
    /// positions. Direction does not matter; a click-sized drag still selects
    /// one slot, and positions beyond the grid clamp to its first/last slot.
    func selectionRange(fromY startY: CGFloat, toY endY: CGFloat) -> SelectionRange {
        let upperBound = endHour * 60
        let start = snappedMinute(atY: startY)
        let end = snappedMinute(atY: endY)

        if start == end {
            if start == upperBound {
                return SelectionRange(startMinute: upperBound - snapMinutes, endMinute: upperBound)
            }
            return SelectionRange(startMinute: start, endMinute: min(start + snapMinutes, upperBound))
        }
        return SelectionRange(startMinute: min(start, end), endMinute: max(start, end))
    }

    // MARK: - Horizontal (day columns)

    /// The x position of a day column's leading edge (index 0 sits right of
    /// the time axis).
    func x(forDayIndex index: Int) -> CGFloat {
        timeColumnWidth + CGFloat(index) * dayWidth
    }

    /// The day column under an x position in grid coordinates. Unclamped:
    /// positions left of the time axis yield negative indices — callers
    /// bound the result to their day range.
    func dayIndex(atX x: CGFloat) -> Int {
        Int(Double((x - timeColumnWidth) / dayWidth).rounded(.down))
    }

    /// A horizontal drag translation -> the whole-day column delta it means
    /// (the move gesture snaps horizontally to whole days).
    func dayDelta(fromPoints dx: CGFloat) -> Int {
        Int((Double(dx) / Double(dayWidth)).rounded())
    }

    /// Maps a pointer x in one day column's local coordinate space to a day
    /// in the whole grid. DragGesture keeps reporting local positions after
    /// leaving its source view, so negative and >width values naturally walk
    /// into neighbouring columns.
    func dayIndex(atLocalX x: CGFloat, relativeTo originDayIndex: Int,
                  dayCount: Int = 7) -> Int
    {
        let offset = Int((Double(x) / Double(dayWidth)).rounded(.down))
        return min(max(originDayIndex + offset, 0), dayCount - 1)
    }

    // MARK: - Move targeting

    /// A snapped landing cell for a move drag: the day column and the
    /// minute-of-day the dragged block's top edge lands on.
    struct MoveTarget: Equatable {
        var dayIndex: Int
        var startMinute: Int
    }

    /// Absolute-position targeting: the
    /// proposed block origin is the pointer's grid-space position minus the
    /// grab offset recorded at drag start, and the landing cell is that
    /// origin's column and snapped minute. Depending only on where the block
    /// IS, the result is independent of where inside the block it was
    /// grabbed and of the original start's sub-slot offset — the old
    /// translation-based math could land one slot off on both counts.
    /// Shared by the move preview and the drop commit, so they cannot
    /// disagree. The start is clamped so the whole [start, start + duration]
    /// block stays inside the grid day.
    func moveTarget(location: CGPoint, grabOffset: CGSize, durationMinutes: Int,
                    dayCount: Int = 7) -> MoveTarget
    {
        let originX = location.x - grabOffset.width
        let originY = location.y - grabOffset.height
        let day = min(max(dayIndex(atX: originX), 0), dayCount - 1)
        let minStart = startHour * 60
        // An over-long duration (wider than the grid) pins to the top rather
        // than producing an inverted clamp range.
        let maxStart = max(endHour * 60 - durationMinutes, minStart)
        let start = min(max(snap(minutes: minutes(atY: originY)), minStart), maxStart)
        return MoveTarget(dayIndex: day, startMinute: start)
    }
}
