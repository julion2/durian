//
//  AddressUtils.swift
//  Durian
//
//  Shared utilities for extracting display names and emails from address strings.
//  Handles nested quotes, email-as-name, and whitespace collapsing.
//

import Foundation

enum AddressUtils {

    // MARK: - Public API

    /// Extract display name from an email address string.
    ///
    /// Handles:
    /// - `"'Julian  Schenker'" <j@x.com>` → `Julian Schenker` (nested quotes + whitespace)
    /// - `029401dcae34$...@foo.com <d@foo.com>` → `` (email-as-name discarded)
    /// - `julian@example.com` → `julian` (bare email → local part)
    /// - `Julian Schenker <j@x.com>` → `Julian Schenker`
    static func extractName(from address: String) -> String {
        var name: String
        let hasBracket = address.contains("<")

        if hasBracket, let range = address.range(of: "<") {
            name = String(address[..<range.lowerBound]).trimmingCharacters(in: .whitespaces)
        } else {
            name = address.trimmingCharacters(in: .whitespaces)
                .trimmingCharacters(in: CharacterSet(charactersIn: "<>"))
                .trimmingCharacters(in: .whitespaces)
        }

        name = stripNestedQuotes(name)

        // Name contains @ → it's an email used as name — show full email
        if name.contains("@") {
            return name
        }

        name = collapseWhitespace(name)
        if name.isEmpty {
            // No display name — use local part of email
            let email = extractEmail(from: address)
            if let at = email.firstIndex(of: "@") {
                return String(email[..<at])
            }
            return address
        }
        return name
    }

    /// Extract email address from `"Name <email>"` format.
    /// Returns the content between `<>`, or the full string if no angle brackets.
    static func extractEmail(from address: String) -> String {
        if let start = address.range(of: "<"), let end = address.range(of: ">") {
            return String(address[start.upperBound..<end.lowerBound])
                .trimmingCharacters(in: .whitespaces)
        }
        return address.trimmingCharacters(in: .whitespaces)
    }

    // MARK: - Private Helpers

    /// Remove one layer of matching outer quotes (" or ').
    private static func stripOuterQuotes(_ s: String) -> String {
        var r = s
        if (r.hasPrefix("\"") && r.hasSuffix("\"")) ||
           (r.hasPrefix("'") && r.hasSuffix("'"))
        {
            r = String(r.dropFirst().dropLast())
        }
        return r.trimmingCharacters(in: .whitespaces)
    }

    /// Repeatedly strip outer quotes until stable (handles `"'Name'"`).
    static func stripNestedQuotes(_ s: String) -> String {
        var result = s
        while true {
            let next = stripOuterQuotes(result)
            if next == result { break }
            result = next
        }
        return result
    }

    /// Collapse runs of whitespace into a single space.
    static func collapseWhitespace(_ s: String) -> String {
        s.replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespaces)
    }
}
