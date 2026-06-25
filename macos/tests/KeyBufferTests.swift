@testable import durian_lib
import XCTest

@MainActor
final class KeyBufferTests: XCTestCase {

    // MARK: - Initial State

    func testInitialBufferIsEmpty() {
        let buffer = KeyBuffer()
        XCTAssertTrue(buffer.isEmpty)
        XCTAssertEqual(buffer.count, 0)
        XCTAssertEqual(buffer.asString, "")
        XCTAssertNil(buffer.countPrefix)
        XCTAssertFalse(buffer.isCountOnly)
    }

    // MARK: - Append + Clear

    func testAppendSingleKey() {
        let buffer = KeyBuffer()
        buffer.append(key: "j")
        XCTAssertFalse(buffer.isEmpty)
        XCTAssertEqual(buffer.count, 1)
        XCTAssertEqual(buffer.asString, "j")
    }

    func testAppendMultipleKeys() {
        let buffer = KeyBuffer()
        buffer.append(key: "g")
        buffer.append(key: "g")
        XCTAssertEqual(buffer.count, 2)
        XCTAssertEqual(buffer.asString, "gg")
    }

    func testClearResetsBuffer() {
        let buffer = KeyBuffer()
        buffer.append(key: "5")
        buffer.append(key: "j")
        buffer.clear()
        XCTAssertTrue(buffer.isEmpty)
        XCTAssertEqual(buffer.asString, "")
        XCTAssertEqual(buffer.displayString, "")
    }

    // MARK: - Count Prefix Parsing

    func testNoCountPrefixForPureSequence() {
        let buffer = KeyBuffer()
        buffer.append(key: "g")
        buffer.append(key: "g")
        XCTAssertNil(buffer.countPrefix)
        XCTAssertEqual(buffer.sequenceWithoutCount, "gg")
    }

    func testSingleDigitCountPrefix() {
        let buffer = KeyBuffer()
        buffer.append(key: "5")
        buffer.append(key: "j")
        XCTAssertEqual(buffer.countPrefix, 5)
        XCTAssertEqual(buffer.sequenceWithoutCount, "j")
    }

    func testMultiDigitCountPrefix() {
        let buffer = KeyBuffer()
        for ch in "12" { buffer.append(key: String(ch)) }
        buffer.append(key: "g")
        buffer.append(key: "g")
        XCTAssertEqual(buffer.countPrefix, 12)
        XCTAssertEqual(buffer.sequenceWithoutCount, "gg")
    }

    func testCountPrefixOnlyLeadingDigits() {
        // "5j3k" should be count=5, sequence="j3k" — only leading digits are count
        let buffer = KeyBuffer()
        buffer.append(key: "5")
        buffer.append(key: "j")
        buffer.append(key: "3")
        buffer.append(key: "k")
        XCTAssertEqual(buffer.countPrefix, 5)
        XCTAssertEqual(buffer.sequenceWithoutCount, "j3k")
    }

    // MARK: - isCountOnly

    func testIsCountOnlyWithDigitsOnly() {
        let buffer = KeyBuffer()
        buffer.append(key: "5")
        XCTAssertTrue(buffer.isCountOnly)
    }

    func testIsCountOnlyWithMultipleDigits() {
        let buffer = KeyBuffer()
        buffer.append(key: "1")
        buffer.append(key: "2")
        XCTAssertTrue(buffer.isCountOnly)
    }

    func testIsCountOnlyFalseWhenSequenceFollows() {
        let buffer = KeyBuffer()
        buffer.append(key: "5")
        buffer.append(key: "j")
        XCTAssertFalse(buffer.isCountOnly)
    }

    func testIsCountOnlyFalseForEmptyBuffer() {
        let buffer = KeyBuffer()
        XCTAssertFalse(buffer.isCountOnly)
    }

    // MARK: - Display String

    func testDisplayStringUpdatesOnAppend() {
        let buffer = KeyBuffer()
        XCTAssertEqual(buffer.displayString, "")
        buffer.append(key: "d")
        XCTAssertEqual(buffer.displayString, "d")
        buffer.append(key: "d")
        XCTAssertEqual(buffer.displayString, "dd")
    }

    // MARK: - KeyEvent append (modifier-aware)

    func testAppendKeyEventWithCtrlModifier() {
        let buffer = KeyBuffer()
        let event = KeyEvent(key: "d", modifiers: [.ctrl])
        buffer.append(event)
        XCTAssertEqual(buffer.asString, "ctrl+d")
    }

    func testShiftedLetterBecomesUppercase() {
        let buffer = KeyBuffer()
        let event = KeyEvent(key: "g", modifiers: [.shift])
        buffer.append(event)
        XCTAssertEqual(buffer.asString, "G")
    }
}
