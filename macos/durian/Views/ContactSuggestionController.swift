//
//  ContactSuggestionController.swift
//  Durian
//
//  Manages NSPanel popup for contact suggestions at cursor position
//  Design: Figma node 6:2
//

import AppKit
import SwiftUI

// MARK: - Contact Suggestion Controller

class ContactSuggestionController {
    static let shared = ContactSuggestionController()

    private var popupPanel: NSPanel?
    private var hostingView: NSHostingView<AnyView>?
    private var clickOutsideMonitor: Any?
    private var currentOnDismiss: (() -> Void)?
    private var currentOnSelect: ((Contact) -> Void)?

    private init() {}

    // MARK: - Public API

    /// Check if popup is currently visible
    var isVisible: Bool {
        popupPanel?.isVisible ?? false
    }

    /// Show the contact suggestion popup below the cursor in the given token field
    func show(
        for tokenField: NSTokenField,
        contacts: [Contact],
        selectedIndex: Int,
        onSelect: @escaping (Contact) -> Void,
        onDismiss: @escaping () -> Void
    ) {
        // Store callbacks
        currentOnDismiss = onDismiss
        currentOnSelect = onSelect

        // Create or update the popup
        if popupPanel == nil {
            createPopupPanel()
        }

        // Update content
        updateContent(contacts: contacts, selectedIndex: selectedIndex)

        // Position at cursor
        positionPopup(for: tokenField)

        // Show the panel
        popupPanel?.orderFront(nil)

        // Setup click-outside monitor
        setupClickOutsideMonitor()
    }

    /// Update popup content without recreating the window
    func update(contacts: [Contact], selectedIndex: Int) {
        updateContent(contacts: contacts, selectedIndex: selectedIndex)
    }

    /// Reposition the popup (call if cursor moves)
    func reposition(for tokenField: NSTokenField) {
        positionPopup(for: tokenField)
    }

    /// Hide and cleanup the popup
    func dismiss() {
        popupPanel?.orderOut(nil)
        removeClickOutsideMonitor()
        currentOnDismiss = nil
        currentOnSelect = nil
    }

    // MARK: - Private: Panel Creation

    private func createPopupPanel() {
        // Create borderless panel
        let panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: 200, height: 100),
            styleMask: [.borderless, .nonactivatingPanel],
            backing: .buffered,
            defer: true
        )

        // Configure panel behavior
        panel.level = .popUpMenu
        panel.isFloatingPanel = true
        panel.becomesKeyOnlyIfNeeded = true
        panel.hidesOnDeactivate = false
        panel.backgroundColor = .clear
        panel.isOpaque = false
        panel.hasShadow = false  // SwiftUI view provides shadow

        popupPanel = panel
    }

    private func updateContent(contacts: [Contact], selectedIndex: Int) {
        guard let panel = popupPanel else { return }

        // Create SwiftUI view
        let popupView = ContactSuggestionPopup(
            contacts: contacts,
            selectedIndex: selectedIndex,
            onSelect: { [weak self] contact in
                self?.currentOnSelect?(contact)
                self?.dismiss()
            },
            onDismiss: { [weak self] in
                self?.currentOnDismiss?()
                self?.dismiss()
            }
        )

        // Wrap in hosting view
        let hostingView = NSHostingView(rootView: AnyView(popupView))
        hostingView.frame = panel.contentView?.bounds ?? .zero

        // Calculate intrinsic size
        let fittingSize = hostingView.fittingSize

        // Update panel size
        var frame = panel.frame
        frame.size = fittingSize
        panel.setFrame(frame, display: true)

        // Set content
        panel.contentView = hostingView
        self.hostingView = hostingView
    }

    // MARK: - Private: Positioning

    private func positionPopup(for tokenField: NSTokenField) {
        guard let panel = popupPanel,
              let window = tokenField.window else { return }

        // Get cursor position or fall back to field position
        let cursorRect = cursorScreenRect(in: tokenField) ?? fieldScreenRect(for: tokenField)

        // Position popup below cursor with small gap
        let gap: CGFloat = 4
        let popupSize = panel.frame.size

        // Calculate position: align left edge with cursor, below the line
        var popupOrigin = NSPoint(
            x: cursorRect.minX,
            y: cursorRect.minY - popupSize.height - gap
        )

        // Ensure popup stays on screen
        if let screen = window.screen ?? NSScreen.main {
            let screenFrame = screen.visibleFrame

            // Don't go off right edge
            if popupOrigin.x + popupSize.width > screenFrame.maxX {
                popupOrigin.x = screenFrame.maxX - popupSize.width
            }

            // Don't go off left edge
            if popupOrigin.x < screenFrame.minX {
                popupOrigin.x = screenFrame.minX
            }

            // If would go off bottom, show above cursor instead
            if popupOrigin.y < screenFrame.minY {
                popupOrigin.y = cursorRect.maxY + gap
            }
        }

        panel.setFrameOrigin(popupOrigin)
    }

    /// Get screen rect for the text cursor in the token field
    private func cursorScreenRect(in tokenField: NSTokenField) -> NSRect? {
        guard let editor = tokenField.currentEditor() as? NSTextView,
              let layoutManager = editor.layoutManager,
              let textContainer = editor.textContainer else
        {
            return nil
        }

        // Get insertion point location
        let selectedRange = editor.selectedRange()
        let insertionPoint = selectedRange.location

        // Handle empty text case
        guard insertionPoint > 0 || editor.string.isEmpty == false else {
            return nil
        }

        // Get glyph index (use max to handle end of text)
        let glyphIndex = min(
            layoutManager.glyphIndexForCharacter(at: max(0, insertionPoint - 1)),
            layoutManager.numberOfGlyphs > 0 ? layoutManager.numberOfGlyphs - 1 : 0
        )

        // Get bounding rect for the glyph
        var lineRect = layoutManager.boundingRect(
            forGlyphRange: NSRange(location: glyphIndex, length: 1),
            in: textContainer
        )

        // Adjust to cursor position (right edge of character if at end)
        if insertionPoint > 0 {
            lineRect.origin.x = lineRect.maxX
        }
        lineRect.size.width = 1  // Cursor width

        // Add text container origin offset
        lineRect.origin.x += editor.textContainerOrigin.x
        lineRect.origin.y += editor.textContainerOrigin.y

        // Convert to window coordinates
        let windowRect = editor.convert(lineRect, to: nil)

        // Convert to screen coordinates
        guard let window = tokenField.window else { return nil }
        let screenRect = window.convertToScreen(windowRect)

        return screenRect
    }

    /// Fallback: get screen rect for the entire token field
    private func fieldScreenRect(for tokenField: NSTokenField) -> NSRect {
        guard let window = tokenField.window else {
            return NSRect(x: 100, y: 100, width: 200, height: 24)
        }

        // Get field bounds in window coordinates
        let fieldBounds = tokenField.bounds
        let windowRect = tokenField.convert(fieldBounds, to: nil)

        // Convert to screen coordinates
        return window.convertToScreen(windowRect)
    }

    // MARK: - Private: Click Outside Monitor

    private func setupClickOutsideMonitor() {
        removeClickOutsideMonitor()

        clickOutsideMonitor = NSEvent.addLocalMonitorForEvents(
            matching: [.leftMouseDown, .rightMouseDown]
        ) { [weak self] event in
            guard let self = self,
                  let panel = popupPanel,
                  panel.isVisible else
            {
                return event
            }

            // Check if click is outside the popup
            let clickLocation = NSEvent.mouseLocation
            if !NSPointInRect(clickLocation, panel.frame) {
                currentOnDismiss?()
                dismiss()
            }

            return event
        }
    }

    private func removeClickOutsideMonitor() {
        if let monitor = clickOutsideMonitor {
            NSEvent.removeMonitor(monitor)
            clickOutsideMonitor = nil
        }
    }

    deinit {
        removeClickOutsideMonitor()
    }
}
