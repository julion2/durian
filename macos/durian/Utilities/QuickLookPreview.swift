//
//  QuickLookPreview.swift
//  Durian
//
//  QuickLook preview support for email attachments
//

import AppKit
import Foundation
import Quartz

class QuickLookPreviewController: NSObject, QLPreviewPanelDataSource, QLPreviewPanelDelegate {
    var attachments: [EmailAttachment] = []
    var currentIndex: Int = 0
    private var tempURLs: [URL] = []

    override init() {
        super.init()
    }

    deinit {
        cleanupTempFiles()
    }

    func showPreview(for attachments: [EmailAttachment], startingAt index: Int) {
        Log.debug("QUICKLOOK", "showPreview called with \(attachments.count) attachments, index: \(index)")

        self.attachments = attachments
        currentIndex = index

        cleanupTempFiles()
        tempURLs = createTempFiles()

        Log.debug("QUICKLOOK", "Created \(tempURLs.count) temp files")

        guard let panel = QLPreviewPanel.shared() else {
            Log.error("QUICKLOOK", "Failed to get shared preview panel")
            return
        }

        Log.debug("QUICKLOOK", "Got preview panel, setting datasource and delegate")

        panel.dataSource = self
        panel.delegate = self
        panel.currentPreviewItemIndex = index

        Log.debug("QUICKLOOK", "Making panel key and ordering front")
        panel.makeKeyAndOrderFront(nil)

        Log.debug("QUICKLOOK", "Panel shown, isVisible: \(panel.isVisible)")
    }

    private func createTempFiles() -> [URL] {
        let tempDir = FileManager.default.temporaryDirectory
        var urls: [URL] = []

        for attachment in attachments {
            let tempURL = tempDir.appendingPathComponent(attachment.filename)

            do {
                try attachment.data.write(to: tempURL)
                urls.append(tempURL)
                Log.debug("QUICKLOOK", "Created temp file: \(tempURL.lastPathComponent)")
            } catch {
                Log.error("QUICKLOOK", "Failed to write temp file: \(error)")
            }
        }

        return urls
    }

    private func cleanupTempFiles() {
        for url in tempURLs {
            try? FileManager.default.removeItem(at: url)
        }
        tempURLs.removeAll()
    }

    func numberOfPreviewItems(in panel: QLPreviewPanel!) -> Int {
        attachments.count
    }

    func previewPanel(_ panel: QLPreviewPanel!, previewItemAt index: Int) -> QLPreviewItem! {
        guard index >= 0 && index < tempURLs.count else {
            return nil
        }
        return tempURLs[index] as QLPreviewItem
    }

    func previewPanel(_ panel: QLPreviewPanel!, handle event: NSEvent!) -> Bool {
        if event.type == .keyDown {
            if event.keyCode == 53 {
                panel.close()
                return true
            }
        }
        return false
    }
}

class QuickLookManager {
    static let shared = QuickLookManager()
    private var controller: QuickLookPreviewController?

    private init() {}

    func showPreview(for attachments: [EmailAttachment], startingAt index: Int) {
        controller = QuickLookPreviewController()
        controller?.showPreview(for: attachments, startingAt: index)
    }

    func closePreview() {
        QLPreviewPanel.shared()?.close()
        controller = nil
    }
}
