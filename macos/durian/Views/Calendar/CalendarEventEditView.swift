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

    /// The attendee email being typed; committed to the draft on return.
    @State private var attendeeInput: String = ""

    /// Title focus, requested on appear for a NEW event so typing can start
    /// immediately (an edit keeps the existing title, so no field is grabbed).
    @FocusState private var titleFocused: Bool

    var body: some View {
        VStack(spacing: 0) {
            headerBar
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    titleCard
                    calendarCard
                    timeCard
                    if draft.isNew {
                        // Attendees and the online-meeting request are
                        // create-only; editing an existing meeting's attendee
                        // set is not supported yet (the write path preserves
                        // it), so no control is offered that could touch it.
                        meetingCard
                    }
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
        // Escape cancels the edit. The Cancel button's .cancelAction shortcut
        // does not fire reliably in a plain detail pane (no dialog context),
        // so catch the exit command on the view itself.
        .onExitCommand { onCancel() }
        .onAppear {
            // Deferred one tick: focus requested while the view is still being
            // installed is dropped.
            if draft.isNew {
                DispatchQueue.main.async { titleFocused = true }
            }
        }
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
                Button("Save") {
                    // A typed-but-not-committed attendee email still counts.
                    addAttendee()
                    onSave(draft)
                }
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
                .focused($titleFocused)
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

            if draft.allDay {
                // Make the all-day semantics explicit: the rows below only
                // pick days, and the write path snaps them to full days.
                Text("Covers whole days — no start or end time.")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }

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

    /// Attendees + online-meeting request for a NEW event. Local-first:
    /// saving only writes the local .ics — the invitations (and the online
    /// meeting) go out on the next manual `durian calendar sync`, which
    /// previews them first; automatic sync never sends them.
    private var meetingCard: some View {
        card("Meeting") {
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Online meeting")
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                    Text("Teams for Microsoft, Meet for Google")
                        .font(.caption)
                        .foregroundStyle(Color.Detail.textSecondary)
                }
                Spacer()
                Toggle("", isOn: $draft.requestOnlineMeeting)
                    .labelsHidden()
                    .toggleStyle(.switch)
            }

            Divider().overlay(Color.Detail.border)

            ForEach(draft.attendees, id: \.self) { email in
                HStack(spacing: 8) {
                    Image(systemName: "person")
                        .font(.caption)
                        .foregroundStyle(Color.Detail.textSecondary)
                        .frame(width: 16)
                    Text(email)
                        .font(.callout)
                        .foregroundStyle(Color.Detail.textBody)
                        .lineLimit(1)
                    Spacer(minLength: 8)
                    Button {
                        draft.attendees.removeAll { $0 == email }
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                            .foregroundStyle(Color.Detail.textSecondary)
                    }
                    .buttonStyle(.plain)
                    .help("Remove attendee")
                    .accessibilityLabel("Remove \(email)")
                }
            }

            HStack(spacing: 8) {
                Image(systemName: "person.badge.plus")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
                    .frame(width: 16)
                TextField("Add attendee (email, press Return)", text: $attendeeInput)
                    .textFieldStyle(.plain)
                    .font(.callout)
                    .foregroundStyle(Color.Detail.textBody)
                    .onSubmit(addAttendee)
                if !trimmedAttendeeInput.isEmpty,
                   !Self.looksLikeEmail(trimmedAttendeeInput)
                {
                    // Subtle malformed-email hint; Return simply does nothing
                    // until the address parses, and Save ignores it.
                    Image(systemName: "exclamationmark.circle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .help("Not a valid email address")
                        .accessibilityLabel("Not a valid email address")
                }
            }

            if !draft.attendees.isEmpty || draft.requestOnlineMeeting {
                Label("Saved locally first — invitations are sent when you run 'durian calendar sync' (automatic sync skips them).",
                      systemImage: "paperplane")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
        }
    }

    /// The typed attendee email, trimmed — shared by the commit below and the
    /// inline validation hint so both always agree.
    private var trimmedAttendeeInput: String {
        attendeeInput.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// Minimal shape check (user@host, no spaces) — deliverability is the
    /// provider's problem; this only guards against obvious typos.
    private static func looksLikeEmail(_ candidate: String) -> Bool {
        candidate.split(separator: "@").count == 2 && !candidate.contains(" ")
    }

    /// Commits the typed attendee email to the draft: trimmed, must look like
    /// an email (user@host), duplicates (case-insensitive) and blanks ignored.
    private func addAttendee() {
        let email = trimmedAttendeeInput
        guard !email.isEmpty, Self.looksLikeEmail(email) else { return }
        guard !draft.attendees.contains(where: { $0.caseInsensitiveCompare(email) == .orderedSame }) else {
            attendeeInput = ""
            return
        }
        draft.attendees.append(email)
        attendeeInput = ""
    }

    private var notesCard: some View {
        card("Notes") {
            TextEditor(text: $draft.description)
                .frame(minHeight: 100)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .scrollContentBackground(.hidden)
                // Cancel the editor's built-in text inset so the notes text
                // left-aligns with the card heading and the other fields.
                .padding(.horizontal, -5)
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
                                     @ViewBuilder content: () -> Content) -> some View
    {
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
