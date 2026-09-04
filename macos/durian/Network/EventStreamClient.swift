//
//  EventStreamClient.swift
//  Durian
//
//  Lightweight SSE client for real-time new-mail events from durian serve.
//

import Foundation

// MARK: - SSE Event Models

struct NewMailEvent: Decodable {
    let account: String
    let total_new: Int
    let messages: [NewMailMessage]
}

struct NewMailMessage: Decodable {
    let thread_id: String
    let subject: String
    let from: String
    let snippet: String
}

struct OutboxUpdateEvent: Decodable {
    let item_id: Int64
    let status: String   // "sent", "failed", "queued", or a reconciliation status
    let error: String?
    let subject: String?
    let to: String?
}

/// Emitted by the serve calendar autosync loop after a download-only run that
/// changed the local vdir, so the GUI can refetch instead of showing stale data.
struct CalendarUpdatedEvent: Decodable {
    let account: String
    let downloaded: Int
    let pruned: Int
}

// MARK: - Event Stream Client

@MainActor
class EventStreamClient: ObservableObject {
    @Published var isConnected = false

    private var streamTask: Task<Void, Never>?
    private let eventsURL = AppServer.eventsURL
    private let reconnectDelay: UInt64 = 5_000_000_000 // 5 seconds

    var onNewMail: ((NewMailEvent) -> Void)?
    var onOutboxUpdate: ((OutboxUpdateEvent) -> Void)?
    var onCalendarUpdated: ((CalendarUpdatedEvent) -> Void)?

    // MARK: - Connection

    func connect() {
        guard streamTask == nil else { return }
        Log.debug("EVENTS", "Starting SSE connection...")
        streamTask = Task { await streamLoop() }
    }

    func disconnect() {
        Log.debug("EVENTS", "Disconnecting SSE")
        streamTask?.cancel()
        streamTask = nil
        isConnected = false
    }

    // MARK: - Stream Loop (reconnects on error)

    private func streamLoop() async {
        while !Task.isCancelled {
            guard NetworkMonitor.shared.isConnected else {
                Log.debug("EVENTS", "Offline, waiting to retry...")
                isConnected = false
                try? await Task.sleep(nanoseconds: reconnectDelay)
                continue
            }

            do {
                try await readStream()
            } catch is CancellationError {
                break
            } catch {
                Log.error("EVENTS", "Stream error: \(error.localizedDescription)")
            }

            isConnected = false
            guard !Task.isCancelled else { break }
            Log.warning("EVENTS", "Reconnecting in 5s...")
            try? await Task.sleep(nanoseconds: reconnectDelay)
        }
        isConnected = false
    }

    // MARK: - SSE Parser

    private func readStream() async throws {
        var request = URLRequest(url: eventsURL)
        if let token = EmailBackend.authToken {
            request.addValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        let (bytes, response) = try await URLSession.shared.bytes(for: request)

        guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
            Log.warning("EVENTS", "Server returned non-200, will retry")
            return
        }

        isConnected = true
        Log.info("EVENTS", "Connected to SSE stream")

        var currentEvent: String?
        var dataBuffer: String = ""

        for try await line in bytes.lines {
            if Task.isCancelled { break }

            // Heartbeat/comment or empty line — flush any buffered event first
            if line.hasPrefix(":") || line.isEmpty {
                if let eventType = currentEvent, !dataBuffer.isEmpty {
                    handleSSEEvent(type: eventType, data: dataBuffer)
                    dataBuffer = ""
                    currentEvent = nil
                }
                continue
            }

            // Parse SSE fields
            if line.hasPrefix("event:") {
                // New event starting — flush any buffered event first
                // (bytes.lines drops empty lines, so we can't rely on \n\n delimiter)
                if let eventType = currentEvent, !dataBuffer.isEmpty {
                    handleSSEEvent(type: eventType, data: dataBuffer)
                    dataBuffer = ""
                }
                currentEvent = line.dropFirst(6).trimmingCharacters(in: .whitespaces)
            } else if line.hasPrefix("data:") {
                let value = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
                if dataBuffer.isEmpty {
                    dataBuffer = value
                } else {
                    dataBuffer += "\n" + value
                }
            }
        }

        // Flush last buffered event when stream ends
        if let eventType = currentEvent, !dataBuffer.isEmpty {
            handleSSEEvent(type: eventType, data: dataBuffer)
        }
    }

    // MARK: - Event Dispatch

    private func handleSSEEvent(type: String, data: String) {
        // Never log the payload. A new_mail frame carries subject, from and
        // snippet, an outbox_update carries the recipients — dumping it here
        // put exactly the content the redacted logs below avoid straight into
        // the unified log. The grep gate cannot catch this line because the
        // sensitive words live in the server's JSON, not in the source text.
        Log.debug("EVENTS", "SSE event type=\(type) bytes=\(data.utf8.count)")
        guard let jsonData = data.data(using: .utf8) else { return }

        switch type {
        case "new_mail":
            do {
                let event = try JSONDecoder().decode(NewMailEvent.self, from: jsonData)
                Log.info("EVENTS", "new_mail — \(event.total_new) message(s) for \(event.account)")
                for msg in event.messages {
                    Log.debug("EVENTS", "  thread=\(msg.thread_id)")
                }
                onNewMail?(event)
            } catch {
                Log.error("EVENTS", "Failed to decode new_mail event: \(error)")
            }

        case "outbox_update":
            do {
                let event = try JSONDecoder().decode(OutboxUpdateEvent.self, from: jsonData)
                Log.info("EVENTS", "outbox_update — id=\(event.item_id) status=\(event.status)")
                onOutboxUpdate?(event)
            } catch {
                Log.error("EVENTS", "Failed to decode outbox_update event: \(error)")
            }

        case "calendar_updated":
            do {
                let event = try JSONDecoder().decode(CalendarUpdatedEvent.self, from: jsonData)
                Log.info("EVENTS", "calendar_updated — \(event.downloaded) new, \(event.pruned) pruned for \(event.account)")
                onCalendarUpdated?(event)
            } catch {
                Log.error("EVENTS", "Failed to decode calendar_updated event: \(error)")
            }

        default:
            Log.debug("EVENTS", "Unknown event type: \(type)")
        }
    }
}
