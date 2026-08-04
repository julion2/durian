import Foundation
import os

enum Log {
    private static let subsystem = Bundle.main.bundleIdentifier ?? "org.js-lab.durian"
    private static let lock = NSLock()
    private static var loggers: [String: Logger] = [:]
    private static var signposters: [String: OSSignposter] = [:]

    private static func logger(for category: String) -> Logger {
        lock.lock()
        defer { lock.unlock() }
        if let cached = loggers[category] { return cached }
        let l = Logger(subsystem: subsystem, category: category)
        loggers[category] = l
        return l
    }

    static func signposter(for category: String) -> OSSignposter {
        lock.lock()
        defer { lock.unlock() }
        if let cached = signposters[category] { return cached }
        let s = OSSignposter(subsystem: subsystem, category: category)
        signposters[category] = s
        return s
    }

    static func debug(_ cat: String, _ msg: String) {
        logger(for: cat).debug("\(msg, privacy: .public)")
    }

    static func info(_ cat: String, _ msg: String) {
        logger(for: cat).info("\(msg, privacy: .public)")
    }

    static func warning(_ cat: String, _ msg: String) {
        logger(for: cat).warning("\(msg, privacy: .public)")
    }

    static func error(_ cat: String, _ msg: String) {
        logger(for: cat).error("\(msg, privacy: .public)")
    }

    /// Logs a single sensitive value (mail subject, sender, body preview) on a
    /// channel that is redacted in the on-disk unified log store (`log stream`
    /// / sysdiagnose) and visible only when a debugger is attached. Use this
    /// instead of interpolating mail content into debug()/info()/etc — the
    /// grep gate (.github/scripts/swift-log-grep-gate.sh) blocks the latter,
    /// because those force `privacy: .public` and would leak the value to any
    /// forensic-image reader (ADR-0001 Persona 1). `label` is a non-sensitive
    /// tag (e.g. "subject"); `value` is the sensitive content.
    static func sensitive(_ cat: String, _ label: String, _ value: String) {
        // Compiled out of release builds entirely — the shipped app never
        // emits the value, so there is nothing for a forensic-image reader to
        // recover. In Debug builds it goes out at .debug with `value` marked
        // .private: `log stream` / sysdiagnose show `label=<private>`, and the
        // real value resolves only under an attached debugger.
        #if DEBUG
        logger(for: cat).debug("\(label, privacy: .public)=\(value, privacy: .private)")
        #endif
    }
}
