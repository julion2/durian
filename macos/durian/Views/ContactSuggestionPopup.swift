//
//  ContactSuggestionPopup.swift
//  Durian
//
//  Custom autocomplete popup for contact suggestions
//  Design: Figma node 6:2
//

import SwiftUI

// MARK: - Contact Suggestion Row

struct ContactSuggestionRow: View {
    let contact: Contact
    let isSelected: Bool

    var body: some View {
        HStack(spacing: 8) {
            // Avatar with initials
            AvatarView(name: contact.displayName, email: contact.email, size: 22)

            // Name and Email
            VStack(alignment: .leading, spacing: 1) {
                Text(contact.displayName)
                    .font(.system(size: 12, weight: .medium))
                    .foregroundColor(.primary)
                    .lineLimit(1)

                Text(contact.email)
                    .font(.system(size: 10, weight: .medium))
                    .foregroundColor(Color.Detail.textSecondary)
                    .lineLimit(1)
            }

            Spacer(minLength: 0)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .frame(height: 37)
        .background(isSelected ? Color.Detail.buttonBackground : Color.Detail.cardBackground)
        .cornerRadius(8)
    }
}

// MARK: - Contact Suggestion Popup

struct ContactSuggestionPopup: View {
    let contacts: [Contact]
    let selectedIndex: Int
    let onSelect: (Contact) -> Void
    let onDismiss: () -> Void

    private let maxHeight: CGFloat = 200  // ~5 rows visible
    private let popupWidth: CGFloat = 200

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView(.vertical, showsIndicators: false) {
                VStack(spacing: 0) {
                    ForEach(Array(contacts.enumerated()), id: \.element.id) { index, contact in
                        ContactSuggestionRow(
                            contact: contact,
                            isSelected: index == selectedIndex
                        )
                        .id(index)
                        .contentShape(Rectangle())
                        .onTapGesture {
                            onSelect(contact)
                        }
                    }
                }
            }
            .frame(maxHeight: maxHeight)
            .onChange(of: selectedIndex) { _, newIndex in
                withAnimation(.easeInOut(duration: 0.1)) {
                    proxy.scrollTo(newIndex, anchor: .center)
                }
            }
        }
        .padding(6)
        .frame(width: popupWidth)
        .background(Color.Detail.cardBackground)
        .cornerRadius(8)
        .overlay(
            RoundedRectangle(cornerRadius: 8)
                .stroke(Color.Detail.border, lineWidth: 1)
        )
        .shadow(color: Color.primary.opacity(0.08), radius: 8, x: 0, y: 4)
        .fixedSize(horizontal: true, vertical: true)
    }
}
