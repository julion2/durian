//
//  AttachmentCacheManager.swift
//  Durian
//
//  Caches downloaded attachments locally to avoid repeated IMAP fetches.
//  Prefetches attachments when a thread is opened.
//

import Foundation

@MainActor
class AttachmentCacheManager: ObservableObject {
    static let shared = AttachmentCacheManager()

    private let fileManager = FileManager.default
    private let indexFile: URL
    private let cacheDir: URL
    private let settingsProvider: () -> AttachmentCacheSettings
    private var index: [String: CachedAttachment] = [:]
    private var prefetchTasks: [String: Task<Void, Never>] = [:]
    private var failedKeys: Set<String> = []

    init(cacheDir: URL? = nil,
         settingsProvider: @escaping () -> AttachmentCacheSettings = { SettingsManager.shared.attachmentCacheSettings })
    {
        let resolvedDir: URL
        if let cacheDir {
            resolvedDir = cacheDir
        } else {
            let caches = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask).first!
            resolvedDir = caches.appendingPathComponent("org.js-lab.durian/attachments", isDirectory: true)
        }
        self.cacheDir = resolvedDir
        indexFile = resolvedDir.appendingPathComponent(".cache-index.json")
        self.settingsProvider = settingsProvider
        try? fileManager.createDirectory(at: resolvedDir, withIntermediateDirectories: true)
        loadIndex()
        evict()
    }

    // MARK: - Public API

    /// Returns cached data if available, nil otherwise.
    func get(messageId: String, partId: Int) -> Data? {
        let key = cacheKey(messageId: messageId, partId: partId)
        guard var entry = index[key] else { return nil }

        // TTL check — pinned entries are exempt (consistent with evict()).
        let settings = settingsProvider()
        if !entry.pinned && Date().timeIntervalSince(entry.cachedAt) > settings.ttl {
            remove(key: key)
            return nil
        }

        guard fileManager.fileExists(atPath: entry.localPath.path) else {
            index.removeValue(forKey: key)
            saveIndex()
            return nil
        }

        guard let data = try? Data(contentsOf: entry.localPath) else { return nil }

        // Update access metadata in-memory only; the next put/evict/clear
        // will persist it. Avoids a JSON-encode + disk-write per cache hit.
        entry.lastAccessDate = Date()
        entry.accessCount += 1
        index[key] = entry

        return data
    }

    /// Store attachment data in cache.
    func put(messageId: String, partId: Int, filename: String, data: Data) {
        let key = cacheKey(messageId: messageId, partId: partId)
        let safeFilename = filename.replacingOccurrences(of: "/", with: "_")
        let localPath = cacheDir.appendingPathComponent("\(key)_\(safeFilename)")

        do {
            try data.write(to: localPath)
        } catch {
            Log.error("CACHE", "Failed to write \(filename): \(error)")
            return
        }

        index[key] = CachedAttachment(
            id: UUID(),
            filename: filename,
            localPath: localPath,
            sizeBytes: Int64(data.count),
            cachedAt: Date(),
            lastAccessDate: Date(),
            accessCount: 1,
            emailUID: 0,
            pinned: false
        )
        saveIndex()
        evict()
    }

    /// Check if an attachment is cached (or known to be unavailable).
    func isCached(messageId: String, partId: Int) -> Bool {
        let key = cacheKey(messageId: messageId, partId: partId)
        if failedKeys.contains(key) { return false }
        guard let entry = index[key] else { return false }
        let settings = settingsProvider()
        if !entry.pinned && Date().timeIntervalSince(entry.cachedAt) > settings.ttl {
            return false
        }
        return fileManager.fileExists(atPath: entry.localPath.path)
    }

    /// Whether a prefetch already failed for this attachment (stale UID, etc.)
    func prefetchFailed(messageId: String, partId: Int) -> Bool {
        failedKeys.contains(cacheKey(messageId: messageId, partId: partId))
    }

    /// Prefetch all attachments for a thread's messages in the background.
    func prefetch(messages: [ThreadMessage], backend: EmailBackend) {
        for message in messages {
            guard let attachments = message.attachments, !attachments.isEmpty else { continue }
            for attachment in attachments {
                let key = cacheKey(messageId: message.id, partId: attachment.partId)
                guard index[key] == nil else { continue }
                guard prefetchTasks[key] == nil else { continue }

                guard !failedKeys.contains(key) else { continue }

                prefetchTasks[key] = Task {
                    do {
                        let (data, _) = try await backend.downloadAttachment(
                            messageId: message.id,
                            partId: attachment.partId
                        )
                        put(messageId: message.id, partId: attachment.partId,
                            filename: attachment.filename, data: data)
                        Log.debug("CACHE", "Prefetched \(attachment.filename) (\(data.count) bytes)")
                    } catch {
                        failedKeys.insert(key)
                        Log.debug("CACHE", "Prefetch failed for \(attachment.filename): \(error)")
                    }
                    prefetchTasks.removeValue(forKey: key)
                }
            }
        }
    }

    /// Cancel all active prefetch tasks.
    func cancelPrefetch() {
        for (_, task) in prefetchTasks {
            task.cancel()
        }
        prefetchTasks.removeAll()
    }

    /// Total cache size in bytes.
    var totalSize: Int64 {
        index.values.reduce(0) { $0 + $1.sizeBytes }
    }

    /// Clear entire cache.
    func clearAll() {
        for (_, entry) in index {
            try? fileManager.removeItem(at: entry.localPath)
        }
        index.removeAll()
        saveIndex()
        Log.info("CACHE", "Cache cleared")
    }

    // MARK: - Eviction

    private func evict() {
        let settings = settingsProvider()
        let now = Date()

        // Phase 1: Remove expired entries. Snapshot the keys first — mutating
        // `index` during iteration is undefined behavior in Swift.
        let expiredKeys: [String] = index.compactMap { key, entry in
            guard !entry.pinned else { return nil }
            return now.timeIntervalSince(entry.cachedAt) > settings.ttl ? key : nil
        }
        for key in expiredKeys {
            remove(key: key)
        }

        // Phase 2: LRU eviction if still over size limit. Pinned entries are
        // immune — the loop bails when no unpinned candidates remain, even if
        // pinned alone exceeds the cap.
        while totalSize > settings.maxSizeBytes {
            guard let oldest = index
                .filter({ !$0.value.pinned })
                .min(by: { $0.value.lastAccessDate < $1.value.lastAccessDate })
            else { break }
            remove(key: oldest.key)
        }
    }

    // MARK: - Persistence

    private func loadIndex() {
        guard fileManager.fileExists(atPath: indexFile.path) else { return }
        do {
            let data = try Data(contentsOf: indexFile)
            index = try JSONDecoder().decode([String: CachedAttachment].self, from: data)
        } catch {
            Log.warning("CACHE", "Failed to load index: \(error)")
            index = [:]
        }
    }

    private func saveIndex() {
        do {
            let data = try JSONEncoder().encode(index)
            try data.write(to: indexFile)
        } catch {
            Log.error("CACHE", "Failed to save index: \(error)")
        }
    }

    // MARK: - Helpers

    private func cacheKey(messageId: String, partId: Int) -> String {
        "\(messageId):\(partId)"
    }

    private func remove(key: String) {
        guard let entry = index[key] else { return }
        try? fileManager.removeItem(at: entry.localPath)
        index.removeValue(forKey: key)
        saveIndex()
    }
}
