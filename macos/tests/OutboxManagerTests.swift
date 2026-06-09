@testable import durian_lib
import XCTest

@MainActor
private class MockOutboxBackend: OutboxBackend {
    var outboxItems: [OutboxEntry] = []

    func listOutbox() async -> [OutboxEntry] {
        outboxItems
    }
}

@MainActor
final class OutboxManagerTests: XCTestCase {

    private func makeEntry(id: Int64, subject: String) -> OutboxEntry {
        OutboxEntry(id: id, subject: subject, to: "test@example.com",
                    attempts: 0, last_error: nil, created_at: 1743868800)
    }

    func testRefreshUpdatesPendingCount() async {
        let mock = MockOutboxBackend()
        mock.outboxItems = [
            makeEntry(id: 1, subject: "Email 1"),
            makeEntry(id: 2, subject: "Email 2"),
            makeEntry(id: 3, subject: "Email 3"),
        ]
        let manager = OutboxManager(backend: { mock })

        manager.refresh()
        try? await Task.sleep(nanoseconds: 200_000_000)

        XCTAssertEqual(manager.pendingCount, 3)
    }

    func testRefreshEmptyOutbox() async {
        let mock = MockOutboxBackend()
        mock.outboxItems = []
        let manager = OutboxManager(backend: { mock })

        manager.refresh()
        try? await Task.sleep(nanoseconds: 200_000_000)

        XCTAssertEqual(manager.pendingCount, 0)
    }

    func testRefreshNoBackend() async {
        let manager = OutboxManager(backend: { nil })

        manager.refresh()
        try? await Task.sleep(nanoseconds: 200_000_000)

        XCTAssertEqual(manager.pendingCount, 0)
    }
}
