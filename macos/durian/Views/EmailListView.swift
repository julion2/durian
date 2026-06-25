import SwiftUI

enum DateGrouping: Hashable, Comparable {
    case today, yesterday, thisWeek, lastWeek
    case month(year: Int, month: Int)

    var displayName: String {
        switch self {
        case .today: return "Today"
        case .yesterday: return "Yesterday"
        case .thisWeek: return "This Week"
        case .lastWeek: return "Last Week"
        case .month(let year, let month):
            let formatter = DateFormatter()
            formatter.dateFormat = "MMMM yyyy"
            var components = DateComponents()
            components.year = year
            components.month = month
            components.day = 1
            if let date = Calendar.current.date(from: components) {
                return formatter.string(from: date)
            }
            return "\(month)/\(year)"
        }
    }

    var sortOrder: Int {
        switch self {
        case .today: return 0
        case .yesterday: return 1
        case .thisWeek: return 2
        case .lastWeek: return 3
        case .month(let year, let month):
            // Must be > 3 (after lastWeek), newer months = smaller value
            return 100 + (2100 - year) * 12 + (12 - month)
        }
    }

    static func < (lhs: DateGrouping, rhs: DateGrouping) -> Bool {
        lhs.sortOrder < rhs.sortOrder
    }
}

struct EmailListView: View {
    let emails: [MailMessage]
    let emailListGeneration: Int             // Incremented only on data change, not cursor
    @Binding var cursorId: String?           // Current cursor position (highlighted)
    @Binding var selection: Set<String>      // Marked emails (for batch operations)
    let onEmailAppear: (MailMessage) -> Void

    // Context menu callbacks
    var onTogglePin: ((String) -> Void)?
    var onToggleRead: ((String) -> Void)?
    var onDelete: ((String) -> Void)?
    var onReply: ((String) -> Void)?
    var onForward: ((String) -> Void)?
    var onShowTagPicker: ((String) -> Void)?

    @State private var cachedItems: [ListItem] = []

    var body: some View {
        let items = cachedItems.isEmpty ? buildEmailList() : cachedItems
        let groupPositions = computeGroupPositions(from: items)

        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: 0) {
                    ForEach(items) { item in
                        listItemView(item, groupPositions: groupPositions)
                    }
                }
            }
            .overlayScrollbars()
            .onChange(of: cursorId) { _, newCursorId in
                if let cursorId = newCursorId {
                    proxy.scrollTo(cursorId, anchor: .center)
                }
            }
            .onChange(of: emailListGeneration) { _, _ in
                cachedItems = buildEmailList()
            }
            .onChange(of: emails) { _, _ in
                cachedItems = buildEmailList()
            }
            .onAppear {
                if cachedItems.isEmpty { cachedItems = buildEmailList() }
            }
        }
    }

    @ViewBuilder
    private func listItemView(_ item: ListItem, groupPositions: [String: (isFirst: Bool, isLast: Bool)]) -> some View {
        switch item {
        case .header(let title, let style):
            headerView(title: title, style: style)
        case .email(let email):
            emailRow(email: email, groupPositions: groupPositions)
        }
    }

    @ViewBuilder
    private func headerView(title: String, style: HeaderStyle) -> some View {
        Text(title)
            .font(.caption)
            .fontWeight(style == .pinned ? .semibold : .medium)
            .foregroundStyle(style == .pinned ? Color.yellow : Color.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.top, 12)
            .padding(.bottom, 4)
            .padding(.horizontal, 12)
    }

    // MARK: - List Building

    private enum HeaderStyle {
        case pinned, date
    }

    private enum ListItem: Identifiable {
        case header(String, HeaderStyle)
        case email(MailMessage)

        var id: String {
            switch self {
            case .header(let title, _): return "header-\(title)"
            case .email(let email): return email.id
            }
        }
    }

    private func buildEmailList() -> [ListItem] {
        var items: [ListItem] = []

        // Pinned emails first
        let pinnedEmails = emails.filter { $0.isPinned }.sorted { $0.timestamp > $1.timestamp }
        if !pinnedEmails.isEmpty {
            items.append(.header("Pinned", .pinned))
            items.append(contentsOf: pinnedEmails.map { .email($0) })
        }

        // Then unpinned emails grouped by date
        let unpinnedEmails = emails.filter { !$0.isPinned }
        let grouped = groupEmails(unpinnedEmails)

        for (group, groupEmails) in grouped {
            items.append(.header(group.displayName, .date))
            items.append(contentsOf: groupEmails.map { .email($0) })
        }

        return items
    }

    /// Pre-computed selection group positions — O(n) instead of O(n²)
    /// Takes pre-built items to avoid double buildEmailList() call
    private func computeGroupPositions(from items: [ListItem]) -> [String: (isFirst: Bool, isLast: Bool)] {
        let emailIds = items.compactMap { item -> String? in
            if case .email(let email) = item { return email.id }
            return nil
        }

        var result: [String: (isFirst: Bool, isLast: Bool)] = [:]
        for (i, id) in emailIds.enumerated() {
            let isSelected = selection.contains(id) || cursorId == id
            guard isSelected else {
                result[id] = (true, true)
                continue
            }
            let prevId = i > 0 ? emailIds[i - 1] : nil
            let prevSelected = prevId != nil && (selection.contains(prevId!) || cursorId == prevId)
            let nextId = i < emailIds.count - 1 ? emailIds[i + 1] : nil
            let nextSelected = nextId != nil && (selection.contains(nextId!) || cursorId == nextId)
            result[id] = (isFirst: !prevSelected, isLast: !nextSelected)
        }
        return result
    }

    @ViewBuilder
    private func emailRow(email: MailMessage, groupPositions: [String: (isFirst: Bool, isLast: Bool)]) -> some View {
        // Cursor position gets highlight, marked emails show selection indicator
        let isCursor = cursorId == email.id
        let isMarked = selection.contains(email.id)
        let isSelected = isCursor || isMarked

        // Pre-computed position in selection group for corner radius
        let pos = groupPositions[email.id] ?? (true, true)
        let (isFirstInGroup, isLastInGroup) = pos

        EquatableView(content: EmailRowView(
            email: email,
            isSelected: isSelected,
            isFirstInGroup: isFirstInGroup,
            isLastInGroup: isLastInGroup,
            onTogglePin: {
                cursorId = email.id
                selection = [email.id]
                onTogglePin?(email.id)
            },
            onToggleRead: {
                cursorId = email.id
                selection = [email.id]
                onToggleRead?(email.id)
            },
            onDelete: {
                cursorId = email.id
                selection = [email.id]
                onDelete?(email.id)
            },
            onReply: {
                cursorId = email.id
                selection = [email.id]
                onReply?(email.id)
            },
            onForward: {
                cursorId = email.id
                selection = [email.id]
                onForward?(email.id)
            },
            onShowTagPicker: {
                cursorId = email.id
                selection = [email.id]
                onShowTagPicker?(email.id)
            }
        ))
        .id(email.id)
        .contentShape(Rectangle())
        .onTapGesture {
            // Click sets both cursor and selection
            cursorId = email.id
            selection = [email.id]
        }
        .onAppear {
            onEmailAppear(email)
        }
    }

    private func groupEmails(_ emails: [MailMessage]) -> [(DateGrouping, [MailMessage])] {
        let calendar = Calendar.current
        let now = Date()
        let todayStart = calendar.startOfDay(for: now)
        let yesterdayStart = calendar.date(byAdding: .day, value: -1, to: todayStart)!
        let thisWeekStart = calendar.date(from: calendar.dateComponents([.yearForWeekOfYear, .weekOfYear], from: now))!
        let lastWeekStart = calendar.date(byAdding: .weekOfYear, value: -1, to: thisWeekStart)!

        var groups: [DateGrouping: [MailMessage]] = [:]
        for email in emails {
            let emailDate = Date(timeIntervalSince1970: TimeInterval(email.timestamp))
            let group = categorizeDate(emailDate, todayStart: todayStart, yesterdayStart: yesterdayStart, thisWeekStart: thisWeekStart, lastWeekStart: lastWeekStart, calendar: calendar)
            if groups[group] == nil { groups[group] = [] }
            groups[group]?.append(email)
        }
        // Sort within each group by timestamp (newest first)
        return groups.map { ($0.key, $0.value.sorted { $0.timestamp > $1.timestamp }) }
            .sorted { $0.0 < $1.0 }
    }

    private func categorizeDate(_ date: Date, todayStart: Date, yesterdayStart: Date, thisWeekStart: Date, lastWeekStart: Date, calendar: Calendar) -> DateGrouping {
        if date >= todayStart { return .today }
        else if date >= yesterdayStart { return .yesterday }
        else if date >= thisWeekStart { return .thisWeek }
        else if date >= lastWeekStart { return .lastWeek }
        else {
            let c = calendar.dateComponents([.year, .month], from: date)
            return .month(year: c.year ?? 2024, month: c.month ?? 1)
        }
    }
}
