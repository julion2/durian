import AppKit
import SwiftUI

// MARK: - Overlay Scrollbars Modifier

extension View {
    /// Makes ScrollView use thin overlay-style scrollbars (like modern macOS apps)
    func overlayScrollbars() -> some View {
        background(ScrollViewStyleConfigurator())
    }
}

/// Finds the enclosing NSScrollView and sets scrollerStyle to .overlay
private struct ScrollViewStyleConfigurator: NSViewRepresentable {
    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        // Delay to ensure view hierarchy is built
        DispatchQueue.main.async {
            if let scrollView = findScrollView(from: view) {
                scrollView.scrollerStyle = .overlay
            }
        }
        return view
    }

    func updateNSView(_ nsView: NSView, context: Context) {}

    /// Traverses up the view hierarchy to find the enclosing NSScrollView
    private func findScrollView(from view: NSView) -> NSScrollView? {
        var current: NSView? = view
        while let v = current {
            if let scrollView = v as? NSScrollView {
                return scrollView
            }
            current = v.superview
        }
        return nil
    }
}
