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
    private init() {}

    private let backend = CalendarBackend()
    private var loadTask: Task<Void, Never>?
    private var detailTask: Task<Void, Never>?

    @Published var calendars: [CalendarInfo] = []
    @Published var events: [CalendarEvent] = []
    @Published var selectedEventID: String? {
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
        return events.first { $0.id == id }
    }

    /// Which accounts' calendars to show. The calendar is deliberately NOT
    /// scoped by the active mail profile — it queries every configured account
    /// so the calendar always appears regardless of which mail profile is
    /// selected. Accounts without a calendar vdir simply return nothing.
    private var accountIdentifiers: [String] {
        ConfigManager.shared.getAccounts().map { $0.email }
    }

    // MARK: - Loading

    func refresh() {
        loadTask?.cancel()
        let accounts = accountIdentifiers
        let query = searchQuery.trimmingCharacters(in: .whitespacesAndNewlines)
        let (from, to) = window()

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
                    from: query.isEmpty ? from : nil,
                    to: query.isEmpty ? to : nil,
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

            loadedEvents.sort { $0.start < $1.start }
            calendars = Self.dedup(loadedCalendars)
            events = loadedEvents
            // Keep a valid selection.
            if let id = selectedEventID, !loadedEvents.contains(where: { $0.id == id }) {
                selectedEventID = loadedEvents.first?.id
            } else if selectedEventID == nil {
                selectedEventID = loadedEvents.first?.id
            }
        }
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

    /// RSVP to a meeting. TODO: wire to a write endpoint (POST /calendars/rsvp)
    /// that edits the owner's PARTSTAT in the local .ics and applies the sync
    /// (reusing the Stage-2 RSVP engine + safety rails). For now a no-op so the
    /// detail view can show the controls.
    func requestRSVP(_ event: CalendarEvent, response: String) {
        Log.info("CALENDAR", "RSVP '\(response)' requested for \(event.uid) — write path not yet wired")
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
        Task { [weak self] in
            guard let self else { return }
            if await backend.putEvent(draft.toWrite()) == nil {
                BannerManager.shared.showWarning(
                    title: "Couldn't save event",
                    message: "The write failed — make sure the durian CLI is up to date."
                )
            }
            refresh()
        }
    }

    /// Deletes the selected event locally and refreshes.
    func deleteSelected() {
        guard let event = detailEvent ?? selectedEvent, !event.account.isEmpty else { return }
        let account = event.account
        let uid = event.uid
        let calendar = event.calendar
        selectedEventID = nil
        Task { [weak self] in
            guard let self else { return }
            if await backend.deleteEvent(account: account, ref: uid, calendar: calendar) == false {
                BannerManager.shared.showWarning(
                    title: "Couldn't delete event",
                    message: "The delete failed — make sure the durian CLI is up to date."
                )
            }
            refresh()
        }
    }

    /// Fetches the full detail (attendees, organizer, description) of the
    /// selected event; the list only carries summaries. Falls back silently.
    private func loadDetail() {
        detailTask?.cancel()
        detailEvent = nil
        guard let id = selectedEventID,
              let summary = events.first(where: { $0.id == id })
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
        return cals.filter { seen.insert($0.name).inserted }.sorted { $0.name < $1.name }
    }
}
