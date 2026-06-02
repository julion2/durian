//
//  DurianApp.swift
//  Durian
//
//  Created by Julian Schenker on 15.09.25.
//

import SwiftUI
import UserNotifications

// MARK: - Notification Delegate

class NotificationDelegate: NSObject, UNUserNotificationCenterDelegate {
    /// Handle notification click — route to the email thread
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        let userInfo = response.notification.request.content.userInfo
        if let threadId = userInfo["threadId"] as? String {
            Log.info("NOTIFICATIONS", "Clicked notification for thread \(threadId)")
            Task { @MainActor in
                AccountManager.shared.selectEmail(threadId: threadId)
            }
        }
        completionHandler()
    }

    /// Show notifications even when app is in foreground (needed for testing)
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound])
    }
}

// MARK: - App

@main
struct DurianApp: App {
    @Environment(\.openWindow) private var openWindow
    @StateObject private var profileManager = ProfileManager.shared
    @StateObject private var accountManager = AccountManager.shared
    @StateObject private var settingsManager = SettingsManager.shared
    @State private var cliVersion: String = ""

    /// Override the macOS global appearance with the per-app
    /// `settings.theme` from config.pkl: "light" / "dark" force the
    /// app chrome regardless of system setting; "system" (the default)
    /// or any unknown value returns nil, letting macOS decide.
    private var appColorScheme: ColorScheme? {
        switch settingsManager.settings.theme {
        case "light": return .light
        case "dark":  return .dark
        default:      return nil
        }
    }

    private static let notificationDelegate = NotificationDelegate()

    init() {
        // Setup sync manager (creates script + launchd agent if needed)
        SyncManager.shared.setup()

        // Set notification delegate before requesting permission
        UNUserNotificationCenter.current().delegate = Self.notificationDelegate

        // Request notification permission for sync warnings/errors
        requestNotificationPermission()
    }

    private func requestNotificationPermission() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { granted, error in
            if granted {
                Log.info("NOTIFICATIONS", "Permission granted")
            } else if let error = error {
                Log.error("NOTIFICATIONS", "Permission error - \(error.localizedDescription)")
            } else {
                Log.warning("NOTIFICATIONS", "Permission denied")
            }
        }
    }
    
    var body: some Scene {
        WindowGroup {
            ContentView()
                .preferredColorScheme(appColorScheme)
        }
        .commands {
            CommandGroup(replacing: .appInfo) {
                Button("About Durian") {
                    showAbout()
                }
            }

            CommandGroup(replacing: .appSettings) {
                Button("Preferences...") {
                    openConfig()
                }
                .keyboardShortcut(",", modifiers: .command)
            }
            
            CommandGroup(after: .newItem) {
                Button("Reload Keymaps") {
                    KeymapsManager.shared.reloadKeymaps()
                }
                .keyboardShortcut("k", modifiers: [.command, .shift])
                
                Button("Reload Config") {
                    SettingsManager.shared.reloadSettings()
                    ProfileManager.shared.loadProfiles()
                }
                .keyboardShortcut("c", modifiers: [.command, .shift])
                
                Divider()
                
                Button("Full Sync") {
                    Task {
                        await SyncManager.shared.fullSync()
                    }
                }
                .keyboardShortcut("r", modifiers: [.command, .shift])
            }
            
            // Profile Menu
            CommandMenu("Profiles") {
                ForEach(Array(profileManager.profiles.enumerated()), id: \.element.id) { index, profile in
                    Button(action: {
                        Task {
                            await accountManager.switchProfile(profile)
                        }
                    }) {
                        HStack {
                            if profile == profileManager.currentProfile {
                                Image(systemName: "checkmark")
                            }
                            Text(profile.name)
                        }
                    }
                    .keyboardShortcut(KeyEquivalent(Character(String(index + 1))), modifiers: .command)
                }
            }
        }
        
        // Compose Window - supports multiple windows via UUID
        WindowGroup("New Message", for: UUID.self) { $draftId in
            if let draftId = draftId {
                ComposeWindow(draftId: draftId)
            }
        }
        .defaultSize(width: 650, height: 550)
    }
    
    private func showAbout() {
        Task {
            if let backend = accountManager.emailBackend,
               let info = await backend.fetchVersion() {
                cliVersion = "\(info.version) (\(info.commit))"
            }
            await MainActor.run {
                let guiVersion = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"

                let credits = NSMutableAttributedString()
                let style: [NSAttributedString.Key: Any] = [.font: NSFont.systemFont(ofSize: 11)]
                let dimStyle: [NSAttributedString.Key: Any] = [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor]
                credits.append(NSAttributedString(string: "CLI: \(cliVersion.isEmpty ? "not connected" : cliVersion)\n\n", attributes: style))
                credits.append(NSAttributedString(string: "A macOS email client for power users.", attributes: dimStyle))
                NSApp.orderFrontStandardAboutPanel(options: [
                    .credits: credits,
                    .applicationVersion: guiVersion,
                ])
            }
        }
    }

    private func openConfig() {
        let configURL = FileManager.default.durianConfigURL().appendingPathComponent("config.pkl")
        NSWorkspace.shared.open(configURL)
    }
}
