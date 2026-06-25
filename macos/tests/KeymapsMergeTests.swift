@testable import durian_lib
import XCTest

@MainActor
final class KeymapsMergeTests: XCTestCase {

    private var manager: KeymapsManager { KeymapsManager.shared }

    // MARK: - No user overrides

    func testEmptyUserKeymapsReturnsAllDefaults() {
        let merged = manager.mergeWithDefaults(userKeymaps: [])

        // Should contain known defaults
        XCTAssertTrue(merged.contains(where: { $0.action == "next_email" && $0.key == "j" }))
        XCTAssertTrue(merged.contains(where: { $0.action == "delete" && $0.key == "dd" }))
        XCTAssertTrue(merged.contains(where: { $0.action == "reply" && $0.key == "r" && $0.context == "thread" }))
    }

    // MARK: - Override existing binding

    func testUserOverridesDefaultByBindingKey() {
        // Default: "a" in list context = archive
        // User remaps "a" in list context to toggle_star
        let user: [KeymapEntry] = [
            .init(action: "toggle_star", key: "a"),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        let aBindings = merged.filter { $0.key == "a" && $0.context == "list" && $0.modifiers.isEmpty }
        XCTAssertEqual(aBindings.count, 1)
        XCTAssertEqual(aBindings.first?.action, "toggle_star")
    }

    func testOverridePreservesPositionInArray() {
        // The overridden entry should stay at the same position as the default
        let user: [KeymapEntry] = [
            .init(action: "toggle_star", key: "j", supportsCount: true),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        // "j" in list context is the first default — should still be near the top
        guard let idx = merged.firstIndex(where: { $0.key == "j" && $0.context == "list" && $0.modifiers.isEmpty }) else {
            XCTFail("Expected j binding in merged result")
            return
        }
        XCTAssertEqual(merged[idx].action, "toggle_star")
        XCTAssertLessThan(idx, 5, "Overridden binding should keep its position near the start")
    }

    // MARK: - Add new binding

    func testUserAddsNewBinding() {
        let user: [KeymapEntry] = [
            .init(action: "tag_op", key: "T", tags: "+todo -inbox"),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        XCTAssertTrue(merged.contains(where: { $0.action == "tag_op" && $0.key == "T" }))
    }

    func testNewBindingsAppendedAfterDefaults() {
        let user: [KeymapEntry] = [
            .init(action: "tag_op", key: "W", tags: "+waiting"),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        guard let wIdx = merged.firstIndex(where: { $0.key == "W" && $0.action == "tag_op" }) else {
            XCTFail("Expected W binding")
            return
        }
        // New entries should be at the end, after all defaults
        guard let lastDefaultIdx = merged.lastIndex(where: { $0.action == "reply" && $0.key == "r" && $0.context == "thread" }) else {
            XCTFail("Expected thread reply default")
            return
        }
        XCTAssertGreaterThan(wIdx, lastDefaultIdx)
    }

    // MARK: - Disable default

    func testEnabledFalseRemovesDefault() {
        // Disable the default "dd" delete binding
        let user: [KeymapEntry] = [
            .init(action: "delete", key: "dd", enabled: false, sequence: true),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        XCTAssertFalse(merged.contains(where: { $0.key == "dd" && $0.context == "list" }))
    }

    func testEnabledFalseDoesNotAffectOtherDefaults() {
        let user: [KeymapEntry] = [
            .init(action: "delete", key: "dd", enabled: false, sequence: true),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        // Other defaults still present
        XCTAssertTrue(merged.contains(where: { $0.action == "next_email" && $0.key == "j" }))
        XCTAssertTrue(merged.contains(where: { $0.action == "archive" && $0.key == "a" }))
    }

    // MARK: - Context isolation

    func testOverrideRespectContext() {
        // Override "r" only in thread context, list "r" (reply) stays untouched
        let user: [KeymapEntry] = [
            .init(action: "forward", key: "r", context: "thread"),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        // Thread: should be overridden to forward
        let threadR = merged.filter { $0.key == "r" && $0.context == "thread" && $0.modifiers.isEmpty }
        XCTAssertEqual(threadR.count, 1)
        XCTAssertEqual(threadR.first?.action, "forward")

        // List: default reply should still be there
        let listR = merged.filter { $0.key == "r" && $0.context == "list" && $0.modifiers.isEmpty }
        XCTAssertTrue(listR.contains(where: { $0.action == "reply" }))
    }

    // MARK: - Modifier isolation

    func testOverrideRespectsModifiers() {
        // Override Cmd+R (reload_inbox) but leave plain "r" (reply) alone
        let user: [KeymapEntry] = [
            .init(action: "compose", key: "r", modifiers: ["cmd"]),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        let cmdR = merged.filter { $0.key == "r" && $0.modifiers == ["cmd"] && $0.context == "list" }
        XCTAssertEqual(cmdR.count, 1)
        XCTAssertEqual(cmdR.first?.action, "compose")

        // Plain "r" in list context should still be reply
        let plainR = merged.filter { $0.key == "r" && $0.modifiers.isEmpty && $0.context == "list" }
        XCTAssertTrue(plainR.contains(where: { $0.action == "reply" }))
    }

    // MARK: - Combined: override + add + disable

    func testMixedUserConfig() {
        let user: [KeymapEntry] = [
            // Override: remap archive to "e"
            .init(action: "archive", key: "a", enabled: false),
            .init(action: "archive", key: "e"),
            // Add: new tag shortcut
            .init(action: "tag_op", key: "T", tags: "+todo -inbox"),
            // Disable: remove visual mode
            .init(action: "enter_visual_mode", key: "v", enabled: false),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        // "a" disabled
        XCTAssertFalse(merged.contains(where: { $0.key == "a" && $0.context == "list" }))
        // "e" added as archive
        XCTAssertTrue(merged.contains(where: { $0.key == "e" && $0.action == "archive" }))
        // "T" tag_op added
        XCTAssertTrue(merged.contains(where: { $0.key == "T" && $0.action == "tag_op" }))
        // "v" disabled
        XCTAssertFalse(merged.contains(where: { $0.key == "v" && $0.action == "enter_visual_mode" }))
        // Other defaults still intact
        XCTAssertTrue(merged.contains(where: { $0.action == "next_email" && $0.key == "j" }))
        XCTAssertTrue(merged.contains(where: { $0.action == "compose" && $0.key == "c" }))
    }

    // MARK: - Duplicate user entries

    func testDuplicateUserEntriesLastWins() {
        let user: [KeymapEntry] = [
            .init(action: "archive", key: "j"),
            .init(action: "compose", key: "j"),
        ]
        let merged = manager.mergeWithDefaults(userKeymaps: user)

        let jBindings = merged.filter { $0.key == "j" && $0.context == "list" && $0.modifiers.isEmpty }
        XCTAssertEqual(jBindings.count, 1)
        XCTAssertEqual(jBindings.first?.action, "compose", "Last user entry should win for same bindingKey")
    }

    // MARK: - bindingKey

    func testBindingKeyFormat() {
        let entry = KeymapEntry(action: "page_down", key: "d", modifiers: ["ctrl"], context: "thread")
        XCTAssertEqual(entry.bindingKey, "d|ctrl|thread")
    }

    func testBindingKeyModifiersSorted() {
        let entry = KeymapEntry(action: "test", key: "x", modifiers: ["shift", "cmd"])
        XCTAssertEqual(entry.bindingKey, "x|cmd+shift|list")
    }

    func testBindingKeyNoModifiers() {
        let entry = KeymapEntry(action: "next_email", key: "j")
        XCTAssertEqual(entry.bindingKey, "j||list")
    }
}
