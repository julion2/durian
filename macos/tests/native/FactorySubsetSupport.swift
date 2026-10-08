import Combine
import Foundation

struct AttachmentCacheSettings: Sendable {
    var maxSizeMB = 200
    var ttlDays = 7
    var maxSizeBytes: Int64 { Int64(maxSizeMB) * 1_048_576 }
    var ttl: TimeInterval { TimeInterval(ttlDays) * 86_400 }
}

@MainActor
final class SettingsManager {
    static let shared = SettingsManager()
    var attachmentCacheSettings = AttachmentCacheSettings()
}

// EmailDraftFactory's attachment-forwarding API references these production
// collaborators. Factory composition tests do not call that API, so the native
// subset supplies only its compile-time surface and links no app/UI code.
@MainActor
final class EmailBackend {
    func downloadAttachment(messageId _: String, partId _: Int) async throws -> (Data, String) {
        fatalError("attachment download is outside the factory subset")
    }
}

enum Log {
    static func debug(_: String, _: String) {}
    static func info(_: String, _: String) {}
    static func warning(_: String, _: String) {}
    static func error(_: String, _: String) {}
}
