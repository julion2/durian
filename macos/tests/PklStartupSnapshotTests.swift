@testable import durian_lib
import Foundation
import XCTest

final class PklStartupSnapshotTests: XCTestCase {
    func testMatchingSnapshotLoadsAndSourceOrSchemaChangesInvalidateIt() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let store = fixture.store()
        let dependencies = try store.dependencies(for: fixture.graph)
        try store.save(try configuration(), dependencies: dependencies)

        XCTAssertEqual(store.load()?.config.settings.theme, "dark")
        XCTAssertEqual(store.load()?.profiles.profiles.first?.name, "Work")

        try Data("value = 2".utf8).write(to: fixture.helperURL)
        XCTAssertNil(store.load())

        try Data("value = 1".utf8).write(to: fixture.helperURL)
        XCTAssertNotNil(store.load())
        try Data("changed schema".utf8).write(to: fixture.schemaDirectory.appendingPathComponent("Config.pkl"))
        XCTAssertNil(store.load())
    }

    func testSnapshotRejectsAppEvaluatorFormatChangesAndCorruption() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let store = fixture.store()
        try store.save(try configuration(), dependencies: store.dependencies(for: fixture.graph))

        XCTAssertNil(fixture.store(appIdentity: "app-v2").load())
        XCTAssertNil(fixture.store(evaluatorIdentity: "pkl-v2").load())

        var payload = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fixture.snapshotURL)) as? [String: Any]
        )
        payload["formatVersion"] = PklStartupSnapshot.formatVersion - 1
        try JSONSerialization.data(withJSONObject: payload).write(to: fixture.snapshotURL)
        XCTAssertNil(store.load())

        try Data("not json".utf8).write(to: fixture.snapshotURL)
        XCTAssertNil(store.load())
    }

    func testChangedSourceCannotBeSavedWithOldFingerprint() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let store = fixture.store()
        let dependencies = try store.dependencies(for: fixture.graph)
        try Data("changed = true".utf8).write(to: fixture.modules[0])

        XCTAssertThrowsError(try store.save(try configuration(), dependencies: dependencies)) { error in
            guard case PklSnapshotError.changedDuringEvaluation = error else {
                return XCTFail("Unexpected error: \(error)")
            }
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.snapshotURL.path))
    }

    func testRefreshCapturesThenEvaluatesAndAtomicallySavesSecondResult() async throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let events = SnapshotEvents()
        let store = fixture.store(
            analyze: { _ in
                events.append("analyze")
                return fixture.graph
            },
            evaluate: { _ in
                events.append("evaluate")
                return try self.configuration(theme: "light")
            }
        )

        await store.refresh()

        XCTAssertEqual(events.values, ["analyze", "evaluate"])
        XCTAssertEqual(store.load()?.config.settings.theme, "light")
        let attributes = try FileManager.default.attributesOfItem(atPath: fixture.snapshotURL.path)
        XCTAssertEqual((attributes[.posixPermissions] as? NSNumber)?.intValue, 0o600)
        let siblings = try FileManager.default.contentsOfDirectory(atPath: fixture.root.path)
        XCTAssertFalse(siblings.contains { $0.hasSuffix(".tmp") })
    }

    func testRefreshRejectsSourceChangeDuringEvaluation() async throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let store = fixture.store(
            analyze: { _ in fixture.graph },
            evaluate: { _ in
                try Data("changed = true".utf8).write(to: fixture.modules[0])
                return try self.configuration()
            }
        )

        await store.refresh()

        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.snapshotURL.path))
    }

    func testMissingEvaluatorIdentityCannotSaveOrLoad() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let validStore = fixture.store()
        let dependencies = try validStore.dependencies(for: fixture.graph)
        let store = fixture.store(evaluatorIdentity: nil)

        XCTAssertThrowsError(try store.save(try configuration(), dependencies: dependencies))
        XCTAssertNil(store.load())
    }

    func testSnapshotUsesEvaluatorIdentityResolvedAfterConstruction() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        let identity = SnapshotIdentity()
        let store = fixture.store(evaluatorIdentityProvider: { identity.value })
        let dependencies = try store.dependencies(for: fixture.graph)

        XCTAssertThrowsError(try store.save(try configuration(), dependencies: dependencies))
        identity.value = "eventually-resolved-pkl"
        try store.save(try configuration(), dependencies: dependencies)

        XCTAssertEqual(store.load()?.config.settings.theme, "dark")
    }

    func testResourceReadsAndGlobImportsDisableSnapshotWithLegalTrivia() throws {
        let sources = [
            #"value = read("env:HOME")"#,
            "value = read /* outer /* nested */ comment */ ? // optional\n (\"env:HOME\")",
            #"value = read* ("file:*.txt")"#,
            #"import* "modules/*.pkl" as modules"#,
            "import /* outer /* nested */ comment */ * \"modules/*.pkl\" as modules",
            "import // glob follows\n * \"modules/*.pkl\" as modules",
        ]

        for source in sources {
            let fixture = try SnapshotFixture()
            defer { fixture.remove() }
            try Data(source.utf8).write(to: fixture.modules[0])

            XCTAssertThrowsError(try fixture.store().dependencies(for: fixture.graph)) { error in
                guard case PklSnapshotError.externalInput = error else {
                    return XCTFail("Unexpected error for \(source): \(error)")
                }
            }
        }
    }

    func testScannerConservativelyDisablesCommentAndStringFalsePositives() throws {
        for source in [
            "// Example: read(\"env:HOME\")\nvalue = 1",
            "message = \"never call read(\\\"env:HOME\\\")\"",
            "message = \"example import* pattern\"",
        ] {
            let fixture = try SnapshotFixture()
            defer { fixture.remove() }
            try Data(source.utf8).write(to: fixture.modules[0])
            XCTAssertThrowsError(try fixture.store().dependencies(for: fixture.graph))
        }
    }

    func testOrdinaryImportsAndReadLikeIdentifiersRemainCacheable() throws {
        let fixture = try SnapshotFixture()
        defer { fixture.remove() }
        try Data("import \"helper.pkl\"\nreader = 2\nvalue = reader * 3 // read by app".utf8)
            .write(to: fixture.modules[0])

        XCTAssertNoThrow(try fixture.store().dependencies(for: fixture.graph))
    }

    func testGoldenPklAnalyzerOutputDecodes() throws {
        let root = URL(fileURLWithPath: try XCTUnwrap(ProcessInfo.processInfo.environment["TEST_SRCDIR"]))
        let workspace = ProcessInfo.processInfo.environment["TEST_WORKSPACE"] ?? "_main"
        let fixtureURL = root.appendingPathComponent(workspace)
            .appendingPathComponent("macos/tests/Fixtures/pkl-imports-0.31.1.json")

        let graph = try JSONDecoder().decode(PklImportGraph.self, from: Data(contentsOf: fixtureURL))

        XCTAssertEqual(graph.imports.count, 4)
        XCTAssertEqual(
            graph.imports["file:///fixtures/pkl/config.pkl"]?.first?.uri,
            "file:///fixtures/pkl/helper.pkl"
        )
        XCTAssertEqual(graph.resolvedImports.count, 4)
    }

    func testDefaultSnapshotPathUsesCurrentBundleIdentifier() {
        let identifier = Bundle.main.bundleIdentifier ?? "org.js-lab.durian"

        XCTAssertTrue(PklStartupSnapshot.defaultSnapshotURL.path.contains("/\(identifier)/"))
        XCTAssertTrue(PklStartupSnapshot.defaultSnapshotURL.lastPathComponent.contains(
            "v\(PklStartupSnapshot.formatVersion)"
        ))
    }

    private func configuration(theme: String = "dark") throws -> PklStartupConfiguration {
        let decoder = JSONDecoder()
        let config = try decoder.decode(
            AppConfig.self,
            from: Data("{\"accounts\":[],\"settings\":{\"theme\":\"\(theme)\"},\"sync\":{},\"signatures\":{}}".utf8)
        )
        let profiles = try decoder.decode(
            ProfilesConfig.self,
            from: Data(#"{"profiles":[{"name":"Work","accounts":["work"],"default":true}]}"#.utf8)
        )
        return PklStartupConfiguration(config: config, profiles: profiles, keymaps: KeymapConfig())
    }
}

private struct SnapshotFixture: @unchecked Sendable {
    let root: URL
    let snapshotURL: URL
    let modules: [URL]
    let helperURL: URL
    let schemaDirectory: URL
    let graph: PklImportGraph

    init() throws {
        let fixtureRoot = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let configDirectory = fixtureRoot.appendingPathComponent("config", isDirectory: true)
        let schemaDirectory = fixtureRoot.appendingPathComponent("schema", isDirectory: true)
        try FileManager.default.createDirectory(at: configDirectory, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: schemaDirectory, withIntermediateDirectories: true)

        let modules = ["config.pkl", "profiles.pkl", "keymaps.pkl"].map {
            configDirectory.appendingPathComponent($0)
        }
        let helper = configDirectory.appendingPathComponent("helper.pkl")
        for (url, contents) in zip(modules, ["import \"helper.pkl\"", "profiles = new {}", "keymaps = new {}"]) {
            try Data(contents.utf8).write(to: url)
        }
        try Data("value = 1".utf8).write(to: helper)

        let schemaNames = ["Config.pkl", "Profiles.pkl", "Keymaps.pkl"]
        for name in schemaNames {
            try Data("module schema.\(name)".utf8).write(to: schemaDirectory.appendingPathComponent(name))
        }

        let topSources = modules.map { $0.standardizedFileURL.absoluteString }
        let helperSource = helper.standardizedFileURL.absoluteString
        let schemaSources = schemaNames.map { "modulepath:/\($0)" }
        var imports = Dictionary(uniqueKeysWithValues: (topSources + [helperSource] + schemaSources).map {
            ($0, [PklImportGraph.ImportedModule]())
        })
        imports[topSources[0]] = [
            PklImportGraph.ImportedModule(uri: helperSource),
            PklImportGraph.ImportedModule(uri: schemaSources[0]),
        ]
        imports[topSources[1]] = [PklImportGraph.ImportedModule(uri: schemaSources[1])]
        imports[topSources[2]] = [PklImportGraph.ImportedModule(uri: schemaSources[2])]

        var resolved = Dictionary(uniqueKeysWithValues: (modules + [helper]).map {
            ($0.standardizedFileURL.absoluteString, $0.resolvingSymlinksInPath().standardizedFileURL.absoluteString)
        })
        for (source, name) in zip(schemaSources, schemaNames) {
            resolved[source] = schemaDirectory.appendingPathComponent(name)
                .resolvingSymlinksInPath().standardizedFileURL.absoluteString
        }

        root = fixtureRoot
        snapshotURL = fixtureRoot.appendingPathComponent("startup.json")
        self.modules = modules
        helperURL = helper
        self.schemaDirectory = schemaDirectory
        graph = PklImportGraph(imports: imports, resolvedImports: resolved)
    }

    func store(
        appIdentity: String = "app-v1",
        evaluatorIdentity: String? = "pkl-v1",
        evaluatorIdentityProvider: PklStartupSnapshot.EvaluatorIdentity? = nil,
        analyze: PklStartupSnapshot.Analyze? = nil,
        evaluate: PklStartupSnapshot.Evaluate? = nil
    ) -> PklStartupSnapshot {
        PklStartupSnapshot(
            snapshotURL: snapshotURL,
            moduleURLs: modules,
            schemaDirectoryURL: schemaDirectory,
            appIdentity: appIdentity,
            evaluatorIdentity: evaluatorIdentityProvider ?? { evaluatorIdentity },
            analyze: analyze ?? { _ in graph },
            evaluate: evaluate ?? { _ in throw NSError(domain: "SnapshotFixture", code: 1) }
        )
    }

    func remove() {
        try? FileManager.default.removeItem(at: root)
    }
}

private final class SnapshotIdentity: @unchecked Sendable {
    private let lock = NSLock()
    private var storedValue: String?

    var value: String? {
        get { lock.withLock { storedValue } }
        set { lock.withLock { storedValue = newValue } }
    }
}

private final class SnapshotEvents: @unchecked Sendable {
    private let lock = NSLock()
    private var events: [String] = []

    var values: [String] { lock.withLock { events } }

    func append(_ event: String) {
        lock.withLock { events.append(event) }
    }
}
