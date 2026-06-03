//
//  AttachmentModels.swift
//  Durian
//
//  Models for incoming email attachment support
//

import Foundation

struct IncomingAttachmentMetadata: Identifiable, Codable, Hashable {
    let id: UUID
    let section: String
    let filename: String
    let mimeType: String
    let sizeBytes: Int64
    let disposition: AttachmentDisposition
    let contentId: String?

    init(id: UUID = UUID(), section: String, filename: String, mimeType: String, sizeBytes: Int64, disposition: AttachmentDisposition = .attachment, contentId: String? = nil) {
        self.id = id
        self.section = section
        self.filename = filename
        self.mimeType = mimeType
        self.sizeBytes = sizeBytes
        self.disposition = disposition
        self.contentId = contentId
    }

    var sizeFormatted: String {
        ByteCountFormatter.string(fromByteCount: sizeBytes, countStyle: .file)
    }

    var icon: String {
        if mimeType.hasPrefix("image/") {
            return "photo"
        } else if mimeType.hasPrefix("video/") {
            return "video"
        } else if mimeType.hasPrefix("audio/") {
            return "music.note"
        } else if mimeType.contains("pdf") {
            return "doc.fill"
        } else if mimeType.contains("zip") || mimeType.contains("archive") {
            return "doc.zipper"
        } else if mimeType.contains("word") || mimeType.contains("document") {
            return "doc.text"
        } else if mimeType.contains("excel") || mimeType.contains("spreadsheet") {
            return "tablecells"
        } else if mimeType.contains("powerpoint") || mimeType.contains("presentation") {
            return "rectangle.on.rectangle.angled"
        } else {
            return "doc"
        }
    }

    var isInlineImage: Bool {
        disposition == .inline && mimeType.hasPrefix("image/")
    }
}

enum AttachmentDisposition: String, Codable, Hashable {
    case inline
    case attachment
}

enum AttachmentDownloadState: Codable, Hashable {
    case notDownloaded
    case downloading(progress: Double)
    case downloaded(cachePath: String)
    case failed(error: String)
}

struct CachedAttachment: Codable, Hashable {
    let id: UUID
    let filename: String
    let localPath: URL
    let sizeBytes: Int64
    let cachedAt: Date
    var lastAccessDate: Date
    var accessCount: Int
    let emailUID: UInt32
    var pinned: Bool
}

enum AttachmentError: Error, LocalizedError {
    case failedToExtract
    case networkError
    case cacheError
    case parseError
    case notFound
    case circuitBreakerOpen
    case downloadTimeout
    case corruptedData

    var errorDescription: String? {
        switch self {
        case .failedToExtract:
            return "Failed to extract attachment data"
        case .networkError:
            return "Network error while downloading attachment"
        case .cacheError:
            return "Failed to cache attachment"
        case .parseError:
            return "Failed to parse attachment metadata"
        case .notFound:
            return "Attachment not found on server"
        case .circuitBreakerOpen:
            return "Attachment download temporarily unavailable (circuit breaker open)"
        case .downloadTimeout:
            return "Attachment download timed out"
        case .corruptedData:
            return "Attachment data appears to be corrupted"
        }
    }
}
