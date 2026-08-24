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
        guard [3, 6].contains(hex.count), hex.allSatisfy({ $0.isHexDigit }),
              Scanner(string: hex).scanHexInt64(&int)
        else {
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
            preconditionFailure("validated hex length")
        }
        self.init(red: r, green: g, blue: b)
    }
}

// MARK: - Contrast

extension Color {
    /// Black or white, whichever has the greater WCAG contrast against this
    /// color in the current appearance.
    func contrastingForeground() -> Color {
        let color = NSColor(self).usingColorSpace(.sRGB) ?? .black
        var red: CGFloat = 0
        var green: CGFloat = 0
        var blue: CGFloat = 0
        var alpha: CGFloat = 0
        color.getRed(&red, green: &green, blue: &blue, alpha: &alpha)

        func linear(_ component: CGFloat) -> CGFloat {
            if component <= 0.04045 { return component / 12.92 }
            return CGFloat(pow(Double((component + 0.055) / 1.055), 2.4))
        }

        let luminance = 0.2126 * linear(red) + 0.7152 * linear(green) + 0.0722 * linear(blue)
        return luminance > 0.179 ? .black : .white
    }
}

// MARK: - Calendar Event Colors

/// Memoizes the derived fill/ink pair per source color. The derivation is
/// pure, but each miss costs an NSColor colorspace conversion plus two fresh
/// dynamic NSColors — and eventFill/eventInk are called for every event block
/// on every render of the week/month grids (dozens of blocks, re-rendered on
/// each drag frame and cursor step). There are only a handful of distinct
/// calendar colors, so the cache turns that into a dictionary lookup.
/// NSLock-guarded (same pattern as Log) so it is safe from any thread even
/// though SwiftUI bodies run on the main one.
private enum EventColorCache {
    struct Pair {
        let fill: Color
        let ink: Color
    }

    private static let lock = NSLock()
    private static var pairs: [Color: Pair] = [:]

    static func pair(for color: Color, derive: () -> Pair) -> Pair {
        lock.lock()
        if let cached = pairs[color] {
            lock.unlock()
            return cached
        }
        lock.unlock()
        // Derive outside the lock: NSColor conversion is the expensive part
        // and must not serialize unrelated lookups. A racing double-derive is
        // harmless — both sides compute the same value.
        let derived = derive()
        lock.lock()
        pairs[color] = derived
        lock.unlock()
        return derived
    }
}

extension Color {
    /// This color's HSB components. Resolved through deviceRGB so a dynamic
    /// or catalog color (e.g. the `.secondary` fallback a calendar with no
    /// color of its own gets) still yields concrete numbers.
    private var hsbComponents: (hue: CGFloat, saturation: CGFloat) {
        let ns = NSColor(self).usingColorSpace(.deviceRGB) ?? NSColor.gray
        var h: CGFloat = 0, s: CGFloat = 0, b: CGFloat = 0, a: CGFloat = 0
        ns.getHue(&h, saturation: &s, brightness: &b, alpha: &a)
        return (h, s)
    }

    /// A light/dark pair built from fixed HSB values.
    private static func dynamicHSB(light: (CGFloat, CGFloat, CGFloat),
                                   dark: (CGFloat, CGFloat, CGFloat)) -> Color
    {
        let dynamic = NSColor(name: nil, dynamicProvider: { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            let c = isDark ? dark : light
            return NSColor(hue: c.0, saturation: c.1, brightness: c.2, alpha: 1)
        })
        return Color(nsColor: dynamic)
    }

    /// The memoized fill/ink pair for this color; derives both on first use.
    private var eventPair: EventColorCache.Pair {
        EventColorCache.pair(for: self) {
            let (h, saturation) = hsbComponents
            // Whether this color carries a hue at all, or is a gray. A
            // calendar with no color of its own, and the neutral "+X"
            // overflow block, land here.
            let hasHue = saturation >= 0.08
            let c: CGFloat = hasHue ? 1 : 0
            return EventColorCache.Pair(
                fill: Color.dynamicHSB(light: (h, c * 0.24, hasHue ? 0.98 : 0.91),
                                       dark: (h, c * 0.45, hasHue ? 0.30 : 0.26)),
                ink: Color.dynamicHSB(light: (h, c * 0.95, 0.36),
                                      dark: (h, c * 0.42, 0.95))
            )
        }
    }

    /// The fill of an event block: a pale wash of the calendar's hue.
    ///
    /// The HUE is the only thing taken from the calendar — both chroma and
    /// lightness are pinned. Server colors arrive at any saturation and any
    /// lightness, and a block has to carry readable text whichever it is, so
    /// normalizing only one of the two is not enough. (It is specifically not
    /// enough to raise HSB brightness: at high saturation that yields a fully
    /// vivid color, not a tint — a wash needs the SATURATION pulled down.)
    ///
    /// Grays get a slightly darker fill, because at the saturation-derived
    /// value a neutral calendar would sit at #FAFAFA and vanish into the grid.
    func eventFill() -> Color {
        eventPair.fill
    }

    /// The text color for content sitting on `eventFill()`: the same hue at
    /// full chroma and the far end of the lightness range. Ink in the block's
    /// own color family reads as part of the block, where flat black or white
    /// would read as a label stuck on top of it — and against a wash this
    /// pale it clears 7:1 for every hue.
    func eventInk() -> Color {
        eventPair.ink
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

        /// The keyboard cursor. Neutral on purpose: it has to clear 3:1
        /// against whatever it borders on BOTH sides (an event fill in any
        /// calendar hue, and the grid behind it), which a user-configurable
        /// accent cannot promise. Being the one non-hue on screen is also
        /// what makes it findable among the coloured blocks.
        static let cursor = adaptive(light: "3f3f46", dark: "d4d4d8")

        // Background colors
        static let cardBackground = adaptive(light: "ffffff", dark: "2a2a2c")
        static let border = adaptive(light: "e5e7eb", dark: "3a3a3c")
        static let buttonBackground = adaptive(light: "f3f3f5", dark: "3a3a3c")
    }
}
