//
//  PklEvaluator.swift
//  Durian
//
//  Evaluates .pkl config files via pkl-swift library.
//  Schemas are bundled as app resources and served via modulePaths.
//

import Foundation
import PklSwift

enum PklEvaluator {
    private static let pklPathDefaultsKey = "PklExecPath"

    /// Resolve the `pkl` binary once, robustly, even when launched from
    /// Dock/Spotlight — where macOS apps inherit only the minimal launchd PATH
    /// (`/usr/bin:/bin:/usr/sbin:/sbin`), not the user's shell PATH. Hardcoding
    /// a couple of install dirs breaks for anyone whose pkl lives elsewhere
    /// (nix, asdf, a custom prefix), so this is a cascade that ends in an
    /// authoritative login-shell probe and caches whatever it finds.
    static let resolvedPkl: String? = resolvePkl()

    private static func resolvePkl() -> String? {
        let fm = FileManager.default

        // 1. Explicit override — respected verbatim.
        if let env = ProcessInfo.processInfo.environment["PKL_EXEC"],
           fm.isExecutableFile(atPath: env)
        {
            return env
        }

        // 2. Path discovered on a previous launch — avoids re-probing the shell
        //    every start once we've found it once.
        if let saved = UserDefaults.standard.string(forKey: pklPathDefaultsKey),
           fm.isExecutableFile(atPath: saved)
        {
            persist(saved)
            return saved
        }

        // 3. Common absolute install locations (fast, no subprocess). Covers
        //    Homebrew (arm/intel), /usr/local, nix per-user + system profiles,
        //    and ~/.local/bin — but is explicitly NOT the source of truth.
        let user = NSUserName()
        let home = NSHomeDirectory()
        let candidates = [
            "/opt/homebrew/bin/pkl",
            "/usr/local/bin/pkl",
            "/etc/profiles/per-user/\(user)/bin/pkl",
            "\(home)/.nix-profile/bin/pkl",
            "/run/current-system/sw/bin/pkl",
            "\(home)/.local/bin/pkl",
        ]
        if let hit = candidates.first(where: { fm.isExecutableFile(atPath: $0) }) {
            persist(hit)
            return hit
        }

        // 4. Whatever PATH the process did inherit.
        if let hit = findInPath("pkl") {
            persist(hit)
            return hit
        }

        // 5. Authoritative fallback: ask the user's login shell, which sources
        //    their profile (Homebrew, nix, asdf, ...) regardless of how pkl was
        //    installed. This is the non-hardcoded answer; ~100 ms, once.
        if let hit = probeLoginShell("pkl") {
            persist(hit)
            return hit
        }

        Log.error("CONFIG", "pkl binary not found in PKL_EXEC, common paths, PATH, or login shell")
        return nil
    }

    /// Cache a resolved path for this process (PKL_EXEC, consumed by pkl-swift)
    /// and across launches (UserDefaults).
    private static func persist(_ path: String) {
        setenv("PKL_EXEC", path, 1)
        UserDefaults.standard.set(path, forKey: pklPathDefaultsKey)
    }

    private static func findInPath(_ name: String) -> String? {
        let paths = (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":")
        for p in paths {
            let full = "\(p)/\(name)"
            if FileManager.default.isExecutableFile(atPath: full) {
                return full
            }
        }
        return nil
    }

    /// Resolve `name` through the user's login shell so their profile PATH
    /// (nix/Homebrew/asdf/...) applies, independent of how the app was launched.
    private static func probeLoginShell(_ name: String) -> String? {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let process = Process()
        process.executableURL = URL(fileURLWithPath: shell)
        // -l sources login files (where nix/Homebrew put pkl on PATH); command -v
        // prints the resolved absolute path.
        process.arguments = ["-lc", "command -v \(name)"]
        let stdout = Pipe()
        process.standardOutput = stdout
        process.standardError = Pipe()
        do {
            try process.run()
            process.waitUntilExit()
            guard process.terminationStatus == 0 else { return nil }
            let data = stdout.fileHandleForReading.readDataToEndOfFile()
            let path = String(data: data, encoding: .utf8)?
                .trimmingCharacters(in: .whitespacesAndNewlines)
            if let path, !path.isEmpty, FileManager.default.isExecutableFile(atPath: path) {
                return path
            }
        } catch {
            return nil
        }
        return nil
    }

    /// Schema directory inside the app bundle.
    private static let schemaDir: String? = {
        guard let resourcePath = Bundle.main.resourcePath else { return nil }
        for candidate in [
            (resourcePath as NSString).appendingPathComponent("schema"),
            resourcePath,
        ] {
            let test = (candidate as NSString).appendingPathComponent("Config.pkl")
            if FileManager.default.fileExists(atPath: test) {
                return candidate
            }
        }
        return nil
    }()

    /// Evaluate a .pkl file and decode directly into the given type via pkl-swift.
    static func eval<T: Decodable>(_ type: T.Type, from url: URL) async throws -> T {
        guard resolvedPkl != nil else { throw PklEvalError.binaryNotFound }
        var options = EvaluatorOptions.preconfigured
        if let sd = schemaDir {
            options.modulePaths = [sd]
        }

        return try await withEvaluator(options: options) { evaluator in
            try await evaluator.evaluateModule(
                source: .path(url.path),
                as: type
            )
        }
    }

    /// Synchronous eval via pkl CLI subprocess — safe to call from init()
    /// without risking Swift Concurrency deadlocks.
    static func evalSync<T: Decodable>(_ type: T.Type, from url: URL) throws -> T {
        guard let pklBin = resolvedPkl else { throw PklEvalError.binaryNotFound }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: pklBin)

        var args = ["eval", "--format", "json"]
        if let sd = schemaDir {
            args += ["--module-path", sd, "--allowed-modules", "file:,modulepath:"]
        }
        args.append(url.path)
        process.arguments = args

        let stdout = Pipe()
        let stderr = Pipe()
        process.standardOutput = stdout
        process.standardError = stderr

        try process.run()
        process.waitUntilExit()

        guard process.terminationStatus == 0 else {
            let errMsg = String(data: stderr.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? "unknown error"
            throw PklEvalError.evaluationFailed(errMsg)
        }

        let data = stdout.fileHandleForReading.readDataToEndOfFile()
        return try JSONDecoder().decode(type, from: data)
    }
}

enum PklEvalError: LocalizedError {
    case evaluationFailed(String)
    case binaryNotFound

    var errorDescription: String? {
        switch self {
        case .evaluationFailed(let msg):
            return "pkl eval failed: \(msg)"
        case .binaryNotFound:
            return "pkl binary not found. Install pkl (brew install pkl) or set PKL_EXEC; profiles and config are unavailable until then."
        }
    }
}
