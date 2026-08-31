@testable import durian_lib
import XCTest

final class ConfigTests: XCTestCase {
    private let fullConfigJSON = """
    {
      "settings": {
        "notifications_enabled": true,
        "theme": "dark",
        "load_remote_images": true
      },
      "sync": {
        "mode": "bidirectional",
        "gui_auto_sync": false,
        "auto_fetch_interval": 120,
        "full_sync_interval": 7200
      },
      "signatures": {
        "default": "Best regards",
        "work": "Kind regards,\\nTest User\\nAcme Corp."
      },
      "accounts": [
        { "name": "Personal", "email": "alice@example.com" },
        { "name": "Work", "email": "alice@company.com", "default_signature": "work" }
      ]
    }
    """

    func testDecodeFullConfig() throws {
        let config = try JSONDecoder().decode(AppConfig.self, from: Data(fullConfigJSON.utf8))

        XCTAssertEqual(config.accounts.count, 2)
        XCTAssertEqual(config.accounts[0].name, "Personal")
        XCTAssertEqual(config.accounts[0].email, "alice@example.com")
        XCTAssertNil(config.accounts[0].defaultSignature)
        XCTAssertEqual(config.accounts[1].name, "Work")
        XCTAssertEqual(config.accounts[1].email, "alice@company.com")
        XCTAssertEqual(config.accounts[1].defaultSignature, "work")
        XCTAssertEqual(config.settings.theme, "dark")
        XCTAssertTrue(config.settings.notificationsEnabled)
        XCTAssertTrue(config.settings.loadRemoteImages)
        XCTAssertEqual(config.sync.mode, "bidirectional")
        XCTAssertFalse(config.sync.guiAutoSync)
        XCTAssertEqual(config.sync.autoFetchInterval, 120.0)
        XCTAssertEqual(config.sync.fullSyncInterval, 7200)
        XCTAssertEqual(config.signatures["default"], "Best regards")
        XCTAssertNotNil(config.signatures["work"])
    }

    func testDecodeMinimalConfigUsesDefaults() throws {
        let config = try JSONDecoder().decode(
            AppConfig.self,
            from: Data(#"{"settings":{},"sync":{},"signatures":{},"accounts":[]}"#.utf8)
        )

        XCTAssertTrue(config.accounts.isEmpty)
        XCTAssertEqual(config.settings.theme, "system")
        XCTAssertTrue(config.settings.notificationsEnabled)
        XCTAssertFalse(config.settings.loadRemoteImages)
        XCTAssertEqual(config.sync.mode, "bidirectional")
        XCTAssertTrue(config.sync.guiAutoSync)
        XCTAssertEqual(config.sync.autoFetchInterval, 120.0)
        XCTAssertTrue(config.signatures.isEmpty)
    }

    func testJSONIntegerDecodesAsTimeInterval() throws {
        let config = try JSONDecoder().decode(
            AppConfig.self,
            from: Data(#"{"sync":{"auto_fetch_interval":60,"full_sync_interval":3600}}"#.utf8)
        )

        XCTAssertEqual(config.sync.autoFetchInterval, 60)
        XCTAssertEqual(config.sync.fullSyncInterval, 3600)
    }

    func testMailAccountSignatureAndCalendarDefaults() throws {
        let account = MailAccount(name: "Work", email: "w@co.com", defaultSignature: "formal")
        XCTAssertEqual(account.defaultSignature, "formal")

        let withoutCalendar = try JSONDecoder().decode(
            MailAccount.self,
            from: Data(#"{"name":"Personal","email":"me@example.com"}"#.utf8)
        )
        let disabledCalendar = try JSONDecoder().decode(
            MailAccount.self,
            from: Data(#"{"name":"Mail only","email":"mail@example.com","calendar":{"enabled":false}}"#.utf8)
        )
        XCTAssertTrue(withoutCalendar.calendarEnabled)
        XCTAssertFalse(disabledCalendar.calendarEnabled)
    }

    func testParseFailureIsVisibleWhileAccountsRemainEmpty() async throws {
        let configURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        let reportedMessage = LockedValue<String?>(nil)
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                throw NSError(
                    domain: "ConfigTests",
                    code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "invalid syntax"]
                )
            },
            parseErrorHandler: { reportedMessage.set($0) }
        )
        await manager.reloadConfig()

        XCTAssertEqual(manager.lastParseError, "invalid syntax")
        XCTAssertTrue(manager.getAccounts().isEmpty)
        XCTAssertEqual(
            reportedMessage.get(),
            "Configuration failed to parse: invalid syntax. The bundled GUI schema may be out of date — run ./macos/install.sh to rebuild."
        )
    }

    func testSuccessfulReloadClearsParseError() async throws {
        let configURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        let shouldFail = LockedValue(true)
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                if shouldFail.get() {
                    throw NSError(
                        domain: "ConfigTests",
                        code: 1,
                        userInfo: [NSLocalizedDescriptionKey: "invalid syntax"]
                    )
                }
                return AppConfig(accounts: [MailAccount(name: "Work", email: "work@example.com")])
            },
            parseErrorHandler: { _ in }
        )
        await manager.reloadConfig()
        XCTAssertNotNil(manager.lastParseError)

        shouldFail.set(false)
        await manager.reloadConfig()

        XCTAssertNil(manager.lastParseError)
        XCTAssertEqual(manager.getAccounts().map(\.email), ["work@example.com"])
    }

    func testFailedOrMissingReloadClearsPreviouslyLoadedConfig() async throws {
        let configURL = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        let shouldFail = LockedValue(false)
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                if shouldFail.get() {
                    throw NSError(
                        domain: "ConfigTests",
                        code: 1,
                        userInfo: [NSLocalizedDescriptionKey: "invalid syntax"]
                    )
                }
                return AppConfig(accounts: [MailAccount(name: "Work", email: "work@example.com")])
            },
            parseErrorHandler: { _ in }
        )
        await manager.reloadConfig()
        XCTAssertEqual(manager.getAccounts().map(\.email), ["work@example.com"])

        shouldFail.set(true)
        await manager.reloadConfig()
        XCTAssertEqual(manager.lastParseError, "invalid syntax")
        XCTAssertTrue(manager.getAccounts().isEmpty)

        try FileManager.default.removeItem(at: configURL)
        await manager.reloadConfig()
        XCTAssertNil(manager.lastParseError)
        XCTAssertTrue(manager.getAccounts().isEmpty)
    }
}

private final class LockedValue<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value

    init(_ value: Value) {
        self.value = value
    }

    func get() -> Value {
        lock.withLock { value }
    }

    func set(_ value: Value) {
        lock.withLock { self.value = value }
    }
}
