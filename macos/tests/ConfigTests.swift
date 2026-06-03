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
}
