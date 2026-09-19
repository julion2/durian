@testable import durian_lib
import SwiftUI
import XCTest

final class ProfileTests: XCTestCase {

    // MARK: - Test JSON (as pkl eval would produce)

    private let profilesJSON = """
    {
      "profiles": [
        { "name": "All", "accounts": ["*"], "default": true, "color": "#3B82F6" },
        { "name": "Personal", "accounts": ["personal"], "color": "#10B981" },
        {
          "name": "Work", "accounts": ["work", "company"], "color": "#F59E0B",
          "folders": [
            { "name": "Inbox", "icon": "tray", "query": "tag:inbox" },
            { "name": "Priority", "icon": "star", "query": "tag:flagged AND tag:inbox" }
          ]
        }
      ]
    }
    """

    // MARK: - Helpers

    private static let testDefaultFolders: [FolderConfig] = [
        FolderConfig(name: "Inbox", icon: "tray", query: "tag:inbox", isSection: false)
    ]

    private func makeAllProfile() -> Profile {
        Profile(name: "All", accounts: ["*"], isDefault: true, color: nil, folders: Self.testDefaultFolders)
    }

    private func makeAccountProfile(accounts: [String]) -> Profile {
        Profile(name: "Work", accounts: accounts, isDefault: false, color: "#F59E0B", folders: Self.testDefaultFolders)
    }

    // MARK: - JSON Decoding

    func testDecodeProfiles() throws {
        let data = profilesJSON.data(using: .utf8)!
        let config = try JSONDecoder().decode(ProfilesConfig.self, from: data)

        XCTAssertEqual(config.profiles.count, 3)
        XCTAssertEqual(config.profiles[0].name, "All")
        XCTAssertEqual(config.profiles[0].accounts, ["*"])
        XCTAssertEqual(config.profiles[0].default, true)
        XCTAssertEqual(config.profiles[1].name, "Personal")
        XCTAssertEqual(config.profiles[1].accounts, ["personal"])
        XCTAssertEqual(config.profiles[2].name, "Work")
        XCTAssertEqual(config.profiles[2].accounts, ["work", "company"])
    }

    func testProfileColors() throws {
        let data = profilesJSON.data(using: .utf8)!
        let config = try JSONDecoder().decode(ProfilesConfig.self, from: data)
        XCTAssertEqual(config.profiles[0].color, "#3B82F6")
        XCTAssertEqual(config.profiles[1].color, "#10B981")
    }

    func testProfileFolders() throws {
        let data = profilesJSON.data(using: .utf8)!
        let config = try JSONDecoder().decode(ProfilesConfig.self, from: data)

        let workFolders = config.profiles[2].folders
        XCTAssertNotNil(workFolders)
        XCTAssertEqual(workFolders?.count, 2)
        XCTAssertEqual(workFolders?[0].name, "Inbox")
        XCTAssertEqual(workFolders?[1].name, "Priority")
        XCTAssertEqual(workFolders?[1].query, "tag:flagged AND tag:inbox")
    }

    // MARK: - Query Building

    @MainActor
    func testBuildQueryAllProfile() {
        let profile = makeAllProfile()
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let query = manager.buildQuery(folderName: "Inbox")
        XCTAssertEqual(query, "tag:inbox")
    }

    @MainActor
    func testBuildQueryAccountProfile() {
        let profile = makeAccountProfile(accounts: ["work"])
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let query = manager.buildQuery(folderName: "Inbox")
        XCTAssertEqual(query, "(tag:inbox) AND (path:work/**)")
    }

    @MainActor
    func testBuildQueryMultipleAccounts() {
        let profile = makeAccountProfile(accounts: ["work", "company"])
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let query = manager.buildQuery(folderName: "Inbox")
        XCTAssertEqual(query, "(tag:inbox) AND (path:work/** OR path:company/**)")
    }

    @MainActor
    func testApplyProfileFilterAllProfile() {
        let profile = makeAllProfile()
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let filtered = manager.applyProfileFilter(to: "from:alice@example.com")
        XCTAssertEqual(filtered, "from:alice@example.com")
    }

    @MainActor
    func testApplyProfileFilterAccountProfile() {
        let profile = makeAccountProfile(accounts: ["work"])
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let filtered = manager.applyProfileFilter(to: "from:alice@example.com")
        XCTAssertEqual(filtered, "(from:alice@example.com) AND (path:work/**)")
    }

    @MainActor
    func testBuildQueryFallbackTag() {
        let profile = makeAllProfile()
        let manager = ProfileManager(profiles: [profile], currentProfile: profile)

        let query = manager.buildQuery(folderName: "Sent")
        XCTAssertEqual(query, "tag:sent")
    }

    @MainActor
    func testConcurrentPreparationWaitsForConfiguredDefaultProfile() async throws {
        let json = """
        {
          "profiles": [
            { "name": "All", "accounts": ["*"] },
            {
              "name": "Work", "accounts": ["work"], "default": true,
              "folders": [
                { "name": "Inbox", "icon": "tray", "query": "tag:priority" }
              ]
            }
          ]
        }
        """
        let evaluationCount = ProfileCounter()
        let manager = ProfileManager(
            configURL: URL(fileURLWithPath: "/unused/profiles.pkl"),
            evaluator: { _ in
                evaluationCount.increment()
                try await Task.sleep(nanoseconds: 25_000_000)
                return try JSONDecoder().decode(ProfilesConfig.self, from: Data(json.utf8))
            },
            fileExists: { _ in true }
        )

        async let preparation: Void = manager.prepareProfiles()
        async let readiness: Void = manager.waitUntilReady()
        _ = await (preparation, readiness)

        XCTAssertTrue(manager.isReady)
        XCTAssertEqual(evaluationCount.value, 1)
        XCTAssertEqual(manager.currentProfile?.name, "Work")
        XCTAssertEqual(
            manager.buildQuery(folderName: "Inbox"),
            "(tag:priority) AND (path:work/**)"
        )
    }

    @MainActor
    func testAppliedStartupProfilesAreImmediatelyReadyWithoutEvaluation() async throws {
        let evaluationCount = ProfileCounter()
        let manager = ProfileManager(
            configURL: URL(fileURLWithPath: "/unused/profiles.pkl"),
            evaluator: { _ in
                evaluationCount.increment()
                throw NSError(domain: "ProfileTests", code: 1)
            },
            fileExists: { _ in true }
        )
        let config = try JSONDecoder().decode(
            ProfilesConfig.self,
            from: Data(
                #"{"profiles":[{"name":"Work","accounts":["work"],"default":true,"folders":[{"name":"Inbox","query":"tag:priority"}]}]}"#.utf8
            )
        )

        manager.applyEvaluatedProfiles(config)
        await manager.waitUntilReady()

        XCTAssertTrue(manager.isReady)
        XCTAssertEqual(evaluationCount.value, 0)
        XCTAssertEqual(manager.currentProfile?.name, "Work")
        XCTAssertEqual(
            manager.buildQuery(folderName: "Inbox"),
            "(tag:priority) AND (path:work/**)"
        )
    }

    @MainActor
    func testReadinessWaitDoesNotStartIndependentProfileEvaluation() async throws {
        let evaluationCount = ProfileCounter()
        let manager = ProfileManager(
            configURL: URL(fileURLWithPath: "/unused/profiles.pkl"),
            evaluator: { _ in
                evaluationCount.increment()
                throw NSError(domain: "ProfileTests", code: 1)
            },
            fileExists: { _ in true }
        )
        let readiness = Task { @MainActor in
            await manager.waitUntilReady()
        }
        await Task.yield()

        XCTAssertEqual(evaluationCount.value, 0)
        XCTAssertFalse(manager.isReady)

        let config = try JSONDecoder().decode(
            ProfilesConfig.self,
            from: Data(#"{"profiles":[{"name":"Work","accounts":["work"],"default":true}]}"#.utf8)
        )
        manager.applyEvaluatedProfiles(config)
        await readiness.value

        XCTAssertEqual(evaluationCount.value, 0)
        XCTAssertTrue(manager.isReady)
        XCTAssertEqual(manager.currentProfile?.name, "Work")
    }

    @MainActor
    func testManualReloadDuringStartupPerformsSubsequentRead() async throws {
        let evaluationCount = ProfileCounter()
        let manager = ProfileManager(
            configURL: URL(fileURLWithPath: "/unused/profiles.pkl"),
            evaluator: { _ in
                let count = evaluationCount.increment()
                if count == 1 {
                    try await Task.sleep(for: .milliseconds(40))
                }
                let name = count == 1 ? "First" : "Reloaded"
                return try JSONDecoder().decode(
                    ProfilesConfig.self,
                    from: Data("{\"profiles\":[{\"name\":\"\(name)\",\"accounts\":[\"*\"]}]}".utf8)
                )
            },
            fileExists: { _ in true }
        )

        async let preparation: Void = manager.prepareProfiles()
        try await Task.sleep(for: .milliseconds(5))
        async let reload: Void = manager.reloadProfiles()
        _ = await (preparation, reload)

        XCTAssertEqual(evaluationCount.value, 2)
        XCTAssertEqual(manager.currentProfile?.name, "Reloaded")
    }

    @MainActor
    func testMissingProfilesUsesReadyFallbackWithoutEvaluation() async {
        let evaluationCount = ProfileCounter()
        let manager = ProfileManager(
            configURL: URL(fileURLWithPath: "/missing/profiles.pkl"),
            evaluator: { _ in
                evaluationCount.increment()
                throw NSError(domain: "ProfileTests", code: 1)
            },
            fileExists: { _ in false }
        )

        await manager.prepareProfiles()

        XCTAssertTrue(manager.isReady)
        XCTAssertEqual(evaluationCount.value, 0)
        XCTAssertEqual(manager.currentProfile?.name, "All")
    }

    // MARK: - Color(hex:)

    func testColorHexRed() {
        let color = Color(hex: "#FF0000")
        XCTAssertNotNil(color)
    }

    func testColorHexWithoutHash() {
        let color = Color(hex: "00FF00")
        XCTAssertNotNil(color)
    }

    func testColorHexInvalidFallback() {
        let fallback = Color(red: 0.6, green: 0.4, blue: 0.2)
        XCTAssertEqual(Color(hex: "XYZ"), fallback)
        XCTAssertEqual(Color(hex: "123xyz"), fallback)
        XCTAssertEqual(Color(hex: "FFFF"), fallback)
        XCTAssertEqual(Color(hex: ""), fallback)
    }

    func testColorHexShorthandExpands() {
        XCTAssertEqual(Color(hex: "F53"), Color(hex: "FF5533"))
    }

    func testContrastingForegroundForLightAndDarkColors() {
        XCTAssertEqual(Color(hex: "FFFFFF").contrastingForeground(), .black)
        XCTAssertEqual(Color(hex: "000000").contrastingForeground(), .white)
    }

    // MARK: - Profile.isAll

    func testProfileIsAll() {
        let profile = makeAllProfile()
        XCTAssertTrue(profile.isAll)
    }

    func testProfileIsNotAll() {
        let profile = makeAccountProfile(accounts: ["work"])
        XCTAssertFalse(profile.isAll)
    }

    // MARK: - Resolved Accent Color

    func testResolvedAccentColor_PerProfileOverride() async {
        let profile = Profile(name: "Work", accounts: ["work"], isDefault: false,
                              color: "#FF0000", folders: Self.testDefaultFolders)
        let manager = await ProfileManager(profiles: [profile], currentProfile: profile)
        let color = await manager.resolvedAccentColor
        XCTAssertEqual(color, Color(hex: "#FF0000"))
    }

    func testResolvedAccentColor_NoProfileColor() async {
        let profile = Profile(name: "Plain", accounts: ["work"], isDefault: false,
                              color: nil, folders: Self.testDefaultFolders)
        let manager = await ProfileManager(profiles: [profile], currentProfile: profile)
        let color = await manager.resolvedAccentColor
        XCTAssertEqual(color, Color.accentColor)
    }
}

private final class ProfileCounter: @unchecked Sendable {
    private let lock = NSLock()
    private var count = 0

    var value: Int { lock.withLock { count } }

    @discardableResult
    func increment() -> Int {
        lock.withLock {
            count += 1
            return count
        }
    }
}
