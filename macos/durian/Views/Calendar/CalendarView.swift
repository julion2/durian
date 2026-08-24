//
//  CalendarView.swift
//  Durian
//
//  Top-level calendar container, shown in place of the mail view when
//  AppRouter.mode == .calendar. Shell: an optional sidebar (mini month +
//  calendar toggles), the active view (agenda/week/month/year) over the
//  echo/command line, and one floating card that switches between peek and
//  edit states.
//

import SwiftUI

struct CalendarView: View {
    @ObservedObject var manager = CalendarManager.shared
    @ObservedObject var appRouter = AppRouter.shared
    @ObservedObject var profileManager = ProfileManager.shared
    @State private var peekContentHeight: CGFloat = 240

    var body: some View {
        // A self-contained layout (NOT a NavigationSplitView) so the calendar's
        // column widths never bleed into the mail view's shared split state.
        //
        // There is no permanent detail column. It cost roughly a quarter of the
        // window to show a handful of facts, and it took that width from the
        // one thing the grid cannot do without — on a 1440pt window it pushed
        // the day columns down to ~123pt, where titles truncate after four
        // characters. The echo line under the grid carries the headline facts
        // instead; peek and edit use the same temporary floating surface.
        NavigationStack {
            HStack(spacing: 0) {
                if manager.sidebarVisible {
                    sidebar
                        .frame(width: 210)
                        .transition(.move(edge: .leading))
                    Divider()
                }

                VStack(spacing: 0) {
                    mainColumn
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                    Divider()
                    // One row, two states — status until `:` turns it into the
                    // command line, exactly as vim uses its last line.
                    if manager.commandLineActive {
                        CalendarCommandLine()
                    } else {
                        CalendarEchoLine()
                    }
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .animation(.easeInOut(duration: 0.2), value: manager.sidebarVisible)
            .animation(.easeInOut(duration: 0.2), value: manager.editingDraft?.id)
            .overlayPreferenceValue(CalendarEventAnchorPreferenceKey.self) { anchors in
                GeometryReader { geometry in
                    eventCard(anchors: anchors, geometry: geometry)
                }
            }
            .navigationTitle("Calendar")
            .navigationSubtitle(manager.periodLabel)
            .toolbar { toolbarContent }
        }
    }

    /// Peek and edit remain overlays, so neither squeezes the grid like the old
    /// sidebar. Peek is a small nonmodal popover beside its event; explicit
    /// editing gets the larger centered task surface.
    ///
    /// This remains an in-window overlay rather than NSPopover: a real popover
    /// takes key-window status and fights the global vim key monitor.
    @ViewBuilder
    private func eventCard(anchors: [EventID: Anchor<CGRect>],
                           geometry: GeometryProxy) -> some View
    {
        if let draft = manager.editingDraft {
            modalEditorCard {
                CalendarEventEditView(
                    draft: draft,
                    calendars: manager.calendars,
                    onSave: { manager.commitDraft($0) },
                    onCancel: { manager.cancelEdit() }
                )
                .id(draft.id)
            }
        } else if manager.detailExpanded,
                  let event = manager.detailEvent ?? manager.selectedEvent
        {
            compactPeek(event: event, anchor: anchors[event.id], geometry: geometry)
        }
    }

    private func modalEditorCard<Content: View>(@ViewBuilder content: () -> Content) -> some View {
        ZStack {
            Color.black.opacity(0.15)
                .ignoresSafeArea()

            content()
                .frame(width: 460, height: 520)
                .background {
                    Color.clear.glassEffect(
                        .regular.tint(Color(nsColor: .windowBackgroundColor).opacity(0.45)),
                        in: .rect(cornerRadius: 16)
                    )
                }
                .clipShape(.rect(cornerRadius: 16))
                .shadow(color: .black.opacity(0.35), radius: 32, y: 16)
        }
        .transition(.opacity)
    }

    /// The routine inspection surface: event-attached, content-sized, and with
    /// no scrim. The calendar remains visually present and pointer-accessible.
    private func compactPeek(
        event: CalendarEvent,
        anchor: Anchor<CGRect>?,
        geometry: GeometryProxy
    ) -> some View {
        let maxHeight = max(120, min(400, geometry.size.height - 24))
        let viewportHeight = min(peekContentHeight, maxHeight)
        let origin = peekOrigin(
            eventFrame: anchor.map { geometry[$0] },
            cardSize: CGSize(width: 352, height: viewportHeight),
            containerSize: geometry.size
        )

        return ZStack(alignment: .topLeading) {
            ScrollView(.vertical) {
                CalendarEventDetailView(event: event)
                    .frame(width: 352)
                    .background {
                        GeometryReader { content in
                            Color.clear.preference(
                                key: CalendarPeekContentHeightPreferenceKey.self,
                                value: content.size.height
                            )
                        }
                    }
            }
                // A new event gets a new scroll container, always starting at
                // its title instead of inheriting the previous event's offset.
                .id(event.id)
                .frame(width: 352, height: viewportHeight)
                .scrollBounceBehavior(.basedOnSize)
                .background(Color(nsColor: .windowBackgroundColor), in: .rect(cornerRadius: 14))
                .overlay {
                    RoundedRectangle(cornerRadius: 14)
                        .stroke(Color.primary.opacity(0.1), lineWidth: 0.5)
                }
                .clipShape(.rect(cornerRadius: 14))
                .shadow(color: .black.opacity(0.2), radius: 20, y: 6)
                .onTapGesture(count: 2) { manager.beginEdit() }
                .offset(x: origin.x, y: origin.y)
        }
        .onPreferenceChange(CalendarPeekContentHeightPreferenceKey.self) { height in
            if height > 0 { peekContentHeight = height }
        }
        .transition(.opacity.combined(with: .scale(scale: 0.98)))
    }

    private func peekOrigin(eventFrame: CGRect?, cardSize: CGSize,
                            containerSize: CGSize) -> CGPoint
    {
        let inset: CGFloat = 12
        let gap: CGFloat = 8
        let maxX = max(inset, containerSize.width - cardSize.width - inset)
        let maxY = max(inset, containerSize.height - cardSize.height - inset)
        guard let eventFrame else { return CGPoint(x: maxX, y: inset) }

        let x: CGFloat
        if eventFrame.maxX + gap + cardSize.width <= containerSize.width - inset {
            x = eventFrame.maxX + gap
        } else if eventFrame.minX - gap - cardSize.width >= inset {
            x = eventFrame.minX - gap - cardSize.width
        } else {
            x = min(max(eventFrame.midX - cardSize.width / 2, inset), maxX)
        }
        let y = min(max(eventFrame.minY, inset), maxY)
        return CGPoint(x: x, y: y)
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

    private func calendarRow(_ calendar: CalendarInfo) -> some View {
        CalendarSidebarRow(calendar: calendar,
                           visible: manager.isCalendarVisible(calendar),
                           toggle: { manager.toggleCalendar(calendar) })
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
        }
    }

    // MARK: - Helpers

}

private struct CalendarPeekContentHeightPreferenceKey: PreferenceKey {
    static let defaultValue: CGFloat = 0

    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        let next = nextValue()
        if next > 0 { value = next }
    }
}

// MARK: - Sidebar row

/// One calendar in the sidebar list. The whole row toggles the calendar's
/// visibility: a hidden calendar renders dimmed with a hollow swatch. The eye
/// appears on hover only — visibility is already carried by the swatch and the
/// dimming, so a permanent icon on every row would be a third encoding of the
/// same state. Its width is reserved either way, so the row never reflows.
private struct CalendarSidebarRow: View {
    let calendar: CalendarInfo
    let visible: Bool
    let toggle: () -> Void

    @State private var isHovered = false

    var body: some View {
        Button(action: toggle) {
            HStack(spacing: 8) {
                swatch.frame(width: 9, height: 9)
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
                    .opacity(isHovered ? 1 : 0)
                    .frame(width: 12)
            }
            // The app's row recipe: 12/8 inside the pill, 8 outside as the
            // gutter that makes the hover fill read as an inset row.
            .padding(.horizontal, 12).padding(.vertical, 8)
            .background(
                RoundedRectangle(cornerRadius: 8, style: .continuous)
                    .fill(isHovered ? Color.primary.opacity(0.06) : .clear)
            )
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .padding(.horizontal, 8)
        .onHover { isHovered = $0 }
        .help(visible ? "Hide \(calendar.name)" : "Show \(calendar.name)")
    }

    @ViewBuilder
    private var swatch: some View {
        if visible {
            Circle().fill(calendar.color)
        } else {
            Circle().strokeBorder(calendar.color.opacity(0.6), lineWidth: 1.5)
        }
    }
}
