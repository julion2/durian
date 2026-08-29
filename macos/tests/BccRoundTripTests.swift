@testable import durian_lib
import XCTest

/// The GUI half of the Bcc persistence contract.
///
/// Blind recipients survive only inside our own Drafts mailbox: the sending
/// server strips the header on delivery, which is what makes the copy blind.
/// So every layer that drops the field destroys it for good. The CLI now
/// parses, stores and serves it; these tests pin the three places on the Swift
/// side that have to carry it from the JSON response to the next save.
final class BccRoundTripTests: XCTestCase {

    // MARK: - Decoding

    /// The API sends `bcc` on the full thread view. A `ThreadMessage` without
    /// the property decodes successfully and silently discards the value —
    /// there is no decoding error to notice, which is why this needs a test.
    func testThreadMessageDecodesBcc() throws {
        let json = """
        {
            "id": "draft@example.com",
            "from": "author@example.com",
            "to": "to@example.com",
            "cc": "cc@example.com",
            "bcc": "blind@example.com",
            "date": "Mon, 01 Jan 2024 00:00:00 +0000",
            "timestamp": 1704067200,
            "body": "body"
        }
        """.data(using: .utf8)!

        let message = try JSONDecoder().decode(ThreadMessage.self, from: json)
        XCTAssertEqual(message.bcc, "blind@example.com")
    }

    /// A received message carries no Bcc, and the light view omits it. Absence
    /// must stay decodable rather than failing the whole thread.
    func testThreadMessageDecodesWithoutBcc() throws {
        let json = """
        {
            "id": "received@example.com",
            "from": "sender@example.com",
            "to": "me@example.com",
            "date": "Mon, 01 Jan 2024 00:00:00 +0000",
            "timestamp": 1704067200,
            "body": "body"
        }
        """.data(using: .utf8)!

        let message = try JSONDecoder().decode(ThreadMessage.self, from: json)
        XCTAssertNil(message.bcc)
    }

    // MARK: - Draft projection

    /// Reopening a draft for editing must restore the blind recipients, or the
    /// next save mails the message to fewer people than the user addressed.
    func testCreateFromDraftCarriesBcc() {
        var message = makeDraftMessage()
        message.bcc = "blind-one@example.com, blind-two@example.com"

        let draft = EmailDraft.createFromDraft(message: message)

        XCTAssertEqual(draft.bcc, ["blind-one@example.com", "blind-two@example.com"])
        // The visible recipients must not be disturbed by the new field.
        XCTAssertEqual(draft.to, ["to@example.com"])
        XCTAssertEqual(draft.cc, ["cc@example.com"])
    }

    /// A draft that never had blind recipients yields an empty list, so
    /// DraftService omits `--bcc` entirely rather than passing an empty value.
    func testCreateFromDraftWithoutBccIsEmpty() {
        let draft = EmailDraft.createFromDraft(message: makeDraftMessage())
        XCTAssertTrue(draft.bcc.isEmpty)
    }

    // MARK: - Helpers

    private func makeDraftMessage() -> MailMessage {
        var message = MailMessage(
            threadId: "thread-1",
            subject: "Quarterly numbers",
            from: "author@example.com",
            to: "to@example.com",
            date: "Mon, 01 Jan 2024 00:00:00 +0000",
            timestamp: 1_704_067_200,
            tags: "draft"
        )
        message.cc = "cc@example.com"
        message.body = "body"
        return message
    }
}
