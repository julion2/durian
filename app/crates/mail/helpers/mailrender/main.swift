// mailrender – HTML mail rasterizer helper, spawned by the app (one process, a pool of WebViews).
//
// Protocol: newline-delimited JSON.
//   stdin  {"id":"…","html":"…","width":600,"out":"/path/to/snapshot.png"}
//   stdout {"id":"…","ok":true,"width":600,"height":1188,"clipped":false,
//           "strips":[{path,y,height}],"links":[{x,y,w,h,href}],"ms":41.2}
//
// Every WebView: non-persistent data store, content JavaScript disabled, a content rule list
// that blocks every network load, and a navigation policy that allows only the document we
// load – sanitized mail renders from the HTML string alone. Jobs time out; a hung or crashed
// WebContent process is replaced. Pages render in strips (GPU texture limits), cut at a
// maximum height.
// The app never blocks on us: it shows the text/plain part until a snapshot arrives.

import AppKit
import ImageIO
import UniformTypeIdentifiers
import WebKit

setvbuf(stdout, nil, _IOLBF, 0)

struct Job {
    let id: String
    let html: String
    let width: CGFloat
    let out: String
    let queuedAt: Date
}

func debug(_ message: String) {
    if ProcessInfo.processInfo.environment["MAILRENDER_DEBUG"] != nil {
        FileHandle.standardError.write("[mailrender] \(message)\n".data(using: .utf8)!)
    }
}

func reply(_ object: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: object),
          let line = String(data: data, encoding: .utf8) else { return }
    print(line)
}

/// Tallest strip: GPUI's GPU atlas holds images up to 16384 device px; strips of 4000 pt
/// (8000 px at 2x) stay well inside it and keep each decoded image ~40 MB.
let stripHeight: CGFloat = 4000
/// Longest mail rendered; the rest is cut (the text part has all of it).
let maxHeight: CGFloat = 16000
/// A mail that hasn't rendered by then never will: the worker is reset.
let jobTimeout: TimeInterval = 15

final class Worker: NSObject, WKNavigationDelegate {
    let config: WKWebViewConfiguration
    var web: WKWebView
    var job: Job?
    var startedAt = Date()
    var marks: [String: Double] = [:]
    var warming = false
    /// the one navigation we start ourselves (the HTML string); anything after it is refused
    var expectingLoad = false

    func mark(_ name: String) { marks[name] = Date().timeIntervalSince(startedAt) * 1000 }
    let onDone: (Worker) -> Void

    init(rules: WKContentRuleList?, onDone: @escaping (Worker) -> Void) {
        config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        config.defaultWebpagePreferences.allowsContentJavaScript = false
        config.suppressesIncrementalRendering = true
        if let rules { config.userContentController.add(rules) }
        web = Worker.makeWebView(config)
        self.onDone = onDone
        super.init()
        web.navigationDelegate = self
    }

    // 1pt tall so scrollHeight reports content height, not the viewport
    static func makeWebView(_ config: WKWebViewConfiguration) -> WKWebView {
        WKWebView(frame: NSRect(x: 0, y: 0, width: 600, height: 1), configuration: config)
    }

    /// A fresh WebView (and WebContent process) after a hang or crash.
    func reset() {
        web.navigationDelegate = nil
        web.stopLoading()
        web = Worker.makeWebView(config)
        web.navigationDelegate = self
    }

    /// Pays the WebContent process launch before the first real mail arrives.
    func warmUp() {
        warming = true
        expectingLoad = true
        web.loadHTMLString("<!doctype html><p>warm</p>", baseURL: nil)
    }

    func start(_ job: Job) {
        self.job = job
        startedAt = Date()
        marks = [:]
        web.frame = NSRect(x: 0, y: 0, width: job.width, height: 1)
        expectingLoad = true
        // never a file: base URL – it would let the mail read local files
        web.loadHTMLString(job.html, baseURL: nil)
        debug("start \(job.id)")
        DispatchQueue.main.asyncAfter(deadline: .now() + jobTimeout) { [weak self] in
            guard let self, self.job?.id == job.id else { return }
            self.reset()
            self.fail("timed out")
        }
    }

    func fail(_ message: String) {
        if let job { reply(["id": job.id, "ok": false, "error": message]) }
        finish()
    }

    func finish() {
        job = nil
        onDone(self)
    }

    /// Only the document we load: no redirects, meta refreshes, form posts, followed links,
    /// frames navigating.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        let ours = expectingLoad && action.targetFrame?.isMainFrame == true && action.navigationType == .other
        if ours { expectingLoad = false }
        decisionHandler(ours ? .allow : .cancel)
    }

    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        reset()
        if warming {
            warming = false
            return onDone(self)
        }
        fail("renderer crashed")
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        fail(error.localizedDescription)
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        fail(error.localizedDescription)
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        if warming {
            warming = false
            return onDone(self)
        }
        guard let job else { return }
        mark("load")
        debug("loaded \(job.id)")
        // our own measuring script is privileged; content scripts stay disabled
        let js = """
        JSON.stringify({
          height: Math.ceil(document.documentElement.scrollHeight),
          links: [...document.querySelectorAll('a[href]')].flatMap(a => [...a.getClientRects()].map(r => ({
            x: +r.left.toFixed(1), y: +(r.top + window.scrollY).toFixed(1), w: +r.width.toFixed(1), h: +r.height.toFixed(1), href: a.href
          })))
        })
        """
        webView.evaluateJavaScript(js) { [weak self] result, error in
            guard let self, self.job?.id == job.id else { return }
            guard let json = result as? String, let data = json.data(using: .utf8),
                  let info = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let measured = info["height"] as? Double else { return self.fail("measure: \(String(describing: error))") }
            self.mark("measure")
            let height = min(CGFloat(measured), maxHeight)
            let links = (info["links"] as? [[String: Any]] ?? []).filter { ($0["y"] as? Double ?? 0) < Double(height) }
            webView.frame.size.height = height
            // one runloop turn so WebKit lays out at the new size before we snapshot
            DispatchQueue.main.async {
                self.snapshot(job, height: height, clipped: CGFloat(measured) > maxHeight, links: links)
            }
        }
    }

    /// The page in strips, top to bottom; each is encoded off the main thread while the next
    /// is taken. The worker is free once the last strip is taken; the reply follows when every
    /// strip is on disk.
    func snapshot(_ job: Job, height: CGFloat, clipped: Bool, links: [[String: Any]]) {
        let count = max(1, Int((height / stripHeight).rounded(.up)))
        let encodes = DispatchGroup()
        var strips = [[String: Any]](repeating: [:], count: count)
        var failed = false
        let lock = NSLock()

        func take(_ n: Int) {
            guard self.job?.id == job.id else { return }
            if n == count {
                self.mark("snapshot")
                let (started, marks) = (self.startedAt, self.marks)
                self.finish()
                encodes.notify(queue: .main) {
                    guard !failed else { return reply(["id": job.id, "ok": false, "error": "write failed"]) }
                    var marks = marks
                    marks["encode"] = Date().timeIntervalSince(started) * 1000
                    reply([
                        "id": job.id, "ok": true, "width": Double(job.width), "height": Double(height), "clipped": clipped,
                        "strips": strips, "links": links,
                        "ms": marks["encode"] ?? 0,
                        "queued_ms": started.timeIntervalSince(job.queuedAt) * 1000,
                        "marks": marks,
                    ])
                }
                return
            }
            let top = CGFloat(n) * stripHeight
            let cfg = WKSnapshotConfiguration()
            cfg.rect = NSRect(x: 0, y: top, width: job.width, height: min(stripHeight, height - top))
            cfg.snapshotWidth = NSNumber(value: Double(job.width))  // points; WebKit applies the 2x backing scale
            self.web.takeSnapshot(with: cfg) { image, error in
                guard let image, let cg = image.cgImage(forProposedRect: nil, context: nil, hints: nil)
                else { return self.fail("snapshot: \(String(describing: error))") }
                let url = URL(fileURLWithPath: job.out).deletingPathExtension().appendingPathExtension("\(n).jpg")
                encodes.enter()
                DispatchQueue.global(qos: .userInitiated).async {
                    // JPEG encodes several times faster than PNG at 2x; ImageIO straight from the
                    // CGImage – much cheaper than a TIFF round-trip
                    var written = false
                    if let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.jpeg.identifier as CFString, 1, nil) {
                        CGImageDestinationAddImage(dest, cg, [kCGImageDestinationLossyCompressionQuality: 0.88] as CFDictionary)
                        written = CGImageDestinationFinalize(dest)
                    }
                    lock.lock()
                    strips[n] = ["path": url.path, "y": Double(top), "height": Double(cfg.rect.height)]
                    failed = failed || !written
                    lock.unlock()
                    encodes.leave()
                }
                take(n + 1)
            }
        }
        take(0)
    }
}

final class Pool {
    var workers: [Worker] = []  // owns the workers: WKWebView.navigationDelegate is weak
    var idle: [Worker] = []
    var queue: [Job] = []

    init(size: Int, rules: WKContentRuleList?) {
        for _ in 0..<size {
            workers.append(Worker(rules: rules) { [unowned self] worker in
                self.idle.append(worker)
                self.pump()
            })
        }
        // warm workers report idle via onDone once their blank document loaded
        workers.forEach { $0.warmUp() }
    }

    func submit(_ job: Job) {
        debug("submit \(job.id) idle=\(idle.count)")
        queue.append(job)
        pump()
    }

    func pump() {
        while !idle.isEmpty, !queue.isEmpty {
            idle.removeLast().start(queue.removeFirst())
        }
    }
}

let poolSize = Int(CommandLine.arguments.dropFirst().first ?? "3") ?? 3
var pool: Pool?

// block every network load: remote images/fonts/css never leave the machine
// (only network schemes – a ".*" filter would also block the about:blank document itself)
// (WebKit's rule regexes have no disjunctions, hence one rule per scheme)
let blockAll = "[" + ["https?", "wss?", "ftp"].map { #"{"trigger":{"url-filter":"^\#($0)://"},"action":{"type":"block"}}"# }.joined(separator: ",") + "]"
WKContentRuleListStore.default().compileContentRuleList(forIdentifier: "mailrender-block-all", encodedContentRuleList: blockAll) { rules, error in
    if let error { FileHandle.standardError.write("content rules: \(error)\n".data(using: .utf8)!) }
    // no rules, no rendering: without the network block a mail could load trackers
    guard let rules else {
        reply(["ready": false, "error": "content rules unavailable"])
        exit(1)
    }
    pool = Pool(size: poolSize, rules: rules)
    reply(["ready": true, "pool": poolSize])

    Thread.detachNewThread {
        debug("reading stdin")
        while let line = readLine() {
            debug("line \(line.prefix(40))")
            guard let data = line.data(using: .utf8),
                  let req = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let id = req["id"] as? String, let html = req["html"] as? String, let out = req["out"] as? String else { continue }
            let width = CGFloat(req["width"] as? Double ?? 600)
            let job = Job(id: id, html: html, width: width, out: out, queuedAt: Date())
            DispatchQueue.main.async { pool?.submit(job) }
        }
        exit(0)  // app closed our stdin
    }
}

// no NSApplication: a plain run loop keeps the helper out of the Dock and the app switcher
RunLoop.main.run()
