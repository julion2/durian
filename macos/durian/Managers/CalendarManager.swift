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
import SwiftUI

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

    @Published var calendars: [CalendarInfo] = [] {
        didSet {
            rebuildColorIndex()
            // The parse depends on the calendar list (`-a` resolution), so a
            // list that changes mid-typing must re-resolve the preview.
            if commandLineActive { refreshCommandPreview() }
        }
    }

    /// Calendar color by (account, name) key, with a name-only fallback map —
    /// rebuilt when `calendars` changes so views resolve an event's color with
    /// a dictionary lookup instead of a linear scan per event per render (the
    /// week/month grids do that lookup for every block on every render).
    private var colorByCalendarKey: [String: Color] = [:]
    private var colorByCalendarName: [String: Color] = [:]

    private func rebuildColorIndex() {
        colorByCalendarKey = [:]
        colorByCalendarName = [:]
        for calendar in calendars {
            colorByCalendarKey[calendar.visibilityKey] = calendar.color
            // First one wins on a name collision, matching the old scan's
            // `first(where:)` behaviour for events that miss the exact key.
            if colorByCalendarName[calendar.name] == nil {
                colorByCalendarName[calendar.name] = calendar.color
            }
        }
    }

    /// The event's calendar color. Resolved per (account, name) — two accounts
    /// commonly both own a calendar called "Calendar", and a name-only match
    /// colored one account's events with the other's calendar color. The
    /// name-only fallback only catches events whose account has no exact match
    /// (e.g. a calendar list still loading).
    func color(for event: CalendarEvent) -> Color {
        colorByCalendarKey[CalendarInfo.key(account: event.account, name: event.calendar)]
            ?? colorByCalendarName[event.calendar]
            ?? .secondary
    }

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
            if selectedEventID != oldValue {
                loadDetail()
                if commandLineActive { refreshCommandPreview() }
            }
        }
    }

    /// The selected event with its full detail (attendees, organizer,
    /// description), fetched lazily — the list endpoint only returns summaries.
    @Published var detailEvent: CalendarEvent? {
        didSet {
            if commandLineActive { refreshCommandPreview() }
        }
    }
    @Published var isLoading = false

    @Published var viewMode: CalendarViewMode = .agenda
    @Published var anchorDate: Date = Date()
    @Published var searchQuery: String = ""

    /// The keyboard's position in the grid — a wall-clock instant snapped to
    /// the slot grid, distinct from the selected EVENT.
    ///
    /// A time grid needs two cursors. With only an event cursor there is no
    /// way to point at empty space, which is why creating an event had to
    /// invent a default time instead of using the one you were looking at.
    /// `:new` and `beginCreate()` both take this as their implicit argument,
    /// and Tab cycles the events that overlap it.
    @Published var cursorDate: Date = CalendarManager.snapToSlot(Date()) {
        didSet {
            cachedEventsAtCursor = nil
            // The cursor supplies the parse's implicit day/time, so a preview
            // shown while the cursor moves must follow it.
            if commandLineActive { refreshCommandPreview() }
        }
    }

    /// Whether the full-detail card is up. Detail is on demand, not always
    /// on: the echo line carries the headline facts continuously, and this is
    /// the one keystroke that opens everything else.
    @Published var detailExpanded = false

    /// The `:` line. Open, it replaces the echo line — the same place vim
    /// puts both.
    @Published var commandLineActive = false
    @Published var commandText = "" {
        didSet { refreshCommandPreview() }
    }

    /// What the current command text would do. Parsed ONCE per input change
    /// (keystroke / cursor move / calendar-list change) rather than on every
    /// read: the command line AND each of the week grid's seven day columns
    /// read it per render, and re-parsing per read multiplied the parser by
    /// the column count. The preview and the commit read the same stored
    /// parse, so they cannot disagree.
    @Published private(set) var commandPreview: CalendarCommand = .none

    private func refreshCommandPreview() {
        commandPreview = commandLineActive
            ? CalendarCommandParser.parse(commandText, cursor: cursorDate, calendars: calendars,
                                          selectedEvent: detailEvent ?? selectedEvent)
            : .none
    }

    func openCommandLine() {
        commandLineActive = true
        // Setting the text (re)parses via its didSet, now that the line is
        // active — order matters.
        commandText = ""
    }

    func closeCommandLine() {
        commandLineActive = false
        commandText = ""
    }

    /// Runs the typed command. An unparseable line stays open with its error
    /// showing rather than being thrown away — retyping a whole command
    /// because of one bad token is the thing that makes command lines feel
    /// hostile.
    func runCommand() {
        switch commandPreview {
        case .none, .invalid:
            return
        case .create(let start, let end, let title, let notes, let calendarKey):
            guard let target = resolveCalendar(calendarKey) else {
                BannerManager.shared.showWarning(
                    title: "No calendars available",
                    message: "Run 'durian calendar sync' first to sync your calendars."
                )
                closeCommandLine()
                return
            }
            var draft = CalendarEventDraft(account: target.account, calendar: target.name,
                                           start: start, end: end)
            draft.subject = title
            draft.description = notes ?? ""
            cursorDate = Self.snapToSlot(start)
            // `:new tomorrow …` / `:new friday …` can land outside the visible
            // period — page to it, otherwise the event is created off-screen
            // and appears to have gone nowhere.
            pageToCursor()
            commitDraft(draft)
        case .modifySelected(let patch):
            guard let event = detailEvent, event.id == selectedEventID else {
                BannerManager.shared.showWarning(
                    title: "Event details still loading",
                    message: "Try the command again in a moment."
                )
                return
            }
            if event.recurring && event.seriesStart == nil {
                BannerManager.shared.showWarning(
                    title: "Event details still loading",
                    message: "Try the command again in a moment."
                )
                return
            }
            var draft = CalendarEventDraft(from: event)
            draft.start = patch.start
            draft.end = patch.end
            if let title = patch.title { draft.subject = title }
            if let notes = patch.notes { draft.description = notes }
            let wasPeeking = detailExpanded
            commitDraft(draft, revealExisting: wasPeeking)
        case .goToday:
            cursorToNow()
        case .setView(let mode):
            setViewMode(mode)
        case .editSelected:
            beginEdit()
        case .deleteSelected:
            deleteSelected()
        }
        closeCommandLine()
    }

    /// The named calendar, or the busiest one — usually the primary, where the
    /// alphabetically-first is usually something like "Birthdays".
    private func resolveCalendar(_ key: String?) -> CalendarInfo? {
        if let key, let match = calendars.first(where: { $0.visibilityKey == key }) {
            return match
        }
        return calendars.max { $0.eventCount < $1.eventCount }
    }

    var selectedEvent: CalendarEvent? {
        guard let id = selectedEventID else { return nil }
        return store.byID[id]
    }

    /// Which accounts' calendars to show. The calendar is deliberately NOT
    /// scoped by the active mail profile — it queries every configured account
    /// so the calendar always appears regardless of which mail profile is
    /// selected. Accounts without a calendar vdir simply return nothing.
    ///
    /// The reserved `local` identifier is always included: it serves the
    /// local-only calendars from `calendar.local_calendars`, which belong to no
    /// account and would otherwise have no identifier to be fetched under. It
    /// costs nothing when none are configured — the endpoint answers with an
    /// empty list, exactly like an account that has no vdir yet.
    private var accountIdentifiers: [String] {
        ConfigManager.shared.getAccounts().map { $0.email } + [Self.localCalendarAccount]
    }

    /// Mirrors config.LocalCalendarAccount on the Go side.
    private static let localCalendarAccount = "local"

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
        // The cursor stack is derived from `events`; every path that changes
        // the projection comes through here.
        cachedEventsAtCursor = nil
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

    // nonisolated: constants read by the nonisolated load/save helpers below
    // (referencing a MainActor-isolated static there is an error in Swift 6).
    private nonisolated static let hiddenCalendarsKey = "durian.calendar.hiddenCalendars"
    private nonisolated static let hideDeclinedKey = "durian.calendar.hideDeclined"
    private nonisolated static let sidebarVisibleKey = "durian.calendar.sidebarVisible"

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

    /// j/k: the next/previous event in time order. The time cursor follows the
    /// landing event, so anything created afterwards still appears where you
    /// were just looking. Events that overlap sit next to each other in this
    /// order, so j/k walks a stack without needing a second cursor for it.
    func moveSelection(by delta: Int) {
        guard !events.isEmpty else { return }
        guard let current = events.firstIndex(where: { $0.id == selectedEventID }) else {
            // Nothing selected (e.g. right after a delete): j enters the list
            // at the top, k at the bottom — treating "no selection" as index 0
            // made the first j skip the first event.
            select(delta >= 0 ? events[0] : events[events.count - 1])
            return
        }
        let next = min(max(current + delta, 0), events.count - 1)
        select(events[next])
    }

    func selectFirst() { if let first = events.first { select(first) } }
    func selectLast() { if let last = events.last { select(last) } }

    /// Selects an event and keeps the time cursor on the same object. Mouse and
    /// keyboard selection share this path so the echo line and edit/delete
    /// actions cannot refer to different events.
    func select(_ event: CalendarEvent) {
        selectedEventID = event.id
        cursorDate = Self.snapToSlot(event.start)
        pageToCursor()
    }

    // MARK: - Time cursor

    nonisolated static let slotMinutes = 15

    /// Rounds an instant down onto the slot grid.
    nonisolated static func snapToSlot(_ date: Date) -> Date {
        let cal = Calendar.current
        var comps = cal.dateComponents([.year, .month, .day, .hour, .minute], from: date)
        comps.minute = ((comps.minute ?? 0) / slotMinutes) * slotMinutes
        return cal.date(from: comps) ?? date
    }

    /// h/l: a whole day, keeping the time of day. This is the motion that can
    /// land on empty space — which is exactly what makes the cursor usable as
    /// the implicit argument to `:new` and `n`.
    func moveCursor(days: Int) {
        cursorDate = Calendar.current.date(byAdding: .day, value: days, to: cursorDate) ?? cursorDate
        pageToCursor()
        syncSelectionToCursor()
    }

    /// Puts the cursor on the current time and brings it into view.
    func cursorToNow() {
        cursorDate = Self.snapToSlot(Date())
        anchorDate = cursorDate
        refresh()
        syncSelectionToCursor()
    }

    /// The period the current view actually shows, anchored on anchorDate.
    /// This must match what the view renders, NOT the fetch window: paging
    /// against the wrong period re-anchors (and refreshes) on cursor steps
    /// that are still on screen. The agenda in particular shows 30 days from
    /// the anchor day — paging it by the anchor's WEEK re-anchored on every
    /// single day step past the first week, rebuilding the whole list each
    /// time.
    private func visiblePeriod() -> DateInterval? {
        let cal = Calendar.current
        switch viewMode {
        case .agenda:
            let start = cal.startOfDay(for: anchorDate)
            guard let end = cal.date(byAdding: .day, value: 30, to: start) else { return nil }
            return DateInterval(start: start, end: end)
        case .week:
            return cal.dateInterval(of: .weekOfYear, for: anchorDate)
        case .month:
            return cal.dateInterval(of: .month, for: anchorDate)
        case .year:
            return cal.dateInterval(of: .year, for: anchorDate)
        }
    }

    /// Pages the visible period when the cursor walks off its edge — the
    /// cursor leads, the view follows. Half-open containment: an instant
    /// exactly on the period's end belongs to the NEXT period
    /// (DateInterval.contains would call it inside).
    private func pageToCursor() {
        guard let visible = visiblePeriod(),
              !(visible.start <= cursorDate && cursorDate < visible.end)
        else { return }
        anchorDate = cursorDate
        refresh()
    }

    /// Backing cache for `eventsAtCursor`, invalidated on cursor moves and
    /// projection changes. The echo line reads the stack several times per
    /// body and the selection sync reads it on every h/l step — filtering and
    /// sorting the full event list on each read did that work O(reads) times
    /// per keystroke.
    private var cachedEventsAtCursor: [CalendarEvent]?

    /// The events the cursor instant falls inside, earliest first. All-day
    /// events participate too: selecting one places the cursor at its start,
    /// and the echo line must continue to describe that selection.
    var eventsAtCursor: [CalendarEvent] {
        if let cached = cachedEventsAtCursor { return cached }
        let stack = events
            .filter { $0.start <= cursorDate && cursorDate < $0.end }
            .sorted { $0.start < $1.start }
        cachedEventsAtCursor = stack
        return stack
    }

    /// After a cursor-only move (h/l) the selection re-resolves onto whatever
    /// the cursor now sits on, or onto nothing over free time — so `i`, `dd`
    /// and `v` always act on the thing being looked at.
    private func syncSelectionToCursor() {
        let under = eventsAtCursor
        if let current = selectedEventID, under.contains(where: { $0.id == current }) { return }
        selectedEventID = under.first?.id
    }

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
            // the card shows the new response.
            refresh(force: true)
            loadDetail()
        }
    }

    // MARK: - Editing (local-first writes)

    /// The draft shown in the floating event card; nil while it is in peek mode
    /// or closed.
    @Published var editingDraft: CalendarEventDraft?

    /// Transforms the floating peek into an editor for the selected event (uses
    /// the full detail so the description/attendees are available).
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
        detailExpanded = true
        editingDraft = CalendarEventDraft(from: event)
    }

    /// Opens the floating editor for a new event, defaulting to `date` (or the
    /// time cursor) in the busiest calendar.
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
        // No explicit date means "where I am looking": the time cursor is the
        // implicit argument, so a new event lands in the slot under it rather
        // than at an invented default hour.
        let start = date ?? cursorDate
        let end = cal.date(byAdding: .hour, value: 1, to: start) ?? start
        detailExpanded = false
        editingDraft = CalendarEventDraft(account: calendar.account, calendar: calendar.name, start: start, end: end)
    }

    /// Escape/Cancel leaves an existing event in peek mode. Cancelling a new
    /// event closes the card because there is no event to peek yet.
    func cancelEdit() {
        let returnsToPeek = editingDraft?.isNew == false
        editingDraft = nil
        detailExpanded = returnsToPeek
    }

    /// Persists a draft (create or update) via the local-first write API and
    /// refreshes.
    func commitDraft(_ draft: CalendarEventDraft, revealExisting: Bool = true) {
        editingDraft = nil
        detailExpanded = !draft.isNew && revealExisting
        applyDraftOptimistically(draft)
        // The selected summary now carries the optimistic values. Drop any
        // pre-edit detail so the peek cannot flash stale title/time/location;
        // refetch the richer fields only AFTER the PUT, otherwise GET can race
        // the write and restore the old values into the peek.
        if !draft.isNew {
            detailTask?.cancel()
            detailEvent = nil
        }
        Task { [weak self] in
            guard let self else { return }
            guard await backend.putEvent(draft.toWrite()) != nil else {
                BannerManager.shared.showWarning(
                    title: "Couldn't save event",
                    message: "The write failed — make sure the durian CLI is up to date."
                )
                refresh(force: true)
                if !draft.isNew { loadDetail() }
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
            if !draft.isNew { loadDetail() }
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
        detailExpanded = false
        editingDraft = nil
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
