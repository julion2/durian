//
//  EmailDetailView.swift
//  Durian
//
//  Modern email detail view with chat-style cards for reply threads
//  Uses ThreadMessage data from CLI instead of HTML parsing
//

import AppKit
import Quartz
import SwiftUI
import UniformTypeIdentifiers

// MARK: - Email Detail View

struct EmailDetailView: View {
    let email: MailMessage
    let onReply: () -> Void
    let onReplyAll: () -> Void
    let onForward: () -> Void
    let onLoadBody: () -> Void
    var onEditDraft: (() -> Void)? = nil
    var currentFolder: String? = nil
    var onAddTag: ((String) -> Void)? = nil
    var onRemoveTag: ((String) -> Void)? = nil
    @Binding var focusedMessageIndex: Int
    var isThreadFocused: Bool = false

    // MARK: - State

    @State private var messageHeights: [String: CGFloat] = [:]  // Use message ID as key
    @State private var detailScrollView: NSScrollView?

    // MARK: - Body

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    ScrollViewFinder(scrollView: $detailScrollView)
                        .frame(height: 0)
                        .id("thread-top")
                    headerSection
                    messageCards
                }
            }
            .onChange(of: focusedMessageIndex) { _, newIndex in
                withAnimation(.easeInOut(duration: 0.15)) {
                    if newIndex == 0 {
                        proxy.scrollTo("thread-top", anchor: .top)
                    } else {
                        proxy.scrollTo("msg-\(newIndex)", anchor: .top)
                    }
                }
            }
        }
        .overlayScrollbars()
        .background(Color(NSColor.controlBackgroundColor))
        .onReceive(NotificationCenter.default.publisher(for: .threadScrollDown)) { _ in
            scrollBy(80)
        }
        .onReceive(NotificationCenter.default.publisher(for: .threadScrollUp)) { _ in
            scrollBy(-80)
        }
        .onReceive(NotificationCenter.default.publisher(for: .threadScrollToTop)) { _ in
            guard let sv = detailScrollView else { return }
            sv.contentView.setBoundsOrigin(.zero)
            sv.reflectScrolledClipView(sv.contentView)
        }
        .onReceive(NotificationCenter.default.publisher(for: .threadScrollToBottom)) { _ in
            guard let sv = detailScrollView,
                  let docView = sv.documentView else { return }
            let maxY = max(docView.frame.height - sv.contentView.bounds.height, 0)
            sv.contentView.setBoundsOrigin(NSPoint(x: 0, y: maxY))
            sv.reflectScrolledClipView(sv.contentView)
        }
        .onAppear {
            // Auto-load body if not loaded
            switch email.bodyState {
            case .notLoaded, .failed:
                onLoadBody()
            case .loading, .loaded:
                break
            }
        }
        // Reset state when email changes
        .onChange(of: email.id) {
            messageHeights = [:]
            focusedMessageIndex = 0
        }
    }

    // MARK: - Message Cards (Chat-Style Thread View)

    @ViewBuilder
    private var messageCards: some View {
        switch email.bodyState {
        case .notLoaded:
            loadingCard(text: "Click to load") {
                onLoadBody()
            }

        case .loading:
            loadingCard(text: nil, action: nil)

        case .loaded:
            // Use thread messages from CLI if available
            if let messages = email.threadMessages, !messages.isEmpty {
                Color.clear.frame(height: 0)
                    .onAppear {
                        if let backend = AccountManager.shared.emailBackend {
                            AttachmentCacheManager.shared.prefetch(messages: messages, backend: backend)
                        }
                    }
                ForEach(Array(messages.enumerated()), id: \.element.id) { index, message in
                    Color.clear.frame(height: 0)
                        .id("msg-\(index)")
                    ThreadMessageCardView(
                        message: message,
                        isFirst: index == 0,
                        isLast: index == 0,  // Newest message (first) gets reply button
                        email: email,
                        contentHeight: bindingForMessageId(message.id),
                        isFocused: isThreadFocused && index == focusedMessageIndex,
                        onReply: onReply,
                        onReplyAll: onReplyAll,
                        onForward: onForward,
                        onEditDraft: onEditDraft
                    )
                }
            } else {
                // Fallback: single message with body/html directly from email
                singleMessageFallback
            }

        case .failed(let errorMessage):
            errorCard(message: errorMessage)
        }
    }

    @ViewBuilder
    private var singleMessageFallback: some View {
        // Use htmlBody if available, otherwise body
        let htmlContent = email.htmlBody ?? ""
        let textContent = email.body ?? ""

        VStack(alignment: .leading, spacing: 16) {
            // Sender row
            HStack(alignment: .top, spacing: 12) {
                AvatarView(name: email.from, email: email.from, size: 40)

                VStack(alignment: .leading, spacing: 2) {
                    Text(extractName(from: email.from))
                        .font(.system(size: 16, weight: .semibold))
                        .foregroundColor(Color.Detail.textPrimary)

                    Text("Details")
                        .font(.system(size: 14))
                        .foregroundColor(Color.Detail.textSecondary)
                }

                Spacer()

                Text(formatDate(email.date))
                    .font(.system(size: 14))
                    .foregroundColor(Color.Detail.textTertiary)
                    .lineLimit(1)
            }

            // Content
            if !htmlContent.isEmpty {
                NonScrollingWebView(
                    html: htmlContent,
                    theme: SettingsManager.shared.settings.theme,
                    loadRemoteImages: SettingsManager.shared.settings.loadRemoteImages,
                    emailId: email.id,
                    contentHeight: bindingForMessageId("fallback")
                )
                .frame(height: max(messageHeights["fallback"] ?? 100, 50))
            } else if !textContent.isEmpty {
                Text(textContent)
                    .font(.system(size: 14))
                    .foregroundColor(Color.Detail.textPrimary)
                    .textSelection(.enabled)
            }

            // Action footer
            actionFooter
        }
        .padding(.top, 24)
        .padding(.horizontal, 24)
        .padding(.bottom, 16)
        .background(Color.Detail.cardBackground)
        .cornerRadius(10)
        .shadow(color: Color.primary.opacity(0.1), radius: 3, x: 0, y: 1)
        .padding(.horizontal, 32)
        .padding(.top, 24)
        .padding(.bottom, 32)
    }

    private func scrollBy(_ delta: CGFloat) {
        guard let sv = detailScrollView,
              let docView = sv.documentView else { return }
        let current = sv.contentView.bounds.origin
        let maxY = max(docView.frame.height - sv.contentView.bounds.height, 0)
        let newY = min(max(current.y + delta, 0), maxY)
        sv.contentView.setBoundsOrigin(NSPoint(x: current.x, y: newY))
        sv.reflectScrolledClipView(sv.contentView)
    }

    private func bindingForMessageId(_ id: String) -> Binding<CGFloat> {
        Binding(
            get: { messageHeights[id] ?? 100 },
            set: { messageHeights[id] = $0 }
        )
    }

    @ViewBuilder
    private func loadingCard(text: String?, action: (() -> Void)?) -> some View {
        VStack {
            if let text = text {
                Text(text)
                    .foregroundColor(Color.Detail.textTertiary)
                    .padding(.vertical, 20)
                    .frame(maxWidth: .infinity, alignment: .center)
                    .contentShape(Rectangle())
                    .onTapGesture {
                        action?()
                    }
            } else {
                HStack(spacing: 8) {
                    ProgressView()
                        .scaleEffect(0.8)
                    Text("Loading...")
                        .foregroundColor(Color.Detail.textTertiary)
                }
                .padding(.vertical, 20)
                .frame(maxWidth: .infinity, alignment: .center)
            }
        }
        .background(Color.Detail.cardBackground)
        .cornerRadius(10)
        .shadow(color: Color.primary.opacity(0.1), radius: 3, x: 0, y: 1)
        .padding(.horizontal, 32)
        .padding(.top, 24)
        .padding(.bottom, 32)
    }

    @ViewBuilder
    private func errorCard(message: String) -> some View {
        Text("Failed: \(message)")
            .foregroundColor(.red)
            .padding(.vertical, 20)
            .frame(maxWidth: .infinity, alignment: .center)
            .background(Color.Detail.cardBackground)
            .cornerRadius(10)
            .shadow(color: Color.primary.opacity(0.1), radius: 3, x: 0, y: 1)
            .padding(.horizontal, 32)
            .padding(.top, 24)
            .padding(.bottom, 32)
    }

    // MARK: - Header Section

    private var parsedTags: [String] {
        (email.tags?.split(separator: ",").map(String.init) ?? [])
            .filter { $0 != currentFolder }
    }

    @ViewBuilder
    private var headerSection: some View {
        VStack(alignment: .leading, spacing: 0) {
            Text(email.subject)
                .font(.system(size: 20, weight: .bold))
                .foregroundColor(Color.Detail.textPrimary)
                .textSelection(.enabled)
                .padding(.horizontal, 32)
                .padding(.top, 32)
                .padding(.bottom, 8)

            if !parsedTags.isEmpty || onAddTag != nil {
                TagChipsView(
                    tags: parsedTags,
                    onRemoveTag: { tag in onRemoveTag?(tag) },
                    onAddTag: { tag in onAddTag?(tag) }
                )
                .padding(.horizontal, 32)
                .padding(.bottom, 8)
            }
        }
    }

    // MARK: - Action Footer

    @ViewBuilder
    private var actionFooter: some View {
        HStack {
            Spacer()

            if email.isDraft, let onEditDraft = onEditDraft {
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

// MARK: - Thread Message Card View

/// A single message card in the thread view - uses ThreadMessage from CLI
