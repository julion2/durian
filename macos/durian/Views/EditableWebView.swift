//
//  EditableWebView.swift
//  Durian
//
//  A contentEditable WKWebView for composing emails with HTML support.
//  Used when HTML signatures or formatting are present.
//

import SwiftUI
import WebKit

struct EditableWebView: NSViewRepresentable {
    @Binding var plainText: String
    var htmlSignature: String?
    @Binding var contentHeight: CGFloat
    let font: NSFont
    let textColor: NSColor
    let backgroundColor: NSColor
    let placeholderText: String
    @Binding var formatCommand: String?
    @Binding var fontSizeCommand: Int?
    @Binding var fontFamilyCommand: String?
    @Binding var htmlBody: String
    var onFormatStateChange: ((_ bold: Bool, _ italic: Bool, _ underline: Bool, _ strikethrough: Bool, _ fontSize: Int, _ fontFamily: String, _ alignment: String) -> Void)?
    var onVimModeChange: ((_ mode: String) -> Void)?
    var onSearchRequest: (() -> Void)?
    var vimInsertExitKeys: [String] = []

    func makeNSView(context: Context) -> WKWebView {
        // Per-instance config (userContentController + message handlers must
        // belong to this single editor), but share the SharedWebKit.composePool
        // so successive open-and-close compose windows reuse one WebContent
        // process instead of spawning fresh. Distinct from the read-only pool
        // — editor message handlers shouldn't share a process with rendered
        // mail content.
        let config = WKWebViewConfiguration()
        config.processPool = SharedWebKit.composePool
        config.defaultWebpagePreferences.allowsContentJavaScript = true
        config.preferences.javaScriptCanOpenWindowsAutomatically = false

        // Message handler for content changes
        let handler = context.coordinator
        config.userContentController.add(handler, name: "textChanged")
        config.userContentController.add(handler, name: "htmlChanged")
        config.userContentController.add(handler, name: "heightChanged")
        config.userContentController.add(handler, name: "formatState")
        config.userContentController.add(handler, name: "vimModeChanged")
        config.userContentController.add(handler, name: "vimYank")
        config.userContentController.add(handler, name: "vimPaste")
        config.userContentController.add(handler, name: "vimSearch")

        let webView = ScrollPassthroughWebView(frame: .zero, configuration: config)
        SharedWebKit.applyEnergyDefaults(to: webView)
        #if DEBUG
        webView.isInspectable = true
        #endif
        webView.navigationDelegate = context.coordinator
        webView.wantsLayer = true
        webView.layer?.backgroundColor = NSColor.clear.cgColor

        // Allow editing
        webView.setValue(true, forKey: "drawsBackground")
        webView.setValue(false, forKey: "drawsBackground")

        context.coordinator.webView = webView
        context.coordinator.parent = self

        let html = EditableWebViewHTML.buildHTML(
            plainText: plainText,
            signature: htmlSignature,
            font: font,
            textColor: textColor,
            backgroundColor: backgroundColor,
            placeholder: placeholderText,
            vimInsertExitKeys: vimInsertExitKeys
        )
        context.coordinator.lastLoadedSignature = htmlSignature
        webView.loadHTMLString(html, baseURL: nil)

        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {
        context.coordinator.parent = self

        // Update signature via JS instead of reloading (preserves user formatting)
        if context.coordinator.lastLoadedSignature != htmlSignature {
            context.coordinator.lastLoadedSignature = htmlSignature
            if context.coordinator.initialLoadDone {
                let escaped = (htmlSignature ?? "")
                    .replacingOccurrences(of: "\\", with: "\\\\")
                    .replacingOccurrences(of: "'", with: "\\'")
                    .replacingOccurrences(of: "\n", with: "\\n")
                    .replacingOccurrences(of: "\r", with: "")
                webView.evaluateJavaScript("updateSignature('\(escaped)')")
            } else {
                let html = EditableWebViewHTML.buildHTML(
                    plainText: plainText,
                    signature: htmlSignature,
                    font: font,
                    textColor: textColor,
                    backgroundColor: backgroundColor,
                    placeholder: placeholderText,
                    vimInsertExitKeys: vimInsertExitKeys
                )
                webView.loadHTMLString(html, baseURL: nil)
            }
        }

        // Execute formatting command if requested
        if let cmd = formatCommand {
            DispatchQueue.main.async {
                self.formatCommand = nil
            }
            let js: String
            if cmd == "insertUnorderedList" || cmd == "insertOrderedList" {
                let tag = cmd == "insertUnorderedList" ? "ul" : "ol"
                js = """
                restoreSelection();
                toggleList('\(tag)');
                """
            } else if cmd == "removeFormat" {
                js = """
                (function() {
                    const editor = document.getElementById('editor');
                    editor.focus();
                    restoreSelection();
                    const sel = window.getSelection();
                    if (!sel.rangeCount) return;
                    const range = sel.getRangeAt(0);
                    if (range.collapsed) return;
                    const fragment = range.extractContents();
                    const tmp = document.createElement('div');
                    tmp.appendChild(fragment);
                    // Strip all attributes from every element
                    tmp.querySelectorAll('*').forEach(function(el) {
                        while (el.attributes.length > 0) {
                            el.removeAttribute(el.attributes[0].name);
                        }
                    });
                    // Unwrap inline/presentational elements, keep block structure (div, p, br, ul, ol, li)
                    const inline = ['SPAN','FONT','B','I','U','S','STRIKE','STRONG','EM','SUB','SUP','MARK','A','ABBR','CITE','CODE','SMALL','BIG','DEL','INS'];
                    tmp.querySelectorAll(inline.join(',')).forEach(function(el) {
                        while (el.firstChild) el.parentNode.insertBefore(el.firstChild, el);
                        el.parentNode.removeChild(el);
                    });
                    // Re-insert cleaned fragment
                    const cleaned = document.createDocumentFragment();
                    while (tmp.firstChild) cleaned.appendChild(tmp.firstChild);
                    range.insertNode(cleaned);
                    window.webkit.messageHandlers.htmlChanged.postMessage(getEditorHTML());
                    notifyFormatState();
                })();
                """
            } else {
                js = """
                document.getElementById('editor').focus();
                restoreSelection();
                document.execCommand('\(cmd)', false, null);
                window.webkit.messageHandlers.htmlChanged.postMessage(getEditorHTML());
                notifyFormatState();
                """
            }
            webView.evaluateJavaScript(js, completionHandler: nil)
        }

        // Execute font size command if requested
        if let size = fontSizeCommand {
            DispatchQueue.main.async {
                self.fontSizeCommand = nil
            }
            let js = """
            (function() {
                const editor = document.getElementById('editor');
                editor.focus();
                restoreSelection();
                const sel = window.getSelection();
                if (!sel.rangeCount || sel.isCollapsed) return;
                const range = sel.getRangeAt(0);
                const fragment = range.extractContents();
                const span = document.createElement('span');
                span.style.fontSize = '\(size)px';
                span.appendChild(fragment);
                // Strip inherited font-size from extracted ancestors so our size wins
                span.querySelectorAll('[style]').forEach(function(el) {
                    el.style.removeProperty('font-size');
                    if (!el.getAttribute('style') || !el.getAttribute('style').trim()) el.removeAttribute('style');
                });
                span.querySelectorAll('font[size]').forEach(function(f) { f.removeAttribute('size'); });
                range.insertNode(span);
                sel.removeAllRanges();
                const nr = document.createRange();
                nr.selectNodeContents(span);
                sel.addRange(nr);
                notifyFormatState();
            })();
            """
            webView.evaluateJavaScript(js, completionHandler: nil)
        }

        // Execute font family command if requested
        if let family = fontFamilyCommand {
            DispatchQueue.main.async {
                self.fontFamilyCommand = nil
            }
            let stacks: [String: String] = [
                "Helvetica": "'Helvetica Neue', Helvetica, Arial, sans-serif",
                "Arial": "Arial, Helvetica, sans-serif",
                "Times New Roman": "'Times New Roman', Times, serif",
                "Georgia": "Georgia, 'Times New Roman', serif",
                "Courier": "'Courier New', Courier, monospace",
            ]
            let stack = stacks[family] ?? "'\(family)', sans-serif"
            let js = """
            (function() {
                const editor = document.getElementById('editor');
                editor.focus();
                restoreSelection();
                const sel = window.getSelection();
                if (!sel.rangeCount || sel.isCollapsed) return;
                const range = sel.getRangeAt(0);
                const fragment = range.extractContents();
                const span = document.createElement('span');
                span.style.fontFamily = "\(stack)";
                span.appendChild(fragment);
                span.querySelectorAll('[style]').forEach(function(el) {
                    el.style.removeProperty('font-family');
                    if (!el.getAttribute('style') || !el.getAttribute('style').trim()) el.removeAttribute('style');
                });
                span.querySelectorAll('font[face]').forEach(function(f) { f.removeAttribute('face'); });
                range.insertNode(span);
                sel.removeAllRanges();
                const nr = document.createRange();
                nr.selectNodeContents(span);
                sel.addRange(nr);
                notifyFormatState();
            })();
            """
            webView.evaluateJavaScript(js, completionHandler: nil)
        }
    }


    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    static func dismantleNSView(_ nsView: WKWebView, coordinator: Coordinator) {
        // Stop in-flight loads on compose-window close so the editor's
        // WebContent process doesn't keep doing work after dismissal.
        nsView.stopLoading()
    }

    class Coordinator: NSObject, WKNavigationDelegate, WKScriptMessageHandler {
        weak var webView: WKWebView?
        var parent: EditableWebView?
        var lastLoadedSignature: String?
        var initialLoadDone = false
        private var isUpdating = false
        private var searchObserver: Any?

        override init() {
            super.init()
            searchObserver = NotificationCenter.default.addObserver(
                forName: .vimSearchSubmit, object: nil, queue: .main
            ) { [weak self] notification in
                guard let query = notification.object as? String else { return }
                self?.webView?.window?.makeFirstResponder(self?.webView)
                self?.webView?.evaluateJavaScript("vim.doSearch('\(query)')")
            }
        }

        deinit {
            if let observer = searchObserver {
                NotificationCenter.default.removeObserver(observer)
            }
        }

        func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
            switch message.name {
            case "textChanged":
                if let text = message.body as? String, !isUpdating {
                    isUpdating = true
                    DispatchQueue.main.async {
                        self.parent?.plainText = text
                        self.isUpdating = false
                    }
                }
            case "htmlChanged":
                if let html = message.body as? String, !isUpdating {
                    DispatchQueue.main.async {
                        self.parent?.htmlBody = html
                    }
                }
            case "heightChanged":
                if let height = message.body as? CGFloat, height > 0 {
                    DispatchQueue.main.async {
                        self.parent?.contentHeight = max(height, 100)
                    }
                }
            case "formatState":
                if let dict = message.body as? [String: Any] {
                    let bold = dict["bold"] as? Bool ?? false
                    let italic = dict["italic"] as? Bool ?? false
                    let underline = dict["underline"] as? Bool ?? false
                    let strikethrough = dict["strikethrough"] as? Bool ?? false
                    let fontSize = dict["fontSize"] as? Int ?? 13
                    let fontFamily = dict["fontFamily"] as? String ?? "Helvetica"
                    let alignment = dict["alignment"] as? String ?? "left"
                    DispatchQueue.main.async {
                        self.parent?.onFormatStateChange?(bold, italic, underline, strikethrough, fontSize, fontFamily, alignment)
                    }
                }
            case "vimModeChanged":
                if let mode = message.body as? String {
                    DispatchQueue.main.async {
                        self.parent?.onVimModeChange?(mode)
                    }
                }
            case "vimYank":
                if let text = message.body as? String {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(text, forType: .string)
                }
            case "vimPaste":
                let before = (message.body as? [String: Any])?["before"] as? Bool ?? false
                let text = NSPasteboard.general.string(forType: .string) ?? ""
                let escaped = text
                    .replacingOccurrences(of: "\\", with: "\\\\")
                    .replacingOccurrences(of: "'", with: "\\'")
                    .replacingOccurrences(of: "\n", with: "\\n")
                    .replacingOccurrences(of: "\r", with: "")
                webView?.evaluateJavaScript("vim.doPaste('\(escaped)', \(before))")
            case "vimSearch":
                DispatchQueue.main.async {
                    self.parent?.onSearchRequest?()
                }
            case "vimSearchFocus":
                DispatchQueue.main.async {
                    self.webView?.window?.makeFirstResponder(self.webView)
                }
            default:
                break
            }
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            initialLoadDone = true
            // Measure initial height
            webView.evaluateJavaScript("document.body.scrollHeight") { [weak self] result, _ in
                if let height = result as? CGFloat, height > 0 {
                    DispatchQueue.main.async {
                        self?.parent?.contentHeight = max(height, 100)
                    }
                }
            }
        }

        // Open links in browser
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
