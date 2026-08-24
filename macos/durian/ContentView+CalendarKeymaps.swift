//
//  ContentView+CalendarKeymaps.swift
//  Durian
//
//  Vim-style bindings for the calendar. `gc` (from the mail list) opens it.
//
//    j/k    previous / next event        count-aware
//    gg/G   first / last event
//    h/l    ± a day                       count-aware
//    H/L    ± a period (week/month/year), same as [ / ]
//    t      now
//    v      full detail        i  edit        n/o/O  new        dd  delete
//    s      sidebar            D  hide declined                  q  back to mail
//
//  There is a time cursor underneath all of this, but it is not driven
//  directly: j/k put it on the event they land on, h/l walk it a day at a
//  time. It exists so that "create" has something to aim at — n and `:new`
//  fill the slot under the cursor instead of an invented default hour.
//

import SwiftUI

extension ContentView {
    func registerCalendarKeymapHandlers() {
        // Enter the calendar from the mail list.
        keymapHandler.registerSimpleHandler(for: .goCalendar, context: .list) { [self] in
            appRouter.showCalendar()
            keymapHandler.engine.setContext(.calendar)
        }

        // j/k step from event to event; the time cursor follows the landing
        // event so a create afterwards still lands where you are looking.
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

        // h/l move the cursor a day — the only motion that can land on empty
        // space, which is what `n` and `:new` need to aim at.
        keymapHandler.registerHandler(for: .calendarNextDay, context: .calendar) { count in
            CalendarManager.shared.moveCursor(days: max(1, count))
        }
        keymapHandler.registerHandler(for: .calendarPrevDay, context: .calendar) { count in
            CalendarManager.shared.moveCursor(days: -max(1, count))
        }

        // Period navigation.
        keymapHandler.registerSimpleHandler(for: .calendarPrevPeriod, context: .calendar) {
            CalendarManager.shared.step(-1)
        }
        keymapHandler.registerSimpleHandler(for: .calendarNextPeriod, context: .calendar) {
            CalendarManager.shared.step(1)
        }
        keymapHandler.registerSimpleHandler(for: .calendarToday, context: .calendar) {
            CalendarManager.shared.cursorToNow()
        }
        keymapHandler.registerSimpleHandler(for: .calendarToggleSidebar, context: .calendar) {
            CalendarManager.shared.toggleSidebar()
        }
        keymapHandler.registerSimpleHandler(for: .calendarToggleDeclined, context: .calendar) {
            CalendarManager.shared.hideDeclined.toggle()
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

        // The command line.
        keymapHandler.registerSimpleHandler(for: .calendarCommand, context: .calendar) {
            CalendarManager.shared.openCommandLine()
        }

        // Full detail on demand.
        keymapHandler.registerSimpleHandler(for: .calendarDetail, context: .calendar) {
            guard CalendarManager.shared.selectedEventID != nil else { return }
            CalendarManager.shared.detailExpanded.toggle()
        }

        // Leave: cancel edit back to peek, then close peek, then leave calendar.
        keymapHandler.registerSimpleHandler(for: .closeDetail, context: .calendar) { [self] in
            if CalendarManager.shared.editingDraft != nil {
                CalendarManager.shared.cancelEdit()
                return
            }
            if CalendarManager.shared.detailExpanded {
                CalendarManager.shared.detailExpanded = false
                return
            }
            appRouter.showMail()
            keymapHandler.engine.setContext(.list)
        }
    }
}
