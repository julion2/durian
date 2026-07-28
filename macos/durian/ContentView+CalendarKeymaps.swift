//
//  ContentView+CalendarKeymaps.swift
//  Durian
//
//  Vim-style bindings for the calendar. `gc` (from the mail list) opens the
//  calendar; inside it, j/k move the event cursor, gg/G jump, [ / ] step the
//  period, t goes to today, s toggles the sidebar, and q/Esc return to
//  mail. Registered once from
//  ContentView.onAppear alongside registerKeymapHandlers().
//

import SwiftUI

extension ContentView {
    func registerCalendarKeymapHandlers() {
        // Enter the calendar from the mail list.
        keymapHandler.registerSimpleHandler(for: .goCalendar, context: .list) { [self] in
            appRouter.showCalendar()
            keymapHandler.engine.setContext(.calendar)
        }

        // Event cursor navigation.
        keymapHandler.registerHandler(for: .nextEmail, context: .calendar) { count in
            CalendarManager.shared.moveSelection(by: max(1, count))
        }
        keymapHandler.registerHandler(for: .prevEmail, context: .calendar) { count in
            CalendarManager.shared.moveSelection(by: -max(1, count))
        }
        keymapHandler.registerSimpleHandler(for: .firstEmail, context: .calendar) {
            CalendarManager.shared.selectFirst()
        }
        keymapHandler.registerSimpleHandler(for: .lastEmail, context: .calendar) {
            CalendarManager.shared.selectLast()
        }

        // Period navigation.
        keymapHandler.registerSimpleHandler(for: .calendarPrevPeriod, context: .calendar) {
            CalendarManager.shared.step(-1)
        }
        keymapHandler.registerSimpleHandler(for: .calendarNextPeriod, context: .calendar) {
            CalendarManager.shared.step(1)
        }
        keymapHandler.registerSimpleHandler(for: .calendarToday, context: .calendar) {
            CalendarManager.shared.goToToday()
        }
        keymapHandler.registerSimpleHandler(for: .calendarToggleSidebar, context: .calendar) {
            CalendarManager.shared.toggleSidebar()
        }

        // Create / edit / delete (local-first writes).
        keymapHandler.registerSimpleHandler(for: .calendarNew, context: .calendar) {
            CalendarManager.shared.beginCreate()
        }
        keymapHandler.registerSimpleHandler(for: .calendarEdit, context: .calendar) {
            CalendarManager.shared.beginEdit()
        }
        keymapHandler.registerSimpleHandler(for: .calendarDelete, context: .calendar) {
            CalendarManager.shared.deleteSelected()
        }

        // Leave the calendar.
        keymapHandler.registerSimpleHandler(for: .closeDetail, context: .calendar) { [self] in
            appRouter.showMail()
            keymapHandler.engine.setContext(.list)
        }
    }
}
