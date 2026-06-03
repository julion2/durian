//
//  EmailComposition.swift
//  Durian
//
//  Email composition data models (EmailDraft, EmailAttachment, errors)
//  and the EmailHelper validation utility.
//
//  Reply / forward factory methods live in EmailDraftFactory.swift.
//  Local-draft persistence lives in DraftManager.swift.
//

import Foundation

struct EmailDraft: Identifiable, Codable, Equatable {
    var id: UUID
    var from: String
    var to: [String]
    var cc: [String]
    var bcc: [String]
    var subject: String
    var body: String  // User's editable text
    var isHTML: Bool
    var inReplyTo: String?
    var references: String?
    var createdAt: Date
    var modifiedAt: Date
    var uid: UInt32?
    var accountId: String?
    var attachments: [EmailAttachment] = []

    /// IMAP Message-ID (set after saving to server)
    var messageId: String?

    /// Quoted/forwarded content (read-only, shown as preview)
    var quotedContent: String?
    /// Whether quotedContent is HTML
    var quotedIsHTML: Bool = false

    /// HTML signature (kept separate from body, combined at send time)
    var htmlSignature: String?

    /// HTML body from the rich text editor (formatted user content, excluding signature)
    var htmlBody: String?

    /// Thread ID of the original message (for navigating back after send)
    var replyThreadId: String?

    init(
        id: UUID = UUID(),
        from: String,
        to: [String] = [],
        cc: [String] = [],
        bcc: [String] = [],
        subject: String = "",
        body: String = "",
        isHTML: Bool = false,
        inReplyTo: String? = nil,
        references: String? = nil,
        messageId: String? = nil,
        quotedContent: String? = nil,
        quotedIsHTML: Bool = false,
        htmlSignature: String? = nil,
        htmlBody: String? = nil
    ) {
        self.id = id
        self.from = from
        self.to = to
        self.cc = cc
        self.bcc = bcc
        self.subject = subject
        self.body = body
        self.isHTML = isHTML
        self.inReplyTo = inReplyTo
        self.references = references
        self.messageId = messageId
        self.quotedContent = quotedContent
        self.quotedIsHTML = quotedIsHTML
        self.htmlSignature = htmlSignature
        self.htmlBody = htmlBody
        createdAt = Date()
        modifiedAt = Date()
    }

    mutating func updateModifiedDate() {
        modifiedAt = Date()
    }

    var hasRecipients: Bool {
        !to.isEmpty || !cc.isEmpty || !bcc.isEmpty
    }

    var isValid: Bool {
        hasRecipients && !subject.isEmpty
    }

    /// Whether the user has typed any actual content (excludes signature, quoted content).
    /// Subject-only changes are intentionally not counted — subject-only replies aren't a real use case.
    var hasUserContent: Bool {
        if !body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            return true
        }
        if let html = htmlBody {
            let stripped = html.replacingOccurrences(of: "<[^>]+>", with: "", options: .regularExpression)
            if !stripped.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                return true
            }
        }
        return false
    }

    var hasAttachments: Bool {
        !attachments.isEmpty
    }

    var totalAttachmentSize: Int64 {
        attachments.reduce(0) { $0 + Int64($1.data.count) }
    }
}

struct EmailAttachment: Identifiable, Codable, Equatable, Hashable {
    let id: UUID
    let filename: String
    let mimeType: String
    let data: Data

    init(id: UUID = UUID(), filename: String, mimeType: String, data: Data) {
        self.id = id
        self.filename = filename
        self.mimeType = mimeType
        self.data = data
    }

    var size: Int64 {
        Int64(data.count)
    }

    var sizeFormatted: String {
        ByteCountFormatter.string(fromByteCount: size, countStyle: .file)
    }
}

enum EmailSendingError: Error, LocalizedError {
    case noSMTPConfiguration
    case authenticationFailed
    case sendFailed(String)
    case invalidRecipients
    case invalidEmailFormat([String])
    case connectionFailed

    var errorDescription: String? {
        switch self {
        case .noSMTPConfiguration:
            return "SMTP server not configured for this account"
        case .authenticationFailed:
            return "Failed to authenticate with SMTP server"
        case .sendFailed(let message):
            return "Failed to send email: \(message)"
        case .invalidRecipients:
            return "Please provide at least one recipient"
        case .invalidEmailFormat(let emails):
            return "Invalid email addresses: \(emails.joined(separator: ", "))"
        case .connectionFailed:
            return "Failed to connect to SMTP server"
        }
    }

    /// Returns the list of invalid emails if this is an invalidEmailFormat error
    var invalidEmails: [String]? {
        if case .invalidEmailFormat(let emails) = self {
            return emails
        }
        return nil
    }
}

// MARK: - Email Helper

enum EmailHelper {
    /// Simple email validation — handles both bare emails and "Name <email>" format
    static func isValidEmail(_ input: String) -> Bool {
        let email = cleanEmail(input)
        guard let atIndex = email.firstIndex(of: "@") else { return false }
        let afterAt = email[email.index(after: atIndex)...]
        return afterAt.contains(".") && !afterAt.hasPrefix(".") && !afterAt.hasSuffix(".")
    }

    /// Clean email address - extract from "Name <email>" format if needed
    static func cleanEmail(_ input: String) -> String {
        let trimmed = input.trimmingCharacters(in: .whitespaces)

        // Standard format: "Name <email>"
        if let start = trimmed.firstIndex(of: "<"),
           let end = trimmed.firstIndex(of: ">"),
           start < end
        {
            let email = String(trimmed[trimmed.index(after: start)..<end])
            if email.contains("@") {
                return email.trimmingCharacters(in: .whitespaces)
            }
        }

        // Malformed: "<Name> email" - get last word with @
        let parts = trimmed.components(separatedBy: .whitespaces)
        if let lastPart = parts.last, lastPart.contains("@") {
            return lastPart
        }

        return trimmed
    }

    /// Validate all recipients and return list of invalid emails
    static func validateRecipients(_ recipients: [String]) -> [String] {
        recipients.filter { !isValidEmail($0) }
    }
}
