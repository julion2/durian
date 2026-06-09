//
//  AccountManager.swift
//  Durian
//
//  Manages email backend for email access
//

import AppKit
import Combine
import Foundation

@MainActor
class AccountManager: ObservableObject {
    static let shared = AccountManager()

    // MARK: - Email Backend Properties
    @Published var emailBackend: EmailBackend?
    @Published var mailMessages: [MailMessage] = []    // Messages
    /// Generation counter — incremented when email data changes (not on cursor moves)
    @Published var emailListGeneration: Int = 0
    @Published var selectedFolder: String = "inbox"
    @Published var isLoadingEmails = false
    @Published var loadingProgress = ""

    /// Set by notification click handler; ContentView observes and navigates to this thread
    @Published var pendingNotificationThreadId: String?

    /// Unread thread counts per folder name (for sidebar badges)
    @Published var folderUnreadCounts: [String: Int] = [:]

    private var cancellables = Set<AnyCancellable>()

    /// Folders from current profile config
    var mailFolders: [MailFolder] {
        let profile = ProfileManager.shared.currentProfile
        let folders = profile?.folders ?? ProfileManager.defaultFolders

        return folders.map { folder in
            if folder.isSection {
                return MailFolder(section: folder.name)
            }
            return MailFolder(name: folder.name.lowercased(), displayName: folder.name, icon: folder.icon)
        }
    }

    private init() {
        setupEmailBackend()
    }

    // MARK: - Backend Setup

    private func setupEmailBackend() {
        Log.debug("BACKEND", "AccountManager: Setting up email backend")
        emailBackend = EmailBackend()

        // Subscribe to backend changes — debounced to avoid cascade storms
        emailBackend?.objectWillChange
            .debounce(for: .milliseconds(50), scheduler: DispatchQueue.main)
            .sink { [weak self] in
                self?.syncFromBackend()
            }.store(in: &cancellables)
    }

    private func syncFromBackend() {
        guard let backend = emailBackend else { return }
        // Only assign if actually changed to avoid unnecessary @Published triggers
        if mailMessages != backend.emails {
            mailMessages = backend.emails
            emailListGeneration += 1
        }
        if isLoadingEmails != backend.isLoadingEmails {
            isLoadingEmails = backend.isLoadingEmails
        }
        if loadingProgress != backend.loadingProgress {
            loadingProgress = backend.loadingProgress
        }
    }

    // MARK: - Folder Unread Counts & Dock Badge

    /// Refresh unread counts for all sidebar folders and update dock badge.
    func refreshFolderCounts() async {
        guard let backend = emailBackend else { return }

        var counts: [String: Int] = [:]
        for folder in mailFolders where !folder.isSection {
            let folderQuery = ProfileManager.shared.buildQuery(folderName: folder.displayName)
            let unreadQuery = "(\(folderQuery)) AND tag:unread"
            let count = await backend.searchCount(query: unreadQuery)
            Log.debug("COUNTS", "\(folder.name): \(count) unread (query: \(unreadQuery))")
            if count > 0 {
                counts[folder.name] = count
            }
        }
        folderUnreadCounts = counts

        // Update dock badge with total inbox unread
        let inboxCount = counts["inbox"] ?? 0
        Log.debug("COUNTS", "Dock badge: \(inboxCount)")
        NSApp.dockTile.badgeLabel = inboxCount > 0 ? "\(inboxCount)" : nil
    }

    // MARK: - Connection

    func connectToAllAccounts() async {
        Log.debug("BACKEND", "AccountManager: Connecting to email backend...")
        guard let backend = emailBackend else {
            Log.error("BACKEND", "Backend not initialized")
            return
        }
        await backend.connect()
        syncFromBackend()
        await selectTag(resolvedFolder())
    }

    // MARK: - Folder/Tag Selection

    /// Returns `selectedFolder` if it exists in the current profile,
    /// otherwise the first non-section folder, falling back to "inbox".
    private func resolvedFolder() -> String {
        let profile = ProfileManager.shared.currentProfile
        let folders = profile?.folders ?? ProfileManager.defaultFolders
        let exists = folders.contains { !$0.isSection && $0.name.lowercased() == selectedFolder }
        if exists { return selectedFolder }
        return folders.first { !$0.isSection }?.name.lowercased() ?? "inbox"
    }

    func selectTag(_ tag: String) async {
        guard let backend = emailBackend else { return }
        selectedFolder = tag
        // Don't clear mailMessages eagerly — the old list stays visible
        // until search() replaces it with fresh results. This avoids the
        // blank-screen flash on rapid folder switching (gi → gs → gd).
        await backend.selectFolder(tag)
        syncFromBackend()
        await refreshFolderCounts()
    }

    // MARK: - Profile Switching

    /// Switch to a different profile and reload the current tag/folder
    func switchProfile(_ profile: Profile) async {
        // Update ProfileManager
        ProfileManager.shared.currentProfile = profile
        Log.info("BACKEND", "Switched to profile '\(profile.name)'")

        await selectTag(resolvedFolder())
    }

    // MARK: - Email Operations

    @discardableResult func fetchEmailBody(id: String) async -> MailMessage? {
        guard let backend = emailBackend else { return nil }
        let standalone = await backend.fetchEmailBody(id: id)
        syncFromBackend()
        return standalone
    }

    func prefetchAroundCursor(cursorId: String) {
        emailBackend?.prefetchAroundCursor(cursorId: cursorId)
    }

    func markAsRead(id: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.markAsRead(id: id)
        } catch {
            Log.error("BACKEND", "Failed to mark as read: \(error)")
        }
        syncFromBackend()
    }

    func toggleReadStatus(id: String) async {
        guard let backend = emailBackend else { return }
        do {
            if let email = mailMessages.first(where: { $0.id == id }) {
                if email.isRead {
                    try await backend.markAsUnread(id: id)
                } else {
                    try await backend.markAsRead(id: id)
                }
            }
        } catch {
            Log.error("BACKEND", "Failed to toggle read status: \(error)")
            BannerManager.shared.showWarning(title: "Read Status Failed", message: "Could not update read status.")
        }
        syncFromBackend()
    }

    func deleteMessage(id: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.deleteMessage(id: id)
        } catch {
            Log.error("BACKEND", "Failed to delete message: \(error)")
            BannerManager.shared.showWarning(title: "Delete Failed", message: "Could not delete message.")
        }
        syncFromBackend()
    }

    func addTag(id: String, tag: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.addTag(id: id, tag: tag)
        } catch {
            Log.error("BACKEND", "Failed to add tag: \(error)")
            BannerManager.shared.showWarning(title: "Tag Failed", message: "Could not add tag '\(tag)'.")
        }
        syncFromBackend()
    }

    func removeTag(id: String, tag: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.removeTag(id: id, tag: tag)
        } catch {
            Log.error("BACKEND", "Failed to remove tag: \(error)")
            BannerManager.shared.showWarning(title: "Tag Failed", message: "Could not remove tag '\(tag)'.")
        }
        syncFromBackend()
    }

    func modifyTags(id: String, add: [String], remove: [String]) async {
        await modifyTagsWithoutSync(id: id, add: add, remove: remove)
        syncFromBackend()
    }

    func modifyTagsWithoutSync(id: String, add: [String], remove: [String]) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.modifyTags(id: id, add: add, remove: remove)
        } catch {
            Log.error("BACKEND", "Failed to modify tags: \(error)")
            BannerManager.shared.showWarning(title: "Tag Failed", message: "Could not modify tags.")
        }
    }

    /// Sync once and refresh folder counts — call after batch operations
    func syncAndRefresh() async {
        syncFromBackend()
        await refreshFolderCounts()
    }

    func fetchAllTags() async -> [String] {
        guard let backend = emailBackend else { return [] }
        let profile = ProfileManager.shared.currentProfile
        if let profile, !profile.isAll {
            return await backend.fetchTags(accounts: profile.accounts)
        }
        return await backend.fetchAllTags()
    }

    func togglePin(id: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.togglePin(id: id)
        } catch {
            Log.error("BACKEND", "Failed to toggle pin: \(error)")
            BannerManager.shared.showWarning(title: "Pin Failed", message: "Could not toggle pin.")
        }
        syncFromBackend()
    }

    func toggleRead(id: String) async {
        guard let backend = emailBackend else { return }
        do {
            try await backend.toggleRead(id: id)
        } catch {
            Log.error("BACKEND", "Failed to toggle read: \(error)")
            BannerManager.shared.showWarning(title: "Read Status Failed", message: "Could not update read status.")
        }
        syncFromBackend()
    }

    /// Optimistically remove emails from the local list without touching the backend.
    func removeLocally(ids: Set<String>) {
        mailMessages.removeAll { ids.contains($0.id) }
        emailListGeneration += 1
    }

    // MARK: - Batch Operations (Multi-Selection)

    func deleteMessages(ids: Set<String>) async {
        await deleteMessagesWithoutSync(ids: ids)
        syncFromBackend()
    }

    func deleteMessagesWithoutSync(ids: Set<String>) async {
        guard let backend = emailBackend else { return }
        var failCount = 0
        for id in ids {
            do { try await backend.deleteMessage(id: id) }
            catch { failCount += 1; Log.error("BACKEND", "Failed to delete \(id): \(error)") }
        }
        if failCount > 0 {
            BannerManager.shared.showWarning(title: "Delete Failed", message: "Could not delete \(failCount) message(s).")
        }
    }

    func toggleReadForMessages(ids: Set<String>) async {
        guard let backend = emailBackend else { return }
        var failCount = 0
        for id in ids {
            do { try await backend.toggleRead(id: id) }
            catch { failCount += 1; Log.error("BACKEND", "Failed to toggle read \(id): \(error)") }
        }
        if failCount > 0 {
            BannerManager.shared.showWarning(title: "Read Status Failed", message: "Could not update \(failCount) message(s).")
        }
        syncFromBackend()
    }

    func markMessagesAsRead(ids: Set<String>) async {
        guard let backend = emailBackend else { return }
        var failCount = 0
        for id in ids {
            do { try await backend.markAsRead(id: id) }
            catch { failCount += 1; Log.error("BACKEND", "Failed to mark read \(id): \(error)") }
        }
        if failCount > 0 {
            BannerManager.shared.showWarning(title: "Read Status Failed", message: "Could not update \(failCount) message(s).")
        }
        syncFromBackend()
    }

    func markMessagesAsUnread(ids: Set<String>) async {
        guard let backend = emailBackend else { return }
        var failCount = 0
        for id in ids {
            do { try await backend.markAsUnread(id: id) }
            catch { failCount += 1; Log.error("BACKEND", "Failed to mark unread \(id): \(error)") }
        }
        if failCount > 0 {
            BannerManager.shared.showWarning(title: "Read Status Failed", message: "Could not update \(failCount) message(s).")
        }
        syncFromBackend()
    }

    // MARK: - Notification Navigation

    /// Select an email by thread ID (called when user clicks a notification)
    func selectEmail(threadId: String) {
        guard mailMessages.contains(where: { $0.id == threadId }) else {
            Log.debug("BACKEND", "Notification thread \(threadId) not found in current list")
            return
        }
        pendingNotificationThreadId = threadId
        NSApp.activate(ignoringOtherApps: true)
    }

    // MARK: - Full Reload

    func reloadEmail() async {
        guard let backend = emailBackend else { return }

        isLoadingEmails = true

        // Quick sync via SyncManager
        let success = await SyncManager.shared.quickSync()

        if !success {
            loadingProgress = SyncManager.shared.syncState.statusText
            isLoadingEmails = false
            return
        }

        // Reload from backend
        loadingProgress = "Loading..."
        Log.debug("BACKEND", "Reloading from backend")
        await backend.reload()
        syncFromBackend()
    }
}
