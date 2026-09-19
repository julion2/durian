//
//  ProfileManager.swift
//  Durian
//
//  Manages mail profiles (account groups) loaded from profiles.pkl
//

import Foundation
import SwiftUI

// MARK: - Folder Config

struct FolderConfig: Hashable {
    let name: String
    let icon: String?
    let query: String
    let isSection: Bool  // true = section header, not a clickable folder
}

// MARK: - Profile

struct Profile: Identifiable, Equatable, Hashable {
    let id = UUID()
    let name: String
    let accounts: [String]  // ["work", "personal"] or ["*"] for all
    let isDefault: Bool
    let color: String?  // Hex color string, e.g. "#3B82F6"
    let folders: [FolderConfig]  // Folders with custom queries

    var isAll: Bool { accounts.contains("*") }

    /// Convert hex color string to SwiftUI Color
    var swiftUIColor: Color {
        guard let hex = color else { return .brown }  // Fallback: Brown
        return Color(hex: hex)
    }

    static func == (lhs: Profile, rhs: Profile) -> Bool {
        lhs.name == rhs.name && lhs.accounts == rhs.accounts
    }

    func hash(into hasher: inout Hasher) {
        hasher.combine(name)
        hasher.combine(accounts)
    }
}

// MARK: - Config Decoding

struct ProfilesConfig: Codable {
    let profiles: [ProfileEntry]

    struct ProfileEntry: Codable {
        let name: String
        let accounts: [String]
        var `default`: Bool?
        var color: String?
        var folders: [FolderEntry]?
    }

    struct FolderEntry: Codable {
        let name: String
        var icon: String?
        var query: String?  // nil or empty = section header
    }
}

// MARK: - Profile Manager

@MainActor
class ProfileManager: ObservableObject {
    static let shared = ProfileManager()

    @Published var profiles: [Profile] = []
    @Published var currentProfile: Profile?
    @Published private(set) var isReady = false

    typealias ProfilesEvaluator = (URL) async throws -> ProfilesConfig

    private let configURL: URL
    private let evaluator: ProfilesEvaluator
    private let fileExists: (String) -> Bool
    private var loadTask: Task<Void, Never>?
    private var readinessContinuations: [CheckedContinuation<Void, Never>] = []

    /// Default folders when none are defined in config
    static let defaultFolders: [FolderConfig] = [
        FolderConfig(name: "Inbox", icon: "tray", query: "tag:inbox", isSection: false)
    ]

    init(
        configURL: URL = FileManager.default.durianConfigURL().appendingPathComponent("profiles.pkl"),
        evaluator: @escaping ProfilesEvaluator = { try await PklEvaluator.eval(ProfilesConfig.self, from: $0) },
        fileExists: @escaping (String) -> Bool = FileManager.default.fileExists(atPath:)
    ) {
        self.configURL = configURL
        self.evaluator = evaluator
        self.fileExists = fileExists
    }

    /// Test-only initializer: inject profiles directly, skip file loading
    init(profiles: [Profile], currentProfile: Profile? = nil) {
        configURL = FileManager.default.durianConfigURL().appendingPathComponent("profiles.pkl")
        evaluator = { try await PklEvaluator.eval(ProfilesConfig.self, from: $0) }
        fileExists = { _ in true }
        self.profiles = profiles
        self.currentProfile = currentProfile ?? profiles.first
        isReady = true
    }

    /// Load profiles once for startup. Concurrent callers join the same task.
    func prepareProfiles() async {
        guard !isReady else { return }
        await loadProfiles()
    }

    /// Wait until startup profile loading (including fallback handling) finishes.
    func waitUntilReady() async {
        guard !isReady else { return }
        await withCheckedContinuation { continuation in
            if isReady {
                continuation.resume()
            } else {
                readinessContinuations.append(continuation)
            }
        }
    }

    /// Force a disk reload, preserving the manual Reload Config behavior.
    func reloadProfiles() async {
        if let loadTask {
            await loadTask.value
        }
        await startProfilesLoad()
    }

    func applyEvaluatedProfiles(_ config: ProfilesConfig) {
        apply(config)
    }

    private func loadProfiles() async {
        if let loadTask {
            await loadTask.value
            return
        }

        await startProfilesLoad()
    }

    private func startProfilesLoad() async {
        let task = Task { @MainActor [weak self] in
            guard let self else { return }
            await loadProfilesFromDisk()
            loadTask = nil
        }
        loadTask = task
        await task.value
    }

    private func loadProfilesFromDisk() async {
        guard fileExists(configURL.path) else {
            Log.warning("PROFILE", "profiles.pkl not found, using defaults")
            applyFallback()
            return
        }

        let config: ProfilesConfig
        do {
            config = try await evaluator(configURL)
        } catch {
            Log.error("PROFILE", "Failed to load profiles.pkl: \(error)")
            applyFallback()
            return
        }

        apply(config)
    }

    private func applyFallback() {
        profiles = [Profile(
            name: "All",
            accounts: ["*"],
            isDefault: true,
            color: nil,
            folders: Self.defaultFolders
        )]
        currentProfile = profiles.first
        markReady()
    }

    private func apply(_ config: ProfilesConfig) {
        profiles = config.profiles.map { entry in
            let folders: [FolderConfig]
            if let entryFolders = entry.folders, !entryFolders.isEmpty {
                folders = entryFolders.map { entry in
                    let query = entry.query ?? ""
                    return FolderConfig(name: entry.name, icon: entry.icon, query: query, isSection: query.isEmpty)
                }
            } else {
                folders = Self.defaultFolders
            }

            return Profile(
                name: entry.name,
                accounts: entry.accounts,
                isDefault: entry.default ?? false,
                color: entry.color,
                folders: folders
            )
        }

        currentProfile = profiles.first(where: { $0.isDefault }) ?? profiles.first
        Log.info("PROFILE", "Loaded \(profiles.count) profiles, current: \(currentProfile?.name ?? "none")")
        if let profile = currentProfile {
            Log.debug("PROFILE", "Current profile has \(profile.folders.count) folders")
        }
        markReady()
    }

    private func markReady() {
        isReady = true
        let continuations = readinessContinuations
        readinessContinuations.removeAll()
        continuations.forEach { $0.resume() }
    }

    /// Resolved app-wide accent color following the precedence:
    ///   1. currentProfile.color (per-profile override)
    ///   2. settings.accent_color (app-wide default in [settings])
    ///   3. system Color.accentColor (fallback)
    var resolvedAccentColor: Color {
        if let hex = currentProfile?.color {
            return Color(hex: hex)
        }
        if let hex = SettingsManager.shared.settings.accentColor {
            return Color(hex: hex)
        }
        return Color.accentColor
    }

    /// Build search query for a folder name
    /// - Looks up query from profile's folder config
    /// - Adds profile path filter for non-"All" profiles
    func buildQuery(folderName: String) -> String {
        guard let profile = currentProfile else {
            return "tag:inbox"
        }

        // Find folder query from config
        let baseQuery: String
        if let folder = profile.folders.first(where: { $0.name.lowercased() == folderName.lowercased() }) {
            baseQuery = folder.query
        } else {
            // Fallback: simple tag query
            baseQuery = "tag:\(folderName.lowercased())"
        }

        // Add profile path filter (except for "All" profile)
        return buildQueryWithProfileFilter(baseQuery: baseQuery)
    }

    /// Add profile path filter to an arbitrary query (e.g. from the search popup)
    func applyProfileFilter(to query: String) -> String {
        buildQueryWithProfileFilter(baseQuery: query)
    }

    /// Add profile path filter to a base query
    private func buildQueryWithProfileFilter(baseQuery: String) -> String {
        guard let profile = currentProfile, !profile.isAll else {
            return baseQuery
        }

        // Build path filter: (path:work/** OR path:personal/**)
        let pathFilters = profile.accounts.map { "path:\($0)/**" }
        let pathQuery = pathFilters.joined(separator: " OR ")

        let query = "(\(baseQuery)) AND (\(pathQuery))"
        Log.debug("PROFILE", "Built query: \(query)") // encgrep:allow assembled from profiles.pkl, not from mail content
        return query
    }
}
