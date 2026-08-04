@testable import durian_lib
import WebKit
import XCTest

/// Tests for the shared WebKit process pools.
///
/// The whole point of `SharedWebKit` is that state is shared *by identity*:
/// N email cards must hand WebKit the same `WKProcessPool` object, or they
/// each get their own WebContent process and the pooling does nothing. Value
/// equality would not catch a regression here — two fresh `WKProcessPool()`
/// instances compare unequal but also *are* unequal, so every assertion below
/// is deliberately an identity check.
final class SharedWebKitTests: XCTestCase {

    // MARK: - Pool identity

    /// Every read-only card must land in the same pool, or there is no pooling.
    func testReadOnlyPoolIsStable() {
        XCTAssertTrue(SharedWebKit.readOnlyPool === SharedWebKit.readOnlyPool)
    }

    /// Repeated config builds must not mint a new pool each time — that was
    /// the bug this type exists to prevent.
    func testEveryReadOnlyConfigSharesOnePool() {
        let configs = (0 ..< 8).map { _ in SharedWebKit.makeReadOnlyConfig() }
        for config in configs {
            XCTAssertTrue(config.processPool === SharedWebKit.readOnlyPool)
        }
    }

    /// The compose editor is deliberately isolated: a stale or crashed editor
    /// renderer must not be able to take the reader fleet down with it.
    func testComposePoolIsSeparateFromReadOnlyPool() {
        XCTAssertFalse(SharedWebKit.composePool === SharedWebKit.readOnlyPool)
    }

    func testComposePoolIsStable() {
        XCTAssertTrue(SharedWebKit.composePool === SharedWebKit.composePool)
    }

    // MARK: - Data store

    /// Nothing may be written to disk: no cookie or cache artefact of a
    /// tracking pixel is allowed to outlive the process.
    func testReadOnlyDataStoreIsNonPersistent() {
        XCTAssertFalse(SharedWebKit.readOnlyDataStore.isPersistent)
    }

    /// Shared by identity, so the same remote image across N cards is fetched
    /// once rather than N times.
    func testEveryReadOnlyConfigSharesOneDataStore() {
        let a = SharedWebKit.makeReadOnlyConfig()
        let b = SharedWebKit.makeReadOnlyConfig()
        XCTAssertTrue(a.websiteDataStore === b.websiteDataStore)
        XCTAssertTrue(a.websiteDataStore === SharedWebKit.readOnlyDataStore)
    }

    // MARK: - Config object freshness

    /// The pool and data store are shared; the configuration object itself is
    /// not. Handing the same `WKWebViewConfiguration` to two WebViews is
    /// undefined behaviour in WebKit — each call must return a fresh one.
    func testConfigObjectIsFreshPerCall() {
        XCTAssertFalse(SharedWebKit.makeReadOnlyConfig() === SharedWebKit.makeReadOnlyConfig())
    }

    // MARK: - Read-only preferences

    /// JavaScript stays on for one-shot height measurement; the CSP in
    /// `buildSecureHTML` (`script-src 'none'`) is what actually blocks content
    /// scripts.
    func testReadOnlyConfigAllowsJavaScriptForHeightMeasurement() {
        XCTAssertTrue(SharedWebKit.makeReadOnlyConfig().defaultWebpagePreferences.allowsContentJavaScript)
    }

    /// A mail must never be able to open a window by itself.
    func testReadOnlyConfigForbidsAutomaticWindowOpening() {
        XCTAssertFalse(SharedWebKit.makeReadOnlyConfig().preferences.javaScriptCanOpenWindowsAutomatically)
    }

    // MARK: - No private API

    /// The PR body says occlusion throttling has no public macOS API and is
    /// deferred; this pins the code to that claim. A KVC probe for
    /// `suspendsActivityWhenWindowIsOccluded` would be a silent no-op at best
    /// and an App Store problem at worst.
    func testNoOcclusionThrottlingProbe() {
        let webView = WKWebView(frame: .zero, configuration: SharedWebKit.makeReadOnlyConfig())
        XCTAssertFalse(webView.responds(to: NSSelectorFromString("setSuspendsActivityWhenWindowIsOccluded:")),
                       "If macOS ever ships this publicly, wire it up deliberately rather than by KVC probe.")
    }
}
