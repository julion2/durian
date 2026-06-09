@testable import durian_lib
import XCTest

final class EmailSendingManagerTests: XCTestCase {

    // MARK: - stripStyleTags

    func testStripsSingleStyleBlock() {
        let html = """
        <div>Hello</div>
        <style>.bold { font-weight: bold; }</style>
        <p class="bold">World</p>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("<style"), "Style block should be stripped")
        XCTAssertTrue(result.contains("<div>Hello</div>"), "Content before style should be preserved")
        XCTAssertTrue(result.contains("<p class=\"bold\">World</p>"), "Content after style should be preserved")
    }

    func testStripsMultipleStyleBlocks() {
        let html = """
        <style>body { font-weight: bold; }</style>
        <div>Content</div>
        <style>p { font-family: 'Comic Sans MS'; }</style>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("<style"), "All style blocks should be stripped")
        XCTAssertTrue(result.contains("<div>Content</div>"), "Content should be preserved")
    }

    func testStripsStyleWithAttributes() {
        let html = """
        <style type="text/css" id="outlook">
        body { font-size: 20px; }
        </style>
        <p>Hello</p>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("<style"), "Style with attributes should be stripped")
        XCTAssertTrue(result.contains("<p>Hello</p>"))
    }

    func testStripsCaseInsensitive() {
        let html = "<STYLE>body{color:red}</STYLE><p>text</p>"
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.lowercased().contains("<style"), "Uppercase STYLE should be stripped")
        XCTAssertTrue(result.contains("<p>text</p>"))
    }

    func testPreservesInlineStyles() {
        let html = """
        <div style="font-weight: bold; color: red;">Important</div>
        <span style="font-size: 12px;">Small</span>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertEqual(result, html, "Inline styles must not be touched")
    }

    func testNoStyleBlocksPassesThrough() {
        let html = "<div>Hello <b>World</b></div>"
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertEqual(result, html)
    }

    func testStripsMultilineStyleBlock() {
        let html = """
        <style>
            .awl a {color: #FFFFFF; text-decoration: none;}
            .abml a {color: #000000; font-family: Roboto-Medium,Helvetica,Arial,sans-serif; font-weight: bold;}
            @media screen and (min-width: 600px) {
                .email-container { width: 600px !important; }
            }
        </style>
        <div class="email-container">Content</div>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("font-weight: bold"), "CSS rules should be gone")
        XCTAssertTrue(result.contains("Content"))
    }

    func testDoesNotStripWordStyleInContent() {
        let html = "<p>Choose a hairstyle</p><div style=\"color:red\">styled</div>"
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertEqual(result, html, "The word 'style' in content must not be touched")
    }

    func testStripsStyleWithCommentedCloseTag() {
        // Some email generators produce comments inside style blocks.
        // The regex uses non-greedy match, so it stops at the first </style>.
        let html = "<style><!-- </style> body{font-weight:bold}</style><p>text</p>"
        let result = EmailSendingManager.stripStyleTags(html)
        // Non-greedy strips up to first </style>, leaving the remnant.
        // The remnant is harmless plain text, not a valid style block.
        XCTAssertFalse(result.contains("<style"), "Opening style tag should be gone")
        XCTAssertTrue(result.contains("<p>text</p>"))
    }

    func testStripsEmptyStyleBlock() {
        let html = "<style></style><p>content</p>"
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("<style"))
        XCTAssertTrue(result.contains("<p>content</p>"))
    }

    // MARK: - cleanEditorArtifacts

    func testCleansWebKitClasses() {
        let html = #"<p class="isSelectedEnd" style="color: blue;">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("isSelectedEnd"))
        XCTAssertTrue(result.contains("color: blue"), "Intentional styles must survive")
    }

    func testCleansCaretColor() {
        let html = #"<p style="caret-color: rgb(0, 0, 0); font-size: 14px;">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("caret-color"))
        XCTAssertTrue(result.contains("font-size: 14px"))
    }

    func testCleansHardcodedBlack() {
        let html = #"<p style="color: rgb(0, 0, 0);">dark mode safe now</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("color: rgb(0"), "Hardcoded black should be stripped")
    }

    func testPreservesNonBlackColor() {
        let html = #"<p style="color: rgb(255, 0, 0);">red text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertTrue(result.contains("color: rgb(255, 0, 0)"), "Non-black colors must survive")
    }

    func testCleansEmptyStyleAttribute() {
        let html = #"<p class="isSelectedEnd" style="caret-color: rgb(0, 0, 0); color: rgb(0, 0, 0);">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertEqual(result, "<p>text</p>", "All artifacts removed, empty style cleaned up")
    }

    func testCleansPasteInheritedAppleFont() {
        let html = #"<p style="margin: 0; font-family: -apple-system, sans-serif">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("font-family"), "Paste-inherited -apple-system should be stripped")
        XCTAssertTrue(result.contains("margin: 0"), "Other styles must survive")
    }

    func testCleansPasteInheritedAppleFontOnly() {
        let html = #"<ul style="font-family: -apple-system, sans-serif"><li>item</li></ul>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("font-family"))
        // Style attribute should be cleaned up entirely since it's now empty
        XCTAssertFalse(result.contains("style="))
    }

    func testPreservesNonAppleFont() {
        let html = #"<p style="font-family: Georgia, serif">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertTrue(result.contains("font-family: Georgia, serif"), "Non-Apple fonts must survive")
    }

    func testCleansPasteAppleFontBetweenOtherStyles() {
        let html = #"<p style="margin: 0; font-family: -apple-system, BlinkMacSystemFont, sans-serif; color: red">text</p>"#
        let result = EmailSendingManager.cleanEditorArtifacts(html)
        XCTAssertFalse(result.contains("font-family"))
        XCTAssertTrue(result.contains("margin: 0"))
        XCTAssertTrue(result.contains("color: red"))
    }

    func testRealWorldGoogleStyleBlock() {
        // Actual CSS from Google notification emails in the DB
        let html = """
        <style>.awl a {color: #FFFFFF; text-decoration: none;} \
        .abml a {color: #000000; font-family: Roboto-Medium,Helvetica,Arial,sans-serif; font-weight: bold; text-decoration: none;} \
        .adgl a {color: rgba(0, 0, 0, 0.87); text-decoration: none;}</style>\
        <div class="abml"><a href="https://example.com">Link</a></div>
        """
        let result = EmailSendingManager.stripStyleTags(html)
        XCTAssertFalse(result.contains("font-weight: bold"), "Google CSS rules should be stripped")
        XCTAssertTrue(result.contains("Link"), "Content should remain")
    }
}
