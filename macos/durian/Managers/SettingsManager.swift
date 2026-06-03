import Combine
import Foundation

class SettingsManager: ObservableObject {
    static let shared = SettingsManager()

    @Published var settings: AppSettings = AppSettings()
    private var cancellables = Set<AnyCancellable>()

    private init() {
        loadSettings()
        setupAutoSave()
    }

    private func loadSettings() {
        settings = ConfigManager.shared.getSettings()
    }

    private func setupAutoSave() {
        // Auto-save when settings change
        $settings
            .dropFirst() // Skip initial value
            .debounce(for: .seconds(0.5), scheduler: DispatchQueue.main)
            .sink { [weak self] _ in
                self?.saveSettings()
            }
            .store(in: &cancellables)
    }

    private func saveSettings() {
        ConfigManager.shared.updateSettings(settings)
    }

    // MARK: - Sync Settings (read from [sync] section)

    /// Sync settings are read-only from config.pkl [sync] section
    var syncSettings: SyncSettings {
        ConfigManager.shared.getSyncSettings()
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
    func reloadSettings() {
        ConfigManager.shared.reloadConfig()
        settings = ConfigManager.shared.getSettings()
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