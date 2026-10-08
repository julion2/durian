//
//  ContentView+Keymaps.swift
//  Durian
//
//  Vim-style keymap handler registration for ContentView.
//

import SwiftUI

extension ContentView {
    /// Register all keymap handlers
    func registerKeymapHandlers() {
        // Navigation: j/k with count support (5j, 3k)
        // In visual mode, extends selection from anchor to cursor
        keymapHandler.registerHandler(for: .nextEmail) { [self] count in
            await MainActor.run {
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current + count)
                } else if !sortedEmailIds.isEmpty {
                    navigateToEmail(at: 0)
                }
            }
        }

        keymapHandler.registerHandler(for: .prevEmail) { [self] count in
            await MainActor.run {
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current - count)
                } else if !sortedEmailIds.isEmpty {
                    navigateToEmail(at: sortedEmailIds.count - 1)
                }
            }
        }

        // First/Last: gg, G
        keymapHandler.registerSimpleHandler(for: .firstEmail) { [self] in
            await MainActor.run {
                navigateToEmail(at: 0)
            }
        }

        keymapHandler.registerSimpleHandler(for: .lastEmail) { [self] in
            await MainActor.run {
                navigateToEmail(at: sortedEmailIds.count - 1)
            }
        }

        // Page navigation: Ctrl+d, Ctrl+u (10 emails per page)
        let pageSize = 10
        keymapHandler.registerHandler(for: .pageDown) { [self] count in
            await MainActor.run {
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current + (pageSize * count))
                } else {
                    navigateToEmail(at: 0)
                }
            }
        }

        keymapHandler.registerHandler(for: .pageUp) { [self] count in
            await MainActor.run {
                if let current = currentEmailIndex() {
                    navigateToEmail(at: current - (pageSize * count))
                } else {
                    navigateToEmail(at: sortedEmailIds.count - 1)
                }
            }
        }

        // Search: /
        keymapHandler.registerSimpleHandler(for: .search) { [self] in
            await MainActor.run {
                showSearchPopup = true
            }
        }

        // Tag Picker: t
        keymapHandler.registerSimpleHandler(for: .tagPicker) { [self] in
            await MainActor.run {
                guard !markedEmails.isEmpty else { return }
                showTagPicker = true
                Task { allTags = await accountManager.fetchAllTags() }
            }
        }

        // Compose: c
        keymapHandler.registerSimpleHandler(for: .compose) { [self] in
            await MainActor.run {
                openNewCompose()
            }
        }

        // Reply: r
        keymapHandler.registerSimpleHandler(for: .reply) { [self] in
            guard let emailId = await MainActor.run(body: { markedEmails.first }) else { return }
            if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                await MainActor.run { applyStandaloneEmail(loaded) }
            }
            await MainActor.run { replyToSelected() }
        }

        // Reply All: R (Shift+r)
        keymapHandler.registerSimpleHandler(for: .replyAll) { [self] in
            guard let emailId = await MainActor.run(body: { markedEmails.first }) else { return }
            if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                await MainActor.run { applyStandaloneEmail(loaded) }
            }
            await MainActor.run { replyAllToSelected() }
        }

        // Forward: f
        keymapHandler.registerSimpleHandler(for: .forward) { [self] in
            guard let emailId = await MainActor.run(body: { markedEmails.first }) else { return }
            if let loaded = await accountManager.fetchEmailBody(id: emailId) {
                await MainActor.run { applyStandaloneEmail(loaded) }
            }
            await MainActor.run { forwardSelected() }
        }

        // View control: q - close detail, search popup, or tag picker (NOT escape - that's for visual mode)
        keymapHandler.registerSimpleHandler(for: .closeDetail) { [self] in
            await MainActor.run {
                if showSearchPopup {
                    showSearchPopup = false
                } else if showTagPicker {
                    showTagPicker = false
                } else {
                    detailMode = .empty
                }
            }
        }

        // Toggle Pin: s
        keymapHandler.registerSimpleHandler(for: .toggleStar) { [self] in
            await MainActor.run {
                guard let emailId = markedEmails.first else { return }
                Task {
                    await accountManager.togglePin(id: emailId)
                }
            }
        }

        // Toggle Read: u - now works with multi-selection
        keymapHandler.registerSimpleHandler(for: .toggleRead) { [self] in
            await MainActor.run {
                toggleRead()
            }
        }

        // Archive: a - add archive tag and remove inbox tag (works with multi-selection)
        keymapHandler.registerSimpleHandler(for: .archiveEmail) { [self] in
            let ids = await MainActor.run { () -> Set<String> in
                let ids = markedEmails
                guard !ids.isEmpty else { return [] }
                let next = nextEmailId(after: ids)
                visualModeAnchor = nil
                keymapHandler.engine.exitVisualMode()
                accountManager.removeLocally(ids: ids)
                advanceCursor(to: next)
                return ids
            }
            for id in ids {
                await accountManager.modifyTagsWithoutSync(id: id, add: ["archive"], remove: ["inbox"])
            }
            await accountManager.syncAndRefresh()
        }

        // Tag Op: configurable tag operations (e.g. T → +todo -inbox, W → +waiting -inbox)
        keymapHandler.registerSimpleHandler(for: .tagOp) { [self] in
            let seq = keymapHandler.engine.lastMatchedSequence
            guard let tagsStr = SequenceMatcher.shared.tagOpTags(for: seq, context: keymapHandler.engine.activeContext) else { return }

            // Parse "+tag1 -tag2" into add/remove arrays
            let parts = tagsStr.split(separator: " ").map(String.init)
            let add = parts.filter { $0.hasPrefix("+") }.map { String($0.dropFirst()) }
            let remove = parts.filter { $0.hasPrefix("-") }.map { String($0.dropFirst()) }

            let ids = await MainActor.run { () -> Set<String> in
                let ids = markedEmails
                guard !ids.isEmpty else { return [] }
                // If removing inbox, advance cursor (like archive)
                if remove.contains("inbox") {
                    let next = nextEmailId(after: ids)
                    visualModeAnchor = nil
                    keymapHandler.engine.exitVisualMode()
                    accountManager.removeLocally(ids: ids)
                    advanceCursor(to: next)
                }
                return ids
            }
            for id in ids {
                await accountManager.modifyTagsWithoutSync(id: id, add: add, remove: remove)
            }
            await accountManager.syncAndRefresh()
        }

        // Delete: dd - works with multi-selection
        keymapHandler.registerSimpleHandler(for: .deleteEmail) { [self] in
            let ids = await MainActor.run { () -> Set<String> in
                guard !markedEmails.isEmpty else { return [] }
                let ids = markedEmails
                let next = nextEmailId(after: ids)
                accountManager.removeLocally(ids: ids)
                visualModeAnchor = nil
                keymapHandler.engine.exitVisualMode()
                advanceCursor(to: next)
                return ids
            }
            await accountManager.deleteMessagesWithoutSync(ids: ids)
            await accountManager.syncAndRefresh()
        }

        // ═══════════════════════════════════════════════════════════
        // FOLDER NAVIGATION HANDLERS
        // ═══════════════════════════════════════════════════════════

        // gi - Go to Inbox
        keymapHandler.registerSimpleHandler(for: .goInbox) { [self] in
            await MainActor.run { selectedTagID = "inbox" }
        }

        // gs - Go to Sent
        keymapHandler.registerSimpleHandler(for: .goSent) { [self] in
            await MainActor.run { selectedTagID = "sent" }
        }

        // gd - Go to Drafts
        keymapHandler.registerSimpleHandler(for: .goDrafts) { [self] in
            await MainActor.run { selectedTagID = "drafts" }
        }

        // ga - Go to Archive
        keymapHandler.registerSimpleHandler(for: .goArchive) { [self] in
            await MainActor.run { selectedTagID = "archive" }
        }

        // g1-g9 - Go to folder by position (sections skipped)
        keymapHandler.registerSimpleHandler(for: .goFolder) { [self] in
            let seq = keymapHandler.engine.lastMatchedSequence
            guard let digit = seq.last, let index = digit.wholeNumberValue, index >= 1 else { return }
            await MainActor.run {
                let folders = accountManager.mailFolders.filter { !$0.isSection }
                guard index <= folders.count else { return }
                selectedTagID = folders[index - 1].name
            }
        }

        // J/K - Next/Prev folder (sections skipped, wraps around)
        keymapHandler.registerSimpleHandler(for: .nextFolder) { [self] in
            await MainActor.run {
                let folders = accountManager.mailFolders.filter { !$0.isSection }
                guard !folders.isEmpty else { return }
                let currentIndex = folders.firstIndex { $0.name == accountManager.selectedFolder } ?? -1
                let nextIndex = (currentIndex + 1) % folders.count
                selectedTagID = folders[nextIndex].name
            }
        }

        // gf - Folder Picker
        keymapHandler.registerSimpleHandler(for: .folderPicker) { [self] in
            await MainActor.run {
                showFolderPicker = true
            }
        }

        keymapHandler.registerSimpleHandler(for: .prevFolder) { [self] in
            await MainActor.run {
                let folders = accountManager.mailFolders.filter { !$0.isSection }
                guard !folders.isEmpty else { return }
                let currentIndex = folders.firstIndex { $0.name == accountManager.selectedFolder } ?? 0
                let prevIndex = (currentIndex - 1 + folders.count) % folders.count
                selectedTagID = folders[prevIndex].name
            }
        }

        // ═══════════════════════════════════════════════════════════
        // VISUAL MODE HANDLERS
        // ═══════════════════════════════════════════════════════════

        // v - Enter LINE visual mode (range selection: anchor to cursor)
        keymapHandler.registerSimpleHandler(for: .enterVisualMode) { [self] in
            await MainActor.run {
                guard keymapHandler.engine.visualModeType == .none else { return }
                keymapHandler.engine.enterVisualMode(.line)
                // Set anchor to current cursor position
                visualModeAnchor = cursorEmailId
                Log.debug("VISUAL", "Entered LINE mode, anchor: \(visualModeAnchor ?? "nil")")
            }
        }

        // V (Shift+v) - Enter TOGGLE visual mode (individual selection with Space)
        keymapHandler.registerSimpleHandler(for: .enterToggleMode) { [self] in
            await MainActor.run {
                guard keymapHandler.engine.visualModeType == .none else { return }
                keymapHandler.engine.enterVisualMode(.toggle)
                // Mark current email initially
                if let currentId = cursorEmailId {
                    markedEmails = [currentId]
                }
                Log.debug("VISUAL", "Entered TOGGLE mode, initial mark: \(cursorEmailId ?? "nil")")
            }
        }

        // Space - Toggle selection (only in TOGGLE mode)
        keymapHandler.registerSimpleHandler(for: .toggleSelection) { [self] in
            await MainActor.run {
                // Only works in Toggle mode
                guard keymapHandler.engine.visualModeType == .toggle else { return }
                guard let currentId = cursorEmailId else { return }

                // Toggle mark on current cursor position
                if markedEmails.contains(currentId) {
                    // Unmark (but keep at least one)
                    if markedEmails.count > 1 {
                        markedEmails.remove(currentId)
                    }
                } else {
                    // Mark
                    markedEmails.insert(currentId)
                }

                // Move cursor to next email (mutt-style)
                if let current = currentEmailIndex(), current < sortedEmailIds.count - 1 {
                    cursorEmailId = sortedEmailIds[current + 1]
                    // Selection stays unchanged (toggle mode)
                }
            }
        }

        // Escape in list context — dispatched by KeySequenceEngine.
        // Popup contexts (.search, .tagPicker) are handled separately via .closePopup;
        // thread context via .closeDetail. This handler covers the remaining list states.
        keymapHandler.registerSimpleHandler(for: .exitVisualMode) { [self] in
            await MainActor.run {
                if visualModeAnchor != nil || keymapHandler.engine.visualModeType != .none {
                    keymapHandler.engine.exitVisualMode()
                    if let cursor = cursorEmailId {
                        markedEmails = [cursor]
                    }
                    visualModeAnchor = nil
                    Log.debug("VISUAL", "Exited visual mode")
                } else if isSearchMode {
                    exitSearchMode()
                } else {
                    detailMode = .empty
                }
            }
        }

        // ═══════════════════════════════════════════════════════════
        // THREAD CONTEXT HANDLERS
        // ═══════════════════════════════════════════════════════════

        // l - Enter thread view
        keymapHandler.registerSimpleHandler(for: .enterThread) { [self] in
            await MainActor.run {
                guard cursorEmailId != nil else { return }
                focusedMessageIndex = 0
                isThreadFocused = true
                keymapHandler.engine.setContext(.thread)
            }
        }

        // j/k in thread - scroll (supports count: 3j = scroll 3x)
        keymapHandler.registerHandler(for: .scrollDown, context: .thread) { count in
            for _ in 0..<count {
                NotificationCenter.default.post(name: .threadScrollDown, object: nil)
            }
        }

        keymapHandler.registerHandler(for: .scrollUp, context: .thread) { count in
            for _ in 0..<count {
                NotificationCenter.default.post(name: .threadScrollUp, object: nil)
            }
        }

        // Ctrl+d/u in thread - half-page scroll
        keymapHandler.registerHandler(for: .pageDown, context: .thread) { count in
            let steps = 5 * count
            for _ in 0..<steps {
                NotificationCenter.default.post(name: .threadScrollDown, object: nil)
            }
        }

        keymapHandler.registerHandler(for: .pageUp, context: .thread) { count in
            let steps = 5 * count
            for _ in 0..<steps {
                NotificationCenter.default.post(name: .threadScrollUp, object: nil)
            }
        }

        // n/N in thread - navigate messages (supports count: 3n = jump 3)
        keymapHandler.registerHandler(for: .nextMessage, context: .thread) { [self] count in
            await MainActor.run {
                let max = currentThreadMessageCount - 1
                focusedMessageIndex = min(focusedMessageIndex + count, max)
            }
        }

        keymapHandler.registerHandler(for: .prevMessage, context: .thread) { [self] count in
            await MainActor.run {
                guard focusedMessageIndex > 0 else { return }
                focusedMessageIndex = max(focusedMessageIndex - count, 0)
            }
        }

        // gg/G in thread - first/last message
        keymapHandler.registerSimpleHandler(for: .firstEmail, context: .thread) { [self] in
            await MainActor.run {
                guard focusedMessageIndex > 0 else { return }
                focusedMessageIndex = 0
            }
        }

        keymapHandler.registerSimpleHandler(for: .lastEmail, context: .thread) { [self] in
            await MainActor.run {
                let last = max(currentThreadMessageCount - 1, 0)
                if focusedMessageIndex == last {
                    NotificationCenter.default.post(name: .threadScrollToBottom, object: nil)
                }
                focusedMessageIndex = last
            }
        }

        // h/Escape in thread - back to list
        keymapHandler.registerSimpleHandler(for: .closeDetail, context: .thread) { [self] in
            await MainActor.run {
                isThreadFocused = false
                keymapHandler.engine.setContext(.list)
            }
        }

        // r in thread - reply
        keymapHandler.registerSimpleHandler(for: .reply, context: .thread) { [self] in
            await MainActor.run {
                replyToSelected()
            }
        }

        keymapHandler.registerSimpleHandler(for: .replyAll, context: .thread) { [self] in
            await MainActor.run { replyAllToSelected() }
        }

        keymapHandler.registerSimpleHandler(for: .forward, context: .thread) { [self] in
            await MainActor.run { forwardSelected() }
        }

        // ═══════════════════════════════════════════════════════════
        // POPUP CONTEXT HANDLERS (search, tag picker)
        // ═══════════════════════════════════════════════════════════

        for ctx in [KeymapContext.search, KeymapContext.tagPicker] {
            keymapHandler.registerSimpleHandler(for: .selectNext, context: ctx) {
                NotificationCenter.default.post(name: .popupSelectNext, object: nil)
            }
            keymapHandler.registerSimpleHandler(for: .selectPrev, context: ctx) {
                NotificationCenter.default.post(name: .popupSelectPrev, object: nil)
            }
            keymapHandler.registerSimpleHandler(for: .closePopup, context: ctx) { [self] in
                await MainActor.run {
                    showSearchPopup = false
                    showTagPicker = false
                    showFolderPicker = false
                    // ESC in search popup should exit search mode entirely —
                    // the user pressed ESC (cancel), not Enter (confirm).
                    // Tag picker ESC should NOT exit search mode (user may
                    // be tagging a search result and wants to return to results).
                    if ctx == .search && isSearchMode {
                        exitSearchMode()
                    }
                }
            }
        }

        Log.debug("KEYMAPS", "All handlers registered")
    }
}
