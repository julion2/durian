@testable import durian_lib
import XCTest

@MainActor
final class AttachmentCacheManagerTests: XCTestCase {

    // MARK: - Helpers

    private var tempDir: URL!

    override func setUp() async throws {
        tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("stability-cache-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
    }

    override func tearDown() async throws {
        try? FileManager.default.removeItem(at: tempDir)
        tempDir = nil
    }

    private func makeManager(maxSizeMB: Int = 100, ttlDays: Int = 7) -> AttachmentCacheManager {
        var settings = AttachmentCacheSettings()
        settings.maxSizeMB = maxSizeMB
        settings.ttlDays = ttlDays
        return AttachmentCacheManager(cacheDir: tempDir, settingsProvider: { settings })
    }

    private func bytes(_ count: Int) -> Data {
        Data(repeating: 0xAB, count: count)
    }

    // MARK: - Basic put/get

    func testPutThenGetReturnsData() async {
        let mgr = makeManager()
        let payload = bytes(1024)

        await mgr.put(messageId: "msg-1", partId: 2, filename: "a.pdf", data: payload)
        let fetched = await mgr.get(messageId: "msg-1", partId: 2)

        XCTAssertEqual(fetched, payload)
    }

    func testGetReturnsNilForUnknownKey() async {
        let mgr = makeManager()
        let fetched = await mgr.get(messageId: "nope", partId: 0)
        XCTAssertNil(fetched)
    }

    // MARK: - TTL Eviction

    func testTTLEvictionRemovesExpiredEntries() async throws {
        let mgr = makeManager(ttlDays: 7)
        await mgr.put(messageId: "msg-old", partId: 1, filename: "old.pdf", data: bytes(1024))

        // Backdate the index entry so it's older than TTL.
        let indexFile = tempDir.appendingPathComponent(".cache-index.json")
        var raw = try JSONDecoder().decode([String: CachedAttachment].self, from: Data(contentsOf: indexFile))
        for (key, var entry) in raw {
            entry = CachedAttachment(
                id: entry.id, filename: entry.filename, localPath: entry.localPath,
                sizeBytes: entry.sizeBytes,
                cachedAt: Date().addingTimeInterval(-8 * 86_400),
                lastAccessDate: entry.lastAccessDate, accessCount: entry.accessCount,
                emailUID: entry.emailUID, pinned: entry.pinned)
            raw[key] = entry
        }
        try JSONEncoder().encode(raw).write(to: indexFile)

        // The first operation lazily loads and evicts the persisted index.
        let fresh = makeManager(ttlDays: 7)
        let fetched = await fresh.get(messageId: "msg-old", partId: 1)
        let size = await fresh.totalSize
        XCTAssertNil(fetched)
        XCTAssertEqual(size, 0)
    }

    func testTTLEvictionHandlesManyExpiredEntries() async throws {
        // Phase 1 of evict() iterates the index while removing entries from
        // it. With 50 expired keys, any mutation-during-iteration regression
        // would surface as a crash or partial cleanup.
        let mgr = makeManager(ttlDays: 7)
        for i in 0..<50 {
            await mgr.put(messageId: "m\(i)", partId: 0, filename: "f\(i).bin", data: bytes(64))
        }

        // Backdate every entry past TTL.
        let indexFile = tempDir.appendingPathComponent(".cache-index.json")
        var raw = try JSONDecoder().decode([String: CachedAttachment].self, from: Data(contentsOf: indexFile))
        for (key, entry) in raw {
            raw[key] = CachedAttachment(
                id: entry.id, filename: entry.filename, localPath: entry.localPath,
                sizeBytes: entry.sizeBytes,
                cachedAt: Date().addingTimeInterval(-30 * 86_400),
                lastAccessDate: entry.lastAccessDate, accessCount: entry.accessCount,
                emailUID: entry.emailUID, pinned: false)
        }
        try JSONEncoder().encode(raw).write(to: indexFile)

        // Fresh manager → the first operation evicts everything.
        let fresh = makeManager(ttlDays: 7)
        let size = await fresh.totalSize
        XCTAssertEqual(size, 0, "all 50 expired entries must be evicted in one Phase 1 sweep")
        for i in 0..<50 {
            let fetched = await fresh.get(messageId: "m\(i)", partId: 0)
            XCTAssertNil(fetched)
        }
    }

    // MARK: - Size-based LRU Eviction

    func testSizeEvictionRemovesLRUEntries() async {
        // 1 MB cap; three 400 KB entries → third put forces eviction of LRU.
        let mgr = makeManager(maxSizeMB: 1)
        let kb400 = bytes(400 * 1024)

        await mgr.put(messageId: "a", partId: 0, filename: "a.bin", data: kb400)
        await mgr.put(messageId: "b", partId: 0, filename: "b.bin", data: kb400)

        // Touch "a" so "b" becomes the LRU.
        _ = await mgr.get(messageId: "a", partId: 0)

        await mgr.put(messageId: "c", partId: 0, filename: "c.bin", data: kb400)

        let a = await mgr.get(messageId: "a", partId: 0)
        let b = await mgr.get(messageId: "b", partId: 0)
        let c = await mgr.get(messageId: "c", partId: 0)
        XCTAssertNotNil(a, "recently accessed must survive")
        XCTAssertNil(b, "LRU must be evicted")
        XCTAssertNotNil(c, "newest must survive")
    }

    // MARK: - Pinned-Entry Survival

    func testPinnedEntriesSurviveTTLAndSizePressure() async throws {
        // Cap = 1 MB. Insert one 400 KB pinned entry that is OLDER than TTL.
        // Pinned must survive TTL (Phase 1) AND LRU pressure (Phase 2).
        let mgr = makeManager(maxSizeMB: 1, ttlDays: 7)
        let kb400 = bytes(400 * 1024)
        await mgr.put(messageId: "pinned", partId: 0, filename: "p.bin", data: kb400)

        // Pin + backdate the entry on disk.
        let indexFile = tempDir.appendingPathComponent(".cache-index.json")
        var raw = try JSONDecoder().decode([String: CachedAttachment].self, from: Data(contentsOf: indexFile))
        let key = "pinned:0"
        guard let original = raw[key] else { return XCTFail("missing entry") }
        raw[key] = CachedAttachment(
            id: original.id, filename: original.filename, localPath: original.localPath,
            sizeBytes: original.sizeBytes,
            cachedAt: Date().addingTimeInterval(-30 * 86_400),
            lastAccessDate: Date().addingTimeInterval(-30 * 86_400),
            accessCount: original.accessCount, emailUID: original.emailUID,
            pinned: true)
        try JSONEncoder().encode(raw).write(to: indexFile)

        // Fresh manager: lazily loads and evicts the manipulated index,
        // exercising Phase 1 (TTL) on the pinned-and-expired entry.
        let fresh = makeManager(maxSizeMB: 1, ttlDays: 7)
        let beforePressure = await fresh.get(messageId: "pinned", partId: 0)
        XCTAssertNotNil(beforePressure,
                        "pinned must survive TTL eviction (Phase 1) even when older than TTL")

        // Pile on three 400 KB unpinned entries → 1.2 MB unpinned + 0.4 MB
        // pinned = 1.6 MB total, well over the 1 MB cap. Each put() triggers
        // evict()'s Phase 2 (LRU) which must skip the pinned entry.
        await fresh.put(messageId: "a", partId: 0, filename: "a.bin", data: kb400)
        await fresh.put(messageId: "b", partId: 0, filename: "b.bin", data: kb400)
        await fresh.put(messageId: "c", partId: 0, filename: "c.bin", data: kb400)

        let afterPressure = await fresh.get(messageId: "pinned", partId: 0)
        XCTAssertNotNil(afterPressure,
                        "pinned must survive LRU eviction (Phase 2) under sustained size pressure")
        // After eviction, totalSize must be ≤ cap + 1 unpinned (LRU stops once
        // under cap). Pinned alone is 400 KB, so totalSize will sit between
        // 400 KB and ~1.4 MB depending on which unpinned survived last.
        let size = await fresh.totalSize
        XCTAssertLessThanOrEqual(size, Int64(1_048_576 + 400 * 1024),
                                 "Phase 2 should evict unpinned until under cap, leaving at most one extra")
        XCTAssertGreaterThanOrEqual(size, Int64(400 * 1024),
                                    "pinned 400 KB always remains")
    }

    // MARK: - Orphan Files

    func testOrphanFileOnDiskWithoutIndexEntry() async throws {
        // A stray file in the cache dir without index entry is currently NOT
        // cleaned up. This test pins the current behaviour so we notice when
        // we add reconciliation later.
        let mgr = makeManager()
        let stray = tempDir.appendingPathComponent("orphan.bin")
        try bytes(2048).write(to: stray)

        // Adding/removing a normal entry triggers evict(), but the orphan
        // is invisible to the index.
        await mgr.put(messageId: "x", partId: 0, filename: "x.bin", data: bytes(128))
        await mgr.clearAll()

        XCTAssertTrue(FileManager.default.fileExists(atPath: stray.path),
                      "orphan currently survives — update test when reconcile is added")
    }

    func testClearRejectsLateWriteFromEarlierGeneration() async {
        let storage = AttachmentCacheStorage(cacheDir: tempDir)
        let settings = AttachmentCacheSettings()
        await storage.put(key: "old:1", filename: "old.bin", data: bytes(11), settings: settings, generation: 0)
        await storage.clearAll(settings: settings, generation: 1)
        await storage.put(key: "late:1", filename: "late.bin", data: bytes(23), settings: settings, generation: 0)
        let clearedSize = await storage.size(settings: settings)
        XCTAssertEqual(clearedSize, 0)
        await storage.put(key: "new:1", filename: "new.bin", data: bytes(37), settings: settings, generation: 1)
        let newSize = await storage.size(settings: settings)
        XCTAssertEqual(newSize, 37, "a clear must not permanently disable future writes")
    }

    func testCancelledWriteDoesNotCreateFiles() async {
        let mgr = makeManager()
        let task = Task { @MainActor in
            await mgr.put(messageId: "cancelled", partId: 0, filename: "cancelled.bin", data: bytes(128))
        }
        // The MainActor task cannot start before this synchronous cancellation.
        task.cancel()
        await task.value
        XCTAssertEqual(try? FileManager.default.contentsOfDirectory(atPath: tempDir.path), [])
    }

    func testReplacementDoesNotLeaveOldFileBehind() async throws {
        let mgr = makeManager()
        await mgr.put(messageId: "../../message", partId: 1, filename: "../old.bin", data: bytes(11))
        await mgr.put(messageId: "../../message", partId: 1, filename: "../new.bin", data: bytes(29))
        let fetched = await mgr.get(messageId: "../../message", partId: 1)
        XCTAssertEqual(fetched, bytes(29))
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: tempDir.path).count, 2,
                       "one data file and the index; keys and filenames cannot escape the cache")
    }
}
