import Combine
import Foundation

struct AttachmentCacheSettings {
    var maxSizeMB = 200
    var ttlDays = 7
    var maxSizeBytes: Int64 { Int64(maxSizeMB) * 1_048_576 }
    var ttl: TimeInterval { TimeInterval(ttlDays) * 86_400 }
}

final class SettingsManager {
    static let shared = SettingsManager()
    var attachmentCacheSettings = AttachmentCacheSettings()
}

final class EmailBackend {
    func downloadAttachment(messageId _: String, partId _: Int) async throws -> (Data, String) {
        fatalError("network access is outside the cache harness")
    }
}

enum Log {
    static func debug(_: String, _: String) {}
    static func info(_: String, _: String) {}
    static func warning(_: String, _: String) {}
    static func error(_: String, _: String) {}
}

@main
enum AttachmentCacheHarness {
    @MainActor
    static func main() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("stability-native-cache-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: root) }

        var settings = AttachmentCacheSettings()
        settings.maxSizeMB = 200
        let manager = AttachmentCacheManager(cacheDir: root, settingsProvider: { settings })

        for sizeMB in [1, 10, 25, 50] {
            let payload = Data(repeating: 0xAB, count: sizeMB * 1_048_576)
            var mainActorTicks = 0
            let heartbeat = Task { @MainActor in
                while !Task.isCancelled {
                    try? await Task.sleep(for: .milliseconds(1))
                    if !Task.isCancelled { mainActorTicks += 1 }
                }
            }
            let putStart = ContinuousClock.now
            await manager.put(messageId: "message-\(sizeMB)", partId: 1, filename: "fixture.bin", data: payload)
            let putDuration = putStart.duration(to: .now)
            heartbeat.cancel()
            if sizeMB == 50 && mainActorTicks == 0 {
                throw AttachmentError.corruptedData
            }

            let getStart = ContinuousClock.now
            let fetched = await manager.get(messageId: "message-\(sizeMB)", partId: 1)
            let getDuration = getStart.duration(to: .now)
            guard fetched == payload else { throw AttachmentError.corruptedData }

            print("size_mb=\(sizeMB) put=\(putDuration) get=\(getDuration) bytes=\(fetched?.count ?? -1) main_actor_ticks_during_put=\(mainActorTicks)")
        }
    }
}
