//
//  DraftManager.swift
//  Durian
//
//  Local-draft persistence via the durian HTTP API (/local-drafts).
//  Distinct from DraftService, which manages IMAP-synced drafts via the CLI.
//

import Foundation

class DraftManager {
    static let shared = DraftManager()

    private let baseURL = AppServer.localDraftsURL
    private let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        return e
    }()
    private let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        return d
    }()

    private init() {}

    private func authHeader(for request: inout URLRequest) {
        if let token = EmailBackend.authToken {
            request.addValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
    }

    func saveDraft(_ draft: EmailDraft) {
        var cleaned = draft
        cleaned.to = Self.filterValidAddresses(draft.to)
        cleaned.cc = Self.filterValidAddresses(draft.cc)
        cleaned.bcc = Self.filterValidAddresses(draft.bcc)

        do {
            let draftData = try encoder.encode(cleaned)
            let body = try JSONSerialization.data(withJSONObject: [
                "draft_json": try JSONSerialization.jsonObject(with: draftData)
            ])

            var request = URLRequest(url: baseURL.appendingPathComponent(draft.id.uuidString))
            request.httpMethod = "PUT"
            request.addValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = body
            request.timeoutInterval = 5
            authHeader(for: &request)

            URLSession.shared.dataTask(with: request) { _, response, error in
                if let error {
                    Log.error("DRAFTING", "Failed to save draft: \(error)")
                    return
                }
                if let http = response as? HTTPURLResponse, http.statusCode == 200 {
                    Log.debug("DRAFTING", "Draft saved: \(draft.id.uuidString)")
                }
            }.resume()
        } catch {
            Log.error("DRAFTING", "Failed to encode draft: \(error)")
        }
    }

    func deleteDraft(id: UUID) {
        var request = URLRequest(url: baseURL.appendingPathComponent(id.uuidString))
        request.httpMethod = "DELETE"
        request.timeoutInterval = 5
        authHeader(for: &request)

        URLSession.shared.dataTask(with: request) { _, _, error in
            if let error {
                Log.error("DRAFTING", "Failed to delete draft: \(error)")
                return
            }
            Log.debug("DRAFTING", "Draft deleted: \(id.uuidString)")
        }.resume()
    }

    /// Filter out addresses that don't contain a valid email (must have @)
    private static func filterValidAddresses(_ addresses: [String]) -> [String] {
        addresses.filter { addr in
            let trimmed = addr.trimmingCharacters(in: .whitespaces)
            return trimmed.isEmpty || trimmed.contains("@")
        }
    }
}
