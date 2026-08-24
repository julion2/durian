@testable import durian_lib
import XCTest

final class ServerHandshakeTests: XCTestCase {
    // MARK: - READY handshake parsing

    func testMatchingAPIProtocolReturnsToken() throws {
        let line = "READY token=secret addr=127.0.0.1:9723 api=\(AppServer.apiProtocol)"
        XCTAssertEqual(try EmailBackend.parseReadyLine(line), "secret")
    }

    func testNoAuthHandshakeReturnsEmptyToken() throws {
        let line = "READY token= addr=127.0.0.1:9723 api=\(AppServer.apiProtocol)"
        XCTAssertEqual(try EmailBackend.parseReadyLine(line), "")
    }

    func testLegacyCLIIsRejected() {
        XCTAssertThrowsError(try EmailBackend.parseReadyLine("READY token=old addr=127.0.0.1:9723")) { error in
            XCTAssertTrue(error.localizedDescription.contains("Incompatible durian CLI API"))
            XCTAssertTrue(error.localizedDescription.contains("legacy"))
        }
    }

    func testDifferentAPIProtocolIsRejected() {
        XCTAssertThrowsError(try EmailBackend.parseReadyLine("READY token=old addr=127.0.0.1:9723 api=999")) { error in
            XCTAssertTrue(error.localizedDescription.contains("found 999"))
            XCTAssertTrue(error.localizedDescription.contains("Update the Durian app"))
        }
    }

    func testOlderAPIProtocolRequestsCLIUpdate() {
        XCTAssertThrowsError(try EmailBackend.parseReadyLine("READY token=old addr=127.0.0.1:9723 api=0")) { error in
            XCTAssertTrue(error.localizedDescription.contains("Update the durian CLI"))
        }
    }

    func testUnrelatedOutputIsIgnored() throws {
        XCTAssertNil(try EmailBackend.parseReadyLine("starting server"))
    }
}
