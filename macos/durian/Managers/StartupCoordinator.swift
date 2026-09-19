import Combine
import Foundation

@MainActor
final class StartupCoordinator: ObservableObject {
    enum State: Equatable {
        case loading
        case ready
    }

    static let shared = StartupCoordinator()

    @Published private(set) var state: State = .loading

    var isReady: Bool { state == .ready }
    var allowsManualReload: Bool { isReady }

    private let moduleURLs: [URL]
    private let fileExists: (String) -> Bool
    private let evaluate: ([URL]) async throws -> PklStartupConfiguration
    private let apply: @MainActor (PklStartupConfiguration) -> Void
    private let loadFallbacks: @MainActor () async -> Void
    private let setupSync: @MainActor () -> Void
    private let loadSnapshot: @Sendable () -> PklStartupConfiguration?
    private let refreshSnapshot: @Sendable () async -> Void
    private var startupTask: Task<Void, Never>?
    private var readinessContinuations: [CheckedContinuation<Void, Never>] = []

    init(
        moduleURLs: [URL] = PklEvaluator.startupModuleURLs,
        fileExists: @escaping (String) -> Bool = FileManager.default.fileExists(atPath:),
        evaluate: @escaping ([URL]) async throws -> PklStartupConfiguration = {
            try await PklEvaluator.evalStartup(moduleURLs: $0)
        },
        apply: @escaping @MainActor (PklStartupConfiguration) -> Void = { configuration in
            SettingsManager.shared.applyEvaluatedConfig(configuration.config)
            ProfileManager.shared.applyEvaluatedProfiles(configuration.profiles)
            KeymapsManager.shared.applyEvaluatedKeymaps(configuration.keymaps)
        },
        loadFallbacks: @escaping @MainActor () async -> Void = {
            async let settings: Void = SettingsManager.shared.prepareSettings()
            async let profiles: Void = ProfileManager.shared.prepareProfiles()
            async let keymaps: Void = KeymapsManager.shared.prepareKeymaps()
            _ = await (settings, profiles, keymaps)
        },
        setupSync: @escaping @MainActor () -> Void = { SyncManager.shared.setup() },
        snapshot: PklStartupSnapshot = PklStartupSnapshot(),
        loadSnapshot: (@Sendable () -> PklStartupConfiguration?)? = nil,
        refreshSnapshot: (@Sendable () async -> Void)? = nil
    ) {
        self.moduleURLs = moduleURLs
        self.fileExists = fileExists
        self.evaluate = evaluate
        self.apply = apply
        self.loadFallbacks = loadFallbacks
        self.setupSync = setupSync
        self.loadSnapshot = loadSnapshot ?? { snapshot.load() }
        self.refreshSnapshot = refreshSnapshot ?? { await snapshot.refresh() }
    }

    func start() {
        guard startupTask == nil, state == .loading else { return }
        startupTask = Task { @MainActor [weak self] in
            await self?.run()
        }
    }

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

    private func run() async {
        var shouldRefreshSnapshot = false
        if moduleURLs.allSatisfy({ fileExists($0.path) }) {
            if let cached = loadSnapshot() {
                apply(cached)
                Log.info("CONFIG", "Loaded validated startup snapshot")
            } else {
                do {
                    apply(try await evaluate(moduleURLs))
                    shouldRefreshSnapshot = true
                } catch {
                    Log.warning("CONFIG", "Batched startup evaluation failed: \(error.localizedDescription)")
                    await loadFallbacks()
                }
            }
        } else {
            Log.warning("CONFIG", "Startup Pkl files missing; using per-manager fallbacks")
            await loadFallbacks()
        }

        setupSync()
        markReady()

        if shouldRefreshSnapshot {
            let refreshSnapshot = refreshSnapshot
            Task.detached(priority: .utility) {
                await refreshSnapshot()
            }
        }
    }

    private func markReady() {
        state = .ready
        startupTask = nil
        let continuations = readinessContinuations
        readinessContinuations.removeAll()
        continuations.forEach { $0.resume() }
        Log.info("CONFIG", "Startup configuration ready")
    }
}
