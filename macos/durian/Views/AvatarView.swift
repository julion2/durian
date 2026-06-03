//
//  AvatarView.swift
//  Durian
//
//  Avatar with real images (Gravatar/Favicon) and initials fallback
//

import SwiftUI

struct AvatarView: View {
    let name: String
    var email: String? = nil  // Optional email for avatar lookup
    var size: CGFloat = 36

    @State private var loadedImage: NSImage? = nil

    private static let avatarColors: [Color] = [
        .red, .orange, .yellow, .green, .mint, .teal,
        .cyan, .blue, .indigo, .purple, .pink, .brown
    ]

    var body: some View {
        ZStack {
            if let image = loadedImage {
                // Real avatar image
                Image(nsImage: image)
                    .resizable()
                    .aspectRatio(contentMode: .fill)
                    .clipShape(Circle())
            } else {
                // Fallback: Initials with hash-based color
                Circle()
                    .fill(colorForName)

                Text(initials)
                    .font(.system(size: size * 0.4, weight: .semibold))
                    .foregroundColor(.white)
            }
        }
        .frame(width: size, height: size)
        .accessibilityLabel("Avatar for \(extractDisplayName(from: name))")
        .accessibilityHidden(true)
        .task(id: email) {
            await loadAvatarImage()
        }
    }

    // MARK: - Avatar Loading

    private func loadAvatarImage() async {
        guard let email = email, !email.isEmpty else { return }

        // Request 2x size for retina displays
        let requestSize = Int(size * 2)
        loadedImage = await AvatarManager.shared.loadAvatar(for: email, size: requestSize)
    }

    // MARK: - Initials (Fallback)

    /// Extract initials from name (max 2 characters)
    /// "Julian Schenker" → "JS"
    /// "Atlassian Home" → "AH"
    /// "info@example.com" → "IN"
    private var initials: String {
        let cleanName = extractDisplayName(from: name)
        let words = cleanName.split(separator: " ")

        if words.count >= 2 {
            // First letter of first two words
            let first = words[0].prefix(1).uppercased()
            let second = words[1].prefix(1).uppercased()
            return first + second
        } else if let firstWord = words.first, !firstWord.isEmpty {
            // First 1-2 letters of single word
            return String(firstWord.prefix(2)).uppercased()
        } else {
            return "?"
        }
    }

    /// Get consistent color based on name hash
    private var colorForName: Color {
        let cleanName = extractDisplayName(from: name).lowercased()
        var hash = 0
        for char in cleanName.unicodeScalars {
            hash = hash &* 31 &+ Int(char.value)
        }
        let index = abs(hash) % Self.avatarColors.count
        return Self.avatarColors[index]
    }

    private func extractDisplayName(from: String) -> String {
        AddressUtils.extractName(from: from)
    }
}
