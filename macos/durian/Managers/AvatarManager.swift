//
//  AvatarManager.swift
//  Durian
//
//  Manages avatar loading from Gravatar and Brandfetch with caching
//

import AppKit
import CryptoKit
import Foundation

@MainActor
class AvatarManager: ObservableObject {
    static let shared = AvatarManager()

    // Memory Cache
    private var imageCache = NSCache<NSString, NSImage>()

    // Failed lookups - don't retry for 24h
    private var failedLookups: [String: Date] = [:]

    // Brandfetch token for company logos
    private let brandfetchToken = "1idWonATCJFIseiVHIH"

    // Personal domains → Gravatar (company domains → Brandfetch)
    private let personalDomains: Set<String> = [
        // Google
        "gmail.com", "googlemail.com",
        // Microsoft
        "outlook.com", "hotmail.com", "live.com", "msn.com", "outlook.de",
        // Yahoo
        "yahoo.com", "yahoo.de", "ymail.com",
        // German providers
        "gmx.de", "gmx.net", "gmx.at", "gmx.ch",
        "web.de",
        "t-online.de",
        "freenet.de",
        "mail.de", "email.de",
        // Apple
        "icloud.com", "me.com", "mac.com",
        // Other
        "aol.com",
        "protonmail.com", "proton.me", "pm.me",
        "posteo.de", "mailbox.org",
        "tutanota.com", "tutanota.de", "tuta.io"
    ]

    private init() {
        imageCache.countLimit = 200
        imageCache.totalCostLimit = 50 * 1024 * 1024 // 50MB
    }

    // MARK: - Public API

    /// Load avatar for email address or name
    /// - Parameters:
    ///   - email: Full email string (can be "Name <email>" format) or just a name
    ///   - size: Desired image size in pixels
    /// - Returns: NSImage if found, nil otherwise (fallback to initials)
    func loadAvatar(for email: String, size: Int = 128) async -> NSImage? {
        // Extract first author from authors string
        // Separators: ", " (comma) or "|" without leading space
        // " | " (space-pipe-space) is part of name, don't split
        let firstAuthor: String
        if email.contains("<") {
            // Has email address - don't split, use as-is
            firstAuthor = email
        } else {
            // Split by comma first (primary separator)
            let byComma = email.components(separatedBy: ",").first?
                .trimmingCharacters(in: .whitespaces) ?? email

            // Check for "|" without leading space (author separator)
            if let pipeRange = byComma.range(of: "|"),
               pipeRange.lowerBound > byComma.startIndex,
               byComma[byComma.index(before: pipeRange.lowerBound)] != " "
            {
                firstAuthor = String(byComma[..<pipeRange.lowerBound])
                    .trimmingCharacters(in: .whitespaces)
            } else {
                firstAuthor = byComma
            }
        }

        // Extract clean email from "Name <email>" format
        let cleanEmail = extractEmail(from: firstAuthor).lowercased()
        let cacheKey = cleanEmail as NSString

        // Check memory cache
        if let cached = imageCache.object(forKey: cacheKey) {
            return cached
        }

        // Check if recently failed (don't retry for 24h)
        if let failedDate = failedLookups[cleanEmail],
           Date().timeIntervalSince(failedDate) < 86400
        {
            return nil
        }

        // Extract domain - either from email or guess from name
        var domain: String?
        var emailForGravatar = cleanEmail  // Track actual email for Gravatar lookup

        if cleanEmail.contains("@") {
            domain = extractDomain(from: cleanEmail)
        } else {
            // No email address (list view) - try contacts DB lookup first
            if let contact = await ContactsManager.shared.findByExactName(cleanEmail),
               !contact.email.isEmpty
            {
                // Found contact! Use their email for avatar lookup
                emailForGravatar = contact.email.lowercased()
                domain = extractDomain(from: contact.email)
                Log.debug("AVATAR", "Contacts lookup '\(cleanEmail)' → \(contact.email)")
            } else {
                // Fallback: guess domain from name (e.g., "Amazon.de" → "amazon.de")
                domain = guessDomainFromName(cleanEmail)
            }
        }

        guard let domain = domain else {
            return nil
        }

        // Try appropriate source based on domain type
        let image: NSImage?
        if personalDomains.contains(domain) {
            // Personal email → try Gravatar
            image = await fetchGravatar(email: emailForGravatar, size: size)
        } else {
            // Company email → try Brandfetch
            image = await fetchBrandfetch(domain: domain)
        }

        // Cache result
        if let image = image {
            imageCache.setObject(image, forKey: cacheKey)
        } else {
            failedLookups[cleanEmail] = Date()
        }

        return image
    }

    /// Clear all caches
    func clearCache() {
        imageCache.removeAllObjects()
        failedLookups.removeAll()
    }

    // MARK: - Private Methods

    /// Extract email address from "Name <email>" format
    private func extractEmail(from string: String) -> String {
        if let start = string.range(of: "<"), let end = string.range(of: ">") {
            return String(string[start.upperBound..<end.lowerBound])
                .trimmingCharacters(in: .whitespaces)
        }
        return string.trimmingCharacters(in: .whitespaces)
    }

    /// Extract domain from email address
    private func extractDomain(from email: String) -> String? {
        guard let atIndex = email.firstIndex(of: "@") else { return nil }
        return String(email[email.index(after: atIndex)...]).lowercased()
    }

    /// Guess domain from a company/brand name (for list view where we only have author name)
    /// Only returns domain if name already looks like a domain (e.g. "Amazon.de")
    /// Returns nil for regular names to avoid false matches
    private func guessDomainFromName(_ name: String) -> String? {
        let cleanName = name.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()

        // Must look like a domain: no spaces, has dot, no @, minimum length
        guard !cleanName.contains(" "),
              !cleanName.contains("@"),
              cleanName.contains("."),
              cleanName.count >= 4 else
        {  // minimum "a.de"
            return nil
        }

        return cleanName
    }

    /// Fetch avatar from Gravatar
    /// Uses MD5 hash of email, returns 404 if no account exists
    private func fetchGravatar(email: String, size: Int) async -> NSImage? {
        // MD5 hash of lowercase trimmed email
        let hash = Insecure.MD5.hash(data: Data(email.utf8))
            .map { String(format: "%02x", $0) }
            .joined()

        // d=404 returns 404 if no Gravatar exists (instead of default image)
        let urlString = "https://gravatar.com/avatar/\(hash)?d=404&s=\(size)"
        guard let url = URL(string: urlString) else { return nil }

        return await fetchImage(from: url)
    }

    /// Known Brandfetch placeholder image MD5 hashes.
    /// Brandfetch returns 200 + a generic placeholder for domains without a real logo,
    /// so we reject responses matching these hashes to avoid showing the same generic icon everywhere.
    private static let brandfetchPlaceholderHashes: Set<String> = [
        "38926c4ecbd5590c77a969ae5a516b49",  // "known domain, no logo" placeholder (2588 bytes)
        "ad18dbe2bac10daa8e99cc27efed1607",  // "unknown domain" placeholder (310 bytes)
    ]

    /// Fetch company logo from Brandfetch
    /// Requires browser User-Agent to bypass hotlinking protection
    private func fetchBrandfetch(domain: String) async -> NSImage? {
        let urlString = "https://cdn.brandfetch.io/\(domain)?c=\(brandfetchToken)"
        guard let url = URL(string: urlString) else { return nil }

        // Browser User-Agent required to bypass hotlinking block
        var request = URLRequest(url: url)
        request.setValue("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko)", forHTTPHeaderField: "User-Agent")

        return await fetchImage(request: request, rejectPlaceholders: true)
    }

    /// Generic image fetcher with error handling (URL version)
    private func fetchImage(from url: URL, rejectPlaceholders: Bool = false) async -> NSImage? {
        let request = URLRequest(url: url)
        return await fetchImage(request: request, rejectPlaceholders: rejectPlaceholders)
    }

    /// Generic image fetcher with error handling (Request version)
    private func fetchImage(request: URLRequest, rejectPlaceholders: Bool = false) async -> NSImage? {
        do {
            let (data, response) = try await URLSession.shared.data(for: request)

            guard let httpResponse = response as? HTTPURLResponse,
                  httpResponse.statusCode == 200 else
            {
                return nil
            }

            // Reject known Brandfetch placeholder images
            if rejectPlaceholders {
                let hash = Insecure.MD5.hash(data: data)
                    .map { String(format: "%02x", $0) }
                    .joined()
                if Self.brandfetchPlaceholderHashes.contains(hash) {
                    return nil
                }
            }

            // Verify we got image data, not HTML error page
            guard let image = NSImage(data: data),
                  image.isValid else
            {
                return nil
            }

            return image
        } catch {
            Log.error("AVATAR", "Failed to fetch \(request.url?.absoluteString ?? "unknown"): \(error.localizedDescription)")
            return nil
        }
    }
}
