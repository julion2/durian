@testable import durian_lib
import XCTest

@MainActor
final class StartupCoordinatorTests: XCTestCase {
    func testSuccessfulBatchAppliesBeforeSyncAndReadiness() async throws {
        let events = LockedEvents()
        let configuration = try makeConfiguration()
        let coordinator = StartupCoordinator(
            moduleURLs: moduleURLs,
            fileExists: { _ in true },
            evaluate: { _ in
                events.append("evaluate")
                return configuration
            },
            apply: { _ in events.append("apply") },
            loadFallbacks: { events.append("fallback") },
            setupSync: { events.append("sync") },
            loadSnapshot: { nil },
            refreshSnapshot: {}
        )

        XCTAssertFalse(coordinator.allowsManualReload)
        coordinator.start()
        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertTrue(coordinator.isReady)
        XCTAssertTrue(coordinator.allowsManualReload)
        XCTAssertEqual(events.values, ["evaluate", "apply", "sync"])
    }

    func testBatchFailureRunsFallbackBeforeSyncAndReadiness() async {
        let events = LockedEvents()
        let coordinator = StartupCoordinator(
            moduleURLs: moduleURLs,
            fileExists: { _ in true },
            evaluate: { _ in
                events.append("evaluate")
                throw NSError(domain: "StartupCoordinatorTests", code: 1)
            },
            apply: { _ in events.append("apply") },
            loadFallbacks: { events.append("fallback") },
            setupSync: { events.append("sync") },
            loadSnapshot: { nil },
            refreshSnapshot: {}
        )

        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertEqual(events.values, ["evaluate", "fallback", "sync"])
        XCTAssertTrue(coordinator.isReady)
    }

    func testMissingModuleSkipsDoomedBatchAndUsesFallback() async {
        let events = LockedEvents()
        let configuration = try! makeConfiguration()
        let coordinator = StartupCoordinator(
            moduleURLs: moduleURLs,
            fileExists: { !$0.hasSuffix("profiles.pkl") },
            evaluate: { _ in
                events.append("evaluate")
                return configuration
            },
            apply: { _ in events.append("apply") },
            loadFallbacks: { events.append("fallback") },
            setupSync: { events.append("sync") },
            loadSnapshot: { nil },
            refreshSnapshot: {}
        )

        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertEqual(events.values, ["fallback", "sync"])
    }

    func testValidSnapshotSkipsEvaluationAndBecomesReady() async throws {
        let events = LockedEvents()
        let configuration = try makeConfiguration()
        let coordinator = StartupCoordinator(
            moduleURLs: moduleURLs,
            fileExists: { _ in true },
            evaluate: { _ in
                events.append("evaluate")
                return configuration
            },
            apply: { _ in events.append("apply") },
            loadFallbacks: { events.append("fallback") },
            setupSync: { events.append("sync") },
            loadSnapshot: {
                events.append("snapshot")
                return configuration
            },
            refreshSnapshot: { events.append("refresh") }
        )

        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertTrue(coordinator.isReady)
        XCTAssertEqual(events.values, ["snapshot", "apply", "sync"])
    }

    func testColdEvaluationReachesReadyBeforeBackgroundSnapshotRefresh() async throws {
        let events = LockedEvents()
        let configuration = try makeConfiguration()
        let refreshGate = AsyncGate()
        let coordinator = StartupCoordinator(
            moduleURLs: moduleURLs,
            fileExists: { _ in true },
            evaluate: { _ in
                events.append("evaluate")
                return configuration
            },
            apply: { _ in events.append("apply") },
            loadFallbacks: { events.append("fallback") },
            setupSync: { events.append("sync") },
            loadSnapshot: { nil },
            refreshSnapshot: {
                events.append("refresh")
                await refreshGate.wait()
            }
        )

        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertTrue(coordinator.isReady)
        XCTAssertEqual(Array(events.values.prefix(3)), ["evaluate", "apply", "sync"])
        await refreshGate.open()
    }

    private var moduleURLs: [URL] {
        ["config.pkl", "profiles.pkl", "keymaps.pkl"].map {
            URL(fileURLWithPath: "/config/\($0)")
        }
    }

    private func makeConfiguration() throws -> PklStartupConfiguration {
        let decoder = JSONDecoder()
        return PklStartupConfiguration(
            config: try decoder.decode(AppConfig.self, from: Data(#"{"accounts":[]}"#.utf8)),
            profiles: try decoder.decode(
                ProfilesConfig.self,
                from: Data(#"{"profiles":[{"name":"All","accounts":["*"]}]}"#.utf8)
            ),
            keymaps: KeymapConfig()
        )
    }
}

private final class LockedEvents: @unchecked Sendable {
    private let lock = NSLock()
    private var events: [String] = []

    var values: [String] { lock.withLock { events } }

    func append(_ event: String) {
        lock.withLock { events.append(event) }
    }
}

private actor AsyncGate {
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
