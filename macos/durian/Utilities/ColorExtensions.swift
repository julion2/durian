//
//  ColorExtensions.swift
//  Durian
//
//  Design system colors from Figma + hex initializer
//

import AppKit
import SwiftUI

// MARK: - Hex Initializer

extension Color {
    init(hex: String) {
        let hex = hex.trimmingCharacters(in: CharacterSet.alphanumerics.inverted)
        var int: UInt64 = 0
        // A non-hex string (e.g. a malformed vdir color file) must fall back
        // instead of silently rendering black.
        guard Scanner(string: hex).scanHexInt64(&int) else {
            self.init(red: 0.6, green: 0.4, blue: 0.2)
            return
        }
        let r, g, b: Double
        switch hex.count {
        case 3: // RGB (e.g. "F53" → "FF5533")
            r = Double((int >> 8) & 0xF) / 15
            g = Double((int >> 4) & 0xF) / 15
            b = Double(int & 0xF) / 15
        case 6: // RRGGBB (e.g. "FF5733")
            r = Double((int >> 16) & 0xFF) / 255
            g = Double((int >> 8) & 0xFF) / 255
            b = Double(int & 0xFF) / 255
        default:
            r = 0.6; g = 0.4; b = 0.2  // Fallback to brown-ish
        }
        self.init(red: r, green: g, blue: b)
    }
}

// MARK: - Calendar Event Tints

extension Color {
    /// A soft wash of a calendar color for a timed event pill's fill. Meant
    /// to be layered over an opaque surface (Color.Detail.cardBackground):
    /// the low opacity keeps the hue recognizable over both the light and
    /// dark surface while titles stay in the primary text color.
    func eventTint(selected: Bool = false) -> Color {
        opacity(selected ? 0.32 : 0.16)
    }

    /// The near-solid fill an all-day chip uses — strong enough to carry
    /// white text, dialed back a step when unselected so a selected chip
    /// reads a notch stronger.
    func eventSolid(selected: Bool = false) -> Color {
        opacity(selected ? 1.0 : 0.9)
    }
}

// MARK: - Design System Colors (from Figma)

extension Color {
    enum Detail {
        private static func nsColor(hex: String) -> NSColor {
            var int: UInt64 = 0
            Scanner(string: hex).scanHexInt64(&int)
            let r = CGFloat((int >> 16) & 0xFF) / 255.0
            let g = CGFloat((int >> 8) & 0xFF) / 255.0
            let b = CGFloat(int & 0xFF) / 255.0
            return NSColor(srgbRed: r, green: g, blue: b, alpha: 1.0)
        }

        private static func adaptive(light: String, dark: String) -> Color {
            let dynamic = NSColor(name: nil, dynamicProvider: { appearance in
                let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
                return isDark ? nsColor(hex: dark) : nsColor(hex: light)
            })
            return Color(nsColor: dynamic)
        }

        // Text colors
        static let textPrimary = adaptive(light: "0a0a0a", dark: "f5f5f5")
        static let textSecondary = adaptive(light: "4a5565", dark: "9ca3af")
        static let textTertiary = adaptive(light: "6a7282", dark: "7d8590")
        static let textBody = adaptive(light: "101828", dark: "e5e7eb")
        static let textPlaceholder = adaptive(light: "717182", dark: "6b7280")

        // Accent colors
        static let linkBlue = Color(hex: "155dfc")

        // Background colors
        static let cardBackground = adaptive(light: "ffffff", dark: "2a2a2c")
        static let border = adaptive(light: "e5e7eb", dark: "3a3a3c")
        static let buttonBackground = adaptive(light: "f3f3f5", dark: "3a3a3c")
    }
}
