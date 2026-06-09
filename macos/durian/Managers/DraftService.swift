//
//  DraftService.swift
//  Durian
//
//  Manages email drafts with IMAP synchronization
//

import Combine
import SwiftUI

/// Response from durian draft save command
struct DraftSaveResponse: Decodable {
    let ok: Bool
    let error: String?
    let message_id: String?
    let uid: UInt32?
}

/// Response from durian draft delete command
struct DraftDeleteResponse: Decodable {
    let ok: Bool
    let error: String?
}

/// Errors that can occur during draft operations
enum DraftError: Error, LocalizedError {
    case noAccountConfigured
    case saveFailed(String)
    case deleteFailed(String)
    case loadFailed(String)
    case cliError(String)

    var errorDescription: String? {
        switch self {
        case .noAccountConfigured:
            return "No email account configured"
        case .saveFailed(let message):
            return "Failed to save draft: \(message)"
        case .deleteFailed(let message):
            return "Failed to delete draft: \(message)"
        case .loadFailed(let message):
            return "Failed to load draft: \(message)"
        case .cliError(let message):
            return "CLI error: \(message)"
        }
    }
}

/// Manages draft lifecycle - creation, editing, saving to IMAP
@MainActor
class DraftService: ObservableObject {
    static let shared = DraftService()

    /// Active drafts indexed by window UUID
    @Published var activeDrafts: [UUID: EmailDraft] = [:]

    /// Drafts currently being saved (to show progress)
    @Published var savingDrafts: Set<UUID> = []

    private let durianPath: String

    private init() {
        durianPath = FileManager.default.resolveDurianPath() ?? "\(NSHomeDirectory())/.local/bin/durian"
    }

    // MARK: - Draft Lifecycle

    /// Create a new draft and return its UUID
    func createDraft(from account: String? = nil) -> UUID {
        let id = UUID()
        let fromAddress = account ?? ConfigManager.shared.getAccounts().first?.email ?? ""
        let draft = EmailDraft(from: fromAddress)
        activeDrafts[id] = draft
        Log.debug("DRAFT", "Created new draft \(id)")
        return id
    }

    /// Create a draft from an existing EmailDraft (for reply/forward)
    func createDraft(with draft: EmailDraft) -> UUID {
        let id = draft.id
        activeDrafts[id] = draft
        Log.debug("DRAFT", "Created draft from template \(id)")
        return id
    }

    /// Get a draft by its UUID
    func getDraft(id: UUID) -> EmailDraft? {
        activeDrafts[id]
    }

    /// Update a draft (called on every change in the compose view)
    func updateDraft(id: UUID, draft: EmailDraft) {
        activeDrafts[id] = draft
    }

    /// Discard a draft without saving to IMAP
    func discard(id: UUID) {
        activeDrafts.removeValue(forKey: id)
        Log.debug("DRAFT", "Discarded draft \(id)")
    }

    /// Clone a draft with a fresh UUID (for undo-send: reopens in a new window).
    func cloneDraft(id: UUID) -> UUID? {
        guard var draft = activeDrafts.removeValue(forKey: id) else { return nil }
        let newId = UUID()
        draft.id = newId
        activeDrafts[newId] = draft
        Log.debug("DRAFT", "Cloned draft \(id) → \(newId)")
        return newId
    }

    // MARK: - IMAP Operations

    /// Save a draft to IMAP (called on window close)
    /// Returns the new Message-ID if successful
    func saveToServer(id: UUID) async throws -> String {
        guard let draft = activeDrafts[id] else {
            throw DraftError.saveFailed("Draft not found")
        }

        // Discard drafts where the user hasn't typed any content
        // (signatures and quoted content don't count)
        if !draft.hasUserContent {
            activeDrafts.removeValue(forKey: id)
            DraftManager.shared.deleteDraft(id: id)
            Log.debug("DRAFT", "Discarded draft with no user content \(id)")
            return ""
        }

        // Get account
        guard let account = ConfigManager.shared.getAccounts().first(where: { $0.email == draft.from })
              ?? ConfigManager.shared.getAccounts().first else
        {
            throw DraftError.noAccountConfigured
        }

        savingDrafts.insert(id)
        defer { savingDrafts.remove(id) }

        // Build command arguments
        var args = [
            "draft", "save",
            "--account", account.email,
            "--from", draft.from,
            "--subject", draft.subject,
            "--body", draft.body
        ]

        // Add recipients
        if !draft.to.isEmpty {
            args += ["--to", draft.to.joined(separator: ",")]
        }
        if !draft.cc.isEmpty {
            args += ["--cc", draft.cc.joined(separator: ",")]
        }
        if !draft.bcc.isEmpty {
            args += ["--bcc", draft.bcc.joined(separator: ",")]
        }

        // Reply threading headers
        if let inReplyTo = draft.inReplyTo, !inReplyTo.isEmpty {
            args += ["--in-reply-to", inReplyTo]
        }
        if let references = draft.references, !references.isEmpty {
            args += ["--references", references]
        }

        // Replace existing draft if we have a message ID
        if let messageId = draft.messageId, !messageId.isEmpty {
            args += ["--replace", messageId]
        }

        // HTML flag
        if draft.isHTML {
            args.append("--html")
        }

        // Note: Attachments would need to be saved to temp files first
        // For now, we skip attachments in IMAP drafts (they're stored locally)

        // Execute CLI command
        let result = try await executeCLI(args: args)

        // Parse response
        guard let data = result.data(using: .utf8) else {
            throw DraftError.cliError("Invalid response")
        }

        let response = try JSONDecoder().decode(DraftSaveResponse.self, from: data)

        if !response.ok {
            throw DraftError.saveFailed(response.error ?? "Unknown error")
        }

        // Update draft with new message ID
        var updatedDraft = draft
        updatedDraft.messageId = response.message_id
        activeDrafts[id] = updatedDraft

        Log.info("DRAFT", "Saved to IMAP - Message-ID: \(response.message_id ?? "unknown")")

        return response.message_id ?? ""
    }

    /// Delete a draft from IMAP after sending
    func deleteAfterSend(id: UUID) async {
        guard let draft = activeDrafts[id],
              let messageId = draft.messageId,
              !messageId.isEmpty else
        {
            // No server-side draft to delete
            activeDrafts.removeValue(forKey: id)
            return
        }

        // Get account
        guard let account = ConfigManager.shared.getAccounts().first(where: { $0.email == draft.from })
              ?? ConfigManager.shared.getAccounts().first else
        {
            activeDrafts.removeValue(forKey: id)
            return
        }

        do {
            let args = [
                "draft", "delete",
                "--account", account.email,
                messageId
            ]

            let result = try await executeCLI(args: args)

            if let data = result.data(using: .utf8),
               let response = try? JSONDecoder().decode(DraftDeleteResponse.self, from: data),
               response.ok
            {
                Log.info("DRAFT", "Deleted from IMAP - \(messageId)")
            }
        } catch {
            Log.error("DRAFT", "Failed to delete from IMAP - \(error)")
        }

        activeDrafts.removeValue(forKey: id)
    }

    /// Load a draft from backend for editing
    /// Returns the UUID of the new draft window
    func loadFromBackend(messageId: String) async throws -> UUID {
        // Load draft metadata — content is populated by the caller
        let id = UUID()
        var draft = EmailDraft(from: ConfigManager.shared.getAccounts().first?.email ?? "")
        draft.messageId = messageId
        activeDrafts[id] = draft
        return id
    }

    // MARK: - CLI Execution

    private func executeCLI(args: [String]) async throws -> String {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: self.durianPath)
                process.arguments = args

                // Set PATH for durian CLI
                var environment = ProcessInfo.processInfo.environment
                let homebrewPaths = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin"
                environment["PATH"] = homebrewPaths + ":" + (environment["PATH"] ?? "")
                process.environment = environment

                let stdout = Pipe()
                let stderr = Pipe()
                process.standardOutput = stdout
                process.standardError = stderr

                do {
                    try process.run()
                    process.waitUntilExit()

                    let outputData = stdout.fileHandleForReading.readDataToEndOfFile()
                    let output = String(data: outputData, encoding: .utf8) ?? ""

                    if process.terminationStatus != 0 {
                        let errorData = stderr.fileHandleForReading.readDataToEndOfFile()
                        let errorOutput = String(data: errorData, encoding: .utf8) ?? "Unknown error"
                        continuation.resume(throwing: DraftError.cliError(errorOutput))
                    } else {
                        continuation.resume(returning: output)
                    }
                } catch {
                    continuation.resume(throwing: DraftError.cliError(error.localizedDescription))
                }
            }
        }
    }
}
