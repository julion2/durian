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
        // 1. Check ~/.local/bin/durian
        let homeURL = homeDirectoryForCurrentUser
        let localBinURL = homeURL.appendingPathComponent(".local/bin/durian")
        if fileExists(atPath: localBinURL.path) {
            return localBinURL.path
        }

        // 2. Check standard Homebrew path
        let brewPath = "/opt/homebrew/bin/durian"
        if fileExists(atPath: brewPath) {
            return brewPath
        }

        // 3. Fallback to /usr/local/bin
        let usrLocalPath = "/usr/local/bin/durian"
        if fileExists(atPath: usrLocalPath) {
            return usrLocalPath
        }

        return nil
    }
}
