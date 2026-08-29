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

    // MARK: - Thread application

    /// applyThread is the only place a decoded ThreadMessage becomes the
    /// MailMessage the compose window later reads. A field missing from this
    /// projection decodes fine and reaches the factory as nil, so neither the
    /// decoding nor the factory test would notice.
    @MainActor
    func testApplyThreadCarriesBccToMailMessage() throws {
        let json = """
        {
            "thread_id": "thread-1",
            "subject": "Quarterly numbers",
            "messages": [{
                "id": "draft@example.com",
                "from": "author@example.com",
                "to": "to@example.com",
                "cc": "cc@example.com",
                "bcc": "blind@example.com",
                "date": "Mon, 01 Jan 2024 00:00:00 +0000",
                "timestamp": 1704067200,
                "body": "body"
            }]
        }
        """.data(using: .utf8)!

        let thread = try JSONDecoder().decode(ThreadContent.self, from: json)
        var message = makeDraftMessage()
        message.bcc = nil

        EmailBackend().applyThread(thread, to: &message)

        XCTAssertEqual(message.bcc, "blind@example.com")
        XCTAssertEqual(message.cc, "cc@example.com")
    }

    /// The path that actually breaks in daily use. A draft is loaded once, a
    /// search or auto-sync then rebuilds the MailMessage list, and rehydration
    /// restores the loaded state from the cache. That restored state is what
    /// "Edit Draft" reads. Rehydration projects a different message than
    /// applyThread does (last rather than newest), so it needs its own test —
    /// the applyThread test never reaches this code.
    /// The thread has two messages, which is the case that distinguishes a
    /// correct projection from an accidental one: with a single message,
    /// `.first` and `.last` are the same element and any choice passes.
    ///
    /// The API sorts newest first, so the draft is `.first` and the mail it
    /// replies to is `.last`. Projecting the wrong end restores the
    /// originating message — whose Bcc is absent, because a received message
    /// never has one.
    @MainActor
    func testCacheRehydrationRestoresBccFromTheNewestMessage() throws {
        let backend = EmailBackend()
        var email = makeDraftMessage()
        // A freshly built list entry: not yet loaded, so rehydration applies.
        email.threadMessages = nil
        email.bcc = nil
        backend.emails = [email]
        backend.threadCache[email.id] = EmailBackend.CachedThread(
            messages: [
                try decodeThreadMessage(id: "draft@example.com", bcc: "blind@example.com", body: "my draft"),
                try decodeThreadMessage(id: "original@example.com", bcc: nil, body: "the original"),
            ],
            timestamp: Date()
        )

        backend.restoreCachedThreads()

        XCTAssertEqual(backend.emails[0].bcc, "blind@example.com",
                       "a draft edited after a sync must still carry its blind recipients")
        XCTAssertEqual(backend.emails[0].body, "my draft",
                       "rehydration must project the same message the fresh load does")
        XCTAssertEqual(backend.emails[0].messageId, "draft@example.com")
    }

    /// Rehydration must be a no-op on the projected fields: what the user sees
    /// and edits cannot depend on whether a sync happened in between.
    @MainActor
    func testCacheRehydrationMatchesTheFreshLoad() throws {
        let messages = [
            try decodeThreadMessage(id: "draft@example.com", bcc: "blind@example.com", body: "my draft"),
            try decodeThreadMessage(id: "original@example.com", bcc: nil, body: "the original"),
        ]

        var freshlyLoaded = makeDraftMessage()
        EmailBackend().applyThread(
            ThreadContent(thread_id: "thread-1", subject: "Quarterly numbers", messages: messages),
            to: &freshlyLoaded
        )

        let backend = EmailBackend()
        var rehydrated = makeDraftMessage()
        rehydrated.threadMessages = nil
        rehydrated.bcc = nil
        backend.emails = [rehydrated]
        backend.threadCache[rehydrated.id] = EmailBackend.CachedThread(messages: messages, timestamp: Date())
        backend.restoreCachedThreads()

        XCTAssertEqual(backend.emails[0].bcc, freshlyLoaded.bcc)
        XCTAssertEqual(backend.emails[0].to, freshlyLoaded.to)
        XCTAssertEqual(backend.emails[0].cc, freshlyLoaded.cc)
        XCTAssertEqual(backend.emails[0].body, freshlyLoaded.body)
        XCTAssertEqual(backend.emails[0].messageId, freshlyLoaded.messageId)
    }

    /// A draft is not always the newest message in its thread: one reply
    /// arriving after the save is enough. The card the user clicked knows
    /// which message it renders, and that message — not the thread aggregate —
    /// is what the compose window has to open.
    func testCreateFromDraftPrefersTheClickedMessage() throws {
        var aggregate = makeDraftMessage()
        // The aggregate describes the newest message, a reply with no Bcc.
        aggregate.bcc = nil
        aggregate.body = "the reply"
        aggregate.messageId = "reply@example.com"

        let clickedDraft = try decodeThreadMessage(
            id: "draft@example.com", bcc: "blind@example.com", body: "my draft")

        let draft = EmailDraft.createFromDraft(message: aggregate, draftMessage: clickedDraft)

        XCTAssertEqual(draft.bcc, ["blind@example.com"],
                       "editing a draft that is not the newest message must use that draft's recipients")
        XCTAssertEqual(draft.body, "my draft")
        XCTAssertEqual(draft.messageId, "draft@example.com")
    }

    /// Without a named message — the whole-email footer — the aggregate stays
    /// the source, so the existing behaviour is unchanged.
    func testCreateFromDraftFallsBackToTheAggregate() {
        var aggregate = makeDraftMessage()
        aggregate.bcc = "blind@example.com"

        let draft = EmailDraft.createFromDraft(message: aggregate, draftMessage: nil)

        XCTAssertEqual(draft.bcc, ["blind@example.com"])
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

    private func decodeThreadMessage(
        id: String = "draft@example.com",
        bcc: String?,
        body: String = "body"
    ) throws -> ThreadMessage {
        let bccField = bcc.map { "\"bcc\": \"\($0)\"," } ?? ""
        let json = """
        {
            "id": "\(id)",
            "from": "author@example.com",
            "to": "to@example.com",
            "cc": "cc@example.com",
            \(bccField)
            "message_id": "\(id)",
            "date": "Mon, 01 Jan 2024 00:00:00 +0000",
            "timestamp": 1704067200,
            "body": "\(body)"
        }
        """.data(using: .utf8)!
        return try JSONDecoder().decode(ThreadMessage.self, from: json)
    }

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
