import AppKit
@testable import durian_lib
import XCTest

/// End-to-end tests for KeySequenceEngine.handleKeyEvent using synthesized
/// NSEvents: Escape must dispatch the active context's own binding before
/// falling back, and Tab/Return must be matchable under their binding names
/// while passing through when unbound.
@MainActor
final class KeySequenceEngineTests: XCTestCase {

    private let engine = KeySequenceEngine.shared

    override func setUp() {
        super.setUp()

        let entries: [KeymapEntry] = [
            .init(action: "next_email", key: "j", context: "list"),
            .init(action: "exit_visual_mode", key: "Escape", context: "list"),
            .init(action: "enter_thread", key: "Enter", context: "list"),
            .init(action: "close_detail", key: "Escape", context: "calendar"),
            .init(action: "calendar_command", key: ":", context: "calendar"),
        ]
        KeymapsManager.shared.keymaps = KeymapConfig()
        KeymapsManager.shared.keymaps.keymaps = entries
        SequenceMatcher.shared.reloadFromConfig()

        engine.clearBuffer()
        engine.setContext(.list)
    }

    // MARK: - Helpers

    private func keyDown(_ characters: String, keyCode: UInt16,
                         flags: NSEvent.ModifierFlags = []) -> NSEvent
    {
        NSEvent.keyEvent(
            with: .keyDown,
            location: .zero,
            modifierFlags: flags,
            timestamp: 0,
            windowNumber: 0,
            context: nil,
            characters: characters,
            charactersIgnoringModifiers: characters,
            isARepeat: false,
            keyCode: keyCode
        )!
    }

    private var escapeEvent: NSEvent { keyDown("\u{1B}", keyCode: 53) }

    // MARK: - Escape Dispatch

    func testEscapeDispatchesContextBinding() async {
        engine.setContext(.calendar)

        let contextHandler = expectation(description: "calendar close_detail dispatched")
        let fallback = expectation(description: "list fallback must not fire")
        fallback.isInverted = true
        engine.registerHandler(for: .closeDetail, context: .calendar) { _ in
            contextHandler.fulfill()
        }
        engine.registerHandler(for: .exitVisualMode, context: .list) { _ in
            fallback.fulfill()
        }

        XCTAssertTrue(engine.handleKeyEvent(escapeEvent))
        await fulfillment(of: [contextHandler, fallback], timeout: 1)
    }

    func testEscapeFallsBackWithoutContextBinding() async {
        // Thread context has no Escape binding in this config
        engine.setContext(.thread)

        let fallback = expectation(description: "list exit handler dispatched")
        engine.registerHandler(for: .exitVisualMode, context: .list) { _ in
            fallback.fulfill()
        }

        XCTAssertTrue(engine.handleKeyEvent(escapeEvent))
        await fulfillment(of: [fallback], timeout: 1)
    }

    func testEscapeInSearchStillClosesPopup() async {
        engine.setContext(.search)

        let closePopup = expectation(description: "close_popup dispatched")
        engine.registerHandler(for: .closePopup, context: .search) { _ in
            closePopup.fulfill()
        }

        XCTAssertTrue(engine.handleKeyEvent(escapeEvent))
        await fulfillment(of: [closePopup], timeout: 1)
    }

    // MARK: - Shifted Punctuation

    func testShiftedPunctuationDispatchesBinding() async {
        engine.setContext(.calendar)

        let command = expectation(description: "calendar_command dispatched")
        engine.registerHandler(for: .calendarCommand, context: .calendar) { _ in
            command.fulfill()
        }

        // The event's character already carries the shifted glyph; the
        // keyCode is the German period key but any layout behaves the same.
        let event = keyDown(":", keyCode: 47, flags: .shift)
        XCTAssertTrue(engine.handleKeyEvent(event))
        await fulfillment(of: [command], timeout: 1)
    }

    // MARK: - Tab / Return

    func testBoundReturnDispatchesUnderEnterName() async {
        let enterThread = expectation(description: "enter_thread dispatched")
        engine.registerHandler(for: .enterThread, context: .list) { _ in
            enterThread.fulfill()
        }

        XCTAssertTrue(engine.handleKeyEvent(keyDown("\r", keyCode: 36)))
        await fulfillment(of: [enterThread], timeout: 1)
    }

    func testUnboundTabPassesThrough() {
        engine.setContext(.calendar)
        XCTAssertFalse(engine.handleKeyEvent(keyDown("\t", keyCode: 48)))
    }

    func testUnboundReturnPassesThrough() {
        engine.setContext(.calendar)
        XCTAssertFalse(engine.handleKeyEvent(keyDown("\r", keyCode: 36)))
    }
}
