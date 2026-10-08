import AppKit
import ApplicationServices
import Foundation
import SwiftUI
import WebKit

private struct MessageList: Decodable {
    let messages: [ThreadMessage]
}

// Registered before singleton/session creation; catches every HTTP request,
// including side effects using URLSession.shared. No sockets or CLI are used.
private final class MockHTTP: URLProtocol {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var payloads: [[String: Any]] = []
    nonisolated(unsafe) static var contentFixtureEnabled = false
    nonisolated(unsafe) private static var bodyIds: [String] = []
    static var sends: [[String: Any]] { lock.withLock { payloads } }
    static var originalBodyIds: [String] { lock.withLock { bodyIds } }
    static let aliceMessages = #"[{"id":"bob","message_id":"<bob@example.com>","subject":"FW: newest Bob","from":"Bob <bob@example.com>","to":"me@example.com","date":"today","timestamp":1704153601,"body":"Newest Bob body"},{"id":"alice","message_id":"<alice@example.com>","references":"<root@example.com>","subject":"Older Alice subject","from":"Alice <alice@example.com>","to":"me@example.com, Carol <carol@example.com>","cc":"Dave <dave@example.com>","date":"yesterday","timestamp":1704153600,"body":"STRIPPED Alice body"}]"#
    static let decoyMessages = #"[{"id":"decoy","message_id":"<decoy@example.com>","subject":"Marked decoy subject","from":"Decoy <decoy@example.com>","to":"me@example.com","date":"today","timestamp":1704153700,"body":"Wrong marked thread body"}]"#
    static let originalText = "FULL Alice original body\n> ANCESTOR QUOTE retained from prior message"
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let path = request.url!.path
        print("MOCK_HTTP: \(request.httpMethod ?? "GET") \(path)")
        if path == "/api/v1/outbox/send" {
            var data = request.httpBody ?? Data()
            if let stream = request.httpBodyStream {
                stream.open()
                defer { stream.close() }
                var buffer = [UInt8](repeating: 0, count: 4096)
                while stream.hasBytesAvailable {
                    let count = stream.read(&buffer, maxLength: buffer.count)
                    if count <= 0 { break }
                    data.append(contentsOf: buffer.prefix(count))
                }
            }
            if let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
                Self.lock.withLock { Self.payloads.append(object) }
            }
        }
        var json = path == "/api/v1/outbox/send"
            ? "{\"ok\":true,\"id\":\(Self.sends.count),\"send_after\":4102444800}"
            : "{\"ok\":true,\"items\":[],\"results\":[],\"contacts\":[],\"count\":0}"
        if Self.contentFixtureEnabled {
            if path == "/api/v1/search" {
                json = #"{"ok":true,"results":[{"thread_id":"marked-thread","subject":"Marked decoy subject","from":"Decoy <decoy@example.com>","date":"today","timestamp":1704153700,"tags":"inbox"},{"thread_id":"cursor-thread","subject":"FW: newest Bob","from":"Bob <bob@example.com>","date":"today","timestamp":1704153601,"tags":"inbox"}]}"#
            } else if path.hasPrefix("/api/v1/threads/") && request.httpMethod == "GET" {
                let id = path.components(separatedBy: "/").last!
                let messages = id == "cursor-thread" ? Self.aliceMessages : Self.decoyMessages
                json = "{\"ok\":true,\"thread\":{\"thread_id\":\"\(id)\",\"subject\":\"\(id == "cursor-thread" ? "FW: newest Bob" : "Marked decoy subject")\",\"messages\":\(messages)}}"
            } else if path == "/api/v1/message/body" {
                let id = URLComponents(url: request.url!, resolvingAgainstBaseURL: false)?.queryItems?.first(where: { $0.name == "id" })?.value ?? ""
                Self.lock.withLock { Self.bodyIds.append(id) }
                json = String(data: try! JSONSerialization.data(withJSONObject: ["ok": true, "message_body": ["body": Self.originalText]]), encoding: .utf8)!
            }
        }
        let response = HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil,
                                       headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(json.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

private struct AXNode {
    let source: Any
    var identifier: String? = nil
    let role: NSAccessibility.Role?
    let label: String?
    let help: String?
    let value: String?

    var searchableText: String {
        [label, help, value].compactMap { $0 }.joined(separator: " ")
    }
}

private struct RenderedView {
    let window: NSWindow
    let hostingView: NSView
    let accessibilityNodes: [AXNode]
    let scrollDocumentHeight: CGFloat
}

@MainActor
private final class FocusFixture: ObservableObject {
    @Published var index = 0
}

private struct LongDetailFixture: View {
    let message: MailMessage
    @ObservedObject var focus: FocusFixture
    var body: some View {
        EmailDetailView(email: message, onReply: { _ in }, onReplyAll: { _ in },
                        onForward: { _ in }, onLoadBody: {}, focusedMessageIndex: $focus.index,
                        isThreadFocused: true)
    }
}

@MainActor
private final class NativeUITestRunner {
    private var failures: [String] = []

    func configure() {
        URLProtocol.registerClass(MockHTTP.self)
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockHTTP.self]
        let profile = Profile(name: "QA", accounts: ["qa"], isDefault: true, color: nil, folders: [])
        let backend = EmailBackend(session: URLSession(configuration: config), serverConnector: { "mock-token" },
                                   profileManager: ProfileManager(profiles: [profile], currentProfile: profile))
        AccountManager.shared.emailBackend = backend
        ConfigManager.shared.applyEvaluatedConfig(AppConfig(accounts: [MailAccount(name: "qa", email: "me@example.com")]))
        var sync = SyncSettings()
        sync.guiAutoSync = false
        SettingsManager.shared.applyEvaluatedConfig(AppConfig(accounts: [MailAccount(name: "qa", email: "me@example.com")], sync: sync))
        Task { await backend.connect() }
    }

    func run() throws {
        let sceneMode = ProcessInfo.processInfo.environment["NATIVE_UI_SCENE"] == "1"
        if !sceneMode { configure() }
        pumpRunLoop()
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        if !sceneMode { app.finishLaunching() }
        let startupDark = ProcessInfo.processInfo.environment["NATIVE_UI_APPEARANCE"] == "dark"
        app.appearance = NSAppearance(named: startupDark ? .darkAqua : .aqua)

        if sceneMode {
            try testRealContentViewCallbacks()
        } else {
            let loaded = try testLoadedStateAndReplyAction()
            try capture(loaded.hostingView)
            loaded.window.close()

            let loading = testLoadingState()
            loading.window.close()

            let empty = testEmptyState()
            empty.window.close()

            let long = try testLongThreadState(comparedWith: loaded.scrollDocumentHeight)
            long.window.close()

            let compose = testComposeState()
            compose.window.close()

            try testRealContentViewCallbacks()
        }

        if failures.isEmpty {
            print("SUMMARY: PASS mode=\(sceneMode ? "windowgroup" : "hosted") failures=0")
            return
        }

        for failure in failures {
            print("FAIL: \(failure)")
        }
        print("SUMMARY: FAIL failures=\(failures.count)")
        throw UITestFailure.assertionsFailed
    }

    private func testLoadedStateAndReplyAction() throws -> RenderedView {
        var message = try makeThread(count: 2, subject: "FW: newest Bob", bodyPrefix: "Newest Bob body")
        let fixture = """
        {"messages":[{"id":"bob","message_id":"<bob@example.com>","subject":"FW: newest Bob","from":"Bob <bob@example.com>","to":"me@example.com","date":"today","timestamp":1704153601,"body":"Newest Bob body"},{"id":"alice","message_id":"<alice@example.com>","references":"<root@example.com>","subject":"Older Alice subject","from":"Alice <alice@example.com>","to":"me@example.com, Carol <carol@example.com>","cc":"Dave <dave@example.com>","date":"yesterday","timestamp":1704153600,"body":"Older Alice quoted body"}]}
        """
        message.threadMessages = try JSONDecoder().decode(MessageList.self, from: Data(fixture.utf8)).messages
        var replyCount = 0
        var selected: ThreadMessage?
        var action = "reply"
        let rendered = render(EmailDetailView(
            email: message,
            onReply: { selected = $0; replyCount += 1 },
            onReplyAll: { selected = $0; replyCount += 1; action = "reply-all" },
            onForward: { selected = $0; replyCount += 1; action = "forward" },
            onLoadBody: {},
            focusedMessageIndex: .constant(0)
        ))

        assert(rendered.contains("FW: newest Bob"), "loaded state exposes newest subject")
        NotificationCenter.default.post(name: .threadScrollToBottom, object: nil)
        pumpRunLoop()
        try capture(rendered.hostingView)
        for kind in ["reply", "reply-all", "forward"] {
            let nodes = accessibilityTree(from: rendered.hostingView) + accessibilityTreeFromApplication()
            guard let button = nodes.first(where: { $0.identifier == "\(kind)-alice" }) else {
                failures.append("missing exact AX identifier \(kind)-alice")
                continue
            }
            let before = replyCount
            assert(performPress(button.source), "AX \(kind)-alice press succeeds")
            pumpRunLoop()
            assert(replyCount == before + 1, "\(kind) callback exactly once")
            assert(selected?.id == "alice" && selected?.subject == "Older Alice subject", "\(kind) selects older Alice not newest Bob")
            guard let selected else { continue }
            let source = message.selectingMessage(selected)
            var draft: EmailDraft
            switch kind {
            case "reply-all": draft = EmailDraft.createReplyAll(from: source, fromAccount: "me@example.com")
            case "forward":
                draft = EmailDraft.createForward(from: source, fromAccount: "me@example.com")
                draft.to = ["forward@example.com"]
            default: draft = EmailDraft.createReply(from: source, fromAccount: "me@example.com")
            }
            assert(action == kind, "exact selector distinguishes Reply from Reply All")
            let id = DraftService.shared.createDraft(with: draft)
            let compose = render(ComposeWindow(draftId: id))
            try capture(compose.hostingView, suffix: "-compose-\(kind)")
            let sendNodes = accessibilityTree(from: compose.hostingView) + accessibilityTreeFromApplication()
            if let send = sendNodes.first(where: {
                $0.role == .button && ($0.searchableText == "Send" || $0.searchableText.contains("Send (⌘Return)"))
            }) {
                let count = MockHTTP.sends.count
                assert(performPress(send.source), "actual ComposeWindow AX Send")
                for _ in 0..<10 where MockHTTP.sends.count == count { pumpRunLoop() }
                assert(MockHTTP.sends.count == count + 1, "one HTTP outbox send for \(kind)")
                if let payload = MockHTTP.sends.last, MockHTTP.sends.count == count + 1 {
                    assert(payload["subject"] as? String == (kind == "forward" ? "Fwd: Older Alice subject" : "Re: Older Alice subject"), "HTTP selected subject")
                    assert(payload["to"] as? [String] == (kind == "forward" ? ["forward@example.com"] : ["Alice <alice@example.com>"]), "HTTP recipient")
                    assert(payload["cc"] as? [String] == (kind == "reply-all" ? ["Carol <carol@example.com>", "Dave <dave@example.com>"] : []), "HTTP CC")
                    assert(payload["in_reply_to"] as? String == (kind == "forward" ? nil : "<alice@example.com>"), "HTTP in_reply_to")
                    assert(payload["references"] as? String == (kind == "forward" ? nil : "<root@example.com> <alice@example.com>"), "HTTP references")
                    let body = payload["body"] as? String ?? ""
                    assert(body.contains("Older Alice quoted body") && !body.contains("Newest Bob body"), "HTTP quotes older Alice only")
                }
            } else {
                failures.append("ComposeWindow toolbar Send is not AX reachable in standalone NSHostingView; no private-send shortcut used")
                print("COMPOSE_AX: \(sendNodes.filter { $0.role == .button }.map(\.searchableText))")
            }
            compose.window.orderOut(nil)
            DraftService.shared.discard(id: id)
        }
        return rendered
    }

    private func testRealContentViewCallbacks() throws {
        MockHTTP.contentFixtureEnabled = true
        defer { MockHTTP.contentFixtureEnabled = false }
        let profiles = try JSONDecoder().decode(ProfilesConfig.self, from: Data(#"{"profiles":[{"name":"QA","accounts":["qa"],"default":true,"folders":[{"name":"Inbox","icon":"tray","query":"tag:inbox"}]}]}"#.utf8))
        ProfileManager.shared.applyEvaluatedProfiles(profiles)
        let backend = AccountManager.shared.emailBackend!
        let alice = try JSONDecoder().decode([ThreadMessage].self, from: Data(MockHTTP.aliceMessages.utf8))
        let decoy = try JSONDecoder().decode([ThreadMessage].self, from: Data(MockHTTP.decoyMessages.utf8))
        backend.threadCache["cursor-thread"] = EmailBackend.CachedThread(messages: alice, timestamp: Date())
        backend.threadCache["marked-thread"] = EmailBackend.CachedThread(messages: decoy, timestamp: Date())
        let sceneMode = ProcessInfo.processInfo.environment["NATIVE_UI_SCENE"] == "1"
        let content: RenderedView
        if sceneMode {
            guard let window = NSApp.windows.first(where: { $0.isVisible && $0.contentView != nil }), let view = window.contentView else {
                throw UITestFailure.snapshotFailed
            }
            content = RenderedView(window: window, hostingView: view,
                                   accessibilityNodes: accessibilityTree(from: view), scrollDocumentHeight: 0)
            print("WINDOWGROUP_MAIN: window=\(window.windowNumber)")
        } else {
            content = render(ContentView())
        }
        defer { content.window.orderOut(nil) }
        for _ in 0..<10 { pumpRunLoop() }
        assert(AccountManager.shared.mailMessages.count == 2, "real startup search preserves both fixture threads")
        AccountManager.shared.selectEmail(threadId: "marked-thread")
        pumpRunLoop()
        let engine = KeymapHandler.shared.engine
        engine.setContext(.list)
        func key(_ text: String, code: UInt16, modifiers: NSEvent.ModifierFlags = []) {
            let event = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: modifiers,
                                        timestamp: 0, windowNumber: content.window.windowNumber, context: nil,
                                        characters: text, charactersIgnoringModifiers: text, isARepeat: false, keyCode: code)!
            assert(engine.handleKeyEvent(event), "real keymap consumes \(text)")
            pumpRunLoop()
        }
        key("V", code: 9, modifiers: .shift)
        assert(engine.visualModeType == .toggle, "toggle mode enters with decoy marked")
        key("j", code: 38)
        for _ in 0..<5 { pumpRunLoop() }
        let current = accessibilityTree(from: content.hostingView) + accessibilityTreeFromApplication()
        assert(current.contains { $0.identifier == "reply-alice" }, "cursor detail shows Alice while marked batch remains decoy")
        try capture(content.hostingView, suffix: "-real-content-cursor-marked")
        for kind in ["reply", "reply-all", "forward"] {
            let before = Set(DraftService.shared.activeDrafts.keys)
            let priorWindows = Set(NSApp.windows.map(\.windowNumber))
            let bodyCount = MockHTTP.originalBodyIds.count
            let nodes = accessibilityTree(from: content.hostingView) + accessibilityTreeFromApplication()
            guard let button = nodes.first(where: { $0.identifier == "\(kind)-alice" }) else {
                failures.append("real ContentView missing \(kind)-alice")
                continue
            }
            assert(performPress(button.source), "real ContentView older \(kind) AX callback")
            for _ in 0..<25 where Set(DraftService.shared.activeDrafts.keys).subtracting(before).isEmpty { pumpRunLoop() }
            let added = Set(DraftService.shared.activeDrafts.keys).subtracting(before)
            assert(added.count == 1, "real callback creates one observable draft")
            assert(Array(MockHTTP.originalBodyIds.dropFirst(bodyCount)) == ["alice"], "real callback fetches original id=alice exactly once")
            guard let id = added.first, var draft = DraftService.shared.getDraft(id: id) else { continue }
            assert(draft.replyThreadId == "cursor-thread", "compose parent is cursor thread, never marked-thread")
            assert(draft.subject == (kind == "forward" ? "Fwd: Older Alice subject" : "Re: Older Alice subject"), "real callback selected Alice subject")
            assert(draft.quotedContent?.contains("ANCESTOR QUOTE") == true && draft.quotedContent?.contains("STRIPPED Alice body") == false, "real callback quotes fetched full ancestor, not stripped body")
            if kind == "forward" {
                draft.to = ["forward@example.com"]
                DraftService.shared.updateDraft(id: id, draft: draft)
            }
            let compose: RenderedView
            if sceneMode {
                for _ in 0..<20 where !NSApp.windows.contains(where: { !priorWindows.contains($0.windowNumber) && $0.isVisible }) { pumpRunLoop() }
                let addedWindows = NSApp.windows.filter { !priorWindows.contains($0.windowNumber) && $0.isVisible }
                assert(addedWindows.count == 1, "production WindowGroup opens exactly one new compose window")
                guard let window = addedWindows.first, let view = window.contentView else { continue }
                compose = RenderedView(window: window, hostingView: view, accessibilityNodes: accessibilityTree(from: view), scrollDocumentHeight: 0)
                print("WINDOWGROUP_COMPOSE: kind=\(kind) window=\(window.windowNumber) title=\(window.title) draft=\(id)")
            } else {
                print("SCENE_GAP: hosting actual draft \(id) after production openWindow request")
                compose = render(ComposeWindow(draftId: id))
            }
            assert(waitForQuoteDOM(compose.hostingView, expected: "ANCESTOR QUOTE"), "\(kind) startup WK complete DOM contains full ancestor quote")
            try auditQuoteContrast(compose.hostingView, label: "\(kind)-startup")
            if kind == "reply" {
                let startupDark = NSApp.effectiveAppearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                for appearance in startupDark ? ["light", "dark"] : ["dark", "light"] {
                    NSApp.appearance = NSAppearance(named: appearance == "light" ? .aqua : .darkAqua)
                    compose.hostingView.layoutSubtreeIfNeeded()
                    for _ in 0..<5 { pumpRunLoop() }
                    assert(waitForQuoteDOM(compose.hostingView, expected: "ANCESTOR QUOTE"), "\(kind) \(appearance) WK complete DOM contains full ancestor quote")
                    try auditQuoteContrast(compose.hostingView, label: "\(kind)-transition-\(appearance)")
                }
            }
            let sendNodes = accessibilityTree(from: compose.hostingView) + accessibilityTreeFromApplication()
            let count = MockHTTP.sends.count
            if let send = sendNodes.first(where: { $0.role == .button && ($0.searchableText == "Send" || $0.searchableText.contains("Send (⌘Return)")) }) {
                assert(performPress(send.source), "real callback draft actual ComposeWindow Send")
                for _ in 0..<20 where MockHTTP.sends.count == count { pumpRunLoop() }
                assert(MockHTTP.sends.count == count + 1, "real \(kind) exactly one outbox HTTP send")
                if let payload = MockHTTP.sends.last, MockHTTP.sends.count == count + 1 {
                    let body = payload["body"] as? String ?? ""
                    assert(body.contains("FULL Alice original body") && body.contains("ANCESTOR QUOTE") && !body.contains("STRIPPED Alice body") && !body.contains("Wrong marked thread body"), "HTTP includes fetched full ancestor quote, no wrong thread")
                    assert(payload["subject"] as? String == draft.subject, "HTTP retains real selected subject")
                    assert(payload["to"] as? [String] == (kind == "forward" ? ["forward@example.com"] : ["Alice <alice@example.com>"]), "real HTTP recipient")
                    assert(payload["cc"] as? [String] == (kind == "reply-all" ? ["Carol <carol@example.com>", "Dave <dave@example.com>"] : []), "real HTTP CC")
                    assert(payload["in_reply_to"] as? String == (kind == "forward" ? nil : "<alice@example.com>"), "real HTTP in_reply_to")
                    assert(payload["references"] as? String == (kind == "forward" ? nil : "<root@example.com> <alice@example.com>"), "real HTTP references")
                }
            } else { failures.append("real callback compose Send missing") }
            compose.window.orderOut(nil)
            DraftService.shared.discard(id: id)
            content.window.makeKeyAndOrderFront(nil)
            pumpRunLoop()
        }
        engine.exitVisualMode()
    }

    private func auditQuoteContrast(_ view: NSView, label: String) throws {
        var found = false
        for web in allSubviews(of: view).compactMap({ $0 as? WKWebView }) {
            var result: [String: Any]?
            var finished = false
            web.evaluateJavaScript("""
                (() => {
                    const el = Array.from(document.querySelectorAll('div')).find(e => e.textContent.includes('ANCESTOR QUOTE'));
                    if (!el) return null;
                    const style = getComputedStyle(el);
                    return { color: style.color, rgb: style.color.match(/[\\d.]+/g).slice(0,3).map(Number),
                        background: style.backgroundColor, bodyBackground: getComputedStyle(document.body).backgroundColor,
                        important: el.style.getPropertyPriority('color'), darkMedia: matchMedia('(prefers-color-scheme: dark)').matches,
                        state: document.readyState, text: el.textContent };
                })()
                """) { value, error in
                result = value as? [String: Any]
                finished = true
                if let error { print("CONTRAST_DOM_ERROR: \(error)") }
            }
            let deadline = Date().addingTimeInterval(2)
            while !finished && Date() < deadline { pumpRunLoop() }
            guard let result, let rgb = result["rgb"] as? [Double], rgb.count == 3 else { continue }
            found = true
            var background: NSColor!
            let appearance = web.effectiveAppearance
            appearance.performAsCurrentDrawingAppearance {
                background = NSColor(Color.Detail.cardBackground).usingColorSpace(.deviceRGB)
            }
            func luminance(_ values: [Double]) -> Double {
                let linear = values.map { value -> Double in
                    let c = value / 255
                    return c <= 0.04045 ? c / 12.92 : pow((c + 0.055) / 1.055, 2.4)
                }
                return linear[0] * 0.2126 + linear[1] * 0.7152 + linear[2] * 0.0722
            }
            let bg = [background.redComponent, background.greenComponent, background.blueComponent].map { Double($0) * 255 }
            let fgLum = luminance(rgb), bgLum = luminance(bg)
            let ratio = (max(fgLum, bgLum) + 0.05) / (min(fgLum, bgLum) + 0.05)
            print("CONTRAST_DOM: label=\(label) app=\(NSApp.effectiveAppearance.name.rawValue) web=\(appearance.name.rawValue) foreground=\(rgb) resolved_card_background=\(bg) ratio=\(ratio) dom=\(result)")
            assert(result["background"] as? String == "rgba(0, 0, 0, 0)", "\(label) quote transparent over production card background")
            assert(ratio >= 4.5, "\(label) quote contrast >=4.5 (actual \(ratio))")
        }
        assert(found, "\(label) exact ancestor DOM is contrast-measurable")
        try capture(view, suffix: "-\(label)")
    }

    private func waitForQuoteDOM(_ view: NSView, expected: String) -> Bool {
        let deadline = Date().addingTimeInterval(5)
        while Date() < deadline {
            for web in allSubviews(of: view).compactMap({ $0 as? WKWebView }) where !web.isLoading {
                var result: [String: String]?
                var finished = false
                web.evaluateJavaScript("({state:document.readyState,text:document.body.innerText})") { value, error in
                    result = value as? [String: String]
                    finished = true
                    if let error { print("DOM_ERROR: \(error)") }
                }
                let evaluationDeadline = min(deadline, Date().addingTimeInterval(1))
                while !finished && Date() < evaluationDeadline { pumpRunLoop() }
                if result?["state"] == "complete", result?["text"]?.contains(expected) == true {
                    print("QUOTE_DOM: complete isLoading=\(web.isLoading) text=\(result!["text"]!)")
                    return true
                }
            }
            pumpRunLoop()
        }
        return false
    }

    private func testLoadingState() -> RenderedView {
        var message = makeBaseMessage(subject: "Loading state subject", from: "Loading Sender <loading@example.com>")
        message.bodyState = .loading
        let rendered = render(EmailDetailView(
            email: message,
            onReply: { _ in },
            onReplyAll: { _ in },
            onForward: { _ in },
            onLoadBody: {},
            focusedMessageIndex: .constant(0)
        ))
        assert(rendered.contains("Loading..."), "loading state exposes its progress label")
        return rendered
    }

    private func testEmptyState() -> RenderedView {
        var message = makeBaseMessage(subject: "Empty state subject", from: "Empty Sender <empty@example.com>")
        message.body = ""
        message.bodyState = .loaded(body: "", attributedBody: nil)
        let rendered = render(EmailDetailView(
            email: message,
            onReply: { _ in },
            onReplyAll: { _ in },
            onForward: { _ in },
            onLoadBody: {},
            focusedMessageIndex: .constant(0)
        ))
        assert(rendered.contains("Empty state subject"), "empty loaded state exposes its subject")
        assert(rendered.contains("Empty Sender"), "empty loaded state still exposes sender context")
        assert(!rendered.contains("Loaded state body"), "empty loaded state does not retain loaded fixture content")
        return rendered
    }

    private func testLongThreadState(comparedWith loadedHeight: CGFloat) throws -> RenderedView {
        let message = try makeThread(count: 500, subject: "Long thread subject", bodyPrefix: "Long thread body")
        let focus = FocusFixture()
        let start = ContinuousClock.now
        let rendered = render(LongDetailFixture(message: message, focus: focus))
        let duration = start.duration(to: .now)
        assert(rendered.contains("Long thread subject"), "long-thread state exposes its subject")
        assert(rendered.contains("Long thread body 0"), "long-thread state exposes the newest message")
        assert(rendered.scrollDocumentHeight > loadedHeight, "long-thread scroll document is taller than a single-message document")
        print("LONG_THREAD: messages=500 render=\(duration) document_height=\(rendered.scrollDocumentHeight)")
        focus.index = 499
        pumpRunLoop()
        pumpRunLoop()
        pumpRunLoop()
        pumpRunLoop()
        let focusedNodes = accessibilityTree(from: rendered.hostingView) + accessibilityTreeFromApplication()
        assert(focusedNodes.contains { $0.identifier == "reply-message-499" }, "500-message focus navigation reaches oldest card")
        try capture(rendered.hostingView, suffix: "-500-focused-oldest")
        NotificationCenter.default.post(name: .threadScrollToBottom, object: nil)
        pumpRunLoop()
        let nodes = accessibilityTree(from: rendered.hostingView) + accessibilityTreeFromApplication()
        assert(nodes.contains { $0.identifier == "reply-message-499" }, "500-message scroll reaches oldest card action")
        try capture(rendered.hostingView, suffix: "-500-oldest")
        return rendered
    }

    private func testComposeState() -> RenderedView {
        let draft = EmailDraft(
            from: "me@example.com",
            to: ["recipient@example.com"],
            subject: "Synthetic compose subject",
            body: "Synthetic compose body"
        )
        let rendered = render(ComposeForm(
            accounts: [MailAccount(name: "Synthetic", email: "me@example.com")],
            existingDraft: draft,
            triggerSend: .constant(false),
            showingFilePicker: .constant(false),
            currentDraft: .constant(nil),
            onDismiss: {}
        ))
        assert(rendered.contains("Synthetic compose subject"), "real ComposeForm exposes the synthetic subject")
        assert(rendered.contains("recipient@example.com"), "real ComposeForm exposes the synthetic recipient")
        return rendered
    }

    private func render<V: View>(_ view: V) -> RenderedView {
        let controller = NSHostingController(rootView: AnyView(view))
        let hostingView = controller.view as! NSHostingView<AnyView>
        let frame = NSRect(x: 0, y: 0, width: 1_200, height: 800)
        hostingView.frame = frame

        let window = NSWindow(
            contentRect: frame,
            styleMask: [.titled, .closable],
            backing: .buffered,
            defer: false
        )
        window.appearance = nil
        window.contentViewController = controller
        window.makeKeyAndOrderFront(nil)
        hostingView.layoutSubtreeIfNeeded()
        pumpRunLoop()
        hostingView.layoutSubtreeIfNeeded()

        let nodes = accessibilityTree(from: hostingView) + accessibilityTreeFromApplication()
        let documentHeight = allSubviews(of: hostingView)
            .compactMap { ($0 as? NSScrollView)?.documentView?.frame.height }
            .max() ?? hostingView.frame.height
        return RenderedView(
            window: window,
            hostingView: hostingView,
            accessibilityNodes: nodes,
            scrollDocumentHeight: documentHeight
        )
    }

    private func capture(_ view: NSView, suffix: String = "") throws {
        guard let path = ProcessInfo.processInfo.environment["NATIVE_UI_SCREENSHOT"], !path.isEmpty else {
            throw UITestFailure.missingScreenshotPath
        }
        guard let representation = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw UITestFailure.snapshotFailed
        }
        view.cacheDisplay(in: view.bounds, to: representation)
        var snapshots: [(NSImage, NSRect)] = []
        for web in allSubviews(of: view).compactMap({ $0 as? WKWebView }) {
            var image: NSImage?
            var complete = false
            web.takeSnapshot(with: nil) { result, error in
                image = result
                complete = true
                if let error { print("SNAPSHOT_ERROR: \(error)") }
            }
            let deadline = Date().addingTimeInterval(2)
            while !complete && Date() < deadline { pumpRunLoop() }
            if let image { snapshots.append((image, view.convert(web.bounds, from: web))) }
        }
        guard let bitmap = CGContext(data: nil, width: Int(view.bounds.width), height: Int(view.bounds.height),
                                     bitsPerComponent: 8, bytesPerRow: Int(view.bounds.width) * 4,
                                     space: CGColorSpaceCreateDeviceRGB(),
                                     bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { throw UITestFailure.snapshotFailed }
        let context = NSGraphicsContext(cgContext: bitmap, flipped: false)
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = context
        let appearance = view.window?.effectiveAppearance ?? view.effectiveAppearance
        appearance.performAsCurrentDrawingAppearance {
            let background = view.window?.backgroundColor ?? .windowBackgroundColor
            let resolvedBackground = background.usingColorSpace(.deviceRGB)!
            bitmap.setFillColor(resolvedBackground.cgColor)
            bitmap.fill(view.bounds)
            let native = NSImage(size: view.bounds.size)
            native.addRepresentation(representation)
            native.draw(in: view.bounds, from: .zero, operation: .sourceOver, fraction: 1)
            for (image, rectangle) in snapshots {
                let rect = NSRect(x: rectangle.minX, y: view.bounds.height - rectangle.maxY,
                                  width: rectangle.width, height: rectangle.height)
                image.draw(in: rect)
            }
            print("CAPTURE_APPEARANCE: \(appearance.name.rawValue) window_background=\(resolvedBackground) opaque=true wk_snapshots=\(snapshots.count)")
        }
        NSGraphicsContext.restoreGraphicsState()
        guard let image = bitmap.makeImage() else { throw UITestFailure.snapshotFailed }
        let opaque = NSBitmapImageRep(cgImage: image)
        guard let png = opaque.representation(using: .png, properties: [:]), png.count > 10_000 else {
            throw UITestFailure.snapshotFailed
        }
        let destination = path.replacingOccurrences(of: ".png", with: "\(suffix).png")
        try png.write(to: URL(fileURLWithPath: destination), options: .atomic)
        print("SCREENSHOT: \(destination) bytes=\(png.count)")
    }

    private func assert(_ condition: @autoclosure () -> Bool, _ message: String) {
        if condition() {
            print("PASS: \(message)")
        } else {
            failures.append(message)
        }
    }

    private func pumpRunLoop() {
        let deadline = Date().addingTimeInterval(0.2)
        while Date() < deadline {
            RunLoop.main.run(mode: .default, before: deadline)
        }
    }
}

private enum UITestFailure: Error {
    case assertionsFailed
    case missingScreenshotPath
    case snapshotFailed
}

private func makeBaseMessage(subject: String, from: String) -> MailMessage {
    MailMessage(
        threadId: "synthetic-\(UUID().uuidString)",
        subject: subject,
        from: from,
        to: "me@example.com",
        date: "Tue, 02 Jan 2024 00:00:00 +0000",
        timestamp: 1_704_153_600,
        tags: "inbox"
    )
}

private func makeThread(count: Int, subject: String, bodyPrefix: String) throws -> MailMessage {
    let messages = (0 ..< count).map { index in
        """
        {"id":"message-\(index)","from":"Sender \(index) <sender\(index)@example.com>","to":"me@example.com","date":"Tue, 02 Jan 2024 00:00:00 +0000","timestamp":\(1_704_153_600 - index),"body":"\(bodyPrefix) \(index)"}
        """
    }.joined(separator: ",")
    let decoded = try JSONDecoder().decode(MessageList.self, from: Data("{\"messages\":[\(messages)]}".utf8))
    var message = makeBaseMessage(subject: subject, from: decoded.messages[0].from)
    message.body = decoded.messages[0].body
    message.bodyState = .loaded(body: decoded.messages[0].body, attributedBody: nil)
    message.threadMessages = decoded.messages
    return message
}

private func accessibilityTree(from root: NSView) -> [AXNode] {
    var nodes: [AXNode] = []
    var visited = Set<ObjectIdentifier>()

    func visit(_ source: Any) {
        guard let object = source as? NSObject else { return }
        let identifier = ObjectIdentifier(object)
        guard visited.insert(identifier).inserted else { return }

        if let view = source as? NSView {
            nodes.append(AXNode(
                source: source,
                identifier: view.accessibilityIdentifier(),
                role: view.accessibilityRole(),
                label: view.accessibilityLabel(),
                help: view.accessibilityHelp(),
                value: String(describing: view.accessibilityValue() ?? "")
            ))
            for child in view.accessibilityChildren() ?? [] { visit(child) }
            for child in view.subviews { visit(child) }
        } else if let element = source as? NSAccessibilityElement {
            nodes.append(AXNode(
                source: source,
                identifier: element.accessibilityIdentifier(),
                role: element.accessibilityRole(),
                label: element.accessibilityLabel(),
                help: element.accessibilityHelp(),
                value: String(describing: element.accessibilityValue() ?? "")
            ))
            for child in element.accessibilityChildren() ?? [] { visit(child) }
        }
    }

    visit(root)
    return nodes
}

private func performPress(_ source: Any) -> Bool {
    if let view = source as? NSView,
       view.accessibilityActionNames().contains(.press)
    {
        return view.accessibilityPerformPress()
    }
    if let element = source as? NSAccessibilityElement,
       element.accessibilityActionNames().contains(.press)
    {
        return element.accessibilityPerformPress()
    }
    if CFGetTypeID(source as CFTypeRef) == AXUIElementGetTypeID() {
        let element = source as! AXUIElement
        var actions: CFArray?
        guard AXUIElementCopyActionNames(element, &actions) == .success,
              let names = actions as? [String], names.contains(kAXPressAction)
        else {
            return false
        }
        return AXUIElementPerformAction(element, kAXPressAction as CFString) == .success
    }
    return false
}

private func accessibilityTreeFromApplication() -> [AXNode] {
    let root = AXUIElementCreateApplication(ProcessInfo.processInfo.processIdentifier)
    AXUIElementSetMessagingTimeout(root, 0.05)
    var nodes: [AXNode] = []
    var visited = Set<CFHashCode>()
    func attribute(_ name: String, _ element: AXUIElement) -> CFTypeRef? {
        var value: CFTypeRef?
        guard AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success else { return nil }
        return value
    }
    func visit(_ element: AXUIElement) {
        guard nodes.count < 3000, visited.insert(CFHash(element)).inserted else { return }
        AXUIElementSetMessagingTimeout(element, 0.05)
        let role = attribute(kAXRoleAttribute, element) as? String
        // WebKit's remote accessibility objects can block synchronous
        // same-process traversal. The tested actions are native SwiftUI.
        guard role != "AXWebArea" else { return }
        nodes.append(AXNode(source: element,
                            identifier: attribute(kAXIdentifierAttribute, element) as? String,
                            role: role.map(NSAccessibility.Role.init(rawValue:)),
                            label: (attribute(kAXTitleAttribute, element) as? String)
                                ?? (attribute(kAXDescriptionAttribute, element) as? String),
                            help: attribute(kAXHelpAttribute, element) as? String,
                            value: role == "AXTextArea" ? nil : attribute(kAXValueAttribute, element) as? String))
        if let children = attribute(kAXChildrenAttribute, element) as? [AXUIElement] {
            children.forEach(visit)
        }
    }
    visit(root)
    return nodes
}

private func allSubviews(of view: NSView) -> [NSView] {
    view.subviews.flatMap { [$0] + allSubviews(of: $0) }
}

private extension RenderedView {
    func contains(_ text: String) -> Bool {
        accessibilityNodes.contains { $0.searchableText.localizedCaseInsensitiveContains(text) }
    }
}

#if !UI_WINDOW_SCENE
@main
enum UITestHarness {
    @MainActor
    static func main() {
        do {
            try NativeUITestRunner().run()
        } catch {
            fputs("UI HARNESS ERROR: \(error)\n", stderr)
            exit(1)
        }
    }
}
#endif
