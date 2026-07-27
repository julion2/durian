//
//  AppRouter.swift
//  Durian
//
//  The one piece of top-level navigation state: which mode the main window
//  shows (mail vs calendar). The app is otherwise single-purpose (email), so
//  the calendar is reached via the Calendar menu / a shortcut / vim `gc`,
//  which flip this mode — mirroring how the Profiles menu switches profiles.
//

import Combine

enum AppMode {
    case mail
    case calendar
}

@MainActor
final class AppRouter: ObservableObject {
    static let shared = AppRouter()
    private init() {}

    @Published var mode: AppMode = .mail

    func showCalendar() {
        mode = .calendar
        // Entering the calendar is the natural moment to pick up background
        // sync changes, so bypass the coverage skip.
        CalendarManager.shared.refresh(force: true)
    }

    func showMail() {
        mode = .mail
    }

    func toggle() {
        mode == .calendar ? showMail() : showCalendar()
    }
}
