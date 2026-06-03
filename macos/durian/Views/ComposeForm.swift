//
//  ComposeForm.swift
//  Durian
//
//  Email composition form
//

import AppKit
import Combine
import SwiftUI

extension Notification.Name {
    static let vimSearchSubmit = Notification.Name("vimSearchSubmit")
}
import UniformTypeIdentifiers

struct ComposeForm: View {
    @StateObject private var sendingManager = EmailSendingManager.shared

    let accounts: [MailAccount]
    let existingDraft: EmailDraft?
    @Binding var triggerSend: Bool
    @Binding var showingFilePicker: Bool
    @Binding var currentDraft: EmailDraft?
    let onDismiss: () -> Void

    @State var draft: EmailDraft
    @State private var showError = false
    @State private var errorMessage = ""
    @State var autoSaveCancellable: AnyCancellable?
    @State private var selectedSignature: String?
    @State private var selectedAttachmentIndex: Int? = nil
    @State private var keyMonitor: Any?
    @State private var showCcBcc: Bool = false
    @State private var quotedContentHeight: CGFloat = 100  // Dynamic height for WebView
    @State private var editorHeight: CGFloat = 100        // Dynamic height for EditableWebView
    @State private var formatCommand: String?             // Pending format command for EditableWebView
    @State private var fontSizeCommand: Int?              // Pending font size command for EditableWebView
    @State private var fontFamilyCommand: String?         // Pending font family command for EditableWebView
    @State private var isBold: Bool = false
    @State private var isItalic: Bool = false
    @State private var isUnderline: Bool = false
    @State private var isStrikethrough: Bool = false
    @State private var currentFontSize: Int = 13
    @State private var currentFontFamily: String = "Helvetica"
    @State private var currentAlignment: String = "left"
    @State private var vimMode: String = "insert"
    @State private var showVimSearch: Bool = false
    @State private var vimSearchText: String = ""
    @FocusState private var focusedField: ComposeField?  // Shared focus state

    // Contact suggestion popup state (internal so ComposeForm+Contacts extension can access)
    @State var contactSuggestions: [Contact] = []
    @State var selectedSuggestionIndex: Int = 0
    @State var activeTokenField: ComposeField? = nil
    @State var activeNSTokenField: NSTokenField? = nil  // Reference to actual NSTokenField
    @State var showingContactPopup = false
    @State var contactSearchTask: Task<Void, Never>?

    private let signatures: [String: String]
    private let maxAttachmentSize: Int64 = 25_000_000
    private let maxTotalSize: Int64 = 50_000_000
    private let maxAttachments: Int = 10

    // MARK: - Colors

    private let labelColor = Color.Detail.textSecondary
    private let placeholderColor = Color.Detail.textPlaceholder
    private let textColor = Color.Detail.textPrimary

    init(accounts: [MailAccount], existingDraft: EmailDraft? = nil, triggerSend: Binding<Bool>, showingFilePicker: Binding<Bool>, currentDraft: Binding<EmailDraft?>, onDismiss: @escaping () -> Void) {
        self.accounts = accounts
        self.existingDraft = existingDraft
        _triggerSend = triggerSend
        _showingFilePicker = showingFilePicker
        _currentDraft = currentDraft
        self.onDismiss = onDismiss
        signatures = ConfigManager.shared.getSignatures()

        let defaultAccount = accounts.first?.email ?? ""

        if let existing = existingDraft {
            _draft = State(initialValue: existing)
            _selectedSignature = State(initialValue: nil)
            // Auto-expand Cc/Bcc if draft has values
            _showCcBcc = State(initialValue: !existing.cc.isEmpty || !existing.bcc.isEmpty)
        } else {
            _draft = State(initialValue: EmailDraft(from: defaultAccount))

            let account = accounts.first { $0.email == defaultAccount }
            _selectedSignature = State(initialValue: account?.defaultSignature)
            _showCcBcc = State(initialValue: false)
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            // Form Area
            formSection

            // Formatting Toolbar
            ComposeToolbar(
                onFormat: { command in
                    formatCommand = command
                },
                onFontSize: { size in
                    fontSizeCommand = size
                },
                onFontFamily: { family in
                    fontFamilyCommand = family
                },
                boldActive: isBold,
                italicActive: isItalic,
                underlineActive: isUnderline,
                strikethroughActive: isStrikethrough,
                currentFontSize: currentFontSize,
                currentFontFamily: currentFontFamily,
                currentAlignment: currentAlignment,
                vimMode: vimMode
            )

            // Message Editor
            messageEditor

            // Attachments (if any)
            if !draft.attachments.isEmpty {
                attachmentsScrollView
            }

        }
        .navigationTitle("")
        .fileImporter(
            isPresented: $showingFilePicker,
            allowedContentTypes: [.data],
            allowsMultipleSelection: true
        ) { result in
            handleFileSelection(result)
        }
        .alert("Error", isPresented: $showError) {
            Button("OK") { showError = false }
        } message: {
            Text(errorMessage)
        }
        // Note: triggerSend is handled by ComposeWindow.handleSend()
        // Do not add onChange handler here to avoid double-sending
        .onChange(of: draft) { oldValue, newDraft in
            currentDraft = newDraft
        }
        .onChange(of: focusedField) { _, newField in
            // Dismiss contact suggestions when focus moves away from recipient fields
            if newField != .to && newField != .cc && newField != .bcc {
                dismissContactSuggestions()
            }
        }
        .onAppear {
            currentDraft = draft
            updateBodyWithSignature()
            setupKeyMonitor()
        }
        .onDisappear {
            autoSaveCancellable?.cancel()
            removeKeyMonitor()
            currentDraft = nil
        }
        .overlay(alignment: .bottomLeading) {
            if showVimSearch {
                VimSearchPill(text: $vimSearchText, onSubmit: {
                    let escaped = vimSearchText
                        .replacingOccurrences(of: "\\", with: "\\\\")
                        .replacingOccurrences(of: "'", with: "\\'")
                    NotificationCenter.default.post(name: .vimSearchSubmit, object: escaped)
                    showVimSearch = false
                }, onDismiss: {
                    showVimSearch = false
                    NotificationCenter.default.post(name: .vimSearchSubmit, object: "")
                })
                .padding(.leading, 20)
                .padding(.bottom, 48)
            }
        }
    }

    // MARK: - Form Section

    private var formSection: some View {
        VStack(spacing: 0) {
            // To Row
            toRow

            // Cc/Bcc Rows (expandable)
            if showCcBcc {
                ccRow
                bccRow
            }

            // From Row
            fromRow

            // Subject Row
            subjectRow

            Divider()
                .padding(.horizontal, 24)
        }
        .padding(.top, 16)
        .padding(.bottom, 8)
    }

    // MARK: - To Row

    private var toRow: some View {
        HStack(alignment: .center, spacing: 12) {
            Text("To:")
                .font(.system(size: 14, weight: .medium))
                .foregroundColor(labelColor)
                .frame(width: 50, alignment: .leading)

            TokenField(
                tokens: $draft.to,
                focusedField: $focusedField,
                fieldIdentifier: .to,
                onCommit: { scheduleAutoSave() },
                onPartialTextChange: { query, tokenField in
                    handlePartialTextChange(query: query, tokenField: tokenField, field: .to)
                },
                onKeyCommand: { command in
                    handleSuggestionKeyCommand(command)
                }
            )

            // Expand Cc/Bcc Button
            Button(action: {
                withAnimation(.easeInOut(duration: 0.2)) {
                    showCcBcc.toggle()
                }
            }) {
                Image(systemName: showCcBcc ? "chevron.up" : "chevron.down")
                    .font(.system(size: 12, weight: .medium))
                    .foregroundColor(labelColor)
                    .frame(width: 24, height: 24)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(showCcBcc ? "Hide Cc/Bcc" : "Show Cc/Bcc")
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 10)
    }

    // MARK: - Cc Row

    private var ccRow: some View {
        HStack(alignment: .center, spacing: 12) {
            Text("Cc:")
                .font(.system(size: 14, weight: .medium))
                .foregroundColor(labelColor)
                .frame(width: 50, alignment: .leading)

            TokenField(
                tokens: $draft.cc,
                focusedField: $focusedField,
                fieldIdentifier: .cc,
                onCommit: { scheduleAutoSave() },
                onPartialTextChange: { query, tokenField in
                    handlePartialTextChange(query: query, tokenField: tokenField, field: .cc)
                },
                onKeyCommand: { command in
                    handleSuggestionKeyCommand(command)
                }
            )

            // Spacer to align with To row
            Color.clear
                .frame(width: 24, height: 24)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 8)
        .transition(.opacity.combined(with: .move(edge: .top)))
    }

    // MARK: - Bcc Row

    private var bccRow: some View {
        HStack(alignment: .center, spacing: 12) {
            Text("Bcc:")
                .font(.system(size: 14, weight: .medium))
                .foregroundColor(labelColor)
                .frame(width: 50, alignment: .leading)

            TokenField(
                tokens: $draft.bcc,
                focusedField: $focusedField,
                fieldIdentifier: .bcc,
                onCommit: { scheduleAutoSave() },
                onPartialTextChange: { query, tokenField in
                    handlePartialTextChange(query: query, tokenField: tokenField, field: .bcc)
                },
                onKeyCommand: { command in
                    handleSuggestionKeyCommand(command)
                }
            )

            // Spacer to align with To row
            Color.clear
                .frame(width: 24, height: 24)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 8)
        .transition(.opacity.combined(with: .move(edge: .top)))
    }

    // MARK: - From Row

    private var fromRow: some View {
        HStack(spacing: 12) {
            Text("From:")
                .font(.system(size: 14, weight: .medium))
                .foregroundColor(labelColor)
                .frame(width: 50, alignment: .leading)

            // Account Menu
            Menu {
                ForEach(accounts, id: \.email) { account in
                    Button(action: {
                        draft.from = account.email
                        if let defaultSig = account.defaultSignature {
                            selectedSignature = defaultSig
                        }
                        updateBodyWithSignature()
                        scheduleAutoSave()
                    }) {
                        HStack {
                            Text(account.email)
                            if account.email == draft.from {
                                Spacer()
                                Image(systemName: "checkmark")
                            }
                        }
                    }
                }
            } label: {
                HStack(spacing: 6) {
                    Text(draft.from)
                        .font(.system(size: 14))
                        .foregroundColor(textColor)
                    Image(systemName: "chevron.down")
                        .font(.system(size: 10, weight: .medium))
                        .foregroundColor(labelColor)
                }
            }
            .buttonStyle(.plain)

            Spacer()

            // Signature Button
            if !signatures.isEmpty {
                Menu {
                    Button(action: {
                        selectedSignature = nil
                        updateBodyWithSignature()
                    }) {
                        HStack {
                            Text("None")
                            if selectedSignature == nil {
                                Spacer()
                                Image(systemName: "checkmark")
                            }
                        }
                    }

                    Divider()

                    ForEach(signatures.keys.sorted(), id: \.self) { key in
                        Button(action: {
                            selectedSignature = key
                            updateBodyWithSignature()
                        }) {
                            HStack {
                                Text(key.capitalized)
                                if selectedSignature == key {
                                    Spacer()
                                    Image(systemName: "checkmark")
                                }
                            }
                        }
                    }
                } label: {
                    Image(systemName: "signature")
                        .font(.system(size: 14))
                        .foregroundColor(labelColor)
                        .frame(width: 28, height: 28)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help("Select Signature")
            }
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 10)
    }



    // MARK: - Subject Row

    private var subjectRow: some View {
        ZStack(alignment: .leading) {
            if draft.subject.isEmpty {
                Text("Subject")
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundColor(placeholderColor)
            }

            TextField("", text: $draft.subject)
                .textFieldStyle(.plain)
                .font(.system(size: 14, weight: .semibold))
                .foregroundColor(textColor)
                .onChange(of: draft.subject) {
                    scheduleAutoSave()
                }
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 12)
    }

    // MARK: - Message Editor

    private var messageEditor: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                ZStack(alignment: .topLeading) {
                    EditableWebView(
                        plainText: $draft.body,
                        htmlSignature: draft.htmlSignature,
                        contentHeight: $editorHeight,
                        font: .systemFont(ofSize: 14),
                        textColor: NSColor(textColor),
                        backgroundColor: NSColor(Color.Detail.cardBackground),
                        placeholderText: "Message",
                        formatCommand: $formatCommand,
                        fontSizeCommand: $fontSizeCommand,
                        fontFamilyCommand: $fontFamilyCommand,
                        htmlBody: Binding(
                            get: { draft.htmlBody ?? "" },
                            set: { draft.htmlBody = $0.isEmpty ? nil : $0 }
                        ),
                        onFormatStateChange: { bold, italic, underline, strikethrough, fontSize, fontFamily, alignment in
                            isBold = bold
                            isItalic = italic
                            isUnderline = underline
                            isStrikethrough = strikethrough
                            currentFontSize = fontSize
                            currentFontFamily = fontFamily
                            currentAlignment = alignment
                        },
                        onVimModeChange: { mode in
                            vimMode = mode
                        },
                        onSearchRequest: {
                            vimSearchText = ""
                            showVimSearch = true
                        },
                        vimInsertExitKeys: KeymapsManager.shared.keymaps.keymaps
                            .filter { $0.context == "compose_normal" && $0.action == "exit_insert" }
                            .map { $0.key }
                    )
                    .frame(height: editorHeight)

                    // Native placeholder (instant, no WebView load delay)
                    if draft.body.isEmpty && (draft.htmlSignature == nil || draft.htmlSignature!.isEmpty) {
                        Text("Message")
                            .font(.system(size: 14))
                            .foregroundColor(placeholderColor)
                            .padding(.leading, 6)
                            .padding(.top, 9)
                            .allowsHitTesting(false)
                    }
                }
                .padding(.horizontal, 12)
                .padding(.top, 0)
                .padding(.bottom, 8)
                .onChange(of: draft.body) {
                    scheduleAutoSave()
                }

                // Quoted content (for reply/forward) - rendered with full HTML fidelity
                if let quoted = draft.quotedContent, !quoted.isEmpty {
                    quotedContentView(quoted)
                }
            }
        }
        .background(Color.Detail.cardBackground)
        .cornerRadius(8)
        .padding(.horizontal, 24)
        .padding(.vertical, 8)
    }

    // MARK: - Quoted Content View

    @ViewBuilder
    private func quotedContentView(_ quoted: String) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            // Visual separator
            Divider()
                .padding(.horizontal, 12)
                .padding(.vertical, 8)

            // Always use NonScrollingWebView for consistent scrolling behavior
            // For plain text, wrap in basic HTML
            let htmlContent = draft.quotedIsHTML ? quoted : plainTextToHTML(quoted)

            NonScrollingWebView(
                html: htmlContent,
                contentHeight: $quotedContentHeight
            )
            .frame(height: quotedContentHeight)
        }
    }

    /// Detect whether a signature string contains HTML tags
    private func isHTMLSignature(_ text: String) -> Bool {
        let pattern = "<\\s*(?:b|i|u|em|strong|br|div|span|p|a|img|table|tr|td|h[1-6]|ul|ol|li|hr|font|style)\\b[^>]*>"
        return text.range(of: pattern, options: [.regularExpression, .caseInsensitive]) != nil
    }

    /// Convert plain text quote to HTML with proper styling
    private func plainTextToHTML(_ text: String) -> String {
        // Escape HTML and convert newlines to <br>
        let escaped = text
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\n", with: "<br>")

        return "<div style=\"font-family: -apple-system, monospace; font-size: 13px; color: #666; white-space: pre-wrap;\">\(escaped)</div>"
    }

    // MARK: - Attachments

    private var attachmentsScrollView: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(Array(draft.attachments.enumerated()), id: \.element.id) { index, attachment in
                    AttachmentChip(
                        filename: attachment.filename,
                        size: attachment.sizeFormatted,
                        isSelected: selectedAttachmentIndex == index,
                        onClick: {
                            selectedAttachmentIndex = index
                        },
                        onRemove: {
                            removeAttachment(id: attachment.id)
                        }
                    )
                }
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 8)
        }
        .background(Color(NSColor.controlBackgroundColor))
    }


    // MARK: - Signature Handling

    private struct BodySections {
        var userContent: String
        var signature: String?
        var quotedContent: String
    }

    private func parseBodySections(_ body: String) -> BodySections {
        let quoteSeparators = [
            "---\nOn ",
            "---------- Forwarded message"
        ]

        var quotedContent = ""
        var contentBeforeQuote = body

        for separator in quoteSeparators {
            if let range = body.range(of: separator) {
                contentBeforeQuote = String(body[..<range.lowerBound])
                quotedContent = String(body[range.lowerBound...])
                break
            }
        }

        var userContent = contentBeforeQuote
        var existingSignature: String? = nil

        for (_, sigText) in signatures {
            if let sigRange = contentBeforeQuote.range(of: "\n\n" + sigText, options: .backwards) {
                userContent = String(contentBeforeQuote[..<sigRange.lowerBound])
                existingSignature = sigText
                break
            }
        }

        return BodySections(
            userContent: userContent,
            signature: existingSignature,
            quotedContent: quotedContent
        )
    }

    private func updateBodyWithSignature() {
        let sections = parseBodySections(draft.body)

        var newBody = sections.userContent

        if let signatureKey = selectedSignature,
           let signatureText = signatures[signatureKey],
           !signatureText.isEmpty
        {
            if isHTMLSignature(signatureText) {
                // HTML signature — rendered in EditableWebView
                draft.htmlSignature = signatureText
            } else {
                // Plain text — convert to HTML for EditableWebView rendering
                let htmlConverted = signatureText
                    .replacingOccurrences(of: "&", with: "&amp;")
                    .replacingOccurrences(of: "<", with: "&lt;")
                    .replacingOccurrences(of: ">", with: "&gt;")
                    .replacingOccurrences(of: "\n", with: "<br>")
                draft.htmlSignature = htmlConverted
            }
        } else {
            draft.htmlSignature = nil
        }

        if !sections.quotedContent.isEmpty {
            newBody += "\n\n" + sections.quotedContent
        }

        draft.body = newBody
    }

    // MARK: - Auto-Save

    func scheduleAutoSave() {
        autoSaveCancellable?.cancel()

        autoSaveCancellable = Just(())
            .delay(for: .seconds(0.5), scheduler: DispatchQueue.main)
            .sink { _ in
                var updatedDraft = draft
                updatedDraft.updateModifiedDate()

                Log.debug("DRAFTING", "Auto-saving draft locally only")
                DraftManager.shared.saveDraft(updatedDraft)
            }
    }

    private func saveDraftToServer() async {
        var updatedDraft = draft
        updatedDraft.updateModifiedDate()

        Log.debug("DRAFTING", "Saving draft to local storage")
        DraftManager.shared.saveDraft(updatedDraft)
    }

    // MARK: - File Handling

    private func handleFileSelection(_ result: Result<[URL], Error>) {
        switch result {
        case .success(let urls):
            for url in urls {
                guard url.startAccessingSecurityScopedResource() else {
                    Log.error("COMPOSE", "Failed to access file: \(url.lastPathComponent)")
                    continue
                }

                defer {
                    url.stopAccessingSecurityScopedResource()
                }

                do {
                    let data = try Data(contentsOf: url)

                    guard canAddAttachment(size: Int64(data.count)) else {
                        continue
                    }

                    let mimeType = getMimeType(for: url)
                    let attachment = EmailAttachment(
                        filename: url.lastPathComponent,
                        mimeType: mimeType,
                        data: data
                    )

                    draft.attachments.append(attachment)
                    scheduleAutoSave()

                } catch {
                    Log.error("COMPOSE", "Failed to read file: \(error)")
                    showErrorMessage("Failed to attach: \(url.lastPathComponent)")
                }
            }

        case .failure(let error):
            Log.error("COMPOSE", "File picker error: \(error)")
        }
    }

    private func canAddAttachment(size: Int64) -> Bool {
        guard draft.attachments.count < maxAttachments else {
            showErrorMessage("Maximum \(maxAttachments) attachments allowed")
            return false
        }

        guard size <= maxAttachmentSize else {
            showErrorMessage("File too large (max 25MB)")
            return false
        }

        guard draft.totalAttachmentSize + size <= maxTotalSize else {
            showErrorMessage("Total attachments too large (max 50MB)")
            return false
        }

        return true
    }

    private func getMimeType(for url: URL) -> String {
        if let uti = UTType(filenameExtension: url.pathExtension) {
            return uti.preferredMIMEType ?? "application/octet-stream"
        }
        return "application/octet-stream"
    }

    private func removeAttachment(id: UUID) {
        draft.attachments.removeAll { $0.id == id }
        scheduleAutoSave()
    }

    private func showErrorMessage(_ message: String) {
        errorMessage = message
        showError = true
    }

    // MARK: - Key Monitor (for QuickLook)

    private func setupKeyMonitor() {
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { event in
            if event.keyCode == 49 {
                if let index = selectedAttachmentIndex {
                    Log.debug("QUICKLOOK", "Space pressed, opening preview for attachment \(index)")
                    QuickLookManager.shared.showPreview(for: draft.attachments, startingAt: index)
                    return nil
                }
            }
            return event
        }
    }

    private func removeKeyMonitor() {
        if let monitor = keyMonitor {
            NSEvent.removeMonitor(monitor)
            keyMonitor = nil
        }
    }

}

