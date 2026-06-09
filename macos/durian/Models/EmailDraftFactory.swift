//
//  EmailDraftFactory.swift
//  Durian
//
//  Reply / forward / draft factory methods on EmailDraft.
//  Split out from EmailComposition.swift to keep the model file focused
//  on data types.
//

import Foundation

// MARK: - Reply / Forward Factories

extension EmailDraft {
    /// Create a draft for editing an existing draft message
    static func createFromDraft(message: MailMessage) -> EmailDraft {
        let toAddresses = message.to.map { parseEmailList($0) } ?? []
        let ccAddresses = message.cc.map { parseEmailList($0) } ?? []

        var draft = EmailDraft(
            from: message.from,
            to: toAddresses,
            cc: ccAddresses,
            subject: message.subject,
            body: message.body ?? "",
            isHTML: message.htmlBody != nil && !(message.htmlBody?.isEmpty ?? true),
            messageId: message.messageId,
            htmlBody: message.htmlBody
        )
        draft.replyThreadId = message.id
        return draft
    }

    /// Returns the message ID of the message that should be quoted in a reply.
    /// Used to lazy-load the original (unstripped) body before creating the draft.
    static func replyTargetMessageId(for message: MailMessage, fromAccount: String) -> String? {
        findReplyTarget(message: message, fromAccount: fromAccount).bodySourceId
    }

    /// Create a reply draft from a mail message
    /// - Parameters:
    ///   - message: The original message to reply to
    ///   - fromAccount: The email address to send from
    ///   - originalBody: Optional unstripped body fetched via lazy-loading (text, html).
    ///     When provided, used for quoting instead of the stripped thread body.
    /// - Returns: A new EmailDraft configured as a reply
    static func createReply(from message: MailMessage, fromAccount: String,
                            originalBody: (body: String, html: String?)? = nil) -> EmailDraft
    {
        let target = findReplyTarget(message: message, fromAccount: fromAccount)

        // Fallback if target.from has no email (e.g. cache restored without headers)
        var replyTo = target.from
        if !replyTo.contains("@") {
            if let threadFrom = message.threadMessages?.last?.from, threadFrom.contains("@") {
                replyTo = threadFrom
            }
        }

        // Build subject with Re: prefix (avoid Re: Re: Re:)
        let subject = message.subject.hasPrefix("Re:")
            ? message.subject
            : "Re: \(message.subject)"

        // Build references chain from target message
        var references = target.references ?? ""
        if let messageId = target.messageId, !messageId.isEmpty {
            if !references.isEmpty {
                references += " "
            }
            references += messageId
        }

        // Use original (unstripped) body for quoting if available, otherwise fall back to stripped
        let quoteBody = originalBody?.body ?? target.body
        let quoteHTML = originalBody?.html ?? target.html
        let hasHTML = quoteHTML != nil && !quoteHTML!.isEmpty

        // Quote the target body (use HTML if available)
        let quotedBody = hasHTML
            ? quoteBodyHTML(quoteHTML!, from: target.from, date: target.date)
            : Self.quoteBody(quoteBody ?? "", from: target.from, date: target.date)

        var draft = EmailDraft(
            from: fromAccount,
            to: [replyTo],
            subject: subject,
            body: "",  // User writes here
            inReplyTo: target.messageId,
            references: references.isEmpty ? nil : references,
            quotedContent: quotedBody,
            quotedIsHTML: hasHTML
        )
        draft.replyThreadId = message.id
        return draft
    }

    /// Create a reply-all draft from a mail message
    /// - Parameters:
    ///   - message: The original message to reply to
    ///   - fromAccount: The email address to send from
    /// - Returns: A new EmailDraft configured as a reply-all
    static func createReplyAll(from message: MailMessage, fromAccount: String,
                               originalBody: (body: String, html: String?)? = nil) -> EmailDraft
    {
        var draft = createReply(from: message, fromAccount: fromAccount, originalBody: originalBody)
        let target = findReplyTarget(message: message, fromAccount: fromAccount)

        // Build CC from the TARGET message's To/CC (not thread-level fields,
        // which may be from the user's own sent message)
        var ccRecipients: [String] = []

        // Add target's To recipients (except the reply-to sender and self)
        if let originalTo = target.to {
            let toAddresses = parseEmailList(originalTo)
            let senderEmail = extractEmail(from: draft.to.first ?? target.from).lowercased()
            for address in toAddresses {
                let emailOnly = extractEmail(from: address).lowercased()
                if emailOnly != fromAccount.lowercased() && emailOnly != senderEmail {
                    ccRecipients.append(address)
                }
            }
        }

        // Add target's CC recipients (except self)
        if let originalCC = target.cc {
            let ccAddresses = parseEmailList(originalCC)
            for address in ccAddresses {
                let emailOnly = extractEmail(from: address).lowercased()
                if emailOnly != fromAccount.lowercased() {
                    ccRecipients.append(address)
                }
            }
        }

        draft.cc = ccRecipients
        return draft
    }

    /// Create a forward draft from a mail message (without attachments).
    /// Use `collectForwardAttachments(from:backend:)` to populate attachments.
    static func createForward(from message: MailMessage, fromAccount: String) -> EmailDraft {
        // Build subject with Fwd: prefix
        let subject = message.subject.hasPrefix("Fwd:")
            ? message.subject
            : "Fwd: \(message.subject)"

        // Forward all messages in the thread (oldest first)
        if let threadMessages = message.threadMessages, !threadMessages.isEmpty {
            let anyHTML = threadMessages.contains { $0.html != nil && !($0.html!.isEmpty) }
            let forwardedBody = anyHTML
                ? buildForwardThreadHTML(threadMessages)
                : buildForwardThread(threadMessages)

            var draft = EmailDraft(
                from: fromAccount,
                to: [],
                subject: subject,
                body: "",
                quotedContent: forwardedBody,
                quotedIsHTML: anyHTML
            )
            draft.replyThreadId = message.id
            return draft
        }

        // Fallback: single message (no thread loaded)
        let hasHTML = message.htmlBody != nil && !message.htmlBody!.isEmpty
        let forwardedBody = hasHTML
            ? buildForwardBodyHTML(message)
            : buildForwardBody(message)

        var draft = EmailDraft(
            from: fromAccount,
            to: [],
            subject: subject,
            body: "",
            quotedContent: forwardedBody,
            quotedIsHTML: hasHTML
        )
        draft.replyThreadId = message.id
        return draft
    }

    /// Result of collecting attachments for a forward, including any that had to be skipped.
    struct ForwardAttachmentResult {
        var attachments: [EmailAttachment]
        var skipped: [String]  // filenames that couldn't be downloaded or were too large
    }

    /// Download non-inline attachments from a message (or all messages in its thread)
    /// so they can be included in a forward draft. Inline attachments referenced by
    /// cid: in the HTML are intentionally skipped — they're already embedded in the
    /// forwarded body.
    ///
    /// Returns the downloaded attachments plus filenames of any that had to be skipped
    /// due to size limits or download errors. The 25 MB per-file / 50 MB total /
    /// 10-file limits from ComposeForm apply.
    static func collectForwardAttachments(
        from message: MailMessage,
        backend: EmailBackend
    ) async -> ForwardAttachmentResult {
        // Attachments are downloaded via /messages/{message_id}/attachments/{part_id}.
        // ThreadMessage already carries per-message attachment metadata including
        // partId, so thread-based forwards work directly. Single-message fallback
        // is intentionally skipped — incomingAttachments lacks partId, and in
        // practice forwardSelected() requires the body to be loaded which also
        // populates threadMessages.
        var sources: [(messageId: String, atts: [AttachmentInfo])] = []

        if let threadMessages = message.threadMessages, !threadMessages.isEmpty {
            for tm in threadMessages {
                guard let atts = tm.attachments, !atts.isEmpty else { continue }
                sources.append((tm.id, atts))
            }
        }

        var result = ForwardAttachmentResult(attachments: [], skipped: [])
        let maxPerFile: Int64 = 25 * 1024 * 1024
        let maxTotal: Int64 = 50 * 1024 * 1024
        let maxCount = 10
        var totalSize: Int64 = 0

        for source in sources {
            for att in source.atts {
                // Skip inline attachments — they're embedded via cid: in the HTML body
                if att.disposition.lowercased() == "inline" {
                    continue
                }

                // Enforce limits before download
                if result.attachments.count >= maxCount {
                    result.skipped.append(att.filename)
                    continue
                }
                if Int64(att.size) > maxPerFile {
                    result.skipped.append(att.filename)
                    continue
                }
                if totalSize + Int64(att.size) > maxTotal {
                    result.skipped.append(att.filename)
                    continue
                }

                do {
                    let (data, _) = try await backend.downloadAttachment(
                        messageId: source.messageId,
                        partId: att.partId
                    )
                    result.attachments.append(EmailAttachment(
                        filename: att.filename,
                        mimeType: att.contentType,
                        data: data
                    ))
                    totalSize += Int64(data.count)
                } catch {
                    Log.warning("COMPOSE", "Forward attachment download failed: \(att.filename) — \(error)")
                    result.skipped.append(att.filename)
                }
            }
        }

        return result
    }

    // MARK: - Reply Target Resolution

    /// Fields needed to construct a reply from a specific thread message.
    private struct ReplyTarget {
        let bodySourceId: String?  // message ID for fetching original body
        let from: String
        let to: String?
        let cc: String?
        let date: String
        let body: String?
        let html: String?
        let messageId: String?
        let references: String?
    }

    /// Find the correct message to reply to in a thread.
    ///
    /// When the newest message in a thread was sent by the current user,
    /// replying to it would set To: to ourselves. This method finds the
    /// appropriate non-self message to reply to instead.
    private static func findReplyTarget(message: MailMessage, fromAccount: String) -> ReplyTarget {
        let accountEmail = fromAccount.lowercased()
        let newestFrom = extractEmail(from: message.from).lowercased()

        // Case 1: newest message is not from self — use as-is
        guard newestFrom == accountEmail else {
            return ReplyTarget(bodySourceId: message.threadMessages?.first?.id,
                               from: message.from, to: message.to, cc: message.cc,
                               date: message.date, body: message.body, html: message.htmlBody,
                               messageId: message.messageId, references: message.references)
        }

        // Case 2: newest is from self — find the most recent non-self message
        if let threads = message.threadMessages {
            for tm in threads {
                if extractEmail(from: tm.from).lowercased() != accountEmail {
                    return ReplyTarget(bodySourceId: tm.id,
                                       from: tm.from, to: tm.to, cc: tm.cc,
                                       date: tm.date, body: tm.body, html: tm.html,
                                       messageId: tm.message_id, references: tm.references)
                }
            }
        }

        // Case 3: all messages from self — reply to original recipients
        return ReplyTarget(bodySourceId: message.threadMessages?.first?.id,
                           from: message.to ?? message.from, to: message.to, cc: message.cc,
                           date: message.date, body: message.body, html: message.htmlBody,
                           messageId: message.messageId, references: message.references)
    }

    // MARK: - Address Parsing Helpers

    /// Extract email address from various formats:
    /// - "Name <email>" -> "email"
    /// - "<Name> email" -> "email"  (malformed but common)
    /// - "email" -> "email"
    private static func extractEmail(from address: String) -> String {
        let trimmed = address.trimmingCharacters(in: .whitespaces)

        // Standard format: "Name <email>"
        if let start = trimmed.firstIndex(of: "<"),
           let end = trimmed.firstIndex(of: ">"),
           start < end
        {
            let email = String(trimmed[trimmed.index(after: start)..<end])
            // Validate it looks like an email
            if email.contains("@") {
                return email.trimmingCharacters(in: .whitespaces)
            }
        }

        // Malformed format: "<Name> email" - extract last word if it contains @
        let parts = trimmed.components(separatedBy: .whitespaces)
        if let lastPart = parts.last, lastPart.contains("@") {
            return lastPart
        }

        // Just return as-is (probably already an email)
        return trimmed
    }

    /// Parse comma-separated email list, handling commas in unquoted display names
    /// e.g. "van der Zee, Warden (EBV) <a@b.com>, c@d.com" → ["van der Zee, Warden (EBV) <a@b.com>", "c@d.com"]
    private static func parseEmailList(_ list: String) -> [String] {
        var results: [String] = []
        var current = ""
        var inQuotes = false
        var inAngleBracket = false

        for char in list {
            switch char {
            case "\"":
                inQuotes.toggle()
                current.append(char)
            case "<":
                inAngleBracket = true
                current.append(char)
            case ">":
                inAngleBracket = false
                current.append(char)
            case ",":
                if inQuotes || inAngleBracket {
                    current.append(char)
                } else if current.contains("<") && current.contains(">") {
                    // Complete "Name <email>" address — comma is a separator
                    let trimmed = current.trimmingCharacters(in: .whitespaces)
                    if !trimmed.isEmpty { results.append(trimmed) }
                    current = ""
                } else if current.contains("@") {
                    // Plain email without angle brackets — comma is a separator
                    let trimmed = current.trimmingCharacters(in: .whitespaces)
                    if !trimmed.isEmpty { results.append(trimmed) }
                    current = ""
                } else {
                    // No complete address yet — comma is part of display name
                    current.append(char)
                }
            default:
                current.append(char)
            }
        }

        let trimmed = current.trimmingCharacters(in: .whitespaces)
        if !trimmed.isEmpty { results.append(trimmed) }
        return results
    }

    // MARK: - Body Builders (Reply)

    /// Quote body text for reply (plain text)
    private static func quoteBody(_ body: String, from: String, date: String) -> String {
        var quoted = "On \(date), \(from) wrote:\n"

        let lines = body.components(separatedBy: .newlines)
        for line in lines {
            quoted += "> \(line)\n"
        }

        return quoted
    }

    /// Quote body HTML for reply (preserves formatting)
    private static func quoteBodyHTML(_ html: String, from: String, date: String) -> String {
        """
        <div style="color: #555;">
        <p style="font-size: 12px; color: #888; margin-bottom: 8px;">On \(escapeHTML(date)), \(escapeHTML(from)) wrote:</p>
        <div style="border-left: 2px solid #ccc; padding-left: 10px; margin-left: 5px;">
        \(html)
        </div>
        </div>
        """
    }

    // MARK: - Body Builders (Forward)

    /// Build forwarded thread body (plain text, all messages oldest-first)
    private static func buildForwardThread(_ messages: [ThreadMessage]) -> String {
        // Thread messages arrive newest-first from API; keep that order
        // in forwards so the most recent reply is at the top.
        messages.map { msg in
            var part = "---------- Forwarded message ----------\n"
            part += "From: \(msg.from)\n"
            if let to = msg.to { part += "To: \(to)\n" }
            part += "Date: \(msg.date)\n"
            part += "\n"
            part += msg.body
            return part
        }.joined(separator: "\n\n")
    }

    /// Build forwarded thread body (HTML, newest-first to match the app view)
    private static func buildForwardThreadHTML(_ messages: [ThreadMessage]) -> String {
        // Thread messages arrive newest-first from API; keep that order
        // in forwards so the most recent reply is at the top.
        let parts = messages.map { msg -> String in
            var html = """
            <div style="color: #666; margin-bottom: 16px;">
            <p style="font-size: 12px; color: #888; margin-bottom: 8px;">---------- Forwarded message ----------</p>
            <p style="font-size: 12px; margin-bottom: 8px;">
            <b>From:</b> \(escapeHTML(msg.from))<br>
            """
            if let to = msg.to {
                html += "<b>To:</b> \(escapeHTML(to))<br>"
            }
            html += """
            <b>Date:</b> \(escapeHTML(msg.date))
            </p>
            <hr style="border: none; border-top: 1px solid #ccc; margin: 8px 0;">
            \(msg.html ?? msg.body)
            </div>
            """
            return html
        }
        return parts.joined(separator: "\n")
    }

    /// Build forwarded message body with original headers (plain text)
    private static func buildForwardBody(_ message: MailMessage) -> String {
        var body = "---------- Forwarded message ----------\n"
        body += "From: \(message.from)\n"
        if let to = message.to {
            body += "To: \(to)\n"
        }
        body += "Date: \(message.date)\n"
        body += "Subject: \(message.subject)\n"
        body += "\n"
        body += message.body ?? ""
        return body
    }

    /// Build forwarded message body with original headers (HTML)
    private static func buildForwardBodyHTML(_ message: MailMessage) -> String {
        var html = """
        <div style="color: #666;">
        <p style="font-size: 12px; color: #888; margin-bottom: 8px;">---------- Forwarded message ----------</p>
        <p style="font-size: 12px; margin-bottom: 8px;">
        <b>From:</b> \(escapeHTML(message.from))<br>
        """
        if let to = message.to {
            html += "<b>To:</b> \(escapeHTML(to))<br>"
        }
        html += """
        <b>Date:</b> \(escapeHTML(message.date))<br>
        <b>Subject:</b> \(escapeHTML(message.subject))
        </p>
        <hr style="border: none; border-top: 1px solid #ccc; margin: 8px 0;">
        \(message.htmlBody ?? message.body ?? "")
        </div>
        """
        return html
    }

    /// Escape HTML special characters
    private static func escapeHTML(_ string: String) -> String {
        string
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\"", with: "&quot;")
    }
}
