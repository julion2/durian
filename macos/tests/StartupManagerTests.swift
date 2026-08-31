@testable import durian_lib
import XCTest

@MainActor
final class StartupManagerTests: XCTestCase {
    func testInitialSettingsApplicationDoesNotAutosave() async throws {
        let saves = SettingsSaves()
        let manager = SettingsManager(
            configManager: ConfigManager(config: AppConfig(accounts: [])),
            autoSaveDelay: 0.01,
            saveHandler: { saves.append($0.theme) }
        )
        let initial = try JSONDecoder().decode(
            AppConfig.self,
            from: Data(#"{"settings":{"theme":"dark"}}"#.utf8)
        )

        manager.applyEvaluatedConfig(initial)
        try await Task.sleep(for: .milliseconds(30))
        XCTAssertTrue(saves.values.isEmpty)

        manager.settings.theme = "light"
        try await Task.sleep(for: .milliseconds(30))
        XCTAssertEqual(saves.values, ["light"])
    }

    func testManualSettingsReloadDuringStartupReadsAgain() async throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data().write(to: root)
        defer { try? FileManager.default.removeItem(at: root) }
        let evaluations = ManagerCounter()
        let configManager = ConfigManager(
            configURL: root,
            evaluator: { _ in
                let count = evaluations.increment()
                if count == 1 {
                    try await Task.sleep(for: .milliseconds(40))
                }
                let theme = count == 1 ? "dark" : "light"
                return try JSONDecoder().decode(
                    AppConfig.self,
                    from: Data("{\"settings\":{\"theme\":\"\(theme)\"}}".utf8)
                )
            },
            parseErrorHandler: { _ in }
        )
        let manager = SettingsManager(configManager: configManager, autoSaveDelay: 60)

        async let preparation: Void = manager.prepareSettings()
        try await Task.sleep(for: .milliseconds(5))
        async let reload: Void = manager.reloadSettings()
        _ = await (preparation, reload)

        XCTAssertEqual(evaluations.value, 2)
        XCTAssertEqual(manager.settings.theme, "light")
    }

    func testManualKeymapsReloadDuringStartupReadsAgain() async throws {
        let evaluations = ManagerCounter()
        let manager = KeymapsManager(
            configURL: URL(fileURLWithPath: "/unused/keymaps.pkl"),
            evaluator: { _ in
                let count = evaluations.increment()
                if count == 1 {
                    try await Task.sleep(for: .milliseconds(40))
                }
                var config = KeymapConfig()
                config.keymaps = [KeymapEntry(action: count == 1 ? "first" : "reloaded", key: "x")]
                return config
            },
            fileExists: { _ in true }
        )

        async let preparation: Void = manager.prepareKeymaps()
        try await Task.sleep(for: .milliseconds(5))
        async let reload: Void = manager.reloadKeymaps()
        _ = await (preparation, reload)

        XCTAssertEqual(evaluations.value, 2)
        XCTAssertEqual(manager.getKeymap(for: "reloaded")?.key, "x")
    }
}

private final class SettingsSaves: @unchecked Sendable {
    private let lock = NSLock()
    private var themes: [String] = []

    var values: [String] { lock.withLock { themes } }

    func append(_ theme: String) {
        lock.withLock { themes.append(theme) }
    }
}

private final class ManagerCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0

    var value: Int { lock.withLock { count } }

    @discardableResult
    func increment() -> Int {
        lock.withLock {
            count += 1
            return count
        }
    }
}
