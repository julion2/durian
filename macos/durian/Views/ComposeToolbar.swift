//
//  ComposeToolbar.swift
//  Durian
//
//  Rich text formatting toolbar (visual only, functionality to be added later)
//

import SwiftUI

/// Formatting toolbar for the email composer
/// Currently visual-only - formatting functionality will be added later
struct ComposeToolbar: View {
    var onFormat: ((String) -> Void)?
    var onFontSize: ((Int) -> Void)?
    var onFontFamily: ((String) -> Void)?
    var boldActive: Bool = false
    var italicActive: Bool = false
    var underlineActive: Bool = false
    var strikethroughActive: Bool = false
    var currentFontSize: Int = 13
    var currentFontFamily: String = "Helvetica"
    var currentAlignment: String = "left"
    var vimMode: String = "insert"

    private var vimModeLabel: String {
        switch vimMode {
        case "normal": return "NORMAL"
        case "visual": return "VISUAL"
        case "visual_line": return "V-LINE"
        default: return "INSERT"
        }
    }

    private var vimModeColor: Color {
        switch vimMode {
        case "normal": return Color(.systemOrange)
        case "visual", "visual_line": return Color(.systemPurple)
        default: return Color(.systemGreen)
        }
    }

    private let availableFonts = ["Helvetica", "Arial", "Times New Roman", "Georgia", "Courier"]
    private let availableSizes = [9, 10, 11, 12, 13, 14, 16, 18, 20, 24, 28, 32]

    var body: some View {
        HStack(spacing: 12) {
            // Vim Mode Indicator
            Text(vimModeLabel)
                .font(.system(size: 10, weight: .bold, design: .monospaced))
                .foregroundColor(vimModeColor)
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(
                    vimModeColor.opacity(0.12),
                    in: RoundedRectangle(cornerRadius: 4)
                )

            Divider()
                .frame(height: 20)

            // Font Picker
            fontPicker

            // Size Picker
            sizePicker

            Divider()
                .frame(height: 20)

            // Bold, Italic, Underline
            textStyleButtons

            Divider()
                .frame(height: 20)

            // Alignment
            alignmentButtons

            Divider()
                .frame(height: 20)

            // Lists
            listButtons

            Divider()
                .frame(height: 20)

            // Clear Formatting
            clearFormattingButton

            Spacer()
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 12)
        .background(Color(NSColor.windowBackgroundColor))
    }

    // MARK: - Font Picker

    private var fontPicker: some View {
        Menu {
            ForEach(availableFonts, id: \.self) { font in
                Button(action: { onFontFamily?(font) }) {
                    HStack {
                        Text(font)
                        if font == currentFontFamily {
                            Spacer()
                            Image(systemName: "checkmark")
                        }
                    }
                }
            }
        } label: {
            HStack(spacing: 8) {
                Text(currentFontFamily)
                    .font(.system(size: 13, weight: .medium))
                    .foregroundColor(Color.Detail.textPrimary)
                Image(systemName: "chevron.down")
                    .font(.system(size: 10, weight: .medium))
                    .foregroundColor(Color.Detail.textPrimary)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(minWidth: 120)
            .background(Color.Detail.buttonBackground)
            .cornerRadius(8)
        }
        .buttonStyle(.plain)
    }

    // MARK: - Size Picker

    private var sizePicker: some View {
        Menu {
            ForEach(availableSizes, id: \.self) { size in
                Button(action: { onFontSize?(size) }) {
                    HStack {
                        Text("\(size)")
                        if size == currentFontSize {
                            Spacer()
                            Image(systemName: "checkmark")
                        }
                    }
                }
            }
        } label: {
            HStack(spacing: 8) {
                Text("\(currentFontSize)")
                    .font(.system(size: 13, weight: .medium))
                    .foregroundColor(Color.Detail.textPrimary)
                Image(systemName: "chevron.down")
                    .font(.system(size: 10, weight: .medium))
                    .foregroundColor(Color.Detail.textPrimary)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            .frame(minWidth: 60)
            .background(Color.Detail.buttonBackground)
            .cornerRadius(8)
        }
        .buttonStyle(.plain)
    }

    // MARK: - Text Style Buttons (B, I, U)

    private var textStyleButtons: some View {
        HStack(spacing: 4) {
            ToolbarIconButton(icon: "bold", action: { onFormat?("bold") }, isActive: boldActive)
            ToolbarIconButton(icon: "italic", action: { onFormat?("italic") }, isActive: italicActive)
            ToolbarIconButton(icon: "underline", action: { onFormat?("underline") }, isActive: underlineActive)
            ToolbarIconButton(icon: "strikethrough", action: { onFormat?("strikeThrough") }, isActive: strikethroughActive)
        }
    }

    // MARK: - Alignment Buttons

    private var alignmentButtons: some View {
        HStack(spacing: 4) {
            ToolbarIconButton(icon: "text.alignleft", action: { onFormat?("justifyLeft") }, isActive: currentAlignment == "left")
            ToolbarIconButton(icon: "text.aligncenter", action: { onFormat?("justifyCenter") }, isActive: currentAlignment == "center")
            ToolbarIconButton(icon: "text.alignright", action: { onFormat?("justifyRight") }, isActive: currentAlignment == "right")
            ToolbarIconButton(icon: "text.justify", action: { onFormat?("justifyFull") }, isActive: currentAlignment == "justify")
        }
    }

    // MARK: - List Buttons

    private var listButtons: some View {
        HStack(spacing: 4) {
            ToolbarIconButton(icon: "list.bullet", action: { onFormat?("insertUnorderedList") })
            ToolbarIconButton(icon: "list.number", action: { onFormat?("insertOrderedList") })
        }
    }

    // MARK: - Clear Formatting Button

    private var clearFormattingButton: some View {
        ClearFormattingButton(action: { onFormat?("removeFormat") })
    }

}

// MARK: - Clear Formatting Button (textformat + diagonal line)

struct ClearFormattingButton: View {
    let action: () -> Void
    @State private var isHovered: Bool = false

    var body: some View {
        Button(action: action) {
            ZStack {
                Image(systemName: "textformat")
                    .font(.system(size: 14))
                Rectangle()
                    .frame(width: 18, height: 1.5)
                    .rotationEffect(.degrees(-30))
            }
            .foregroundColor(Color.Detail.textSecondary)
            .frame(width: 28, height: 28)
            .background(
                RoundedRectangle(cornerRadius: 6)
                    .fill(isHovered ? Color.Detail.buttonBackground : Color.clear)
            )
        }
        .buttonStyle(.plain)
        .onHover { hovering in
            isHovered = hovering
        }
    }
}

// MARK: - Toolbar Icon Button

struct ToolbarIconButton: View {
    let icon: String
    let action: () -> Void
    var isActive: Bool = false

    @State private var isHovered: Bool = false

    var body: some View {
        Button(action: action) {
            Image(systemName: icon)
                .font(.system(size: 14, weight: .regular))
                .foregroundColor(isActive ? .accentColor : Color.Detail.textSecondary)
                .frame(width: 28, height: 28)
                .background(
                    RoundedRectangle(cornerRadius: 6)
                        .fill(isActive ? ProfileManager.shared.resolvedAccentColor.opacity(0.15) : (isHovered ? Color.Detail.buttonBackground : Color.clear))
                )
        }
        .buttonStyle(.plain)
        .onHover { hovering in
            isHovered = hovering
        }
    }
}
