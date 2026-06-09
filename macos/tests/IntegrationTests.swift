@testable import durian_lib
import XCTest

/// Integration tests that hit a real durian server.
/// The server URL is passed via DURIAN_TEST_URL environment variable.
/// These tests validate that the Go backend produces JSON that Swift can decode.
@MainActor
final class IntegrationTests: XCTestCase {

    private var baseURL: URL!

    override func setUp() async throws {
        guard let urlString = ProcessInfo.processInfo.environment["DURIAN_TEST_URL"] else {
            throw XCTSkip("DURIAN_TEST_URL not set — skipping integration tests")
        }
        guard let url = URL(string: urlString) else {
            throw XCTSkip("DURIAN_TEST_URL is not a valid URL: \(urlString)")
        }
        baseURL = url
    }

    // MARK: - Helpers

    private func get(_ path: String) async throws -> (Data, HTTPURLResponse) {
        let url = baseURL.appendingPathComponent(path)
        let (data, response) = try await URLSession.shared.data(from: url)
        let httpResp = try XCTUnwrap(response as? HTTPURLResponse)
        return (data, httpResp)
    }

    // MARK: - Search

    func testSearchReturnsDecodableResults() async throws {
        let url = baseURL.appendingPathComponent("/search")
        var components = URLComponents(url: url, resolvingAgainstBaseURL: false)!
        components.queryItems = [
            URLQueryItem(name: "query", value: "tag:inbox"),
            URLQueryItem(name: "limit", value: "10"),
        ]

        let (data, httpResp) = try await URLSession.shared.data(from: components.url!)
        XCTAssertEqual(httpResp.statusCode, 200)

        // This is THE contract test: can Swift decode what Go produces?
        let response = try JSONDecoder().decode(DurianResponse.self, from: data)
        XCTAssertTrue(response.ok)
        XCTAssertNotNil(response.results)
        XCTAssertGreaterThan(response.results?.count ?? 0, 0)

        // Verify search result fields
        if let first = response.results?.first {
            XCTAssertFalse(first.thread_id.isEmpty)
            XCTAssertFalse(first.subject.isEmpty)
            XCTAssertFalse(first.from.isEmpty)
            XCTAssertFalse(first.tags.isEmpty)
        }
    }

    func testSearchCountReturnsInt() async throws {
        let url = baseURL.appendingPathComponent("/search/count")
        var components = URLComponents(url: url, resolvingAgainstBaseURL: false)!
        components.queryItems = [URLQueryItem(name: "query", value: "tag:inbox")]

        let (data, httpResp) = try await URLSession.shared.data(from: components.url!)
        XCTAssertEqual(httpResp.statusCode, 200)

        struct CountResponse: Decodable { let count: Int }
        let response = try JSONDecoder().decode(CountResponse.self, from: data)
        XCTAssertGreaterThan(response.count, 0)
    }

    // MARK: - Tags

    func testListTagsDecodable() async throws {
        let (data, httpResp) = try await get("/tags")
        XCTAssertEqual(httpResp.statusCode, 200)

        let response = try JSONDecoder().decode(DurianResponse.self, from: data)
        XCTAssertTrue(response.ok)
        XCTAssertNotNil(response.tags)
        XCTAssertTrue(response.tags?.contains("inbox") ?? false)
    }

    // MARK: - Threads

    func testShowThreadDecodable() async throws {
        // First search to get a thread_id
        let searchURL = baseURL.appendingPathComponent("/search")
        var components = URLComponents(url: searchURL, resolvingAgainstBaseURL: false)!
        components.queryItems = [
            URLQueryItem(name: "query", value: "tag:inbox"),
            URLQueryItem(name: "limit", value: "1"),
        ]
        let (searchData, _) = try await URLSession.shared.data(from: components.url!)
        let searchResp = try JSONDecoder().decode(DurianResponse.self, from: searchData)
        let threadId = try XCTUnwrap(searchResp.results?.first?.thread_id)

        // Now fetch the thread
        let (data, httpResp) = try await get("/threads/\(threadId)")
        XCTAssertEqual(httpResp.statusCode, 200)

        let response = try JSONDecoder().decode(DurianResponse.self, from: data)
        XCTAssertTrue(response.ok)

        let thread = try XCTUnwrap(response.thread)
        XCTAssertEqual(thread.thread_id, threadId)
        XCTAssertFalse(thread.subject.isEmpty)
        XCTAssertGreaterThan(thread.messages.count, 0)

        // Verify ThreadMessage fields
        let msg = thread.messages[0]
        XCTAssertFalse(msg.id.isEmpty)
        XCTAssertFalse(msg.from.isEmpty)
        XCTAssertFalse(msg.body.isEmpty)
    }

    // MARK: - Outbox

    func testListOutboxDecodable() async throws {
        let (data, httpResp) = try await get("/outbox")
        XCTAssertEqual(httpResp.statusCode, 200)

        // Outbox returns an array directly (not wrapped in DurianResponse)
        let entries = try JSONDecoder().decode([OutboxEntry].self, from: data)
        // Empty is fine — we just need to verify the type decodes
        XCTAssertNotNil(entries)
    }

    // MARK: - Local Drafts

    func testListLocalDraftsDecodable() async throws {
        let (data, httpResp) = try await get("/local-drafts")
        XCTAssertEqual(httpResp.statusCode, 200)

        struct LocalDraft: Decodable {
            let id: String
            let draft_json: String
            let updated_at: Int64
        }
        let drafts = try JSONDecoder().decode([LocalDraft].self, from: data)
        XCTAssertNotNil(drafts)
    }

    // MARK: - Version

    func testVersionEndpoint() async throws {
        let (data, httpResp) = try await get("/version")
        XCTAssertEqual(httpResp.statusCode, 200)

        struct VersionResponse: Decodable {
            let version: String
            let commit: String
        }
        let response = try JSONDecoder().decode(VersionResponse.self, from: data)
        XCTAssertFalse(response.version.isEmpty)
    }
}
