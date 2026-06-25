@testable import durian_lib
import XCTest

@MainActor
final class ContactsManagerTests: XCTestCase {

    // MARK: - Contact Model

    func testDisplayStringWithName() {
        let c = Contact(id: "1", email: "alice@example.com", name: "Alice Smith",
                        usageCount: 5, source: "imported", createdAt: Date())
        XCTAssertEqual(c.displayString, "Alice Smith <alice@example.com>")
    }

    func testDisplayStringWithoutName() {
        let c = Contact(id: "1", email: "alice@example.com", name: nil,
                        usageCount: 0, source: "imported", createdAt: Date())
        XCTAssertEqual(c.displayString, "alice@example.com")
    }

    func testDisplayStringWithEmptyName() {
        let c = Contact(id: "1", email: "alice@example.com", name: "",
                        usageCount: 0, source: "imported", createdAt: Date())
        XCTAssertEqual(c.displayString, "alice@example.com")
    }

    func testDisplayNameWithName() {
        let c = Contact(id: "1", email: "alice@example.com", name: "Alice",
                        usageCount: 0, source: "imported", createdAt: Date())
        XCTAssertEqual(c.displayName, "Alice")
    }

    func testDisplayNameFallsBackToEmail() {
        let c = Contact(id: "1", email: "alice@example.com", name: nil,
                        usageCount: 0, source: "imported", createdAt: Date())
        XCTAssertEqual(c.displayName, "alice@example.com")
    }

    // MARK: - Contact from API Response

    func testContactFromResponse() {
        let response = ContactResponse(
            id: "abc", email: "bob@example.com", name: "Bob",
            last_used: "2026-04-01T12:00:00Z",
            usage_count: 3, source: "sent",
            created_at: "2026-01-01T00:00:00Z"
        )
        let contact = Contact(from: response)

        XCTAssertEqual(contact.id, "abc")
        XCTAssertEqual(contact.email, "bob@example.com")
        XCTAssertEqual(contact.name, "Bob")
        XCTAssertEqual(contact.usageCount, 3)
        XCTAssertEqual(contact.source, "sent")
        XCTAssertNotNil(contact.lastUsed)
        XCTAssertNotNil(contact.createdAt)
    }

    func testContactFromResponseNilLastUsed() {
        let response = ContactResponse(
            id: "abc", email: "bob@example.com", name: nil,
            last_used: nil,
            usage_count: 0, source: "imported",
            created_at: "2026-01-01T00:00:00Z"
        )
        let contact = Contact(from: response)

        XCTAssertNil(contact.lastUsed)
        XCTAssertNotNil(contact.createdAt)
    }

    func testContactFromResponseSQLiteDate() {
        let response = ContactResponse(
            id: "abc", email: "bob@example.com", name: "Bob",
            last_used: "2026-04-01 12:00:00",
            usage_count: 1, source: "imported",
            created_at: "2026-01-01 00:00:00"
        )
        let contact = Contact(from: response)
        XCTAssertNotNil(contact.lastUsed, "Should parse SQLite date format")
    }

    func testContactFromResponseISO8601WithFractions() {
        let response = ContactResponse(
            id: "abc", email: "bob@example.com", name: "Bob",
            last_used: "2026-04-01T12:00:00.123Z",
            usage_count: 0, source: "imported",
            created_at: "2026-01-01T00:00:00.000Z"
        )
        let contact = Contact(from: response)
        XCTAssertNotNil(contact.lastUsed, "Should parse ISO8601 with fractional seconds")
    }

    // MARK: - Contact Hashable/Identifiable

    func testContactEquality() {
        let c1 = Contact(id: "1", email: "a@b.com", name: "A", usageCount: 0, source: "imported", createdAt: Date())
        let c2 = Contact(id: "1", email: "a@b.com", name: "A", usageCount: 0, source: "imported", createdAt: c1.createdAt)
        XCTAssertEqual(c1, c2)
    }
}
