//
//  DayLayoutIndex.swift
//  Durian
//
//  Overlap-aware lane layout for one day column of the week grid. Membership
//  is overlap with [dayStart, dayEnd) — not "starts on the day" — and each
//  block's minutes are clamped to the day, so a multi-day or midnight-
//  spanning event draws a clamped block on every day it touches. The greedy
//  first-free-lane clustering and the "+X" overflow collapsing moved here
//  out of CalendarWeekView's dayColumn so the layout is a value computed
//  once per change (see WeekLayoutCache) instead of per column per body
//  evaluation.
//

import Foundation

struct DayLayoutIndex {

    /// Placement for one visible timed event: its minutes clamped to the
    /// day, its lane, and the lane count of its overlap cluster (which
    /// determines the lane width). The continues flags mark a block cut off
    /// by the day edge, so the view can square that corner instead of
    /// drawing a rounded "end" that isn't one.
    struct Block: Identifiable {
        let event: CalendarEvent
        let startMinute: Int
        let endMinute: Int
        let lane: Int
        let laneCount: Int
        let continuesBefore: Bool
        let continuesAfter: Bool
        var id: EventID { event.id }
        var minutes: Int { endMinute - startMinute }
    }

    /// A "+X" block collapsing the events of the overflow lanes of one
    /// cluster into the last visible lane over their combined time span.
    struct OverflowBlock: Identifiable {
        struct ID: Hashable {
            let startMinute: Int
            let first: EventID?
        }

        let events: [CalendarEvent] // hidden events, sorted by start
        let startMinute: Int
        let endMinute: Int
        let lane: Int
        let laneCount: Int
        var id: ID { ID(startMinute: startMinute, first: events.first?.id) }
    }

    private(set) var visible: [Block] = []
    private(set) var overflow: [OverflowBlock] = []

    init() {}

    // MARK: - Membership

    /// Half-open overlap of [event.start, event.end) with [dayStart,
    /// dayEnd). A zero-duration event counts on the day it starts.
    static func overlaps(_ event: CalendarEvent, dayStart: Date, dayEnd: Date) -> Bool {
        event.start < dayEnd && (event.end > dayStart || event.start >= dayStart)
    }

    // MARK: - Layout

    /// Lays out the timed events overlapping [dayStart, dayEnd). All-day
    /// events are ignored (they live in the header row). `calendar` supplies
    /// the wall-clock minute mapping (injectable for deterministic tests).
    init(events: [CalendarEvent], dayStart: Date, dayEnd: Date,
         maxVisibleLanes: Int, calendar: Foundation.Calendar = .current)
    {
        let entries = events
            .filter { !$0.allDay && Self.overlaps($0, dayStart: dayStart, dayEnd: dayEnd) }
            .map { event -> Entry in
                let continuesBefore = event.start < dayStart
                let continuesAfter = event.end > dayEnd
                let start = continuesBefore ? 0 : Self.wallMinutes(event.start, calendar: calendar)
                // `>=`: an event ending exactly at midnight belongs to this
                // day in full — its end's wall clock would read 0.
                let endRaw = event.end >= dayEnd ? 24 * 60 : Self.wallMinutes(event.end, calendar: calendar)
                // Rendered extent keeps the 15-minute visual minimum, so
                // zero-duration events still claim their block's space.
                return Entry(event: event,
                             startMinute: start,
                             endMinute: max(endRaw, start + 15),
                             continuesBefore: continuesBefore,
                             continuesAfter: continuesAfter)
            }
        guard !entries.isEmpty else { return }
        // Longer events first on equal starts so they take the leftmost
        // lane; id as the final key keeps the layout stable across refreshes.
        let sorted = entries.sorted { a, b in
            if a.startMinute != b.startMinute { return a.startMinute < b.startMinute }
            if a.endMinute != b.endMinute { return a.endMinute > b.endMinute }
            return a.event.id < b.event.id
        }
        // Group into overlap clusters (an event joins the current cluster
        // while it starts before the cluster's running maximum end) and lay
        // each cluster out into lanes.
        var cluster: [Entry] = []
        var clusterMaxEnd = Int.min
        for entry in sorted {
            if !cluster.isEmpty && entry.startMinute >= clusterMaxEnd {
                layoutCluster(cluster, maxVisibleLanes: maxVisibleLanes)
                cluster.removeAll()
            }
            cluster.append(entry)
            clusterMaxEnd = cluster.count == 1 ? entry.endMinute : max(clusterMaxEnd, entry.endMinute)
        }
        layoutCluster(cluster, maxVisibleLanes: maxVisibleLanes)
    }

    private struct Entry {
        let event: CalendarEvent
        let startMinute: Int
        let endMinute: Int
        let continuesBefore: Bool
        let continuesAfter: Bool

        func block(lane: Int, laneCount: Int) -> Block {
            Block(event: event, startMinute: startMinute, endMinute: endMinute,
                  lane: lane, laneCount: laneCount,
                  continuesBefore: continuesBefore, continuesAfter: continuesAfter)
        }
    }

    /// Assigns each cluster event the lowest lane that is free at its start
    /// (greedy sweep). When the cluster needs more lanes than fit, the
    /// events of the surplus lanes collapse into one OverflowBlock in the
    /// last visible lane.
    private mutating func layoutCluster(_ cluster: [Entry], maxVisibleLanes: Int) {
        guard !cluster.isEmpty else { return }
        var laneEnds: [Int] = []
        var lanes: [Int] = []
        for entry in cluster {
            if let lane = laneEnds.indices.first(where: { laneEnds[$0] <= entry.startMinute }) {
                laneEnds[lane] = entry.endMinute
                lanes.append(lane)
            } else {
                laneEnds.append(entry.endMinute)
                lanes.append(laneEnds.count - 1)
            }
        }
        let laneCount = laneEnds.count
        if laneCount <= maxVisibleLanes {
            for (i, entry) in cluster.enumerated() {
                visible.append(entry.block(lane: lanes[i], laneCount: laneCount))
            }
            return
        }
        // More concurrent events than lanes: the last visible lane becomes
        // the "+X" block for everything at or beyond it.
        let overflowLane = maxVisibleLanes - 1
        var hidden: [Entry] = []
        for (i, entry) in cluster.enumerated() {
            if lanes[i] < overflowLane {
                visible.append(entry.block(lane: lanes[i], laneCount: maxVisibleLanes))
            } else {
                hidden.append(entry)
            }
        }
        guard let first = hidden.first else { return }
        overflow.append(OverflowBlock(
            events: hidden.map(\.event),
            startMinute: hidden.map(\.startMinute).min() ?? first.startMinute,
            endMinute: hidden.map(\.endMinute).max() ?? first.endMinute,
            lane: overflowLane,
            laneCount: maxVisibleLanes
        ))
    }

    /// Wall-clock minutes from midnight — matches how the grid draws hours,
    /// so a 09:00 event sits on the 09 line even across DST days.
    private static func wallMinutes(_ date: Date, calendar: Foundation.Calendar) -> Int {
        let comps = calendar.dateComponents([.hour, .minute], from: date)
        return (comps.hour ?? 0) * 60 + (comps.minute ?? 0)
    }
}

// MARK: - Week cache

/// Memoizes the week's per-day layouts so the greedy lane layout runs at
/// most once per input change — not once per column per body evaluation,
/// and not on drag frames (a resize step re-evaluates the body but hits the
/// cache; a move routes its per-frame translation around the grid entirely).
///
/// Invalidation signature: the day keys plus each event's layout-relevant
/// fields (id, start, end, allDay) plus maxVisibleLanes. The column width
/// participates only via maxVisibleLanes — the layout is in minutes/lanes,
/// pixel widths are applied at render time. A plain reference type held in
/// @State: SwiftUI keeps the instance across body evaluations, and a cache
/// update must not itself trigger a re-render.
final class WeekLayoutCache {
    private var signature: Int?
    private var layouts: [Date: DayLayoutIndex] = [:]

    func layouts(days: [Date], events: [CalendarEvent], maxVisibleLanes: Int,
                 calendar: Foundation.Calendar = .current) -> [Date: DayLayoutIndex]
    {
        var hasher = Hasher()
        hasher.combine(days)
        hasher.combine(maxVisibleLanes)
        hasher.combine(events.count)
        for event in events {
            hasher.combine(event.id)
            hasher.combine(event.start)
            hasher.combine(event.end)
            hasher.combine(event.allDay)
        }
        let sig = hasher.finalize()
        if sig == signature { return layouts }

        var result: [Date: DayLayoutIndex] = [:]
        for day in days {
            guard let dayEnd = calendar.date(byAdding: .day, value: 1, to: day) else { continue }
            result[day] = DayLayoutIndex(events: events, dayStart: day, dayEnd: dayEnd,
                                         maxVisibleLanes: maxVisibleLanes, calendar: calendar)
        }
        layouts = result
        signature = sig
        return result
    }
}
