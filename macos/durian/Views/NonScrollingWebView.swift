//
//  NonScrollingWebView.swift
//  Durian
//
//  A WKWebView that doesn't scroll and sizes itself to fit its content.
//  Used for embedding HTML content in a parent ScrollView.
//

import SwiftUI
import WebKit

// MARK: - Custom WebView that passes scroll events to parent

/// A WKWebView subclass that passes scroll wheel events to its parent ScrollView
class ScrollPassthroughWebView: WKWebView {
    override func scrollWheel(with event: NSEvent) {
        // Pass scroll events to parent instead of handling them
        nextResponder?.scrollWheel(with: event)
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
        _contentHeight = contentHeight
    }

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()

        // Enable JavaScript for height measurement only
        // Note: We need JS enabled to measure content height, but CSP blocks external scripts
        config.defaultWebpagePreferences.allowsContentJavaScript = true

        // SECURITY: Disable auto-opening windows
        config.preferences.javaScriptCanOpenWindowsAutomatically = false

        // Use custom WebView that passes scroll events to parent
        let webView = ScrollPassthroughWebView(frame: .zero, configuration: config)
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
               let url = navigationAction.request.url
            {
                NSWorkspace.shared.open(url)
                decisionHandler(.cancel)
            } else {
                decisionHandler(.allow)
            }
        }
    }
}
