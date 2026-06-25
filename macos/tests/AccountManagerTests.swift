@testable import durian_lib
import XCTest

@MainActor
final class AccountManagerTests: XCTestCase {

    // MARK: - removeLocally

    func testRemoveLocallyRemovesMessages() {
        let manager = AccountManager.shared

        // Seed some messages
        let msg1 = MailMessage(threadId: "t1", subject: "First", from: "a@b.com",
                               date: "2026-04-05", timestamp: 1743868800, tags: "inbox")
        let msg2 = MailMessage(threadId: "t2", subject: "Second", from: "c@d.com",
                               date: "2026-04-05", timestamp: 1743868801, tags: "inbox")
        let msg3 = MailMessage(threadId: "t3", subject: "Third", from: "e@f.com",
                               date: "2026-04-05", timestamp: 1743868802, tags: "inbox")

        manager.mailMessages = [msg1, msg2, msg3]
        let genBefore = manager.emailListGeneration

        manager.removeLocally(ids: Set(["t1", "t3"]))

        XCTAssertEqual(manager.mailMessages.count, 1)
        XCTAssertEqual(manager.mailMessages.first?.id, "t2")
        XCTAssertGreaterThan(manager.emailListGeneration, genBefore)
    }

    func testRemoveLocallyEmptySet() {
        let manager = AccountManager.shared

        let msg = MailMessage(threadId: "t1", subject: "Test", from: "a@b.com",
                              date: "2026-04-05", timestamp: 1743868800, tags: "inbox")
        manager.mailMessages = [msg]

        manager.removeLocally(ids: Set())

        XCTAssertEqual(manager.mailMessages.count, 1)
    }

    func testRemoveLocallyNonexistentIds() {
        let manager = AccountManager.shared

        let msg = MailMessage(threadId: "t1", subject: "Test", from: "a@b.com",
                              date: "2026-04-05", timestamp: 1743868800, tags: "inbox")
        manager.mailMessages = [msg]

        manager.removeLocally(ids: Set(["nonexistent"]))

        XCTAssertEqual(manager.mailMessages.count, 1)
    }

    // MARK: - mailFolders

    func testMailFoldersReturnsDefaultFolders() {
        let manager = AccountManager.shared
        let folders = manager.mailFolders

        // Should have at least inbox
        let names = folders.map { $0.name }
        XCTAssertTrue(names.contains("inbox"))
    }
}
