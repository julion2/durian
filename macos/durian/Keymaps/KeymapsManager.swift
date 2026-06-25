import Combine
import Foundation
import SwiftUI

// MARK: - Notifications

extension Notification.Name {
    static let keymapsDidChange = Notification.Name("keymapsDidChange")
}

class KeymapsManager: ObservableObject {
    static let shared = KeymapsManager()

    @Published var keymaps: KeymapConfig = KeymapConfig()
    private var cancellables = Set<AnyCancellable>()

    private init() {
        loadKeymapsBlocking()
    }

    private func loadKeymapsBlocking() {
        let pklURL = getKeymapsURL()

        guard FileManager.default.fileExists(atPath: pklURL.path) else {
            Log.warning("KEYMAPS", "keymaps.pkl not found, using defaults")
            keymaps = KeymapConfig()
            keymaps.keymaps = getDefaultKeymaps()
            NotificationCenter.default.post(name: .keymapsDidChange, object: nil)
            return
        }

        do {
            keymaps = try PklEvaluator.evalSync(KeymapConfig.self, from: pklURL)
            keymaps.keymaps = mergeWithDefaults(userKeymaps: keymaps.keymaps)
            Log.info("KEYMAPS", "Loaded \(keymaps.keymaps.count) keymaps (merged with defaults)")
        } catch {
            Log.error("KEYMAPS", "Failed to load: \(error)")
            keymaps = KeymapConfig()
            keymaps.keymaps = getDefaultKeymaps()
        }

        NotificationCenter.default.post(name: .keymapsDidChange, object: nil)
    }

    /// Returns the default keymaps array
    private func getDefaultKeymaps() -> [KeymapEntry] {
        [
            // Navigation
            KeymapEntry(action: "next_email", key: "j", supportsCount: true),
            KeymapEntry(action: "prev_email", key: "k", supportsCount: true),
            KeymapEntry(action: "next_email", key: "Down"),
            KeymapEntry(action: "prev_email", key: "Up"),
            KeymapEntry(action: "last_email", key: "G"),
            KeymapEntry(action: "first_email", key: "gg", sequence: true),
            KeymapEntry(action: "page_down", key: "d", modifiers: ["ctrl"], supportsCount: true),
            KeymapEntry(action: "page_up", key: "u", modifiers: ["ctrl"], supportsCount: true),
            KeymapEntry(action: "archive", key: "a"),
            KeymapEntry(action: "compose", key: "c"),
            KeymapEntry(action: "reply", key: "r"),
            KeymapEntry(action: "reply_all", key: "R"),
            KeymapEntry(action: "forward", key: "f"),
            KeymapEntry(action: "toggle_read", key: "u"),
            KeymapEntry(action: "toggle_star", key: "s"),
            KeymapEntry(action: "delete", key: "dd", sequence: true, supportsCount: true),
            // Folder Navigation
            KeymapEntry(action: "go_inbox", key: "gi", sequence: true),
            KeymapEntry(action: "go_sent", key: "gs", sequence: true),
            KeymapEntry(action: "go_drafts", key: "gd", sequence: true),
            KeymapEntry(action: "go_archive", key: "ga", sequence: true),
            KeymapEntry(action: "go_folder", key: "g1", sequence: true),
            KeymapEntry(action: "go_folder", key: "g2", sequence: true),
            KeymapEntry(action: "go_folder", key: "g3", sequence: true),
            KeymapEntry(action: "go_folder", key: "g4", sequence: true),
            KeymapEntry(action: "go_folder", key: "g5", sequence: true),
            KeymapEntry(action: "go_folder", key: "g6", sequence: true),
            KeymapEntry(action: "go_folder", key: "g7", sequence: true),
            KeymapEntry(action: "go_folder", key: "g8", sequence: true),
            KeymapEntry(action: "go_folder", key: "g9", sequence: true),
            KeymapEntry(action: "next_folder", key: "J"),
            KeymapEntry(action: "prev_folder", key: "K"),
            KeymapEntry(action: "folder_picker", key: "gf", sequence: true),
            // Search
            KeymapEntry(action: "search", key: "/"),
            KeymapEntry(action: "search", key: "/", modifiers: ["cmd"]),
            // Tag Picker
            KeymapEntry(action: "tag_picker", key: "t"),
            // View Control
            KeymapEntry(action: "close_detail", key: "q"),
            KeymapEntry(action: "close_detail", key: "Escape"),
            KeymapEntry(action: "reload_inbox", key: "r", modifiers: ["cmd"]),
            // Visual Mode
            KeymapEntry(action: "enter_visual_mode", key: "v"),
            KeymapEntry(action: "enter_toggle_mode", key: "V"),
            KeymapEntry(action: "toggle_selection", key: " "),
            KeymapEntry(action: "exit_visual_mode", key: "Escape"),
            // Search context
            KeymapEntry(action: "select_next", key: "j", modifiers: ["ctrl"], context: "search"),
            KeymapEntry(action: "select_prev", key: "k", modifiers: ["ctrl"], context: "search"),
            KeymapEntry(action: "select_next", key: "n", modifiers: ["ctrl"], context: "search"),
            KeymapEntry(action: "select_prev", key: "p", modifiers: ["ctrl"], context: "search"),
            // Tag picker context
            KeymapEntry(action: "select_next", key: "j", modifiers: ["ctrl"], context: "tag_picker"),
            KeymapEntry(action: "select_prev", key: "k", modifiers: ["ctrl"], context: "tag_picker"),
            KeymapEntry(action: "select_next", key: "n", modifiers: ["ctrl"], context: "tag_picker"),
            KeymapEntry(action: "select_prev", key: "p", modifiers: ["ctrl"], context: "tag_picker"),
            // Compose normal context
            KeymapEntry(action: "exit_insert", key: "jk", sequence: true, context: "compose_normal"),
            // List context: enter thread
            KeymapEntry(action: "enter_thread", key: "l"),
            // Thread context
            KeymapEntry(action: "scroll_down", key: "j", supportsCount: true, context: "thread"),
            KeymapEntry(action: "scroll_up", key: "k", supportsCount: true, context: "thread"),
            KeymapEntry(action: "page_down", key: "d", modifiers: ["ctrl"], supportsCount: true, context: "thread"),
            KeymapEntry(action: "page_up", key: "u", modifiers: ["ctrl"], supportsCount: true, context: "thread"),
            KeymapEntry(action: "next_message", key: "n", supportsCount: true, context: "thread"),
            KeymapEntry(action: "prev_message", key: "N", supportsCount: true, context: "thread"),
            KeymapEntry(action: "first_email", key: "gg", sequence: true, context: "thread"),
            KeymapEntry(action: "last_email", key: "G", context: "thread"),
            KeymapEntry(action: "close_detail", key: "h", context: "thread"),
            KeymapEntry(action: "close_detail", key: "Escape", context: "thread"),
            KeymapEntry(action: "reply", key: "r", context: "thread"),
        ]
    }

    /// Merges user-defined keymaps with the built-in defaults.
    /// User entries override defaults that share the same (key + modifiers + context).
    /// Entries with enabled=false are removed from the final result.
    func mergeWithDefaults(userKeymaps: [KeymapEntry]) -> [KeymapEntry] {
        let userByKey = Dictionary(userKeymaps.map { ($0.bindingKey, $0) }, uniquingKeysWith: { _, last in last })
        var seen = Set<String>()

        // Defaults first — replaced by user entry if same bindingKey exists
        var merged = getDefaultKeymaps().map { entry -> KeymapEntry in
            seen.insert(entry.bindingKey)
            return userByKey[entry.bindingKey] ?? entry
        }

        // Append user entries that don't overlap with any default
        for entry in userKeymaps where !seen.contains(entry.bindingKey) {
            merged.append(entry)
        }

        return merged.filter { $0.enabled }
    }

    private func getKeymapsURL() -> URL {
        FileManager.default.durianConfigURL().appendingPathComponent("keymaps.pkl")
    }


    // MARK: - Public API

    func setKeymap(for action: String, key: String, modifiers: [String] = []) {
        if let index = keymaps.keymaps.firstIndex(where: { $0.action == action }) {
            keymaps.keymaps[index].key = key
            keymaps.keymaps[index].modifiers = modifiers
            Log.debug("KEYMAPS", "Set \(action) = \(formatKeymap(key: key, modifiers: modifiers))")
        }
    }

    func getKeymap(for action: String) -> KeymapEntry? {
        keymaps.keymaps.first(where: { $0.action == action })
    }

    func getKeymapsForAction(_ action: String) -> [KeymapEntry] {
        keymaps.keymaps.filter { $0.action == action }
    }

    func isKeymapPressed(key: String, modifiers: [String], for action: String) -> Bool {
        guard keymaps.globalSettings.keymapsEnabled else {
            return false
        }

        let matchingKeymaps = keymaps.keymaps.filter {
            $0.action == action
        }

        return matchingKeymaps.contains { keymap in
            keymap.key.lowercased() == key.lowercased() &&
            Set(keymap.modifiers) == Set(modifiers)
        }
    }

    private func formatKeymap(key: String, modifiers: [String]) -> String {
        let modString = modifiers.isEmpty ? "" : modifiers.joined(separator: "+") + "+"
        return modString + key
    }

    // Public method to manually reload keymaps
    func reloadKeymaps() {
        Log.info("KEYMAPS", "Manual keymaps reload requested")
        loadKeymapsBlocking()
    }
}

struct KeymapConfig: Codable {
    var keymaps: [KeymapEntry]
    var globalSettings: KeymapGlobalSettings

    init() {
        keymaps = []
        globalSettings = KeymapGlobalSettings()
    }

    enum CodingKeys: String, CodingKey {
        case keymaps
        case globalSettings = "global_settings"
    }
}

struct KeymapEntry: Codable {
    var action: String
    var key: String
    var modifiers: [String]
    var enabled: Bool
    var sequence: Bool
    var supportsCount: Bool
    var context: String
    var tags: String?  // For tag_op action: "+todo -inbox"

    enum CodingKeys: String, CodingKey {
        case action, key, modifiers, enabled, sequence
        case supportsCount = "supports_count"
        case context, tags
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        action = try container.decode(String.self, forKey: .action)
        key = try container.decode(String.self, forKey: .key)
        modifiers = try container.decodeIfPresent([String].self, forKey: .modifiers) ?? []
        enabled = try container.decodeIfPresent(Bool.self, forKey: .enabled) ?? true
        sequence = try container.decodeIfPresent(Bool.self, forKey: .sequence) ?? false
        supportsCount = try container.decodeIfPresent(Bool.self, forKey: .supportsCount) ?? false
        context = try container.decodeIfPresent(String.self, forKey: .context) ?? "list"
        tags = try container.decodeIfPresent(String.self, forKey: .tags)
    }

    init(action: String, key: String, modifiers: [String] = [], enabled: Bool = true, sequence: Bool = false, supportsCount: Bool = false, context: String = "list", tags: String? = nil) {
        self.action = action
        self.key = key
        self.modifiers = modifiers
        self.enabled = enabled
        self.sequence = sequence
        self.supportsCount = supportsCount
        self.context = context
        self.tags = tags
    }

    /// The identity used for merging: same key + modifiers + context = same binding slot.
    var bindingKey: String {
        let mods = modifiers.sorted().joined(separator: "+")
        return "\(key)|\(mods)|\(context)"
    }
}

struct KeymapGlobalSettings: Codable {
    var keymapsEnabled: Bool = true
    var sequenceTimeout: Double = 1.0

    enum CodingKeys: String, CodingKey {
        case keymapsEnabled = "keymaps_enabled"
        case sequenceTimeout = "sequence_timeout"
    }

    init() {}

    init(keymapsEnabled: Bool = true, sequenceTimeout: Double = 1.0) {
        self.keymapsEnabled = keymapsEnabled
        self.sequenceTimeout = sequenceTimeout
    }

    // Custom init for backwards compatibility
    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        keymapsEnabled = try container.decodeIfPresent(Bool.self, forKey: .keymapsEnabled) ?? true
        sequenceTimeout = try container.decodeIfPresent(Double.self, forKey: .sequenceTimeout) ?? 1.0
    }
}
