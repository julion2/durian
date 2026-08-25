//
//  CalendarEventEditView.swift
//  Durian
//
//  Create/edit form shown in the floating event card. A normal event keeps the
//  keyboard-first Save action; a meeting exposes the one irreversible action
//  explicitly as Send / Send Update.
//
//  Laid out flush, matching CalendarEventDetailView: the form's fields are
//  mostly one line each, and a bordered card around a single line adds a
//  border, a radius and a heading to group something that proximity already
//  groups. A leading glyph per row names the kind of field, exactly as the
//  detail card's fact rows do — the two surfaces show the same event, so they
//  should read as the same surface.
//

import SwiftUI

import AppKit

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
                if willSendNotifications {
                    Button(draft.isNew ? "Send" : "Send Update", action: commit)
                        .keyboardShortcut(.defaultAction)
                        .buttonStyle(.borderedProminent)
                        .disabled(!isValid)
                } else {
                    // Keep Return as the normal Save action without adding a
                    // permanent button to a keyboard-first editor. Meetings
                    // alone surface a visible action because they email people.
                    Button("Save", action: commit)
                        .keyboardShortcut(.defaultAction)
                        .frame(width: 0, height: 0)
                        .opacity(0)
                        .accessibilityHidden(true)
                        .disabled(!isValid)
                }
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
            HStack(spacing: 10) {
                Image(systemName: draft.allDay ? "sun.max" : "clock")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundStyle(selectedCalendarColor)
                    .frame(width: 16)
                Text("All day")
                    .font(.callout)
                    .foregroundStyle(Color.Detail.textBody)
                Spacer()
                Toggle("All day", isOn: $draft.allDay)
                    .labelsHidden()
                    .toggleStyle(.switch)
                    .controlSize(.small)
            }

            VStack(spacing: 0) {
                dateRow("Starts", selection: startSelection, isStart: true)

                Divider()
                    .padding(.leading, 30)

                dateRow("Ends", selection: $draft.end, isStart: false)
            }
            .background(
                RoundedRectangle(cornerRadius: 10)
                    .fill(Color.Detail.cardBackground)
            )
            .overlay {
                RoundedRectangle(cornerRadius: 10)
                    .strokeBorder(Color.Detail.border.opacity(0.7), lineWidth: 0.5)
                    .allowsHitTesting(false)
            }

            if !draft.allDay {
                durationPresets
            }

            if draft.recurring {
                Label("Recurring event — saving changes the whole series.",
                      systemImage: "repeat")
                    .font(.caption)
                    .foregroundStyle(Color.Detail.textSecondary)
            }
        }
    }

    /// Attendees for an owned meeting, plus the online-meeting request for a
    /// NEW event. A meeting's header action says Send rather than hiding the
    /// external effect behind a generic Save button.
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

            fieldRow("person.badge.plus") {
                ContactTokenField(
                    tokens: $draft.attendees,
                    contactToken: { $0.email },
                    isValidToken: EmailTokenHelper.isValidEmail,
                    wrapsTokens: true,
                    tokenFieldAccessibilityLabel: "Attendees",
                    onPartialTextChange: { attendeeInput = $0 }
                )
                .frame(minHeight: 24)
                if !trimmedAttendeeInput.isEmpty,
                   !EmailTokenHelper.isValidEmail(trimmedAttendeeInput)
                {
                    Image(systemName: "exclamationmark.circle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .help("Not a valid email address")
                        .accessibilityLabel("Not a valid email address")
                }
            }

            if !invalidAttendees.isEmpty {
                Text("Remove or correct invalid attendee: \(invalidAttendees.joined(separator: ", "))")
                    .font(.caption)
                    .foregroundStyle(.orange)
                    .accessibilityLabel("Invalid attendees: \(invalidAttendees.joined(separator: ", "))")
            }

            if willSendNotifications {
                Label(draft.isNew ? "Sending will invite these attendees." : "Sending will notify the meeting attendees.",
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

    private var invalidAttendees: [String] {
        draft.attendees.filter { !EmailTokenHelper.isValidEmail($0) }
    }

    /// Commits pending attendee text for mouse-driven saves. Keyboard commits
    /// are handled by NSTokenField; normalization and dedup keep both paths
    /// equivalent.
    private func addAttendee() {
        let email = EmailTokenHelper.cleanEmail(trimmedAttendeeInput)
        guard EmailTokenHelper.isValidEmail(email) else { return }
        guard !draft.attendees.contains(where: {
            EmailTokenHelper.cleanEmail($0).caseInsensitiveCompare(email) == .orderedSame
        }) else {
            attendeeInput = ""
            return
        }
        draft.attendees.append(email)
        attendeeInput = ""
    }

    private func commit() {
        // A typed-but-not-committed attendee email still counts.
        addAttendee()
        draft.attendees = draft.attendees.reduce(into: []) { result, attendee in
            let email = EmailTokenHelper.cleanEmail(attendee)
            guard EmailTokenHelper.isValidEmail(email),
                  !result.contains(where: { $0.caseInsensitiveCompare(email) == .orderedSame }) else { return }
            result.append(email)
        }
        onSave(draft)
    }

    private var willSendNotifications: Bool {
        draft.sendsNotifications
            || (!trimmedAttendeeInput.isEmpty && EmailTokenHelper.isValidEmail(trimmedAttendeeInput))
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

    /// One stop on the event timeline. Separate controls make both keyboard
    /// editing and mouse selection predictable; the color rail preserves the
    /// relationship between start and end without another heading or label.
    private func dateRow(_ label: String, selection: Binding<Date>, isStart: Bool) -> some View {
        HStack(spacing: 10) {
            ZStack {
                Rectangle()
                    .fill(selectedCalendarColor.opacity(0.35))
                    .frame(width: 1)
                    .frame(maxHeight: .infinity)
                Circle()
                    .fill(isStart ? selectedCalendarColor : Color.Detail.cardBackground)
                    .overlay {
                        Circle().strokeBorder(selectedCalendarColor, lineWidth: 1.5)
                    }
                    .frame(width: 9, height: 9)
            }
            .frame(width: 18)

            Text(label)
                .font(.caption)
                .fontWeight(.medium)
                .foregroundStyle(Color.Detail.textSecondary)
                .frame(width: 42, alignment: .leading)

            Spacer(minLength: 6)

            CalendarDateField(
                selection: selection,
                elements: [.yearMonthDay],
                accessibilityLabel: "\(label) date"
            )
            .frame(width: 116, height: 22)

            if !draft.allDay {
                CalendarDateField(
                    selection: selection,
                    elements: [.hourMinute],
                    accessibilityLabel: "\(label) time"
                )
                .frame(width: 64, height: 22)
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 9)
    }

    /// Common meeting lengths stay one click away. Any manually entered end
    /// remains valid and simply leaves every preset unselected.
    private var durationPresets: some View {
        HStack(spacing: 4) {
            Text("Duration")
                .font(.caption)
                .foregroundStyle(Color.Detail.textTertiary)

            Spacer(minLength: 6)

            ForEach(Self.durationOptions, id: \.seconds) { option in
                let selected = abs(draft.end.timeIntervalSince(draft.start) - option.seconds) < 1
                Button {
                    draft.end = draft.start.addingTimeInterval(option.seconds)
                } label: {
                    Text(option.label)
                        .font(.caption)
                        .fontWeight(selected ? .semibold : .regular)
                        .foregroundStyle(selected ? selectedCalendarColor : Color.Detail.textSecondary)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 3)
                        .background(
                            Capsule()
                                .fill(selected ? selectedCalendarColor.opacity(0.14) : .clear)
                        )
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Set duration to \(option.accessibilityLabel)")
            }
        }
    }

    private static let durationOptions: [(label: String, accessibilityLabel: String, seconds: TimeInterval)] = [
        ("15m", "15 minutes", 15 * 60),
        ("30m", "30 minutes", 30 * 60),
        ("1h", "1 hour", 60 * 60),
        ("1.5h", "1 hour 30 minutes", 90 * 60),
        ("2h", "2 hours", 2 * 60 * 60),
    ]

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
                    attendeeInput = ""
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

    /// Changing the start keeps the existing duration. This matches how users
    /// move an event: choosing a new start should not unexpectedly shorten it
    /// or leave the end behind on the previous day.
    private var startSelection: Binding<Date> {
        Binding(
            get: { draft.start },
            set: { newStart in
                let duration = draft.end.timeIntervalSince(draft.start)
                draft.start = newStart
                draft.end = newStart.addingTimeInterval(duration)
            }
        )
    }

    private var isValid: Bool {
        guard !draft.subject.trimmingCharacters(in: .whitespaces).isEmpty else { return false }
        guard trimmedAttendeeInput.isEmpty
            || EmailTokenHelper.isValidEmail(trimmedAttendeeInput) else { return false }
        guard invalidAttendees.isEmpty else { return false }
        if draft.allDay {
            // A same-day all-day event is valid — the write path snaps it to a
            // full day. Only an end day before the start day is invalid.
            let cal = Calendar.current
            return cal.startOfDay(for: draft.end) >= cal.startOfDay(for: draft.start)
        }
        return draft.end > draft.start
    }
}

/// A native date field without AppKit's bezel. Unlike SwiftUI's compact
/// DatePicker, its individual date/time components accept direct keyboard
/// input while preserving the other components of the bound Date.
private struct CalendarDateField: NSViewRepresentable {
    @Binding var selection: Date
    let elements: NSDatePicker.ElementFlags
    let accessibilityLabel: String

    func makeCoordinator() -> Coordinator {
        Coordinator(self)
    }

    func makeNSView(context: Context) -> NSDatePicker {
        let picker = NSDatePicker()
        picker.datePickerStyle = .textField
        picker.datePickerElements = elements
        picker.presentsCalendarOverlay = elements.contains(.yearMonthDay)
        picker.isBordered = false
        picker.drawsBackground = false
        picker.font = .systemFont(ofSize: 13)
        picker.dateValue = selection
        picker.target = context.coordinator
        picker.action = #selector(Coordinator.dateChanged(_:))
        picker.setAccessibilityLabel(accessibilityLabel)
        return picker
    }

    func updateNSView(_ picker: NSDatePicker, context: Context) {
        context.coordinator.parent = self
        picker.datePickerElements = elements
        picker.presentsCalendarOverlay = elements.contains(.yearMonthDay)
        picker.setAccessibilityLabel(accessibilityLabel)
        if picker.dateValue != selection {
            picker.dateValue = selection
        }
    }

    final class Coordinator: NSObject {
        var parent: CalendarDateField

        init(_ parent: CalendarDateField) {
            self.parent = parent
        }

        @objc func dateChanged(_ sender: NSDatePicker) {
            parent.selection = sender.dateValue
        }
    }
}
