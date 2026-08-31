import CryptoKit
import Darwin
import Foundation

/// Last-known-good evaluated startup configuration. A snapshot is accepted
/// only when all local modules in Pkl's import graph, bundled schema contents,
/// evaluator identity, and the app cache format still match.
struct PklStartupSnapshot: @unchecked Sendable {
    static let formatVersion = 2

    struct Dependency: Codable, Equatable {
        let sourceURI: String
        let resolvedPath: String
        let contentHash: String
    }

    private struct Payload: Codable {
        let formatVersion: Int
        let appIdentity: String
        let evaluatorIdentity: String
        let schemaIdentity: String
        let dependencies: [Dependency]
        let configuration: PklStartupConfiguration
    }

    typealias Analyze = @Sendable ([URL]) async throws -> PklImportGraph
    typealias Evaluate = @Sendable ([URL]) async throws -> PklStartupConfiguration
    typealias EvaluatorIdentity = @Sendable () -> String?

    let snapshotURL: URL
    let moduleURLs: [URL]
    let schemaDirectoryURL: URL?
    let appIdentity: String
    private let evaluatorIdentity: EvaluatorIdentity
    private let analyze: Analyze
    private let evaluate: Evaluate

    init(
        snapshotURL: URL = PklStartupSnapshot.defaultSnapshotURL,
        moduleURLs: [URL] = PklEvaluator.startupModuleURLs,
        schemaDirectoryURL: URL? = PklEvaluator.schemaDirectoryURL,
        appIdentity: String = PklStartupSnapshot.defaultAppIdentity,
        evaluatorIdentity: @escaping EvaluatorIdentity = { PklEvaluator.evaluatorIdentity },
        analyze: @escaping Analyze = { try await PklEvaluator.analyzeImports(moduleURLs: $0) },
        evaluate: @escaping Evaluate = { try await PklEvaluator.evalStartup(moduleURLs: $0) }
    ) {
        self.snapshotURL = snapshotURL
        self.moduleURLs = moduleURLs
        self.schemaDirectoryURL = schemaDirectoryURL
        self.appIdentity = appIdentity
        self.evaluatorIdentity = evaluatorIdentity
        self.analyze = analyze
        self.evaluate = evaluate
    }

    func load() -> PklStartupConfiguration? {
        guard let evaluatorIdentity = evaluatorIdentity(),
              let data = try? Data(contentsOf: snapshotURL),
              let payload = try? JSONDecoder().decode(Payload.self, from: data),
              let schemaIdentity = try? currentSchemaIdentity(),
              payload.formatVersion == Self.formatVersion,
              payload.appIdentity == appIdentity,
              payload.evaluatorIdentity == evaluatorIdentity,
              payload.schemaIdentity == schemaIdentity,
              dependenciesStillMatch(payload.dependencies)
        else {
            return nil
        }
        return payload.configuration
    }

    func captureDependencies() async -> [Dependency]? {
        do {
            return try dependencies(for: try await analyze(moduleURLs))
        } catch {
            Log.warning("CONFIG", "Startup snapshot disabled: \(error.localizedDescription)")
            return nil
        }
    }

    /// Build a snapshot outside startup's critical path. Analyze and capture
    /// first, then evaluate the same modules again. Saving re-hashes every
    /// dependency, so only that second successful Codable result is persisted.
    /// It is never reapplied during the launch that created it.
    func refresh() async {
        guard let dependencies = await captureDependencies() else { return }

        do {
            let configuration = try await evaluate(moduleURLs)
            try save(configuration, dependencies: dependencies)
            Log.info("CONFIG", "Saved validated startup snapshot")
        } catch {
            Log.warning("CONFIG", "Startup snapshot not saved: \(error.localizedDescription)")
        }
    }

    func dependencies(for graph: PklImportGraph) throws -> [Dependency] {
        let sources = Set(graph.imports.keys)
        guard sources == Set(graph.resolvedImports.keys) else {
            throw PklSnapshotError.incompleteImportGraph
        }

        let importedSources = Set(graph.imports.values.flatMap { $0.map(\.uri) })
        guard importedSources.isSubset(of: sources) else {
            throw PklSnapshotError.incompleteImportGraph
        }

        let requiredSources = Set(moduleURLs.map { $0.standardizedFileURL.absoluteString })
        guard requiredSources.isSubset(of: sources) else {
            throw PklSnapshotError.incompleteImportGraph
        }

        return try sources.sorted().map { sourceURI in
            guard let resolvedURI = graph.resolvedImports[sourceURI],
                  let resolvedURL = URL(string: resolvedURI),
                  resolvedURL.isFileURL
            else {
                throw PklSnapshotError.nonLocalDependency(sourceURI)
            }

            let currentURL = try currentURL(for: sourceURI)
            let expectedPath = currentURL.resolvingSymlinksInPath().standardizedFileURL.path
            let analyzedPath = resolvedURL.resolvingSymlinksInPath().standardizedFileURL.path
            guard expectedPath == analyzedPath else {
                throw PklSnapshotError.changedDependency(sourceURI)
            }

            let data = try Data(contentsOf: currentURL)
            guard isDeterministicSource(data) else {
                throw PklSnapshotError.externalInput(sourceURI)
            }
            return Dependency(
                sourceURI: sourceURI,
                resolvedPath: analyzedPath,
                contentHash: Self.hash(data)
            )
        }
    }

    func dependenciesStillMatch(_ dependencies: [Dependency]) -> Bool {
        let requiredSources = Set(moduleURLs.map { $0.standardizedFileURL.absoluteString })
        guard requiredSources.isSubset(of: Set(dependencies.map(\.sourceURI))) else { return false }

        return dependencies.allSatisfy { dependency in
            guard let url = try? currentURL(for: dependency.sourceURI),
                  url.resolvingSymlinksInPath().standardizedFileURL.path == dependency.resolvedPath,
                  let data = try? Data(contentsOf: url),
                  isDeterministicSource(data)
            else {
                return false
            }
            return Self.hash(data) == dependency.contentHash
        }
    }

    func save(_ configuration: PklStartupConfiguration, dependencies: [Dependency]) throws {
        guard let evaluatorIdentity = evaluatorIdentity() else {
            throw PklSnapshotError.missingEvaluatorIdentity
        }
        guard dependenciesStillMatch(dependencies) else {
            throw PklSnapshotError.changedDuringEvaluation
        }

        let payload = Payload(
            formatVersion: Self.formatVersion,
            appIdentity: appIdentity,
            evaluatorIdentity: evaluatorIdentity,
            schemaIdentity: try currentSchemaIdentity(),
            dependencies: dependencies,
            configuration: configuration
        )
        let data = try JSONEncoder().encode(payload)
        try atomicWrite(data)
    }

    private func currentURL(for sourceURI: String) throws -> URL {
        guard let sourceURL = URL(string: sourceURI) else {
            throw PklSnapshotError.nonLocalDependency(sourceURI)
        }

        switch sourceURL.scheme {
        case "file":
            return sourceURL
        case "modulepath":
            guard let schemaDirectoryURL else {
                throw PklSnapshotError.nonLocalDependency(sourceURI)
            }
            return schemaDirectoryURL.appendingPathComponent(
                sourceURL.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            )
        default:
            throw PklSnapshotError.nonLocalDependency(sourceURI)
        }
    }

    /// Resource reads can depend on files, environment, properties, or the
    /// network. Glob imports can change when a directory gains a file. This is
    /// deliberately a conservative scanner, not a complete Pkl lexer: it
    /// recognizes legal whitespace and nested-comment trivia, and may disable
    /// caching for read/import-like text inside comments or strings.
    private func isDeterministicSource(_ data: Data) -> Bool {
        guard let source = String(data: data, encoding: .utf8) else { return false }
        return !containsResourceRead(source) && !containsGlobImport(source)
    }

    private func containsResourceRead(_ source: String) -> Bool {
        var searchStart = source.startIndex

        while let keyword = source.range(of: "read", range: searchStart ..< source.endIndex) {
            searchStart = keyword.upperBound
            guard isTokenBoundary(source, before: keyword.lowerBound),
                  isTokenBoundary(source, after: keyword.upperBound)
            else {
                continue
            }

            var cursor = keyword.upperBound
            skipPklTrivia(in: source, from: &cursor)
            if cursor < source.endIndex, source[cursor] == "?" || source[cursor] == "*" {
                cursor = source.index(after: cursor)
                skipPklTrivia(in: source, from: &cursor)
            }
            if cursor < source.endIndex, source[cursor] == "(" {
                return true
            }
        }

        return false
    }

    private func containsGlobImport(_ source: String) -> Bool {
        var searchStart = source.startIndex

        while let keyword = source.range(of: "import", range: searchStart ..< source.endIndex) {
            searchStart = keyword.upperBound
            guard isTokenBoundary(source, before: keyword.lowerBound),
                  isTokenBoundary(source, after: keyword.upperBound)
            else {
                continue
            }

            var cursor = keyword.upperBound
            skipPklTrivia(in: source, from: &cursor)
            if cursor < source.endIndex, source[cursor] == "*" {
                return true
            }
        }

        return false
    }

    private func isTokenBoundary(_ source: String, before index: String.Index) -> Bool {
        guard index > source.startIndex else { return true }
        return !isIdentifierCharacter(source[source.index(before: index)])
    }

    private func isTokenBoundary(_ source: String, after index: String.Index) -> Bool {
        guard index < source.endIndex else { return true }
        return !isIdentifierCharacter(source[index])
    }

    private func isIdentifierCharacter(_ character: Character) -> Bool {
        character == "_" || character.unicodeScalars.allSatisfy {
            CharacterSet.alphanumerics.contains($0)
        }
    }

    private func skipPklTrivia(in source: String, from index: inout String.Index) {
        while index < source.endIndex {
            if source[index].isWhitespace {
                index = source.index(after: index)
                continue
            }

            if source[index...].hasPrefix("//") {
                index = source[index...].firstIndex(of: "\n") ?? source.endIndex
                continue
            }

            if source[index...].hasPrefix("/*") {
                index = source.index(index, offsetBy: 2)
                var depth = 1
                while index < source.endIndex, depth > 0 {
                    if source[index...].hasPrefix("/*") {
                        depth += 1
                        index = source.index(index, offsetBy: 2)
                    } else if source[index...].hasPrefix("*/") {
                        depth -= 1
                        index = source.index(index, offsetBy: 2)
                    } else {
                        index = source.index(after: index)
                    }
                }
                continue
            }

            return
        }
    }

    private func currentSchemaIdentity() throws -> String {
        guard let schemaDirectoryURL else { return "no-schema-directory" }
        let schemaURLs = try FileManager.default.contentsOfDirectory(
            at: schemaDirectoryURL,
            includingPropertiesForKeys: [.isRegularFileKey],
            options: [.skipsHiddenFiles]
        ).filter { $0.pathExtension == "pkl" }.sorted { $0.lastPathComponent < $1.lastPathComponent }

        var content = Data()
        for url in schemaURLs {
            content.append(Data(url.lastPathComponent.utf8))
            content.append(0)
            content.append(try Data(contentsOf: url))
            content.append(0)
        }
        return Self.hash(content)
    }

    private func atomicWrite(_ data: Data) throws {
        let directory = snapshotURL.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let temporaryURL = directory.appendingPathComponent(".\(snapshotURL.lastPathComponent).\(UUID().uuidString).tmp")
        defer { try? FileManager.default.removeItem(at: temporaryURL) }

        guard FileManager.default.createFile(
            atPath: temporaryURL.path,
            contents: nil,
            attributes: [.posixPermissions: 0o600]
        ) else {
            throw CocoaError(.fileWriteUnknown)
        }
        let handle = try FileHandle(forWritingTo: temporaryURL)
        do {
            try handle.write(contentsOf: data)
            try handle.synchronize()
            try handle.close()
        } catch {
            try? handle.close()
            throw error
        }

        guard rename(temporaryURL.path, snapshotURL.path) == 0 else {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
    }

    private static func hash(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    static let defaultSnapshotURL: URL = {
        let caches = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask).first
            ?? URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent("Library/Caches", isDirectory: true)
        let identifier = Bundle.main.bundleIdentifier ?? "org.js-lab.durian"
        return caches.appendingPathComponent("\(identifier)/startup-pkl-v\(formatVersion).json")
    }()

    static let defaultAppIdentity: String = {
        let bundle = Bundle.main
        let identifier = bundle.bundleIdentifier ?? "org.js-lab.durian"
        let version = bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
        let build = bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "dev"
        return "\(identifier):\(version):\(build):pkl-startup-\(formatVersion)"
    }()
}

enum PklSnapshotError: LocalizedError {
    case changedDependency(String)
    case changedDuringEvaluation
    case externalInput(String)
    case incompleteImportGraph
    case missingEvaluatorIdentity
    case nonLocalDependency(String)

    var errorDescription: String? {
        switch self {
        case .changedDependency(let uri):
            return "Pkl dependency resolution changed for \(uri)"
        case .changedDuringEvaluation:
            return "Pkl sources changed during evaluation"
        case .externalInput(let uri):
            return "Pkl module depends on an external input: \(uri)"
        case .incompleteImportGraph:
            return "Pkl import graph is incomplete"
        case .missingEvaluatorIdentity:
            return "Pkl evaluator identity is unavailable"
        case .nonLocalDependency(let uri):
            return "Pkl snapshot dependency is not local: \(uri)"
        }
    }
}
