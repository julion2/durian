@testable import durian_lib
import XCTest

@MainActor
final class BannerManagerTests: XCTestCase {

    /// Create a manager with startup suppression already elapsed
    private func makeManager() -> BannerManager {
        BannerManager(startupTime: Date.distantPast)
    }

    func testShowCriticalSetsCurrentBanner() async {
        let manager = makeManager()
        let banner = BannerMessage(title: "Fail", message: "Something broke", severity: .critical)
        manager.show(banner)

        // show() uses Task with 300ms delay
        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.title, "Fail")
        XCTAssertEqual(manager.currentBanner?.severity, .critical)
    }

    func testShowWarningSetsCurrentBanner() async {
        let manager = makeManager()
        let banner = BannerMessage(title: "Warn", message: "Heads up", severity: .warning)
        manager.show(banner)

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.title, "Warn")
    }

    func testDismissClearsBanner() async {
        let manager = makeManager()
        let banner = BannerMessage(title: "Error", message: "msg", severity: .critical)
        manager.show(banner)

        try? await Task.sleep(nanoseconds: 800_000_000)
        XCTAssertNotNil(manager.currentBanner)

        manager.dismiss()
        XCTAssertNil(manager.currentBanner)
    }

    func testSecondShowReplacesFirst() async {
        let manager = makeManager()
        let first = BannerMessage(title: "First", message: "1", severity: .critical)
        let second = BannerMessage(title: "Second", message: "2", severity: .critical)

        manager.show(first)
        try? await Task.sleep(nanoseconds: 800_000_000)
        manager.show(second)
        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertEqual(manager.currentBanner?.title, "Second")
    }

    func testShowWarningConvenience() async {
        let manager = makeManager()
        manager.showWarning(title: "Net", message: "Offline")

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.severity, .warning)
    }

    func testShowCriticalConvenience() async {
        let manager = makeManager()
        manager.showCritical(title: "Fatal", message: "Crash")

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.severity, .critical)
    }

    func testStartupSuppression() async {
        // Fresh manager — non-critical banners within 4s should be suppressed
        let manager = BannerManager()
        manager.showWarning(title: "Suppressed", message: "Should not appear")

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNil(manager.currentBanner)
    }

    func testShowSuccessSetsCurrentBanner() async {
        let manager = makeManager()
        manager.showSuccess(title: "Done", message: "All good")

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.title, "Done")
        XCTAssertEqual(manager.currentBanner?.severity, .success)
    }

    func testShowInfoSetsCurrentBanner() async {
        let manager = makeManager()
        manager.showInfo(title: "FYI", message: "Just so you know")

        try? await Task.sleep(nanoseconds: 800_000_000)

        XCTAssertNotNil(manager.currentBanner)
        XCTAssertEqual(manager.currentBanner?.title, "FYI")
        XCTAssertEqual(manager.currentBanner?.severity, .info)
    }
}
