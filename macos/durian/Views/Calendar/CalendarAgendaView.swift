//
//  CalendarAgendaView.swift
//  Durian
//
//  The agenda list: events grouped by day (local time), scrollable and
//  cursor-navigable (vim j/k via CalendarManager.selectedEventID). Mirrors the
//  mail list's ScrollView + LazyVStack + ScrollViewReader skeleton.
//

import SwiftUI

struct CalendarAgendaView: View {
    @ObservedObject var manager = CalendarManager.shared

    var body: some View {
        Group {
            if manager.events.isEmpty {
                emptyState
            } else {
                agendaList
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private var agendaList: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0, pinnedViews: [.sectionHeaders]) {
                    ForEach(dayGroups) { group in
                        Section {
                            ForEach(group.events) { event in
                                CalendarEventRow(
                                    event: event,
                                    isSelected: manager.selectedEventID == event.id
                                )
                                .id(event.id)
                                .contentShape(Rectangle())
                                .calendarEventInteractions(event)
                            }
                        } header: {
                            dayHeader(group.date)
                        }
                    }
                }
                .padding(.vertical, 4)
            }
            .onChange(of: manager.selectedEventID) { _, id in
                guard let id else { return }
                withAnimation(.easeInOut(duration: 0.15)) {
                    proxy.scrollTo(id, anchor: .center)
                }
            }
        }
    }

    private func dayHeader(_ date: Date) -> some View {
        Text(Self.dayFormatter.string(from: date))
            .font(.caption).fontWeight(.semibold)
            .foregroundStyle(.secondary)
            .padding(.horizontal, 20).padding(.top, 10).padding(.bottom, 4)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(nsColor: .windowBackgroundColor))
    }

    private var emptyState: some View {
        VStack(spacing: 10) {
            Image(systemName: "calendar").font(.system(size: 34)).foregroundStyle(.tertiary)
            Text("No events in this period.").font(.callout).foregroundStyle(.secondary)
            Text("Run 'durian calendar sync' if calendars look empty.")
                .font(.caption).foregroundStyle(.tertiary)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    // MARK: - Grouping

    private struct DayGroup: Identifiable {
        let date: Date
        let events: [CalendarEvent]
        var id: Date { date }
    }

    private var dayGroups: [DayGroup] {
        let cal = Calendar.current
        let grouped = Dictionary(grouping: manager.events) { cal.startOfDay(for: $0.start) }
        return grouped.keys.sorted().map { day in
            DayGroup(date: day, events: (grouped[day] ?? []).sorted { $0.start < $1.start })
        }
    }

    private static let dayFormatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "EEEE, d MMMM"
        return f
    }()
}
