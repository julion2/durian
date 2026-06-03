//
//  ContentView.swift
//  Durian
//
//  Created by Julian Schenker on 15.09.25.
//

import Combine
import SwiftUI

// MARK: - Popup Navigation Notifications

extension Notification.Name {
    static let popupSelectNext = Notification.Name("popupSelectNext")
    static let popupSelectPrev = Notification.Name("popupSelectPrev")
    static let threadScrollDown = Notification.Name("threadScrollDown")
    static let threadScrollUp = Notification.Name("threadScrollUp")
    static let threadScrollToTop = Notification.Name("threadScrollToTop")
    static let threadScrollToBottom = Notification.Name("threadScrollToBottom")
}

enum DetailViewMode: Equatable {
    case emailDetail(emailId: String)
    case empty
}

struct ContentView: View {
    @Environment(\.openWindow) private var openWindow
    // Note: several properties below are `internal` (no `private`) so that
    // the keymap handler extension in ContentView+Keymaps.swift can access
    // them. ContentView itself is still an internal app type.
    @ObservedObject var accountManager = AccountManager.shared
    @ObservedObject private var keymapsManager = KeymapsManager.shared
    @ObservedObject var keymapHandler = KeymapHandler.shared
    @ObservedObject private var profileManager = ProfileManager.shared
    @ObservedObject private var syncManager = SyncManager.shared
    @ObservedObject private var networkMonitor = NetworkMonitor.shared
    @ObservedObject private var bannerManager = BannerManager.shared
    @State var selectedTagID: String? = "inbox"
    @State var cursorEmailId: String? = nil       // Highlighted email (cursor position)
    @State var markedEmails: Set<String> = []     // Marked emails (selection for batch ops)
    @State var detailMode: DetailViewMode = .empty
    @State var showSearchPopup: Bool = false
    @State var showTagPicker: Bool = false
    @State var showFolderPicker: Bool = false
    @State var allTags: [String] = []
    @State var visualModeAnchor: String? = nil    // Anchor for visual mode range selection
    @State var isSearchMode = false
    @State private var bodyFetchTask: Task<Void, Never>?
    @State private var folderSwitchTask: Task<Void, Never>?
    @State var searchResults: [MailMessage] = []
    @State var lastSearchQuery = ""
    @State var focusedMessageIndex: Int = 0
    @State var isThreadFocused: Bool = false

    var body: some View {
        ZStack {
            emailView

            if showSearchPopup {
                searchPopupOverlay
            }

            if showTagPicker {
                tagPickerOverlay
            }

            if showFolderPicker {
                folderPickerOverlay
            }

            // Banner Overlay (bottom-right toast)
            if let banner = bannerManager.currentBanner {
                VStack {
                    Spacer()
                    HStack {
                        Spacer()
                        BannerView(banner: banner) {
                            bannerManager.dismiss()
                        }
                        .frame(maxWidth: 400)
                    }
                }
                .padding(16)
                .transition(.move(edge: .bottom).combined(with: .opacity))
                .animation(.easeInOut(duration: 0.3), value: bannerManager.currentBanner?.id)
                .zIndex(100)
            }
        }
        .onChange(of: accountManager.pendingNotificationThreadId) { _, threadId in
            guard let threadId = threadId else { return }
            accountManager.pendingNotificationThreadId = nil
            cursorEmailId = threadId
            markedEmails = [threadId]
            handleEmailSelection(threadId)
        }
        .tint(profileManager.resolvedAccentColor)
    }

    // MARK: - Search Popup Overlay

    @ViewBuilder
    private var searchPopupOverlay: some View {
        ZStack {
            // Subtle dimming to normalize glass effect background
            Color.black.opacity(0.15)
                .ignoresSafeArea()
                .onTapGesture {
                    showSearchPopup = false
                }

            // Top-aligned popup
            VStack {
                SearchPopupView(
                    isPresented: $showSearchPopup,
                    selectedEmailId: Binding(
                        get: { markedEmails.first },
                        set: { newId in
                            if let id = newId {
                                markedEmails = [id]
                            }
                        }
                    ),
                    initialQuery: isSearchMode ? lastSearchQuery : "",
                    onResultsActivated: { query, results, selectedId in
                        isSearchMode = true
                        searchResults = results
                        lastSearchQuery = query
                        cursorEmailId = selectedId
                        markedEmails = [selectedId]
                        handleEmailSelection(selectedId)
                    }
                )
                .padding(.top, 80)

                Spacer()
            }
        }
        .ignoresSafeArea()
    }

    // MARK: - Tag Picker Overlay

    @ViewBuilder
    private var tagPickerOverlay: some View {
        ZStack {
            Color.black.opacity(0.15)
                .ignoresSafeArea()
                .onTapGesture {
                    showTagPicker = false
                }

            VStack {
                TagPickerView(
                    isPresented: $showTagPicker,
                    currentTags: currentEmailTags,
                    allTags: allTags,
                    onToggleTag: { tag, isAdding in
                        let ids = markedEmails
                        guard !ids.isEmpty else { return }
                        // Optimistically add new tag to allTags so the UI updates instantly
                        if isAdding && !allTags.contains(tag) {
                            allTags.append(tag)
                            allTags.sort()
                        }
                        // Optimistically update search results so tag pills refresh immediately
                        if isSearchMode {
                            for emailId in ids {
                                if let idx = searchResults.firstIndex(where: { $0.id == emailId }) {
                                    var currentTags = (searchResults[idx].tags ?? "")
                                        .split(separator: ",")
                                        .map { $0.trimmingCharacters(in: .whitespaces) }
                                        .filter { !$0.isEmpty }
                                    if isAdding {
                                        if !currentTags.contains(tag) { currentTags.append(tag) }
                                    } else {
                                        currentTags.removeAll { $0 == tag }
                                    }
                                    searchResults[idx].tags = currentTags.joined(separator: ",")
                                    let tagSet = Set(currentTags)
                                    searchResults[idx].isRead = !tagSet.contains("unread")
                                    searchResults[idx].isPinned = tagSet.contains("flagged")
                                    searchResults[idx].hasAttachment = tagSet.contains("attachment")
                                }
                            }
                        }
                        Task {
                            for id in ids {
                                if isAdding {
                                    await accountManager.modifyTagsWithoutSync(id: id, add: [tag], remove: [])
                                } else {
                                    await accountManager.modifyTagsWithoutSync(id: id, add: [], remove: [tag])
                                }
                            }
                            await accountManager.syncAndRefresh()
                            allTags = await accountManager.fetchAllTags()
                        }
                    }
                )
                .padding(.top, 80)

                Spacer()
            }
        }
        .ignoresSafeArea()
    }

    // MARK: - Folder Picker Overlay

    @ViewBuilder
    private var folderPickerOverlay: some View {
        ZStack {
            Color.black.opacity(0.15)
                .ignoresSafeArea()
                .onTapGesture {
                    showFolderPicker = false
                }

            VStack {
                FolderPickerView(
                    isPresented: $showFolderPicker,
                    folders: accountManager.mailFolders,
                    unreadCounts: accountManager.folderUnreadCounts,
                    currentFolder: accountManager.selectedFolder,
                    onSelect: { folderName in
                        selectedTagID = folderName
                    }
                )
                .padding(.top, 80)

                Spacer()
            }
        }
        .ignoresSafeArea()
    }

    /// Tags on the currently focused email
    private var currentEmailTags: [String] {
        guard let emailId = cursorEmailId,
              let email = accountManager.mailMessages.first(where: { $0.id == emailId })
                          ?? searchResults.first(where: { $0.id == emailId }),
              let tagsString = email.tags else { return [] }
        return tagsString
            .split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
    }

    // MARK: - Email View

    @ViewBuilder
    private var emailView: some View {
        NavigationSplitView {
            SidebarView(
                selectedTagID: $selectedTagID,
                accountManager: accountManager,
                profileManager: profileManager
            )
            .navigationTitle("")
        } content: {
            // Email List
            VStack {
                if accountManager.isLoadingEmails && !accountManager.loadingProgress.isEmpty {
                    HStack {
                        ProgressView()
                            .scaleEffect(0.8)
                        Text(accountManager.loadingProgress)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                    .padding(.vertical, 4)
                }

                if !displayEmails.isEmpty {
                    emailListView
                } else if !isSearchMode && accountManager.isLoadingEmails {
                    VStack {
                        ProgressView()
                        Text(accountManager.loadingProgress)
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .padding(.top, 8)
                    }
                    .frame(maxWidth: .infinity, minHeight: 240, maxHeight: .infinity)
                } else {
                    emptyStateView
                }
            }
            .navigationTitle("Durian")
            .navigationSubtitle(isSearchMode ? "Search: \(lastSearchQuery)" : accountManager.selectedFolder)
            .toolbar {
                // Center: Compose + Email Actions
                ToolbarItemGroup(placement: .principal) {
                    Button(action: { openNewCompose() }) {
                        Image(systemName: "square.and.pencil")
                    }
                    .help("New Message (Cmd+N)")
                    .keyboardShortcut("n", modifiers: .command)

                    Button(action: { replyToSelected() }) {
                        Image(systemName: "arrowshape.turn.up.left")
                    }
                    .help("Reply (R)")
                    .disabled(markedEmails.isEmpty || !selectedEmailHasBody)

                    Button(action: { replyAllToSelected() }) {
                        Image(systemName: "arrowshape.turn.up.left.2")
                    }
                    .help("Reply All (Shift+R)")
                    .disabled(markedEmails.isEmpty || !selectedEmailHasBody)

                    Button(action: { forwardSelected() }) {
                        Image(systemName: "arrowshape.turn.up.right")
                    }
                    .help("Forward (F)")
                    .disabled(markedEmails.isEmpty || !selectedEmailHasBody)

                    Button(action: deleteSelectedEmails) {
                        Image(systemName: "trash")
                    }
                    .help("Delete")
                    .disabled(markedEmails.isEmpty)

                    Button(action: togglePin) {
                        Image(systemName: selectedEmailIsPinned ? "pin.fill" : "pin")
                    }
                    .help(selectedEmailIsPinned ? "Unpin (S)" : "Pin (S)")
                    .disabled(markedEmails.isEmpty)

                    Button(action: toggleRead) {
                        Image(systemName: selectedEmailIsRead ? "envelope.open" : "envelope.badge")
                    }
                    .help(selectedEmailIsRead ? "Mark Unread (U)" : "Mark Read (U)")
                    .disabled(markedEmails.isEmpty)
                }

                // Right: Search & Sync
                ToolbarItemGroup(placement: .automatic) {
                    Button(action: { showSearchPopup = true }) {
                        Image(systemName: "magnifyingglass")
                    }
                    .keyboardShortcut("/", modifiers: .command)
                    .help("Search (Cmd+/)")

                    Button(action: {
                        Task {
                            await syncManager.quickSync()
                            await accountManager.reloadEmail()
                        }
                    }) {
                        Image(systemName: "arrow.triangle.2.circlepath")
                            .rotationEffect(.degrees(syncManager.syncState == .syncing ? 360 : 0))
                            .animation(
                                syncManager.syncState == .syncing
                                    ? .linear(duration: 1).repeatForever(autoreverses: false)
                                    : .default,
                                value: syncManager.syncState
                            )
                            .foregroundColor(syncManager.syncState.color)
                    }
                    .keyboardShortcut("r", modifiers: .command)
                    .help("Sync (Cmd+R)")
                    .disabled(syncManager.isSyncing)
                }
            }
        } detail: {
            // Detail View - always show cursor email, with badge if multi-selected
            if let emailId = cursorEmailId,
               let email = accountManager.mailMessages.first(where: { $0.id == emailId })
                            ?? searchResults.first(where: { $0.id == emailId })
            {
                ZStack(alignment: .bottomTrailing) {
                    EmailDetailView(
                        email: email,
                        onReply: replyToSelected,
                        onReplyAll: replyAllToSelected,
                        onForward: forwardSelected,
                        onLoadBody: {
                            Task {
                                if let loaded = await accountManager.fetchEmailBody(id: email.id) {
                                    applyStandaloneEmail(loaded)
                                }
                            }
                        },
                        onEditDraft: email.isDraft ? editSelectedDraft : nil,
                        currentFolder: accountManager.selectedFolder,
                        onAddTag: { tag in
                            Task { await accountManager.addTag(id: email.id, tag: tag) }
                        },
                        onRemoveTag: { tag in
                            Task { await accountManager.removeTag(id: email.id, tag: tag) }
                        },
                        focusedMessageIndex: $focusedMessageIndex,
                        isThreadFocused: isThreadFocused
                    )
                    .id(email.id)  // Force new View instance on email change to reset @State

                    // Selection badge when multiple emails marked
                    if markedEmails.count > 1 {
                        HStack(spacing: 4) {
                            Image(systemName: "checkmark.circle.fill")
                            Text("\(markedEmails.count) selected")
                        }
                        .font(.caption)
                        .fontWeight(.medium)
                        .padding(.horizontal, 10)
                        .padding(.vertical, 6)
                        .background(profileManager.resolvedAccentColor)
                        .foregroundColor(.white)
                        .cornerRadius(6)
                        .padding(16)
                    }
                }
            } else {
                Text("Select an email")
                    .font(.title2)
                    .foregroundStyle(.secondary)
            }
        }
        .onAppear {
            Task {
                await accountManager.connectToAllAccounts()
            }
            // Register all keymap handlers
            registerKeymapHandlers()
        }
        .onChange(of: selectedTagID) { _, tagId in
            if tagId != nil && tagId != accountManager.selectedFolder {
                exitSearchMode()
            }
        }
        .onChange(of: accountManager.selectedFolder) { _, newFolder in
            if selectedTagID != newFolder {
                selectedTagID = newFolder
            }
        }
        .onChange(of: accountManager.emailListGeneration) { _, _ in
            // Auto-select first email when list data arrives and cursor is empty
            if cursorEmailId == nil, let firstId = accountManager.mailMessages.first?.id {
                cursorEmailId = firstId
                markedEmails = [firstId]
            }
        }
        .onChange(of: markedEmails) { _, newSelection in
            // When selection changes externally (e.g., click), sync cursor
            if newSelection.count == 1, let emailId = newSelection.first {
                cursorEmailId = emailId
                handleEmailSelection(emailId)
            }
        }
        .onChange(of: cursorEmailId) { _, newId in
            if let id = newId {
                accountManager.prefetchAroundCursor(cursorId: id)
            }
        }
        .onChange(of: showSearchPopup) { _, isShowing in
            keymapHandler.engine.setContext(isShowing ? .search : .list)
        }
        .onChange(of: showTagPicker) { _, isShowing in
            keymapHandler.engine.setContext(isShowing ? .tagPicker : .list)
        }
        .onChange(of: showFolderPicker) { _, isShowing in
            keymapHandler.engine.setContext(isShowing ? .tagPicker : .list)
        }

        // Body state for search results is applied directly via applyStandaloneEmail()
        // — no need to sync from accountManager.mailMessages.
        // Intercept Escape/Ctrl+d/u before the sidebar List captures them
        .onKeyPress { press in
            // Escape to exit search mode (sequence engine doesn't dispatch Escape to handlers)
            if press.key == .escape && isSearchMode && !showSearchPopup && !showTagPicker {
                exitSearchMode()
                return .handled
            }
            // Ctrl+d for page down
            if press.key == KeyEquivalent("d") && press.modifiers.contains(.control) {
                let pageSize = 10
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current + pageSize)
                } else {
                    navigateToEmail(at: 0)
                }
                return .handled
            }
            // Ctrl+u for page up
            if press.key == KeyEquivalent("u") && press.modifiers.contains(.control) {
                let pageSize = 10
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current - pageSize)
                } else {
                    navigateToEmail(at: sortedEmailIds.count - 1)
                }
                return .handled
            }
            return .ignored
        }
    }

    /// Number of thread messages for the currently focused email
    var currentThreadMessageCount: Int {
        guard let emailId = cursorEmailId,
              let email = accountManager.mailMessages.first(where: { $0.id == emailId })
                          ?? searchResults.first(where: { $0.id == emailId }),
              let messages = email.threadMessages else { return 1 }
        return max(messages.count, 1)
    }

    // MARK: - Email List

    private var emailListView: some View {
        EmailListView(
            emails: displayEmails,
            emailListGeneration: accountManager.emailListGeneration,
            cursorId: $cursorEmailId,
            selection: $markedEmails,
            onEmailAppear: { email in
                guard email.id == cursorEmailId else { return }
                switch email.bodyState {
                case .notLoaded, .failed:
                    Task {
                        if let loaded = await accountManager.fetchEmailBody(id: email.id) {
                            applyStandaloneEmail(loaded)
                        }
                    }
                case .loading, .loaded:
                    break
                }
            },
            onTogglePin: { emailId in
                Task { await accountManager.togglePin(id: emailId) }
            },
            onToggleRead: { emailId in
                Task { await accountManager.toggleRead(id: emailId) }
            },
            onDelete: { emailId in
                Task {
                    await accountManager.deleteMessage(id: emailId)
                    await MainActor.run {
                        markedEmails = []
                        detailMode = .empty
                    }
                }
            },
            onReply: { emailId in
                handleEmailSelection(emailId)
                Task {
                    if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                        applyStandaloneEmail(loaded)
                    }
                    await MainActor.run { replyToSelected() }
                }
            },
            onForward: { emailId in
                handleEmailSelection(emailId)
                Task {
                    if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                        applyStandaloneEmail(loaded)
                    }
                    await MainActor.run { forwardSelected() }
                }
            },
            onShowTagPicker: { _ in
                Task {
                    allTags = await accountManager.fetchAllTags()
                    showTagPicker = true
                }
            }
        )
    }

    // MARK: - Empty State

    @ViewBuilder
    private var emptyStateView: some View {
        if accountManager.isLoadingEmails {
            // Don't flash empty state while loading
            Color.clear
        } else if isSearchMode {
            ContentUnavailableView("No Results", systemImage: "magnifyingglass",
                                   description: Text(Self.stableMessage(from: Self.searchEmptyMessages, seed: selectedTagID ?? "search")))
        } else {
            ContentUnavailableView("Inbox Zero", systemImage: "tray",
                                   description: Text(Self.stableMessage(from: Self.emptyFolderMessages, seed: selectedTagID ?? "default")))
        }
    }

    /// Pick a message deterministically based on seed (stable per folder, varies between folders)
    private static func stableMessage(from messages: [String], seed: String) -> String {
        let hash = seed.utf8.reduce(0) { $0 &+ Int($1) }
        return messages[abs(hash) % messages.count]
    }

    private static let emptyFolderMessages = [
        "Nothing here. Suspicious.",
        "You've read everything. Now what.",
        "Empty. Go build something.",
        "Inbox zero. Enjoy the 4 seconds.",
        ":q! executed on your inbox.",
        "/dev/null successfully achieved.",
        "Achievement unlocked: not drowning.",
    ]

    private static let searchEmptyMessages = [
        "No mails match. Tighten your query or loosen your expectations.",
        "Zero results. Either very specific or very wrong.",
        "Nothing found. Try from: + date: — it's always from: + date:.",
        "No matches. Your query or your luck.",
    ]

    // MARK: - Display Emails (Search Mode vs Normal)

    private var displayEmails: [MailMessage] {
        isSearchMode ? searchResults : accountManager.mailMessages
    }

    /// Exit search mode and restore the current folder view.
    /// Mirrors the full cleanup in `onChange(of: selectedTagID)`.
    func exitSearchMode() {
        isSearchMode = false
        searchResults = []
        lastSearchQuery = ""
        isThreadFocused = false
        keymapHandler.engine.setContext(.list)
        cursorEmailId = nil
        markedEmails = []
        // Re-fetch — debounced to avoid flooding backend during rapid J/K folder switching
        if let tagId = selectedTagID {
            folderSwitchTask?.cancel()
            folderSwitchTask = Task {
                try? await Task.sleep(for: .milliseconds(150))
                guard !Task.isCancelled else { return }
                await accountManager.selectTag(tagId)
            }
        }
    }

    // MARK: - Helper Methods

    private func handleEmailSelection(_ emailId: String) {
        detailMode = .emailDetail(emailId: emailId)

        // Debounce body fetch — cancel previous request during rapid j/k navigation
        bodyFetchTask?.cancel()
        bodyFetchTask = Task {
            try? await Task.sleep(for: .milliseconds(150))
            guard !Task.isCancelled else { return }

            if let email = displayEmails.first(where: { $0.id == emailId }) {
                switch email.bodyState {
                case .notLoaded, .failed:
                    if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                        applyStandaloneEmail(loaded)
                    }
                case .loading, .loaded:
                    break
                }
                if !email.isRead {
                    await accountManager.markAsRead(id: emailId)
                }
            } else {
                if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                    applyStandaloneEmail(loaded)
                }
            }
        }
    }

    /// Apply a standalone loaded email into the active email list.
    func applyStandaloneEmail(_ email: MailMessage) {
        if isSearchMode {
            if let index = searchResults.firstIndex(where: { $0.id == email.id }) {
                let originalDate = searchResults[index].date
                searchResults[index] = email
                searchResults[index].date = originalDate
            }
        } else {
            if let index = accountManager.mailMessages.firstIndex(where: { $0.id == email.id }) {
                accountManager.mailMessages[index] = email
            }
        }
    }

    // MARK: - Toolbar Helpers

    private var selectedEmailIsPinned: Bool {
        guard let emailId = markedEmails.first,
              let email = displayEmails.first(where: { $0.id == emailId }) else
        {
            return false
        }
        return email.isPinned
    }

    private var selectedEmailIsRead: Bool {
        guard let emailId = markedEmails.first,
              let email = displayEmails.first(where: { $0.id == emailId }) else
        {
            return true
        }
        return email.isRead
    }

    private var selectedEmailHasBody: Bool {
        guard let emailId = markedEmails.first,
              let email = displayEmails.first(where: { $0.id == emailId }) else
        {
            return false
        }
        if case .loaded = email.bodyState {
            return true
        }
        return false
    }

    private var selectedEmail: MailMessage? {
        guard let emailId = markedEmails.first else { return nil }
        return displayEmails.first(where: { $0.id == emailId })
    }

    private func deleteSelectedEmails() {
        guard !markedEmails.isEmpty else { return }
        let ids = markedEmails
        let next = nextEmailId(after: ids)
        accountManager.removeLocally(ids: ids)
        visualModeAnchor = nil
        keymapHandler.engine.exitVisualMode()
        advanceCursor(to: next)
        Task { @MainActor in
            await accountManager.deleteMessages(ids: ids)
            await accountManager.refreshFolderCounts()
        }
    }

    private func togglePin() {
        guard let emailId = markedEmails.first else { return }
        if keymapHandler.engine.isVisualMode {
            keymapHandler.engine.exitVisualMode()
            visualModeAnchor = nil
            markedEmails = [emailId]
        }
        Task { await accountManager.togglePin(id: emailId) }
    }

    func toggleRead() {
        guard !markedEmails.isEmpty else { return }
        Task {
            await accountManager.toggleReadForMessages(ids: markedEmails)
            await accountManager.refreshFolderCounts()
            await MainActor.run {
                // Exit visual mode after batch action
                if keymapHandler.engine.isVisualMode {
                    keymapHandler.engine.exitVisualMode()
                    visualModeAnchor = nil
                }
            }
        }
    }

    // MARK: - Compose

    func openNewCompose() {
        guard defaultFromAccount != nil else {
            BannerManager.shared.showWarning(title: "No Account", message: "Configure an email account to use this action.")
            return
        }
        let draftId = DraftService.shared.createDraft(from: defaultFromAccount)
        openWindow(value: draftId)
    }

    // MARK: - Reply/Forward Actions

    /// Get default from-account based on current profile
    private var defaultFromAccount: String? {
        // Get first account from current profile
        if let profile = profileManager.currentProfile,
           let accountName = profile.accounts.first,
           accountName != "*"
        {
            // Find matching account by name
            return ConfigManager.shared.getAccounts()
                .first(where: { $0.name.caseInsensitiveCompare(accountName) == .orderedSame })?.email
        }
        // Fallback to first configured account
        return ConfigManager.shared.getAccounts().first?.email
    }

    func replyToSelected() {
        guard let email = selectedEmail,
              case .loaded = email.bodyState,
              let fromAccount = defaultFromAccount else
        {
            Log.warning("COMPOSE", "replyToSelected guard failed — selected=\(selectedEmail != nil), bodyState=\(String(describing: selectedEmail?.bodyState)), fromAccount=\(defaultFromAccount ?? "nil")")
            if defaultFromAccount == nil {
                BannerManager.shared.showWarning(title: "No Account", message: "Configure an email account to use this action.")
            }
            return
        }

        Task {
            let original = await fetchOriginalReplyBody(for: email, fromAccount: fromAccount)
            let replyDraft = EmailDraft.createReply(from: email, fromAccount: fromAccount, originalBody: original)
            let draftId = DraftService.shared.createDraft(with: replyDraft)
            openWindow(value: draftId)
        }
    }

    func replyAllToSelected() {
        guard let email = selectedEmail,
              case .loaded = email.bodyState,
              let fromAccount = defaultFromAccount else
        {
            Log.warning("COMPOSE", "replyAllToSelected guard failed — selected=\(selectedEmail != nil), bodyState=\(String(describing: selectedEmail?.bodyState)), fromAccount=\(defaultFromAccount ?? "nil")")
            if defaultFromAccount == nil {
                BannerManager.shared.showWarning(title: "No Account", message: "Configure an email account to use this action.")
            }
            return
        }

        Task {
            let original = await fetchOriginalReplyBody(for: email, fromAccount: fromAccount)
            let replyDraft = EmailDraft.createReplyAll(from: email, fromAccount: fromAccount, originalBody: original)
            let draftId = DraftService.shared.createDraft(with: replyDraft)
            openWindow(value: draftId)
        }
    }

    /// Fetch unstripped body for the reply target message (lazy-loaded on reply action)
    private func fetchOriginalReplyBody(for email: MailMessage, fromAccount: String) async -> (body: String, html: String?)? {
        guard let targetId = EmailDraft.replyTargetMessageId(for: email, fromAccount: fromAccount),
              let backend = AccountManager.shared.emailBackend else { return nil }
        guard let response = await backend.fetchOriginalBody(messageId: targetId) else { return nil }
        return (body: response.body, html: response.html)
    }

    func forwardSelected() {
        guard let email = selectedEmail,
              case .loaded = email.bodyState,
              let fromAccount = defaultFromAccount else
        {
            Log.warning("COMPOSE", "forwardSelected guard failed — selected=\(selectedEmail != nil), bodyState=\(String(describing: selectedEmail?.bodyState)), fromAccount=\(defaultFromAccount ?? "nil")")
            if defaultFromAccount == nil {
                BannerManager.shared.showWarning(title: "No Account", message: "Configure an email account to use this action.")
            }
            return
        }

        Task {
            var forwardDraft = EmailDraft.createForward(from: email, fromAccount: fromAccount)

            // Copy attachments from the original message(s) into the forward draft.
            // Requires the email backend to fetch attachment bytes via IMAP.
            if let backend = AccountManager.shared.emailBackend {
                let result = await EmailDraft.collectForwardAttachments(from: email, backend: backend)
                forwardDraft.attachments = result.attachments

                if !result.skipped.isEmpty {
                    let msg = "Some attachments were skipped: " + result.skipped.joined(separator: ", ")
                    BannerManager.shared.showWarning(title: "Forward Attachments", message: msg)
                }
            }

            let draftId = DraftService.shared.createDraft(with: forwardDraft)
            openWindow(value: draftId)
        }
    }

    private func editSelectedDraft() {
        guard let email = selectedEmail,
              case .loaded = email.bodyState else { return }
        let draft = EmailDraft.createFromDraft(message: email)
        let draftId = DraftService.shared.createDraft(with: draft)
        openWindow(value: draftId)
    }

    // MARK: - Navigation Helpers

    /// Advance cursor and selection to the given email, or clear if nil.
    func advanceCursor(to emailId: String?) {
        if let emailId = emailId {
            cursorEmailId = emailId
            markedEmails = [emailId]
            detailMode = .emailDetail(emailId: emailId)
        } else {
            cursorEmailId = nil
            markedEmails = []
            detailMode = .empty
        }
    }

    /// Compute the next email to select after removing the given IDs from the list.
    /// Picks the email just below the lowest removed index, or the one above if at the end.
    func nextEmailId(after removedIds: Set<String>) -> String? {
        let ids = sortedEmailIds
        guard let firstRemovedIndex = ids.firstIndex(where: { removedIds.contains($0) }) else { return nil }
        // Try the email just after the last contiguous removed item
        var nextIndex = firstRemovedIndex
        while nextIndex < ids.count && removedIds.contains(ids[nextIndex]) {
            nextIndex += 1
        }
        if nextIndex < ids.count { return ids[nextIndex] }
        // All removed items were at the end — pick the one above
        let prevIndex = firstRemovedIndex - 1
        return prevIndex >= 0 ? ids[prevIndex] : nil
    }

    /// Get sorted email IDs matching visual order (pinned first, then by timestamp)
    var sortedEmailIds: [String] {
        let pinned = displayEmails.filter { $0.isPinned }.sorted { $0.timestamp > $1.timestamp }
        let unpinned = displayEmails.filter { !$0.isPinned }.sorted { $0.timestamp > $1.timestamp }
        return (pinned + unpinned).map { $0.id }
    }

    /// Get current email index in sorted list (based on cursor position)
    func currentEmailIndex() -> Int? {
        guard let currentId = cursorEmailId else { return nil }
        return sortedEmailIds.firstIndex(of: currentId)
    }

    /// Navigate to email at specific index (clamped to valid range)
    /// Updates cursor position and handles visual mode selection
    func navigateToEmail(at index: Int) {
        guard !sortedEmailIds.isEmpty else { return }
        let clampedIndex = max(0, min(index, sortedEmailIds.count - 1))
        let targetId = sortedEmailIds[clampedIndex]

        // Always update cursor position
        cursorEmailId = targetId

        switch keymapHandler.engine.visualModeType {
        case .none:
            // Normal navigation: selection follows cursor
            markedEmails = [targetId]

        case .line:
            // Line mode: selection = all emails from anchor to cursor
            if let anchor = visualModeAnchor,
               let anchorIndex = sortedEmailIds.firstIndex(of: anchor)
            {
                let start = min(anchorIndex, clampedIndex)
                let end = max(anchorIndex, clampedIndex)
                markedEmails = Set(sortedEmailIds[start...end])
            } else {
                // No anchor yet, just add cursor to selection
                markedEmails.insert(targetId)
            }

        case .toggle:
            // Toggle mode: selection stays unchanged, only cursor moves
            break
        }
    }

    /// Get range of email IDs between two indices
    private func emailsInRange(from: Int, to: Int) -> Set<String> {
        let start = min(from, to)
        let end = max(from, to)
        let clampedStart = max(0, start)
        let clampedEnd = min(sortedEmailIds.count - 1, end)
        guard clampedStart <= clampedEnd else { return [] }
        return Set(sortedEmailIds[clampedStart...clampedEnd])
    }
}
