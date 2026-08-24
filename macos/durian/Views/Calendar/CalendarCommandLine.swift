//
//  CalendarCommandLine.swift
//  Durian
//
//  The `:` line, in the place the echo line otherwise occupies — vim puts
//  the status and the command on the same row for the same reason: the
//  command is a temporary state of the status, not a second surface.
//
//  Everything to the right of the input is feedback on what the current text
//  would do. That readout is the answer to the discoverability problem that
//  sinks keyboard-first calendars: you learn the grammar by watching it
//  resolve, instead of by finding a manual.
//

import SwiftUI

struct CalendarCommandLine: View {
    @ObservedObject var manager = CalendarManager.shared
    @FocusState private var focused: Bool

    // MARK: - Body

    var body: some View {
        HStack(spacing: 0) {
            Text(":")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(Color.Detail.textSecondary)
                .padding(.trailing, 2)

            TextField("", text: $manager.commandText)
                .textFieldStyle(.plain)
                .font(.system(size: 12, design: .monospaced))
                .foregroundStyle(Color.Detail.textPrimary)
                .accessibilityLabel("Calendar command")
                .focused($focused)
                .onSubmit { manager.runCommand() }
                .onKeyPress(.escape) {
                    manager.closeCommandLine()
                    return .handled
                }

            Spacer(minLength: 12)

            feedback
        }
        .padding(.horizontal, 12)
        .frame(height: 24)
        .background(Color(nsColor: .windowBackgroundColor))
        .onAppear {
            // Focus asserted a tick late: requested while the view is still
            // being installed it is dropped — the same delay the search, tag
            // and folder popups need.
            DispatchQueue.main.async { focused = true }
        }
    }

    // MARK: - Feedback

    /// The resolved meaning of what is typed, or the reason it does not
    /// resolve. Never empty while there is text, so the line always answers.
    @ViewBuilder
    private var feedback: some View {
        switch manager.commandPreview {
        case .none:
            Text("new · today · week · month · year · agenda · modify · delete")
                .foregroundStyle(Color.Detail.textTertiary)
                .font(.system(size: 11))
                .lineLimit(1)
        case .invalid(let reason):
            Label(reason, systemImage: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
                .font(.system(size: 11))
                .lineLimit(1)
        case .create(let start, let end, let title, _, _):
            HStack(spacing: 6) {
                Image(systemName: "return")
                Text(title).fontWeight(.medium)
                Text("·")
                Text("\(Self.day.string(from: start)) \(CalendarTimeFormat.time(start))–\(CalendarTimeFormat.time(end))")
                    .monospacedDigit()
            }
            .font(.system(size: 11))
            .foregroundStyle(Color.Detail.textSecondary)
            .lineLimit(1)
        case .modifySelected(let patch):
            HStack(spacing: 6) {
                Image(systemName: "return")
                Text(patch.title ?? manager.selectedEvent?.displaySubject ?? "Selected event")
                    .fontWeight(.medium)
                Text("·")
                Text("\(Self.day.string(from: patch.start)) \(CalendarTimeFormat.time(patch.start))–\(CalendarTimeFormat.time(patch.end))")
                    .monospacedDigit()
            }
            .font(.system(size: 11))
            .foregroundStyle(Color.Detail.textSecondary)
            .lineLimit(1)
        case .goToday:
            hint("Jump to now")
        case .setView(let mode):
            hint("Switch to \(mode.title)")
        case .editSelected:
            hint("Modify the selected event")
        case .deleteSelected:
            hint("Delete the selected event")
        }
    }

    private func hint(_ text: String) -> some View {
        Label(text, systemImage: "return")
            .font(.system(size: 11))
            .foregroundStyle(Color.Detail.textSecondary)
            .lineLimit(1)
    }

    // MARK: - Formatting

    private static let day: DateFormatter = {
        let f = DateFormatter()
        f.setLocalizedDateFormatFromTemplate("EEE d MMM")
        return f
    }()
}
