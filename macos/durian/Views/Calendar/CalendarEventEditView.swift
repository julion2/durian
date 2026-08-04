//
//  CalendarEventEditView.swift
//  Durian
//
//  Create/edit form shown IN the detail pane (right column). Local-first:
//  saving writes the local .ics via the API; nothing is sent to Outlook until
//  the next sync. Presented as grouped rounded cards on the pane background.
//

import SwiftUI

struct CalendarEventEditView: View {
    @State var draft: CalendarEventDraft
    let calendars: [CalendarInfo]
    let onSave: (CalendarEventDraft) -> Void
    let onCancel: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            headerBar
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    titleCard
                    calendarCard
                    timeCard
                    notesCard
                    if !draft.isNew {
                        deleteButton
                    }
                }
                .padding(16)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(nsColor: .windowBackgroundColor))
    }

    // MARK: - Header (Cancel / title / Save)

    private var headerBar: some View {
        ZStack {
            Text(draft.isNew ? "New Event" : "Edit Event")
                .font(.headline)
                .foregroundStyle(Color.Detail.textPrimary)
            HStack {
                Button("Cancel", role: .cancel) { onCancel() }
                    .keyboardShortcut(.cancelAction)
                Spacer()
                Button("Save") { onSave(draft) }
                    .keyboardShortcut(.defaultAction)
                    .buttonStyle(.borderedProminent)
                    .disabled(!isValid)
            }
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
    }

    // MARK: - Cards

    private var titleCard: some View {
        card {
            TextField("Title", text: $draft.subject)
                .textFieldStyle(.plain)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(Color.Detail.textPrimary)
            Divider().overlay(Color.Detail.border)
            TextField("Location", text: $draft.location)
                .textFieldStyle(.plain)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
        }
    }

    private var calendarCard: some View {
        card("Calendar") {
            HStack(spacing: 8) {
                Circle()
                    .fill(selectedCalendarColor)
                    .frame(width: 10, height: 10)
                if !calendars.isEmpty && draft.isNew {
                    Picker("", selection: calendarSelection) {
                        ForEach(calendars) { calendar in
                            Text(calendar.name).tag(calendar.visibilityKey)
                        }
                    }
                    .pickerStyle(.menu)
                    .labelsHidden()
                } else {
                    // Moving an existing event between calendars would need a
                    // remote delete + re-invite; the API rejects it, so only
                    // offer the picker for new events.
                    Text(draft.calendar)
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                }
                Spacer(minLength: 0)
            }
        }
    }

    private var timeCard: some View {
        card {
            Picker("", selection: $draft.allDay) {
                Text("All-Day").tag(true)
                Text("Time Slot").tag(false)
            }
            .pickerStyle(.segmented)
            .labelsHidden()

            dateRow("Starts", selection: $draft.start)
            Divider().overlay(Color.Detail.border)
            dateRow("Ends", selection: $draft.end)

            if draft.recurring {
                Label("Recurring event — saving changes the whole series.",
                      systemImage: "repeat")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
        }
    }

    private var notesCard: some View {
        card("Notes") {
            TextEditor(text: $draft.description)
                .frame(minHeight: 100)
                .font(.body)
                .foregroundStyle(Color.Detail.textBody)
                .scrollContentBackground(.hidden)
        }
    }

    private var deleteButton: some View {
        Button(role: .destructive) {
            CalendarManager.shared.deleteSelected()
            onCancel()
        } label: {
            Label("Delete Event", systemImage: "trash")
                .frame(maxWidth: .infinity)
        }
        .buttonStyle(.bordered)
        .tint(.red)
    }

    // MARK: - Styling helpers

    /// A grouped rounded card: hairline border over the card surface, with an
    /// optional caption heading above the content.
    private func card<Content: View>(_ title: String? = nil,
                                     @ViewBuilder content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            if let title {
                Text(title)
                    .font(.caption).fontWeight(.semibold)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
            content()
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.Detail.cardBackground)
        .clipShape(RoundedRectangle(cornerRadius: 11))
        .overlay(RoundedRectangle(cornerRadius: 11).stroke(Color.Detail.border, lineWidth: 1))
    }

    /// A "Starts"/"Ends" row: label on the left, the picker trailing. The time
    /// component disappears while All-Day is on.
    private func dateRow(_ label: String, selection: Binding<Date>) -> some View {
        HStack {
            Text(label)
                .font(.callout)
                .foregroundStyle(Color.Detail.textSecondary)
            Spacer()
            DatePicker("", selection: selection, displayedComponents: dateComponents)
                .labelsHidden()
        }
    }

    /// The draft calendar's color, used as a small accent dot.
    /// Selection over the (account, calendar) identity rather than the bare
    /// name. Two accounts commonly each own a calendar called "Calendar", and
    /// resolving by name alone silently filed new events under whichever
    /// account happened to come first.
    private var calendarSelection: Binding<String> {
        Binding(
            get: { CalendarInfo.key(account: draft.account, name: draft.calendar) },
            set: { key in
                guard let match = calendars.first(where: { $0.visibilityKey == key }) else { return }
                draft.calendar = match.name
                draft.account = match.account
            }
        )
    }

    private var selectedCalendar: CalendarInfo? {
        calendars.first { $0.visibilityKey == CalendarInfo.key(account: draft.account, name: draft.calendar) }
    }

    private var selectedCalendarColor: Color {
        selectedCalendar?.color ?? .secondary
    }

    private var dateComponents: DatePickerComponents {
        draft.allDay ? [.date] : [.date, .hourAndMinute]
    }

    private var isValid: Bool {
        guard !draft.subject.trimmingCharacters(in: .whitespaces).isEmpty else { return false }
        if draft.allDay {
            // A same-day all-day event is valid — the write path snaps it to a
            // full day. Only an end day before the start day is invalid.
            let cal = Calendar.current
            return cal.startOfDay(for: draft.end) >= cal.startOfDay(for: draft.start)
        }
        return draft.end > draft.start
    }
}
