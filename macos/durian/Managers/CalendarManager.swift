//
//  CalendarManager.swift
//  Durian
//
//  Owns the calendar UI state: fetches calendars/events from the CLI HTTP API
//  (via CalendarBackend, reusing the running serve + token), maps them to
//  domain models, and drives the calendar views. Read-only. Refreshes
//  on-demand (calendar is file/sync-driven, no push).
//

import Combine
import Foundation

// MARK: - View mode

enum CalendarViewMode: String, CaseIterable, Identifiable {
    case agenda, week, month, year
    var id: String { rawValue }
    var title: String { rawValue.capitalized }
}

// MARK: - Manager

@MainActor
final class CalendarManager: ObservableObject {
    static let shared = CalendarManager()

    private init() {
        // Restore the persisted sidebar state before the store sink exists.
        // Direct assignments in init skip the didSet persistence observers,
        // so loading never writes back.
        hiddenCalendars = Self.loadHiddenCalendars()
        hideDeclined = UserDefaults.standard.bool(forKey: Self.hideDeclinedKey)
        sidebarVisible = UserDefaults.standard.bool(forKey: Self.sidebarVisibleKey)

        // Mirror the store's projection into the published `events` the views
        // read, limited to the current view window: the store deliberately
        // holds MORE than the view shows (prefetched neighbours, preserved
        // out-of-window events), and showing them would leak neighbours of
        // other periods into e.g. the agenda. This sink only fires on store
        // changes — a `visibleWindow`-only change (the skip path) must call
        // reproject() explicitly.
        store.$events
            .sink { [weak self] all in
                self?.project(all)
            }
            .store(in: &cancellables)
    }

    private let backend = CalendarBackend()
    private let store = CalendarEventStore()
    private var cancellables: Set<AnyCancellable> = []
    private var loadTask: Task<Void, Never>?
    private var detailTask: Task<Void, Never>?

    /// The window the current view shows (nil in search mode) — bounds the
    /// visible projection above. Always the REQUESTED window, never the wider
    /// augmented one, so prefetched neighbours sit in the store without
    /// leaking into the view.
    private var visibleWindow: DateInterval?

    /// The union of the AUGMENTED windows the completed fetches covered
    /// (overlapping fetches accumulate, bounded — see
    /// CalendarRangeCoverage.accumulate), and the account set they were
    /// fetched under. While a requested window stays inside `loadedRange`
    /// (same accounts, not in search, not forced), refresh() skips the
    /// network entirely. Search mode never records a
    /// range (its fetch is query-scoped, not window-scoped), so leaving
    /// search always refetches; an account-set change fails the equality
    /// check and refetches too.
    private var loadedRange: DateInterval?
    private var loadedAccounts: [String] = []

    @Published var calendars: [CalendarInfo] = []

    /// Calendar names the user hid via the sidebar toggles. Persisted across
    /// launches; the visible `events` projection excludes their events (the
    /// store still holds them, so re-showing a calendar is instant).
    @Published var hiddenCalendars: Set<String> = [] {
        didSet {
            guard hiddenCalendars != oldValue else { return }
            Self.saveHiddenCalendars(hiddenCalendars)
            // The store did not change, so the sink will not fire — re-filter
            // explicitly and keep the selection on a still-visible event.
            reproject()
            revalidateSelection()
        }
    }

    /// Hides events the account owner DECLINED (myResponse == "declined").
    /// Persisted; defaults to showing everything. Declining an event you
    /// organize never sets that response, so only genuinely-declined
    /// invitations disappear. Same shape as `hiddenCalendars`: the store keeps
    /// the events, only the visible projection drops them.
    @Published var hideDeclined: Bool = false {
        didSet {
            guard hideDeclined != oldValue else { return }
            UserDefaults.standard.set(hideDeclined, forKey: Self.hideDeclinedKey)
            // The store did not change, so the sink will not fire — re-filter
            // explicitly and keep the selection on a still-visible event.
            reproject()
            revalidateSelection()
        }
    }

    /// Whether the left calendar panel (mini month + calendar list) is shown.
    /// Persisted; defaults to collapsed.
    @Published var sidebarVisible: Bool = false {
        didSet {
            guard sidebarVisible != oldValue else { return }
            UserDefaults.standard.set(sidebarVisible, forKey: Self.sidebarVisibleKey)
        }
    }

    @Published private(set) var events: [CalendarEvent] = []
    @Published var selectedEventID: EventID? {
        didSet {
            if selectedEventID != oldValue { loadDetail() }
        }
    }

    /// The selected event with its full detail (attendees, organizer,
    /// description), fetched lazily — the list endpoint only returns summaries.
    @Published var detailEvent: CalendarEvent?
    @Published var isLoading = false

    @Published var viewMode: CalendarViewMode = .agenda
    @Published var anchorDate: Date = Date()
    @Published var searchQuery: String = ""

    var selectedEvent: CalendarEvent? {
        guard let id = selectedEventID else { return nil }
        return store.byID[id]
    }

    /// Which accounts' calendars to show. The calendar is deliberately NOT
    /// scoped by the active mail profile — it queries every configured account
    /// so the calendar always appears regardless of which mail profile is
    /// selected. Accounts without a calendar vdir simply return nothing.
    private var accountIdentifiers: [String] {
        ConfigManager.shared.getAccounts().map { $0.email }
    }

    // MARK: - Loading

    /// Refreshes the view for the current mode/anchor/query. Fetches cover an
    /// AUGMENTED window (2x the requested width, centered — see
    /// CalendarRangeCoverage), so as long as navigation stays inside the last
    /// fetched band the network — and the loading spinner — are skipped and
    /// only the projection window moves. `force` bypasses the skip: after a
    /// local write the store must reconcile against the server even when the
    /// range is covered.
    func refresh(force: Bool = false) {
        let accounts = accountIdentifiers
        let query = searchQuery.trimmingCharacters(in: .whitespacesAndNewlines)
        let (from, to) = window()
        let requested = DateInterval(start: from, end: to)

        // The projection window follows the VIEW immediately (both paths):
        // the store may already hold events for the new range, and the sink
        // only re-filters on store changes, so re-project explicitly.
        visibleWindow = query.isEmpty ? requested : nil
        reproject()

        if !force, query.isEmpty, accounts == loadedAccounts,
           CalendarRangeCoverage.covers(loadedRange, requested)
        {
            // Already prefetched: the store holds everything this window can
            // show. No spinner, no fetch, no calendar re-list — and an
            // in-flight load (if any) keeps running and will reconcile into
            // the store under the projection window set above.
            revalidateSelection()
            return
        }

        loadTask?.cancel()
        // Prefetch around the view (skip augmentation in search mode: that
        // fetch is query-scoped, not window-scoped).
        let fetchWindow = query.isEmpty ? CalendarRangeCoverage.augmented(requested) : nil

        loadTask = Task { [weak self] in
            guard let self else { return }
            isLoading = true
            // Only the task that still owns the load may clear the spinner: a
            // cancelled task racing its replacement must not turn isLoading
            // off right after the new task turned it on.
            defer { if !Task.isCancelled { isLoading = false } }

            var loadedCalendars: [CalendarInfo] = []
            var loadedEvents: [CalendarEvent] = []
            for account in accounts {
                if Task.isCancelled { return }
                let cals = await backend.listCalendars(account: account)
                for wire in cals {
                    var info = CalendarInfo(from: wire)
                    info.account = account
                    loadedCalendars.append(info)
                }
                let wire = await backend.listEvents(
                    account: account,
                    from: fetchWindow?.start,
                    to: fetchWindow?.end,
                    query: query.isEmpty ? nil : query
                )
                for w in wire {
                    if var event = CalendarEvent(from: w) {
                        event.account = account
                        loadedEvents.append(event)
                    }
                }
            }
            if Task.isCancelled { return }

            calendars = Self.dedup(loadedCalendars)
            // Diff the fetch into the store instead of replacing the array:
            // unchanged events keep their identity, so the views settle once.
            // Reconcile is scoped to the window the fetch ACTUALLY covered
            // (the augmented one), so windowed removal stays correct; in
            // search mode the result is the full visible set (nil window).
            store.reconcile(fetched: loadedEvents, within: fetchWindow)
            if let fetchWindow {
                // Widening band: an overlapping fetch UNIONS into the
                // previous coverage (so stepping back over visited weeks
                // stays spinner-free), a disjoint jump or an oversized union
                // resets — see CalendarRangeCoverage.accumulate. Coverage
                // fetched under a different account set never unions: those
                // events were not fetched for the new set.
                loadedRange = accounts == loadedAccounts
                    ? CalendarRangeCoverage.accumulate(loadedRange, adding: fetchWindow)
                    : fetchWindow
            } else {
                loadedRange = nil
            }
            loadedAccounts = accounts
            // Keep a valid selection (the store sink has already updated
            // `events` synchronously above).
            revalidateSelection()
        }
    }

    /// Re-filters `events` from the store under the current `visibleWindow`.
    /// Needed whenever the window changes WITHOUT a store change (the skip
    /// path): the store sink does not fire then.
    private func reproject() {
        project(store.events)
    }

    private func project(_ all: [CalendarEvent]) {
        events = Self.visibleEvents(all, window: visibleWindow, hidden: hiddenCalendars,
                                    hideDeclined: hideDeclined)
    }

    /// The single visibility filter behind the projection, extracted pure so
    /// it is unit-testable: an event is visible when its calendar is not
    /// hidden, it is not a declined invitation while `hideDeclined` is on,
    /// AND it overlaps the window (nil window = search mode, no
    /// window bound). Overlap, not start-containment: a multi-day event that
    /// starts before the window must still reach the week grid, which renders
    /// it clamped on each day it touches. Agenda/month/year bucket by start
    /// day, so such an event simply groups under its own (possibly
    /// pre-window) start day — per-day spanning there is deferred.
    nonisolated static func visibleEvents(_ events: [CalendarEvent], window: DateInterval?,
                                          hidden: Set<String>,
                                          hideDeclined: Bool = false) -> [CalendarEvent]
    {
        events.filter { event in
            // Visibility is scoped per (account, calendar), matching the
            // hidden-set keys, so hiding "Work" in one account leaves another
            // account's "Work" visible.
            if hidden.contains(CalendarInfo.key(account: event.account, name: event.calendar)) {
                return false
            }
            if hideDeclined, event.myResponse == "declined" {
                return false
            }
            guard let window else { return true }
            return CalendarEventStore.overlaps(window, start: event.start, end: event.end)
        }
    }

    /// Keeps the selection pointing at a visible event, falling back to the
    /// first one when it left the window (or nothing was selected yet).
    private func revalidateSelection() {
        if let id = selectedEventID, !events.contains(where: { $0.id == id }) {
            selectedEventID = events.first?.id
        } else if selectedEventID == nil {
            selectedEventID = events.first?.id
        }
    }

    // MARK: - Sidebar & per-calendar visibility

    func toggleSidebar() {
        sidebarVisible.toggle()
    }

    /// Shows/hides a calendar's events; the didSet on `hiddenCalendars`
    /// persists, re-projects and revalidates the selection.
    func toggleCalendar(_ calendar: CalendarInfo) {
        let key = calendar.visibilityKey
        if hiddenCalendars.contains(key) {
            hiddenCalendars.remove(key)
        } else {
            hiddenCalendars.insert(key)
        }
    }

    func isCalendarVisible(_ calendar: CalendarInfo) -> Bool {
        !hiddenCalendars.contains(calendar.visibilityKey)
    }

    // MARK: - Sidebar state persistence

    private static let hiddenCalendarsKey = "durian.calendar.hiddenCalendars"
    private static let hideDeclinedKey = "durian.calendar.hideDeclined"
    private static let sidebarVisibleKey = "durian.calendar.sidebarVisible"

    /// Load/save split out with an injectable UserDefaults so the round-trip
    /// is testable against a scratch suite.
    nonisolated static func loadHiddenCalendars(from defaults: UserDefaults = .standard) -> Set<String> {
        Set(defaults.stringArray(forKey: hiddenCalendarsKey) ?? [])
    }

    nonisolated static func saveHiddenCalendars(_ hidden: Set<String>,
                                                to defaults: UserDefaults = .standard)
    {
        // Sorted for a stable on-disk representation (sets have no order).
        defaults.set(hidden.sorted(), forKey: hiddenCalendarsKey)
    }

    // MARK: - Navigation used by the menu / keymaps

    func goToToday() {
        anchorDate = Date()
        refresh()
    }

    func step(_ direction: Int) {
        let cal = Calendar.current
        let component: Calendar.Component
        switch viewMode {
        case .agenda, .week: component = .weekOfYear
        case .month: component = .month
        case .year: component = .year
        }
        anchorDate = cal.date(byAdding: component, value: direction, to: anchorDate) ?? anchorDate
        refresh()
    }

    func setViewMode(_ mode: CalendarViewMode) {
        guard mode != viewMode else { return }
        viewMode = mode
        refresh()
    }

    func setSearch(_ query: String) {
        searchQuery = query
        refresh()
    }

    /// Moves the cursor within the sorted event list (vim j/k).
    func moveSelection(by delta: Int) {
        guard !events.isEmpty else { return }
        let current = events.firstIndex { $0.id == selectedEventID } ?? 0
        let next = min(max(current + delta, 0), events.count - 1)
        selectedEventID = events[next].id
    }

    func selectFirst() { selectedEventID = events.first?.id }
    func selectLast() { selectedEventID = events.last?.id }

    /// RSVP to a meeting via the local-first write endpoint (POST
    /// /calendars/rsvp): only the owner's PARTSTAT in the local .ics changes —
    /// no mail is sent now. The organizer is notified on the next `durian
    /// calendar sync`, which previews the reply behind its confirmation gate.
    func requestRSVP(_ event: CalendarEvent, response: String) {
        guard !event.account.isEmpty else { return }
        let account = event.account
        let uid = event.uid
        let calendar = event.calendar
        Task { [weak self] in
            guard let self else { return }
            guard await backend.rsvp(account: account, calendar: calendar,
                                     ref: uid, response: response) != nil
            else {
                BannerManager.shared.showWarning(
                    title: "Couldn't save RSVP",
                    message: "The write failed — make sure the durian CLI is up to date."
                )
                return
            }
            // Make the local-first model explicit: the response is only saved
            // locally until the next calendar sync notifies the organizer.
            let name = ConfigManager.shared.getAccounts()
                .first { $0.email == account }?.name ?? account
            let syncTarget = name.contains(" ") ? "\"\(name)\"" : name
            BannerManager.shared.showInfo(
                title: "Response saved",
                message: "Run 'durian calendar sync \(syncTarget)' (or wait for the next sync) to notify the organizer."
            )
            // Reconcile the store with the server and refetch the detail so
            // the pane shows the new response.
            refresh(force: true)
            loadDetail()
        }
    }

    // MARK: - Editing (local-first writes)

    /// The draft shown in the create/edit sheet; nil when no sheet is open.
    @Published var editingDraft: CalendarEventDraft?

    /// Opens the edit sheet for the selected event (uses the full detail so the
    /// description/attendees are available).
    func beginEdit() {
        guard let event = detailEvent ?? selectedEvent else { return }
        if event.recurring && event.seriesStart == nil {
            // Only the detail fetch knows the series master's times; editing a
            // recurring event from the summary alone would write the selected
            // occurrence's date onto the master and shift the whole series.
            BannerManager.shared.showWarning(
                title: "Event details still loading",
                message: "Try editing again in a moment."
            )
            return
        }
        editingDraft = CalendarEventDraft(from: event)
    }

    /// Opens the edit sheet for a new event, defaulting the time to `date` (or
    /// the next hour on the anchor day) in the first calendar.
    func beginCreate(at date: Date? = nil) {
        // Default to the busiest calendar (usually the primary one) rather than
        // the alphabetically-first (e.g. "Birthdays").
        guard let calendar = calendars.max(by: { $0.eventCount < $1.eventCount }) else {
            BannerManager.shared.showWarning(
                title: "No calendars available",
                message: "Run 'durian calendar sync' first to sync your calendars."
            )
            return
        }
        let cal = Calendar.current
        let start: Date
        if let date {
            start = date
        } else {
            let day = cal.dateComponents([.year, .month, .day], from: anchorDate)
            var comps = cal.dateComponents([.hour], from: Date())
            comps.year = day.year; comps.month = day.month; comps.day = day.day; comps.minute = 0
            start = cal.date(from: comps) ?? Date()
        }
        let end = cal.date(byAdding: .hour, value: 1, to: start) ?? start
        editingDraft = CalendarEventDraft(account: calendar.account, calendar: calendar.name, start: start, end: end)
    }

    /// Persists a draft (create or update) via the local-first write API and
    /// refreshes.
    func commitDraft(_ draft: CalendarEventDraft) {
        editingDraft = nil
        applyDraftOptimistically(draft)
        Task { [weak self] in
            guard let self else { return }
            guard await backend.putEvent(draft.toWrite()) != nil else {
                BannerManager.shared.showWarning(
                    title: "Couldn't save event",
                    message: "The write failed — make sure the durian CLI is up to date."
                )
                refresh(force: true)
                return
            }
            if draft.isNew && (!draft.attendees.isEmpty || draft.requestOnlineMeeting) {
                // Make the local-first model explicit: inviting attendees (or
                // requesting an online meeting) is a notifying action, so
                // automatic sync will NOT push it — the invitations go out
                // only on a manual sync, behind its preview gate.
                let name = ConfigManager.shared.getAccounts()
                    .first { $0.email == draft.account }?.name ?? draft.account
                let syncTarget = name.contains(" ") ? "\"\(name)\"" : name
                BannerManager.shared.showInfo(
                    title: "Event saved locally",
                    message: "Run 'durian calendar sync \(syncTarget)' to send the invitations — automatic sync does not send them."
                )
            }
            // A local edit must reconcile against the server even when the
            // window is covered by a previous fetch.
            refresh(force: true)
        }
    }

    /// Shows an edit's result immediately by upserting the edited copy into
    /// the store; the refresh after the PUT reconciles with the server. Only
    /// safe for an existing NON-recurring event still in the same calendar:
    /// a new event has no uid yet, a series edit writes the master (the
    /// occurrences' times are derived server-side), and a calendar change
    /// changes the identity — those simply wait for the refetch.
    private func applyDraftOptimistically(_ draft: CalendarEventDraft) {
        guard !draft.isNew, !draft.recurring else { return }
        let id = EventID(account: draft.account, calendar: draft.calendar, uid: draft.uid, occurrence: nil)
        guard let existing = store.byID[id] else { return }
        let updated = CalendarEvent(
            uid: existing.uid, calendar: existing.calendar, subject: draft.subject,
            start: draft.start, end: draft.end, allDay: draft.allDay,
            location: draft.location.isEmpty ? nil : draft.location,
            myResponse: existing.myResponse, onlineMeeting: existing.onlineMeeting,
            onlineMeetingURL: existing.onlineMeetingURL, recurring: existing.recurring,
            organizer: existing.organizer, attendees: existing.attendees,
            description: draft.description.isEmpty ? nil : draft.description,
            account: existing.account
        )
        store.applyOptimistic(updated)
    }

    /// Reschedules a timed event from a drag (move or resize) by writing new
    /// start/end to the local .ics, preserving everything else (the API's
    /// update-merge keeps attendees/RRULE). Recurring and all-day events are
    /// left to the edit form — their series/whole-day semantics don't map onto
    /// a free-form drag.
    func reschedule(_ event: CalendarEvent, start newStart: Date, end newEnd: Date) {
        guard !event.recurring, !event.allDay, !event.account.isEmpty else { return }
        guard newStart != event.start || newEnd != event.end else { return }
        var draft = CalendarEventDraft(from: event)
        draft.start = newStart
        draft.end = newEnd
        // Optimistic: settle the block at its new spot before the round-trip so
        // the drag feels immediate; refresh() reconciles with the server. The
        // id is stable across a move (non-recurring), so the upsert lands on
        // the same entry and an existing selection stays valid by itself.
        var moved = event
        moved.start = newStart
        moved.end = newEnd
        store.applyOptimistic(moved)
        Task { [weak self] in
            guard let self else { return }
            if await backend.putEvent(draft.toWrite()) == nil {
                BannerManager.shared.showWarning(
                    title: "Couldn't move event",
                    message: "The write failed — make sure the durian CLI is up to date."
                )
            }
            refresh(force: true)
        }
    }

    /// Deletes the selected event locally and refreshes.
    func deleteSelected() {
        guard let event = detailEvent ?? selectedEvent, !event.account.isEmpty else { return }
        let account = event.account
        let uid = event.uid
        let calendar = event.calendar
        selectedEventID = nil
        // Optimistic: drop the event (for a series, this occurrence — the
        // refresh clears its siblings) so the UI reacts before the round-trip.
        store.remove(event.id)
        Task { [weak self] in
            guard let self else { return }
            if await backend.deleteEvent(account: account, ref: uid, calendar: calendar) == false {
                BannerManager.shared.showWarning(
                    title: "Couldn't delete event",
                    message: "The delete failed — make sure the durian CLI is up to date."
                )
            }
            refresh(force: true)
        }
    }

    /// Fetches the full detail (attendees, organizer, description) of the
    /// selected event; the list only carries summaries. Falls back silently.
    private func loadDetail() {
        detailTask?.cancel()
        detailEvent = nil
        guard let id = selectedEventID,
              let summary = store.byID[id]
        else { return }

        let account = summary.account
        let uid = summary.uid
        let calendar = summary.calendar
        detailTask = Task { [weak self] in
            guard let self else { return }
            guard let wire = await backend.event(account: account, ref: uid, calendar: calendar),
                  var full = CalendarEvent(from: wire)
            else { return }
            if Task.isCancelled || selectedEventID != id { return }
            full.account = account
            // The detail endpoint resolves the series master, so keep the
            // occurrence's own start/end for a correct date on recurring
            // events — but remember the master's times: editing a series must
            // write those, not the occurrence's (see CalendarEventDraft).
            full.seriesStart = full.start
            full.seriesEnd = full.end
            full.start = summary.start
            full.end = summary.end
            detailEvent = full
        }
    }

    // MARK: - Window

    /// The [from, to) window for the current view mode, anchored on anchorDate.
    /// Local-day aligned so it matches how the views group events; the extra day
    /// of padding catches events that straddle a local/UTC boundary. The API
    /// filters on the absolute instants regardless of zone.
    private func window() -> (Date, Date) {
        let cal = Calendar.current
        let dayStart = cal.startOfDay(for: anchorDate)
        let pad = 1
        switch viewMode {
        case .agenda:
            return (dayStart, cal.date(byAdding: .day, value: 30, to: dayStart) ?? dayStart)
        case .week:
            let start = cal.dateInterval(of: .weekOfYear, for: anchorDate)?.start ?? dayStart
            let from = cal.date(byAdding: .day, value: -pad, to: start) ?? start
            return (from, cal.date(byAdding: .day, value: 7 + pad, to: start) ?? start)
        case .month:
            let start = cal.dateInterval(of: .month, for: anchorDate)?.start ?? dayStart
            let from = cal.date(byAdding: .day, value: -7, to: start) ?? start
            let end = cal.date(byAdding: .month, value: 1, to: start) ?? start
            return (from, cal.date(byAdding: .day, value: 7, to: end) ?? end)
        case .year:
            let start = cal.dateInterval(of: .year, for: anchorDate)?.start ?? dayStart
            return (start, cal.date(byAdding: .year, value: 1, to: start) ?? start)
        }
    }

    /// A human label for the current period, e.g. "August 2026" or "1 – 7 Aug".
    var periodLabel: String {
        let cal = Calendar.current
        let f = DateFormatter()
        switch viewMode {
        case .agenda:
            return "Upcoming"
        case .week:
            guard let interval = cal.dateInterval(of: .weekOfYear, for: anchorDate) else { return "" }
            f.dateFormat = "d MMM"
            let end = cal.date(byAdding: .day, value: -1, to: interval.end) ?? interval.end
            return "\(f.string(from: interval.start)) – \(f.string(from: end))"
        case .month:
            f.dateFormat = "MMMM yyyy"
            return f.string(from: anchorDate)
        case .year:
            f.dateFormat = "yyyy"
            return f.string(from: anchorDate)
        }
    }

    private static func dedup(_ cals: [CalendarInfo]) -> [CalendarInfo] {
        var seen = Set<String>()
        // Dedup per (account, calendar): two accounts may each own a calendar
        // of the same name, and both must survive. Ordered by account, then
        // name, so the sidebar groups cleanly by account.
        return cals
            .filter { seen.insert($0.visibilityKey).inserted }
            .sorted { ($0.account, $0.name) < ($1.account, $1.name) }
    }
}
