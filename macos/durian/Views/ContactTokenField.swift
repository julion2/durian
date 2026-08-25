//
//  ContactTokenField.swift
//  Durian
//
//  Shared recipient/attendee token field with contact autocomplete.
//

import SwiftUI

import AppKit

struct ContactTokenField: View {
    @Binding var tokens: [String]
    var focusedField: FocusState<ComposeField?>.Binding? = nil
    var fieldIdentifier: ComposeField? = nil
    var contactToken: (Contact) -> String = { $0.displayString }
    var isValidToken: (String) -> Bool = { !$0.isEmpty }
    var wrapsTokens = false
    var tokenFieldAccessibilityLabel: String? = nil
    var onCommit: (() -> Void)?
    var onPartialTextChange: ((String) -> Void)?

    @State private var suggestionOwner = UUID()
    @State private var suggestions: [Contact] = []
    @State private var selectedIndex = 0
    @State private var tokenField: NSTokenField?
    @State private var searchTask: Task<Void, Never>?
    @State private var currentQuery = ""

    var body: some View {
        TokenField(
            tokens: $tokens,
            focusedField: focusedField,
            fieldIdentifier: fieldIdentifier,
            isValidToken: isValidToken,
            wrapsTokens: wrapsTokens,
            accessibilityLabel: tokenFieldAccessibilityLabel,
            onCommit: {
                dismissSuggestions()
                onCommit?()
            },
            onPartialTextChange: handlePartialTextChange,
            onKeyCommand: handleKeyCommand
        )
        .onDisappear {
            searchTask?.cancel()
            dismissSuggestions()
        }
    }

    // MARK: - Suggestions

    private func handlePartialTextChange(_ query: String, _ field: NSTokenField) {
        tokenField = field
        currentQuery = query
        onPartialTextChange?(query)

        guard query.count >= 2 else {
            searchTask?.cancel()
            dismissSuggestions()
            return
        }

        searchTask?.cancel()
        searchTask = Task {
            try? await Task.sleep(for: .milliseconds(80))
            guard !Task.isCancelled else { return }

            let existing = Set(tokens.map { EmailTokenHelper.cleanEmail($0).lowercased() })
            let results = await ContactsManager.shared.search(query: query, limit: 8)
                .filter { !existing.contains($0.email.lowercased()) }
            guard !Task.isCancelled,
                  currentQuery == query,
                  tokenField === field else { return }

            guard !results.isEmpty else {
                dismissSuggestions()
                return
            }

            suggestions = results
            selectedIndex = 0
            ContactSuggestionController.shared.show(
                owner: suggestionOwner,
                for: field,
                contacts: results,
                selectedIndex: 0,
                onSelect: selectContact,
                onDismiss: dismissSuggestions
            )
        }
    }

    private func handleKeyCommand(_ command: SuggestionKeyCommand) -> Bool {
        guard !suggestions.isEmpty,
              tokenField != nil,
              ContactSuggestionController.shared.isVisible(for: suggestionOwner) else { return false }

        switch command {
        case .up:
            selectedIndex = max(0, selectedIndex - 1)
        case .down:
            selectedIndex = min(suggestions.count - 1, selectedIndex + 1)
        case .enter, .tab:
            selectContact(suggestions[selectedIndex])
            return true
        case .escape:
            dismissSuggestions()
            return true
        }
        ContactSuggestionController.shared.update(
            owner: suggestionOwner,
            contacts: suggestions,
            selectedIndex: selectedIndex
        )
        return true
    }

    private func selectContact(_ contact: Contact) {
        guard let tokenField else { return }
        let token = contactToken(contact)
        guard isValidToken(token) else { return }
        TokenFieldHelper.replacePartialTextWithToken(token, in: tokenField)
        DispatchQueue.main.async {
            if let newTokens = tokenField.objectValue as? [String] {
                tokens = newTokens
            }
            onPartialTextChange?("")
            dismissSuggestions()
            onCommit?()
        }
    }

    private func dismissSuggestions() {
        searchTask?.cancel()
        searchTask = nil
        currentQuery = ""
        suggestions = []
        selectedIndex = 0
        tokenField = nil
        ContactSuggestionController.shared.dismiss(owner: suggestionOwner)
    }
}
