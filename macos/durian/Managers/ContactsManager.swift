//
//  ContactsManager.swift
//  Durian
//
//  Manages contacts via HTTP API (delegates to CLI backend)
//

import Foundation

// MARK: - Contact Model

struct Contact: Identifiable, Hashable {
    let id: String
    let email: String
    let name: String?
    var lastUsed: Date?
    var usageCount: Int
    let source: String
    let createdAt: Date

    /// Returns formatted display string: "Name <email>" or just "email"
    var displayString: String {
        if let name = name, !name.isEmpty {
            return "\(name) <\(email)>"
        }
        return email
    }

    /// Returns just the name or email if no name
    var displayName: String {
        if let name = name, !name.isEmpty {
            return name
        }
        return email
    }

    /// Convert from API response to domain model
    init(from response: ContactResponse) {
        id = response.id
        email = response.email
        name = response.name
        usageCount = response.usage_count
        source = response.source
        lastUsed = Self.parseDate(response.last_used)
        createdAt = Self.parseDate(response.created_at) ?? Date()
    }

    init(id: String, email: String, name: String?, lastUsed: Date? = nil,
         usageCount: Int, source: String, createdAt: Date)
    {
        self.id = id
        self.email = email
        self.name = name
        self.lastUsed = lastUsed
        self.usageCount = usageCount
        self.source = source
        self.createdAt = createdAt
    }

    private static func parseDate(_ string: String?) -> Date? {
        guard let string, !string.isEmpty else { return nil }
        let iso8601 = ISO8601DateFormatter()
        iso8601.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = iso8601.date(from: string) { return date }
        iso8601.formatOptions = [.withInternetDateTime]
        if let date = iso8601.date(from: string) { return date }
        let sqliteFormatter = DateFormatter()
        sqliteFormatter.dateFormat = "yyyy-MM-dd HH:mm:ss"
        sqliteFormatter.timeZone = TimeZone(identifier: "UTC")
        return sqliteFormatter.date(from: string)
    }
}

// MARK: - Contacts Manager

@MainActor
class ContactsManager {
    static let shared = ContactsManager()

    private init() {}

    private var backend: EmailBackend? {
        AccountManager.shared.emailBackend
    }

    // MARK: - Public API

    /// Search contacts by email or name prefix
    func search(query: String, limit: Int = 10) async -> [Contact] {
        guard !query.isEmpty, let backend else { return [] }
        let results = await backend.searchContacts(query: query, limit: limit)
        return results.map { Contact(from: $0) }
    }

    /// Find contact by exact name match (case-insensitive)
    func findByExactName(_ name: String) async -> Contact? {
        guard !name.isEmpty, let backend else { return nil }
        guard let result = await backend.findContactByExactName(name) else { return nil }
        return Contact(from: result)
    }

    /// Get all contacts ordered by usage
    func list(limit: Int = 100) async -> [Contact] {
        guard let backend else { return [] }
        let results = await backend.listContacts(limit: limit)
        return results.map { Contact(from: $0) }
    }

    /// Increment usage count for emails (fire-and-forget)
    func incrementUsage(for emails: [String]) {
        let validEmails = emails.filter { $0.contains("@") && $0.contains(".") }
        guard !validEmails.isEmpty else { return }
        Task { [backend] in
            guard let backend else {
                Log.warning("CONTACTS", "Cannot update usage: backend not available")
                return
            }
            await backend.incrementContactUsage(for: validEmails)
        }
    }
}
