@testable import durian_lib
import XCTest

final class EmailDraftFactoryTests: XCTestCase {

    private struct FixtureThread: Decodable {
        let messages: [ThreadMessage]
    }

    func testReplyToMixedThreadTargetsMostRecentNonSelfMessage() throws {
        let message = try makeMessage(
            subject: "Release plan",
            aggregateFrom: "Me <me@example.com>",
            messages: [
                threadMessage(
                    id: "newest-self",
                    from: "Me <me@example.com>",
                    to: "Alice <alice@example.com>",
                    body: "My latest answer",
                    messageId: "<self@example.com>"
                ),
                threadMessage(
                    id: "older-alice",
                    from: "Alice <alice@example.com>",
                    to: "Me <me@example.com>",
                    body: "Alice's question",
                    messageId: "<alice@example.com>",
                    references: "<root@example.com>"
                ),
            ]
        )

        let draft = EmailDraft.createReply(from: message, fromAccount: "me@example.com")

        XCTAssertEqual(draft.to, ["Alice <alice@example.com>"])
        XCTAssertEqual(draft.inReplyTo, "<alice@example.com>")
        XCTAssertEqual(draft.references, "<root@example.com> <alice@example.com>")
        XCTAssertEqual(draft.quotedContent, "On Tue, 02 Jan 2024 00:00:00 +0000, Alice <alice@example.com> wrote:\n> Alice's question\n")
        XCTAssertEqual(EmailDraft.replyTargetMessageId(for: message, fromAccount: "me@example.com"), "older-alice")
    }

    func testReplyAllUsesTargetMessageRecipientsInsteadOfNewestSelfMessage() throws {
        let message = try makeMessage(
            subject: "Release plan",
            aggregateFrom: "Me <me@example.com>",
            messages: [
                threadMessage(
                    id: "newest-self",
                    from: "Me <me@example.com>",
                    to: "Wrong Recipient <wrong@example.com>",
                    body: "My latest answer",
                    messageId: "<self@example.com>"
                ),
                threadMessage(
                    id: "older-alice",
                    from: "Alice <alice@example.com>",
                    to: "Me <me@example.com>, Bob <bob@example.com>",
                    cc: "Carol <carol@example.com>",
                    body: "Alice's question",
                    messageId: "<alice@example.com>"
                ),
            ]
        )

        let draft = EmailDraft.createReplyAll(from: message, fromAccount: "me@example.com")

        XCTAssertEqual(draft.to, ["Alice <alice@example.com>"])
        XCTAssertEqual(draft.cc, ["Bob <bob@example.com>", "Carol <carol@example.com>"])
        XCTAssertFalse(draft.cc.contains(where: { $0.contains("wrong@example.com") }))
    }

    func testReplyQuotesFetchedFullHTMLInsteadOfStrippedThreadBody() throws {
        let message = try makeMessage(
            subject: "Release plan",
            aggregateFrom: "Alice <alice@example.com>",
            messages: [
                threadMessage(
                    id: "newest-alice",
                    from: "Alice <alice@example.com>",
                    to: "Me <me@example.com>",
                    body: "stripped body",
                    html: "<p>stripped body</p>",
                    messageId: "<alice@example.com>"
                ),
            ]
        )

        let draft = EmailDraft.createReply(
            from: message,
            fromAccount: "me@example.com",
            originalBody: (body: "full text fallback", html: "<p>full <strong>HTML</strong></p>")
        )

        XCTAssertTrue(draft.quotedIsHTML)
        XCTAssertTrue(draft.quotedContent?.contains("<p>full <strong>HTML</strong></p>") == true)
        XCTAssertFalse(draft.quotedContent?.contains("stripped body") == true)
    }

    // MARK: - Fixtures

    private func makeMessage(
        subject: String,
        aggregateFrom: String,
        messages: [String]
    ) throws -> MailMessage {
        let threadJSON = """
        {
          "thread_id": "thread-1",
          "subject": \(jsonString(subject)),
          "messages": [\(messages.joined(separator: ","))]
        }
        """
        let thread = try JSONDecoder().decode(FixtureThread.self, from: Data(threadJSON.utf8))
        var message = MailMessage(
            threadId: "thread-1",
            subject: subject,
            from: aggregateFrom,
            to: "Alice <alice@example.com>",
            date: "Wed, 03 Jan 2024 00:00:00 +0000",
            timestamp: 1_704_240_000,
            tags: "inbox"
        )
        message.threadMessages = thread.messages
        message.body = thread.messages.first?.body
        message.htmlBody = thread.messages.first?.html
        message.messageId = thread.messages.first?.message_id
        message.references = thread.messages.first?.references
        message.bodyState = .loaded(body: message.body ?? "", attributedBody: nil)
        return message
    }

    private func threadMessage(
        id: String,
        from: String,
        to: String,
        cc: String? = nil,
        body: String,
        html: String? = nil,
        messageId: String,
        references: String? = nil
    ) -> String {
        var fields = [
            "\"id\":\(jsonString(id))",
            "\"from\":\(jsonString(from))",
            "\"to\":\(jsonString(to))",
            "\"date\":\(jsonString(id == "newest-self" ? "Wed, 03 Jan 2024 00:00:00 +0000" : "Tue, 02 Jan 2024 00:00:00 +0000"))",
            "\"timestamp\":1704240000",
            "\"body\":\(jsonString(body))",
            "\"message_id\":\(jsonString(messageId))",
        ]
        if let cc { fields.append("\"cc\":\(jsonString(cc))") }
        if let html { fields.append("\"html\":\(jsonString(html))") }
        if let references { fields.append("\"references\":\(jsonString(references))") }
        return "{\(fields.joined(separator: ","))}"
    }

    private func jsonString(_ value: String) -> String {
        let data = try! JSONSerialization.data(withJSONObject: value, options: .fragmentsAllowed)
        return String(decoding: data, as: UTF8.self)
    }
}
