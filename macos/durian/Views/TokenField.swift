//
//  TokenField.swift
//  Durian
//
//  Native NSTokenField wrapper for email addresses with autocomplete
//

import AppKit
import SwiftUI

// MARK: - Compose Field Enum (Shared Focus)

enum ComposeField: Hashable {
    case to
    case cc
    case bcc
    case subject
    case body
}

// MARK: - Suggestion Key Command

enum SuggestionKeyCommand {
    case up
    case down
    case enter
    case escape
    case tab
}

// MARK: - Token Field Helper

/// Helper to clear partial text and insert a token in an NSTokenField
enum TokenFieldHelper {
    /// Clear partial text and insert a complete token
    static func replacePartialTextWithToken(_ token: String, in tokenField: NSTokenField) {
        guard let editor = tokenField.currentEditor() as? NSTextView else { return }

        let fullText = editor.string

        // Find the start of the partial text (after last attachment character or separator)
        let attachmentChar = "\u{FFFC}"
        var partialStartIndex = fullText.startIndex

        if let lastAttachmentIndex = fullText.lastIndex(of: Character(attachmentChar)) {
            partialStartIndex = fullText.index(after: lastAttachmentIndex)
        } else {
            // Check for separators
            let separators = CharacterSet(charactersIn: ",;\n")
            for (index, char) in fullText.enumerated().reversed() {
                if char.unicodeScalars.allSatisfy({ separators.contains($0) }) {
                    partialStartIndex = fullText.index(fullText.startIndex, offsetBy: index + 1)
                    break
                }
            }
        }

        // Calculate range to replace
        let partialRange = partialStartIndex..<fullText.endIndex
        let nsRange = NSRange(partialRange, in: fullText)

        // Replace partial text with empty string
        editor.replaceCharacters(in: nsRange, with: "")

        // Now get the current tokens and add the new one
        var currentTokens = (tokenField.objectValue as? [String]) ?? []
        currentTokens.append(token)
        tokenField.objectValue = currentTokens as NSArray

        // Move cursor to end (after the new token) with no selection
        DispatchQueue.main.async {
            if let newEditor = tokenField.currentEditor() as? NSTextView {
                let endPosition = newEditor.string.count
                newEditor.setSelectedRange(NSRange(location: endPosition, length: 0))
            }
        }
    }
}

// MARK: - Token Field (NSTokenField Wrapper)

struct TokenField: NSViewRepresentable {
    @Binding var tokens: [String]
    var focusedField: FocusState<ComposeField?>.Binding
    let fieldIdentifier: ComposeField
    var onCommit: (() -> Void)? = nil

    // Callbacks for custom autocomplete popup
    var onPartialTextChange: ((String, NSTokenField) -> Void)? = nil
    var onKeyCommand: ((SuggestionKeyCommand) -> Bool)? = nil  // Returns true if handled

    func makeCoordinator() -> Coordinator {
        Coordinator(self)
    }

    func makeNSView(context: Context) -> NSTokenField {
        let tokenField = NSTokenField()
        tokenField.delegate = context.coordinator

        // Styling
        tokenField.isBordered = false
        tokenField.backgroundColor = .clear
        tokenField.drawsBackground = false
        tokenField.focusRingType = .none
        tokenField.font = .systemFont(ofSize: 14)

        // Token behavior
        tokenField.tokenizingCharacterSet = CharacterSet(charactersIn: ";\n")
        tokenField.tokenStyle = .rounded

        // Layout
        tokenField.lineBreakMode = .byClipping
        tokenField.cell?.isScrollable = true
        tokenField.cell?.wraps = false

        // Set initial value
        tokenField.objectValue = tokens as NSArray

        context.coordinator.tokenField = tokenField

        return tokenField
    }

    func updateNSView(_ nsView: NSTokenField, context: Context) {
        // Update tokens if changed from outside
        let currentTokens = (nsView.objectValue as? [String]) ?? []
        if currentTokens != tokens {
            nsView.objectValue = tokens as NSArray
        }

        // Handle focus
        let shouldBeFocused = focusedField.wrappedValue == fieldIdentifier
        let isFocused = nsView.window?.firstResponder == nsView.currentEditor()

        if shouldBeFocused && !isFocused {
            DispatchQueue.main.async {
                nsView.window?.makeFirstResponder(nsView)
            }
        }
    }

    // MARK: - Coordinator

    class Coordinator: NSObject, NSTokenFieldDelegate {
        var parent: TokenField
        weak var tokenField: NSTokenField?

        init(_ parent: TokenField) {
            self.parent = parent
        }

        // MARK: - Token Field Delegate

        func controlTextDidChange(_ notification: Notification) {
            guard let tokenField = notification.object as? NSTokenField else { return }

            // Update parent tokens
            if let newTokens = tokenField.objectValue as? [String] {
                if newTokens != parent.tokens {
                    DispatchQueue.main.async {
                        self.parent.tokens = newTokens
                    }
                }
            }

            // Notify about partial text for custom autocomplete
            notifyPartialTextChange(tokenField: tokenField)
        }

        func controlTextDidBeginEditing(_ notification: Notification) {
            DispatchQueue.main.async {
                self.parent.focusedField.wrappedValue = self.parent.fieldIdentifier
            }
        }

        func controlTextDidEndEditing(_ notification: Notification) {
            // Commit any pending tokens
            guard let tokenField = notification.object as? NSTokenField else { return }

            if let newTokens = tokenField.objectValue as? [String] {
                DispatchQueue.main.async {
                    self.parent.tokens = newTokens
                    self.parent.onCommit?()
                }
            }
        }

        func control(_ control: NSControl, textView: NSTextView, doCommandBy commandSelector: Selector) -> Bool {
            // Map selector to suggestion command
            let command: SuggestionKeyCommand? = {
                switch commandSelector {
                case #selector(NSResponder.moveUp(_:)):
                    return .up
                case #selector(NSResponder.moveDown(_:)):
                    return .down
                case #selector(NSResponder.insertNewline(_:)):
                    return .enter
                case #selector(NSResponder.cancelOperation(_:)):
                    return .escape
                case #selector(NSResponder.insertTab(_:)):
                    return .tab
                default:
                    return nil
                }
            }()

            // Let parent handle if custom popup is showing
            if let command = command,
               let handler = parent.onKeyCommand,
               handler(command)
            {
                return true  // We handled it, don't let NSTokenField process
            }

            // Default handling for Enter - commit tokens
            if commandSelector == #selector(NSResponder.insertNewline(_:)) {
                if let tokenField = control as? NSTokenField,
                   let tokens = tokenField.objectValue as? [String]
                {
                    DispatchQueue.main.async {
                        self.parent.tokens = tokens
                        self.parent.onCommit?()
                    }
                }
                return false // Let NSTokenField handle the tokenization
            }

            return false
        }

        // MARK: - Partial Text Extraction

        /// Extract the current partial input (text being typed, not yet a token)
        private func notifyPartialTextChange(tokenField: NSTokenField) {
            guard let editor = tokenField.currentEditor() as? NSTextView else { return }

            // Get the full text in the editor
            let fullText = editor.string

            // Get existing tokens
            let existingTokens = (tokenField.objectValue as? [String]) ?? []

            // Extract partial input (text after the last token)
            let partialText = extractPartialInput(from: fullText, existingTokens: existingTokens)

            // Notify parent
            DispatchQueue.main.async {
                self.parent.onPartialTextChange?(partialText, tokenField)
            }
        }

        /// Extract partial text from full editor string
        private func extractPartialInput(from fullText: String, existingTokens: [String]) -> String {
            // The NSTokenField editor contains tokens as special characters (attachment characters)
            // followed by any text being typed. We need to find text after the last token separator.

            // Simple approach: find text after the last token separator or attachment
            let separators = CharacterSet(charactersIn: ",;\n")

            // Split by separators and get the last component
            let components = fullText.components(separatedBy: separators)
            let lastComponent = components.last?.trimmingCharacters(in: .whitespaces) ?? ""

            // Also check for attachment character (used by NSTokenField for tokens)
            // The attachment character is \u{FFFC}
            let attachmentChar = "\u{FFFC}"
            if let lastAttachmentIndex = fullText.lastIndex(of: Character(attachmentChar)) {
                let afterAttachment = String(fullText[fullText.index(after: lastAttachmentIndex)...])
                let trimmed = afterAttachment.trimmingCharacters(in: .whitespaces)
                return trimmed
            }

            return lastComponent
        }

        // MARK: - Token Representation

        func tokenField(_ tokenField: NSTokenField, displayStringForRepresentedObject representedObject: Any) -> String? {
            // Display only the name part, not the full "Name <email>" format
            guard let fullString = representedObject as? String else { return nil }
            return extractDisplayName(from: fullString)
        }

        func tokenField(_ tokenField: NSTokenField, editingStringForRepresentedObject representedObject: Any) -> String? {
            // When editing, show the full email address
            representedObject as? String
        }

        func tokenField(_ tokenField: NSTokenField, representedObjectForEditing editingString: String) -> Any? {
            // Clean the email when creating a token
            let cleaned = EmailTokenHelper.cleanEmail(editingString)
            return cleaned.isEmpty ? nil : cleaned
        }

        func tokenField(_ tokenField: NSTokenField, styleForRepresentedObject representedObject: Any) -> NSTokenField.TokenStyle {
            // Use rounded style for all tokens
            .rounded
        }

        // MARK: - Token Context Menu

        func tokenField(_ tokenField: NSTokenField, hasMenuForRepresentedObject representedObject: Any) -> Bool {
            true
        }

        func tokenField(_ tokenField: NSTokenField, menuForRepresentedObject representedObject: Any) -> NSMenu? {
            guard let fullString = representedObject as? String else { return nil }
            let email = extractEmail(from: fullString)

            let menu = NSMenu()

            // Display email as disabled menu item (just for display)
            let emailItem = NSMenuItem(title: email, action: nil, keyEquivalent: "")
            emailItem.isEnabled = false
            menu.addItem(emailItem)

            menu.addItem(NSMenuItem.separator())

            // Copy Address action
            let copyItem = NSMenuItem(title: "Copy Address", action: #selector(copyEmailAddress(_:)), keyEquivalent: "")
            copyItem.representedObject = email
            copyItem.target = self
            menu.addItem(copyItem)

            return menu
        }

        @objc private func copyEmailAddress(_ sender: NSMenuItem) {
            guard let email = sender.representedObject as? String else { return }
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(email, forType: .string)
        }

        // MARK: - Email/Name Extraction Helpers

        /// Extract display name from "Name <email>" format, or return email if no name
        private func extractDisplayName(from fullString: String) -> String {
            let trimmed = fullString.trimmingCharacters(in: .whitespaces)
            // Check for "Name <email>" or "Name<email>" format
            if let angleBracket = trimmed.range(of: "<"),
               trimmed.contains(">")
            {
                let namePart = String(trimmed[..<angleBracket.lowerBound])
                    .trimmingCharacters(in: .whitespaces)
                    .trimmingCharacters(in: CharacterSet(charactersIn: "\""))
                if !namePart.isEmpty {
                    return namePart
                }
            }
            // No name found, return as-is (probably just email)
            return trimmed
        }

        /// Extract email from "Name <email>" format, or return as-is if just email
        private func extractEmail(from fullString: String) -> String {
            // Check for "Name <email>" format
            if let startRange = fullString.range(of: "<"),
               let endRange = fullString.range(of: ">")
            {
                let email = String(fullString[startRange.upperBound..<endRange.lowerBound])
                return email.trimmingCharacters(in: .whitespaces)
            }
            // No angle brackets, assume it's just the email
            return fullString
        }

        // MARK: - Autocomplete (Disabled - using custom popup)

        func tokenField(_ tokenField: NSTokenField, completionsForSubstring substring: String, indexOfToken tokenIndex: Int, indexOfSelectedItem selectedIndex: UnsafeMutablePointer<Int>?) -> [Any]? {
            // Return nil to disable native autocomplete
            // Our custom ContactSuggestionPopup handles this instead
            nil
        }
    }
}

// MARK: - Email Helper Alias (uses EmailHelper from EmailComposition)

enum EmailTokenHelper {
    static func isValidEmail(_ email: String) -> Bool {
        EmailHelper.isValidEmail(email)
    }

    static func cleanEmail(_ input: String) -> String {
        EmailHelper.cleanEmail(input)
    }
}
