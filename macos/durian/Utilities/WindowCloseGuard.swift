//
//  WindowCloseGuard.swift
//  Durian
//
//  NSViewRepresentable that intercepts window close to allow
//  async save before dismissal.
//

import AppKit
import SwiftUI

struct WindowCloseGuard: NSViewRepresentable {
    @Binding var allowClose: Bool
    @Binding var window: NSWindow?
    let onCloseAttempt: () -> Void

    // MARK: - Coordinator

    class Coordinator: NSObject, NSWindowDelegate {
        var originalDelegate: NSWindowDelegate?
        var allowClose = false
        var onCloseAttempt: (() -> Void)?

        func windowShouldClose(_ sender: NSWindow) -> Bool {
            if allowClose { return true }
            onCloseAttempt?()
            return false
        }

        // Forward all other delegate calls to SwiftUI's original delegate
        override func responds(to aSelector: Selector!) -> Bool {
            super.responds(to: aSelector) || (originalDelegate?.responds(to: aSelector) ?? false)
        }

        override func forwardingTarget(for aSelector: Selector!) -> Any? {
            if let d = originalDelegate, d.responds(to: aSelector) { return d }
            return super.forwardingTarget(for: aSelector)
        }
    }

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    // MARK: - NSViewRepresentable lifecycle

    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        DispatchQueue.main.async {
            guard let window = view.window else { return }
            self.window = window
            context.coordinator.originalDelegate = window.delegate
            context.coordinator.allowClose = allowClose
            context.coordinator.onCloseAttempt = onCloseAttempt
            window.delegate = context.coordinator
        }
        return view
    }

    func updateNSView(_ nsView: NSView, context: Context) {
        context.coordinator.allowClose = allowClose
        context.coordinator.onCloseAttempt = onCloseAttempt
    }
}
