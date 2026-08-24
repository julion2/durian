import Foundation

extension FileManager {
    /// Returns the durian config directory, respecting XDG_CONFIG_HOME.
    /// Falls back to ~/.config/durian/ if XDG_CONFIG_HOME is unset.
    func durianConfigURL() -> URL {
        if let xdg = ProcessInfo.processInfo.environment["XDG_CONFIG_HOME"], !xdg.isEmpty {
            return URL(fileURLWithPath: xdg).appendingPathComponent("durian")
        }
        return homeDirectoryForCurrentUser.appendingPathComponent(".config/durian")
    }

    func resolveDurianPath() -> String? {
        // Match the CLI a terminal would launch instead of maintaining a
        // second resolution order that can select a different installation.
        if let path = ProcessInfo.processInfo.environment["PATH"] {
            for directory in path.split(separator: ":") {
                let candidate = URL(fileURLWithPath: String(directory)).appendingPathComponent("durian").path
                if isExecutableFile(atPath: candidate) {
                    return candidate
                }
            }
        }

        // Finder launches can have a minimal PATH. Keep deterministic
        // fallbacks for the supported installation locations.
        let fallbacks = [
            "/opt/homebrew/bin/durian",
            "/usr/local/bin/durian",
            homeDirectoryForCurrentUser.appendingPathComponent(".local/bin/durian").path,
        ]
        for candidate in fallbacks where isExecutableFile(atPath: candidate) {
            return candidate
        }

        return nil
    }
}
