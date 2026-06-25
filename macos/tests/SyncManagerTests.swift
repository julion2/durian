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

    // Note: Deeper SyncManager coverage (concurrent sync lock behavior,
    // consecutiveFailures threshold, banner suppression) requires
    // dependency injection for NetworkMonitor / ConfigManager /
    // ProfileManager / runDurianSync. That's a separate refactor.
}
