@testable import durian_lib
import XCTest

@MainActor
private class MockSearchBackend: SearchBackend {
    var searchResults: [MailMessage] = []
    var lastQuery: String?
    var lastLimit: Int?

    func searchAll(query: String, limit: Int) async -> [MailMessage] {
        lastQuery = query
        lastLimit = limit
        return searchResults
    }
}

@MainActor
final class SearchManagerTests: XCTestCase {

    private func makeMessage(id: String, subject: String) -> MailMessage {
        MailMessage(threadId: id, subject: subject, from: "test@example.com",
                    date: "2026-04-05", timestamp: 1743868800, tags: "inbox")
    }

    private func makeManager(results: [MailMessage] = []) -> (SearchManager, MockSearchBackend) {
        let mock = MockSearchBackend()
        mock.searchResults = results
        let manager = SearchManager(
            backend: { mock },
            profileFilter: { $0 }  // identity — no filtering
        )
        return (manager, mock)
    }

    // MARK: - Tests

    func testSearchEmptyQueryClearsResults() {
        let (manager, _) = makeManager()
        manager.search(query: "")
        XCTAssertTrue(manager.results.isEmpty)
        XCTAssertFalse(manager.isSearching)
    }

    func testSearchWhitespaceClearsResults() {
        let (manager, _) = makeManager()
        manager.search(query: "   ")
        XCTAssertTrue(manager.results.isEmpty)
        XCTAssertFalse(manager.isSearching)
    }

    func testSearchSetsIsSearching() {
        let (manager, _) = makeManager()
        manager.search(query: "test")
        // isSearching should be true immediately (before debounce completes)
        XCTAssertTrue(manager.isSearching)
    }

    func testSearchReturnsResults() async {
        let msg = makeMessage(id: "t1", subject: "Meeting Notes")
        let (manager, mock) = makeManager(results: [msg])

        manager.search(query: "meeting")
        try? await Task.sleep(nanoseconds: 500_000_000)

        XCTAssertEqual(manager.results.count, 1)
        XCTAssertEqual(manager.results.first?.subject, "Meeting Notes")
        XCTAssertEqual(mock.lastQuery, "meeting")
        XCTAssertFalse(manager.isSearching)
    }

    func testSearchCancellation() async {
        let msg1 = makeMessage(id: "t1", subject: "First")
        let msg2 = makeMessage(id: "t2", subject: "Second")
        let (manager, mock) = makeManager(results: [msg2])

        // Fire two searches rapidly — only the second should win
        mock.searchResults = [msg1]
        manager.search(query: "first")
        mock.searchResults = [msg2]
        manager.search(query: "second")

        // Wait for debounce + execution
        try? await Task.sleep(nanoseconds: 500_000_000)

        XCTAssertEqual(mock.lastQuery, "second")
        XCTAssertEqual(manager.results.count, 1)
        XCTAssertEqual(manager.results.first?.subject, "Second")
    }

    func testClearResetsState() async {
        let msg = makeMessage(id: "t1", subject: "Test")
        let (manager, _) = makeManager(results: [msg])

        manager.search(query: "test")
        try? await Task.sleep(nanoseconds: 500_000_000)
        XCTAssertFalse(manager.results.isEmpty)

        manager.clear()
        try? await Task.sleep(nanoseconds: 200_000_000)
        XCTAssertTrue(manager.results.isEmpty)
        XCTAssertFalse(manager.isSearching)
    }
}
