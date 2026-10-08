@testable import durian_lib
import Foundation
import XCTest

final class NightlyDiagnosticsTests: XCTestCase {
    private var directory: URL!

    override func setUpWithError() throws {
        directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("stability-diagnostics-tests-\(UUID().uuidString)", isDirectory: true)
            .appendingPathComponent("Diagnostics", isDirectory: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: directory.deletingLastPathComponent())
    }

    // MARK: - Helpers

    private let fixedDate = Date(timeIntervalSince1970: 1_700_000_000.75)

    private func makeDiagnostics(
        bundleIdentifier: String? = "org.js-lab.durian.nightly",
        maxFileBytes: Int = NightlyDiagnostics.defaultMaxFileBytes,
        maxPending: Int = NightlyDiagnostics.defaultMaxPending,
        queue: DispatchQueue = DispatchQueue(label: "test.diagnostics")
    ) -> NightlyDiagnostics {
        let date = fixedDate
        return NightlyDiagnostics(
            bundleIdentifier: bundleIdentifier, directory: directory,
            maxFileBytes: maxFileBytes, maxPending: maxPending,
            now: { date }, queue: queue
        )
    }

    private var currentURL: URL { directory.appendingPathComponent(NightlyDiagnostics.currentFileName) }
    private var rotatedURL: URL { directory.appendingPathComponent(NightlyDiagnostics.rotatedFileName) }

    private func lines(_ url: URL) throws -> [String] {
        try String(contentsOf: url, encoding: .utf8).split(separator: "\n").map(String.init)
    }

    private func permissions(_ url: URL) throws -> Int {
        let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
        return (attributes[.posixPermissions] as? NSNumber)?.intValue ?? -1
    }

    // MARK: - Enablement

    func testNightlyDetectedOnlyByBundleSuffix() {
        XCTAssertTrue(NightlyDiagnostics.isNightly(bundleIdentifier: "org.js-lab.durian.nightly"))
        XCTAssertFalse(NightlyDiagnostics.isNightly(bundleIdentifier: "org.js-lab.durian"))
        XCTAssertFalse(NightlyDiagnostics.isNightly(bundleIdentifier: "org.js-lab.durian.nightly.helper"))
        XCTAssertFalse(NightlyDiagnostics.isNightly(bundleIdentifier: nil))
    }

    func testReleaseRecordsNothingAndCreatesNoFiles() {
        let diagnostics = makeDiagnostics(bundleIdentifier: "org.js-lab.durian")
        XCTAssertFalse(diagnostics.isEnabled)
        diagnostics.record(.httpSearch, .ok, status: 200, durationMs: 12)
        diagnostics.flush()
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    func testReleaseHeartbeatNeverPings() {
        let diagnostics = makeDiagnostics(bundleIdentifier: "org.js-lab.durian")
        let pings = Counter()
        let heartbeat = UIHeartbeat(diagnostics: diagnostics, schedulePing: { _ in pings.increment() })
        heartbeat.start()
        heartbeat.startForTesting()
        heartbeat.tick()
        heartbeat.stop()
        XCTAssertEqual(pings.value, 0)
        XCTAssertFalse(FileManager.default.fileExists(atPath: directory.path))
    }

    // MARK: - Schema

    func testExactSerializedSchema() throws {
        let diagnostics = makeDiagnostics()
        diagnostics.record(.httpThread, .ok, status: 200, durationMs: 42)
        diagnostics.record(.httpOutboxSend, .httpError, status: 503, durationMs: 7, count: 3)
        diagnostics.record(.uiHeartbeat, .stall, durationMs: 300)
        diagnostics.flush()

        XCTAssertEqual(try lines(currentURL), [
            #"{"v":1,"ts":1700000000,"op":"http.thread","outcome":"ok","status":200,"ms":42,"count":1}"#,
            #"{"v":1,"ts":1700000000,"op":"http.outbox_send","outcome":"http_error","status":503,"ms":7,"count":3}"#,
            #"{"v":1,"ts":1700000000,"op":"ui.heartbeat","outcome":"stall","status":0,"ms":300,"count":1}"#,
        ])
        for line in try lines(currentURL) {
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any])
            XCTAssertEqual(Set(object.keys), ["v", "ts", "op", "outcome", "status", "ms", "count"])
        }
    }

    func testValuesAreClamped() {
        let event = DiagnosticEvent(
            timestamp: Date(timeIntervalSince1970: -5), operation: .httpOther, outcome: .transportError,
            status: 42, durationMs: -1, count: Int.max
        )
        XCTAssertEqual(
            event.jsonLine,
            "{\"v\":1,\"ts\":0,\"op\":\"http.other\",\"outcome\":\"transport_error\",\"status\":0,\"ms\":0,\"count\":1000000}\n"
        )
        XCTAssertEqual(
            DiagnosticEvent(timestamp: fixedDate, operation: .httpOther, outcome: .ok, status: 200,
                            durationMs: Int.max, count: 1).durationMs,
            DiagnosticEvent.maxDurationMs
        )
    }

    func testSchemaVocabularyIsStaticSnakeCase() {
        let allowed = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyz._")
        let values = DiagnosticOperation.allCases.map(\.rawValue) + DiagnosticOutcome.allCases.map(\.rawValue)
        for value in values {
            XCTAssertFalse(value.isEmpty)
            XCTAssertTrue(value.unicodeScalars.allSatisfy(allowed.contains), value)
        }
    }

    func testHTTPOperationsDiscardIdentifiersAndQueries() {
        let cases: [(String, String, DiagnosticOperation)] = [
            ("GET", "/version", .httpVersion),
            ("GET", "/search?query=from%3Aalice%40example.com&limit=50", .httpSearch),
            ("GET", "/search/count?query=tag:inbox", .httpSearchCount),
            ("GET", "/threads/secret-thread-id", .httpThread),
            ("POST", "/threads/secret-thread-id/tags", .httpThreadTags),
            ("GET", "/tags?account=alice%40example.com", .httpTags),
            ("GET", "/message/body?id=%3Cabc%40example.com%3E", .httpMessageBody),
            ("POST", "/messages/%3Cabc%40example.com%3E/reactions", .httpReaction),
            ("GET", "/outbox", .httpOutboxList),
            ("POST", "/outbox/send", .httpOutboxSend),
            ("DELETE", "/outbox/17", .httpOutboxDelete),
            ("GET", "/contacts?limit=100", .httpContactsList),
            ("GET", "/contacts/search?query=alice", .httpContactsSearch),
            ("POST", "/contacts/usage", .httpContactsUsage),
            ("GET", "/unknown/alice@example.com", .httpOther),
            ("GET", "", .httpOther),
        ]
        for (method, endpoint, expected) in cases {
            let operation = DiagnosticOperation.http(method: method, endpoint: endpoint)
            XCTAssertEqual(operation, expected, "\(method) route")
            XCTAssertFalse(operation.rawValue.contains("alice"))
            XCTAssertFalse(operation.rawValue.contains("secret"))
        }
    }

    // MARK: - Bounded storage

    func testRotationKeepsTwoBoundedFiles() throws {
        let maxBytes = 512
        let diagnostics = makeDiagnostics(maxFileBytes: maxBytes)
        for index in 0..<200 {
            diagnostics.record(.httpSearch, .ok, status: 200, durationMs: index)
        }
        diagnostics.flush()

        let files = try FileManager.default.contentsOfDirectory(atPath: directory.path).sorted()
        XCTAssertEqual(files, [NightlyDiagnostics.rotatedFileName, NightlyDiagnostics.currentFileName].sorted())
        for url in [currentURL, rotatedURL] {
            let size = try XCTUnwrap(FileManager.default.attributesOfItem(atPath: url.path)[.size] as? NSNumber)
            XCTAssertLessThanOrEqual(size.intValue, maxBytes)
            XCTAssertFalse(try lines(url).isEmpty)
        }
        // The newest event is retained in the current file.
        XCTAssertTrue(try XCTUnwrap(lines(currentURL).last).contains("\"ms\":199"))
    }

    func testBacklogIsBoundedAndDropsAreCounted() throws {
        let queue = DispatchQueue(label: "test.diagnostics.suspended")
        let diagnostics = makeDiagnostics(maxPending: 2, queue: queue)
        queue.suspend()
        for _ in 0..<5 {
            diagnostics.record(.httpTags, .ok, status: 200, durationMs: 1)
        }
        queue.resume()
        diagnostics.flush()

        let written = try lines(currentURL)
        XCTAssertEqual(written.filter { $0.contains("\"op\":\"http.tags\"") }.count, 2)
        XCTAssertEqual(written.filter { $0.contains("\"op\":\"diagnostics.dropped\"") }, [
            #"{"v":1,"ts":1700000000,"op":"diagnostics.dropped","outcome":"dropped","status":0,"ms":0,"count":3}"#,
        ])
    }

    // MARK: - Permissions

    func testFilesArePrivate() throws {
        let diagnostics = makeDiagnostics(maxFileBytes: 256)
        for _ in 0..<20 {
            diagnostics.record(.httpVersion, .ok, status: 200, durationMs: 1)
        }
        diagnostics.flush()

        XCTAssertEqual(try permissions(directory), 0o700)
        XCTAssertEqual(try permissions(currentURL), 0o600)
        XCTAssertEqual(try permissions(rotatedURL), 0o600)
    }

    func testTightensExistingPermissions() throws {
        try FileManager.default.createDirectory(
            at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o755]
        )
        XCTAssertTrue(FileManager.default.createFile(
            atPath: currentURL.path, contents: nil, attributes: [.posixPermissions: 0o644]
        ))
        let diagnostics = makeDiagnostics()
        diagnostics.record(.httpVersion, .ok, status: 200, durationMs: 1)
        diagnostics.flush()

        XCTAssertEqual(try permissions(directory), 0o700)
        XCTAssertEqual(try permissions(currentURL), 0o600)
    }

    func testDoesNotFollowSymlinkedLogFile() throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let target = directory.deletingLastPathComponent().appendingPathComponent("target.txt")
        XCTAssertTrue(FileManager.default.createFile(atPath: target.path, contents: Data()))
        try FileManager.default.createSymbolicLink(at: currentURL, withDestinationURL: target)

        let diagnostics = makeDiagnostics()
        diagnostics.record(.httpVersion, .ok, status: 200, durationMs: 1)
        diagnostics.flush()

        XCTAssertEqual(try Data(contentsOf: target), Data())
    }

    // MARK: - UI heartbeat

    func testHeartbeatRecordsOnlyStallsAboveThreshold() throws {
        let diagnostics = makeDiagnostics()
        let clock = ManualClock()
        let pings = PingQueue()
        let heartbeat = UIHeartbeat(
            diagnostics: diagnostics, uptimeNanoseconds: { clock.now },
            schedulePing: { pings.enqueue($0) }
        )
        heartbeat.startForTesting()

        heartbeat.tick()
        clock.advance(milliseconds: 250)
        pings.drain()

        heartbeat.tick()
        clock.advance(milliseconds: 251)
        pings.drain()
        diagnostics.flush()

        XCTAssertEqual(try lines(currentURL), [
            #"{"v":1,"ts":1700000000,"op":"ui.heartbeat","outcome":"stall","status":0,"ms":251,"count":1}"#,
        ])
    }

    func testHeartbeatQueuesAtMostOnePing() throws {
        let diagnostics = makeDiagnostics()
        let clock = ManualClock()
        let pings = PingQueue()
        let heartbeat = UIHeartbeat(
            diagnostics: diagnostics, uptimeNanoseconds: { clock.now },
            schedulePing: { pings.enqueue($0) }
        )
        heartbeat.startForTesting()

        for _ in 0..<10 {
            heartbeat.tick()
            clock.advance(milliseconds: 200)
        }
        XCTAssertEqual(pings.count, 1)
        pings.drain()
        diagnostics.flush()

        // One stall measured from the first (only) ping, not ten.
        XCTAssertEqual(try lines(currentURL).count, 1)
        XCTAssertTrue(try XCTUnwrap(lines(currentURL).first).contains("\"ms\":2000"))
    }

    func testStoppedHeartbeatDiscardsInFlightPing() {
        let diagnostics = makeDiagnostics()
        let clock = ManualClock()
        let pings = PingQueue()
        let heartbeat = UIHeartbeat(
            diagnostics: diagnostics, uptimeNanoseconds: { clock.now },
            schedulePing: { pings.enqueue($0) }
        )
        heartbeat.startForTesting()
        heartbeat.tick()

        // App resigns active / system sleeps while the ping waits.
        heartbeat.stop()
        clock.advance(milliseconds: 60_000)
        heartbeat.startForTesting()
        heartbeat.tick()
        XCTAssertEqual(pings.count, 1, "no second ping while the stale one is queued")
        pings.drain()
        diagnostics.flush()

        XCTAssertFalse(FileManager.default.fileExists(atPath: currentURL.path))
    }

    func testElapsedMillisecondsIsMonotonicSafe() {
        XCTAssertEqual(NightlyDiagnostics.elapsedMilliseconds(since: 5_000_000, now: 1_000_000), 0)
        XCTAssertEqual(NightlyDiagnostics.elapsedMilliseconds(since: 1_000_000, now: 301_000_000), 300)
    }
}

// MARK: - Test doubles

private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var storage = 0
    var value: Int { lock.withLock { storage } }
    func increment() { lock.withLock { storage += 1 } }
}

private final class ManualClock: @unchecked Sendable {
    private let lock = NSLock()
    private var storage: UInt64 = 1_000_000_000
    var now: UInt64 { lock.withLock { storage } }
    func advance(milliseconds: UInt64) { lock.withLock { storage += milliseconds * 1_000_000 } }
}

private final class PingQueue: @unchecked Sendable {
    private let lock = NSLock()
    private var pings: [@Sendable () -> Void] = []
    var count: Int { lock.withLock { pings.count } }
    func enqueue(_ ping: @escaping @Sendable () -> Void) { lock.withLock { pings.append(ping) } }
    func drain() {
        let pending = lock.withLock { () -> [@Sendable () -> Void] in
            defer { pings.removeAll() }
            return pings
        }
        pending.forEach { $0() }
    }
}
