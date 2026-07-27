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

import SwiftUI

struct CalendarWeekView: View {
    @ObservedObject var manager = CalendarManager.shared
    private let calendar = Calendar.current

    private let hourHeight: CGFloat = 44
    private let timeColumnWidth: CGFloat = 48
    private let startHour = 0
    private let endHour = 24

    var body: some View {
        // The GeometryReader supplies the day-column width the lane layout
        // needs (the HStack splits the remainder after the time axis evenly
        // across the 7 flexible columns).
        GeometryReader { geo in
            let colWidth = max((geo.size.width - timeColumnWidth) / 7, 1)
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(spacing: 0, pinnedViews: [.sectionHeaders]) {
                        Section {
                            HStack(alignment: .top, spacing: 0) {
                                timeAxis
                                ForEach(weekDays, id: \.self) { day in
                                    dayColumn(day, colWidth: colWidth).frame(maxWidth: .infinity)
                                }
                            }
                        } header: {
                            VStack(spacing: 0) {
                                dayHeaderRow
                                allDayRow
                                Divider()
                            }
                            .background(Color(nsColor: .windowBackgroundColor))
                        }
                    }
                }
                .onChange(of: manager.selectedEventID) { _, _ in
                    scrollToSelectedHour(proxy)
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

    private var dayHeaderRow: some View {
        HStack(spacing: 0) {
            Color.clear.frame(width: timeColumnWidth)
            ForEach(weekDays, id: \.self) { day in
                VStack(spacing: 1) {
                    Text(Self.weekdayFormatter.string(from: day))
                        .font(.caption2).foregroundStyle(.secondary)
                    Text("\(calendar.component(.day, from: day))")
                        .font(.callout)
                        .fontWeight(calendar.isDateInToday(day) ? .bold : .regular)
                        .foregroundStyle(calendar.isDateInToday(day) ? Color.accentColor : Color.Detail.textPrimary)
                }
                .frame(maxWidth: .infinity)
            }
        }
        .padding(.vertical, 4)
    }

    private var allDayRow: some View {
        HStack(alignment: .top, spacing: 0) {
            Text("all-day").font(.system(size: 8)).foregroundStyle(.secondary)
                .frame(width: timeColumnWidth)
            ForEach(weekDays, id: \.self) { day in
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
                Text(String(format: "%02d", hour))
                    .font(.system(size: 9)).foregroundStyle(.secondary)
                    .frame(height: hourHeight, alignment: .top)
                    .id("hour-\(hour)")
            }
        }
        .frame(width: timeColumnWidth)
    }

    private func dayColumn(_ day: Date, colWidth: CGFloat) -> some View {
        let layout = dayLayout(timedEvents(day), maxVisibleLanes: maxVisibleLanes(colWidth))
        func laneWidth(_ laneCount: Int) -> CGFloat {
            colWidth / CGFloat(max(laneCount, 1))
        }
        return ZStack(alignment: .topLeading) {
            ForEach(startHour ... endHour, id: \.self) { hour in
                Rectangle().fill(Color.Detail.border.opacity(0.6))
                    .frame(height: 0.5)
                    .frame(maxWidth: .infinity, alignment: .top)
                    .offset(y: CGFloat(hour - startHour) * hourHeight)
            }
            ForEach(layout.visible) { item in
                timedBlock(item.event)
                    .frame(width: laneWidth(item.laneCount))
                    .offset(x: CGFloat(item.lane) * laneWidth(item.laneCount), y: yOffset(item.event))
            }
            ForEach(layout.overflow) { item in
                overflowBlock(item)
                    .frame(width: laneWidth(item.laneCount))
                    .offset(x: CGFloat(item.lane) * laneWidth(item.laneCount), y: minuteOffset(item.startMinute))
            }
        }
        .frame(height: CGFloat(endHour - startHour) * hourHeight, alignment: .top)
        .overlay(alignment: .trailing) {
            Rectangle().fill(Color.Detail.border.opacity(0.6)).frame(width: 0.5)
        }
        .contentShape(Rectangle())
        // Tap an empty part of a day to create a new event at that time.
        // Event blocks handle their own tap, so this only fires on empty space.
        .gesture(SpatialTapGesture().onEnded { value in
            let hour = min(max(Int(value.location.y / hourHeight) + startHour, startHour), endHour - 1)
            if let date = dateAt(day, hour: hour) {
                manager.beginCreate(at: date)
            }
        })
    }

    private func dateAt(_ day: Date, hour: Int) -> Date? {
        var comps = calendar.dateComponents([.year, .month, .day], from: day)
        comps.hour = hour
        comps.minute = 0
        return calendar.date(from: comps)
    }

    private func timedBlock(_ event: CalendarEvent) -> some View {
        let selected = manager.selectedEventID == event.id
        return VStack(alignment: .leading, spacing: 1) {
            Text(event.displaySubject).font(.system(size: 10)).fontWeight(.medium).lineLimit(2)
            if blockHeight(event) > 26 {
                Text(Self.timeFormatter.string(from: event.start)).font(.system(size: 8))
            }
        }
        .padding(.horizontal, 3).padding(.vertical, 2)
        .frame(maxWidth: .infinity, alignment: .topLeading)
        .frame(height: max(blockHeight(event), 16), alignment: .top)
        .background(RoundedRectangle(cornerRadius: 4).fill(color(for: event).opacity(selected ? 1.0 : 0.9)))
        .overlay(RoundedRectangle(cornerRadius: 4).strokeBorder(Color.primary, lineWidth: selected ? 2 : 0))
        .foregroundStyle(.white)
        .padding(.horizontal, 1)
        .contentShape(Rectangle())
        .onTapGesture { manager.selectedEventID = event.id }
    }

    /// A "+X" block standing in for concurrent events that exceed the visible
    /// lanes. Tapping selects the earliest hidden event; tapping again cycles
    /// through the rest, so every hidden event stays reachable via the detail
    /// pane.
    private func overflowBlock(_ item: OverflowItem) -> some View {
        let selected = item.events.contains { $0.id == manager.selectedEventID }
        let height = max(CGFloat(item.endMinute - item.startMinute) / 60 * hourHeight, 16)
        return Text("+\(item.events.count)")
            .font(.system(size: 10)).fontWeight(.semibold)
            .padding(.horizontal, 3).padding(.vertical, 2)
            .frame(maxWidth: .infinity, alignment: .topLeading)
            .frame(height: height, alignment: .top)
            .background(RoundedRectangle(cornerRadius: 4).fill(Color.secondary.opacity(selected ? 0.9 : 0.7)))
            .overlay(RoundedRectangle(cornerRadius: 4).strokeBorder(Color.primary, lineWidth: selected ? 2 : 0))
            .foregroundStyle(.white)
            .padding(.horizontal, 1)
            .contentShape(Rectangle())
            .onTapGesture {
                let ids = item.events.map(\.id)
                if let current = manager.selectedEventID, let idx = ids.firstIndex(of: current) {
                    manager.selectedEventID = ids[(idx + 1) % ids.count]
                } else {
                    manager.selectedEventID = ids.first
                }
            }
    }

    private func eventChip(_ event: CalendarEvent) -> some View {
        let selected = manager.selectedEventID == event.id
        return Text(event.displaySubject)
            .font(.system(size: 9)).lineLimit(1)
            .padding(.horizontal, 4).padding(.vertical, 1)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 3).fill(color(for: event).opacity(selected ? 1.0 : 0.85)))
            .overlay(RoundedRectangle(cornerRadius: 3).strokeBorder(Color.primary, lineWidth: selected ? 1.5 : 0))
            .foregroundStyle(.white)
            .contentShape(Rectangle())
            .onTapGesture { manager.selectedEventID = event.id }
    }

    // MARK: - Overlap lanes

    /// Placement for one visible timed event: its lane and the lane count of
    /// its overlap cluster (which determines the lane width).
    private struct LaidOutEvent: Identifiable {
        let event: CalendarEvent
        let lane: Int
        let laneCount: Int
        var id: String { event.id }
    }

    /// A "+X" block collapsing the events of the overflow lanes of one
    /// cluster into the last visible lane over their combined time span.
    private struct OverflowItem: Identifiable {
        let events: [CalendarEvent] // hidden events, sorted by start
        let startMinute: Int
        let endMinute: Int
        let lane: Int
        let laneCount: Int
        var id: String { "overflow-\(startMinute)-\(events.first?.id ?? "")" }
    }

    private struct DayLayout {
        var visible: [LaidOutEvent] = []
        var overflow: [OverflowItem] = []
    }

    private struct LaneEntry {
        let event: CalendarEvent
        let startMinute: Int
        let endMinute: Int
    }

    /// How many side-by-side lanes the column width supports (~40pt per lane,
    /// at least 1, at most 4). A week column is typically only ~110-120pt wide
    /// (7 columns share the width), so 60pt/lane collapsed even two concurrent
    /// events into a "+2" block; 40pt keeps 2-3 events side-by-side.
    private func maxVisibleLanes(_ colWidth: CGFloat) -> Int {
        min(max(Int(colWidth / 40), 1), 4)
    }

    /// Groups a day's timed events into overlap clusters (an event joins the
    /// current cluster while it starts before the cluster's running maximum
    /// end) and lays each cluster out into lanes.
    private func dayLayout(_ events: [CalendarEvent], maxVisibleLanes: Int) -> DayLayout {
        var layout = DayLayout()
        guard !events.isEmpty else { return layout }
        // Longer events first on equal starts so they take the leftmost lane;
        // id as the final key keeps the layout stable across refreshes.
        let sorted = events.sorted { a, b in
            if a.start != b.start { return a.start < b.start }
            if a.end != b.end { return a.end > b.end }
            return a.id < b.id
        }
        var cluster: [LaneEntry] = []
        var clusterMaxEnd = Int.min
        for event in sorted {
            let start = minutesFromMidnight(event.start)
            // Overlap uses the rendered extent: at least the 15-minute visual
            // minimum, so zero-duration events still claim their block's space.
            let end = max(start + Int(event.end.timeIntervalSince(event.start) / 60), start + 15)
            if !cluster.isEmpty && start >= clusterMaxEnd {
                layoutCluster(cluster, maxVisibleLanes: maxVisibleLanes, into: &layout)
                cluster.removeAll()
            }
            cluster.append(LaneEntry(event: event, startMinute: start, endMinute: end))
            clusterMaxEnd = cluster.count == 1 ? end : max(clusterMaxEnd, end)
        }
        layoutCluster(cluster, maxVisibleLanes: maxVisibleLanes, into: &layout)
        return layout
    }

    /// Assigns each cluster event the lowest lane that is free at its start
    /// (greedy sweep). When the cluster needs more lanes than fit, the events
    /// of the surplus lanes collapse into one OverflowItem in the last
    /// visible lane.
    private func layoutCluster(_ cluster: [LaneEntry], maxVisibleLanes: Int, into layout: inout DayLayout) {
        guard !cluster.isEmpty else { return }
        var laneEnds: [Int] = []
        var lanes: [Int] = []
        for entry in cluster {
            if let lane = laneEnds.indices.first(where: { laneEnds[$0] <= entry.startMinute }) {
                laneEnds[lane] = entry.endMinute
                lanes.append(lane)
            } else {
                laneEnds.append(entry.endMinute)
                lanes.append(laneEnds.count - 1)
            }
        }
        let laneCount = laneEnds.count
        if laneCount <= maxVisibleLanes {
            for (i, entry) in cluster.enumerated() {
                layout.visible.append(LaidOutEvent(event: entry.event, lane: lanes[i], laneCount: laneCount))
            }
            return
        }
        // More concurrent events than lanes: the last visible lane becomes
        // the "+X" block for everything at or beyond it.
        let overflowLane = maxVisibleLanes - 1
        var hidden: [LaneEntry] = []
        for (i, entry) in cluster.enumerated() {
            if lanes[i] < overflowLane {
                layout.visible.append(LaidOutEvent(event: entry.event, lane: lanes[i], laneCount: maxVisibleLanes))
            } else {
                hidden.append(entry)
            }
        }
        guard let first = hidden.first else { return }
        layout.overflow.append(OverflowItem(
            events: hidden.map(\.event),
            startMinute: hidden.map(\.startMinute).min() ?? first.startMinute,
            endMinute: hidden.map(\.endMinute).max() ?? first.endMinute,
            lane: overflowLane,
            laneCount: maxVisibleLanes
        ))
    }

    // MARK: - Geometry

    private func yOffset(_ event: CalendarEvent) -> CGFloat {
        minuteOffset(minutesFromMidnight(event.start))
    }

    private func minuteOffset(_ minute: Int) -> CGFloat {
        CGFloat(minute - startHour * 60) / 60 * hourHeight
    }

    private func blockHeight(_ event: CalendarEvent) -> CGFloat {
        let minutes = max(event.end.timeIntervalSince(event.start) / 60, 15)
        return CGFloat(minutes) / 60 * hourHeight
    }

    private func minutesFromMidnight(_ date: Date) -> Int {
        let comps = calendar.dateComponents([.hour, .minute], from: date)
        return (comps.hour ?? 0) * 60 + (comps.minute ?? 0)
    }

    // MARK: - Data

    private func color(for event: CalendarEvent) -> Color {
        manager.calendars.first { $0.name == event.calendar }?.color ?? .secondary
    }

    private var weekDays: [Date] {
        guard let interval = calendar.dateInterval(of: .weekOfYear, for: manager.anchorDate) else { return [] }
        return (0 ..< 7).compactMap { calendar.date(byAdding: .day, value: $0, to: interval.start) }
    }

    private func eventsOn(_ day: Date) -> [CalendarEvent] {
        let start = calendar.startOfDay(for: day)
        return manager.events.filter { calendar.isDate($0.start, inSameDayAs: start) }
    }

    private func timedEvents(_ day: Date) -> [CalendarEvent] {
        eventsOn(day).filter { !$0.allDay }.sorted { $0.start < $1.start }
    }

    private func allDayEvents(_ day: Date) -> [CalendarEvent] {
        eventsOn(day).filter { $0.allDay }
    }

    private static let weekdayFormatter: DateFormatter = {
        let f = DateFormatter(); f.dateFormat = "EEE"; return f
    }()

    private static let timeFormatter: DateFormatter = {
        let f = DateFormatter(); f.timeStyle = .short; f.dateStyle = .none; return f
    }()
}
