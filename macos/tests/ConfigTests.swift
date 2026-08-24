@testable import durian_lib
import XCTest

final class ConfigTests: XCTestCase {

    // MARK: - Test JSON Strings (as pkl eval would produce)

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

    // MARK: - Full Config Decoding

    func testDecodeFullConfig() throws {
        let data = fullConfigJSON.data(using: .utf8)!
        let config = try JSONDecoder().decode(AppConfig.self, from: data)

        // Accounts
        XCTAssertEqual(config.accounts.count, 2)
        XCTAssertEqual(config.accounts[0].name, "Personal")
        XCTAssertEqual(config.accounts[0].email, "alice@example.com")
        XCTAssertNil(config.accounts[0].defaultSignature)
        XCTAssertEqual(config.accounts[1].name, "Work")
        XCTAssertEqual(config.accounts[1].email, "alice@company.com")
        XCTAssertEqual(config.accounts[1].defaultSignature, "work")

        // Settings
        XCTAssertEqual(config.settings.theme, "dark")
        XCTAssertTrue(config.settings.notificationsEnabled)
        XCTAssertTrue(config.settings.loadRemoteImages)

        // Sync
        XCTAssertEqual(config.sync.mode, "bidirectional")
        XCTAssertFalse(config.sync.guiAutoSync)
        XCTAssertEqual(config.sync.autoFetchInterval, 120.0)
        XCTAssertEqual(config.sync.fullSyncInterval, 7200)

        // Signatures
        XCTAssertEqual(config.signatures["default"], "Best regards")
        XCTAssertNotNil(config.signatures["work"])
    }

    // MARK: - Minimal Config (defaults)

    func testDecodeMinimalConfig() throws {
        let minimalJSON = """
        { "settings": {}, "sync": {}, "signatures": {}, "accounts": [] }
        """
        let data = minimalJSON.data(using: .utf8)!
        let config = try JSONDecoder().decode(AppConfig.self, from: data)

        XCTAssertEqual(config.accounts.count, 0)
        XCTAssertEqual(config.settings.theme, "system")
        XCTAssertTrue(config.settings.notificationsEnabled)
        XCTAssertFalse(config.settings.loadRemoteImages)
        XCTAssertEqual(config.sync.mode, "bidirectional")
        XCTAssertTrue(config.sync.guiAutoSync)
        XCTAssertEqual(config.sync.autoFetchInterval, 120.0)
        XCTAssertTrue(config.signatures.isEmpty)
    }

    // MARK: - MailAccount

    func testMailAccountWithSignature() {
        let account = MailAccount(name: "Work", email: "w@co.com", defaultSignature: "formal")
        XCTAssertEqual(account.defaultSignature, "formal")
    }

    func testMailAccountWithoutSignature() {
        let account = MailAccount(name: "Personal", email: "me@me.com")
        XCTAssertNil(account.defaultSignature)
    }

    func testCalendarDefaultsEnabled() throws {
        let withoutCalendar = try JSONDecoder().decode(
            MailAccount.self,
            from: Data(#"{"name":"Personal","email":"me@example.com"}"#.utf8)
        )
        let emptyCalendar = try JSONDecoder().decode(
            MailAccount.self,
            from: Data(#"{"name":"Work","email":"work@example.com","calendar":{}}"#.utf8)
        )

        XCTAssertTrue(withoutCalendar.calendarEnabled)
        XCTAssertTrue(emptyCalendar.calendarEnabled)
    }

    func testCalendarCanBeDisabled() throws {
        let account = try JSONDecoder().decode(
            MailAccount.self,
            from: Data(#"{"name":"Mail only","email":"mail@example.com","calendar":{"enabled":false}}"#.utf8)
        )

        XCTAssertFalse(account.calendarEnabled)
    }

    // MARK: - Config Loading Errors

    func testParseFailureIsVisibleWhileAccountsRemainEmpty() throws {
        let configURL = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        var reportedMessage: String?
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                throw NSError(
                    domain: "ConfigTests",
                    code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "invalid syntax"]
                )
            },
            parseErrorHandler: { reportedMessage = $0 }
        )

        XCTAssertEqual(manager.lastParseError, "invalid syntax")
        XCTAssertTrue(manager.getAccounts().isEmpty)
        XCTAssertEqual(
            reportedMessage,
            "Configuration failed to parse: invalid syntax. The bundled GUI schema may be out of date — run ./macos/install.sh to rebuild."
        )
    }

    func testSuccessfulReloadClearsParseError() throws {
        let configURL = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        var shouldFail = true
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                if shouldFail {
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
        XCTAssertNotNil(manager.lastParseError)

        shouldFail = false
        manager.reloadConfig()

        XCTAssertNil(manager.lastParseError)
        XCTAssertEqual(manager.getAccounts().map(\.email), ["work@example.com"])
    }

    func testFailedReloadClearsPreviouslyLoadedConfig() throws {
        let configURL = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        var shouldFail = false
        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                if shouldFail {
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
        XCTAssertEqual(manager.getAccounts().map(\.email), ["work@example.com"])

        shouldFail = true
        manager.reloadConfig()

        XCTAssertEqual(manager.lastParseError, "invalid syntax")
        XCTAssertTrue(manager.getAccounts().isEmpty)
    }

    func testMissingConfigOnReloadClearsPreviouslyLoadedConfig() throws {
        let configURL = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try Data().write(to: configURL)
        defer { try? FileManager.default.removeItem(at: configURL) }

        let manager = ConfigManager(
            configURL: configURL,
            evaluator: { _ in
                AppConfig(accounts: [MailAccount(name: "Work", email: "work@example.com")])
            },
            parseErrorHandler: { _ in }
        )
        XCTAssertEqual(manager.getAccounts().map(\.email), ["work@example.com"])

        try FileManager.default.removeItem(at: configURL)
        manager.reloadConfig()

        XCTAssertNil(manager.lastParseError)
        XCTAssertTrue(manager.getAccounts().isEmpty)
    }
}
