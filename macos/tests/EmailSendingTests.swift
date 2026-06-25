@testable import durian_lib
import XCTest

@MainActor
final class EmailSendingTests: XCTestCase {

    // MARK: - EmailDraft

    func testHasRecipientsWithTo() {
        var draft = EmailDraft(from: "me@example.com")
        draft.to = ["alice@example.com"]
        XCTAssertTrue(draft.hasRecipients)
    }

    func testHasRecipientsWithCCOnly() {
        var draft = EmailDraft(from: "me@example.com")
        draft.cc = ["alice@example.com"]
        XCTAssertTrue(draft.hasRecipients)
    }

    func testHasRecipientsWithBCCOnly() {
        var draft = EmailDraft(from: "me@example.com")
        draft.bcc = ["alice@example.com"]
        XCTAssertTrue(draft.hasRecipients)
    }

    func testHasRecipientsEmpty() {
        let draft = EmailDraft(from: "me@example.com")
        XCTAssertFalse(draft.hasRecipients)
    }

    func testIsValidRequiresRecipientsAndSubject() {
        var draft = EmailDraft(from: "me@example.com")
        XCTAssertFalse(draft.isValid)

        draft.to = ["alice@example.com"]
        XCTAssertFalse(draft.isValid) // still no subject

        draft.subject = "Hello"
        XCTAssertTrue(draft.isValid)
    }

    // MARK: - EmailHelper

    func testIsValidEmail() {
        XCTAssertTrue(EmailHelper.isValidEmail("user@example.com"))
        XCTAssertTrue(EmailHelper.isValidEmail("user@sub.domain.co.uk"))
        XCTAssertTrue(EmailHelper.isValidEmail("Alice <alice@example.com>"))
        XCTAssertFalse(EmailHelper.isValidEmail("not-an-email"))
        XCTAssertFalse(EmailHelper.isValidEmail("user@"))
        XCTAssertFalse(EmailHelper.isValidEmail("user@.com"))
        XCTAssertFalse(EmailHelper.isValidEmail(""))
    }

    func testCleanEmail() {
        XCTAssertEqual(EmailHelper.cleanEmail("alice@example.com"), "alice@example.com")
        XCTAssertEqual(EmailHelper.cleanEmail("Alice Smith <alice@example.com>"), "alice@example.com")
        XCTAssertEqual(EmailHelper.cleanEmail("  alice@example.com  "), "alice@example.com")
    }

    func testValidateRecipientsReturnsInvalid() {
        let invalid = EmailHelper.validateRecipients(["good@example.com", "bad", "also@good.com"])
        XCTAssertEqual(invalid, ["bad"])
    }

    func testValidateRecipientsAllValid() {
        let invalid = EmailHelper.validateRecipients(["a@b.com", "c@d.com"])
        XCTAssertTrue(invalid.isEmpty)
    }

    // MARK: - EmailSendingError

    func testErrorDescriptions() {
        XCTAssertNotNil(EmailSendingError.noSMTPConfiguration.errorDescription)
        XCTAssertNotNil(EmailSendingError.invalidRecipients.errorDescription)
        XCTAssertNotNil(EmailSendingError.sendFailed("timeout").errorDescription)
        XCTAssertNotNil(EmailSendingError.invalidEmailFormat(["bad"]).errorDescription)
        XCTAssertTrue(EmailSendingError.invalidEmailFormat(["bad"]).errorDescription!.contains("bad"))
    }
}
