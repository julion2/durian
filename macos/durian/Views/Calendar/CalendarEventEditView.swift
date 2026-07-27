//
//  CalendarEventEditView.swift
//  Durian
//
//  Create/edit form shown IN the detail pane (right column). Local-first:
//  saving writes the local .ics via the API; nothing is sent to Outlook until
//  the next sync.
//

import SwiftUI

struct CalendarEventEditView: View {
    @State var draft: CalendarEventDraft
    let calendars: [CalendarInfo]
    let onSave: (CalendarEventDraft) -> Void
    let onCancel: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    Text(draft.isNew ? "New Event" : "Edit Event")
                        .font(.title3).fontWeight(.semibold)
                        .foregroundStyle(Color.Detail.textPrimary)

                    if draft.recurring {
                        Label("Recurring event — saving changes the whole series.",
                              systemImage: "repeat")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }

                    labeled("Subject") {
                        TextField("Subject", text: $draft.subject).textFieldStyle(.roundedBorder)
                    }

                    if !calendars.isEmpty {
                        labeled("Calendar") {
                            Picker("", selection: $draft.calendar) {
                                ForEach(calendars) { calendar in
                                    Text(calendar.name).tag(calendar.name)
                                }
                            }
                            .labelsHidden()
                            // Moving an existing event between calendars would
                            // need a remote delete + re-invite; the API rejects
                            // it, so only offer the picker for new events.
                            .disabled(!draft.isNew)
                            .onChange(of: draft.calendar) { _, name in
                                if let match = calendars.first(where: { $0.name == name }) {
                                    draft.account = match.account
                                }
                            }
                        }
                    }

                    Toggle("All day", isOn: $draft.allDay)

                    labeled("Start") {
                        DatePicker("", selection: $draft.start, displayedComponents: dateComponents).labelsHidden()
                    }
                    labeled("End") {
                        DatePicker("", selection: $draft.end, displayedComponents: dateComponents).labelsHidden()
                    }
                    labeled("Location") {
                        TextField("Location", text: $draft.location).textFieldStyle(.roundedBorder)
                    }
                    labeled("Notes") {
                        TextEditor(text: $draft.description)
                            .frame(minHeight: 100)
                            .font(.body)
                            .overlay(RoundedRectangle(cornerRadius: 5).stroke(Color.Detail.border))
                    }
                }
                .padding(20)
            }

            Divider()
            HStack {
                Button("Cancel", role: .cancel) { onCancel() }
                    .keyboardShortcut(.cancelAction)
                Spacer()
                Button("Save") { onSave(draft) }
                    .keyboardShortcut(.defaultAction)
                    .buttonStyle(.borderedProminent)
                    .disabled(!isValid)
            }
            .padding(12)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func labeled<Content: View>(_ label: String, @ViewBuilder _ content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label).font(.caption).foregroundStyle(.secondary)
            content()
        }
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
