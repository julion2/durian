//
//  ComposeForm+Contacts.swift
//  Durian
//
//  Contact suggestion popup handling for ComposeForm.
//  Split out of ComposeForm.swift to keep the main form file under 800 lines.
//

import AppKit
import SwiftUI

extension ComposeForm {
    // MARK: - Contact Suggestion Handling

    /// Handle partial text changes from TokenField for autocomplete
    func handlePartialTextChange(query: String, tokenField: NSTokenField, field: ComposeField) {
        activeTokenField = field
        activeNSTokenField = tokenField  // Store reference for later use

        // Require at least 2 characters to search
        guard query.count >= 2 else {
            contactSearchTask?.cancel()
            dismissContactSuggestions()
            return
        }

        // Debounce: cancel previous search, wait 150ms before firing
        contactSearchTask?.cancel()
        contactSearchTask = Task {
            try? await Task.sleep(for: .milliseconds(80))
            guard !Task.isCancelled else { return }

            // Get existing tokens to filter out duplicates
            let existingEmails = currentTokensForField(field)

            // Search contacts via HTTP API
            let results = await ContactsManager.shared.search(query: query, limit: 8)
                .filter { contact in
                    !existingEmails.contains { existing in
                        existing.lowercased().contains(contact.email.lowercased())
                    }
                }

            guard !Task.isCancelled else { return }

            guard !results.isEmpty else {
                dismissContactSuggestions()
                return
            }

            contactSuggestions = results
            selectedSuggestionIndex = 0
            showingContactPopup = true

            // Show popup at cursor position
            ContactSuggestionController.shared.show(
                for: tokenField,
                contacts: results,
                selectedIndex: 0,
                onSelect: { [self] contact in
                    selectContact(contact)
                },
                onDismiss: { [self] in
                    dismissContactSuggestions()
                }
            )
        }
    }

    /// Handle keyboard commands when suggestion popup is visible
    func handleSuggestionKeyCommand(_ command: SuggestionKeyCommand) -> Bool {
        guard showingContactPopup, !contactSuggestions.isEmpty else {
            return false
        }

        switch command {
        case .up:
            selectedSuggestionIndex = max(0, selectedSuggestionIndex - 1)
            ContactSuggestionController.shared.update(
                contacts: contactSuggestions,
                selectedIndex: selectedSuggestionIndex
            )
            return true

        case .down:
            selectedSuggestionIndex = min(contactSuggestions.count - 1, selectedSuggestionIndex + 1)
            ContactSuggestionController.shared.update(
                contacts: contactSuggestions,
                selectedIndex: selectedSuggestionIndex
            )
            return true

        case .enter, .tab:
            selectContact(contactSuggestions[selectedSuggestionIndex])
            return true

        case .escape:
            dismissContactSuggestions()
            return true
        }
    }

    /// Select a contact and add it to the appropriate field
    func selectContact(_ contact: Contact) {
        guard let field = activeTokenField,
              let tokenField = activeNSTokenField else { return }

        // Clear partial text and insert the token
        TokenFieldHelper.replacePartialTextWithToken(contact.displayString, in: tokenField)

        // Update our binding to match the tokenField's new state
        DispatchQueue.main.async {
            if let newTokens = tokenField.objectValue as? [String] {
                switch field {
                case .to:
                    self.draft.to = newTokens
                case .cc:
                    self.draft.cc = newTokens
                case .bcc:
                    self.draft.bcc = newTokens
                default:
                    break
                }
            }
            self.dismissContactSuggestions()
            self.scheduleAutoSave()
        }
    }

    /// Dismiss the contact suggestions popup
    func dismissContactSuggestions() {
        showingContactPopup = false
        contactSuggestions = []
        selectedSuggestionIndex = 0
        activeTokenField = nil
        activeNSTokenField = nil
        ContactSuggestionController.shared.dismiss()
    }

    /// Get current tokens for a field (for duplicate filtering)
    func currentTokensForField(_ field: ComposeField) -> [String] {
        switch field {
        case .to: return draft.to
        case .cc: return draft.cc
        case .bcc: return draft.bcc
        default: return []
        }
    }
}
