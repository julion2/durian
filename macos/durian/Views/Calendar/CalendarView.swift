//
//  CalendarView.swift
//  Durian
//
//  Top-level calendar container, shown in place of the mail view when
//  AppRouter.mode == .calendar. Three-column shell: sidebar = calendars,
//  content = the active view (agenda in v1; week/month/year are scaffolded),
//  detail = the selected event. Read-only.
//

import SwiftUI

struct CalendarView: View {
    @ObservedObject var manager = CalendarManager.shared
    @ObservedObject var appRouter = AppRouter.shared
    @ObservedObject var profileManager = ProfileManager.shared

    var body: some View {
        // A self-contained layout (NOT a NavigationSplitView) so the calendar's
        // column widths never bleed into the mail view's shared split state.
        NavigationStack {
            GeometryReader { geo in
                let sidebarWidth: CGFloat = 210
                // Collapsed: content + detail take the full width.
                let rest = max(geo.size.width - (manager.sidebarVisible ? sidebarWidth : 0), 200)
                // Agenda ≈ half/half; the grid views give the content ≈3/4.
                let contentFraction: CGFloat = manager.viewMode == .agenda ? 0.55 : 0.74
                HStack(spacing: 0) {
                    if manager.sidebarVisible {
                        sidebar
                            .frame(width: sidebarWidth)
                            .transition(.move(edge: .leading))
                        Divider()
                    }
                    mainColumn.frame(width: rest * contentFraction)
                    Divider()
                    detailPane.frame(maxWidth: .infinity)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
                .animation(.easeInOut(duration: 0.2), value: manager.sidebarVisible)
            }
            .navigationTitle("Calendar")
            .navigationSubtitle(manager.periodLabel)
            .toolbar { toolbarContent }
        }
    }

    private var detailPane: some View {
        Group {
            if let draft = manager.editingDraft {
                CalendarEventEditView(
                    draft: draft,
                    calendars: manager.calendars,
                    onSave: { manager.commitDraft($0) },
                    onCancel: { manager.editingDraft = nil }
                )
                .id(draft.id)
            } else if let event = manager.detailEvent ?? manager.selectedEvent {
                CalendarEventDetailView(event: event)
                    .id(manager.selectedEventID)
            } else {
                placeholder("Select an event", systemImage: "calendar")
            }
        }
    }

    // MARK: - Sidebar (mini month + calendars)

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 0) {
            MiniMonthView()
                .padding(.horizontal, 12)
                .padding(.top, 12)

            Divider()
                .padding(.horizontal, 12)
                .padding(.vertical, 10)

            Text("Calendars")
                .font(.caption).fontWeight(.semibold).foregroundStyle(.secondary)
                .padding(.horizontal, 16).padding(.bottom, 6)

            if manager.calendars.isEmpty {
                Text("No calendars synced yet.")
                    .font(.callout).foregroundStyle(.secondary)
                    .padding(.horizontal, 16)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 2) {
                        ForEach(calendarsByAccount, id: \.account) { group in
                            // Only label the group when more than one account is
                            // present — a single account needs no header.
                            if calendarsByAccount.count > 1 {
                                Text(group.account)
                                    .font(.system(size: 11)).fontWeight(.semibold)
                                    .foregroundStyle(Color.Detail.textTertiary)
                                    .lineLimit(1).truncationMode(.middle)
                                    .padding(.horizontal, 20).padding(.top, 8).padding(.bottom, 2)
                            }
                            ForEach(group.calendars) { calendar in
                                calendarRow(calendar)
                            }
                        }
                    }
                }
            }
            Spacer()
        }
    }

    /// The sidebar calendars grouped by account, preserving the manager's
    /// (account, name) ordering so groups render contiguously and sorted.
    private var calendarsByAccount: [(account: String, calendars: [CalendarInfo])] {
        var order: [String] = []
        var groups: [String: [CalendarInfo]] = [:]
        for calendar in manager.calendars {
            if groups[calendar.account] == nil { order.append(calendar.account) }
            groups[calendar.account, default: []].append(calendar)
        }
        return order.map { (account: $0, calendars: groups[$0] ?? []) }
    }

    /// One calendar in the sidebar list. The whole row toggles the
    /// calendar's visibility; a hidden calendar renders dimmed with a
    /// hollow swatch and a slashed eye.
    private func calendarRow(_ calendar: CalendarInfo) -> some View {
        let visible = manager.isCalendarVisible(calendar)
        return Button {
            manager.toggleCalendar(calendar)
        } label: {
            HStack(spacing: 8) {
                Group {
                    if visible {
                        Circle().fill(calendar.color)
                    } else {
                        Circle().strokeBorder(calendar.color.opacity(0.6), lineWidth: 1.5)
                    }
                }
                .frame(width: 9, height: 9)
                Text(calendar.name)
                    .font(.system(size: 13)).lineLimit(1)
                    .foregroundStyle(visible ? Color.Detail.textPrimary : Color.Detail.textTertiary)
                Spacer()
                Text("\(calendar.eventCount)")
                    .font(.caption)
                    .foregroundStyle(visible ? Color.Detail.textSecondary : Color.Detail.textTertiary)
                Image(systemName: visible ? "eye" : "eye.slash")
                    .font(.system(size: 10))
                    .foregroundStyle(Color.Detail.textTertiary)
            }
            .padding(.horizontal, 12).padding(.vertical, 6)
            .padding(.horizontal, 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help(visible ? "Hide \(calendar.name)" : "Show \(calendar.name)")
    }

    // MARK: - Content column

    @ViewBuilder
    private var mainColumn: some View {
        switch manager.viewMode {
        case .agenda:
            CalendarAgendaView()
        case .week:
            CalendarWeekView()
        case .month:
            CalendarMonthView()
        case .year:
            CalendarYearView()
        }
    }

    // MARK: - Toolbar

    @ToolbarContentBuilder
    private var toolbarContent: some ToolbarContent {
        ToolbarItem(placement: .navigation) {
            Button { manager.toggleSidebar() } label: { Image(systemName: "sidebar.leading") }
                .help("Toggle calendar sidebar (s)")
        }
        ToolbarItem(placement: .principal) {
            Picker("View", selection: Binding(
                get: { manager.viewMode },
                set: { manager.setViewMode($0) }
            )) {
                ForEach(CalendarViewMode.allCases) { mode in
                    Text(mode.title).tag(mode)
                }
            }
            .pickerStyle(.segmented)
            .fixedSize()
        }
        ToolbarItemGroup(placement: .automatic) {
            Button { manager.step(-1) } label: { Image(systemName: "chevron.left") }
                .help("Previous period")
            Button { manager.goToToday() } label: { Text("Today") }
            Button { manager.step(1) } label: { Image(systemName: "chevron.right") }
                .help("Next period")
            if manager.isLoading {
                ProgressView().controlSize(.small)
            }
            Button { manager.beginCreate() } label: { Image(systemName: "plus") }
                .help("New event (n)")
            if manager.selectedEventID != nil {
                Button { manager.beginEdit() } label: { Image(systemName: "pencil") }
                    .help("Edit event (i)")
                Button { manager.deleteSelected() } label: { Image(systemName: "trash") }
                    .help("Delete event (dd)")
            }
        }
    }

    // MARK: - Helpers

    private func placeholder(_ text: String, systemImage: String) -> some View {
        VStack(spacing: 10) {
            Image(systemName: systemImage).font(.system(size: 34)).foregroundStyle(.tertiary)
            Text(text).font(.callout).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}
