//
//  OutboxManager.swift
//  Durian
//
//  Tracks outbox state (pending count) for badge display.
//

import Combine
import Foundation

@MainActor
class OutboxManager: ObservableObject {
    static let shared = OutboxManager()

    @Published var pendingCount: Int = 0

    private let backendProvider: () -> (any OutboxBackend)?

    private init() {
        backendProvider = { AccountManager.shared.emailBackend }
    }

    /// Test-only initializer for dependency injection
    init(backend: @escaping () -> (any OutboxBackend)?) {
        backendProvider = backend
    }

    /// Refresh the pending count from the server.
    func refresh() {
        Task {
            guard let backend = backendProvider() else { return }
            let items = await backend.listOutbox()
            pendingCount = items.count
            Log.debug("OUTBOX", "Pending count: \(pendingCount)")
        }
    }
}
