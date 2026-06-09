//
//  SearchPopupView.swift
//  Durian
//
//  Raycast-style search popup for email queries
//

import SwiftUI

struct SearchPopupView: View {
    @Binding var isPresented: Bool
    @Binding var selectedEmailId: String?
    let initialQuery: String
    let onResultsActivated: (String, [MailMessage], String) -> Void

    @StateObject private var searchManager = SearchManager()
    @State private var query: String = ""
    @State private var selectedIndex: Int = 0
    @FocusState private var isTextFieldFocused: Bool

    /// Fixed glass height — keeps sampling region constant to prevent color shift on resize
    private let maxGlassHeight: CGFloat = 560

    var body: some View {
        VStack(spacing: 0) {
            // Search Input
            searchInputView

            // Only show content when query is not empty
            if !query.isEmpty {
                Divider()
                    .opacity(0.3)

                Group {
                    if searchManager.isSearching {
                        loadingView
                    } else if searchManager.results.isEmpty {
                        noResultsView
                    } else {
                        resultsListView
                    }
                }
            }
        }
        .frame(width: 680)
        .background(alignment: .top) {
            // Glass is always at max size so sampling region never changes
            Color.clear
                .frame(width: 680, height: maxGlassHeight)
                .glassEffect(.regular.tint(Color(nsColor: .windowBackgroundColor).opacity(0.45)), in: .rect(cornerRadius: 16))
        }
        .clipShape(.rect(cornerRadius: 16))
        .shadow(color: .black.opacity(0.35), radius: 32, y: 16)
        .onAppear {
            // Pre-fill query when reopening search in search mode
            if !initialQuery.isEmpty {
                query = initialQuery
                searchManager.search(query: initialQuery)
            }
            // Delay focus slightly so the window is ready to accept first responder
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.05) {
                isTextFieldFocused = true
            }
        }
        .onChange(of: query) { _, newQuery in
            searchManager.search(query: newQuery)
            selectedIndex = 0
        }
        .onChange(of: isTextFieldFocused) { _, focused in
            // Re-assert focus so key handlers (Escape, arrows) keep working
            if !focused {
                DispatchQueue.main.async { isTextFieldFocused = true }
            }
        }
        .onKeyPress(.upArrow) {
            if selectedIndex > 0 { selectedIndex -= 1 }
            return .handled
        }
        .onKeyPress(.downArrow) {
            if selectedIndex < searchManager.results.count - 1 { selectedIndex += 1 }
            return .handled
        }
        .onReceive(NotificationCenter.default.publisher(for: .popupSelectNext)) { _ in
            if selectedIndex < searchManager.results.count - 1 { selectedIndex += 1 }
        }
        .onReceive(NotificationCenter.default.publisher(for: .popupSelectPrev)) { _ in
            if selectedIndex > 0 { selectedIndex -= 1 }
        }
        .onKeyPress(.escape) {
            close()
            return .handled
        }
        .onKeyPress(.return) {
            selectCurrentResult()
            return .handled
        }
    }

    // MARK: - Subviews

    private var searchInputView: some View {
        HStack(spacing: 14) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(.secondary)
                .font(.title2)
                .fontWeight(.medium)

            TextField("Search emails...", text: $query)
                .textFieldStyle(.plain)
                .font(.title2)
                .focused($isTextFieldFocused)

            if !query.isEmpty {
                Button {
                    query = ""
                    searchManager.clear()
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(.tertiary)
                        .font(.title3)
                }
                .buttonStyle(.plain)
            }

            if searchManager.isSearching {
                ProgressView()
                    .scaleEffect(0.8)
            }
        }
        .padding(.horizontal, 20)
        .padding(.vertical, 18)
    }

    private var loadingView: some View {
        HStack(spacing: 10) {
            ProgressView()
                .scaleEffect(0.8)
            Text("Searching...")
                .foregroundStyle(.secondary)
                .font(.subheadline)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 24)
    }

    private var noResultsView: some View {
        VStack(spacing: 6) {
            Image(systemName: "magnifyingglass")
                .font(.title)
                .foregroundStyle(.tertiary)
            Text("No results")
                .font(.subheadline)
                .foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 24)
    }

    private var resultsListView: some View {
        VStack(spacing: 0) {
            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(spacing: 0) {
                        ForEach(Array(searchManager.results.enumerated()), id: \.element.id) { index, email in
                            SearchResultRow(
                                email: email,
                                isSelected: index == selectedIndex
                            )
                            .id(index)
                            .contentShape(Rectangle())
                            .onTapGesture {
                                selectedIndex = index
                                selectCurrentResult()
                            }
                        }
                    }
                }
                .frame(maxHeight: 450)
                .onChange(of: selectedIndex) { _, newIndex in
                    withAnimation(.easeInOut(duration: 0.1)) {
                        proxy.scrollTo(newIndex, anchor: .center)
                    }
                }
            }

            Divider()
                .opacity(0.3)

            // Footer with result count and keyboard hints
            HStack {
                Text("\(searchManager.results.count) result\(searchManager.results.count == 1 ? "" : "s")")
                    .font(.caption2)
                    .foregroundStyle(.tertiary)

                Spacer()

                HStack(spacing: 12) {
                    Text("↑↓ Navigate")
                    Text("↵ Open")
                    Text("⎋ Close")
                }
                .font(.caption2)
                .foregroundStyle(.tertiary)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 8)
        }
    }

    // MARK: - Actions

    private func selectCurrentResult() {
        guard !searchManager.results.isEmpty,
              selectedIndex < searchManager.results.count else { return }

        let email = searchManager.results[selectedIndex]
        selectedEmailId = email.id
        onResultsActivated(query, searchManager.results, email.id)
        close()
    }

    private func close() {
        searchManager.clear()
        query = ""
        isPresented = false
    }
}

// MARK: - Search Result Row

struct SearchResultRow: View {
    let email: MailMessage
    let isSelected: Bool

    /// Tags to hide from pills (already represented as icons or not useful)
    private static let hiddenTags: Set<String> = ["unread", "attachment", "flagged"]

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            // Unread indicator
            Circle()
                .fill(email.isRead ? Color.clear : Color.blue)
                .frame(width: 8, height: 8)
                .padding(.top, 6)

            VStack(alignment: .leading, spacing: 4) {
                // Row 1: sender, flagged, attachment, date
                HStack(spacing: 6) {
                    Text(senderName)
                        .font(.headline)
                        .fontWeight(email.isRead ? .regular : .semibold)
                        .lineLimit(1)

                    Spacer()

                    if email.isPinned {
                        Image(systemName: "star.fill")
                            .font(.caption)
                            .foregroundStyle(.yellow)
                    }

                    if email.hasAttachment {
                        Image(systemName: "paperclip")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }

                    Text(email.date)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }

                // Row 2: subject
                Text(email.subject.isEmpty ? "(No Subject)" : email.subject)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)

                // Row 3: body preview
                if let preview = email.bodyPreview, !preview.isEmpty {
                    Text(preview)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .lineLimit(2)
                }

                // Row 4: tag pills
                if !visibleTags.isEmpty {
                    HStack(spacing: 4) {
                        ForEach(visibleTags, id: \.self) { tag in
                            Text(tag)
                                .font(.caption2)
                                .foregroundStyle(.secondary)
                                .padding(.horizontal, 6)
                                .padding(.vertical, 2)
                                .background(Color.gray.opacity(0.2), in: Capsule())
                        }
                    }
                }
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 10)
        .background(isSelected ? ProfileManager.shared.resolvedAccentColor.opacity(0.2) : Color.clear, in: RoundedRectangle(cornerRadius: 8))
        .padding(.horizontal, 6)
        .contentShape(Rectangle())
    }

    private var senderName: String {
        let from = email.from
        if let range = from.range(of: "<") {
            let namePart = String(from[..<range.lowerBound]).trimmingCharacters(in: .whitespaces)
            if !namePart.isEmpty { return namePart }
        }
        return from
    }

    private var visibleTags: [String] {
        guard let tags = email.tags else { return [] }
        return tags
            .split(separator: ",")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty && !Self.hiddenTags.contains($0) }
    }
}
