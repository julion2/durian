//
//  SequenceMatcher.swift
//  Durian
//
//  Pattern matching for key sequences - dynamically loaded from keymaps.pkl
//

import Foundation

/// Matches key buffer contents against defined sequences
class SequenceMatcher {

    // MARK: - Singleton

    static let shared = SequenceMatcher()

    // MARK: - Dynamic Sequence Storage

    /// All defined key sequences (loaded from config), grouped by context
    private var sequences: [SequenceDefinition] = []

    /// Per-context lookup by sequence string
    private var contextSequenceLookup: [KeymapContext: [String: KeymapAction]] = [:]

    /// Per-context prefixes for partial matching
    private var contextPrefixes: [KeymapContext: Set<String>] = [:]

    /// Per-context actions that support count prefix
    private var contextCountSupported: [KeymapContext: Set<KeymapAction>] = [:]

    /// Per-context tag_op tags lookup: sequence → tags string (e.g. "+todo -inbox")
    private var contextTagOps: [KeymapContext: [String: String]] = [:]

    // MARK: - Init

    private init() {
        reloadFromConfig()

        // Observe config changes
        NotificationCenter.default.addObserver(
            self,
            selector: #selector(configDidChange),
            name: .keymapsDidChange,
            object: nil
        )
    }

    deinit {
        NotificationCenter.default.removeObserver(self)
    }

    // MARK: - Config Loading

    @objc private func configDidChange() {
        reloadFromConfig()
    }

    /// Reload sequences from KeymapsManager config
    func reloadFromConfig() {
        let keymapEntries = KeymapsManager.shared.keymaps.keymaps

        // Build sequences from config, tagged with context
        // Entries without modifiers use key directly (vim-style: "j", "gg", "gi")
        // Entries with ctrl modifier use normalized form ("ctrl+d", "ctrl+u")
        // Other modifier entries (Cmd+r, etc.) are handled by KeymapHandler.handleLegacyKeymap()
        sequences = keymapEntries
            .filter { $0.modifiers.isEmpty || $0.modifiers == ["ctrl"] }
            .compactMap { entry -> SequenceDefinition? in
                guard let action = KeymapAction(rawValue: entry.action) else {
                    Log.debug("SEQMATCH", "Unknown action '\(entry.action)' - skipping")
                    return nil
                }
                let seqKey = entry.modifiers == ["ctrl"] ? "ctrl+\(entry.key.lowercased())" : entry.key
                return SequenceDefinition(seqKey, action)
            }

        // Group entries by context and build per-context lookups
        contextSequenceLookup = [:]
        contextPrefixes = [:]
        contextCountSupported = [:]
        contextTagOps = [:]

        for context in KeymapContext.allCases {
            let contextEntries = keymapEntries.filter {
                ($0.modifiers.isEmpty || $0.modifiers == ["ctrl"])
                && (KeymapContext(rawValue: $0.context) ?? .list) == context
            }

            // Sequence lookup
            var lookup: [String: KeymapAction] = [:]
            for entry in contextEntries {
                guard let action = KeymapAction(rawValue: entry.action) else { continue }
                let seqKey = entry.modifiers == ["ctrl"] ? "ctrl+\(entry.key.lowercased())" : entry.key
                lookup[seqKey] = action
            }
            contextSequenceLookup[context] = lookup

            // Prefixes
            var prefixes: Set<String> = []
            for (seq, _) in lookup where seq.count > 1 {
                for i in 1..<seq.count {
                    prefixes.insert(String(seq.prefix(i)))
                }
            }
            contextPrefixes[context] = prefixes

            // Tag ops lookup
            var tagOps: [String: String] = [:]
            for entry in contextEntries where entry.action == "tag_op" {
                if let tags = entry.tags {
                    let seqKey = entry.modifiers == ["ctrl"] ? "ctrl+\(entry.key.lowercased())" : entry.key
                    tagOps[seqKey] = tags
                }
            }
            contextTagOps[context] = tagOps

            // Count support
            contextCountSupported[context] = Set(
                contextEntries
                    .filter { $0.supportsCount }
                    .compactMap { KeymapAction(rawValue: $0.action) }
            )
        }

        let totalSeqs = contextSequenceLookup.values.reduce(0) { $0 + $1.count }
        Log.debug("SEQMATCH", "Loaded \(totalSeqs) sequences across \(contextSequenceLookup.count) contexts")
    }

    // MARK: - Public API

    /// Match buffer contents against known sequences in the given context
    /// - Parameters:
    ///   - buffer: Current key buffer contents
    ///   - context: Active keymap context (defaults to .list)
    /// - Returns: Match result
    func match(buffer: String, context: KeymapContext = .list) -> SequenceMatchResult {
        // Empty buffer
        if buffer.isEmpty {
            return .noMatch
        }

        let lookup = contextSequenceLookup[context] ?? [:]
        let prefixes = contextPrefixes[context] ?? []
        let countSupported = contextCountSupported[context] ?? []

        // Parse count prefix if present (e.g., "5j" -> count=5, sequence="j")
        let (count, sequence) = parseCountAndSequence(buffer)

        // If only digits, waiting for action
        if sequence.isEmpty && count != nil {
            return .partial
        }

        // Check for exact match
        if let action = lookup[sequence] {
            let finalCount = count ?? 1
            // Validate count support from config
            if finalCount > 1 && !countSupported.contains(action) {
                return .match(action: action, count: 1)
            }
            return .match(action: action, count: finalCount)
        }

        // Check for partial match (could become a longer sequence)
        if prefixes.contains(sequence) {
            return .partial
        }

        return .noMatch
    }

    /// Get tags for a tag_op sequence in a given context
    func tagOpTags(for sequence: String, context: KeymapContext) -> String? {
        contextTagOps[context]?[sequence]
    }

    /// Get all sequences for an action
    func sequences(for action: KeymapAction) -> [String] {
        sequences.filter { $0.action == action }.map { $0.sequence }
    }

    /// Get all defined sequences (for help display)
    var allSequences: [SequenceDefinition] {
        sequences
    }

    /// Check if an action supports count prefix in a given context
    func supportsCount(_ action: KeymapAction, context: KeymapContext = .list) -> Bool {
        contextCountSupported[context]?.contains(action) ?? false
    }

    // MARK: - Private

    /// Parse count prefix from buffer
    /// e.g., "5j" → (5, "j"), "12gg" → (12, "gg"), "gg" → (nil, "gg")
    func parseCountAndSequence(_ buffer: String) -> (count: Int?, sequence: String) {
        var digits = ""
        var rest = ""
        var foundNonDigit = false

        for char in buffer {
            if char.isNumber && !foundNonDigit {
                digits.append(char)
            } else {
                foundNonDigit = true
                rest.append(char)
            }
        }

        let count = digits.isEmpty ? nil : Int(digits)
        return (count, rest)
    }
}
