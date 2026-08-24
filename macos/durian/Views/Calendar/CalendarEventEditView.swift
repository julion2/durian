//
//  CalendarEventEditView.swift
//  Durian
//
//  Create/edit form shown in the floating event card. Local-first:
//  saving writes the local .ics via the API; nothing is sent to Outlook until
//  the next sync.
//
//  Laid out flush, matching CalendarEventDetailView: the form's fields are
//  mostly one line each, and a bordered card around a single line adds a
//  border, a radius and a heading to group something that proximity already
//  groups. A leading glyph per row names the kind of field, exactly as the
//  detail card's fact rows do — the two surfaces show the same event, so they
//  should read as the same surface.
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
                VStack(alignment: .leading, spacing: 0) {
                    header

                    Divider().padding(.vertical, 14)

                    VStack(alignment: .leading, spacing: 10) {
                        locationRow
                        calendarRow
                    }

                    timeSection

                    if draft.canEditAttendees {
                        meetingSection
                    }

                    notesSection

                    Spacer(minLength: 0)
                }
                .padding(18)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(nsColor: .windowBackgroundColor))
        // Escape cancels the edit. Catch the exit command on the card itself;
        // there is deliberately no duplicate visible Cancel button.
        .onExitCommand { onCancel() }
        .onAppear {
            // Deferred one tick: focus requested while the view is still being
            // installed is dropped.
            if draft.isNew {
                DispatchQueue.main.async { titleFocused = true }
            }
        }
    }

    // MARK: - Header

    private var headerBar: some View {
        ZStack {
            Text(draft.isNew ? "New Event" : "Edit Event")
                .font(.headline)
                .foregroundStyle(Color.Detail.textPrimary)
            HStack {
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

    // MARK: - Title header

    /// The title beside the calendar's color bar — the same shape the detail
    /// pane's header has, so opening the editor keeps the event's anchor in
    /// place.
    private var header: some View {
        HStack(alignment: .top, spacing: 10) {
            RoundedRectangle(cornerRadius: 2)
                .fill(selectedCalendarColor)
                .frame(width: 4)
                .frame(maxHeight: .infinity)

            TextField("Title", text: $draft.subject)
                .textFieldStyle(.plain)
                .font(.title3).fontWeight(.semibold)
                .foregroundStyle(Color.Detail.textPrimary)
                .focused($titleFocused)
        }
        .fixedSize(horizontal: false, vertical: true)
    }

    // MARK: - Rows

    private var locationRow: some View {
        fieldRow("mappin.and.ellipse") {
            TextField("Location", text: $draft.location)
                .textFieldStyle(.plain)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
        }
    }

    private var calendarRow: some View {
        fieldRow("calendar") {
            Circle()
                .fill(selectedCalendarColor)
                .frame(width: 8, height: 8)
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

    private var timeSection: some View {
        section("Time") {
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
            dateRow("Ends", selection: $draft.end)

            if draft.recurring {
                Label("Recurring event — saving changes the whole series.",
                      systemImage: "repeat")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
        }
    }

    /// Attendees for an owned meeting, plus the online-meeting request for a
    /// NEW event. Saving only writes the local .ics; attendee notifications go
    /// out on the next manual `durian calendar sync`, after its preview.
    private var meetingSection: some View {
        section("Meeting") {
            if draft.isNew {
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
            }

            ForEach(draft.attendees, id: \.self) { email in
                fieldRow("person") {
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

            fieldRow("person.badge.plus") {
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

            if draft.attendeesChanged || draft.requestOnlineMeeting {
                Label("Saved locally first — attendee updates are sent when you run 'durian calendar sync' (automatic sync skips them).",
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

    private var notesSection: some View {
        section("Notes") {
            TextEditor(text: $draft.description)
                .frame(minHeight: 100)
                .font(.callout)
                .foregroundStyle(Color.Detail.textBody)
                .scrollContentBackground(.hidden)
                // Cancel the editor's built-in text inset so the notes text
                // left-aligns with the section heading and the other fields.
                .padding(.horizontal, -5)
        }
    }

    // MARK: - Styling helpers

    /// One field: a glyph naming its kind, then the input. The glyph replaces
    /// the caption heading a card would have needed — the same recipe as the
    /// detail card's factRow.
    private func fieldRow<Content: View>(_ symbol: String,
                                         @ViewBuilder content: () -> Content) -> some View
    {
        HStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 12))
                .foregroundStyle(Color.Detail.textTertiary)
                .frame(width: 16)
            content()
        }
    }

    /// A titled flush group, identical in treatment to the detail card's
    /// sections: a caption heading over the rows, no border, no fill.
    private func section<Content: View>(_ title: String,
                                        @ViewBuilder content: () -> Content) -> some View
    {
        VStack(alignment: .leading, spacing: 8) {
            Text(title)
                .font(.caption).fontWeight(.semibold)
                .foregroundStyle(Color.Detail.textTertiary)
            content()
        }
        .padding(.top, 20)
        .frame(maxWidth: .infinity, alignment: .leading)
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
                // Local-only calendars never reach a provider. If the user
                // switches to one after entering meeting details, discard
                // those hidden values rather than submitting a request the
                // API must reject.
                if match.account == "local" {
                    draft.attendees = []
                    draft.requestOnlineMeeting = false
                }
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
