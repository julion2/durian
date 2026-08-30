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
            setupSync: { events.append("sync") }
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
            setupSync: { events.append("sync") }
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
            setupSync: { events.append("sync") }
        )

        coordinator.start()
        await coordinator.waitUntilReady()

        XCTAssertEqual(events.values, ["fallback", "sync"])
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
