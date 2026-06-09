//
//  ComposeFormSubviews.swift
//  Durian
//
//  Small reusable subviews used by ComposeForm (AttachmentChip and
//  VimSearchPill). Split out to keep ComposeForm focused on the form
//  itself.
//

import AppKit
import SwiftUI

// MARK: - Attachment Chip

struct AttachmentChip: View {
    let filename: String
    let size: String
    let isSelected: Bool
    let onClick: () -> Void
    let onRemove: () -> Void

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: "doc.fill")
                .font(.caption)
                .foregroundStyle(.secondary)

            VStack(alignment: .leading, spacing: 2) {
                Text(filename)
                    .font(.caption)
                    .lineLimit(1)
                Text(size)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }

            Button(action: onRemove) {
                Image(systemName: "xmark.circle.fill")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .background(isSelected ? ProfileManager.shared.resolvedAccentColor.opacity(0.3) : ProfileManager.shared.resolvedAccentColor.opacity(0.1))
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(isSelected ? ProfileManager.shared.resolvedAccentColor : Color.clear, lineWidth: 2)
        )
        .cornerRadius(8)
        .onTapGesture {
            onClick()
        }
    }
}

// MARK: - Vim Search Pill

struct VimSearchPill: View {
    @Binding var text: String
    var onSubmit: () -> Void
    var onDismiss: () -> Void
    @FocusState private var isFocused: Bool

    var body: some View {
        HStack(spacing: 4) {
            Text("/")
                .font(.system(size: 12, design: .monospaced))
                .foregroundColor(Color.Detail.textTertiary)
            TextField("", text: $text)
                .font(.system(size: 12, design: .monospaced))
                .textFieldStyle(.plain)
                .frame(width: 180)
                .focused($isFocused)
                .onSubmit { onSubmit() }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 5)
        .background(Color.Detail.cardBackground)
        .cornerRadius(8)
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(Color.Detail.border, lineWidth: 1)
        )
        .shadow(color: .black.opacity(0.08), radius: 8, y: 4)
        .onAppear { isFocused = true }
        .onChange(of: isFocused) { _, focused in
            if !focused { onDismiss() }
        }
    }
}
