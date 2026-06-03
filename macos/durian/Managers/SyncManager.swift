//
//  SyncManager.swift
//  Durian
//
//  Manages email synchronization via durian CLI
//

import Combine
import Foundation
import SwiftUI
import UserNotifications

// MARK: - Sync State

enum SyncState: Equatable {
    case idle
    case syncing           // Rotating icon - sync in progress
    case success           // Green - sync completed
    case failed(String)    // Red - sync failed

    var color: Color {
        switch self {
        case .idle: return .secondary
        case .syncing: return .blue
        case .success: return .green
        case .failed: return .red
        }
    }

    var shouldNotify: Bool {
        switch self {
        case .failed: return true
        default: return false
        }
    }

    var statusText: String {
        switch self {
        case .idle: return ""
        case .syncing: return "Syncing..."
        case .success: return "Synced"
        case .failed(let reason): return "Failed: \(reason)"
        }
    }
}

// MARK: - Sync Manager

@MainActor
class SyncManager: ObservableObject {
    static let shared = SyncManager()

    // MARK: - Published State
    @Published var syncState: SyncState = .idle
    @Published var lastSyncTime: Date?

    // MARK: - Sync Lock (prevents multiple concurrent syncs)
    private var syncLock = false

    // MARK: - Failure Tracking (suppress transient failure banners)
    private var consecutiveFailures: Int = 0
    private var userSawFailureBanner: Bool = false
    private var pendingOfflineBanner: DispatchWorkItem?

    // MARK: - SSE Event Stream
    private var eventStream: EventStreamClient?
    private var reloadDebounceTask: DispatchWorkItem?

    /// True if a sync is currently in progress
    var isSyncing: Bool { syncLock }

    // MARK: - Paths
    private let durianPath: String

    // MARK: - Timers
    private var quickSyncTimer: Timer?
    private var fullSyncTimer: Timer?
    private var cancellables = Set<AnyCancellable>()

    private init() {
        // Initial path resolution, will be refreshed in runDurianSync if needed
        durianPath = FileManager.default.resolveDurianPath() ?? ""
    }

    // MARK: - Setup (call on app start)

    func setup() {
        Log.debug("SYNC", "Setting up SyncManager...")
        Log.debug("SYNC", "Config - guiAutoSync=\(SettingsManager.shared.guiAutoSync), autoFetchInterval=\(SettingsManager.shared.autoFetchInterval)s, fullSyncInterval=\(SettingsManager.shared.fullSyncInterval)s")

        // Start timers based on config (if online)
        if NetworkMonitor.shared.isConnected {
            startQuickSyncTimer()
            startFullSyncTimer()
        } else {
            Log.debug("SYNC", "Offline at startup, timers not started")
        }

        // Start SSE event stream for real-time new-mail notifications
        let stream = EventStreamClient()
        stream.onNewMail = { [weak self] event in
            Task { @MainActor in
                self?.handleNewMailEvent(event)
            }
        }
        stream.onOutboxUpdate = { [weak self] event in
            Task { @MainActor in
                self?.handleOutboxUpdate(event)
            }
        }
        stream.connect()
        eventStream = stream

        // React to network changes
        NetworkMonitor.shared.$isConnected
            .dropFirst()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] isConnected in
                Task { @MainActor in
                    if isConnected {
                        Log.info("SYNC", "Back online, restarting timers and syncing")
                        self?.pendingOfflineBanner?.cancel()
                        self?.pendingOfflineBanner = nil
                        if self?.userSawFailureBanner == true {
                            BannerManager.shared.showSuccess(title: "Back Online", message: "Connection restored. Syncing now...")
                            self?.userSawFailureBanner = false
                        }
                        self?.restartTimers()
                        await self?.quickSync()
                    } else {
                        Log.info("SYNC", "Went offline, stopping timers")
                        self?.stopTimers()
                        // Delay offline banner — brief disconnects (WiFi switch) shouldn't notify
                        let work = DispatchWorkItem { [weak self] in
                            Task { @MainActor in
                                guard let self = self, !NetworkMonitor.shared.isConnected else { return }
                                self.userSawFailureBanner = true
                                BannerManager.shared.showWarning(title: "Offline", message: "No network connection. Sync paused.")
                            }
                        }
                        self?.pendingOfflineBanner = work
                        DispatchQueue.main.asyncAfter(deadline: .now() + 30, execute: work)
                    }
                }
            }
            .store(in: &cancellables)

        Log.info("SYNC", "Setup complete")
    }

    // MARK: - Timer Management

    func startQuickSyncTimer() {
        guard SettingsManager.shared.guiAutoSync else {
            Log.debug("SYNC", "GUI auto-sync disabled, not starting quick sync timer")
            return
        }
        guard NetworkMonitor.shared.isConnected else {
            Log.debug("SYNC", "Offline, not starting quick sync timer")
            return
        }

        let interval = SettingsManager.shared.autoFetchInterval
        Log.debug("SYNC", "Starting quick sync timer with interval \(interval)s")

        quickSyncTimer?.invalidate()
        quickSyncTimer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self = self else { return }
                guard !self.syncLock else {
                    Log.debug("SYNC", "Quick sync timer skipped - sync already in progress")
                    return
                }
                guard NetworkMonitor.shared.isConnected else {
                    Log.debug("SYNC", "Quick sync timer skipped - offline")
                    return
                }
                await self.quickSync()
            }
        }
    }

    func startFullSyncTimer() {
        guard SettingsManager.shared.guiAutoSync else {
            Log.debug("SYNC", "GUI auto-sync disabled, not starting full sync timer")
            return
        }
        guard NetworkMonitor.shared.isConnected else {
            Log.debug("SYNC", "Offline, not starting full sync timer")
            return
        }

        let interval = SettingsManager.shared.fullSyncInterval
        Log.debug("SYNC", "Starting full sync timer with interval \(interval)s (\(interval/3600)h)")

        fullSyncTimer?.invalidate()
        fullSyncTimer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self = self else { return }
                guard !self.syncLock else {
                    Log.debug("SYNC", "Full sync timer skipped - sync already in progress")
                    return
                }
                guard NetworkMonitor.shared.isConnected else {
                    Log.debug("SYNC", "Full sync timer skipped - offline")
                    return
                }
                await self.fullSync()
            }
        }
    }

    func stopTimers() {
        Log.debug("SYNC", "Stopping all sync timers")
        quickSyncTimer?.invalidate()
        quickSyncTimer = nil
        fullSyncTimer?.invalidate()
        fullSyncTimer = nil
    }

    func restartTimers() {
        Log.debug("SYNC", "Restarting timers with new settings")
        stopTimers()
        if SettingsManager.shared.guiAutoSync {
            startQuickSyncTimer()
            startFullSyncTimer()
        }
    }

    // MARK: - Failure Suppression

    /// Show banner only after 3+ consecutive failures
    private func showFailureBannerIfThresholdMet(title: String, message: String) {
        guard consecutiveFailures >= 3 else {
            Log.debug("SYNC", "Suppressing banner (\(consecutiveFailures)/3 consecutive failures)")
            return
        }
        userSawFailureBanner = true
        BannerManager.shared.showWarning(title: title, message: message)
    }

    /// Reset failure state on successful sync
    private func recordSyncSuccess() {
        consecutiveFailures = 0
        userSawFailureBanner = false
    }

    // MARK: - Quick Sync (Cmd+R)

    /// Quick sync - syncs current profile's INBOX only
    @discardableResult
    func quickSync() async -> Bool {
        guard !syncLock else {
            Log.debug("SYNC", "Quick sync - already syncing, skipping")
            return false
        }

        syncLock = true
        defer { syncLock = false }

        // Get current profile for targeted sync
        guard let currentProfile = ProfileManager.shared.currentProfile else {
            Log.debug("SYNC", "Quick sync - no current profile, skipping")
            return false
        }

        // Use account name for single-account profiles, sync all for multi/wildcard.
        // Profile accounts are maildir path names — only pass to sync if there's
        // a matching CLI account (by name, case-insensitive). Otherwise sync all.
        let knownAccountNames = Set(ConfigManager.shared.getAccounts().map { $0.name.lowercased() })
        let accountName: String?
        if !currentProfile.isAll && currentProfile.accounts.count == 1,
           let profileAccount = currentProfile.accounts.first,
           knownAccountNames.contains(profileAccount.lowercased())
        {
            accountName = profileAccount
        } else {
            accountName = nil
        }
        Log.debug("SYNC", "Quick sync starting for \(accountName ?? "all") INBOX")
        syncState = .syncing

        let success = await runDurianSync(account: accountName, mailbox: "INBOX", timeout: 60)

        if success {
            Log.info("SYNC", "Quick sync completed successfully")
            recordSyncSuccess()
            syncState = .success

            // Reload email list to show new messages (before updating lastSyncTime
            // so the notification recency filter uses the previous sync time)
            await reloadEmailList()
            lastSyncTime = Date()

            // After 3 seconds, go back to idle
            Task {
                try? await Task.sleep(nanoseconds: 3_000_000_000)
                if case .success = self.syncState {
                    self.syncState = .idle
                }
            }
        } else {
            Log.error("SYNC", "Quick sync failed")
            consecutiveFailures += 1
            syncState = .failed("sync error")
            if !NetworkMonitor.shared.isConnected {
                showFailureBannerIfThresholdMet(title: "Offline", message: "Sync skipped — no network connection.")
            } else {
                showFailureBannerIfThresholdMet(title: "Sync Failed", message: "Could not sync emails. Will retry automatically.")
            }
        }

        return success
    }

    // MARK: - Full Sync (Cmd+Shift+R or timer)

    /// Full sync - syncs all accounts with longer timeout
    @discardableResult
    func fullSync() async -> Bool {
        guard !syncLock else {
            Log.debug("SYNC", "Full sync - already syncing, skipping")
            return false
        }

        syncLock = true
        defer { syncLock = false }

        Log.debug("SYNC", "Full sync starting (all accounts)")
        // No UI feedback for full sync (runs in background)

        let success = await runDurianSync(account: nil, mailbox: nil, timeout: 300)

        if success {
            Log.info("SYNC", "Full sync completed successfully")
            recordSyncSuccess()

            // Reload before updating lastSyncTime (notification recency filter)
            await reloadEmailList()
            lastSyncTime = Date()
        } else {
            Log.error("SYNC", "Full sync failed")
            consecutiveFailures += 1
            if !NetworkMonitor.shared.isConnected {
                showFailureBannerIfThresholdMet(title: "Offline", message: "Background sync skipped — no network connection.")
            } else {
                showFailureBannerIfThresholdMet(title: "Full Sync Failed", message: "Background sync encountered an error.")
            }
        }

        return success
    }

    // MARK: - Core Sync Logic

    /// Run durian sync with optional account and mailbox targeting
    /// - Parameters:
    ///   - account: Specific account name to sync (nil = all accounts)
    ///   - mailbox: Specific mailbox to sync (nil = all mailboxes)
    ///   - timeout: Command timeout in seconds
    private func runDurianSync(account: String?, mailbox: String?, timeout: TimeInterval) async -> Bool {
        guard let resolvedPath = FileManager.default.resolveDurianPath() else {
            Log.error("SYNC", "durian CLI not found in ~/.local/bin or /usr/local/bin")
            BannerManager.shared.showCritical(title: "Durian CLI Not Found", message: "Install durian to sync emails.")
            return false
        }

        // Build command args: sync [account] [mailbox]
        var args = ["sync"]
        if let account = account {
            args.append(account)
            if let mailbox = mailbox {
                args.append(mailbox)
            }
        }

        Log.debug("SYNC", "Running \(resolvedPath) \(args.joined(separator: " ")) (timeout: \(Int(timeout))s)")
        let result = await runCommand(resolvedPath, args: args, timeout: timeout)

        if result.success {
            Log.debug("SYNC", "durian sync completed successfully")
            if let output = result.output, !output.isEmpty {
                Log.debug("SYNC", "Output: \(output.prefix(500))")
            }
        } else {
            Log.error("SYNC", "durian sync failed")
            if let error = result.error, !error.isEmpty {
                Log.error("SYNC", "Error: \(error)")
            }
        }

        return result.success
    }

    /// Reload the email list after sync to show new messages
    private func reloadEmailList() async {
        guard let backend = AccountManager.shared.emailBackend else { return }
        Log.debug("SYNC", "Reloading email list")
        await backend.reload()
    }

    // MARK: - SSE New Mail Handler

    /// Handle a new_mail SSE event — send notifications and refresh the email list.
    private func handleNewMailEvent(_ event: NewMailEvent) {
        Log.info("NOTIFY", "SSE event: account=\(event.account) total_new=\(event.total_new) messages=\(event.messages.count)")
        for (i, msg) in event.messages.enumerated() {
            Log.debug("NOTIFY", "  [\(i)] thread=\(msg.thread_id) from=\(msg.from) subject=\(msg.subject)")
        }

        // Debounced reload — multiple accounts fire SSE events within seconds
        reloadDebounceTask?.cancel()
        let work = DispatchWorkItem { [weak self] in
            Task { @MainActor in
                guard let backend = AccountManager.shared.emailBackend else { return }
                await backend.reload()
                await AccountManager.shared.refreshFolderCounts()
                self?.lastSyncTime = Date()
            }
        }
        reloadDebounceTask = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5, execute: work)

        // Notifications
        guard SettingsManager.shared.settings.notificationsEnabled else {
            Log.debug("NOTIFY", "Notifications disabled globally, skipping")
            return
        }
        // Per-account notification filter
        if let account = ConfigManager.shared.getAccounts().first(where: { $0.email == event.account }),
           let notify = account.notifications, !notify
        {
            Log.debug("NOTIFY", "Notifications disabled for account \(event.account), skipping")
            return
        }
        guard !event.messages.isEmpty else { return }

        let center = UNUserNotificationCenter.current()

        if event.messages.count <= 3 {
            // Individual notifications — sender, subject, body preview
            for msg in event.messages {
                let content = UNMutableNotificationContent()
                content.title = msg.from
                content.subtitle = msg.subject
                content.body = msg.snippet
                content.sound = .default
                content.userInfo = ["threadId": msg.thread_id]

                let identifier = "newmail-\(msg.thread_id)"
                Log.info("NOTIFY", "Posting notification: id=\(identifier) thread=\(msg.thread_id) from=\(msg.from) subject=\(msg.subject)")
                let request = UNNotificationRequest(
                    identifier: identifier,
                    content: content,
                    trigger: nil
                )
                center.add(request)
            }
        } else {
            // Batch — less noisy for bulk arrivals
            let content = UNMutableNotificationContent()
            content.title = "\(event.total_new) new emails"
            content.body = event.messages.prefix(3)
                .map { "\($0.from): \($0.subject)" }
                .joined(separator: "\n")
            content.sound = .default
            content.userInfo = ["threadId": event.messages[0].thread_id]

            let identifier = "newmail-batch-\(event.account)"
            Log.info("NOTIFY", "Posting batch notification: id=\(identifier) count=\(event.total_new)")
            let request = UNNotificationRequest(
                identifier: identifier,
                content: content,
                trigger: nil
            )
            center.add(request)
        }
    }

    // MARK: - Outbox Update Handler

    /// Handle an outbox_update SSE event — show banners for sent/failed status.
    private func handleOutboxUpdate(_ event: OutboxUpdateEvent) {
        Log.info("OUTBOX", "SSE outbox_update: id=\(event.item_id) status=\(event.status)")

        switch event.status {
        case "sent":
            // If undo countdown is still active for this item, let it handle cleanup
            // instead of showing a duplicate "Sent" banner.
            if EmailSendingManager.shared.isUndoActive(itemId: event.item_id) {
                EmailSendingManager.shared.handleSentEvent(itemId: event.item_id)
            } else {
                let subject = event.subject ?? "Email"
                BannerManager.shared.showSuccess(title: "Sent Successfully", message: "\(subject) has been delivered.")
            }
            // Refresh email list so Sent folder shows the new message
            Task {
                await AccountManager.shared.emailBackend?.reload()
                await AccountManager.shared.refreshFolderCounts()
            }
        case "failed":
            let detail = event.error ?? "Unknown error"
            let subject = event.subject ?? "Email"
            BannerManager.shared.showWarning(title: "\(subject) Not Sent", message: detail)
        default:
            break
        }

        // Notify OutboxManager to refresh counts
        OutboxManager.shared.refresh()
    }

    // MARK: - Command Execution

    private struct CommandResult {
        let success: Bool
        let output: String?
        let error: String?
    }

    /// Run a command directly with timeout
    private func runCommand(_ path: String, args: [String], timeout: TimeInterval) async -> CommandResult {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: path)
                process.arguments = args

                // Set up environment with Homebrew paths
                var env = ProcessInfo.processInfo.environment
                let homebrewPaths = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin"
                if let existingPath = env["PATH"] {
                    env["PATH"] = "\(homebrewPaths):\(existingPath)"
                } else {
                    env["PATH"] = "\(homebrewPaths):/usr/bin:/bin:/usr/sbin:/sbin"
                }
                process.environment = env

                let outputPipe = Pipe()
                let errorPipe = Pipe()
                process.standardOutput = outputPipe
                process.standardError = errorPipe

                // Set up timeout
                var didTimeout = false
                let timeoutWorkItem = DispatchWorkItem {
                    if process.isRunning {
                        Log.error("SYNC", "Command timed out after \(timeout)s, terminating process")
                        didTimeout = true
                        process.terminate()
                    }
                }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: timeoutWorkItem)

                do {
                    try process.run()
                    process.waitUntilExit()

                    // Cancel timeout timer if process completed
                    timeoutWorkItem.cancel()

                    if didTimeout {
                        continuation.resume(returning: CommandResult(
                            success: false,
                            output: nil,
                            error: "Command timed out after \(Int(timeout)) seconds"
                        ))
                        return
                    }

                    let outputData = outputPipe.fileHandleForReading.readDataToEndOfFile()
                    let errorData = errorPipe.fileHandleForReading.readDataToEndOfFile()

                    let output = String(data: outputData, encoding: .utf8)
                    let error = String(data: errorData, encoding: .utf8)

                    let success = process.terminationStatus == 0
                    continuation.resume(returning: CommandResult(success: success, output: output, error: error))
                } catch {
                    timeoutWorkItem.cancel()
                    continuation.resume(returning: CommandResult(success: false, output: nil, error: error.localizedDescription))
                }
            }
        }
    }
}
