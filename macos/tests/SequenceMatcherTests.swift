@testable import durian_lib
import XCTest

@MainActor
final class SequenceMatcherTests: XCTestCase {

    /// Inject a curated set of test keymaps into the shared KeymapsManager
    /// and trigger a SequenceMatcher reload. Runs before each test so state
    /// is deterministic across tests.
    override func setUp() {
        super.setUp()

        let entries: [KeymapEntry] = [
            // List context
            .init(action: "next_email", key: "j", modifiers: [],
                  sequence: false, supportsCount: true, context: "list"),
            .init(action: "prev_email", key: "k", modifiers: [],
                  sequence: false, supportsCount: true, context: "list"),
            .init(action: "first_email", key: "gg", modifiers: [],
                  sequence: true, supportsCount: false, context: "list"),
            .init(action: "go_inbox", key: "gi", modifiers: [],
                  sequence: true, supportsCount: false, context: "list"),
            .init(action: "delete", key: "dd", modifiers: [],
                  sequence: true, supportsCount: true, context: "list"),
            .init(action: "page_down", key: "d", modifiers: ["ctrl"],
                  sequence: false, supportsCount: true, context: "list"),
            .init(action: "compose", key: "c", modifiers: [],
                  sequence: false, supportsCount: false, context: "list"),
            // Thread context (to verify context isolation)
            .init(action: "reply", key: "r", modifiers: [],
                  sequence: false, supportsCount: false, context: "thread"),
        ]

        KeymapsManager.shared.keymaps = KeymapConfig()
        KeymapsManager.shared.keymaps.keymaps = entries
        SequenceMatcher.shared.reloadFromConfig()
    }

    // MARK: - Exact Match

    func testExactSingleKeyMatch() {
        let result = SequenceMatcher.shared.match(buffer: "j", context: .list)
        XCTAssertEqual(result, .match(action: .nextEmail, count: 1))
    }

    func testExactMultiKeyMatch() {
        let result = SequenceMatcher.shared.match(buffer: "gg", context: .list)
        XCTAssertEqual(result, .match(action: .firstEmail, count: 1))
    }

    func testExactDeleteSequence() {
        let result = SequenceMatcher.shared.match(buffer: "dd", context: .list)
        XCTAssertEqual(result, .match(action: .deleteEmail, count: 1))
    }

    // MARK: - Partial Match

    func testPartialMatchForSharedPrefix() {
        // "g" is a prefix of both "gg" and "gi" — must return partial
        let result = SequenceMatcher.shared.match(buffer: "g", context: .list)
        XCTAssertEqual(result, .partial)
    }

    func testPartialMatchForDd() {
        // "d" alone — but wait, "d" with ctrl is page_down (ctrl+d).
        // Plain "d" is only a prefix of "dd" → partial.
        let result = SequenceMatcher.shared.match(buffer: "d", context: .list)
        XCTAssertEqual(result, .partial)
    }

    // MARK: - No Match

    func testEmptyBufferIsNoMatch() {
        let result = SequenceMatcher.shared.match(buffer: "", context: .list)
        XCTAssertEqual(result, .noMatch)
    }

    func testUnknownSequenceIsNoMatch() {
        let result = SequenceMatcher.shared.match(buffer: "xyz", context: .list)
        XCTAssertEqual(result, .noMatch)
    }

    // MARK: - Count Prefix

    func testSingleDigitCountWithAction() {
        let result = SequenceMatcher.shared.match(buffer: "5j", context: .list)
        XCTAssertEqual(result, .match(action: .nextEmail, count: 5))
    }

    func testMultiDigitCount() {
        let result = SequenceMatcher.shared.match(buffer: "12j", context: .list)
        XCTAssertEqual(result, .match(action: .nextEmail, count: 12))
    }

    func testCountWithMultiKeySequence() {
        let result = SequenceMatcher.shared.match(buffer: "3dd", context: .list)
        XCTAssertEqual(result, .match(action: .deleteEmail, count: 3))
    }

    func testCountOnlyIsPartial() {
        // Pure digits → waiting for action
        let result = SequenceMatcher.shared.match(buffer: "5", context: .list)
        XCTAssertEqual(result, .partial)
    }

    func testCountIgnoredWhenActionDoesNotSupportIt() {
        // `compose` has supportsCount=false → count is forced to 1
        let result = SequenceMatcher.shared.match(buffer: "5c", context: .list)
        XCTAssertEqual(result, .match(action: .compose, count: 1))
    }

    func testCountIgnoredForFirstEmail() {
        // `first_email` (gg) has supportsCount=false
        let result = SequenceMatcher.shared.match(buffer: "5gg", context: .list)
        XCTAssertEqual(result, .match(action: .firstEmail, count: 1))
    }

    // MARK: - Ctrl Modifier Matching

    func testCtrlModifierMatch() {
        // "ctrl+d" is normalized form for Ctrl+d → page_down
        let result = SequenceMatcher.shared.match(buffer: "ctrl+d", context: .list)
        XCTAssertEqual(result, .match(action: .pageDown, count: 1))
    }

    func testCtrlWithCount() {
        let result = SequenceMatcher.shared.match(buffer: "3ctrl+d", context: .list)
        XCTAssertEqual(result, .match(action: .pageDown, count: 3))
    }

    // MARK: - Context Isolation

    func testContextSpecificMatch() {
        // "r" is only defined in .thread context
        let result = SequenceMatcher.shared.match(buffer: "r", context: .thread)
        XCTAssertEqual(result, .match(action: .reply, count: 1))
    }

    func testContextDoesNotLeakIntoList() {
        // "r" is NOT defined in .list context
        let result = SequenceMatcher.shared.match(buffer: "r", context: .list)
        XCTAssertEqual(result, .noMatch)
    }

    // MARK: - supportsCount API

    func testSupportsCountReturnsTrueForNextEmail() {
        XCTAssertTrue(SequenceMatcher.shared.supportsCount(.nextEmail, context: .list))
    }

    func testSupportsCountReturnsFalseForCompose() {
        XCTAssertFalse(SequenceMatcher.shared.supportsCount(.compose, context: .list))
    }
}
