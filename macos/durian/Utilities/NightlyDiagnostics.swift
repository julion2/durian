import Foundation

// MARK: - Schema

/// Closed set of diagnosed operations. Raw values are the only operation text
/// ever written; they are static and never derived from request content.
enum DiagnosticOperation: String, CaseIterable, Sendable {
    case httpVersion = "http.version"
    case httpSearch = "http.search"
    case httpSearchCount = "http.search_count"
    case httpThread = "http.thread"
    case httpThreadTags = "http.thread_tags"
    case httpTags = "http.tags"
    case httpMessageBody = "http.message_body"
    case httpReaction = "http.reaction"
    case httpOutboxList = "http.outbox_list"
    case httpOutboxSend = "http.outbox_send"
    case httpOutboxDelete = "http.outbox_delete"
    case httpContactsList = "http.contacts_list"
    case httpContactsSearch = "http.contacts_search"
    case httpContactsUsage = "http.contacts_usage"
    case httpOther = "http.other"
    case uiHeartbeat = "ui.heartbeat"
    case diagnosticsDropped = "diagnostics.dropped"

    /// Maps a CLI API request onto a static route category. Only fixed path
    /// segments are compared; identifiers and query strings are discarded and
    /// unknown routes collapse to `.httpOther`.
    static func http(method: String, endpoint: String) -> DiagnosticOperation {
        let path = endpoint.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false).first ?? ""
        let parts = path.split(separator: "/")
        let method = method.uppercased()

        switch (parts.first, parts.count) {
        case ("version", 1): return .httpVersion
        case ("search", 1): return .httpSearch
        case ("search", 2) where parts[1] == "count": return .httpSearchCount
        case ("threads", 2) where method == "GET": return .httpThread
        case ("threads", 3) where parts[2] == "tags" && method == "POST": return .httpThreadTags
        case ("tags", 1): return .httpTags
        case ("message", 2) where parts[1] == "body": return .httpMessageBody
        case ("messages", 3) where parts[2] == "reactions" && method == "POST": return .httpReaction
        case ("outbox", 1) where method == "GET": return .httpOutboxList
        case ("outbox", 2) where parts[1] == "send" && method == "POST": return .httpOutboxSend
        case ("outbox", 2) where method == "DELETE": return .httpOutboxDelete
        case ("contacts", 1): return .httpContactsList
        case ("contacts", 2) where parts[1] == "search": return .httpContactsSearch
        case ("contacts", 2) where parts[1] == "usage" && method == "POST": return .httpContactsUsage
        default: return .httpOther
        }
    }
}

enum DiagnosticOutcome: String, CaseIterable, Sendable {
    case ok
    case cancelled
    case transportError = "transport_error"
    case httpError = "http_error"
    case decodeError = "decode_error"
    case stall
    case dropped
}

/// One diagnostics record. Every field is a closed enum or a clamped integer,
/// so a serialized line cannot carry free text, identifiers, URLs, paths,
/// tokens or error descriptions.
struct DiagnosticEvent: Equatable, Sendable {
    static let schemaVersion = 1
    static let maxDurationMs = 3_600_000
    static let maxCount = 1_000_000

    let timestamp: Int
    let operation: DiagnosticOperation
    let outcome: DiagnosticOutcome
    /// HTTP status (100...599) or 0 when there is none.
    let status: Int
    let durationMs: Int
    let count: Int

    init(timestamp: Date, operation: DiagnosticOperation, outcome: DiagnosticOutcome, status: Int, durationMs: Int, count: Int) {
        let seconds = timestamp.timeIntervalSince1970
        self.timestamp = seconds.isFinite ? max(0, Int(seconds.rounded(.down))) : 0
        self.operation = operation
        self.outcome = outcome
        self.status = (100...599).contains(status) ? status : 0
        self.durationMs = min(max(durationMs, 0), Self.maxDurationMs)
        self.count = min(max(count, 0), Self.maxCount)
    }

    /// Fixed key order; values are enum raw values (static ASCII) or integers.
    var jsonLine: String {
        "{\"v\":\(Self.schemaVersion),\"ts\":\(timestamp),\"op\":\"\(operation.rawValue)\","
            + "\"outcome\":\"\(outcome.rawValue)\",\"status\":\(status),\"ms\":\(durationMs),\"count\":\(count)}\n"
    }
}

// MARK: - Recorder

/// Nightly-only local diagnostics: bounded JSONL files with owner-only
/// permissions, written on a background queue. Release builds (bundle ID
/// without the `.nightly` suffix) record nothing and touch no files.
final class NightlyDiagnostics: @unchecked Sendable {
    static let currentFileName = "diagnostics.jsonl"
    static let rotatedFileName = "diagnostics.1.jsonl"
    static let defaultMaxFileBytes = 64 * 1024
    static let defaultMaxPending = 256

    static let shared: NightlyDiagnostics = {
        let identifier = Bundle.main.bundleIdentifier ?? "org.js-lab.durian"
        return NightlyDiagnostics(bundleIdentifier: identifier, directory: defaultDirectory(bundleIdentifier: identifier))
    }()

    let isEnabled: Bool
    let directory: URL
    private let maxFileBytes: Int
    private let maxPending: Int
    private let now: @Sendable () -> Date
    private let queue: DispatchQueue

    private let lock = NSLock()
    private var pending = 0
    private var dropped = 0
    // Touched only on `queue`.
    private var directoryReady = false

    init(
        bundleIdentifier: String?,
        directory: URL,
        maxFileBytes: Int = NightlyDiagnostics.defaultMaxFileBytes,
        maxPending: Int = NightlyDiagnostics.defaultMaxPending,
        now: @escaping @Sendable () -> Date = { Date() },
        queue: DispatchQueue = DispatchQueue(label: "org.js-lab.durian.diagnostics", qos: .utility)
    ) {
        isEnabled = Self.isNightly(bundleIdentifier: bundleIdentifier)
        self.directory = directory
        self.maxFileBytes = max(maxFileBytes, 256)
        self.maxPending = max(maxPending, 1)
        self.now = now
        self.queue = queue
    }

    static func isNightly(bundleIdentifier: String?) -> Bool {
        bundleIdentifier?.hasSuffix(".nightly") == true
    }

    static func defaultDirectory(bundleIdentifier: String) -> URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Application Support", isDirectory: true)
        return base
            .appendingPathComponent(bundleIdentifier, isDirectory: true)
            .appendingPathComponent("Diagnostics", isDirectory: true)
    }

    /// Monotonic milliseconds since `start` (a `DispatchTime` uptime value,
    /// which does not advance while the system sleeps).
    static func elapsedMilliseconds(since start: UInt64, now: UInt64 = DispatchTime.now().uptimeNanoseconds) -> Int {
        now > start ? Int((now - start) / 1_000_000) : 0
    }

    // MARK: Recording

    /// Non-blocking. Drops (and later counts) events when the background
    /// backlog is full, so callers on the main thread never wait on I/O.
    func record(_ operation: DiagnosticOperation, _ outcome: DiagnosticOutcome, status: Int = 0, durationMs: Int, count: Int = 1) {
        guard isEnabled else { return }
        let event = DiagnosticEvent(
            timestamp: now(), operation: operation, outcome: outcome,
            status: status, durationMs: durationMs, count: count
        )

        lock.lock()
        guard pending < maxPending else {
            dropped += 1
            lock.unlock()
            return
        }
        pending += 1
        lock.unlock()

        queue.async { [self] in
            append(event)
            lock.lock()
            pending -= 1
            let droppedCount = dropped
            dropped = 0
            lock.unlock()
            if droppedCount > 0 {
                append(DiagnosticEvent(
                    timestamp: now(), operation: .diagnosticsDropped, outcome: .dropped,
                    status: 0, durationMs: 0, count: droppedCount
                ))
            }
        }
    }

    /// Waits for queued writes. Tests only; never call on the main thread in the app.
    func flush() {
        queue.sync {}
    }

    // MARK: File I/O (queue only)

    private func append(_ event: DiagnosticEvent) {
        guard prepareDirectory() else { return }
        let line = Data(event.jsonLine.utf8)
        let current = directory.appendingPathComponent(Self.currentFileName)

        var info = stat()
        if lstat(current.path, &info) == 0, Int(info.st_size) + line.count > maxFileBytes {
            let rotated = directory.appendingPathComponent(Self.rotatedFileName)
            if rename(current.path, rotated.path) != 0 {
                unlink(current.path)
            }
        }

        let fd = open(current.path, O_WRONLY | O_APPEND | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard fd >= 0 else {
            directoryReady = false
            return
        }
        defer { close(fd) }
        _ = fchmod(fd, 0o600)
        line.withUnsafeBytes { buffer in
            guard let base = buffer.baseAddress else { return }
            _ = write(fd, base, buffer.count)
        }
    }

    private func prepareDirectory() -> Bool {
        if directoryReady { return true }
        do {
            try FileManager.default.createDirectory(
                at: directory, withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
        } catch {
            return false
        }
        var info = stat()
        guard lstat(directory.path, &info) == 0, (info.st_mode & S_IFMT) == S_IFDIR,
              chmod(directory.path, 0o700) == 0
        else { return false }
        directoryReady = true
        return true
    }
}

// MARK: - UI Heartbeat

/// Detects main-thread stalls: an off-main timer enqueues at most one
/// MainActor ping at a time and records a `ui.heartbeat`/`stall` event when
/// the ping waited longer than the threshold. `stop()` invalidates an
/// in-flight ping (it is not measured) so sleep or inactivity never produces a
/// false stall; a new ping is only sent once the previous one was delivered.
final class UIHeartbeat: @unchecked Sendable {
    static let stallThresholdMs = 250

    private let diagnostics: NightlyDiagnostics
    private let interval: DispatchTimeInterval
    private let uptimeNanoseconds: @Sendable () -> UInt64
    private let schedulePing: @Sendable (@escaping @Sendable () -> Void) -> Void
    private let timerQueue = DispatchQueue(label: "org.js-lab.durian.heartbeat", qos: .utility)

    private let lock = NSLock()
    private var timer: DispatchSourceTimer?
    private var isRunning = false
    private var generation = 0
    private var pingInFlight = false
    private var pingSentAt: UInt64 = 0

    init(
        diagnostics: NightlyDiagnostics,
        interval: DispatchTimeInterval = .milliseconds(200),
        uptimeNanoseconds: @escaping @Sendable () -> UInt64 = { DispatchTime.now().uptimeNanoseconds },
        schedulePing: @escaping @Sendable (@escaping @Sendable () -> Void) -> Void = { ping in
            Task { @MainActor in ping() }
        }
    ) {
        self.diagnostics = diagnostics
        self.interval = interval
        self.uptimeNanoseconds = uptimeNanoseconds
        self.schedulePing = schedulePing
    }

    deinit {
        timer?.cancel()
    }

    /// Begins monitoring; no-op when diagnostics are disabled or already running.
    func start() {
        guard diagnostics.isEnabled else { return }
        lock.lock()
        defer { lock.unlock() }
        guard !isRunning else { return }
        isRunning = true
        generation += 1
        let source = DispatchSource.makeTimerSource(queue: timerQueue)
        source.schedule(deadline: .now() + interval, repeating: interval, leeway: .milliseconds(50))
        source.setEventHandler { [weak self] in self?.tick() }
        timer = source
        source.resume()
    }

    /// Stops monitoring; an in-flight ping is discarded when it arrives.
    func stop() {
        lock.lock()
        defer { lock.unlock() }
        isRunning = false
        generation += 1
        timer?.cancel()
        timer = nil
    }

    // MARK: Internal (exposed for tests)

    /// Marks the heartbeat as running without a timer, for deterministic tests.
    func startForTesting() {
        lock.lock()
        defer { lock.unlock() }
        isRunning = diagnostics.isEnabled
        generation += 1
    }

    func tick() {
        lock.lock()
        guard isRunning, !pingInFlight else {
            lock.unlock()
            return
        }
        pingInFlight = true
        pingSentAt = uptimeNanoseconds()
        let pingGeneration = generation
        lock.unlock()

        schedulePing { [weak self] in self?.pong(generation: pingGeneration) }
    }

    private func pong(generation pingGeneration: Int) {
        let receivedAt = uptimeNanoseconds()
        lock.lock()
        pingInFlight = false
        let sentAt = pingSentAt
        let valid = isRunning && pingGeneration == generation
        lock.unlock()
        guard valid else { return }

        let waitedMs = NightlyDiagnostics.elapsedMilliseconds(since: sentAt, now: receivedAt)
        if waitedMs > Self.stallThresholdMs {
            diagnostics.record(.uiHeartbeat, .stall, durationMs: waitedMs)
        }
    }
}
