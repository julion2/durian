//
//  AppServer.swift
//  Durian
//
//  Single source of truth for the local `durian serve` endpoint.
//

import Foundation

/// The local durian serve endpoint. The port is scoped by bundle id so the
/// Nightly and Release apps never contend for the same port — and never kill
/// each other's server. serve is told which port to use via `--port`.
enum AppServer {
    /// Local HTTP contract required by this GUI. Increment when a GUI release
    /// requires an incompatible or newly mandatory server capability.
    static let apiProtocol = 1

    /// 9723 for Release, 9724 for Nightly (bundle id ends in ".nightly").
    static let port: Int = {
        let id = Bundle.main.bundleIdentifier ?? ""
        return id.hasSuffix(".nightly") ? 9724 : 9723
    }()

    static var apiBaseURL: URL { URL(string: "http://localhost:\(port)/api/v1")! }
    static var eventsURL: URL { URL(string: "http://localhost:\(port)/api/v1/events")! }
    static var localDraftsURL: URL { URL(string: "http://localhost:\(port)/api/v1/local-drafts")! }
}
