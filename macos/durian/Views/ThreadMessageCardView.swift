//
//  ThreadMessageCardView.swift
//  Durian
//
//  Per-message card used in the thread view of EmailDetailView.
//  Split out of EmailDetailView.swift to keep that file focused on
//  thread-level layout and header/footer composition.
//

import AppKit
import Quartz
import SwiftUI
import UniformTypeIdentifiers

struct ThreadMessageCardView: View {
    let message: ThreadMessage
    let isFirst: Bool
    let isLast: Bool
    let email: MailMessage  // Parent email for expanded details
    @Binding var contentHeight: CGFloat
    var isFocused: Bool = false
    let onReply: () -> Void
    let onReplyAll: () -> Void
    let onForward: () -> Void
    var onEditDraft: (() -> Void)? = nil

    // Each card manages its own expanded state
    @State private var isDetailsExpanded: Bool = false
    @State private var downloadStates: [Int: AttachmentDownloadState] = [:]
    @State private var selectedAttachmentId: Int? = nil
    @State private var spaceMonitor: AnyObject? = nil
    @State private var resolvedHTML: String? = nil
    @State private var showSignature: Bool = false
    @State private var signatureHeight: CGFloat = 0

    /// Non-inline attachments for this message
    private var displayAttachments: [AttachmentInfo] {
        (message.attachments ?? []).filter { $0.disposition != "inline" }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            senderRow

            if isDetailsExpanded {
                expandedDetails
            }

            // Attachment bar
            if !displayAttachments.isEmpty {
                attachmentBar
            }

            // Content: prefer HTML, fallback to plain text
            if let html = resolvedHTML ?? message.html, !html.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                NonScrollingWebView(
                    html: html,
                    theme: SettingsManager.shared.settings.theme,
                    loadRemoteImages: SettingsManager.shared.settings.loadRemoteImages,
                    emailId: message.id,
                    contentHeight: $contentHeight
                )
                .frame(height: max(contentHeight, 50))
                .task(id: message.id) {
                    await resolveInlineImages()
                }
            } else if !message.body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                Text(message.body)
                    .font(.system(size: 14))
                    .foregroundColor(Color.Detail.textPrimary)
                    .textSelection(.enabled)
            }

            // Hidden signature (collapsed by default, expand via "..." button)
            if let sig = message.hidden_signature, !sig.isEmpty {
                if showSignature {
                    NonScrollingWebView(
                        html: sig,
                        theme: SettingsManager.shared.settings.theme,
                        loadRemoteImages: SettingsManager.shared.settings.loadRemoteImages,
                        emailId: "\(message.id)-sig",
                        contentHeight: $signatureHeight
                    )
                    .frame(height: max(signatureHeight, 20))
                }
                Button {
                    withAnimation(.easeInOut(duration: 0.2)) {
                        showSignature.toggle()
                    }
                } label: {
                    Text(showSignature ? "Hide signature" : "•••")
                        .font(.system(size: 12))
                        .foregroundColor(Color.Detail.textTertiary)
                }
                .buttonStyle(.plain)
            }

            // Action footer only on last (newest) card
            if isLast {
                actionFooter
            }
        }
        // Click anywhere outside attachment chips clears selection
        .onTapGesture { selectedAttachmentId = nil }
        .padding(.top, 24)
        .padding(.horizontal, 24)
        .padding(.bottom, 16)
        .background(Color.Detail.cardBackground)
        .cornerRadius(10)
        .overlay(
            HStack(spacing: 0) {
                if isFocused {
                    ProfileManager.shared.resolvedAccentColor.frame(width: 3)
                }
                Spacer()
            }
            .clipShape(RoundedRectangle(cornerRadius: 10))
        )
        .shadow(color: Color.primary.opacity(0.1), radius: 3, x: 0, y: 1)
        .padding(.leading, isOwnMessage() ? 56 : 32)  // Indent own messages (24pt extra)
        .padding(.trailing, 32)
        .padding(.top, isFirst ? 24 : 0)
        .padding(.bottom, isLast ? 32 : 16)
    }

    // MARK: - Own Message Detection

    /// Check if message is from one of the configured accounts
    private func isOwnMessage() -> Bool {
        let fromEmail = extractEmail(from: message.from).lowercased()
        let ownEmails = ConfigManager.shared.getAccounts().map { $0.email.lowercased() }
        return ownEmails.contains(fromEmail)
    }

    /// Extract email address from "Name <email>" format
    private func extractEmail(from: String) -> String {
        if let start = from.range(of: "<"), let end = from.range(of: ">") {
            return String(from[start.upperBound..<end.lowerBound])
        }
        return from
    }

    // MARK: - Sender Row

    @ViewBuilder
    private var senderRow: some View {
        HStack(alignment: .top, spacing: 12) {
            AvatarView(name: message.from, email: message.from, size: 40)

            VStack(alignment: .leading, spacing: 2) {
                Text(extractName(from: message.from))
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundColor(Color.Detail.textPrimary)

                // To/Cc line with expand chevron
                recipientsRow
            }

            Spacer()

            Text(formatDate(message.date))
                .font(.system(size: 14))
                .foregroundColor(Color.Detail.textTertiary)
                .lineLimit(1)
        }
    }

    // MARK: - Recipients Row (To/Cc)

    @ViewBuilder
    private var recipientsRow: some View {
        HStack(spacing: 4) {
            // To recipients (from message, fallback to parent email)
            if let to = message.to ?? email.to, !to.isEmpty {
                Text("To:")
                    .foregroundColor(Color.Detail.textTertiary)
                Text(extractRecipientNames(to).joined(separator: ", "))
                    .foregroundColor(Color.Detail.textSecondary)
                    .lineLimit(1)
            }

            // Cc recipients (only if present)
            if let cc = message.cc ?? email.cc, !cc.isEmpty {
                Text("Cc:")
                    .foregroundColor(Color.Detail.textTertiary)
                Text(extractRecipientNames(cc).joined(separator: ", "))
                    .foregroundColor(Color.Detail.textSecondary)
                    .lineLimit(1)
            }

            // Expand chevron
            Image(systemName: "chevron.right")
                .font(.system(size: 12, weight: .medium))
                .foregroundColor(Color.Detail.textSecondary)
                .rotationEffect(.degrees(isDetailsExpanded ? 90 : 0))
                .animation(.easeInOut(duration: 0.2), value: isDetailsExpanded)
        }
        .font(.system(size: 14))
        .contentShape(Rectangle())
        .onTapGesture {
            withAnimation(.easeInOut(duration: 0.2)) {
                isDetailsExpanded.toggle()
            }
        }
    }

    /// Extract clean names from recipient list
    /// "\"Lisa Neumayer | kmpro\" <l@x.de>, \"Max Müller\" <m@x.de>" → ["Lisa Neumayer", "Max Müller"]
    private func extractRecipientNames(_ recipients: String) -> [String] {
        // Split by comma, but be careful with commas inside quotes
        let parts = recipients.components(separatedBy: ">,")

        return parts.compactMap { part in
            let trimmed = part.trimmingCharacters(in: .whitespaces)
            guard !trimmed.isEmpty else { return nil }

            // Add back the ">" if it was removed by split (except for last part)
            let fullPart = trimmed.hasSuffix(">") ? trimmed : trimmed + ">"

            // Use extractName to get clean name
            let name = extractName(from: fullPart)

            // Remove "| domain" suffix if present
            if let pipeRange = name.range(of: " |") {
                return String(name[..<pipeRange.lowerBound]).trimmingCharacters(in: .whitespaces)
            }

            if !name.isEmpty { return name }
            // Fallback: show local part of email
            let email = AddressUtils.extractEmail(from: fullPart)
            if let at = email.firstIndex(of: "@") {
                return String(email[..<at])
            }
            return nil
        }
    }

    // MARK: - Attachment Bar

    @ViewBuilder
    private var attachmentBar: some View {
        VStack(alignment: .leading, spacing: 6) {
            if displayAttachments.count > 1 {
                Button {
                    saveAllAttachments()
                } label: {
                    HStack(spacing: 4) {
                        Image(systemName: "arrow.down.circle")
                            .font(.system(size: 11))
                        Text("Save All (\(displayAttachments.count))")
                            .font(.system(size: 11, weight: .medium))
                    }
                    .foregroundColor(Color.Detail.textTertiary)
                }
                .buttonStyle(.plain)
            }
            FlowLayout(spacing: 8) {
                ForEach(displayAttachments, id: \.partId) { attachment in
                    attachmentChip(attachment)
                }
            }
        }
        .onAppear {
            KeymapHandler.shared.attachmentSelected = selectedAttachmentId != nil
            spaceMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
                guard selectedAttachmentId != nil else { return event }
                // Escape: deselect attachment
                if event.keyCode == 53 {
                    selectedAttachmentId = nil
                    return nil
                }
                // Space: toggle QuickLook preview
                guard event.keyCode == 49 else { return event }
                if let panel = QLPreviewPanel.shared(), panel.isVisible {
                    panel.close()
                } else if let attachment = displayAttachments.first(where: { $0.partId == selectedAttachmentId }) {
                    previewAttachment(attachment)
                }
                return nil
            } as AnyObject?
        }
        .onDisappear {
            if let monitor = spaceMonitor { NSEvent.removeMonitor(monitor); spaceMonitor = nil }
            if let panel = QLPreviewPanel.shared(), panel.isVisible { panel.close() }
            KeymapHandler.shared.attachmentSelected = false
        }
        .onChange(of: selectedAttachmentId) { _, newValue in
            KeymapHandler.shared.attachmentSelected = newValue != nil
        }
    }

    @ViewBuilder
    private func attachmentChip(_ attachment: AttachmentInfo) -> some View {
        let state = downloadStates[attachment.partId] ?? .notDownloaded
        let sizeLabel = ByteCountFormatter.string(fromByteCount: Int64(attachment.size), countStyle: .file)
        let isFailed = if case .failed = state { true } else { false }
        let isDownloading = if case .downloading = state { true } else { false }
        let isSelected = selectedAttachmentId == attachment.partId

        HStack(spacing: 8) {
            // File type icon in rounded rect box
            ZStack {
                RoundedRectangle(cornerRadius: 4)
                    .fill(isFailed ? Color.red.opacity(0.08) : Color(NSColor.separatorColor).opacity(0.3))
                    .frame(width: 28, height: 28)
                if isDownloading {
                    ProgressView()
                        .controlSize(.small)
                } else {
                    Image(nsImage: fileTypeIcon(for: attachment))
                        .resizable()
                        .aspectRatio(contentMode: .fit)
                        .frame(width: 20, height: 20)
                }
            }
            VStack(alignment: .leading, spacing: 1) {
                Text(attachment.filename)
                    .font(.system(size: 12, weight: .medium))
                    .lineLimit(1)
                    .truncationMode(.middle)
                Text(sizeLabel)
                    .font(.system(size: 10))
                    .foregroundColor(isSelected ? ProfileManager.shared.resolvedAccentColor.opacity(0.8) : Color.Detail.textTertiary)
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(
            isFailed ? Color.red.opacity(0.12) :
            isSelected ? ProfileManager.shared.resolvedAccentColor.opacity(0.1) :
            Color(NSColor.controlBackgroundColor)
        )
        .foregroundColor(isFailed ? .red : isSelected ? ProfileManager.shared.resolvedAccentColor : Color.Detail.textSecondary)
        .cornerRadius(8)
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(
                    isFailed ? Color.red.opacity(0.3) :
                    isSelected ? ProfileManager.shared.resolvedAccentColor.opacity(0.5) :
                    Color(NSColor.separatorColor).opacity(0.5),
                    lineWidth: isSelected ? 1.5 : 0.5
                )
        )
        .contentShape(Rectangle())
        .onTapGesture {
            guard !isDownloading else { return }
            selectedAttachmentId = isSelected ? nil : attachment.partId
        }
        .contextMenu {
            Button("Save to Downloads") {
                saveToDownloads(attachment)
            }
            Button("Save As...") {
                saveAttachment(attachment)
            }
        }
    }

    private func fileTypeIcon(for attachment: AttachmentInfo) -> NSImage {
        let utType = UTType(mimeType: attachment.contentType)
            ?? UTType(filenameExtension: (attachment.filename as NSString).pathExtension)
            ?? .data
        return NSWorkspace.shared.icon(for: utType)
    }

    private func previewAttachment(_ attachment: AttachmentInfo) {
        downloadStates[attachment.partId] = .downloading(progress: 0)

        Task {
            guard let data = await fetchAttachmentData(attachment) else { return }
            let emailAttachment = EmailAttachment(
                filename: attachment.filename,
                mimeType: attachment.contentType,
                data: data
            )
            QuickLookManager.shared.showPreview(for: [emailAttachment], startingAt: 0)
            downloadStates[attachment.partId] = .notDownloaded
        }
    }

    private func saveToDownloads(_ attachment: AttachmentInfo) {
        guard let downloadsURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first else { return }
        let saveURL = downloadsURL.appendingPathComponent(attachment.filename)

        downloadStates[attachment.partId] = .downloading(progress: 0)

        Task {
            guard let data = await fetchAttachmentData(attachment) else { return }
            do {
                try data.write(to: saveURL)
                downloadStates[attachment.partId] = .downloaded(cachePath: saveURL.path)
                BannerManager.shared.showSuccess(title: "Saved", message: "\(attachment.filename) saved to Downloads")
                Log.info("ATTACHMENT", "Saved \(attachment.filename) to Downloads")
            } catch {
                downloadStates[attachment.partId] = .failed(error: error.localizedDescription)
                Log.error("ATTACHMENT", "Failed to write \(attachment.filename): \(error)")
                scheduleErrorClear(attachment.partId)
            }
        }
    }

    private func saveAllAttachments() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = false
        panel.canChooseDirectories = true
        panel.canCreateDirectories = true
        panel.prompt = "Save All"

        guard panel.runModal() == .OK, let folderURL = panel.url else { return }

        for attachment in displayAttachments {
            downloadStates[attachment.partId] = .downloading(progress: 0)
        }

        Task {
            var savedCount = 0
            for attachment in displayAttachments {
                guard let data = await fetchAttachmentData(attachment) else { continue }
                let saveURL = folderURL.appendingPathComponent(attachment.filename)
                do {
                    try data.write(to: saveURL)
                    downloadStates[attachment.partId] = .downloaded(cachePath: saveURL.path)
                    savedCount += 1
                    Log.info("ATTACHMENT", "Saved \(attachment.filename) to \(folderURL.lastPathComponent)/")
                } catch {
                    downloadStates[attachment.partId] = .failed(error: error.localizedDescription)
                    scheduleErrorClear(attachment.partId)
                }
            }
            if savedCount > 0 {
                BannerManager.shared.showSuccess(title: "Saved", message: "\(savedCount) attachment\(savedCount == 1 ? "" : "s") saved to \(folderURL.lastPathComponent)/")
            }
        }
    }

    private func saveAttachment(_ attachment: AttachmentInfo) {
        let panel = NSSavePanel()
        panel.nameFieldStringValue = attachment.filename
        panel.canCreateDirectories = true

        guard panel.runModal() == .OK, let saveURL = panel.url else { return }

        downloadStates[attachment.partId] = .downloading(progress: 0)

        Task {
            guard let data = await fetchAttachmentData(attachment) else { return }
            do {
                try data.write(to: saveURL)
                downloadStates[attachment.partId] = .downloaded(cachePath: saveURL.path)
                Log.info("ATTACHMENT", "Saved \(attachment.filename) to \(saveURL.path)")
            } catch {
                downloadStates[attachment.partId] = .failed(error: error.localizedDescription)
                Log.error("ATTACHMENT", "Failed to write \(attachment.filename): \(error)")
                scheduleErrorClear(attachment.partId)
            }
        }
    }

    private func fetchAttachmentData(_ attachment: AttachmentInfo) async -> Data? {
        // Check cache first
        if let cached = AttachmentCacheManager.shared.get(messageId: message.id, partId: attachment.partId) {
            Log.debug("ATTACHMENT", "Cache hit for \(attachment.filename)")
            return cached
        }

        guard let backend = AccountManager.shared.emailBackend else {
            downloadStates[attachment.partId] = .failed(error: "Not connected")
            scheduleErrorClear(attachment.partId)
            return nil
        }
        do {
            let (data, _) = try await backend.downloadAttachment(
                messageId: message.id,
                partId: attachment.partId
            )
            // Cache for future access
            AttachmentCacheManager.shared.put(
                messageId: message.id, partId: attachment.partId,
                filename: attachment.filename, data: data
            )
            return data
        } catch {
            downloadStates[attachment.partId] = .failed(error: error.localizedDescription)
            Log.error("ATTACHMENT", "Download failed for \(attachment.filename): \(error)")
            scheduleErrorClear(attachment.partId)
            return nil
        }
    }

    /// Resolve cid: references in HTML by fetching inline attachment data in parallel
    private func resolveInlineImages() async {
        guard let html = message.html else { return }

        let inlineAttachments = (message.attachments ?? []).filter {
            $0.disposition == "inline" && $0.contentId != nil && !$0.contentId!.isEmpty
        }
        guard !inlineAttachments.isEmpty else { return }
        guard html.contains("cid:") else { return }

        // Fetch all inline images in parallel
        let fetched = await withTaskGroup(of: (String, String?).self, returning: [(String, String)].self) { group in
            for attachment in inlineAttachments {
                guard let contentId = attachment.contentId else { continue }
                let cleanId = contentId.trimmingCharacters(in: CharacterSet(charactersIn: "<>"))
                guard html.contains("cid:\(cleanId)") else { continue }

                group.addTask {
                    guard let data = await fetchAttachmentData(attachment) else {
                        return (cleanId, nil)
                    }
                    let base64 = data.base64EncodedString()
                    return (cleanId, "data:\(attachment.contentType);base64,\(base64)")
                }
            }

            var results: [(String, String)] = []
            for await (cleanId, dataURL) in group {
                if let dataURL { results.append((cleanId, dataURL)) }
            }
            return results
        }

        guard !fetched.isEmpty else { return }

        var resolved = html
        for (cleanId, dataURL) in fetched {
            resolved = resolved.replacingOccurrences(of: "cid:\(cleanId)", with: dataURL)
        }

        if resolved != html {
            await MainActor.run {
                resolvedHTML = resolved
            }
        }
    }

    private func scheduleErrorClear(_ partId: Int) {
        Task {
            try? await Task.sleep(for: .seconds(5))
            if case .failed = downloadStates[partId] {
                downloadStates[partId] = .notDownloaded
            }
        }
    }

    // MARK: - Expanded Details

    @ViewBuilder
    private var expandedDetails: some View {
        VStack(alignment: .leading, spacing: 8) {
            // From (from message)
            if message.from.contains("<") || message.from.contains("@") {
                detailRow(label: "From", value: message.from)
            }

            // To (from message if available, else from parent email)
            if let to = message.to, !to.isEmpty {
                detailRow(label: "To", value: to)
            } else if let to = email.to, !to.isEmpty {
                detailRow(label: "To", value: to)
            }

            // Cc (from message if available, else from parent email)
            if let cc = message.cc, !cc.isEmpty {
                detailRow(label: "Cc", value: cc)
            } else if let cc = email.cc, !cc.isEmpty {
                detailRow(label: "Cc", value: cc)
            }

            // Tags (from message)
            if let tags = message.tags, !tags.isEmpty {
                detailRow(label: "Tags", value: tags.joined(separator: ", "))
            }

            // Message-ID (from message)
            detailRow(label: "Message-ID", value: message.id)
        }
        .padding(.leading, 52)
        .padding(.top, 8)
        .transition(.opacity.combined(with: .move(edge: .top)))
    }

    @ViewBuilder
    private func detailRow(label: String, value: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Text("\(label):")
                .font(.system(size: 13))
                .foregroundColor(Color.Detail.textTertiary)

            Text(value)
                .font(.system(size: 13))
                .foregroundColor(Color.Detail.textSecondary)
                .textSelection(.enabled)
        }
    }

    // MARK: - Action Footer

    @ViewBuilder
    private var actionFooter: some View {
        HStack {
            Spacer()

            if message.isDraft, let onEditDraft = onEditDraft {
                Button(action: onEditDraft) {
                    HStack(spacing: 6) {
                        Image(systemName: "pencil")
                            .font(.system(size: 14))
                        Text("Edit Draft")
                            .font(.system(size: 14, weight: .medium))
                    }
                    .padding(.horizontal, 12)
                    .padding(.vertical, 6)
                    .background(ProfileManager.shared.resolvedAccentColor)
                    .foregroundColor(.white)
                    .cornerRadius(6)
                }
                .buttonStyle(.plain)
                .help("Edit Draft")
            } else {
                Button(action: onReply) {
                    Image(systemName: "arrowshape.turn.up.left")
                        .font(.system(size: 16))
                        .foregroundColor(Color.Detail.textTertiary)
                        .frame(width: 36, height: 36)
                }
                .buttonStyle(.plain)
                .help("Reply")
            }
        }
        .padding(.top, 8)
    }

    // MARK: - Helper Methods

    private func extractName(from: String) -> String {
        AddressUtils.extractName(from: from)
    }

    /// Format RFC 2822 date string to readable format
    private func formatDate(_ dateString: String) -> String {
        // Parse RFC 2822 format: "Tue, 30 Dec 2025 17:20:47 +0100"
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss Z"

        guard let date = formatter.date(from: dateString) else {
            // Fallback: try without day name
            formatter.dateFormat = "dd MMM yyyy HH:mm:ss Z"
            guard let date = formatter.date(from: dateString) else {
                return dateString // Return original if parsing fails
            }
            return formatRelativeDate(date)
        }

        return formatRelativeDate(date)
    }

    /// Format date as relative or absolute depending on age
    private func formatRelativeDate(_ date: Date) -> String {
        let calendar = Calendar.current
        let now = Date()

        if calendar.isDateInToday(date) {
            // Today: show time only
            let formatter = DateFormatter()
            formatter.dateFormat = "HH:mm"
            return formatter.string(from: date)
        } else if calendar.isDateInYesterday(date) {
            // Yesterday
            let formatter = DateFormatter()
            formatter.dateFormat = "HH:mm"
            return "Yesterday, \(formatter.string(from: date))"
        } else if let daysAgo = calendar.dateComponents([.day], from: date, to: now).day, daysAgo < 7 {
            // Within last week: show day name
            let formatter = DateFormatter()
            formatter.locale = Locale(identifier: "en_US")
            formatter.dateFormat = "EEEE, HH:mm"
            return formatter.string(from: date)
        } else if calendar.component(.year, from: date) == calendar.component(.year, from: now) {
            // This year: show date without year
            let formatter = DateFormatter()
            formatter.locale = Locale(identifier: "en_US")
            formatter.dateFormat = "MMM d, HH:mm"
            return formatter.string(from: date)
        } else {
            // Older: show full date
            let formatter = DateFormatter()
            formatter.locale = Locale(identifier: "en_US")
            formatter.dateFormat = "MMM d, yyyy, HH:mm"
            return formatter.string(from: date)
        }
    }
}
