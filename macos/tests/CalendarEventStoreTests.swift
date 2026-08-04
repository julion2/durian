@testable import durian_lib
import XCTest

@MainActor
final class CalendarEventStoreTests: XCTestCase {

    // MARK: - Helpers

    /// A fixed base so all offsets in the tests are deterministic.
    private let base = Date(timeIntervalSince1970: 1_780_000_000)

    private func makeEvent(uid: String, start: TimeInterval, durationMinutes: Double = 60,
                           subject: String = "Event", calendar: String = "Calendar",
                           account: String = "user@example.com",
                           recurring: Bool = false) -> CalendarEvent
    {
        let s = base.addingTimeInterval(start)
        return CalendarEvent(
            uid: uid, calendar: calendar, subject: subject,
            start: s, end: s.addingTimeInterval(durationMinutes * 60),
            recurring: recurring, account: account
        )
    }

    private func window(from: TimeInterval, to: TimeInterval) -> DateInterval {
        DateInterval(start: base.addingTimeInterval(from), end: base.addingTimeInterval(to))
    }

    // MARK: - EventID stability

    func testNonRecurringIDStableAcrossReschedule() {
        var event = makeEvent(uid: "u1", start: 0)
        let before = event.id
        event.start = event.start.addingTimeInterval(3600)
        event.end = event.end.addingTimeInterval(7200)
        XCTAssertEqual(event.id, before, "moving a non-recurring event must not change its identity")
        XCTAssertNil(event.id.occurrence)
    }

    func testRecurringOccurrencesGetDistinctIDs() {
        let first = makeEvent(uid: "series", start: 0, recurring: true)
        let second = makeEvent(uid: "series", start: 7 * 24 * 3600, recurring: true)
        XCTAssertNotEqual(first.id, second.id, "occurrences share a uid, so the start must disambiguate")
        XCTAssertEqual(first.id.occurrence, first.start)
    }

    func testIDDistinguishesAccountAndCalendar() {
        let a = makeEvent(uid: "u1", start: 0, calendar: "Work", account: "a@example.com")
        let b = makeEvent(uid: "u1", start: 0, calendar: "Work", account: "b@example.com")
        let c = makeEvent(uid: "u1", start: 0, calendar: "Home", account: "a@example.com")
        XCTAssertNotEqual(a.id, b.id)
        XCTAssertNotEqual(a.id, c.id)
    }

    // MARK: - Reconcile

    func testReconcileInsertsNewEvents() {
        let store = CalendarEventStore()
        let events = [makeEvent(uid: "u1", start: 0), makeEvent(uid: "u2", start: 3600)]
        store.reconcile(fetched: events, within: window(from: 0, to: 86_400))
        XCTAssertEqual(store.events.count, 2)
        XCTAssertNotNil(store.byID[events[0].id])
        XCTAssertNotNil(store.byID[events[1].id])
    }

    func testReconcileUpdatesChangedEventInPlace() {
        let store = CalendarEventStore()
        let original = makeEvent(uid: "u1", start: 0, subject: "Before")
        store.reconcile(fetched: [original], within: window(from: 0, to: 86_400))

        let renamed = makeEvent(uid: "u1", start: 0, subject: "After")
        store.reconcile(fetched: [renamed], within: window(from: 0, to: 86_400))

        XCTAssertEqual(store.events.count, 1)
        XCTAssertEqual(store.byID[original.id]?.subject, "After")
        XCTAssertEqual(renamed.id, original.id, "a field change must land on the same identity")
    }

    func testReconcileRemovesAbsentEventInsideWindow() {
        let store = CalendarEventStore()
        let stays = makeEvent(uid: "stays", start: 0)
        let goes = makeEvent(uid: "goes", start: 3600)
        store.reconcile(fetched: [stays, goes], within: window(from: 0, to: 86_400))

        store.reconcile(fetched: [stays], within: window(from: 0, to: 86_400))
        XCTAssertEqual(store.events.map(\.uid), ["stays"])
        XCTAssertNil(store.byID[goes.id])
    }

    func testReconcilePreservesEventOutsideWindow() {
        let store = CalendarEventStore()
        let lastWeek = makeEvent(uid: "past", start: -7 * 24 * 3600)
        let thisWeek = makeEvent(uid: "now", start: 3600)
        store.reconcile(fetched: [lastWeek], within: window(from: -7 * 24 * 3600, to: 0))

        // A fetch for this week does not contain last week's event; it must
        // survive because it lies outside the fetched window.
        store.reconcile(fetched: [thisWeek], within: window(from: 0, to: 7 * 24 * 3600))
        XCTAssertEqual(store.events.count, 2)
        XCTAssertNotNil(store.byID[lastWeek.id])
    }

    func testReconcileRemovesAbsentEventOverlappingWindow() {
        let store = CalendarEventStore()
        // Starts before the window but ends inside it: the fetch covered it
        // (list fetches return every overlapping event), so its absence
        // means the server deleted it.
        let spanning = makeEvent(uid: "span", start: -3600, durationMinutes: 120)
        store.reconcile(fetched: [spanning], within: nil)

        store.reconcile(fetched: [], within: window(from: 0, to: 86_400))
        XCTAssertNil(store.byID[spanning.id])
        XCTAssertTrue(store.events.isEmpty)
    }

    func testReconcilePreservesEventEndingAtWindowStart() {
        let store = CalendarEventStore()
        // Ends exactly at the window start: half-open on both sides, so it
        // does not overlap the fetch and must survive.
        let earlier = makeEvent(uid: "earlier", start: -3600, durationMinutes: 60)
        store.reconcile(fetched: [earlier], within: nil)

        store.reconcile(fetched: [], within: window(from: 0, to: 86_400))
        XCTAssertNotNil(store.byID[earlier.id])
    }

    func testReconcileWindowIsHalfOpen() {
        let store = CalendarEventStore()
        let atStart = makeEvent(uid: "at-start", start: 0)
        let atEnd = makeEvent(uid: "at-end", start: 86_400)
        store.reconcile(fetched: [atStart, atEnd], within: nil)

        // [0, 86400): the event starting exactly at the window end was not
        // part of the fetch's range, so it must be preserved; the one at the
        // window start was, so it is removed.
        store.reconcile(fetched: [], within: window(from: 0, to: 86_400))
        XCTAssertNil(store.byID[atStart.id])
        XCTAssertNotNil(store.byID[atEnd.id])
    }

    func testReconcileNilWindowRemovesAllAbsent() {
        let store = CalendarEventStore()
        let old = makeEvent(uid: "old", start: -30 * 24 * 3600)
        let hit = makeEvent(uid: "hit", start: 3600)
        store.reconcile(fetched: [old], within: window(from: -31 * 24 * 3600, to: 0))

        // Search mode: the fetch IS the full visible set, so even the event
        // far outside any window is dropped when absent.
        store.reconcile(fetched: [hit], within: nil)
        XCTAssertEqual(store.events.map(\.uid), ["hit"])
    }

    func testReconcileEmptyFetchInsideWindowClearsWindowOnly() {
        let store = CalendarEventStore()
        let inside = makeEvent(uid: "inside", start: 3600)
        let outside = makeEvent(uid: "outside", start: 10 * 24 * 3600)
        store.reconcile(fetched: [inside, outside], within: nil)

        store.reconcile(fetched: [], within: window(from: 0, to: 86_400))
        XCTAssertEqual(store.events.map(\.uid), ["outside"])
    }

    // MARK: - Window overlap (visible projection)

    func testOverlapsIncludesEventSpanningIntoWindow() {
        let w = window(from: 0, to: 86_400)
        // Starts before the window, ends inside: visible (the week grid
        // renders it clamped).
        XCTAssertTrue(CalendarEventStore.overlaps(
            w, start: base.addingTimeInterval(-3600), end: base.addingTimeInterval(3600)))
        // Ends exactly at the window start (half-open): not visible.
        XCTAssertFalse(CalendarEventStore.overlaps(
            w, start: base.addingTimeInterval(-3600), end: base))
        // Starts exactly at the window end: not visible.
        XCTAssertFalse(CalendarEventStore.overlaps(
            w, start: base.addingTimeInterval(86_400), end: base.addingTimeInterval(90_000)))
    }

    func testOverlapsMatchesContainsForZeroDuration() {
        let w = window(from: 0, to: 86_400)
        XCTAssertTrue(CalendarEventStore.overlaps(w, start: base, end: base),
                      "a zero-duration event at the window start counts, matching contains")
        XCTAssertFalse(CalendarEventStore.overlaps(
            w, start: base.addingTimeInterval(-1), end: base.addingTimeInterval(-1)))
    }

    // MARK: - Optimistic mutations

    func testApplyOptimisticInsertsAndUpdates() {
        let store = CalendarEventStore()
        let event = makeEvent(uid: "u1", start: 0)
        store.applyOptimistic(event)
        XCTAssertEqual(store.events.count, 1)

        var moved = event
        moved.start = event.start.addingTimeInterval(1800)
        moved.end = event.end.addingTimeInterval(1800)
        store.applyOptimistic(moved)
        XCTAssertEqual(store.events.count, 1, "a moved event upserts onto its stable id, no duplicate")
        XCTAssertEqual(store.byID[event.id]?.start, moved.start)
    }

    func testRemoveDeletesEvent() {
        let store = CalendarEventStore()
        let event = makeEvent(uid: "u1", start: 0)
        store.applyOptimistic(event)
        store.remove(event.id)
        XCTAssertTrue(store.events.isEmpty)
        XCTAssertNil(store.byID[event.id])
    }

    func testRemoveUnknownIDIsNoOp() {
        let store = CalendarEventStore()
        let event = makeEvent(uid: "u1", start: 0)
        store.applyOptimistic(event)
        store.remove(makeEvent(uid: "other", start: 0).id)
        XCTAssertEqual(store.events.count, 1)
    }

    // MARK: - Snapshot / rollback

    func testSnapshotRestoreRollsBackOptimisticWrite() {
        let store = CalendarEventStore()
        let event = makeEvent(uid: "u1", start: 0, subject: "Original")
        store.applyOptimistic(event)

        let snapshot = store.snapshot()
        store.applyOptimistic(makeEvent(uid: "u1", start: 0, subject: "Edited"))
        store.applyOptimistic(makeEvent(uid: "u2", start: 3600))

        store.restore(snapshot)
        XCTAssertEqual(store.events.count, 1)
        XCTAssertEqual(store.byID[event.id]?.subject, "Original")
    }

    // MARK: - Ordered projection

    func testProjectionIsStartSortedAfterReconcile() {
        let store = CalendarEventStore()
        let shuffled = [
            makeEvent(uid: "c", start: 7200),
            makeEvent(uid: "a", start: 0),
            makeEvent(uid: "b", start: 3600),
        ]
        store.reconcile(fetched: shuffled, within: nil)
        XCTAssertEqual(store.events.map(\.uid), ["a", "b", "c"])
    }

    func testProjectionStaysSortedAfterOptimisticMove() {
        let store = CalendarEventStore()
        let first = makeEvent(uid: "first", start: 0)
        let second = makeEvent(uid: "second", start: 3600)
        store.reconcile(fetched: [first, second], within: nil)

        var moved = first
        moved.start = base.addingTimeInterval(7200)
        moved.end = moved.start.addingTimeInterval(3600)
        store.applyOptimistic(moved)
        XCTAssertEqual(store.events.map(\.uid), ["second", "first"])
    }

    func testProjectionTieBreakIsDeterministic() {
        let store = CalendarEventStore()
        let a = makeEvent(uid: "a", start: 0)
        let b = makeEvent(uid: "b", start: 0)
        store.reconcile(fetched: [b, a], within: nil)
        let once = store.events.map(\.uid)
        store.reconcile(fetched: [a, b], within: nil)
        XCTAssertEqual(store.events.map(\.uid), once, "equal starts must order the same on every rebuild")
    }
}

// MARK: - Draft write payload (attendees / online meeting)

final class CalendarEventDraftWriteTests: XCTestCase {

    func testNewDraftCarriesAttendeesAndOnlineMeeting() {
        var draft = CalendarEventDraft(
            account: "me@example.com", calendar: "Calendar",
            start: Date(timeIntervalSince1970: 1_780_000_000),
            end: Date(timeIntervalSince1970: 1_780_003_600)
        )
        XCTAssertTrue(draft.attendees.isEmpty)
        XCTAssertFalse(draft.requestOnlineMeeting)

        draft.subject = "Kickoff"
        draft.attendees = ["alice@example.com", "bob@example.com"]
        draft.requestOnlineMeeting = true

        let write = draft.toWrite()
        XCTAssertEqual(write.attendees, ["alice@example.com", "bob@example.com"])
        XCTAssertTrue(write.request_online_meeting)
    }

    func testExistingEventDraftNeverSendsAttendees() {
        // Attendee editing is create-only: a draft of an existing event
        // starts empty, and even a tampered draft sends the neutral values so
        // the server-side merge preserves the meeting's attendee set.
        let event = CalendarEvent(
            uid: "evt-1", calendar: "Calendar", subject: "Sync",
            start: Date(timeIntervalSince1970: 1_780_000_000),
            end: Date(timeIntervalSince1970: 1_780_003_600),
            account: "me@example.com"
        )
        var draft = CalendarEventDraft(from: event)
        XCTAssertFalse(draft.isNew)
        XCTAssertTrue(draft.attendees.isEmpty)

        draft.attendees = ["alice@example.com"]
        draft.requestOnlineMeeting = true
        let write = draft.toWrite()
        XCTAssertEqual(write.attendees, [])
        XCTAssertFalse(write.request_online_meeting)
    }
}
