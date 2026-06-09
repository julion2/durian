//
//  MailBackendProtocol.swift
//  Durian
//
//  Abstract protocol for mail backends
//

import Combine
import Foundation

// MARK: - Mail Backend Protocol

/// Subset protocol for search operations (used by SearchManager DI).
@MainActor
protocol SearchBackend {
    func searchAll(query: String, limit: Int) async -> [MailMessage]
}

/// Subset protocol for outbox operations (used by OutboxManager DI).
@MainActor
protocol OutboxBackend {
    func listOutbox() async -> [OutboxEntry]
}

/// Protocol defining the interface for mail backends.
/// EmailBackend conforms to this.
@MainActor
protocol MailBackendProtocol: ObservableObject {
    // MARK: - Connection State
    var isConnected: Bool { get }
    var connectionStatus: String { get }

    // MARK: - Data
    var folders: [MailFolder] { get }
    var emails: [MailMessage] { get }
    var isLoadingEmails: Bool { get }
    var loadingProgress: String { get }

    // MARK: - Connection
    func connect() async
    func disconnect() async

    // MARK: - Folder/Tag Selection
    /// Select a folder or tag to view
    func selectFolder(_ name: String) async

    // MARK: - Email Operations
    @discardableResult func fetchEmailBody(id: String) async -> MailMessage?
    func markAsRead(id: String) async throws
    func markAsUnread(id: String) async throws
    func deleteMessage(id: String) async throws

    // MARK: - Reload
    func reload() async
}
