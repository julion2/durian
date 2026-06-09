//
//  EmailSendingManager.swift
//  Durian
//
//  Manages email sending via the outbox HTTP API
//

import Combine
import Foundation

@MainActor
class EmailSendingManager: ObservableObject {
    static let shared = EmailSendingManager()

    static let sendDelay = 10

    @Published var isSending = false
    @Published var sendingProgress = ""
    @Published var lastError: EmailSendingError?

    /// Tracks a pending send that can still be undone.
    struct PendingUndo {
        let itemId: Int64
        let draftId: UUID
        let recipient: String
        let threadId: String?
        let timer: Timer
        var secondsLeft: Int
        let onUndo: () -> Void
        let onConfirmedSent: () -> Void
    }

    private(set) var pendingUndoInfo: PendingUndo?

    private init() {}

    // MARK: - Undo Send

    /// Returns true if the given outbox item is still in the undo countdown window.
    func isUndoActive(itemId: Int64) -> Bool {
        pendingUndoInfo?.itemId == itemId
    }

    /// Called by SyncManager when an SSE "sent" event arrives for an item
    /// that may still be in the undo window. Cleans up the timer.
    func handleSentEvent(itemId: Int64) {
        guard let pending = pendingUndoInfo, pending.itemId == itemId else { return }
        pending.timer.invalidate()
        pendingUndoInfo = nil
        BannerManager.shared.dismiss()
    }

    /// Starts the undo countdown banner after a successful enqueue.
    func startCountdown(itemId: Int64, draftId: UUID, recipient: String, threadId: String?, onUndo: @escaping () -> Void, onConfirmedSent: @escaping () -> Void) {
        // Cancel any existing countdown
        pendingUndoInfo?.timer.invalidate()

        let secondsLeft = Self.sendDelay

        // Create the repeating timer (fires on main run loop since we're @MainActor)
        let timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] t in
            Task { @MainActor in
                guard let self = self, var pending = self.pendingUndoInfo else {
                    t.invalidate()
                    return
                }

                pending.secondsLeft -= 1
                self.pendingUndoInfo = pending

                if pending.secondsLeft <= 0 {
                    // Countdown expired — email is committed to send
                    t.invalidate()
                    self.pendingUndoInfo = nil
                    BannerManager.shared.dismiss()
                    pending.onConfirmedSent()
                } else {
                    BannerManager.shared.updateMessage("Sending in \(pending.secondsLeft)s to \(pending.recipient)...")
                }
            }
        }

        pendingUndoInfo = PendingUndo(
            itemId: itemId,
            draftId: draftId,
            recipient: recipient,
            threadId: threadId,
            timer: timer,
            secondsLeft: secondsLeft,
            onUndo: onUndo,
            onConfirmedSent: onConfirmedSent
        )

        // Show the initial countdown banner with Undo button
        BannerManager.shared.showPersistentInfo(
            title: "Sending Email",
            message: "Sending in \(secondsLeft)s to \(recipient)...",
            actions: [
                BannerAction("Undo") { [weak self] in
                    Task { @MainActor in
                        self?.performUndo()
                    }
                }
            ],
            onTap: threadId != nil ? { AccountManager.shared.pendingNotificationThreadId = threadId } : nil
        )
    }

    /// Cancels the pending send: deletes outbox item, cleans up, and calls onUndo.
    private func performUndo() {
        guard let pending = pendingUndoInfo else { return }
        pending.timer.invalidate()
        pendingUndoInfo = nil
        BannerManager.shared.dismiss()

        let itemId = pending.itemId
        let undoCb = pending.onUndo

        // Delete from outbox, then re-show compose regardless of result
        Task {
            if let backend = AccountManager.shared.emailBackend {
                let deleted = await backend.deleteOutboxItem(id: itemId)
                if !deleted {
                    Log.warning("EMAIL", "Failed to delete outbox item \(itemId) on undo")
                }
            }
            await MainActor.run { undoCb() }
        }
    }

    /// Send email by enqueuing to the outbox via HTTP API.
    /// The background worker on the server handles actual SMTP delivery.
    /// - Parameters:
    ///   - draft: The email draft to send
    ///   - fromAccount: The account email to send from
    ///   - skipValidation: If true, skip email format validation (used when user confirms "Send Anyway")
    ///   - onUndo: Called if the user clicks "Undo" during the countdown window
    ///   - onConfirmedSent: Called when the countdown expires (email committed to send)
    func send(draft: EmailDraft, fromAccount accountEmail: String, skipValidation: Bool = false, onUndo: @escaping () -> Void = {}, onConfirmedSent: @escaping () -> Void = {}) async throws {
        guard draft.hasRecipients else {
            let error = EmailSendingError.invalidRecipients
            lastError = error
            throw error
        }

        // Validate email formats (unless skipped)
        if !skipValidation {
            let allRecipients = draft.to + draft.cc + draft.bcc
            let invalidEmails = EmailHelper.validateRecipients(allRecipients)

            if !invalidEmails.isEmpty {
                let error = EmailSendingError.invalidEmailFormat(invalidEmails)
                lastError = error
                throw error
            }
        }

        isSending = true
        sendingProgress = "Preparing email..."
        lastError = nil

        defer {
            isSending = false
            sendingProgress = ""
        }

        // Build final body by combining user text, HTML signature, and quoted content
        var finalBody = draft.body
        var finalIsHTML = draft.isHTML

        // Clean contentEditable artifacts from user-typed content only
        // (not from signature or quoted content where styles are intentional)
        let cleanRichHTML: String? = if let raw = draft.htmlBody, !raw.isEmpty {
            Self.cleanEditorArtifacts(raw)
        } else {
            nil
        }

        if let htmlSig = draft.htmlSignature, !htmlSig.isEmpty {
            let userHTML: String
            if let richHTML = cleanRichHTML {
                userHTML = richHTML
            } else {
                userHTML = draft.body
                    .replacingOccurrences(of: "&", with: "&amp;")
                    .replacingOccurrences(of: "<", with: "&lt;")
                    .replacingOccurrences(of: ">", with: "&gt;")
                    .replacingOccurrences(of: "\n", with: "<br>")
            }
            finalBody = "<div>\(userHTML)</div><br><div>-- <br></div>\(htmlSig)"

            if let quoted = draft.quotedContent, !quoted.isEmpty {
                let cleanQuoted = draft.quotedIsHTML ? Self.stripStyleTags(quoted) : quoted
                let quotedHTML = draft.quotedIsHTML ? cleanQuoted : Self.plainTextToHTML(cleanQuoted)
                finalBody += "<br><br>\(quotedHTML)"
            }

            finalIsHTML = true
        } else if let quoted = draft.quotedContent, !quoted.isEmpty {
            if draft.quotedIsHTML {
                let cleanQuoted = Self.stripStyleTags(quoted)
                let userHTML: String
                if let richHTML = cleanRichHTML {
                    userHTML = richHTML
                } else {
                    userHTML = draft.body
                        .replacingOccurrences(of: "&", with: "&amp;")
                        .replacingOccurrences(of: "<", with: "&lt;")
                        .replacingOccurrences(of: ">", with: "&gt;")
                        .replacingOccurrences(of: "\n", with: "<br>")
                }
                finalBody = "<div>\(userHTML)</div><br><br>\(cleanQuoted)"
                finalIsHTML = true
            } else {
                if let richHTML = cleanRichHTML {
                    let quotedHTML = Self.plainTextToHTML(quoted)
                    finalBody = "<div>\(richHTML)</div><br><br>\(quotedHTML)"
                    finalIsHTML = true
                } else {
                    finalBody = draft.body + "\n\n" + quoted
                }
            }
        } else if let richHTML = cleanRichHTML {
            finalBody = "<div>\(richHTML)</div>"
            finalIsHTML = true
        }

        // Build attachment payloads (base64-encoded)
        let attachmentPayloads = draft.attachments.map { att in
            OutboxAttachmentPayload(
                filename: att.filename,
                mime_type: att.mimeType,
                data_base64: att.data.base64EncodedString()
            )
        }

        // Build outbox payload
        let payload = OutboxPayload(
            from: accountEmail,
            to: draft.to,
            cc: draft.cc,
            bcc: draft.bcc,
            subject: draft.subject,
            body: finalBody,
            is_html: finalIsHTML,
            in_reply_to: draft.inReplyTo,
            references: draft.references,
            attachments: attachmentPayloads,
            delay_seconds: Self.sendDelay
        )

        // Enqueue via HTTP
        sendingProgress = "Queuing email..."
        Log.debug("EMAIL", "Enqueuing to outbox")
        Log.debug("EMAIL", "From: \(accountEmail)")
        Log.debug("EMAIL", "To: \(draft.to.joined(separator: ", "))")
        if !draft.cc.isEmpty {
            Log.debug("EMAIL", "CC: \(draft.cc.joined(separator: ", "))")
        }
        if !draft.bcc.isEmpty {
            Log.debug("EMAIL", "BCC: \(draft.bcc.joined(separator: ", "))")
        }
        Log.debug("EMAIL", "Subject: \(draft.subject)")
        if !draft.attachments.isEmpty {
            Log.debug("EMAIL", "Attachments: \(draft.attachments.count)")
        }

        guard let backend = AccountManager.shared.emailBackend else {
            let sendError = EmailSendingError.sendFailed("Mail server not connected")
            lastError = sendError
            BannerManager.shared.showCritical(title: "Cannot Send Email", message: "Mail server not connected.")
            throw sendError
        }

        let result = await backend.enqueueOutbox(payload)

        if result.ok {
            sendingProgress = "Email queued"
            let itemId = result.id ?? -1
            Log.info("EMAIL", "Enqueued successfully (id=\(itemId), send_after=\(result.sendAfter ?? 0))")

            // Update contact usage statistics
            let allRecipients = draft.to + draft.cc + draft.bcc
            updateContactUsage(for: allRecipients)

            // Refresh outbox count
            OutboxManager.shared.refresh()

            // Start undo countdown (banner shown by startCountdown)
            let recipient = draft.to.first ?? "recipient"
            startCountdown(
                itemId: itemId,
                draftId: draft.id,
                recipient: recipient,
                threadId: draft.replyThreadId,
                onUndo: onUndo,
                onConfirmedSent: onConfirmedSent
            )
        } else {
            let errorMessage = result.error ?? "Unknown error"
            Log.error("EMAIL", "Enqueue failed: \(errorMessage)")
            let sendError = EmailSendingError.sendFailed(errorMessage)
            lastError = sendError
            BannerManager.shared.showCritical(title: "Email Not Queued", message: errorMessage)
            throw sendError
        }
    }

    // MARK: - Contact Usage Tracking

    /// Update contact usage statistics for sent recipients
    private func updateContactUsage(for recipients: [String]) {
        guard !recipients.isEmpty else { return }

        let emails = recipients.map { extractEmail(from: $0) }
        ContactsManager.shared.incrementUsage(for: emails)
    }

    /// Extract email address from string (handles "Name <email>" format)
    private nonisolated func extractEmail(from address: String) -> String {
        let trimmed = address.trimmingCharacters(in: .whitespaces)

        if let startIdx = trimmed.lastIndex(of: "<"),
           let endIdx = trimmed.lastIndex(of: ">"),
           startIdx < endIdx
        {
            let start = trimmed.index(after: startIdx)
            return String(trimmed[start..<endIdx]).trimmingCharacters(in: .whitespaces)
        }

        return trimmed
    }

    /// Remove WebKit contentEditable artifacts that bloat the HTML and
    /// can break rendering (e.g. caret-color, hardcoded black color in dark mode).
    nonisolated static func cleanEditorArtifacts(_ html: String) -> String {
        var result = html
        // Strip class="isSelectedEnd" and similar WebKit-internal classes
        result = result.replacingOccurrences(
            of: #" class="isSelectedEnd""#, with: "", options: .caseInsensitive)
        // Strip caret-color (cursor color — meaningless in sent email)
        result = result.replacingOccurrences(
            of: #"caret-color: rgb\([^)]+\);?\s*"#, with: "", options: .regularExpression)
        // Strip hardcoded color: rgb(0,0,0) that breaks dark mode for recipients.
        // Only remove the exact black value — preserve intentional color choices.
        result = result.replacingOccurrences(
            of: #"color: rgb\(0,\s*0,\s*0\);?\s*"#, with: "", options: .regularExpression)
        // Strip paste-inherited -apple-system font-family (Apple-only value that
        // overrides the cross-platform font stack on the wrapper div)
        // Pass 1: preceded by semicolon (middle/end of style)
        result = result.replacingOccurrences(
            of: #";\s*font-family:\s*[^;"]*-apple-system[^;"]*"#, with: "", options: .regularExpression)
        // Pass 2: at start of style (with optional trailing semicolon)
        result = result.replacingOccurrences(
            of: #"font-family:\s*[^;"]*-apple-system[^;"]*;?\s*"#, with: "", options: .regularExpression)
        // Clean up empty style attributes left behind
        result = result.replacingOccurrences(
            of: #" style=\"\s*\""#, with: "", options: .regularExpression)
        return result
    }

    /// Strip <style>...</style> blocks from HTML to prevent CSS leakage
    /// from quoted content into the user's own message and signature.
    nonisolated static func stripStyleTags(_ html: String) -> String {
        html.replacingOccurrences(
            of: "<style[^>]*>[\\s\\S]*?</style>",
            with: "",
            options: [.regularExpression, .caseInsensitive]
        )
    }

    /// Convert plain text to basic HTML (for combining plain text quoted content with HTML signature)
    private static func plainTextToHTML(_ text: String) -> String {
        let escaped = text
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\n", with: "<br>")
        return "<div style=\"font-family: system-ui,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif; font-size: 13px; color: #666; white-space: pre-wrap;\">\(escaped)</div>"
    }
}
