@testable import durian_lib
import SwiftUI
import XCTest

@MainActor
final class SyncManagerTests: XCTestCase {

    // MARK: - SyncState.color

    func testSyncStateIdleColor() {
        XCTAssertEqual(SyncState.idle.color, Color.secondary)
    }

    func testSyncStateSyncingColor() {
        XCTAssertEqual(SyncState.syncing.color, Color.blue)
    }

    func testSyncStateSuccessColor() {
        XCTAssertEqual(SyncState.success.color, Color.green)
    }

    func testSyncStateFailedColor() {
        XCTAssertEqual(SyncState.failed("err").color, Color.red)
    }

    // MARK: - SyncState.shouldNotify

    func testSyncStateShouldNotifyOnlyForFailed() {
        XCTAssertFalse(SyncState.idle.shouldNotify)
        XCTAssertFalse(SyncState.syncing.shouldNotify)
        XCTAssertFalse(SyncState.success.shouldNotify)
        XCTAssertTrue(SyncState.failed("error").shouldNotify)
    }

    // MARK: - SyncState.statusText

    func testSyncStateStatusText() {
        XCTAssertEqual(SyncState.idle.statusText, "")
        XCTAssertEqual(SyncState.syncing.statusText, "Syncing...")
        XCTAssertEqual(SyncState.success.statusText, "Synced")
        XCTAssertEqual(SyncState.failed("timeout").statusText, "Failed: timeout")
    }

    // MARK: - SyncState Equatable

    func testSyncStateEquality() {
        XCTAssertEqual(SyncState.idle, SyncState.idle)
        XCTAssertEqual(SyncState.syncing, SyncState.syncing)
        XCTAssertEqual(SyncState.success, SyncState.success)
        XCTAssertEqual(SyncState.failed("a"), SyncState.failed("a"))
        XCTAssertNotEqual(SyncState.failed("a"), SyncState.failed("b"))
        XCTAssertNotEqual(SyncState.idle, SyncState.syncing)
    }

    // MARK: - SyncManager Singleton Contract

    func testSharedReturnsSameInstance() {
        XCTAssertTrue(SyncManager.shared === SyncManager.shared)
    }

    func testIsSyncingFalseWhenIdle() {
        // When state is idle, no sync is in progress
        XCTAssertFalse(SyncManager.shared.syncState == .syncing)
    }

    func testStopTimersDoesNotCrash() {
        // stopTimers() should be idempotent and safe to call without setup
        SyncManager.shared.stopTimers()
        SyncManager.shared.stopTimers()  // Called twice — must not crash
    }

    // MARK: - Startup Full Sync Ordering

    func testOverdueFullSyncWaitsForMailboxSuccessAndStartsOnce() async {
        let state = StartupSyncState()
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        XCTAssertTrue(manager.hasPendingStartupFullSync)
        XCTAssertFalse(manager.hasFinishedStartupMailbox)
        XCTAssertEqual(state.runCount, 0)

        manager.startupMailboxDidFinish(success: true)
        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        XCTAssertTrue(manager.hasFinishedStartupMailbox)
        XCTAssertFalse(manager.hasPendingStartupFullSync)

        manager.startupMailboxDidFinish(success: true)
        manager.restartTimers()
        try? await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(state.runCount, 1)
    }

    func testBackendFailureAlsoReleasesPendingOverdueSync() async {
        let state = StartupSyncState()
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        manager.startupMailboxDidFinish(success: false)

        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        XCTAssertEqual(state.runCount, 1)
    }

    func testNotOverdueDoesNotBecomePending() async {
        let state = StartupSyncState()
        state.lastFullSyncAt = state.now.addingTimeInterval(-100)
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        manager.startupMailboxDidFinish(success: true)
        try? await Task.sleep(for: .milliseconds(20))

        XCTAssertFalse(manager.hasPendingStartupFullSync)
        XCTAssertEqual(state.runCount, 0)
    }

    func testDisabledThenEnabledAfterMailboxRunsCatchupOnce() async {
        let state = StartupSyncState()
        state.enabled = false
        let manager = makeStartupManager(state)

        manager.restartTimers()
        manager.startupMailboxDidFinish(success: true)
        XCTAssertEqual(state.runCount, 0)

        state.enabled = true
        manager.restartTimers()

        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        manager.restartTimers()
        try? await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(state.runCount, 1)
    }

    func testReconnectBeforeMailboxCreatesPendingThenCompletionReleases() async {
        let state = StartupSyncState()
        state.online = false
        let manager = makeStartupManager(state)

        manager.restartTimers()
        XCTAssertFalse(manager.hasPendingStartupFullSync)

        state.online = true
        manager.restartTimers()
        XCTAssertTrue(manager.hasPendingStartupFullSync)
        XCTAssertEqual(state.runCount, 0)

        manager.startupMailboxDidFinish(success: true)
        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        XCTAssertEqual(state.runCount, 1)
    }

    func testPendingSyncRechecksDisabledAndRunsWhenReenabled() async {
        let state = StartupSyncState()
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        state.enabled = false
        manager.startupMailboxDidFinish(success: true)
        try? await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(state.runCount, 0)
        XCTAssertFalse(manager.hasPendingStartupFullSync)

        state.enabled = true
        manager.restartTimers()
        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        XCTAssertEqual(state.runCount, 1)
    }

    func testEligibilityChangeBeforeCatchupTaskDefersUntilReconnect() async {
        let state = StartupSyncState()
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        manager.startupMailboxDidFinish(success: true)
        state.online = false
        try? await Task.sleep(for: .milliseconds(20))

        XCTAssertEqual(state.runCount, 0)
        XCTAssertTrue(manager.hasPendingStartupFullSync)

        state.online = true
        manager.restartTimers()
        let didRun = await waitForRun(state)
        XCTAssertTrue(didRun)
        XCTAssertEqual(state.runCount, 1)
    }

    func testPendingSyncIsExplicitlyClearedWhenNoLongerOverdue() async {
        let state = StartupSyncState()
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        XCTAssertTrue(manager.hasPendingStartupFullSync)
        state.lastFullSyncAt = state.now
        manager.restartTimers()
        XCTAssertFalse(manager.hasPendingStartupFullSync)

        manager.startupMailboxDidFinish(success: true)
        try? await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(state.runCount, 0)
    }

    func testReconnectRetriesOverdueCatchupAfterQuickSyncReleasesLock() async {
        let state = StartupSyncState()
        state.online = false
        let quickSyncGate = StartupAsyncGate()
        let manager = makeStartupManager(state, syncRunner: { _, mailbox, _ in
            guard mailbox == "INBOX" else { return true }
            state.events.append("quick-start")
            await quickSyncGate.wait()
            state.events.append("quick-finish")
            return true
        })

        manager.startupMailboxDidFinish(success: true)
        state.online = true
        let reconnect = Task { @MainActor in await manager.handleReconnect() }

        let quickStarted = await waitUntil { state.events.values.contains("quick-start") }
        let catchupPending = await waitUntil { manager.hasPendingStartupFullSync }
        XCTAssertTrue(quickStarted)
        XCTAssertTrue(catchupPending)
        XCTAssertEqual(state.runCount, 0)

        await quickSyncGate.open()
        await reconnect.value
        let catchupRan = await waitForRun(state)
        XCTAssertTrue(catchupRan)
        XCTAssertEqual(state.events.values, ["quick-start", "quick-finish", "full"])
    }

    func testFailedCatchupRemainsPendingAndRetriesOnRestart() async {
        let state = StartupSyncState()
        state.startupResults = [false, true]
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        manager.startupMailboxDidFinish(success: true)

        let firstAttemptFailed = await waitUntil {
            state.runCount == 1 && manager.hasPendingStartupFullSync
        }
        XCTAssertTrue(firstAttemptFailed)
        manager.restartTimers()

        let retryRan = await waitUntil { state.runCount == 2 }
        XCTAssertTrue(retryRan)
        XCTAssertFalse(manager.hasPendingStartupFullSync)
        XCTAssertEqual(state.events.values, ["full", "full"])
    }

    func testSuccessfulCatchupAllowsNewlyOverdueReconnectLater() async {
        let state = StartupSyncState()
        state.startupResults = [true, true]
        let manager = makeStartupManager(state)

        manager.startFullSyncTimer()
        manager.startupMailboxDidFinish(success: true)
        let firstCatchupRan = await waitUntil { state.runCount == 1 }
        XCTAssertTrue(firstCatchupRan)

        manager.restartTimers()
        try? await Task.sleep(for: .milliseconds(20))
        XCTAssertEqual(state.runCount, 1)

        state.now.addTimeInterval(state.interval)
        manager.restartTimers()
        let laterCatchupRan = await waitUntil { state.runCount == 2 }
        XCTAssertTrue(laterCatchupRan)
        XCTAssertEqual(state.events.values, ["full", "full"])
    }

    func testFullSyncOverdueUsesWallClockBoundary() {
        let now = Date(timeIntervalSinceReferenceDate: 10_000)
        let interval: TimeInterval = 7_200

        XCTAssertTrue(SyncManager.isFullSyncOverdue(last: nil, interval: interval, now: now))
        XCTAssertFalse(SyncManager.isFullSyncOverdue(
            last: now.addingTimeInterval(-interval + 1),
            interval: interval,
            now: now
        ))
        XCTAssertTrue(SyncManager.isFullSyncOverdue(
            last: now.addingTimeInterval(-interval),
            interval: interval,
            now: now
        ))
    }

    private func makeStartupManager(
        _ state: StartupSyncState,
        syncRunner: (@MainActor (String?, String?, TimeInterval) async -> Bool)? = nil
    ) -> SyncManager {
        let profile = Profile(
            name: "All",
            accounts: ["*"],
            isDefault: true,
            color: nil,
            folders: ProfileManager.defaultFolders
        )
        return SyncManager(
            isAutoSyncEnabled: { state.enabled },
            autoFetchInterval: { 120 },
            fullSyncInterval: { state.interval },
            isOnline: { state.online },
            readLastFullSyncAt: { state.lastFullSyncAt },
            writeLastFullSyncAt: { state.lastFullSyncAt = $0 },
            currentDate: { state.now },
            startupFullSync: { state.runStartupFullSync() },
            profileManager: ProfileManager(profiles: [profile], currentProfile: profile),
            configManager: ConfigManager(config: AppConfig(accounts: [])),
            syncRunner: syncRunner,
            timerSchedulingEnabled: false
        )
    }

    private func waitForRun(_ state: StartupSyncState) async -> Bool {
        for _ in 0 ..< 100 {
            if state.runCount > 0 { return true }
            try? await Task.sleep(for: .milliseconds(5))
        }
        return false
    }

    private func waitUntil(_ condition: @MainActor () -> Bool) async -> Bool {
        for _ in 0 ..< 100 {
            if condition() { return true }
            try? await Task.sleep(for: .milliseconds(5))
        }
        return false
    }
}

@MainActor
private final class StartupSyncState {
    var enabled = true
    var online = true
    var interval: TimeInterval = 7_200
    var now = Date(timeIntervalSinceReferenceDate: 10_000)
    var lastFullSyncAt: Date?
    var runCount = 0
    var startupResults = [true]
    let events = StartupSyncEvents()

    func runStartupFullSync() -> Bool {
        runCount += 1
        events.append("full")
        let success = startupResults.isEmpty ? true : startupResults.removeFirst()
        if success {
            lastFullSyncAt = now
        }
        return success
    }
}

private actor StartupAsyncGate {
    private var continuation: CheckedContinuation<Void, Never>?
    private var isOpen = false

    func wait() async {
        guard !isOpen else { return }
        await withCheckedContinuation { continuation = $0 }
    }

    func open() {
        isOpen = true
        continuation?.resume()
        continuation = nil
    }
}

private final class StartupSyncEvents: @unchecked Sendable {
    private let lock = NSLock()
    private var events: [String] = []

    var values: [String] { lock.withLock { events } }

    func append(_ event: String) {
        lock.withLock { events.append(event) }
    }
}
