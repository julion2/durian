//
//  CalendarEventStore.swift
//  Durian
//
//  A reconciling store for calendar events keyed by their stable EventID.
//  Fetch results are DIFFED into the store (upsert + windowed removal)
//  instead of wholesale-replacing an array: an unchanged event keeps its
//  identity across refreshes, so SwiftUI updates blocks in place rather than
//  remove+insert (no flicker) and the selection survives moves/edits.
//

import Foundation

@MainActor
final class CalendarEventStore: ObservableObject {

    /// All known events by stable id. Not published itself — mutations always
    /// go through rebuildProjection, which publishes `events`.
    private(set) var byID: [EventID: CalendarEvent] = [:]

    /// The ordered projection views read: all events, start-sorted with the
    /// EventID as a deterministic tie-break for equal starts.
    @Published private(set) var events: [CalendarEvent] = []

    // MARK: - Reconcile

    /// Merges a fetch result into the store. Every fetched event is upserted
    /// by id. Removal is scoped: an id absent from `fetched` is dropped only
    /// when it OVERLAPS `window` (the range the fetch covered) — the fetch
    /// returns every event overlapping the window (that is how multi-day
    /// events reach the grid at all), so an absent overlapping event was
    /// deleted server-side, while events fully outside the window were not
    /// fetched and their absence says nothing. With `window == nil` the
    /// fetch is authoritative for everything (search mode) and any absent id
    /// is removed.
    func reconcile(fetched: [CalendarEvent], within window: DateInterval?) {
        let fetchedIDs = Set(fetched.map(\.id))
        for (id, event) in byID where !fetchedIDs.contains(id) {
            if let window {
                if Self.overlaps(window, start: event.start, end: event.end) {
                    byID.removeValue(forKey: id)
                }
            } else {
                byID.removeValue(forKey: id)
            }
        }
        for event in fetched {
            byID[event.id] = event
        }
        rebuildProjection()
    }

    // MARK: - Local mutations (optimistic writes)

    /// Upserts one event by id — the immediate local change for an edit,
    /// create or reschedule before the server round-trip; the following
    /// refresh() reconciles with the server's truth.
    func applyOptimistic(_ event: CalendarEvent) {
        byID[event.id] = event
        rebuildProjection()
    }

    func remove(_ id: EventID) {
        guard byID.removeValue(forKey: id) != nil else { return }
        rebuildProjection()
    }

    // MARK: - Snapshot / rollback

    /// The current contents, for rolling back a failed optimistic write
    /// without waiting for a refetch.
    func snapshot() -> [EventID: CalendarEvent] {
        byID
    }

    func restore(_ snapshot: [EventID: CalendarEvent]) {
        byID = snapshot
        rebuildProjection()
    }

    // MARK: - Window membership

    /// Half-open [start, end) containment, matching the API's fetch window —
    /// an event starting exactly at the window's end was not fetched, so it
    /// must not be reconciled away.
    static func contains(_ window: DateInterval, _ date: Date) -> Bool {
        window.start <= date && date < window.end
    }

    /// Half-open interval overlap: [start, end) intersects [window.start,
    /// window.end). Used by the visible projection so a multi-day event that
    /// starts before the window still reaches the week grid. A zero-duration
    /// event counts when its start lies in the window, matching `contains`.
    static func overlaps(_ window: DateInterval, start: Date, end: Date) -> Bool {
        start < window.end && (end > window.start || start >= window.start)
    }

    // MARK: - Projection

    private func rebuildProjection() {
        events = byID.values.sorted { a, b in
            if a.start != b.start { return a.start < b.start }
            return a.id < b.id
        }
    }
}
