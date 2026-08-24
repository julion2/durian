//
//  KeymapTypes.swift
//  Durian
//
//  Key sequence types and action definitions
//

import Foundation

// MARK: - KeymapContext

/// Context determines which keymap bindings are active
enum KeymapContext: String, CaseIterable, Hashable {
    case list = "list"
    case search = "search"
    case tagPicker = "tag_picker"
    case thread = "thread"
    case composeNormal = "compose_normal"
    case calendar = "calendar"
}

// MARK: - KeymapAction Enum

/// All available actions that can be triggered by keymaps
enum KeymapAction: String, CaseIterable {
    // Navigation
    case nextEmail = "next_email"
    case prevEmail = "prev_email"
    case firstEmail = "first_email"
    case lastEmail = "last_email"
    case pageDown = "page_down"
    case pageUp = "page_up"

    // Email Actions
    case compose = "compose"
    case reply = "reply"
    case replyAll = "reply_all"
    case forward = "forward"
    case deleteEmail = "delete"
    case archiveEmail = "archive"
    case tagOp = "tag_op"
    case toggleRead = "toggle_read"
    case toggleStar = "toggle_star"

    // View Control
    case closeDetail = "close_detail"
    case reloadInbox = "reload_inbox"
    case search = "search"

    // Tag Picker
    case tagPicker = "tag_picker"

    // Folder Navigation
    case goInbox = "go_inbox"
    case goSent = "go_sent"
    case goDrafts = "go_drafts"
    case goArchive = "go_archive"
    case goFolder = "go_folder"
    case nextFolder = "next_folder"
    case prevFolder = "prev_folder"
    case folderPicker = "folder_picker"

    // Visual Mode
    case enterVisualMode = "enter_visual_mode"           // v - line mode (range selection)
    case enterToggleMode = "enter_toggle_mode"           // V - toggle mode (individual selection)
    case toggleSelection = "toggle_selection"            // Space - toggle current (only in toggle mode)
    case exitVisualMode = "exit_visual_mode"             // Escape in visual mode

    // Popup Navigation (search, tag picker)
    case selectNext = "select_next"
    case selectPrev = "select_prev"
    case confirmSelection = "confirm_selection"
    case closePopup = "close_popup"
    case exitInsert = "exit_insert"
    case enterThread = "enter_thread"
    case nextMessage = "next_message"
    case prevMessage = "prev_message"
    case scrollDown = "scroll_down"
    case scrollUp = "scroll_up"

    // Calendar
    case goCalendar = "go_calendar"
    case calendarToday = "calendar_today"
    case calendarPrevPeriod = "calendar_prev_period"
    case calendarNextPeriod = "calendar_next_period"
    case calendarEdit = "calendar_edit"
    case calendarNew = "calendar_new"
    case calendarDelete = "calendar_delete"
    case calendarToggleSidebar = "calendar_toggle_sidebar"
    case calendarToggleDeclined = "calendar_toggle_declined"
    /// h/l: move the time cursor a day, keeping the time of day.
    case calendarPrevDay = "calendar_prev_day"
    case calendarNextDay = "calendar_next_day"
    /// Opens the full-detail card for the event under the cursor.
    case calendarDetail = "calendar_detail"
    /// Opens the `:` command line.
    case calendarCommand = "calendar_command"

    // Note: supportsCount is now defined in keymaps.pkl per-action
    // Use SequenceMatcher.shared.supportsCount(action) to check
}

// MARK: - KeyEvent

/// Represents a single key press event
struct KeyEvent: Equatable {
    let key: String
    let modifiers: Set<KeyModifier>
    let timestamp: Date

    init(key: String, modifiers: Set<KeyModifier> = [], timestamp: Date = Date()) {
        self.key = key
        self.modifiers = modifiers
        self.timestamp = timestamp
    }

    /// String representation for matching (e.g., "G", ":" or "ctrl+d")
    var normalized: String {
        if modifiers.isEmpty {
            // Lowercasing neutralizes Caps Lock for single characters; named
            // keys ("Tab", "Enter") must keep their exact spelling to match
            // binding strings.
            return key.count > 1 ? key : key.lowercased()
        }

        // Shift alone: AppKit has already applied Shift to the character, so
        // the key carries the final glyph on any layout ("G" for Shift+g,
        // ":" for whatever key the layout puts colon on). Letters are
        // uppercased explicitly so Caps Lock combinations still normalize to
        // the uppercase binding form; everything else is the glyph itself.
        if modifiers == [.shift] && key.count == 1 {
            return key.first?.isLetter == true ? key.uppercased() : key
        }

        let modStr = modifiers.sorted(by: { $0.rawValue < $1.rawValue })
            .map { $0.rawValue }
            .joined(separator: "+")
        return "\(modStr)+\(key.lowercased())"
    }
}

// MARK: - KeyModifier

/// Keyboard modifiers
enum KeyModifier: String, Comparable {
    case cmd = "cmd"
    case ctrl = "ctrl"
    case option = "option"
    case shift = "shift"

    static func < (lhs: KeyModifier, rhs: KeyModifier) -> Bool {
        lhs.rawValue < rhs.rawValue
    }
}

// MARK: - MatchResult

/// Result of attempting to match a key sequence
enum SequenceMatchResult: Equatable {
    /// No match found - clear buffer
    case noMatch

    /// Partial match - waiting for more keys (e.g., "g" waiting for "g")
    case partial

    /// Full match found
    case match(action: KeymapAction, count: Int)

    static func == (lhs: SequenceMatchResult, rhs: SequenceMatchResult) -> Bool {
        switch (lhs, rhs) {
        case (.noMatch, .noMatch):
            return true
        case (.partial, .partial):
            return true
        case (.match(let a1, let c1), .match(let a2, let c2)):
            return a1 == a2 && c1 == c2
        default:
            return false
        }
    }
}

// MARK: - SequenceDefinition

/// Defines a key sequence and its action
struct SequenceDefinition {
    let sequence: String      // e.g., "gg", "dd", "j"
    let action: KeymapAction

    init(_ sequence: String, _ action: KeymapAction) {
        self.sequence = sequence
        self.action = action
    }
}
