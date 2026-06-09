//
//  ScrollViewFinder.swift
//  Durian
//
//  Captures a reference to the enclosing NSScrollView for programmatic scrolling.
//

import SwiftUI

struct ScrollViewFinder: NSViewRepresentable {
    @Binding var scrollView: NSScrollView?

    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        DispatchQueue.main.async {
            scrollView = view.enclosingScrollView
        }
        return view
    }

    func updateNSView(_ nsView: NSView, context: Context) {
        if scrollView == nil {
            DispatchQueue.main.async {
                scrollView = nsView.enclosingScrollView
            }
        }
    }
}
