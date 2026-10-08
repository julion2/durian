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

    private let storage: AttachmentCacheStorage
    private let settingsProvider: () -> AttachmentCacheSettings
    private var prefetchTasks: [String: Task<Void, Never>] = [:]
    private var failedKeys: Set<String> = []
    private var generation = 0

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
        storage = AttachmentCacheStorage(cacheDir: resolvedDir)
        self.settingsProvider = settingsProvider
    }

    // MARK: - Public API

    /// Returns cached data if available, nil otherwise.
    func get(messageId: String, partId: Int) async -> Data? {
        await storage.get(key: cacheKey(messageId: messageId, partId: partId), settings: settingsProvider())
    }

    /// Store attachment data in cache.
    func put(messageId: String, partId: Int, filename: String, data: Data) async {
        await storage.put(key: cacheKey(messageId: messageId, partId: partId), filename: filename,
                          data: data, settings: settingsProvider(), generation: generation)
    }

    /// Check if an attachment is cached (or known to be unavailable).
    func isCached(messageId: String, partId: Int) async -> Bool {
        let key = cacheKey(messageId: messageId, partId: partId)
        if failedKeys.contains(key) { return false }
        return await storage.isCached(key: key, settings: settingsProvider())
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
                let key = cacheKey(messageId: message.attachmentCacheId, partId: attachment.partId)
                guard prefetchTasks[key] == nil else { continue }
                guard !failedKeys.contains(key) else { continue }

                let startedGeneration = generation
                prefetchTasks[key] = Task {
                    defer {
                        if generation == startedGeneration {
                            prefetchTasks.removeValue(forKey: key)
                        }
                    }
                    guard !Task.isCancelled else { return }
                    if await isCached(messageId: message.attachmentCacheId, partId: attachment.partId) { return }
                    do {
                        try Task.checkCancellation()
                        let (data, _) = try await backend.downloadAttachment(
                            messageId: message.id,
                            partId: attachment.partId
                        )
                        try Task.checkCancellation()
                        guard generation == startedGeneration else { return }
                        await put(messageId: message.attachmentCacheId, partId: attachment.partId,
                                  filename: attachment.filename, data: data)
                    } catch {
                        guard !Task.isCancelled, generation == startedGeneration else { return }
                        failedKeys.insert(key)
                        Log.debug("CACHE", "Attachment prefetch failed")
                    }
                }
            }
        }
    }

    /// Cancel all active prefetch tasks.
    func cancelPrefetch() {
        generation += 1
        for (_, task) in prefetchTasks {
            task.cancel()
        }
        prefetchTasks.removeAll()
    }

    /// Total cache size in bytes.
    var totalSize: Int64 {
        get async { await storage.size(settings: settingsProvider()) }
    }

    /// Clear entire cache.
    func clearAll() async {
        cancelPrefetch()
        failedKeys.removeAll()
        await storage.clearAll(settings: settingsProvider(), generation: generation)
    }

    private func cacheKey(messageId: String, partId: Int) -> String {
        "\(messageId):\(partId)"
    }
}

/// Owns both the index and its files on a non-main serial executor. Operations
/// never suspend mid-transaction, so a clear cannot race with a disk write.
actor AttachmentCacheStorage {
    private let cacheDir: URL
    private let indexFile: URL
    private let fileManager = FileManager.default
    private var index: [String: CachedAttachment] = [:]
    private var loaded = false
    private var minimumGeneration = 0

    init(cacheDir: URL) {
        self.cacheDir = cacheDir
        indexFile = cacheDir.appendingPathComponent(".cache-index.json")
    }

    func get(key: String, settings: AttachmentCacheSettings) -> Data? {
        loadIfNeeded(settings: settings)
        guard isCached(key: key, settings: settings), var entry = index[key],
              let data = try? Data(contentsOf: entry.localPath) else { return nil }
        entry.lastAccessDate = Date()
        entry.accessCount += 1
        index[key] = entry
        return data
    }

    func put(key: String, filename: String, data: Data, settings: AttachmentCacheSettings, generation: Int) {
        guard !Task.isCancelled, generation >= minimumGeneration else { return }
        loadIfNeeded(settings: settings)
        // The on-disk name is independent of untrusted filenames and message IDs.
        let localPath = index[key]?.localPath ?? cacheDir.appendingPathComponent(UUID().uuidString)
        do {
            try data.write(to: localPath, options: .atomic)
        } catch {
            Log.error("CACHE", "Failed to write attachment cache")
            return
        }
        index[key] = CachedAttachment(
            id: UUID(), filename: filename, localPath: localPath, sizeBytes: Int64(data.count),
            cachedAt: Date(), lastAccessDate: Date(), accessCount: 1, emailUID: 0, pinned: false
        )
        evict(settings: settings)
        saveIndex()
    }

    func isCached(key: String, settings: AttachmentCacheSettings) -> Bool {
        loadIfNeeded(settings: settings)
        guard let entry = index[key] else { return false }
        if (!entry.pinned && Date().timeIntervalSince(entry.cachedAt) > settings.ttl)
            || !fileManager.fileExists(atPath: entry.localPath.path)
        {
            remove(key: key)
            saveIndex()
            return false
        }
        return true
    }

    func size(settings: AttachmentCacheSettings) -> Int64 {
        loadIfNeeded(settings: settings)
        return totalSize
    }

    func clearAll(settings: AttachmentCacheSettings, generation: Int) {
        loadIfNeeded(settings: settings)
        minimumGeneration = max(minimumGeneration, generation)
        for entry in index.values {
            try? fileManager.removeItem(at: entry.localPath)
        }
        index.removeAll()
        saveIndex()
    }

    private var totalSize: Int64 {
        index.values.reduce(0) { $0 + $1.sizeBytes }
    }

    // MARK: - Eviction

    private func evict(settings: AttachmentCacheSettings) {
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

    private func loadIfNeeded(settings: AttachmentCacheSettings) {
        guard !loaded else { return }
        loaded = true
        try? fileManager.createDirectory(at: cacheDir, withIntermediateDirectories: true)
        guard fileManager.fileExists(atPath: indexFile.path) else { return }
        do {
            let data = try Data(contentsOf: indexFile)
            index = try JSONDecoder().decode([String: CachedAttachment].self, from: data)
        } catch {
            Log.warning("CACHE", "Failed to load attachment cache index")
            index = [:]
        }
        evict(settings: settings)
        saveIndex()
    }

    private func saveIndex() {
        do {
            let data = try JSONEncoder().encode(index)
            try data.write(to: indexFile, options: .atomic)
        } catch {
            Log.error("CACHE", "Failed to save attachment cache index")
        }
    }

    // MARK: - Helpers

    private func remove(key: String) {
        guard let entry = index[key] else { return }
        try? fileManager.removeItem(at: entry.localPath)
        index.removeValue(forKey: key)
    }
}
