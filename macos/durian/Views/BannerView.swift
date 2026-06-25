//
//  BannerView.swift
//  Durian
//
//  Banner view for displaying user-facing messages
//

import AppKit
import SwiftUI

struct BannerView: View {
    let banner: BannerMessage
    let onDismiss: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: banner.severity.icon)
                .font(.system(size: 16))
                .foregroundStyle(banner.severity.color)

            VStack(alignment: .leading, spacing: 4) {
                Text(banner.title)
                    .font(.headline)
                Text(banner.message)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            .contentShape(Rectangle())
            .onTapGesture {
                banner.onTap?()
            }
            .onHover { hovering in
                if banner.onTap != nil {
                    if hovering { NSCursor.pointingHand.push() }
                    else { NSCursor.pop() }
                }
            }

            Spacer()

            if !banner.actions.isEmpty {
                HStack(spacing: 6) {
                    ForEach(banner.actions) { action in
                        Button(action.label, role: action.role) {
                            action.handler()
                        }
                        .buttonStyle(.bordered)
                        .controlSize(.small)
                    }
                }
            }

            Button {
                onDismiss()
            } label: {
                Image(systemName: "xmark")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Dismiss")
        }
        .padding(12)
        .background(
            RoundedRectangle(cornerRadius: 10)
                .fill(Color(nsColor: .windowBackgroundColor))
                .shadow(color: .black.opacity(0.2), radius: 8, y: 4)
        )
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(banner.title): \(banner.message)")
        .accessibilityAddTraits(.isStaticText)
    }
}
