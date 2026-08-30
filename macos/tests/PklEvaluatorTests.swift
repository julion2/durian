import Darwin
@testable import durian_lib
import XCTest

final class PklEvaluatorTests: XCTestCase {
    func testResolverDoesNotRunFallbackDuringSynchronousInitialization() async throws {
        let fallbacks = LockedCounter()
        let resolver = PklExecutableResolver(
            immediateExecutable: nil,
            fallback: {
                fallbacks.increment()
                try await Task.sleep(for: .milliseconds(20))
                return nil
            },
            isExecutable: { _ in false }
        )

        XCTAssertNil(resolver.resolvedExecutable)
        XCTAssertEqual(fallbacks.value, 0)

        async let first = resolver.resolve()
        async let second = resolver.resolve()
        let values = try await (first, second)

        XCTAssertNil(values.0)
        XCTAssertNil(values.1)
        XCTAssertEqual(fallbacks.value, 1)
    }

    func testResolverCachesAndPersistsAsynchronouslyResolvedExecutable() async throws {
        let persisted = LockedPaths()
        let resolver = PklExecutableResolver(
            immediateExecutable: nil,
            fallback: { "/bin/sh" },
            persist: { persisted.append($0) }
        )

        XCTAssertNil(resolver.resolvedExecutable)
        let resolved = try await resolver.resolve()
        XCTAssertEqual(resolved, "/bin/sh")
        XCTAssertEqual(resolver.resolvedExecutable, "/bin/sh")
        XCTAssertEqual(persisted.values, ["/bin/sh"])
    }

    func testResolverCancellationOnlyRemovesCanceledWaiter() async throws {
        let fallback = ResolverFallbackHarness()
        let resolver = PklExecutableResolver(
            immediateExecutable: nil,
            fallback: { try await fallback.run() }
        )
        let firstCompleted = LockedCounter()
        let first = Task {
            defer { firstCompleted.increment() }
            return try await resolver.resolve()
        }
        let second = Task { try await resolver.resolve() }

        let bothJoined = await waitUntil { resolver.activeWaiterCount == 2 }
        XCTAssertTrue(bothJoined)
        first.cancel()
        let canceledPromptly = await waitUntil { firstCompleted.value == 1 }
        XCTAssertTrue(canceledPromptly)

        fallback.release(generation: 1)
        do {
            _ = try await first.value
            XCTFail("Expected cancellation")
        } catch is CancellationError {
            // Expected.
        }
        let secondValue = try await second.value
        XCTAssertEqual(secondValue, "/bin/sh")
        XCTAssertEqual(fallback.launchCount, 1)
    }

    func testResolverConcurrentSuccessPersistsExactlyOnce() async throws {
        let fallback = ResolverFallbackHarness()
        let persisted = LockedPaths()
        let resolver = PklExecutableResolver(
            immediateExecutable: nil,
            fallback: { try await fallback.run() },
            persist: { persisted.append($0) }
        )
        let tasks = (0 ..< 8).map { _ in
            Task { try await resolver.resolve() }
        }

        let allJoined = await waitUntil { resolver.activeWaiterCount == tasks.count }
        XCTAssertTrue(allJoined)
        fallback.release(generation: 1)

        for task in tasks {
            let value = try await task.value
            XCTAssertEqual(value, "/bin/sh")
        }
        let cached = try await resolver.resolve()
        XCTAssertEqual(cached, "/bin/sh")
        XCTAssertEqual(fallback.launchCount, 1)
        XCTAssertEqual(persisted.values, ["/bin/sh"])
    }

    func testResolverAllWaitersCancelInvalidatesStaleFallback() async throws {
        let fallback = ResolverFallbackHarness()
        let persisted = LockedPaths()
        let resolver = PklExecutableResolver(
            immediateExecutable: nil,
            fallback: { try await fallback.run(ignoringCancellation: true) },
            persist: { persisted.append($0) }
        )
        let first = Task { try await resolver.resolve() }
        let second = Task { try await resolver.resolve() }

        let bothJoined = await waitUntil { resolver.activeWaiterCount == 2 }
        XCTAssertTrue(bothJoined)
        first.cancel()
        second.cancel()
        for task in [first, second] {
            do {
                _ = try await task.value
                XCTFail("Expected cancellation")
            } catch is CancellationError {
                // Expected.
            }
        }
        XCTAssertEqual(resolver.activeWaiterCount, 0)
        let fallbackCanceled = await waitUntil { fallback.cancellationCount == 1 }
        XCTAssertTrue(fallbackCanceled)

        fallback.release(generation: 1)
        let staleCompleted = await waitUntil { fallback.completionCount == 1 }
        XCTAssertTrue(staleCompleted)
        XCTAssertNil(resolver.resolvedExecutable)
        XCTAssertTrue(persisted.values.isEmpty)

        let fresh = Task { try await resolver.resolve() }
        let freshStarted = await waitUntil {
            fallback.launchCount == 2 && resolver.activeWaiterCount == 1
        }
        XCTAssertTrue(freshStarted)
        fallback.release(generation: 2)

        let freshValue = try await fresh.value
        XCTAssertEqual(freshValue, "/bin/sh")
        XCTAssertEqual(persisted.values, ["/bin/sh"])
    }

    func testProcessTimeoutTerminatesChild() async throws {
        let pid = LockedPID()

        do {
            _ = try await PklProcessRunner.run(
                executableURL: URL(fileURLWithPath: "/bin/sleep"),
                arguments: ["30"],
                timeout: 0.05,
                onLaunch: { pid.set($0) }
            )
            XCTFail("Expected timeout")
        } catch let error as PklEvalError {
            guard case .timedOut = error else {
                return XCTFail("Unexpected Pkl error: \(error)")
            }
        }

        let launchedPID = try await pid.waitUntilSet()
        let processExited = await waitUntilProcessExits(launchedPID)
        XCTAssertTrue(processExited)
    }

    func testProcessTimeoutForceKillsTermIgnoringChild() async throws {
        let pid = LockedPID()

        do {
            _ = try await PklProcessRunner.run(
                executableURL: URL(fileURLWithPath: "/bin/sh"),
                arguments: ["-c", "trap '' TERM; while :; do :; done"],
                timeout: 0.2,
                terminationGracePeriod: 0.25,
                onLaunch: { pid.set($0) }
            )
            XCTFail("Expected timeout")
        } catch let error as PklEvalError {
            guard case .timedOut = error else {
                return XCTFail("Unexpected Pkl error: \(error)")
            }
        }

        let launchedPID = try await pid.waitUntilSet()
        XCTAssertEqual(kill(launchedPID, 0), 0, "Child should survive SIGTERM until SIGKILL escalation")
        let processExited = await waitUntilProcessExits(launchedPID)
        XCTAssertTrue(processExited)
    }

    func testLoginShellFallbackIsTimeoutBounded() async throws {
        let fixture = try ExecutableScript("exec /bin/sleep 30")
        defer { fixture.remove() }
        let pid = LockedPID()

        do {
            _ = try await PklEvaluator.probeLoginShell(
                "pkl",
                shellURL: fixture.url,
                timeout: 0.05,
                onLaunch: { pid.set($0) }
            )
            XCTFail("Expected timeout")
        } catch let error as PklEvalError {
            guard case .timedOut = error else {
                return XCTFail("Unexpected Pkl error: \(error)")
            }
        }

        let launchedPID = try await pid.waitUntilSet()
        let processExited = await waitUntilProcessExits(launchedPID)
        XCTAssertTrue(processExited)
    }

    func testLoginShellFallbackFindsCustomExecutable() async throws {
        let fixture = try ExecutableScript("printf '/bin/sh\\n'")
        defer { fixture.remove() }

        let executable = try await PklEvaluator.probeLoginShell(
            "pkl",
            shellURL: fixture.url,
            timeout: 1
        )

        XCTAssertEqual(executable, "/bin/sh")
    }

    func testLoginShellFallbackCancellationTerminatesChild() async throws {
        let fixture = try ExecutableScript("exec /bin/sleep 30")
        defer { fixture.remove() }
        let pid = LockedPID()
        let task = Task {
            try await PklEvaluator.probeLoginShell(
                "pkl",
                shellURL: fixture.url,
                timeout: 10,
                onLaunch: { pid.set($0) }
            )
        }
        let launchedPID = try await pid.waitUntilSet()
        task.cancel()

        do {
            _ = try await task.value
            XCTFail("Expected cancellation")
        } catch is CancellationError {
            // Expected.
        }
        let processExited = await waitUntilProcessExits(launchedPID)
        XCTAssertTrue(processExited)
    }

    func testTaskCancellationTerminatesChild() async throws {
        let pid = LockedPID()
        let task = Task {
            try await PklProcessRunner.run(
                executableURL: URL(fileURLWithPath: "/bin/sleep"),
                arguments: ["30"],
                timeout: 10,
                onLaunch: { pid.set($0) }
            )
        }
        let launchedPID = try await pid.waitUntilSet()
        task.cancel()

        do {
            _ = try await task.value
            XCTFail("Expected cancellation")
        } catch is CancellationError {
            // Expected.
        }
        let processExited = await waitUntilProcessExits(launchedPID)
        XCTAssertTrue(processExited)
    }

    func testTerminationTimeoutRaceCompletesEveryTaskOnce() async {
        let completions = LockedCounter()

        await withTaskGroup(of: Void.self) { group in
            for _ in 0 ..< 12 {
                group.addTask {
                    _ = try? await PklProcessRunner.run(
                        executableURL: URL(fileURLWithPath: "/bin/sleep"),
                        arguments: ["0.01"],
                        timeout: 0.01
                    )
                    completions.increment()
                }
            }
        }

        XCTAssertEqual(completions.value, 12)
    }

    func testProcessFailureIncludesStderr() async {
        do {
            _ = try await PklProcessRunner.run(
                executableURL: URL(fileURLWithPath: "/bin/sh"),
                arguments: ["-c", "echo syntax exploded >&2; exit 7"],
                timeout: 1
            )
            XCTFail("Expected process failure")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("syntax exploded"))
        }
    }

    func testActualPklBatchUsesModuleNameOutputs() async throws {
        try requireActualPkl()
        let fixture = try PklFixture()
        defer { fixture.remove() }

        let configuration = try await PklEvaluator.evalStartup(moduleURLs: fixture.modules)

        XCTAssertEqual(configuration.config.settings.theme, "dark")
        XCTAssertEqual(configuration.profiles.profiles.first?.name, "Work")
        XCTAssertTrue(configuration.keymaps.keymaps.isEmpty)
    }

    func testActualPklAnalyzeImportsResolvesLocalImport() async throws {
        try requireActualPkl()
        let fixture = try PklFixture()
        defer { fixture.remove() }

        let graph = try await PklEvaluator.analyzeImports(moduleURLs: fixture.modules)
        let configSource = fixture.modules[0].standardizedFileURL.absoluteString
        let importedSource = fixture.importedModule.standardizedFileURL.absoluteString
        let sources = Set((fixture.modules + [fixture.importedModule]).map {
            $0.standardizedFileURL.absoluteString
        })

        XCTAssertEqual(Set(graph.imports.keys), sources)
        XCTAssertEqual(Set(graph.resolvedImports.keys), sources)
        XCTAssertTrue(graph.resolvedImports.values.allSatisfy { URL(string: $0)?.isFileURL == true })
        XCTAssertTrue(graph.imports[configSource]?.contains { $0.uri == importedSource } == true)
        let resolvedURI = try XCTUnwrap(graph.resolvedImports[importedSource])
        let resolvedImport = try XCTUnwrap(URL(string: resolvedURI))
        XCTAssertEqual(
            resolvedImport.resolvingSymlinksInPath().standardizedFileURL.path,
            fixture.importedModule.resolvingSymlinksInPath().standardizedFileURL.path
        )
    }

    func testEvaluatorIdentityUsesResolvedExecutableMetadata() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let executable = root.appendingPathComponent("pkl-0.31.1")
        try Data("fixture".utf8).write(to: executable)
        XCTAssertEqual(chmod(executable.path, 0o700), 0)
        try FileManager.default.setAttributes(
            [.modificationDate: Date(timeIntervalSince1970: 1_700_000_000)],
            ofItemAtPath: executable.path
        )

        let symlink = root.appendingPathComponent("pkl")
        try FileManager.default.createSymbolicLink(at: symlink, withDestinationURL: executable)
        let identity = try XCTUnwrap(PklEvaluator.evaluatorIdentity(forExecutableAt: symlink.path))
        let resolvedPath = executable.resolvingSymlinksInPath().standardizedFileURL.path

        XCTAssertTrue(identity.hasPrefix("\(resolvedPath):"))

        let handle = try FileHandle(forWritingTo: executable)
        try handle.seekToEnd()
        try handle.write(contentsOf: Data("-changed".utf8))
        try handle.close()

        XCTAssertNotEqual(PklEvaluator.evaluatorIdentity(forExecutableAt: symlink.path), identity)
    }

    func testActualPklSyntaxErrorSurfacesStderr() async throws {
        try requireActualPkl()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let module = root.appendingPathComponent("broken.pkl")
        try Data("value =".utf8).write(to: module)

        do {
            _ = try await PklEvaluator.evaluateJSONModules([module])
            XCTFail("Expected syntax error")
        } catch {
            XCTAssertTrue(error.localizedDescription.contains("broken.pkl"))
            XCTAssertTrue(error.localizedDescription.contains("Unexpected end of file"))
        }
    }

    private func waitUntilProcessExits(_ pid: pid_t) async -> Bool {
        for _ in 0 ..< 100 {
            if kill(pid, 0) == -1, errno == ESRCH {
                return true
            }
            try? await Task.sleep(for: .milliseconds(10))
        }
        return false
    }

    private func waitUntil(_ condition: @escaping @Sendable () -> Bool) async -> Bool {
        for _ in 0 ..< 200 {
            if condition() { return true }
            try? await Task.sleep(for: .milliseconds(5))
        }
        return condition()
    }

    private func requireActualPkl(file: StaticString = #filePath, line: UInt = #line) throws {
        if ProcessInfo.processInfo.environment["DURIAN_REQUIRE_PKL_TESTS"] == "1" {
            XCTAssertNotNil(PklEvaluator.resolvedPkl, "CI must install Pkl", file: file, line: line)
            return
        }
        try XCTSkipUnless(PklEvaluator.resolvedPkl != nil, "pkl is not installed")
    }
}

private struct ExecutableScript {
    let root: URL
    let url: URL

    init(_ body: String) throws {
        root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        url = root.appendingPathComponent("fixture.sh")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try Data("#!/bin/sh\n\(body)\n".utf8).write(to: url)
        guard chmod(url.path, 0o700) == 0 else {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
    }

    func remove() {
        try? FileManager.default.removeItem(at: root)
    }
}

private struct PklFixture {
    let root: URL
    let modules: [URL]
    let importedModule: URL

    init() throws {
        let fixtureRoot = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        root = fixtureRoot
        try FileManager.default.createDirectory(at: fixtureRoot, withIntermediateDirectories: true)
        modules = ["config.pkl", "profiles.pkl", "keymaps.pkl"].map {
            fixtureRoot.appendingPathComponent($0)
        }
        importedModule = fixtureRoot.appendingPathComponent("shared.pkl")

        try Data("""
        import "shared.pkl"

        accounts = new Listing {}
        settings { theme = shared.theme }
        sync {}
        signatures = new Mapping {}
        """.utf8).write(to: modules[0])
        try Data("""
        profiles = new Listing {
          new {
            name = "Work"
            accounts = new Listing { "work" }
            default = true
          }
        }
        """.utf8).write(to: modules[1])
        try Data("""
        keymaps = new Listing {}
        global_settings {
          keymaps_enabled = true
          sequence_timeout = 1.0
        }
        """.utf8).write(to: modules[2])
        try Data("theme = \"dark\"".utf8).write(to: importedModule)
    }

    func remove() {
        try? FileManager.default.removeItem(at: root)
    }
}

private final class LockedPID: @unchecked Sendable {
    private let lock = NSLock()
    private var pid: pid_t?

    func set(_ pid: pid_t) {
        lock.withLock { self.pid = pid }
    }

    func waitUntilSet() async throws -> pid_t {
        for _ in 0 ..< 100 {
            if let pid = lock.withLock({ pid }) {
                return pid
            }
            try await Task.sleep(for: .milliseconds(10))
        }
        throw NSError(domain: "PklEvaluatorTests", code: 1)
    }
}

private final class LockedCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0

    var value: Int { lock.withLock { count } }

    func increment() {
        lock.withLock { count += 1 }
    }
}

private final class LockedPaths: @unchecked Sendable {
    private let lock = NSLock()
    private var paths: [String] = []

    var values: [String] { lock.withLock { paths } }

    func append(_ path: String) {
        lock.withLock { paths.append(path) }
    }
}

private final class ResolverFallbackHarness: @unchecked Sendable {
    private let lock = NSLock()
    private var launches = 0
    private var cancellations = 0
    private var completions = 0
    private var releasedGenerations: Set<Int> = []

    var launchCount: Int { lock.withLock { launches } }
    var cancellationCount: Int { lock.withLock { cancellations } }
    var completionCount: Int { lock.withLock { completions } }

    func run(ignoringCancellation: Bool = false) async throws -> String? {
        let generation = lock.withLock {
            launches += 1
            return launches
        }
        var recordedCancellation = false

        while !lock.withLock({ releasedGenerations.contains(generation) }) {
            do {
                try await Task.sleep(for: .milliseconds(5))
            } catch is CancellationError {
                if !recordedCancellation {
                    lock.withLock { cancellations += 1 }
                    recordedCancellation = true
                }
                guard ignoringCancellation else { throw CancellationError() }
                usleep(1_000)
            }
        }

        lock.withLock { completions += 1 }
        return "/bin/sh"
    }

    func release(generation: Int) {
        _ = lock.withLock { releasedGenerations.insert(generation) }
    }
}
