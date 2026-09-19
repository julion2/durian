import Combine
import Foundation

class SettingsManager: ObservableObject {
    static let shared = SettingsManager()

    @Published var settings: AppSettings = AppSettings()
    @Published private(set) var isReady = false
    private var cancellables = Set<AnyCancellable>()
    private var loadTask: Task<Void, Never>?
    private let configManager: ConfigManager
    private let autoSaveDelay: TimeInterval
    private let saveHandler: ((AppSettings) -> Void)?

    init(
        configManager: ConfigManager = .shared,
        autoSaveDelay: TimeInterval = 0.5,
        saveHandler: ((AppSettings) -> Void)? = nil
    ) {
        self.configManager = configManager
        self.autoSaveDelay = autoSaveDelay
        self.saveHandler = saveHandler
        setupAutoSave()
    }

    /// Load settings once for startup. Concurrent callers join the same task.
    @MainActor
    func prepareSettings() async {
        guard !isReady else { return }
        await loadSettings()
    }

    @MainActor
    private func loadSettings() async {
        if let loadTask {
            await loadTask.value
            return
        }

        await startSettingsLoad()
    }

    @MainActor
    private func startSettingsLoad() async {
        let task = Task { @MainActor [weak self] in
            guard let self else { return }
            await configManager.reloadConfig()
            settings = configManager.getSettings()
            isReady = true
            loadTask = nil
        }
        loadTask = task
        await task.value
    }

    @MainActor
    func applyEvaluatedConfig(_ config: AppConfig) {
        configManager.applyEvaluatedConfig(config)
        settings = config.settings
        isReady = true
    }

    private func setupAutoSave() {
        // Auto-save when settings change
        $settings
            .dropFirst() // Skip initial value
            .filter { [weak self] _ in self?.isReady == true }
            .debounce(for: .seconds(autoSaveDelay), scheduler: DispatchQueue.main)
            .sink { [weak self] settings in
                self?.saveSettings(settings)
            }
            .store(in: &cancellables)
    }

    private func saveSettings(_ settings: AppSettings) {
        if let saveHandler {
            saveHandler(settings)
        } else {
            configManager.updateSettings(settings)
        }
    }

    // MARK: - Sync Settings (read from [sync] section)

    /// Sync settings are read-only from config.pkl [sync] section
    var syncSettings: SyncSettings {
        configManager.getSyncSettings()
    }

    /// Whether GUI auto-sync is enabled
    var guiAutoSync: Bool {
        syncSettings.guiAutoSync
    }

    /// Quick sync interval in seconds
    var autoFetchInterval: TimeInterval {
        syncSettings.autoFetchInterval
    }

    /// Full sync interval in seconds
    var fullSyncInterval: TimeInterval {
        syncSettings.fullSyncInterval
    }

    /// Attachment cache settings
    var attachmentCacheSettings: AttachmentCacheSettings {
        syncSettings.attachmentCache
    }

    // MARK: - Public API

    func resetToDefaults() {
        settings = AppSettings()
        Log.info("SETTINGS", "Reset to defaults")
    }

    @MainActor
    func reloadSettings() async {
        if let loadTask {
            await loadTask.value
        }
        await startSettingsLoad()
        Log.info("SETTINGS", "Reloaded from config file")
        Log.info("SETTINGS", "Sync - guiAutoSync=\(guiAutoSync), autoFetchInterval=\(autoFetchInterval)s, fullSyncInterval=\(fullSyncInterval)s")

        // Restart sync timers with new settings
        SyncManager.shared.restartTimers()
    }
}

/// App settings from config.pkl [settings] section
/// Note: Sync-related settings are in SyncSettings (from [sync] section)
struct AppSettings: Codable {
    var notificationsEnabled: Bool = true
    var theme: String = "system"
    var loadRemoteImages: Bool = false  // Security: block tracking pixels by default
    var accentColor: String? = nil      // Hex color, e.g. "#3B82F6". Nil = system default.

    enum CodingKeys: String, CodingKey {
        case notificationsEnabled = "notifications_enabled"
        case theme
        case loadRemoteImages = "load_remote_images"
        case accentColor = "accent_color"
    }

    // Default initializer
    init() {}

    // Custom decoder that handles missing keys gracefully
    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        notificationsEnabled = try container.decodeIfPresent(Bool.self, forKey: .notificationsEnabled) ?? true
        theme = try container.decodeIfPresent(String.self, forKey: .theme) ?? "system"
        loadRemoteImages = try container.decodeIfPresent(Bool.self, forKey: .loadRemoteImages) ?? false
        accentColor = try container.decodeIfPresent(String.self, forKey: .accentColor)
    }
}
