//
//  ComposeWindow.swift
//  Durian
//
//  Standalone window wrapper for ComposeForm
//

import SwiftUI

/// Wrapper view for the compose window
struct ComposeWindow: View {
    let draftId: UUID

    @Environment(\.dismiss) private var dismiss
    @Environment(\.openWindow) private var openWindow
    @StateObject private var draftService = DraftService.shared
    @StateObject private var sendingManager = EmailSendingManager.shared
    @StateObject private var profileManager = ProfileManager.shared

    @State private var triggerSend: Bool = false
    @State private var showSaveError: Bool = false
    @State private var saveErrorMessage: String = ""
    @State private var isSaving: Bool = false
    @State private var showingFilePicker: Bool = false
    @State private var allowClose: Bool = false
    @State private var showInvalidEmailWarning: Bool = false
    @State private var invalidEmails: [String] = []
    @State private var composeNSWindow: NSWindow?

    var body: some View {
        Group {
            let accounts = ConfigManager.shared.getAccounts()

            if isSaving {
                // Window is closing — show nothing to avoid "Draft Not Found" flash
                Color.clear
            } else if let draft = draftService.getDraft(id: draftId) {
                composeView(draft: draft, accounts: accounts)
            } else {
                // Draft not found - might have been discarded
                ContentUnavailableView("Draft Not Found", systemImage: "doc.questionmark")
                    .onAppear { closeWindow() }
            }
        }
        .tint(profileManager.resolvedAccentColor)
    }

    // MARK: - Subviews

    private var noAccountsView: some View {
        VStack(spacing: 16) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 48))
                .foregroundStyle(.secondary)
            Text("No accounts configured")
                .font(.title2)
            Text("Add an account in config.pkl to send emails")
                .foregroundStyle(.secondary)
            Button("Close") {
                closeWindow()
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding()
    }

    private func composeView(draft: EmailDraft, accounts: [MailAccount]) -> some View {
        ZStack {
        ComposeForm(
            accounts: accounts,
            existingDraft: draft,
            triggerSend: $triggerSend,
            showingFilePicker: $showingFilePicker,
            currentDraft: Binding(
                get: { draftService.getDraft(id: draftId) },
                set: { newDraft in
                    if let newDraft = newDraft {
                        draftService.updateDraft(id: draftId, draft: newDraft)
                    }
                }
            ),
            onDismiss: { handleDismiss() }
        )
        .toolbar {
            // Saving indicator (left)
            ToolbarItem(placement: .cancellationAction) {
                if isSaving || draftService.savingDrafts.contains(draftId) {
                    ProgressView()
                        .scaleEffect(0.7)
                }
            }

            // Action icons (right)
            ToolbarItemGroup(placement: .primaryAction) {
                // Placeholder: AI Assist (not yet implemented)
                // Button(action: {}) {
                //     Image(systemName: "sparkles")
                // }
                // .help("AI Assist")

                // Attachment
                Button(action: {
                    showingFilePicker = true
                }) {
                    Image(systemName: "paperclip")
                }
                .help("Add Attachment")

                // Placeholder: Templates (not yet implemented)
                // Button(action: {}) {
                //     Image(systemName: "doc.on.doc")
                // }
                // .help("Templates")

                // More options
                Menu {
                    Button(action: {}) {
                        Label("Show Original", systemImage: "doc.plaintext")
                    }
                    .disabled(draftService.getDraft(id: draftId)?.inReplyTo == nil)
                } label: {
                    Image(systemName: "ellipsis")
                }
                .help("More Options")

                // Send
                Button(action: {
                    triggerSend = true
                }) {
                    Image(systemName: "paperplane.fill")
                }
                .keyboardShortcut(.return, modifiers: .command)
                .disabled(
                    draftService.getDraft(id: draftId)?.to.isEmpty ?? true
                    || sendingManager.isSending
                )
                .help("Send (⌘Return)")
            }
        }
        .onChange(of: triggerSend) { oldValue, newValue in
            if newValue {
                handleSend()
                triggerSend = false
            }
        }
        .onChange(of: showSaveError) { _, show in
            if show {
                BannerManager.shared.showCritical(
                    title: "Draft Not Saved",
                    message: saveErrorMessage,
                    actions: [
                        BannerAction("Retry") { handleDismiss() },
                        BannerAction("Discard", role: .destructive) {
                            draftService.discard(id: draftId)
                            closeWindow()
                        },
                        BannerAction("Keep Editing", role: .cancel) {
                            BannerManager.shared.dismiss()
                        }
                    ]
                )
                showSaveError = false
            }
        }
        .onChange(of: showInvalidEmailWarning) { _, show in
            if show {
                BannerManager.shared.showCritical(
                    title: "Invalid Email Addresses",
                    message: invalidEmails.joined(separator: "\n"),
                    actions: [
                        BannerAction("Cancel", role: .cancel) {
                            BannerManager.shared.dismiss()
                        },
                        BannerAction("Send Anyway") {
                            BannerManager.shared.dismiss()
                            handleSendWithSkipValidation()
                        }
                    ]
                )
                showInvalidEmailWarning = false
            }
        }

        // Sending overlay (visible only in compose window)
        if sendingManager.isSending {
            VStack {
                Spacer()
                HStack {
                    Spacer()
                    HStack(spacing: 10) {
                        ProgressView()
                            .controlSize(.small)
                        Text("Sending...")
                            .font(.headline)
                    }
                    .padding(12)
                    .background(
                        RoundedRectangle(cornerRadius: 10)
                            .fill(Color(nsColor: .windowBackgroundColor))
                            .shadow(color: .black.opacity(0.2), radius: 8, y: 4)
                    )
                }
            }
            .padding(16)
            .transition(.move(edge: .bottom).combined(with: .opacity))
            .animation(.easeInOut(duration: 0.3), value: sendingManager.isSending)
        }
        } // ZStack
        .background(WindowCloseGuard(allowClose: $allowClose, window: $composeNSWindow, onCloseAttempt: { handleDismiss() }))
        .onAppear { KeymapHandler.shared.composeActive = true }
        .onDisappear { KeymapHandler.shared.composeActive = false }
    }

    // MARK: - Actions

    /// Activate the next visible Durian window so the app doesn't appear
    /// to hide when the compose window is removed via orderOut.
    private func activateMainWindow() {
        if let mainWindow = NSApp.windows.first(where: {
            $0.isVisible && $0.canBecomeKey && !($0 is NSPanel)
        }) {
            mainWindow.makeKeyAndOrderFront(nil)
        }
        // Always activate the app to prevent it from going behind other apps
        NSApp.activate()
    }

    private func closeWindow() {
        allowClose = true
        dismiss()
    }

    private func handleDismiss() {
        // Hide the compose window instantly, save draft in background
        composeNSWindow?.orderOut(nil)
        activateMainWindow()
        isSaving = true

        Task {
            do {
                _ = try await draftService.saveToServer(id: draftId)
                await MainActor.run {
                    closeWindow()
                }
            } catch {
                await MainActor.run {
                    // Save failed — bring window back so user can act
                    isSaving = false
                    composeNSWindow?.makeKeyAndOrderFront(nil)
                    saveErrorMessage = error.localizedDescription
                    showSaveError = true
                }
            }
        }
    }

    private func handleSend() {
        guard let draft = draftService.getDraft(id: draftId) else { return }

        // Pre-validate recipients so we can show inline warnings before closing
        guard draft.hasRecipients else { return }

        let allRecipients = draft.to + draft.cc + draft.bcc
        let invalid = EmailHelper.validateRecipients(allRecipients)
        if !invalid.isEmpty {
            invalidEmails = invalid
            showInvalidEmailWarning = true
            return
        }

        // Capture refs before closing — window is gone after closeWindow()
        let reopenDraft = openWindow
        let capturedDraftId = draftId

        // Hide the compose window instantly
        composeNSWindow?.orderOut(nil)
        activateMainWindow()

        // Close via SwiftUI for proper cleanup
        isSaving = true
        closeWindow()

        Task {
            do {
                try await sendingManager.send(
                    draft: draft,
                    fromAccount: draft.from,
                    skipValidation: true,
                    onUndo: {
                        if let newId = DraftService.shared.cloneDraft(id: capturedDraftId) {
                            reopenDraft(value: newId)
                        }
                    },
                    onConfirmedSent: {
                        Task {
                            await DraftService.shared.deleteAfterSend(id: capturedDraftId)
                        }
                    }
                )
            } catch {
                Log.error("COMPOSE", "Send failed - \(error)")
                await MainActor.run {
                    if let sendError = error as? EmailSendingError {
                        BannerManager.shared.show(sendError.bannerMessage)
                    } else {
                        BannerManager.shared.showCritical(title: "Email Not Sent", message: error.localizedDescription)
                    }
                }
            }
        }
    }

    private func handleSendWithSkipValidation() {
        guard let draft = draftService.getDraft(id: draftId) else { return }

        let reopenDraft = openWindow
        let capturedDraftId = draftId

        // Hide content and close immediately — validation already handled
        isSaving = true
        closeWindow()

        Task {
            do {
                try await sendingManager.send(
                    draft: draft,
                    fromAccount: draft.from,
                    skipValidation: true,
                    onUndo: {
                        if let newId = DraftService.shared.cloneDraft(id: capturedDraftId) {
                            reopenDraft(value: newId)
                        }
                    },
                    onConfirmedSent: {
                        Task {
                            await DraftService.shared.deleteAfterSend(id: capturedDraftId)
                        }
                    }
                )
            } catch {
                Log.error("COMPOSE", "Send failed - \(error)")
                await MainActor.run {
                    if let sendError = error as? EmailSendingError {
                        BannerManager.shared.show(sendError.bannerMessage)
                    } else {
                        BannerManager.shared.showCritical(title: "Email Not Sent", message: error.localizedDescription)
                    }
                }
            }
        }
    }
}
