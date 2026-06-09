//
//  NonScrollingWebView.swift
//  Durian
//
//  A WKWebView that doesn't scroll and sizes itself to fit its content.
//  Used for embedding HTML content in a parent ScrollView.
//

import SwiftUI
import WebKit

// MARK: - Shared WebKit infrastructure

/// Pooled WebKit resources reused across every email-card WebView in the
/// app. Without these, each NonScrollingWebView+EditableWebView creates its
/// own WKWebViewConfiguration → its own WKProcessPool → its own WebContent
/// process. A 20-message thread becomes 20+ WebContent processes, all
/// independently suspended/resumed by macOS on every window-occlusion
/// transition.
///
/// Activity Monitor pre-pooling: ~16k `WebKit:ProcessSuspension` events / 12h.
/// Expected post-pooling: cuts to one process per pool, throttled by
/// `suspendsActivityWhenWindowIsOccluded` when the window goes background.
enum SharedWebKit {
    /// Process pool shared by every read-only email-rendering WebView
    /// (NonScrollingWebView). Lifetime = app process.
    static let readOnlyPool = WKProcessPool()

    /// Separate pool for the compose editor — keeps a stale editor renderer
    /// from sharing a process with the reader fleet. The editor has its own
    /// userContentController + message handlers, isolation is desirable.
    static let composePool = WKProcessPool()

    /// Ephemeral, in-memory data store shared across read-only WebViews.
    /// Email HTML is CSP'd (`script-src 'none'`) so cookies/localStorage can
    /// never be written. Sharing the HTTP cache means the same tracking
    /// pixel / remote image across N cards is fetched once, not N times.
    static let readOnlyDataStore: WKWebsiteDataStore = .nonPersistent()

    /// Build a WKWebViewConfiguration pre-wired with the shared read-only
    /// pool + data store + the read-only JS/window prefs. Returns a fresh
    /// instance per call — the SHARED state is the inner pool, not the
    /// config object.
    static func makeReadOnlyConfig() -> WKWebViewConfiguration {
        let config = WKWebViewConfiguration()
        config.processPool = readOnlyPool
        config.websiteDataStore = readOnlyDataStore
        // JS is needed for one-shot height measurement; CSP blocks all
        // inline + external scripts anyway.
        config.defaultWebpagePreferences.allowsContentJavaScript = true
        config.preferences.javaScriptCanOpenWindowsAutomatically = false
        return config
    }

    /// Apply the runtime-only knobs that aren't part of WKWebViewConfiguration:
    /// occlusion throttling, scroll passthrough flags, etc.
    static func applyEnergyDefaults(to webView: WKWebView) {
        // Throttle JS timers + CSS animations when the window is occluded
        // (background, behind another window, etc.). Default is `false` —
        // WebViews keep ticking layout/timer work even when invisible.
        if webView.responds(to: NSSelectorFromString("setSuspendsActivityWhenWindowIsOccluded:")) {
            webView.setValue(true, forKey: "suspendsActivityWhenWindowIsOccluded")
        }
    }
}

// MARK: - Custom WebView that passes scroll events to parent

/// A WKWebView subclass that passes scroll wheel events to its parent ScrollView
class ScrollPassthroughWebView: WKWebView {
    override func scrollWheel(with event: NSEvent) {
        // Pass scroll events to parent instead of handling them
        self.nextResponder?.scrollWheel(with: event)
    }
}

// MARK: - NonScrollingWebView

/// A WebView that sizes itself to fit its HTML content without internal scrolling
struct NonScrollingWebView: NSViewRepresentable {
    let html: String
    let theme: String              // "light", "dark", "system" (default)
    let loadRemoteImages: Bool     // Security: block tracking pixels by default
    let emailId: String            // Track which email this WebView belongs to (for race condition prevention)
    @Binding var contentHeight: CGFloat
    
    init(html: String, theme: String = "system", loadRemoteImages: Bool = false, emailId: String = "", contentHeight: Binding<CGFloat>) {
        self.html = html
        self.theme = theme
        self.loadRemoteImages = loadRemoteImages
        self.emailId = emailId
        self._contentHeight = contentHeight
    }
    
    func makeNSView(context: Context) -> WKWebView {
        // Use the shared read-only config — same WKProcessPool and
        // WKWebsiteDataStore across every email card in the app.
        let webView = ScrollPassthroughWebView(frame: .zero, configuration: SharedWebKit.makeReadOnlyConfig())
        SharedWebKit.applyEnergyDefaults(to: webView)
        #if DEBUG
        webView.isInspectable = true
        #endif
        webView.navigationDelegate = context.coordinator

        // Transparent background (let parent handle background color)
        webView.setValue(false, forKey: "drawsBackground")
        webView.wantsLayer = true
        webView.layer?.backgroundColor = NSColor.clear.cgColor

        context.coordinator.webView = webView
        context.coordinator.parent = self

        return webView
    }

    static func dismantleNSView(_ nsView: WKWebView, coordinator: Coordinator) {
        // Stop any in-flight HTML/image load on teardown so we don't keep
        // the WebContent process awake doing work whose result no view
        // will ever consume.
        nsView.stopLoading()
    }
    
    func updateNSView(_ webView: WKWebView, context: Context) {
        // Update parent reference for height binding
        context.coordinator.parent = self
        
        let styledHTML = buildSecureHTML(html: html, theme: theme, loadRemoteImages: loadRemoteImages)
        
        // Only reload if HTML actually changed - prevents infinite loop
        // (contentHeight binding triggers re-render → updateNSView → reload → didFinish → height update → loop)
        if context.coordinator.lastLoadedHTML != styledHTML {
            context.coordinator.lastLoadedHTML = styledHTML
            context.coordinator.loadedForEmailId = emailId  // Track which email this load is for
            webView.loadHTMLString(styledHTML, baseURL: nil)
        }
    }
    
    private func buildSecureHTML(html: String, theme: String, loadRemoteImages: Bool) -> String {
        // Dynamic CSP based on loadRemoteImages setting
        // Note: script-src stays 'none' — evaluateJavaScript() from Swift bypasses CSP,
        // so height measurement still works while inline <script> tags are blocked.
        let csp: String
        if loadRemoteImages {
            csp = "default-src 'none'; style-src 'unsafe-inline'; img-src data: cid: https:; font-src https: data:;"
        } else {
            csp = "default-src 'none'; style-src 'unsafe-inline'; img-src data: cid:;"
        }
        
        // Theme CSS with robust dark mode (CSS filter invert)
        let themeCSS: String
        switch theme {
        case "light":
            themeCSS = """
                body { background-color: transparent; color: #000000; }
                a { color: #0066cc; }
            """
        case "dark":
            themeCSS = ""
        default: // "system" - follow system preference via @media query
            themeCSS = """
                @media (prefers-color-scheme: light) {
                    body { background-color: transparent; color: #000000; }
                    a { color: #0066cc; }
                }
                @media (prefers-color-scheme: dark) {
                }
            """
        }
        
        return """
        <!DOCTYPE html>
        <html>
        <head>
            <meta charset="UTF-8">
            <meta name="viewport" content="width=device-width, initial-scale=1.0">
            <meta http-equiv="Content-Security-Policy" content="\(csp)">
            <style>
                * { margin: 0; padding: 0; box-sizing: border-box; }
                html, body {
                    background-color: transparent;
                    overflow: hidden;
                }
                body {
                    font-family: -apple-system, BlinkMacSystemFont, sans-serif;
                    font-size: 14px;
                    line-height: 1.5;
                    padding: 0;
                    color-scheme: light dark;
                }
                img { max-width: 100%; height: auto; }
                \(themeCSS)
            </style>
        </head>
        <body>\(html)</body>
        </html>
        """
    }
    
    func makeCoordinator() -> Coordinator {
        Coordinator()
    }
    
    class Coordinator: NSObject, WKNavigationDelegate {
        weak var webView: WKWebView?
        var parent: NonScrollingWebView?
        var lastLoadedHTML: String?  // Track to prevent reload loops
        var loadedForEmailId: String?  // Track which email we loaded for (race condition prevention)
        
        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            // Capture email ID at callback time to detect stale callbacks
            let expectedEmailId = loadedForEmailId

            // Apply dark mode color transformation if needed
            let isDark = parent?.theme == "dark" ||
                (parent?.theme == "system" && NSApp.effectiveAppearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua)
            if isDark {
                webView.evaluateJavaScript(DarkModeTransform.js) { _, _ in
                    // Measure height after dark mode transform (colors may affect layout)
                    webView.evaluateJavaScript("document.body.scrollHeight") { [weak self] result, _ in
                        if let height = result as? CGFloat, height > 0 {
                            DispatchQueue.main.async {
                                guard self?.parent?.emailId == expectedEmailId else { return }
                                self?.parent?.contentHeight = height
                            }
                        }
                    }
                }
            } else {
                // Light mode: just measure height
                webView.evaluateJavaScript("document.body.scrollHeight") { [weak self] result, _ in
                    if let height = result as? CGFloat, height > 0 {
                        DispatchQueue.main.async {
                            guard self?.parent?.emailId == expectedEmailId else { return }
                            self?.parent?.contentHeight = height
                        }
                    }
                }
            }
        }

        
        // Links open in default browser
        func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            if navigationAction.navigationType == .linkActivated,
               let url = navigationAction.request.url {
                NSWorkspace.shared.open(url)
                decisionHandler(.cancel)
            } else {
                decisionHandler(.allow)
            }
        }
    }
}
