import AppKit
@testable import durian_lib
import XCTest

private final class StartupURLProtocol: URLProtocol {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var storedRequests: [URLRequest] = []

    static var requests: [URLRequest] {
        lock.withLock { storedRequests }
    }

    static func reset() {
        lock.withLock { storedRequests.removeAll() }
    }

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest {
        request
    }

    override func startLoading() {
        Self.lock.withLock { Self.storedRequests.append(request) }

        let path = request.url?.path ?? ""
        let data: Data
        if path == "/api/v1/search/count" {
            data = Data(#"{"count":0}"#.utf8)
        } else {
            data = Data(#"{"ok":true,"results":[]}"#.utf8)
        }
        let response = HTTPURLResponse(
            url: request.url!,
            statusCode: 200,
            httpVersion: "HTTP/1.1",
            headerFields: ["Content-Type": "application/json"]
        )!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: data)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

@MainActor
final class AccountManagerTests: XCTestCase {

    private func makeStartupSession() -> URLSession {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [StartupURLProtocol.self]
        return URLSession(configuration: configuration)
    }

    private func makeWorkProfileManager() -> ProfileManager {
        let profile = Profile(
            name: "Work",
            accounts: ["work"],
            isDefault: true,
            color: nil,
            folders: [
                FolderConfig(name: "Focus", icon: "tray", query: "tag:priority", isSection: false)
            ]
        )
        return ProfileManager(profiles: [profile], currentProfile: profile)
    }

    private func searchQuery(from request: URLRequest) -> String? {
        guard let url = request.url else { return nil }
        return URLComponents(url: url, resolvingAgainstBaseURL: false)?
            .queryItems?
            .first(where: { $0.name == "query" })?
            .value
    }

    // MARK: - Initial Connection

    func testBackendConnectionDoesNotInitiateSearch() async {
        StartupURLProtocol.reset()
        let session = makeStartupSession()
        defer { session.invalidateAndCancel() }
        let backend = EmailBackend(
            session: session,
            serverConnector: { "test-token" },
            profileManager: makeWorkProfileManager()
        )

        await backend.connect()

        XCTAssertTrue(backend.isConnected)
        XCTAssertEqual(backend.connectionStatus, "Connected")
        XCTAssertTrue(StartupURLProtocol.requests.isEmpty)
    }

    func testConcurrentStartupConnectionPerformsOneConfiguredFolderSearch() async {
        _ = NSApplication.shared
        StartupURLProtocol.reset()
        let session = makeStartupSession()
        defer { session.invalidateAndCancel() }
        let connectionCount = StartupCounter()
        let startupCompletions = StartupCompletions()
        let profileManager = makeWorkProfileManager()
        let backend = EmailBackend(
            session: session,
            serverConnector: {
                connectionCount.increment()
                try await Task.sleep(for: .milliseconds(25))
                return "test-token"
            },
            profileManager: profileManager
        )
        let manager = AccountManager(
            emailBackend: backend,
            profileManager: profileManager,
            startupCompletion: { startupCompletions.append($0) }
        )

        async let firstConnection = manager.connectToAllAccounts()
        async let secondConnection = manager.connectToAllAccounts()
        let results = await (firstConnection, secondConnection)

        let requests = StartupURLProtocol.requests
        let searchRequests = requests.filter { $0.url?.path == "/api/v1/search" }
        XCTAssertTrue(results.0)
        XCTAssertTrue(results.1)
        XCTAssertEqual(connectionCount.value, 1)
        XCTAssertEqual(startupCompletions.values, [true])
        XCTAssertEqual(searchRequests.count, 1)
        XCTAssertEqual(
            searchRequests.first.flatMap(searchQuery),
            "(tag:priority) AND (path:work/**)"
        )
        XCTAssertEqual(requests.first?.url?.path, "/api/v1/search")
        XCTAssertEqual(requests.filter { $0.url?.path == "/api/v1/search/count" }.count, 1)
        XCTAssertEqual(manager.selectedFolder, "focus")
    }

    func testFailedStartupConnectionDoesNotSearch() async {
        StartupURLProtocol.reset()
        let session = makeStartupSession()
        defer { session.invalidateAndCancel() }
        let profileManager = makeWorkProfileManager()
        let startupCompletions = StartupCompletions()
        let backend = EmailBackend(session: session, serverConnector: {
            throw NSError(
                domain: "AccountManagerTests",
                code: 1,
                userInfo: [NSLocalizedDescriptionKey: "server failed"]
            )
        }, profileManager: profileManager)
        let manager = AccountManager(
            emailBackend: backend,
            profileManager: profileManager,
            startupCompletion: { startupCompletions.append($0) }
        )

        await manager.connectToAllAccounts()

        XCTAssertFalse(backend.isConnected)
        XCTAssertTrue(backend.connectionStatus.contains("server failed"))
        XCTAssertTrue(StartupURLProtocol.requests.isEmpty)
        XCTAssertEqual(manager.selectedFolder, "inbox")
        XCTAssertEqual(startupCompletions.values, [false])
    }

    func testUnexpectedServeDeathClearsConnectionAndReconnectsWithOneNewSearch() async throws {
        _ = NSApplication.shared
        StartupURLProtocol.reset()
        let session = makeStartupSession()
        defer { session.invalidateAndCancel() }
        let profileManager = makeWorkProfileManager()
        let processes = StartupProcesses()
        let startupCompletions = StartupCompletions()
        defer { processes.terminateAll() }
        let backend = EmailBackend(
            session: session,
            serverProcessFactory: { _ in
                let process = Process()
                process.executableURL = URL(fileURLWithPath: "/bin/sh")
                process.arguments = [
                    "-c",
                    "printf 'READY token=test-token addr=127.0.0.1:9723 api=\(AppServer.apiProtocol)\\n'; exec /bin/sleep 30",
                ]
                processes.append(process)
                return process
            },
            profileManager: profileManager
        )
        let manager = AccountManager(
            emailBackend: backend,
            profileManager: profileManager,
            startupCompletion: { startupCompletions.append($0) }
        )

        let firstConnection = await manager.connectToAllAccounts()
        XCTAssertTrue(firstConnection)
        let firstProcess = try XCTUnwrap(processes.values.first)
        firstProcess.terminate()
        let disconnected = await waitUntil { !backend.isConnected }
        XCTAssertTrue(disconnected)
        XCTAssertTrue(backend.connectionStatus.contains("server exited"))

        let secondConnection = await manager.connectToAllAccounts()
        XCTAssertTrue(secondConnection)

        XCTAssertTrue(backend.isConnected)
        XCTAssertEqual(processes.values.count, 2)
        XCTAssertEqual(
            StartupURLProtocol.requests.filter { $0.url?.path == "/api/v1/search" }.count,
            2
        )
        XCTAssertEqual(startupCompletions.values, [true])
        await backend.disconnect()
    }

    private func waitUntil(_ condition: @MainActor () -> Bool) async -> Bool {
        for _ in 0 ..< 100 {
            if condition() { return true }
            try? await Task.sleep(for: .milliseconds(10))
        }
        return false
    }

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

private final class StartupCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0

    var value: Int { lock.withLock { count } }

    func increment() {
        lock.withLock { count += 1 }
    }
}

private final class StartupProcesses: @unchecked Sendable {
    private let lock = NSLock()
    private var processes: [Process] = []

    var values: [Process] { lock.withLock { processes } }

    func append(_ process: Process) {
        lock.withLock { processes.append(process) }
    }

    func terminateAll() {
        lock.withLock {
            processes.filter(\.isRunning).forEach { $0.terminate() }
        }
    }
}

private final class StartupCompletions: @unchecked Sendable {
    private let lock = NSLock()
    private var completions: [Bool] = []

    var values: [Bool] { lock.withLock { completions } }

    func append(_ success: Bool) {
        lock.withLock { completions.append(success) }
    }
}
