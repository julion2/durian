//
//  PklEvaluator.swift
//  Durian
//
//  Evaluates .pkl config files asynchronously through the Pkl CLI.
//  Schemas are bundled as app resources and served via modulePaths.
//

import Foundation

struct PklStartupConfiguration: Codable {
    let config: AppConfig
    let profiles: ProfilesConfig
    let keymaps: KeymapConfig
}

struct PklImportGraph: Decodable {
    struct ImportedModule: Decodable {
        let uri: String
    }

    let imports: [String: [ImportedModule]]
    let resolvedImports: [String: String]
}

enum PklEvaluator {
    private static let pklPathDefaultsKey = "PklExecPath"
    static let processTimeout: TimeInterval = 10
    // `pkl analyze imports` evaluates an internal repl: module. Using the same
    // allowlist for eval and analyze keeps project/import resolution aligned.
    static let allowedModules = "repl:,file:,modulepath:"

    /// Startup may read this property while constructing the first window, so
    /// its initial resolution is deliberately limited to filesystem lookups.
    /// The login-shell fallback runs asynchronously on first evaluation.
    private static let executableResolver = PklExecutableResolver(
        immediateExecutable: resolvePklWithoutShell(),
        fallback: { try await probeLoginShell("pkl") },
        persist: { persist($0) }
    )

    static var resolvedPkl: String? { executableResolver.resolvedExecutable }

    private static func resolvePklWithoutShell() -> String? {
        let fm = FileManager.default

        if let env = ProcessInfo.processInfo.environment["PKL_EXEC"],
           fm.isExecutableFile(atPath: env)
        {
            return env
        }

        if let saved = UserDefaults.standard.string(forKey: pklPathDefaultsKey),
           fm.isExecutableFile(atPath: saved)
        {
            persist(saved)
            return saved
        }

        let user = NSUserName()
        let home = NSHomeDirectory()
        let candidates = [
            "/opt/homebrew/bin/pkl",
            "/usr/local/bin/pkl",
            "/etc/profiles/per-user/\(user)/bin/pkl",
            "\(home)/.nix-profile/bin/pkl",
            "/run/current-system/sw/bin/pkl",
            "\(home)/.local/bin/pkl",
        ]
        if let hit = candidates.first(where: { fm.isExecutableFile(atPath: $0) }) {
            persist(hit)
            return hit
        }

        if let hit = findInPath("pkl") {
            persist(hit)
            return hit
        }
        return nil
    }

    private static func persist(_ path: String) {
        setenv("PKL_EXEC", path, 1)
        UserDefaults.standard.set(path, forKey: pklPathDefaultsKey)
    }

    private static func findInPath(_ name: String) -> String? {
        let paths = (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":")
        for path in paths {
            let full = "\(path)/\(name)"
            if FileManager.default.isExecutableFile(atPath: full) {
                return full
            }
        }
        return nil
    }

    static func probeLoginShell(
        _ name: String,
        shellURL: URL? = nil,
        timeout: TimeInterval = processTimeout,
        onLaunch: (@Sendable (pid_t) -> Void)? = nil
    ) async throws -> String? {
        let shellURL = shellURL ?? URL(
            fileURLWithPath: ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        )
        let output = try await PklProcessRunner.run(
            executableURL: shellURL,
            arguments: ["-lc", "command -v \(name)"],
            timeout: timeout,
            onLaunch: onLaunch
        )
        let path = output.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !path.isEmpty, FileManager.default.isExecutableFile(atPath: path) else { return nil }
        return path
    }

    static let schemaDirectoryURL: URL? = {
        guard let resourcePath = Bundle.main.resourcePath else { return nil }
        for candidate in [
            (resourcePath as NSString).appendingPathComponent("schema"),
            resourcePath,
        ] {
            let test = (candidate as NSString).appendingPathComponent("Config.pkl")
            if FileManager.default.fileExists(atPath: test) {
                return URL(fileURLWithPath: candidate, isDirectory: true)
            }
        }
        return nil
    }()

    static var startupModuleURLs: [URL] {
        let configDirectory = FileManager.default.durianConfigURL()
        return ["config.pkl", "profiles.pkl", "keymaps.pkl"].map {
            configDirectory.appendingPathComponent($0)
        }
    }

    static func eval<T: Decodable>(_ type: T.Type, from url: URL) async throws -> T {
        let outputs = try await evaluateJSONModules([url])
        guard let data = outputs[url.deletingPathExtension().lastPathComponent] else {
            throw PklEvalError.evaluationFailed("pkl produced no output for \(url.lastPathComponent)")
        }
        return try JSONDecoder().decode(type, from: data)
    }

    /// Evaluate all startup modules in one Pkl process. Multiple module output
    /// is written by `%{moduleName}`, which is covered by an actual-Pkl test.
    static func evalStartup(moduleURLs: [URL] = startupModuleURLs) async throws -> PklStartupConfiguration {
        guard moduleURLs.count == 3 else {
            throw PklEvalError.evaluationFailed("expected config, profiles, and keymaps modules")
        }

        let outputs = try await evaluateJSONModules(moduleURLs)
        let decoder = JSONDecoder()
        guard let configData = outputs["config"],
              let profilesData = outputs["profiles"],
              let keymapsData = outputs["keymaps"]
        else {
            throw PklEvalError.evaluationFailed("pkl did not produce all startup outputs")
        }

        return try PklStartupConfiguration(
            config: decoder.decode(AppConfig.self, from: configData),
            profiles: decoder.decode(ProfilesConfig.self, from: profilesData),
            keymaps: decoder.decode(KeymapConfig.self, from: keymapsData)
        )
    }

    static func analyzeImports(moduleURLs: [URL] = startupModuleURLs) async throws -> PklImportGraph {
        let outputDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent("durian-pkl-imports-\(UUID().uuidString)", isDirectory: true)
        let outputURL = outputDirectory.appendingPathComponent("imports.json")
        try FileManager.default.createDirectory(at: outputDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: outputDirectory) }

        var arguments = [
            "analyze", "imports",
            "--format", "json",
            "--allowed-modules", allowedModules,
            "--timeout", "9",
            "--output-path", outputURL.path,
        ]
        if let schemaDirectoryURL {
            arguments += ["--module-path", schemaDirectoryURL.path]
        }
        arguments += moduleURLs.map(\.path)

        try await runPkl(arguments: arguments)
        return try JSONDecoder().decode(PklImportGraph.self, from: Data(contentsOf: outputURL))
    }

    static func evaluateJSONModules(_ moduleURLs: [URL]) async throws -> [String: Data] {
        let outputDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent("durian-pkl-eval-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: outputDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: outputDirectory) }

        var arguments = [
            "eval",
            "--format", "json",
            "--allowed-modules", allowedModules,
            "--timeout", "9",
            "--output-path", outputDirectory.appendingPathComponent("%{moduleName}.json").path,
        ]
        if let schemaDirectoryURL {
            arguments += ["--module-path", schemaDirectoryURL.path]
        }
        arguments += moduleURLs.map(\.path)

        try await runPkl(arguments: arguments)

        var outputs: [String: Data] = [:]
        for url in moduleURLs {
            let name = url.deletingPathExtension().lastPathComponent
            outputs[name] = try Data(contentsOf: outputDirectory.appendingPathComponent("\(name).json"))
        }
        return outputs
    }

    static func runPkl(arguments: [String], timeout: TimeInterval = processTimeout) async throws {
        guard let pklBinary = try await executableResolver.resolve() else {
            Log.error("CONFIG", "pkl binary not found in PKL_EXEC, common paths, PATH, or login shell")
            throw PklEvalError.binaryNotFound
        }
        _ = try await PklProcessRunner.run(
            executableURL: URL(fileURLWithPath: pklBinary),
            arguments: arguments,
            timeout: timeout
        )
    }
}

/// Joins concurrent asynchronous fallback lookups while keeping startup-safe
/// filesystem resolution synchronously observable for snapshot validation.
final class PklExecutableResolver: @unchecked Sendable {
    typealias Fallback = @Sendable () async throws -> String?
    typealias IsExecutable = @Sendable (String) -> Bool
    typealias Persist = @Sendable (String) -> Void

    private let lock = NSLock()
    private let fallback: Fallback
    private let isExecutable: IsExecutable
    private let persist: Persist
    private var executable: String?
    private var fallbackLookup: FallbackLookup?
    private var fallbackFinished = false

    private struct FallbackLookup {
        let id: UUID
        var task: Task<Void, Never>?
        var waiters: [UUID: PklResolverWaiter]
    }

    init(
        immediateExecutable: String?,
        fallback: @escaping Fallback,
        isExecutable: @escaping IsExecutable = FileManager.default.isExecutableFile(atPath:),
        persist: @escaping Persist = { _ in }
    ) {
        executable = immediateExecutable
        self.fallback = fallback
        self.isExecutable = isExecutable
        self.persist = persist
    }

    var resolvedExecutable: String? {
        lock.withLock { executable }
    }

    var activeWaiterCount: Int {
        lock.withLock { fallbackLookup?.waiters.count ?? 0 }
    }

    func resolve() async throws -> String? {
        try await PklResolverWaiter().wait(on: self)
    }

    fileprivate func register(_ waiter: PklResolverWaiter) -> PklResolverRegistration {
        var lookupToStart: UUID?
        let registration: PklResolverRegistration

        lock.lock()
        if let executable {
            registration = .resolved(executable)
        } else if fallbackFinished {
            registration = .resolved(nil)
        } else if var lookup = fallbackLookup {
            lookup.waiters[waiter.id] = waiter
            fallbackLookup = lookup
            registration = .joined
        } else {
            let id = UUID()
            fallbackLookup = FallbackLookup(
                id: id,
                task: nil,
                waiters: [waiter.id: waiter]
            )
            lookupToStart = id
            registration = .joined
        }
        lock.unlock()

        if let lookupToStart {
            startFallback(id: lookupToStart)
        }
        return registration
    }

    fileprivate func cancel(waiterID: UUID) {
        var taskToCancel: Task<Void, Never>?

        lock.lock()
        if var lookup = fallbackLookup, lookup.waiters.removeValue(forKey: waiterID) != nil {
            if lookup.waiters.isEmpty {
                // Invalidate before cancellation so a non-cooperative stale
                // fallback cannot cache or persist its eventual result.
                fallbackLookup = nil
                taskToCancel = lookup.task
            } else {
                fallbackLookup = lookup
            }
        }
        lock.unlock()

        taskToCancel?.cancel()
    }

    private func startFallback(id: UUID) {
        let fallback = fallback
        let task = Task.detached { [weak self] in
            do {
                let candidate = try await fallback()
                self?.completeFallback(id: id, result: .success(candidate))
            } catch {
                self?.completeFallback(id: id, result: .failure(error))
            }
        }

        var cancelStaleTask = false
        lock.lock()
        if var lookup = fallbackLookup, lookup.id == id {
            lookup.task = task
            fallbackLookup = lookup
        } else {
            cancelStaleTask = true
        }
        lock.unlock()

        if cancelStaleTask {
            task.cancel()
        }
    }

    private func completeFallback(id: UUID, result: Result<String?, Error>) {
        let resolved: String?
        let waiterResult: Result<String?, Error>
        let isTerminal: Bool

        switch result {
        case .success(let candidate):
            resolved = candidate.flatMap { isExecutable($0) ? $0 : nil }
            waiterResult = .success(resolved)
            isTerminal = true
        case .failure(let error) where error is CancellationError:
            resolved = nil
            waiterResult = .failure(CancellationError())
            isTerminal = false
        case .failure:
            // Preserve the existing shell-probe fallback behavior: a failed
            // lookup resolves to no executable rather than surfacing its error.
            resolved = nil
            waiterResult = .success(nil)
            isTerminal = true
        }

        let waiters: [PklResolverWaiter]
        lock.lock()
        guard let lookup = fallbackLookup, lookup.id == id else {
            lock.unlock()
            return
        }
        waiters = Array(lookup.waiters.values)
        fallbackLookup = nil
        if isTerminal {
            executable = resolved
            fallbackFinished = true
        }
        lock.unlock()

        if let resolved, isTerminal {
            persist(resolved)
        }
        waiters.forEach { $0.resume(with: waiterResult) }
    }
}

fileprivate enum PklResolverRegistration {
    case joined
    case resolved(String?)
}

/// Owns one caller's continuation so cancellation can race safely with the
/// resolver-owned shared fallback completion.
fileprivate final class PklResolverWaiter: @unchecked Sendable {
    let id = UUID()

    private let lock = NSLock()
    private var continuation: CheckedContinuation<String?, Error>?
    private var isCancelled = false
    private var isFinished = false

    func wait(on resolver: PklExecutableResolver) async throws -> String? {
        do {
            return try await withTaskCancellationHandler {
                try Task.checkCancellation()
                return try await withCheckedThrowingContinuation { continuation in
                    install(continuation, on: resolver)
                }
            } onCancel: {
                self.cancel(on: resolver)
            }
        } catch is CancellationError {
            throw CancellationError()
        }
    }

    private func install(
        _ continuation: CheckedContinuation<String?, Error>,
        on resolver: PklExecutableResolver
    ) {
        lock.lock()
        self.continuation = continuation
        if isCancelled {
            isFinished = true
            self.continuation = nil
            lock.unlock()
            continuation.resume(throwing: CancellationError())
            return
        }

        let registration = resolver.register(self)
        lock.unlock()

        if case .resolved(let executable) = registration {
            resume(with: .success(executable))
        }
    }

    private func cancel(on resolver: PklExecutableResolver) {
        lock.lock()
        guard !isFinished else {
            lock.unlock()
            return
        }
        isCancelled = true
        lock.unlock()

        resolver.cancel(waiterID: id)
        resume(with: .failure(CancellationError()))
    }

    func resume(with result: Result<String?, Error>) {
        lock.lock()
        guard !isFinished, let continuation else {
            lock.unlock()
            return
        }
        isFinished = true
        self.continuation = nil
        lock.unlock()
        continuation.resume(with: result)
    }
}

enum PklProcessRunner {
    /// Runs a child process without blocking an executor. Timeout, cancellation,
    /// launch failure, and process termination race through one lock-guarded
    /// completion owner, so the continuation can only be resumed once.
    static func run(
        executableURL: URL,
        arguments: [String],
        timeout: TimeInterval,
        terminationGracePeriod: TimeInterval = 0.1,
        onLaunch: (@Sendable (pid_t) -> Void)? = nil
    ) async throws -> String {
        let state = PklProcessState()
        return try await withTaskCancellationHandler {
            try Task.checkCancellation()
            return try await withCheckedThrowingContinuation { continuation in
                let processDirectory = FileManager.default.temporaryDirectory
                    .appendingPathComponent("durian-pkl-process-\(UUID().uuidString)", isDirectory: true)
                let outputURL = processDirectory.appendingPathComponent("stdout")
                let errorURL = processDirectory.appendingPathComponent("stderr")
                let outputHandle: FileHandle
                let errorHandle: FileHandle

                do {
                    try FileManager.default.createDirectory(at: processDirectory, withIntermediateDirectories: true)
                    guard FileManager.default.createFile(atPath: outputURL.path, contents: nil),
                          FileManager.default.createFile(atPath: errorURL.path, contents: nil)
                    else {
                        throw CocoaError(.fileWriteUnknown)
                    }
                    outputHandle = try FileHandle(forWritingTo: outputURL)
                    errorHandle = try FileHandle(forWritingTo: errorURL)
                } catch {
                    try? FileManager.default.removeItem(at: processDirectory)
                    continuation.resume(throwing: error)
                    return
                }

                let process = Process()
                process.executableURL = executableURL
                process.arguments = arguments
                process.standardOutput = outputHandle
                process.standardError = errorHandle

                guard state.install(
                    continuation: continuation,
                    process: process,
                    outputHandle: outputHandle,
                    outputURL: outputURL,
                    errorHandle: errorHandle,
                    errorURL: errorURL,
                    processDirectory: processDirectory,
                    terminationGracePeriod: terminationGracePeriod
                ) else {
                    return
                }

                process.terminationHandler = { process in
                    state.processDidTerminate(status: process.terminationStatus)
                }

                do {
                    guard try state.runProcess() else { return }
                    onLaunch?(process.processIdentifier)
                    state.scheduleTimeout(seconds: timeout)
                } catch {
                    state.fail(error)
                }
            }
        } onCancel: {
            state.fail(CancellationError(), terminateProcess: true)
        }
    }
}

private final class PklProcessState: @unchecked Sendable {
    private let lock = NSLock()
    private var isFinished = false
    private var earlyError: Error?
    private var continuation: CheckedContinuation<String, Error>?
    private var process: Process?
    private var outputHandle: FileHandle?
    private var outputURL: URL?
    private var errorHandle: FileHandle?
    private var errorURL: URL?
    private var processDirectory: URL?
    private var terminationGracePeriod: TimeInterval = 0.1
    private var timeoutTask: Task<Void, Never>?

    func install(
        continuation: CheckedContinuation<String, Error>,
        process: Process,
        outputHandle: FileHandle,
        outputURL: URL,
        errorHandle: FileHandle,
        errorURL: URL,
        processDirectory: URL,
        terminationGracePeriod: TimeInterval
    ) -> Bool {
        lock.lock()
        if isFinished {
            let error = earlyError ?? CancellationError()
            lock.unlock()
            try? outputHandle.close()
            try? errorHandle.close()
            try? FileManager.default.removeItem(at: processDirectory)
            continuation.resume(throwing: error)
            return false
        }
        self.continuation = continuation
        self.process = process
        self.outputHandle = outputHandle
        self.outputURL = outputURL
        self.errorHandle = errorHandle
        self.errorURL = errorURL
        self.processDirectory = processDirectory
        self.terminationGracePeriod = terminationGracePeriod
        lock.unlock()
        return true
    }

    /// Process.run() is serialized with cancellation so cancellation cannot
    /// observe a not-yet-running process and then allow it to launch afterward.
    func runProcess() throws -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard !isFinished, let process else { return false }
        try process.run()
        return true
    }

    func scheduleTimeout(seconds: TimeInterval) {
        let nanoseconds = UInt64(max(0, seconds) * 1_000_000_000)
        let task = Task { [weak self] in
            try? await Task.sleep(nanoseconds: nanoseconds)
            self?.fail(PklEvalError.timedOut(seconds), terminateProcess: true)
        }

        lock.lock()
        if isFinished {
            lock.unlock()
            task.cancel()
        } else {
            timeoutTask = task
            lock.unlock()
        }
    }

    func processDidTerminate(status: Int32) {
        guard let completion = claimCompletion() else { return }
        completion.resumeForTermination(status: status)
    }

    func fail(_ error: Error, terminateProcess: Bool = false) {
        lock.lock()
        guard !isFinished else {
            lock.unlock()
            return
        }
        guard continuation != nil else {
            isFinished = true
            earlyError = error
            lock.unlock()
            return
        }
        let completion = takeCompletionLocked()
        lock.unlock()

        if terminateProcess {
            completion.terminateProcess()
        }
        completion.resume(throwing: error)
    }

    private func claimCompletion() -> ProcessCompletion? {
        lock.lock()
        defer { lock.unlock() }
        guard !isFinished, continuation != nil else { return nil }
        return takeCompletionLocked()
    }

    private func takeCompletionLocked() -> ProcessCompletion {
        isFinished = true
        let completion = ProcessCompletion(
            continuation: continuation!,
            process: process,
            outputHandle: outputHandle,
            outputURL: outputURL,
            errorHandle: errorHandle,
            errorURL: errorURL,
            processDirectory: processDirectory,
            terminationGracePeriod: terminationGracePeriod,
            timeoutTask: timeoutTask
        )
        continuation = nil
        process = nil
        outputHandle = nil
        outputURL = nil
        errorHandle = nil
        errorURL = nil
        processDirectory = nil
        timeoutTask = nil
        return completion
    }
}

private struct ProcessCompletion: @unchecked Sendable {
    let continuation: CheckedContinuation<String, Error>
    let process: Process?
    let outputHandle: FileHandle?
    let outputURL: URL?
    let errorHandle: FileHandle?
    let errorURL: URL?
    let processDirectory: URL?
    let terminationGracePeriod: TimeInterval
    let timeoutTask: Task<Void, Never>?

    func terminateProcess() {
        guard let process, process.isRunning else { return }
        process.terminate()

        // Some children ignore SIGTERM. Retain and re-check the same Process
        // object after a short grace period before escalating to SIGKILL.
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + terminationGracePeriod) {
            guard process.isRunning else { return }
            _ = Darwin.kill(process.processIdentifier, SIGKILL)
        }
    }

    func readOutputs() -> (output: String, error: String) {
        timeoutTask?.cancel()
        try? outputHandle?.close()
        try? errorHandle?.close()
        defer {
            if let processDirectory {
                try? FileManager.default.removeItem(at: processDirectory)
            }
        }
        let output = outputURL.flatMap { try? String(contentsOf: $0, encoding: .utf8) } ?? ""
        let error = errorURL.flatMap { try? String(contentsOf: $0, encoding: .utf8) } ?? ""
        return (
            output.trimmingCharacters(in: .whitespacesAndNewlines),
            error.trimmingCharacters(in: .whitespacesAndNewlines)
        )
    }

    func resumeForTermination(status: Int32) {
        let output = readOutputs()
        if status == 0 {
            continuation.resume(returning: output.output)
        } else {
            continuation.resume(throwing: PklEvalError.evaluationFailed(
                output.error.isEmpty ? "pkl exited with status \(status)" : output.error
            ))
        }
    }

    func resume(throwing error: Error) {
        _ = readOutputs()
        continuation.resume(throwing: error)
    }
}

enum PklEvalError: LocalizedError {
    case evaluationFailed(String)
    case binaryNotFound
    case timedOut(TimeInterval)

    var errorDescription: String? {
        switch self {
        case .evaluationFailed(let message):
            return "pkl eval failed: \(message)"
        case .binaryNotFound:
            return "pkl binary not found. Install pkl (brew install pkl) or set PKL_EXEC; profiles and config are unavailable until then."
        case .timedOut(let seconds):
            return "pkl evaluation timed out after \(Int(seconds)) seconds"
        }
    }
}
