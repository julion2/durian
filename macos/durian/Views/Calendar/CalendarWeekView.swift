//
//  CalendarWeekView.swift
//  Durian
//
//  A time-grid week view: 7 day columns over an hourly axis, timed events
//  positioned by start/duration, all-day events in a header row. Overlapping
//  events are laid out side-by-side in lanes; when more events run
//  concurrently than fit the column width, the surplus collapses into a
//  "+X" block. Tapping an event selects it.
//
//  The lane layout lives in DayLayoutIndex (overlap-aware, so multi-day and
//  midnight-spanning events render clamped blocks on every day they touch)
//  and is memoized by WeekLayoutCache. All pixel<->time conversions go
//  through one TimeGeometry instance. A move drag targets the ABSOLUTE
//  pointer position in the grid's named coordinate space, minus the grab
//  offset recorded at drag start — so where a block lands depends only on
//  where the block visually is, never on where inside it it was grabbed or
//  on the original start's sub-slot offset. Escape cancels an in-progress
//  drag/resize without committing.
//

import AppKit
import SwiftUI

struct CalendarWeekView: View {
    @ObservedObject var manager = CalendarManager.shared
    private let calendar = Calendar.current

    private let hourHeight: CGFloat = 44
    private let timeColumnWidth: CGFloat = 48
    private let startHour = 0
    private let endHour = 24

    /// The named space the move gesture reads absolute positions in: the
    /// grid HStack including the leading time axis, so x values line up with
    /// TimeGeometry.x(forDayIndex:).
    private static let gridSpace = "weekgrid"

    /// Memoized per-day lane layouts — see WeekLayoutCache for the
    /// invalidation signature.
    @State private var layoutCache = WeekLayoutCache()

    // MARK: - Drag state (move / resize)

    private enum DragKind { case move, resizeStart, resizeEnd }

    /// The in-progress drag. A move records the grab offset (pointer to
    /// block origin, fixed for the whole gesture) and routes its per-frame
    /// snapped target through dragPreview, so only the floating preview
    /// re-renders while the pointer moves. A resize keeps its raw
    /// translation here (the block grows/shifts in place; the grid body
    /// re-evaluates but the layout cache hits). Committed times come from
    /// the same shared functions the previews use (TimeGeometry.moveTarget /
    /// resizedMinutes), so preview and drop cannot disagree.
    private struct DragSession {
        let eventID: EventID
        let kind: DragKind
        var grabOffset: CGSize = .zero
        var translation: CGSize = .zero
    }

    @State private var drag: DragSession?

    /// True from an Escape cancel until the still-held pointer is released:
    /// the remaining gesture events of the cancelled sequence must do
    /// nothing (no session reopen, no commit on release).
    @State private var dragCancelled = false

    /// The local Escape key monitor, alive only while a drag session runs.
    /// Installed when a session opens, removed in endDragSession — the one
    /// teardown path drop, cancel and view disappearance all use.
    @State private var escapeMonitor: Any?

    /// Carries the move drag's snapped landing cell outside of view state:
    /// only MovePreview subscribes, so publishing it while the pointer moves
    /// does not recompute the week grid (which would re-run the lane layout
    /// for all seven columns per frame).
    private final class DragPreviewModel: ObservableObject {
        @Published var target: TimeGeometry.MoveTarget?
    }

    @State private var dragPreview = DragPreviewModel()

    /// A time range being drawn on empty grid space before the editor opens.
    private struct CreateSelection {
        var start: Date
        var end: Date
    }

    @State private var createSelection: CreateSelection?

    var body: some View {
        // The GeometryReader supplies the day-column width the lane layout
        // needs (the HStack splits the remainder after the time axis evenly
        // across the 7 flexible columns).
        GeometryReader { geo in
            let colWidth = max((geo.size.width - timeColumnWidth) / 7, 1)
            let geometry = TimeGeometry(hourHeight: hourHeight, dayWidth: colWidth,
                                        timeColumnWidth: timeColumnWidth,
                                        startHour: startHour, endHour: endHour)
            // The week's days, resolved ONCE per body evaluation and passed
            // down — the header rows, the layout and the column loop all need
            // them, and the body re-evaluates on every resize-drag frame.
            let days = weekDays
            // Computed ONCE per body for all 7 columns (and the move
            // preview) — a cache hit when neither events nor lane cap
            // changed, e.g. on every resize-drag step.
            let layouts = layoutCache.layouts(days: days, events: manager.events,
                                              maxVisibleLanes: maxVisibleLanes(colWidth),
                                              calendar: calendar)
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(spacing: 0, pinnedViews: [.sectionHeaders]) {
                        Section {
                            HStack(alignment: .top, spacing: 0) {
                                timeAxis
                                ForEach(Array(days.enumerated()), id: \.element) { index, day in
                                    dayColumn(layouts[day] ?? DayLayoutIndex(), day: day,
                                              dayIndex: index, geometry: geometry)
                                        .frame(maxWidth: .infinity)
                                }
                            }
                            // The space the move gesture and the floating
                            // copy share: absolute pointer x/y here map
                            // straight through TimeGeometry.
                            .coordinateSpace(.named(Self.gridSpace))
                            // The move drag renders as one floating copy up
                            // here, above all columns, instead of offsetting
                            // the block inside its column: crossing column
                            // boundaries there fought the sibling z-order and
                            // moved the gesture's own host view every frame,
                            // which is what made the move drag flicker.
                            .overlay(alignment: .topLeading) {
                                movePreview(geometry: geometry)
                            }
                        } header: {
                            VStack(spacing: 0) {
                                dayHeaderRow(days)
                                allDayRow(days)
                                Divider()
                            }
                            .background(Color(nsColor: .windowBackgroundColor))
                        }
                    }
                }
                .onChange(of: manager.selectedEventID) { _, _ in
                    scrollToSelectedHour(proxy)
                }
                // A drag session must not outlive the grid: tear it down (and
                // its Escape monitor) if the view goes away mid-drag.
                .onDisappear {
                    endDragSession()
                    createSelection = nil
                }
            }
        }
    }

    /// Scrolls the time grid so the selected timed event's hour is visible —
    /// used when j/k moves the cursor to an off-screen event.
    private func scrollToSelectedHour(_ proxy: ScrollViewProxy) {
        guard let id = manager.selectedEventID,
              let event = manager.events.first(where: { $0.id == id }),
              !event.allDay
        else { return }
        let hour = min(max(calendar.component(.hour, from: event.start), startHour), endHour - 1)
        withAnimation(.easeInOut(duration: 0.2)) {
            proxy.scrollTo("hour-\(hour)", anchor: .center)
        }
    }

    // MARK: - Header (pinned inside the scroll view so its columns share the
    // grid's width context and stay aligned).

    private func dayHeaderRow(_ days: [Date]) -> some View {
        HStack(spacing: 0) {
            Color.clear.frame(width: timeColumnWidth)
            ForEach(days, id: \.self) { day in
                VStack(spacing: 1) {
                    Text(Self.weekdayFormatter.string(from: day))
                        .font(.caption2).foregroundStyle(.secondary)
                    DayNumberBadge(date: day)
                }
                .frame(maxWidth: .infinity)
            }
        }
        .padding(.vertical, 4)
    }

    private func allDayRow(_ days: [Date]) -> some View {
        HStack(alignment: .top, spacing: 0) {
            Text("All-day")
                .font(.system(size: 10))
                .foregroundStyle(Color.Detail.textTertiary)
                .padding(.trailing, 8)
                .frame(width: timeColumnWidth, alignment: .trailing)
            ForEach(days, id: \.self) { day in
                VStack(spacing: 2) {
                    ForEach(allDayEvents(day)) { event in
                        eventChip(event)
                    }
                }
                .frame(maxWidth: .infinity)
                .padding(.horizontal, 1)
            }
        }
        .frame(minHeight: 18)
        .padding(.vertical, 2)
    }

    // MARK: - Time grid

    private var timeAxis: some View {
        VStack(alignment: .trailing, spacing: 0) {
            ForEach(startHour ..< endHour, id: \.self) { hour in
                Text(CalendarTimeFormat.axisHour(hour))
                    .font(.system(size: 10))
                    .foregroundStyle(Color.Detail.textTertiary)
                    .frame(height: hourHeight, alignment: .top)
                    .id("hour-\(hour)")
            }
        }
        .padding(.trailing, 8)
        .frame(width: timeColumnWidth, alignment: .trailing)
    }

    private func dayColumn(_ layout: DayLayoutIndex, day: Date, dayIndex: Int,
                           geometry: TimeGeometry) -> some View
    {
        func laneWidth(_ laneCount: Int) -> CGFloat {
            geometry.dayWidth / CGFloat(max(laneCount, 1))
        }
        return ZStack(alignment: .topLeading) {
            emptyGridInteraction(day: day, dayIndex: dayIndex, geometry: geometry)

            ForEach(startHour ... endHour, id: \.self) { hour in
                // The grid recedes so the blocks can carry the contrast: the
                // hour lines are a hint at the time axis, not a table rule.
                Rectangle().fill(Color.Detail.border.opacity(0.45))
                    .frame(height: 0.5)
                    .frame(maxWidth: .infinity, alignment: .top)
                    .offset(y: geometry.y(forMinutes: hour * 60))
                    .allowsHitTesting(false)
            }
            createSelectionBlock(on: day, geometry: geometry)
            ForEach(layout.visible) { block in
                // The block never moves during a drag: a move shows the
                // floating copy in movePreview while the original stays here
                // dimmed (keeping the gesture's host view stationary), and a
                // resize only shifts an edge in place.
                timedBlock(block, dayIndex: dayIndex, geometry: geometry)
                    .frame(width: laneWidth(block.laneCount))
                    .offset(x: CGFloat(block.lane) * laneWidth(block.laneCount),
                            y: blockY(block, geometry: geometry))
                    // Keep the block being resized on top of its neighbours.
                    .zIndex(drag?.eventID == block.event.id ? 1 : 0)
            }
            ForEach(layout.overflow) { item in
                overflowBlock(item, geometry: geometry)
                    .frame(width: laneWidth(item.laneCount))
                    .offset(x: CGFloat(item.lane) * laneWidth(item.laneCount),
                            y: geometry.y(forMinutes: item.startMinute))
            }
            if calendar.isDateInToday(day) {
                nowIndicator(geometry: geometry)
            }
            commandPreviewBlock(on: day, geometry: geometry)
        }
        .frame(height: geometry.totalHeight, alignment: .top)
        .overlay(alignment: .trailing) {
            Rectangle().fill(Color.Detail.border.opacity(0.45)).frame(width: 0.5)
                .allowsHitTesting(false)
        }
        .contentShape(Rectangle())
    }

    /// Owns pointer input only where no event block is above it. A mouse drag
    /// paints a snapped range within one day or across neighbouring columns;
    /// release opens the normal editor with those exact times. Trackpad/wheel
    /// scrolling remains owned by ScrollView.
    private func emptyGridInteraction(day: Date, dayIndex: Int,
                                      geometry: TimeGeometry) -> some View
    {
        Color.clear
            .frame(maxWidth: .infinity)
            .frame(height: geometry.totalHeight)
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 6)
                    .onChanged { value in
                        guard drag == nil, !manager.commandLineActive else { return }
                        createSelection = createInterval(
                            from: value.startLocation,
                            to: value.location,
                            on: day,
                            dayIndex: dayIndex,
                            geometry: geometry
                        )
                    }
                    .onEnded { value in
                        guard drag == nil, !manager.commandLineActive else {
                            createSelection = nil
                            return
                        }
                        let selection = createInterval(
                            from: value.startLocation,
                            to: value.location,
                            on: day,
                            dayIndex: dayIndex,
                            geometry: geometry
                        )
                        createSelection = nil
                        if let selection {
                            manager.beginCreate(at: selection.start, endingAt: selection.end)
                        }
                    }
            )
            // Preserve the existing fast path: a double-click creates a
            // one-hour event at the clicked hour.
            .simultaneousGesture(SpatialTapGesture(count: 2).onEnded { value in
                let hour = min(max(Int(value.location.y / hourHeight) + startHour, startHour), endHour - 1)
                if let date = dateAt(day, hour: hour) {
                    manager.beginCreate(at: date)
                }
            })
    }

    /// Resolves the absolute interval selected from one column into any other
    /// visible day. The mouse-down time stays fixed; the current pointer's day
    /// and time form the other endpoint, and dragging backwards simply swaps
    /// the resulting dates.
    private func createInterval(from startLocation: CGPoint, to location: CGPoint,
                                on day: Date, dayIndex: Int,
                                geometry: TimeGeometry) -> CreateSelection?
    {
        var startMinute = geometry.snappedMinute(atY: startLocation.y)
        // Starting exactly on the grid's lower edge means the final slot, not
        // midnight of the following day.
        if startMinute == endHour * 60 {
            startMinute -= geometry.snapMinutes
        }
        let targetDayIndex = geometry.dayIndex(
            atLocalX: location.x,
            relativeTo: dayIndex,
            dayCount: weekDays.count
        )
        var targetMinute = geometry.snappedMinute(atY: location.y)
        // The lower edge of an earlier column means that day's final slot,
        // not midnight of the day after it. Forward drags keep midnight so a
        // full target day can still be selected.
        if targetDayIndex < dayIndex, targetMinute == endHour * 60 {
            targetMinute -= geometry.snapMinutes
        }
        guard targetDayIndex < weekDays.count,
              let anchor = date(on: day, minutes: startMinute),
              let target = date(on: weekDays[targetDayIndex],
                                minutes: targetMinute)
        else { return nil }

        if anchor == target {
            guard let end = calendar.date(byAdding: .minute, value: geometry.snapMinutes, to: anchor) else { return nil }
            return CreateSelection(start: anchor, end: end)
        }
        return CreateSelection(start: min(anchor, target), end: max(anchor, target))
    }

    @ViewBuilder
    private func createSelectionBlock(on day: Date, geometry: TimeGeometry) -> some View {
        if let selection = createSelection,
           let range = createSelectionRange(selection, on: day)
        {
            let height = geometry.height(forMinutes: range.endMinute - range.startMinute)
            let shape = RoundedRectangle(cornerRadius: 8)
            shape
                .fill(Color.Detail.cursor.opacity(0.16))
                .overlay {
                    shape.strokeBorder(Color.Detail.cursor.opacity(0.85), lineWidth: 1.5)
                }
                .overlay(alignment: .topLeading) {
                    if height >= 24 {
                        Text(createSelectionLabel(selection, on: day))
                            .font(.system(size: 10, weight: .semibold))
                            .foregroundStyle(Color.Detail.cursor)
                            .padding(.horizontal, 6)
                            .padding(.vertical, 3)
                    }
                }
                .frame(maxWidth: .infinity)
                .frame(height: max(height, 4))
                .padding(.horizontal, 1)
                .offset(y: geometry.y(forMinutes: range.startMinute))
                .allowsHitTesting(false)
                .zIndex(3)
        }
    }

    /// The slice of an absolute selection visible in one day column.
    private func createSelectionRange(_ selection: CreateSelection,
                                      on day: Date) -> TimeGeometry.SelectionRange?
    {
        let dayStart = calendar.startOfDay(for: day)
        guard let dayEnd = calendar.date(byAdding: .day, value: 1, to: dayStart),
              selection.start < dayEnd, selection.end > dayStart else { return nil }
        let startMinute = selection.start <= dayStart ? 0 : minutesFromMidnight(selection.start)
        let endMinute = selection.end >= dayEnd ? 24 * 60 : minutesFromMidnight(selection.end)
        return TimeGeometry.SelectionRange(startMinute: startMinute, endMinute: endMinute)
    }

    private func createSelectionLabel(_ selection: CreateSelection, on day: Date) -> String {
        let dayStart = calendar.startOfDay(for: day)
        let dayEnd = calendar.date(byAdding: .day, value: 1, to: dayStart) ?? dayStart
        let startsHere = calendar.isDate(selection.start, inSameDayAs: day)
        // Midnight belongs to the following calendar date, but visually it is
        // also the lower edge of this day's rendered selection slice.
        let endsHere = selection.end > dayStart && selection.end <= dayEnd
        switch (startsHere, endsHere) {
        case (true, true):
            return "\(CalendarTimeFormat.time(selection.start))–\(CalendarTimeFormat.time(selection.end))"
        case (true, false):
            return "\(CalendarTimeFormat.time(selection.start)) →"
        case (false, true):
            return "→ \(CalendarTimeFormat.time(selection.end))"
        case (false, false):
            return "Continues"
        }
    }

    private func dateAt(_ day: Date, hour: Int) -> Date? {
        var comps = calendar.dateComponents([.year, .month, .day], from: day)
        comps.hour = hour
        comps.minute = 0
        return calendar.date(from: comps)
    }

    /// A thin red "now" line across today's column at the current time, with a
    /// leading dot. TimelineView ticks it every minute; it never intercepts
    /// taps so the empty-slot create gesture underneath still fires.
    @ViewBuilder
    private func nowIndicator(geometry: TimeGeometry) -> some View {
        TimelineView(.everyMinute) { context in
            let minutes = minutesNow(context.date)
            if minutes >= startHour * 60 && minutes <= endHour * 60 {
                ZStack(alignment: .leading) {
                    Rectangle().fill(Color.red).frame(height: 1.5)
                    Circle().fill(Color.red).frame(width: 6, height: 6)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .offset(y: geometry.y(forMinutes: minutes) - 3)
                .allowsHitTesting(false)
                .zIndex(2)
            }
        }
    }

    /// The event `:new …` would create, drawn while it is still being typed.
    ///
    /// It reads the SAME parse the Return key commits, so what the grid shows
    /// and what gets saved cannot drift. Dashed, because it does not exist
    /// yet — a solid block would claim a commitment that has not happened.
    @ViewBuilder
    private func commandPreviewBlock(on day: Date, geometry: TimeGeometry) -> some View {
        if manager.commandLineActive,
           case .create(let start, let end, let title, _, _) = manager.commandPreview,
           calendar.isDate(start, inSameDayAs: day)
        {
            let startMinutes = minutesFromMidnight(start)
            let minutes = max(Int(end.timeIntervalSince(start) / 60), 15)
            let shape = RoundedRectangle(cornerRadius: 8)
            Text(title)
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(Color.Detail.cursor)
                .lineLimit(2)
                .padding(.horizontal, 6).padding(.vertical, 3)
                .frame(maxWidth: .infinity, alignment: .topLeading)
                .frame(height: max(geometry.height(forMinutes: minutes), 16), alignment: .top)
                .background(shape.fill(Color.Detail.cursor.opacity(0.10)))
                .overlay(
                    shape.strokeBorder(Color.Detail.cursor.opacity(0.8),
                                       style: StrokeStyle(lineWidth: 1.5, dash: [4, 3]))
                )
                .offset(y: geometry.y(forMinutes: startMinutes))
                .allowsHitTesting(false)
                .zIndex(3)
        }
    }

    private func minutesNow(_ date: Date) -> Int {
        let comps = calendar.dateComponents([.hour, .minute], from: date)
        return (comps.hour ?? 0) * 60 + (comps.minute ?? 0)
    }

    /// The block's y offset: the committed start, except while its top edge
    /// is being dragged — then the live snapped start from the SAME function
    /// the drop commits through.
    private func blockY(_ block: DayLayoutIndex.Block, geometry: TimeGeometry) -> CGFloat {
        if let live = activeResize(block.event, geometry: geometry) {
            return geometry.y(forMinutes: live.start)
        }
        return geometry.y(forMinutes: block.startMinute)
    }

    private func timedBlock(_ block: DayLayoutIndex.Block, dayIndex: Int,
                            geometry: TimeGeometry) -> some View
    {
        let event = block.event
        let selected = manager.selectedEventID == event.id
        let moving = drag?.eventID == event.id && drag?.kind == .move
        let resizing = activeResize(event, geometry: geometry) != nil
        // Recurring/all-day events keep their series/whole-day semantics,
        // which a free-form drag can't express, and a block clamped at a day
        // edge is only a slice of a longer event, so dragging it is equally
        // ambiguous — all of those stay editable via the form.
        let draggable = !event.recurring && !event.allDay
            && !block.continuesBefore && !block.continuesAfter
        let committedHeight = geometry.height(forMinutes: block.minutes)
        // While an edge is dragged, the height previews the live snapped
        // range (blockY handles the top edge's offset).
        let height: CGFloat
        if let live = activeResize(event, geometry: geometry) {
            height = max(geometry.height(forMinutes: live.end - live.start), 16)
        } else {
            height = max(committedHeight, 16)
        }
        // showsTime is keyed to the committed height, not the live one, so
        // the time label doesn't flicker in and out while the block is being
        // resized.
        return EventPillGridLabel(event: event, showsTime: committedHeight > 26)
            .padding(.horizontal, 6).padding(.vertical, 3)
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .frame(height: height, alignment: .top)
            .eventPill(color(for: event), selected: selected,
                       squareTop: block.continuesBefore, squareBottom: block.continuesAfter)
            .overlay(alignment: .top) {
                if draggable { resizeHandle(event, edge: .resizeStart, blockHeight: height, geometry: geometry) }
            }
            .overlay(alignment: .bottom) {
                if draggable { resizeHandle(event, edge: .resizeEnd, blockHeight: height, geometry: geometry) }
            }
            // While moving, the original stays put as a faint ghost marking
            // the origin (the floating copy carries the shadow); while
            // resizing the block itself is the live preview, so it gets the
            // lifted look.
            .opacity(moving ? 0.35 : (resizing ? 0.85 : 1))
            .shadow(color: .black.opacity(resizing ? 0.3 : 0), radius: 4, y: 2)
            .padding(.horizontal, 1)
            .contentShape(Rectangle())
            .calendarEventInteractions(event)
            // Drag the body to move the event in time (and across days). The
            // resize handles sit on top of the edges, so they win there.
            .gesture(moveGesture(block, dayIndex: dayIndex, geometry: geometry, enabled: draggable))
    }

    /// A thin grip on the block's top or bottom edge: the top one drags the
    /// START (end fixed), the bottom one the END (start fixed). The grip
    /// height adapts to the block so the two handles never swallow a short
    /// block's move-draggable middle.
    private func resizeHandle(_ event: CalendarEvent, edge: DragKind, blockHeight: CGFloat,
                              geometry: TimeGeometry) -> some View
    {
        WeekResizeHandle(
            isTop: edge == .resizeStart,
            height: min(8, max(blockHeight / 4, 4)),
            gesture: resizeGesture(event, edge: edge, geometry: geometry)
        )
    }

    private func resizeGesture(_ event: CalendarEvent, edge: DragKind,
                               geometry: TimeGeometry) -> some Gesture
    {
        DragGesture(minimumDistance: 2)
            .onChanged { value in
                guard !dragCancelled else { return }
                if drag == nil { installEscapeMonitor() }
                drag = DragSession(eventID: event.id, kind: edge, translation: value.translation)
            }
            .onEnded { value in
                if dragCancelled { dragCancelled = false; return }
                guard drag?.eventID == event.id else { return }
                let live = resizedMinutes(event, edge: edge,
                                          translationHeight: value.translation.height,
                                          geometry: geometry)
                let day = calendar.startOfDay(for: event.start)
                let start = edge == .resizeStart ? (date(on: day, minutes: live.start) ?? event.start) : event.start
                let end = edge == .resizeEnd ? (date(on: day, minutes: live.end) ?? event.end) : event.end
                // Ease from the live drag to the snapped result so the block
                // settles instead of jumping.
                withAnimation(.easeOut(duration: 0.12)) {
                    endDragSession()
                    manager.reschedule(event, start: start, end: end)
                }
            }
    }

    /// The move gesture for an event body, in the grid's named coordinate
    /// space so pointer positions are absolute. When disabled (recurring/
    /// all-day/day-edge slice) it still returns a gesture so the type is
    /// stable, but does nothing.
    private func moveGesture(_ block: DayLayoutIndex.Block, dayIndex: Int,
                             geometry: TimeGeometry, enabled: Bool) -> some Gesture
    {
        let event = block.event
        return DragGesture(minimumDistance: 6, coordinateSpace: .named(Self.gridSpace))
            .onChanged { value in
                guard enabled, !dragCancelled else { return }
                if drag == nil {
                    // Record the grab offset ONCE: pointer minus the block's
                    // grid-space origin. Fixed for the whole gesture, it
                    // makes the landing cell a function of the block's own
                    // position, not of where inside it the drag started.
                    let laneWidth = geometry.dayWidth / CGFloat(max(block.laneCount, 1))
                    let origin = CGPoint(
                        x: geometry.x(forDayIndex: dayIndex) + CGFloat(block.lane) * laneWidth,
                        y: geometry.y(forMinutes: block.startMinute)
                    )
                    drag = DragSession(eventID: event.id, kind: .move,
                                       grabOffset: CGSize(width: value.startLocation.x - origin.x,
                                                          height: value.startLocation.y - origin.y))
                    installEscapeMonitor()
                }
                guard let session = drag, session.eventID == event.id, session.kind == .move else { return }
                let target = geometry.moveTarget(location: value.location,
                                                 grabOffset: session.grabOffset,
                                                 durationMinutes: blockMinutes(event))
                // Publish only actual cell changes: the floating copy snaps
                // from slot to slot, so most pointer frames are no-ops.
                if dragPreview.target != target { dragPreview.target = target }
            }
            .onEnded { value in
                if dragCancelled { dragCancelled = false; return }
                guard enabled, let session = drag, session.eventID == event.id,
                      session.kind == .move else { return }
                // The SAME targeting function the preview used — the drop
                // lands exactly on the cell the floating copy showed.
                let target = geometry.moveTarget(location: value.location,
                                                 grabOffset: session.grabOffset,
                                                 durationMinutes: blockMinutes(event))
                withAnimation(.easeOut(duration: 0.12)) {
                    endDragSession()
                    if target.dayIndex < weekDays.count,
                       let start = date(on: weekDays[target.dayIndex], minutes: target.startMinute)
                    {
                        let end = start.addingTimeInterval(event.end.timeIntervalSince(event.start))
                        manager.reschedule(event, start: start, end: end)
                    }
                }
            }
    }

    // MARK: - Session lifecycle (Escape cancels)

    /// Installs a local key monitor for the duration of a drag session:
    /// Escape cancels the drag, settling everything back and committing
    /// nothing.
    private func installEscapeMonitor() {
        guard escapeMonitor == nil else { return }
        escapeMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            guard event.keyCode == 53 else { return event } // Escape
            // The pointer is still held: mark the sequence dead so its
            // remaining onChanged/onEnded do nothing until release.
            dragCancelled = true
            withAnimation(.easeOut(duration: 0.12)) {
                endDragSession()
            }
            return nil // consumed
        }
    }

    /// The single teardown path for a drag session — drop, Escape cancel and
    /// view disappearance all come through here, so the key monitor can
    /// never leak.
    private func endDragSession() {
        if let monitor = escapeMonitor {
            NSEvent.removeMonitor(monitor)
            escapeMonitor = nil
        }
        drag = nil
        dragPreview.target = nil
    }

    // MARK: - Drag preview + snapping

    /// The move drag's floating copy: the dragged event rendered once, above
    /// all columns, snapped to the landing cell the drop will commit to.
    @ViewBuilder
    private func movePreview(geometry: TimeGeometry) -> some View {
        if let d = drag, d.kind == .move,
           let event = manager.events.first(where: { $0.id == d.eventID })
        {
            MovePreview(model: dragPreview, geometry: geometry) { target in
                previewBlock(event, target: target, geometry: geometry)
                    // Full column width: until the drop re-lays-out the day,
                    // the landing cell is the whole column (the dimmed
                    // original keeps marking the origin lane).
                    .frame(width: geometry.dayWidth)
            }
        }
    }

    /// The visual body of the floating copy: inert — no gestures, no resize
    /// handles, no hit testing — so pointer events keep flowing to the
    /// stationary original underneath it.
    ///
    /// A block in flight answers a different question than a block at rest.
    /// At rest it says what it is; in flight the only thing worth reading is
    /// where it will land, so its own content recedes behind a blur and the
    /// drop time takes the middle. The tilt, the slight scale and the soft
    /// shadow are what make it read as picked UP rather than merely moved —
    /// a pure translation looks like a rendering artifact.
    private func previewBlock(_ event: CalendarEvent, target: TimeGeometry.MoveTarget,
                              geometry: TimeGeometry) -> some View
    {
        let committedHeight = geometry.height(forMinutes: blockMinutes(event))
        return EventPillGridLabel(event: event, showsTime: committedHeight > 26)
            .padding(.horizontal, 6).padding(.vertical, 3)
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .frame(height: max(committedHeight, 16), alignment: .top)
            .blur(radius: 2)
            .opacity(0.45)
            .overlay {
                Text(dropLabel(target))
                    .font(.system(size: 12, weight: .semibold))
                    .lineLimit(1)
                    .minimumScaleFactor(0.6)
                    .padding(.horizontal, 4)
            }
            .eventPill(color(for: event), selected: false)
            .rotationEffect(.degrees(-3))
            .scaleEffect(1.04)
            .shadow(color: .black.opacity(0.28), radius: 12, y: 6)
            .padding(.horizontal, 1)
            .allowsHitTesting(false)
    }

    /// "Wed 11:00" — the weekday and start time the drop will commit to.
    private func dropLabel(_ target: TimeGeometry.MoveTarget) -> String {
        guard target.dayIndex >= 0, target.dayIndex < weekDays.count else { return "" }
        let day = weekDays[target.dayIndex]
        let weekday = Self.weekdayFormatter.string(from: day)
        guard let date = date(on: day, minutes: target.startMinute) else { return weekday }
        return "\(weekday) \(CalendarTimeFormat.time(date))"
    }

    /// Positions the floating copy at the snapped landing cell. A dedicated
    /// subview observing the preview model so each cell change re-renders
    /// only this view, never the week grid. Renders nothing until the first
    /// onChanged publishes a target.
    private struct MovePreview<Content: View>: View {
        @ObservedObject var model: DragPreviewModel
        let geometry: TimeGeometry
        let content: (TimeGeometry.MoveTarget) -> Content

        init(model: DragPreviewModel, geometry: TimeGeometry,
             @ViewBuilder content: @escaping (TimeGeometry.MoveTarget) -> Content)
        {
            self.model = model
            self.geometry = geometry
            self.content = content
        }

        var body: some View {
            // The copy sits exactly on the cell the drop commits to (the
            // target comes from the moveTarget call onEnded repeats): what
            // you see is where it lands, clicking from slot to slot instead
            // of tracking the pointer continuously.
            if let target = model.target {
                content(target)
                    .offset(x: geometry.x(forDayIndex: target.dayIndex),
                            y: geometry.y(forMinutes: target.startMinute))
            }
        }
    }

    /// The live snapped [start, end] minutes of the block being resized, nil
    /// when this event is not in a resize session. Both the in-place preview
    /// (blockY + block height) and the drop commit read resizedMinutes, so
    /// they cannot disagree.
    private func activeResize(_ event: CalendarEvent, geometry: TimeGeometry) -> (start: Int, end: Int)? {
        guard let d = drag, d.eventID == event.id,
              d.kind == .resizeStart || d.kind == .resizeEnd else { return nil }
        return resizedMinutes(event, edge: d.kind, translationHeight: d.translation.height,
                              geometry: geometry)
    }

    /// Snapped start/end minutes for a resize drag. Top edge: the start
    /// moves within [0, end - one slot] (end fixed); bottom edge: the end
    /// moves within [start + one slot, midnight] (start fixed).
    private func resizedMinutes(_ event: CalendarEvent, edge: DragKind, translationHeight: CGFloat,
                                geometry: TimeGeometry) -> (start: Int, end: Int)
    {
        let startMin = minutesFromMidnight(event.start)
        // Duration-based end so an event ending exactly at midnight reads
        // 1440, not 0 (draggable blocks never continue past their day).
        let endMin = startMin + blockMinutes(event)
        let delta = geometry.snappedMinuteDelta(fromPoints: translationHeight)
        switch edge {
        case .resizeStart:
            return (min(max(startMin + delta, 0), endMin - geometry.snapMinutes), endMin)
        case .resizeEnd:
            return (startMin, min(max(endMin + delta, startMin + geometry.snapMinutes), 24 * 60))
        case .move:
            return (startMin, endMin)
        }
    }

    /// The Date at a wall-clock minute-of-day on `day`'s calendar day.
    private func date(on day: Date, minutes: Int) -> Date? {
        var comps = calendar.dateComponents([.year, .month, .day], from: day)
        comps.hour = minutes / 60
        comps.minute = minutes % 60
        return calendar.date(from: comps)
    }

    /// A "+X" block standing in for concurrent events that exceed the visible
    /// lanes. Tapping selects the earliest hidden event; tapping again cycles
    /// through the rest, so every hidden event stays reachable via the detail
    /// pane.
    private func overflowBlock(_ item: DayLayoutIndex.OverflowBlock, geometry: TimeGeometry) -> some View {
        let selected = item.events.contains { $0.id == manager.selectedEventID }
        let height = max(geometry.height(forMinutes: item.endMinute - item.startMinute), 16)
        return Text("+\(item.events.count)")
            .font(.system(size: 11)).fontWeight(.semibold)
            .padding(.horizontal, 6).padding(.vertical, 3)
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .frame(height: height, alignment: .top)
            // The same chrome the event blocks use, in a neutral gray: the "+X"
            // stands in for events, so it must read as one of them rather than
            // as a separate kind of object. Going through the modifier is also
            // what keeps its radius and selection ring from drifting.
            .eventPill(.secondary, selected: selected)
            .padding(.horizontal, 1)
            .contentShape(Rectangle())
            .onTapGesture {
                let ids = item.events.map(\.id)
                if let current = manager.selectedEventID, let idx = ids.firstIndex(of: current) {
                    manager.select(item.events[(idx + 1) % item.events.count])
                } else if let first = item.events.first {
                    manager.select(first)
                }
            }
    }

    private func eventChip(_ event: CalendarEvent) -> some View {
        let selected = manager.selectedEventID == event.id
        return Text(event.displaySubject)
            .font(.system(size: 10)).fontWeight(.semibold).lineLimit(1)
            .padding(.horizontal, 6).padding(.vertical, 2)
            .frame(maxWidth: .infinity, alignment: .leading)
            .eventPill(color(for: event), selected: selected, cornerRadius: 6)
            .contentShape(Rectangle())
            .calendarEventInteractions(event)
    }

    /// How many side-by-side lanes the column width supports (~40pt per lane,
    /// at least 1, at most 4). A week column is typically only ~110-120pt wide
    /// (7 columns share the width), so 60pt/lane collapsed even two concurrent
    /// events into a "+2" block; 40pt keeps 2-3 events side-by-side.
    private func maxVisibleLanes(_ colWidth: CGFloat) -> Int {
        min(max(Int(colWidth / 40), 1), 4)
    }

    // MARK: - Geometry helpers

    /// The event's rendered duration in minutes (15-minute visual minimum) —
    /// used by the floating move preview, which shows the unclamped event.
    private func blockMinutes(_ event: CalendarEvent) -> Int {
        max(Int(event.end.timeIntervalSince(event.start) / 60), 15)
    }

    private func minutesFromMidnight(_ date: Date) -> Int {
        let comps = calendar.dateComponents([.hour, .minute], from: date)
        return (comps.hour ?? 0) * 60 + (comps.minute ?? 0)
    }

    // MARK: - Data

    private func color(for event: CalendarEvent) -> Color {
        manager.color(for: event)
    }

    private var weekDays: [Date] {
        guard let interval = calendar.dateInterval(of: .weekOfYear, for: manager.anchorDate) else { return [] }
        return (0 ..< 7).compactMap { calendar.date(byAdding: .day, value: $0, to: interval.start) }
    }

    /// Overlap-based, like the timed grid: a multi-day all-day event shows
    /// its chip on every day it covers, not only its first.
    private func allDayEvents(_ day: Date) -> [CalendarEvent] {
        let dayStart = calendar.startOfDay(for: day)
        guard let dayEnd = calendar.date(byAdding: .day, value: 1, to: dayStart) else { return [] }
        return manager.events.filter {
            $0.allDay && DayLayoutIndex.overlaps($0, dayStart: dayStart, dayEnd: dayEnd)
        }
    }

    private static let weekdayFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "EEE"; return f
    }()
}

// MARK: - Resize handle

/// The invisible grip on a block's top or bottom edge, owning its own hover
/// state.
///
/// That ownership is the entire point of it being a separate view. The hover
/// flag used to live on CalendarWeekView, so entering or leaving ANY grip
/// invalidated the whole grid body — seven columns and every block in them —
/// and a block carries two grips, so simply moving the pointer across the
/// week re-rendered everything continuously. Local state keeps each hover to
/// the ~8pt strip it happened in.
private struct WeekResizeHandle<G: Gesture>: View {
    let isTop: Bool
    let height: CGFloat
    let gesture: G

    @State private var hovering = false

    var body: some View {
        // No persistent grip — the affordance is the resize cursor and a faint
        // edge line, both only while the pointer is over the edge.
        Color.white.opacity(0.001) // invisible but hit-testable
            .frame(height: height)
            .overlay(alignment: isTop ? .top : .bottom) {
                Rectangle()
                    .fill(Color.primary.opacity(hovering ? 0.35 : 0))
                    .frame(height: 2)
            }
            .contentShape(Rectangle())
            .onHover { inside in
                if inside, !hovering {
                    hovering = true
                    NSCursor.resizeUpDown.push()
                } else if !inside, hovering {
                    hovering = false
                    NSCursor.pop()
                }
            }
            .onDisappear {
                if hovering {
                    hovering = false
                    NSCursor.pop()
                }
            }
            .gesture(gesture)
    }
}
