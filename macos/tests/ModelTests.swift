@testable import durian_lib
import XCTest

final class ModelTests: XCTestCase {

    // MARK: - MailFolder (tag init)

    func testInboxTagIsSpecial() {
        let folder = MailFolder(tag: "inbox", icon: "tray")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .inbox)
        XCTAssertEqual(folder.displayName, "Inbox")
        XCTAssertEqual(folder.id, "tag:inbox")
    }

    func testSentTagIsSpecial() {
        let folder = MailFolder(tag: "sent", icon: "paperplane")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .sent)
    }

    func testDraftTagIsSpecial() {
        let folder = MailFolder(tag: "draft", icon: "doc.text")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .drafts)
    }

    func testTrashTagIsSpecial() {
        let folder = MailFolder(tag: "deleted", icon: "trash")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .trash)
    }

    func testArchiveTagIsSpecial() {
        let folder = MailFolder(tag: "archive", icon: "archivebox")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .archive)
    }

    func testCustomTagIsNotSpecial() {
        let folder = MailFolder(tag: "newsletter", icon: "newspaper")
        XCTAssertFalse(folder.isSpecial)
        XCTAssertNil(folder.specialType)
        XCTAssertEqual(folder.displayName, "Newsletter")
    }

    // MARK: - MailFolder (name init)

    func testFolderNameInitSpecialDetection() {
        let folder = MailFolder(name: "Inbox", displayName: "Inbox", icon: "tray")
        XCTAssertTrue(folder.isSpecial)
        XCTAssertEqual(folder.specialType, .inbox)
        XCTAssertEqual(folder.id, "folder:Inbox")
    }

    func testFolderNameInitCustom() {
        let folder = MailFolder(name: "Projects", displayName: "Projects", icon: "folder")
        XCTAssertFalse(folder.isSpecial)
        XCTAssertNil(folder.specialType)
    }

    // MARK: - MailMessage

    func testMailMessageUnreadAndFlagged() {
        let msg = MailMessage(
            threadId: "t1",
            subject: "Test",
            from: "alice@example.com",
            date: "2025-01-01",
            timestamp: 1735689600,
            tags: "unread,flagged"
        )
        XCTAssertFalse(msg.isRead)
        XCTAssertTrue(msg.isPinned)
        XCTAssertFalse(msg.hasAttachment)
    }

    func testMailMessageReadNoFlags() {
        let msg = MailMessage(
            threadId: "t2",
            subject: "Read message",
            from: "bob@example.com",
            date: "2025-01-02",
            timestamp: 1735776000,
            tags: "inbox"
        )
        XCTAssertTrue(msg.isRead)
        XCTAssertFalse(msg.isPinned)
    }

    func testMailMessageWithAttachment() {
        let msg = MailMessage(
            threadId: "t3",
            subject: "With file",
            from: "carol@example.com",
            date: "2025-01-03",
            timestamp: 1735862400,
            tags: "inbox,attachment"
        )
        XCTAssertTrue(msg.hasAttachment)
    }

    func testMailMessageIsDraft() {
        let msg = MailMessage(
            threadId: "t4",
            subject: "Draft",
            from: "me@example.com",
            date: "2025-01-04",
            timestamp: 1735948800,
            tags: "draft,inbox"
        )
        XCTAssertTrue(msg.isDraft)
    }

    func testMailMessageIsNotDraft() {
        let msg = MailMessage(
            threadId: "t5",
            subject: "Not a draft",
            from: "me@example.com",
            date: "2025-01-05",
            timestamp: 1736035200,
            tags: "inbox,sent"
        )
        XCTAssertFalse(msg.isDraft)
    }

    func testMailMessageNilTagsNotDraft() {
        var msg = MailMessage(
            threadId: "t6",
            subject: "No tags",
            from: "me@example.com",
            date: "2025-01-06",
            timestamp: 1736121600,
            tags: "inbox"
        )
        msg.tags = nil
        XCTAssertFalse(msg.isDraft)
    }

    // MARK: - EmailBodyState

    func testDisplayBodyNotLoaded() {
        let state = EmailBodyState.notLoaded
        XCTAssertEqual(state.displayBody, "Tap to load email content")
    }

    func testDisplayBodyLoading() {
        let state = EmailBodyState.loading
        XCTAssertEqual(state.displayBody, "Loading...")
    }

    func testDisplayBodyLoaded() {
        let state = EmailBodyState.loaded(body: "Hello world", attributedBody: nil)
        XCTAssertEqual(state.displayBody, "Hello world")
    }

    func testDisplayBodyLoadedEmpty() {
        let state = EmailBodyState.loaded(body: "", attributedBody: nil)
        XCTAssertEqual(state.displayBody, "No content available")
    }

    func testDisplayBodyFailed() {
        let state = EmailBodyState.failed(message: "Network error")
        XCTAssertEqual(state.displayBody, "Failed to load: Network error")
    }

    func testEmailBodyStateEquality() {
        XCTAssertEqual(EmailBodyState.notLoaded, EmailBodyState.notLoaded)
        XCTAssertEqual(EmailBodyState.loading, EmailBodyState.loading)
        XCTAssertEqual(
            EmailBodyState.loaded(body: "x", attributedBody: nil),
            EmailBodyState.loaded(body: "x", attributedBody: nil)
        )
        XCTAssertNotEqual(EmailBodyState.notLoaded, EmailBodyState.loading)
    }
}
