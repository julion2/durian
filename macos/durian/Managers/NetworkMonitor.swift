//
//  NetworkMonitor.swift
//  Durian
//
//  Monitors network connectivity using NWPathMonitor
//

import Foundation
import Network

@MainActor
class NetworkMonitor: ObservableObject {
    static let shared = NetworkMonitor()

    @Published private(set) var isConnected: Bool = true
    @Published private(set) var showReconnectedBanner: Bool = false

    private let monitor = NWPathMonitor()
    private let queue = DispatchQueue(label: "durian.NetworkMonitor")
    private var debounceTask: Task<Void, Never>?

    private init() {
        monitor.pathUpdateHandler = { [weak self] path in
            Task { @MainActor [weak self] in
                guard let self = self else { return }

                let nowConnected = (path.status == .satisfied)

                // Skip if state hasn't changed
                guard nowConnected != isConnected else { return }

                // Debounce: NWPathMonitor fires rapidly during WiFi transitions.
                // Wait 2s before committing the state change — if it flaps back,
                // the previous task is cancelled and no update is published.
                debounceTask?.cancel()
                debounceTask = Task { @MainActor in
                    try? await Task.sleep(nanoseconds: 2_000_000_000)
                    guard !Task.isCancelled else { return }

                    let wasConnected = self.isConnected
                    self.isConnected = nowConnected

                    if !wasConnected && nowConnected {
                        Log.info("NETWORK", "Back online")
                        self.showReconnectedBanner = true
                        try? await Task.sleep(nanoseconds: 3_000_000_000)
                        self.showReconnectedBanner = false
                    } else if wasConnected && !nowConnected {
                        Log.info("NETWORK", "Offline")
                    }
                }
            }
        }
        monitor.start(queue: queue)
        Log.info("NETWORK", "Monitor started")
    }
}
