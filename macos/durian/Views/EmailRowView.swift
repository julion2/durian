import SwiftUI

struct EmailRowView: View, Equatable {
    let email: MailMessage
    var isSelected: Bool = false
    var isFirstInGroup: Bool = true
    var isLastInGroup: Bool = true
    var currentFolder: String = AccountManager.shared.selectedFolder

    // Context menu callbacks (excluded from Equatable)
    var onTogglePin: (() -> Void)?
    var onToggleRead: (() -> Void)?
    var onDelete: (() -> Void)?
    var onReply: (() -> Void)?
    var onForward: (() -> Void)?
    var onShowTagPicker: (() -> Void)?

    static func == (lhs: EmailRowView, rhs: EmailRowView) -> Bool {
        lhs.email.id == rhs.email.id &&
        lhs.email.tags == rhs.email.tags &&
        lhs.email.isRead == rhs.email.isRead &&
        lhs.email.isPinned == rhs.email.isPinned &&
        lhs.email.previewText == rhs.email.previewText &&
        lhs.email.bodyState == rhs.email.bodyState &&
        lhs.isSelected == rhs.isSelected &&
        lhs.isFirstInGroup == rhs.isFirstInGroup &&
        lhs.isLastInGroup == rhs.isLastInGroup &&
        lhs.currentFolder == rhs.currentFolder
    }

    // Cached counterparties — computed once, not on every render
    private var cachedCounterparties: [Participant] {
        Self.resolveCounterparties(email: email)
    }

    var body: some View {
        let parties = cachedCounterparties
        HStack(alignment: .top, spacing: 10) {
            AvatarView(name: parties.first?.name ?? email.from,
                       email: parties.first?.email ?? email.from,
                       size: 32)

            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    if !email.isRead {
                        Circle()
                            .fill(Color.blue)
                            .frame(width: 8, height: 8)
                    }

                    if email.isPinned {
                        Image(systemName: "pin.fill")
                            .font(.caption)
                            .foregroundColor(.yellow)
                    }

                    Text(parties.map(\.name).isEmpty ? Self.extractName(from: email.from) : parties.map(\.name).joined(separator: ", "))
                        .font(.headline)
                        .fontWeight(email.isRead ? .regular : .bold)
                        .foregroundStyle(isSelected ? .white : .primary)
                        .lineLimit(1)
                    Spacer()
                    Text(email.date)
                        .font(.callout)
                        .foregroundStyle(isSelected ? .white.opacity(0.8) : .secondary)
                }

                HStack(spacing: 4) {
                    Text(email.subject.isEmpty ? "(No Subject)" : email.subject)
                        .font(.callout)
                        .fontWeight(email.isRead ? .regular : .semibold)
                        .foregroundStyle(isSelected ? .white.opacity(0.9) : .primary)
                        .lineLimit(1)
                    Spacer()
                    if email.hasAttachment {
                        Image(systemName: "paperclip")
                            .font(.caption)
                            .foregroundStyle(isSelected ? .white.opacity(0.7) : .secondary)
                    }
                }

                if let preview = email.bodyPreview, !preview.isEmpty {
                    Text(preview)
                        .font(.subheadline)
                        .foregroundStyle(isSelected ? .white.opacity(0.7) : .secondary)
                        .lineLimit(2)
                }

                if !visibleTags.isEmpty {
                    tagRow
                }
            }
        }
        .padding(.vertical, 8)
        .padding(.horizontal, 12)
        .background(
            UnevenRoundedRectangle(
                topLeadingRadius: isFirstInGroup ? 6 : 0,
                bottomLeadingRadius: isLastInGroup ? 6 : 0,
                bottomTrailingRadius: isLastInGroup ? 6 : 0,
                topTrailingRadius: isFirstInGroup ? 6 : 0
            )
            .fill(isSelected ? ProfileManager.shared.resolvedAccentColor : Color.clear)
        )
        .padding(.horizontal, 8)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(emailAccessibilityLabel)
        .accessibilityAddTraits(isSelected ? .isSelected : [])
        .accessibilityAddTraits(!email.isRead ? .isHeader : [])
        .drawingGroup()
        .contextMenu {
            Button(action: { onTogglePin?() }) {
                Label(email.isPinned ? "Unpin" : "Pin", systemImage: email.isPinned ? "pin.slash" : "pin")
            }

            Button(action: { onToggleRead?() }) {
                Label(email.isRead ? "Mark as Unread" : "Mark as Read",
                      systemImage: email.isRead ? "envelope.badge" : "envelope.open")
            }

            Divider()

            Button(action: { onReply?() }) {
                Label("Reply", systemImage: "arrowshape.turn.up.left")
            }

            Button(action: { onForward?() }) {
                Label("Forward", systemImage: "arrowshape.turn.up.right")
            }

            Divider()

            Button(action: { onShowTagPicker?() }) {
                Label("Tags...", systemImage: "tag")
            }

            Divider()

            Button(role: .destructive, action: { onDelete?() }) {
                Label("Delete", systemImage: "trash")
            }
        }
    }

    // MARK: - Accessibility

    private var emailAccessibilityLabel: String {
        var parts: [String] = []
        if !email.isRead { parts.append("Unread") }
        if email.isPinned { parts.append("Pinned") }
        let sender = cachedCounterparties.map(\.name).joined(separator: ", ")
        parts.append(sender.isEmpty ? Self.extractName(from: email.from) : sender)
        parts.append(email.subject.isEmpty ? "No Subject" : email.subject)
        parts.append(email.date)
        if email.hasAttachment { parts.append("Has attachment") }
        if !visibleTags.isEmpty { parts.append("Tags: \(visibleTags.joined(separator: ", "))") }
        return parts.joined(separator: ", ")
    }

    // MARK: - Tag Row

    @ViewBuilder
    private var tagRow: some View {
        TruncatedTagsView(tags: visibleTags, availableWidth: 320, isSelected: isSelected)
            .frame(height: 18)
    }

    /// Tags already represented by icons or implied by the current view
    private static let hiddenTags: Set<String> = ["unread", "flagged", "attachment"]

    private var visibleTags: [String] {
        guard let tags = email.tags, !tags.isEmpty else { return [] }
        return tags
            .split(separator: ",")
            .map { String($0.trimmingCharacters(in: .whitespaces)) }
            .filter { !$0.isEmpty && !Self.hiddenTags.contains($0) && $0 != currentFolder }
    }

    // MARK: - Counterparty resolution

    private struct Participant: Hashable {
        let name: String
        let email: String  // may equal name if no email available
    }

    /// Cached own-email set — avoids repeated ConfigManager calls and regex
    private static let ownEmails: Set<String> = {
        Set(ConfigManager.shared.getAccounts().map { $0.email.lowercased() })
    }()
    private static let ownNames: Set<String> = {
        Set(ConfigManager.shared.getAccounts().map {
            $0.name.lowercased().replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
        })
    }()

    private static func isOwn(_ address: String) -> Bool {
        let email = extractEmail(from: address).lowercased()
        if ownEmails.contains(email) { return true }
        let name = extractName(from: address).lowercased()
            .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
        return ownNames.contains(name)
    }

    private static func extractName(from address: String) -> String {
        AddressUtils.extractName(from: address)
    }

    private static func extractEmail(from address: String) -> String {
        AddressUtils.extractEmail(from: address)
    }

    /// Parse a comma-separated address list, respecting quoted names and angle-bracket emails.
    /// Handles: "Last, First" <email>, 'Name' <email>, Name <email>
    private static func parseAddressList(_ raw: String) -> [String] {
        var results: [String] = []
        var current = ""
        var inQuotes = false
        var inAngleBracket = false
        var quoteChar: Character = "\""
        for ch in raw {
            if !inQuotes && !inAngleBracket && (ch == "\"" || ch == "'") {
                inQuotes = true
                quoteChar = ch
                current.append(ch)
            } else if inQuotes && ch == quoteChar {
                inQuotes = false
                current.append(ch)
            } else if !inQuotes && ch == "<" {
                inAngleBracket = true
                current.append(ch)
            } else if inAngleBracket && ch == ">" {
                inAngleBracket = false
                current.append(ch)
            } else if !inQuotes && !inAngleBracket && ch == "," {
                let trimmed = current.trimmingCharacters(in: .whitespaces)
                if !trimmed.isEmpty { results.append(trimmed) }
                current = ""
            } else {
                current.append(ch)
            }
        }
        let trimmed = current.trimmingCharacters(in: .whitespaces)
        if !trimmed.isEmpty { results.append(trimmed) }
        return results
    }

    /// Build a Participant from an address string, storing the clean email for avatar lookup.
    private static func participant(from addr: String) -> Participant {
        let cleanEmail = extractEmail(from: addr)
        return Participant(
            name: extractName(from: addr),
            email: cleanEmail.contains("@") ? cleanEmail : extractName(from: addr)
        )
    }

    /// All non-own participants from thread messages (from + to + cc), deduplicated, ordered.
    private static func resolveCounterparties(email: MailMessage) -> [Participant] {
        // When thread messages are loaded, use real from/to/cc fields
        if let messages = email.threadMessages, !messages.isEmpty {
            var seen = Set<String>()
            var result: [Participant] = []
            for msg in messages {
                let addresses = [msg.from] + Self.parseAddressList(msg.to ?? "")
                    + Self.parseAddressList(msg.cc ?? "")
                for addr in addresses {
                    guard !Self.isOwn(addr) else { continue }
                    let p = Self.participant(from: addr)
                    let key = p.email.lowercased()
                    if seen.insert(key).inserted {
                        result.append(p)
                    }
                }
            }
            if !result.isEmpty { return result }
        }

        // Fallback: parse authors string (before thread load).
        let raw = email.from
        var authors: [String]
        if raw.contains("<") {
            // Structured addresses — use proper parser
            authors = Self.parseAddressList(raw)
        } else {
            // Plain names/emails — split by comma, handle pipe separators
            authors = []
            for segment in raw.components(separatedBy: ",") {
                let trimmed = segment.trimmingCharacters(in: .whitespaces)
                if trimmed.isEmpty { continue }
                if let pipeRange = trimmed.range(of: "|"),
                   pipeRange.lowerBound > trimmed.startIndex,
                   trimmed[trimmed.index(before: pipeRange.lowerBound)] != " "
                {
                    let before = String(trimmed[..<pipeRange.lowerBound]).trimmingCharacters(in: .whitespaces)
                    let after = String(trimmed[trimmed.index(after: pipeRange.lowerBound)...]).trimmingCharacters(in: .whitespaces)
                    if !before.isEmpty { authors.append(before) }
                    if !after.isEmpty { authors.append(after) }
                } else {
                    authors.append(trimmed)
                }
            }
        }
        let others = authors.filter { !Self.isOwn($0) }
        if !others.isEmpty {
            return others.map { Self.participant(from: $0) }
        }
        // All authors are own → sent message. Show recipients instead.
        if let to = email.to, !to.isEmpty {
            let allRecipients = Self.parseAddressList(to)
            let externalRecipients = allRecipients.filter { !Self.isOwn($0) }
            if !externalRecipients.isEmpty {
                return externalRecipients.map { Self.participant(from: $0) }
            }
            // All recipients are also own (sent to self) — show them anyway
            if !allRecipients.isEmpty {
                return allRecipients.map { Self.participant(from: $0) }
            }
        }
        return authors.map { Participant(name: Self.extractName(from: $0), email: $0) }
    }

}

// MARK: - Truncated Tags View

/// Displays tag capsules that fit within the available width, hiding overflow with "+N".
/// Uses character-count estimation instead of view measurement for reliable layout.
private struct TruncatedTagsView: View {
    let tags: [String]
    let availableWidth: CGFloat
    let isSelected: Bool

    private let hPadding: CGFloat = 6
    private let spacing: CGFloat = 4
    private let indicatorWidth: CGFloat = 24

    private func tagWidth(_ tag: String) -> CGFloat {
        CGFloat(tag.count) * 6.5 + hPadding * 2
    }

    private var layout: (shown: [String], overflow: Int) {
        var used: CGFloat = 0
        var shown: [String] = []

        for (i, tag) in tags.enumerated() {
            let w = tagWidth(tag) + (shown.isEmpty ? 0 : spacing)
            let needsIndicator = i < tags.count - 1
            let reserved = needsIndicator ? indicatorWidth + spacing : 0

            if used + w + reserved <= availableWidth {
                used += w
                shown.append(tag)
            } else {
                break
            }
        }
        return (shown, tags.count - shown.count)
    }

    var body: some View {
        let result = layout
        HStack(spacing: spacing) {
            ForEach(result.shown, id: \.self) { tag in
                Text(tag)
                    .font(.caption2)
                    .lineLimit(1)
                    .foregroundStyle(isSelected ? .white.opacity(0.7) : .secondary)
                    .padding(.horizontal, hPadding)
                    .padding(.vertical, 2)
                    .background(
                        isSelected ? Color.white.opacity(0.15) : Color.gray.opacity(0.15),
                        in: Capsule()
                    )
            }
            if result.overflow > 0 {
                Text("+\(result.overflow)")
                    .font(.caption2)
                    .foregroundStyle(isSelected ? Color.white.opacity(0.5) : Color.secondary)
                    .accessibilityLabel("\(result.overflow) more tags")
            }
        }
    }
}

