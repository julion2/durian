//
//  ConfigManager.swift
//  Durian
//
//  Manages app configuration from config.pkl
//  Note: IMAP/SMTP config is handled by CLI, GUI only needs account names/emails
//

import Foundation

// MARK: - Config Models

/// Simplified account info - GUI only needs name/email for account picker
/// IMAP/SMTP configuration is handled by the durian CLI
struct MailAccount: Codable {
    let name: String
    let email: String
    let defaultSignature: String?
    let notifications: Bool?
    let calendar: MailAccountCalendar?

    var calendarEnabled: Bool { calendar?.enabled ?? true }

    enum CodingKeys: String, CodingKey {
        case name, email, notifications, calendar
        case defaultSignature = "default_signature"
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        name = try container.decode(String.self, forKey: .name)
        email = try container.decode(String.self, forKey: .email)
        defaultSignature = try container.decodeIfPresent(String.self, forKey: .defaultSignature)
        notifications = try container.decodeIfPresent(Bool.self, forKey: .notifications)
        calendar = try container.decodeIfPresent(MailAccountCalendar.self, forKey: .calendar)

        // Skip IMAP/SMTP/Auth sections - they're handled by CLI
    }

    init(name: String, email: String, defaultSignature: String? = nil, notifications: Bool? = nil,
         calendar: MailAccountCalendar? = nil)
    {
        self.name = name
        self.email = email
        self.defaultSignature = defaultSignature
        self.notifications = notifications
        self.calendar = calendar
    }
}

struct MailAccountCalendar: Codable {
    let enabled: Bool

    init(enabled: Bool = true) {
        self.enabled = enabled
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        enabled = try container.decodeIfPresent(Bool.self, forKey: .enabled) ?? true
    }
}

/// Sync settings from [sync] section
/// These control GUI auto-sync behavior and intervals
struct SyncSettings: Codable {
    var mode: String = "bidirectional"
    var guiAutoSync: Bool = true
    var autoFetchInterval: TimeInterval = 120.0
    var fullSyncInterval: TimeInterval = 7200
    var attachmentCache: AttachmentCacheSettings = AttachmentCacheSettings()

    enum CodingKeys: String, CodingKey {
        case mode
        case guiAutoSync = "gui_auto_sync"
        case autoFetchInterval = "auto_fetch_interval"
        case fullSyncInterval = "full_sync_interval"
        case attachmentCache = "attachment_cache"
    }

    init() {}

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        mode = try container.decodeIfPresent(String.self, forKey: .mode) ?? "bidirectional"
        guiAutoSync = try container.decodeIfPresent(Bool.self, forKey: .guiAutoSync) ?? true
        autoFetchInterval = try container.decodeIfPresent(TimeInterval.self, forKey: .autoFetchInterval) ?? 120.0
        fullSyncInterval = try container.decodeIfPresent(TimeInterval.self, forKey: .fullSyncInterval) ?? 7200
        attachmentCache = try container.decodeIfPresent(AttachmentCacheSettings.self, forKey: .attachmentCache) ?? AttachmentCacheSettings()
    }
}

/// Attachment cache settings from [sync.attachment_cache] section
struct AttachmentCacheSettings: Codable {
    var maxSizeMB: Int = 100
    var ttlDays: Int = 7

    enum CodingKeys: String, CodingKey {
        case maxSizeMB = "max_size_mb"
        case ttlDays = "ttl_days"
    }

    init() {}

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        maxSizeMB = try container.decodeIfPresent(Int.self, forKey: .maxSizeMB) ?? 100
        ttlDays = try container.decodeIfPresent(Int.self, forKey: .ttlDays) ?? 7
    }

    var maxSizeBytes: Int64 { Int64(maxSizeMB) * 1_048_576 }
    var ttl: TimeInterval { TimeInterval(ttlDays) * 86_400 }
}

struct AppConfig: Codable {
    let accounts: [MailAccount]
    let settings: AppSettings
    let sync: SyncSettings
    let signatures: [String: String]

    init(accounts: [MailAccount], settings: AppSettings = AppSettings(), sync: SyncSettings = SyncSettings(), signatures: [String: String] = [:]) {
        self.accounts = accounts
        self.settings = settings
        self.sync = sync
        self.signatures = signatures
    }

    enum CodingKeys: String, CodingKey {
        case accounts, settings, sync, signatures
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        accounts = try container.decodeIfPresent([MailAccount].self, forKey: .accounts) ?? []
        settings = try container.decodeIfPresent(AppSettings.self, forKey: .settings) ?? AppSettings()
        sync = try container.decodeIfPresent(SyncSettings.self, forKey: .sync) ?? SyncSettings()
        signatures = try container.decodeIfPresent([String: String].self, forKey: .signatures) ?? [:]
    }
}

// MARK: - Config Manager

class ConfigManager {
    static let shared = ConfigManager()

    typealias ConfigEvaluator = (URL) throws -> AppConfig
    typealias ParseErrorHandler = (String) -> Void

    // The config is accessed from many contexts (Views on MainActor, but also
    // background Tasks in AccountManager/DraftService). An NSLock guards the
    // stored value so concurrent reads/writes are race-free without forcing
    // @MainActor on every call site.
    private let lock = NSLock()
    private var _config: AppConfig?
    private var _lastParseError: String?
    private let configURL: URL
    private let evaluator: ConfigEvaluator
    private let parseErrorHandler: ParseErrorHandler

    private var config: AppConfig? {
        get {
            lock.lock()
            defer { lock.unlock() }
            return _config
        }
        set {
            lock.lock()
            defer { lock.unlock() }
            _config = newValue
        }
    }

    private func setLastParseError(_ error: String?) {
        lock.lock()
        defer { lock.unlock() }
        _lastParseError = error
    }

    init(
        configURL: URL = FileManager.default.durianConfigURL().appendingPathComponent("config.pkl"),
        evaluator: @escaping ConfigEvaluator = { try PklEvaluator.evalSync(AppConfig.self, from: $0) },
        parseErrorHandler: @escaping ParseErrorHandler = ConfigManager.showParseError
    ) {
        self.configURL = configURL
        self.evaluator = evaluator
        self.parseErrorHandler = parseErrorHandler
        loadConfigBlocking()
    }

    /// Test-only initializer: inject config directly, skip file loading
    init(config: AppConfig) {
        configURL = FileManager.default.durianConfigURL().appendingPathComponent("config.pkl")
        evaluator = { try PklEvaluator.evalSync(AppConfig.self, from: $0) }
        parseErrorHandler = ConfigManager.showParseError
        _config = config
    }

    /// Synchronous load via pkl CLI subprocess.
    /// Uses PklEvaluator.evalSync (Process + waitUntilExit) to avoid
    /// Swift Concurrency deadlocks from mixing Task.detached with semaphores.
    private func loadConfigBlocking() {
        guard FileManager.default.fileExists(atPath: configURL.path) else {
            config = nil
            setLastParseError(nil)
            Log.warning("CONFIG", "Config not found at \(configURL.path)")
            return
        }

        do {
            config = try evaluator(configURL)
            setLastParseError(nil)
            Log.info("CONFIG", "Loaded config from \(configURL.path)")
        } catch {
            config = nil
            let detail = error.localizedDescription
            setLastParseError(detail)
            Log.error("CONFIG", "Failed to load config: \(detail)")
            parseErrorHandler("Configuration failed to parse: \(detail). The bundled GUI schema may be out of date — run ./macos/install.sh to rebuild.")
        }
    }

    private static func showParseError(_ message: String) {
        Task { @MainActor in
            BannerManager.shared.showCritical(title: "Configuration Error", message: message)
        }
    }

    // MARK: - Public API

    var lastParseError: String? {
        lock.lock()
        defer { lock.unlock() }
        return _lastParseError
    }

    func getAccounts() -> [MailAccount] {
        config?.accounts ?? []
    }

    func getSettings() -> AppSettings {
        config?.settings ?? AppSettings()
    }

    func getSignatures() -> [String: String] {
        config?.signatures ?? [:]
    }

    func getSyncSettings() -> SyncSettings {
        config?.sync ?? SyncSettings()
    }

    /// Reload config from disk (call after editing config.pkl)
    func reloadConfig() {
        Log.info("CONFIG", "Reloading config...")
        loadConfigBlocking()
    }

    func updateSettings(_ newSettings: AppSettings) {
        guard let currentConfig = config else { return }

        let updatedConfig = AppConfig(accounts: currentConfig.accounts, settings: newSettings, sync: currentConfig.sync, signatures: currentConfig.signatures)
        config = updatedConfig
        // Settings are now managed in config.pkl — edit the file directly
    }
}
