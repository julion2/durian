import Foundation
import SwiftUI

// MARK: - Attachment Info (from API)

struct AttachmentInfo: Decodable, Equatable {
    let partId: Int
    let filename: String
    let contentType: String
    let size: Int
    let disposition: String
    let contentId: String?

    enum CodingKeys: String, CodingKey {
        case partId = "part_id"
        case filename
        case contentType = "content_type"
        case size
        case disposition
        case contentId = "content_id"
    }
}

// MARK: - Thread Message

struct ThreadMessage: Decodable, Identifiable, Equatable {
    let id: String
    let from: String
    let to: String?
    let cc: String?
    let date: String
    let timestamp: Int
    let message_id: String?
    let in_reply_to: String?
    let references: String?
    let body: String
    let html: String?
    let hidden_signature: String?
    let attachments: [AttachmentInfo]?
    let tags: [String]?

    var isDraft: Bool {
        tags?.contains("draft") ?? false
    }
}

// MARK: - Email Body State

enum EmailBodyState: Equatable, Hashable {
    case notLoaded
    case loading
    case loaded(body: String, attributedBody: NSAttributedString?)
    case failed(message: String)

    var displayBody: String {
        switch self {
        case .notLoaded:
            return "Tap to load email content"
        case .loading:
            return "Loading..."
        case .loaded(let body, _):
            return body.isEmpty ? "No content available" : body
        case .failed(let message):
            return "Failed to load: \(message)"
        }
    }

    var attributedBody: NSAttributedString? {
        switch self {
        case .loaded(_, let attributed):
            return attributed
        default:
            return nil
        }
    }

    func hash(into hasher: inout Hasher) {
        switch self {
        case .notLoaded:
            hasher.combine(0)
        case .loading:
            hasher.combine(1)
        case .loaded(let body, _):
            hasher.combine(2)
            hasher.combine(body)
        case .failed(let message):
            hasher.combine(3)
            hasher.combine(message)
        }
    }

    static func == (lhs: EmailBodyState, rhs: EmailBodyState) -> Bool {
        switch (lhs, rhs) {
        case (.notLoaded, .notLoaded), (.loading, .loading):
            return true
        case (.loaded(let lBody, _), .loaded(let rBody, _)):
            return lBody == rBody
        case (.failed(let lMsg), .failed(let rMsg)):
            return lMsg == rMsg
        default:
            return false
        }
    }
}

// MARK: - Unified Models

/// Unified folder/tag model
struct MailFolder: Identifiable, Hashable {
    let id: String
    let name: String
    let displayName: String
    let icon: String?
    let accountId: String
    let isSpecial: Bool  // true for inbox, sent, drafts, trash
    let specialType: SpecialFolderType?
    let isSection: Bool  // true for section headers (no query, not clickable)

    enum SpecialFolderType: String {
        case inbox, sent, drafts, trash, archive, junk
    }

    /// Create a section header (non-clickable divider in sidebar)
    init(section title: String) {
        id = "section:\(title)"
        name = title
        displayName = title
        icon = nil
        accountId = "default"
        isSpecial = false
        specialType = nil
        isSection = true
    }

    /// Create for tag-based folder
    init(tag: String, icon: String) {
        id = "tag:\(tag)"
        name = tag
        displayName = tag.capitalized
        self.icon = icon
        accountId = "default"
        isSection = false

        switch tag {
        case "inbox":
            isSpecial = true
            specialType = .inbox
        case "sent":
            isSpecial = true
            specialType = .sent
        case "draft", "drafts":
            isSpecial = true
            specialType = .drafts
        case "deleted", "trash":
            isSpecial = true
            specialType = .trash
        case "archive":
            isSpecial = true
            specialType = .archive
        default:
            isSpecial = false
            specialType = nil
        }
    }

    /// Create from profile folder config (name, displayName, icon)
    init(name: String, displayName: String, icon: String?) {
        id = "folder:\(name)"
        self.name = name
        self.displayName = displayName
        self.icon = icon
        accountId = "default"
        isSection = false

        switch name.lowercased() {
        case "inbox":
            isSpecial = true
            specialType = .inbox
        case "sent":
            isSpecial = true
            specialType = .sent
        case "draft", "drafts":
            isSpecial = true
            specialType = .drafts
        case "deleted", "trash":
            isSpecial = true
            specialType = .trash
        case "archive":
            isSpecial = true
            specialType = .archive
        default:
            isSpecial = false
            specialType = nil
        }
    }
}

/// Unified email model
struct MailMessage: Identifiable, Hashable {
    let id: String  // thread_id
    let subject: String
    var from: String
    var to: String?
    var cc: String?
    var date: String
    let timestamp: Int  // Unix timestamp for grouping
    var tags: String?
    var body: String?
    var htmlBody: String?  // HTML version of body (for WebView rendering)
    var attributedBody: NSAttributedString?
    var isRead: Bool
    var isPinned: Bool
    var hasAttachment: Bool
    var bodyState: EmailBodyState
    var incomingAttachments: [IncomingAttachmentMetadata]

    // Thread messages (loaded from CLI show command)
    var threadMessages: [ThreadMessage]?

    // Preview text from search enrichment (separate from bodyState cache)
    var previewText: String?


    // Reply/Forward metadata (loaded with body)
    var messageId: String?
    var inReplyTo: String?
    var references: String?

    /// Create from search result
    init(threadId: String, subject: String, from: String, to: String? = nil, date: String, timestamp: Int, tags: String) {
        id = threadId
        self.subject = subject
        self.from = from
        self.to = to
        cc = nil
        self.date = date
        self.timestamp = timestamp
        self.tags = tags
        body = nil
        htmlBody = nil
        attributedBody = nil
        let tagSet = Set(tags.split(separator: ","))
        isRead = !tagSet.contains("unread")
        isPinned = tagSet.contains("flagged")
        hasAttachment = tagSet.contains("attachment")
        bodyState = .notLoaded
        incomingAttachments = []
        threadMessages = nil
        messageId = nil
        inReplyTo = nil
        references = nil
    }

    var isDraft: Bool {
        guard let tags = tags else { return false }
        return tags.split(separator: ",").contains("draft")
    }

    /// Body preview for list view (first ~150 chars)
    var bodyPreview: String? {
        // Prefer enrichment preview (always available, no HTML)
        if let preview = previewText, !preview.isEmpty {
            let stripped = preview
                .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
                .trimmingCharacters(in: .whitespacesAndNewlines)
            return String(stripped.prefix(150))
        }
        // Fallback to loaded body
        switch bodyState {
        case .loaded(let body, _):
            guard !body.isEmpty else { return nil }
            let stripped = body
                .replacingOccurrences(of: "<[^>]+>", with: "", options: .regularExpression)
                .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
                .trimmingCharacters(in: .whitespacesAndNewlines)
            return String(stripped.prefix(150))
        default:
            return nil
        }
    }

    // Hashable
    func hash(into hasher: inout Hasher) {
        hasher.combine(id)
        hasher.combine(tags)
        hasher.combine(isRead)
        hasher.combine(isPinned)
        hasher.combine(bodyState)
    }

    static func == (lhs: MailMessage, rhs: MailMessage) -> Bool {
        lhs.id == rhs.id &&
        lhs.tags == rhs.tags &&
        lhs.isRead == rhs.isRead &&
        lhs.isPinned == rhs.isPinned &&
        lhs.bodyState == rhs.bodyState
    }
}

// MARK: - Default Tags

extension MailFolder {
    static let defaultTags: [MailFolder] = [
        MailFolder(tag: "inbox", icon: "tray"),
        MailFolder(tag: "unread", icon: "envelope.badge"),
        MailFolder(tag: "sent", icon: "paperplane"),
        MailFolder(tag: "draft", icon: "doc.text"),
        MailFolder(tag: "archive", icon: "archivebox"),
        MailFolder(tag: "deleted", icon: "trash"),
        MailFolder(tag: "attachment", icon: "paperclip"),
        MailFolder(tag: "flagged", icon: "star"),
        MailFolder(tag: "cal", icon: "calendar"),
    ]
}
