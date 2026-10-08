@testable import durian_lib
import XCTest

/// Regression tests for compose defects reproduced during stabilization.
final class EmailDraftFactoryDefectReproductionTests: XCTestCase {

    private struct FixtureThread: Decodable {
        let messages: [ThreadMessage]
    }

    func testExplicitOlderCardKeepsItsSubjectRecipientsAndReferences() throws {
        let message = try makeMessage(subject: "FW: Different newest subject", messages: [
            threadMessage(id: "newest-bob", from: "Bob <bob@example.com>", body: "Newest body", messageId: "<bob@example.com>"),
            threadMessage(id: "older-alice", from: "Alice <alice@example.com>",
                          to: "me@example.com, Carol <carol@example.com>",
                          body: "Older question", messageId: "<alice@example.com>", subject: "Original subject"),
        ])
        let older = try XCTUnwrap(message.threadMessages?.last)
        let source = message.selectingMessage(older)
        let reply = EmailDraft.createReplyAll(from: source, fromAccount: "me@example.com")
        XCTAssertEqual(EmailDraft.replyTargetMessageId(for: source, fromAccount: "me@example.com"), "older-alice")
        XCTAssertEqual(reply.subject, "Re: Original subject")
        XCTAssertEqual(reply.to, ["Alice <alice@example.com>"])
        XCTAssertEqual(reply.cc, ["Carol <carol@example.com>"])
        XCTAssertEqual(reply.inReplyTo, "<alice@example.com>")
        XCTAssertEqual(reply.references, "<alice@example.com>")
        XCTAssertEqual(reply.replyThreadId, message.id)
        XCTAssertTrue(reply.quotedContent?.contains("Older question") == true)
        XCTAssertFalse(reply.quotedContent?.contains("Newest body") == true)

        let forward = EmailDraft.createForward(from: source, fromAccount: "me@example.com",
                                               originalBody: ("Full older question\n> Nested history", nil))
        XCTAssertEqual(forward.subject, "Fwd: Original subject")
        XCTAssertNil(forward.inReplyTo)
        XCTAssertNil(forward.references)
        XCTAssertTrue(forward.quotedContent?.contains("Nested history") == true)
        XCTAssertFalse(forward.quotedContent?.contains("Newest body") == true)
        XCTAssertEqual(source.threadMessages?.map(\.id), ["older-alice"])
    }

    func testExplicitSelfCardRepliesToItsRecipientsWithoutSwitchingMessage() throws {
        let message = try makeMessage(messages: [
            threadMessage(id: "newest-bob", from: "bob@example.com", body: "Other answer", messageId: "<bob@example.com>"),
            threadMessage(id: "older-self", from: "me@example.com", to: "alice@example.com",
                          body: "My earlier note", messageId: "<self@example.com>", subject: "re: Earlier note"),
        ])
        let source = message.selectingMessage(try XCTUnwrap(message.threadMessages?.last))
        let reply = EmailDraft.createReply(from: source, fromAccount: "me@example.com")
        XCTAssertEqual(reply.to, ["alice@example.com"])
        XCTAssertEqual(reply.inReplyTo, "<self@example.com>")
        XCTAssertEqual(reply.subject, "re: Earlier note")
        XCTAssertTrue(reply.quotedContent?.contains("My earlier note") == true)
    }

    func testReplyAllDeduplicatesRecipientsAcrossToAndCC() throws {
        var message = try makeMessage(messages: [
            threadMessage(
                id: "newest-alice",
                from: "Alice <alice@example.com>",
                to: "Me <me@example.com>, Bob <bob@example.com>, BOB@example.com",
                cc: "Robert <BOB@example.com>, Carol <carol@example.com>, ALICE@example.com, ME@example.com",
                body: "Please review",
                messageId: "<alice@example.com>"
            ),
        ])
        message.to = "Me <me@example.com>, Bob <bob@example.com>, BOB@example.com"
        message.cc = "Robert <BOB@example.com>, Carol <carol@example.com>, ALICE@example.com, ME@example.com"

        let draft = EmailDraft.createReplyAll(from: message, fromAccount: "me@example.com")

        XCTAssertEqual(draft.to, ["Alice <alice@example.com>"])
        XCTAssertEqual(draft.cc, ["Bob <bob@example.com>", "Carol <carol@example.com>"])
    }

    func testFullPlainTextOriginalDoesNotFallBackToStrippedHTML() throws {
        var message = try makeMessage(messages: [
            threadMessage(
                id: "newest-alice",
                from: "Alice <alice@example.com>",
                body: "stripped text",
                html: "<p>stripped HTML</p>",
                messageId: "<alice@example.com>"
            ),
        ])
        message.htmlBody = "<p>stripped HTML</p>"

        let draft = EmailDraft.createReply(
            from: message,
            fromAccount: "me@example.com",
            originalBody: (body: "full original text", html: nil)
        )

        XCTAssertFalse(draft.quotedIsHTML)
        XCTAssertTrue(draft.quotedContent?.contains("> full original text") == true)
        XCTAssertFalse(draft.quotedContent?.contains("stripped HTML") == true)
    }

    func testReplyWithoutFetchedOriginalStillQuotesAvailableHTML() throws {
        var message = try makeMessage(messages: [
            threadMessage(
                id: "newest-alice",
                from: "Alice <alice@example.com>",
                body: "plain fallback",
                html: "<p>available HTML</p>",
                messageId: "<alice@example.com>"
            ),
        ])
        message.htmlBody = "<p>available HTML</p>"

        let draft = EmailDraft.createReply(from: message, fromAccount: "me@example.com")

        XCTAssertTrue(draft.quotedIsHTML)
        XCTAssertTrue(draft.quotedContent?.contains("<p>available HTML</p>") == true)
        XCTAssertFalse(draft.quotedContent?.contains("plain fallback") == true)
    }

    // MARK: - Fixtures

    private func makeMessage(subject: String = "Topic", messages: [String]) throws -> MailMessage {
        let json = """
        {"thread_id":"thread-1","subject":\(jsonString(subject)),"messages":[\(messages.joined(separator: ","))]}
        """
        let thread = try JSONDecoder().decode(FixtureThread.self, from: Data(json.utf8))
        let newest = thread.messages[0]
        var message = MailMessage(
            threadId: "thread-1",
            subject: subject,
            from: newest.from,
            to: newest.to,
            date: newest.date,
            timestamp: newest.timestamp,
            tags: "inbox"
        )
        message.cc = newest.cc
        message.body = newest.body
        message.htmlBody = newest.html
        message.messageId = newest.message_id
        message.references = newest.references
        message.threadMessages = thread.messages
        message.bodyState = .loaded(body: newest.body, attributedBody: nil)
        return message
    }

    private func threadMessage(
        id: String,
        from: String,
        to: String = "Me <me@example.com>",
        cc: String? = nil,
        body: String,
        html: String? = nil,
        messageId: String,
        subject: String? = nil
    ) -> String {
        var fields = [
            "\"id\":\(jsonString(id))",
            "\"from\":\(jsonString(from))",
            "\"to\":\(jsonString(to))",
            "\"date\":\(jsonString("Tue, 02 Jan 2024 00:00:00 +0000"))",
            "\"timestamp\":1704153600",
            "\"body\":\(jsonString(body))",
            "\"message_id\":\(jsonString(messageId))",
        ]
        if let cc { fields.append("\"cc\":\(jsonString(cc))") }
        if let html { fields.append("\"html\":\(jsonString(html))") }
        if let subject { fields.append("\"subject\":\(jsonString(subject))") }
        return "{\(fields.joined(separator: ","))}"
    }

    private func jsonString(_ value: String) -> String {
        let data = try! JSONSerialization.data(withJSONObject: value, options: .fragmentsAllowed)
        return String(decoding: data, as: UTF8.self)
    }
}
