//
//  CalendarBackend.swift
//  Durian
//
//  Read-only calendar client for the durian CLI HTTP server. Reuses the
//  already-running `durian serve` and the bearer token EmailBackend obtained at
//  startup (EmailBackend.authToken) — it never starts its own server. Mirrors
//  EmailBackend's generic request pipeline. See openapi.yaml /calendars*.
//

import Foundation

// MARK: - Wire models (snake_case, match openapi.yaml)

struct CalendarWire: Decodable {
    let name: String
    let color: String?
    let event_count: Int
}

struct CalendarPersonWire: Decodable {
    let name: String?
    let email: String
}

struct CalendarAttendeeWire: Decodable {
    let name: String?
    let email: String
    let type: String?
    let response: String?
}

struct CalendarEventWire: Decodable {
    let calendar: String
    let uid: String
    let subject: String
    let start: String
    let end: String
    let all_day: Bool
    let location: String?
    let my_response: String?
    let online_meeting: Bool?
    let online_meeting_url: String?
    let recurring: Bool?
    let organizer: CalendarPersonWire?
    let attendees: [CalendarAttendeeWire]?
    let description: String?
}

private struct CalendarsResponse: Decodable {
    let ok: Bool
    let calendars: [CalendarWire]?
}

private struct CalendarEventsResponse: Decodable {
    let ok: Bool
    let events: [CalendarEventWire]?
}

private struct CalendarEventResponse: Decodable {
    let ok: Bool
    let event: CalendarEventWire?
}

/// PUT /calendars/event body (snake_case). uid empty = create.
struct CalendarEventWrite: Encodable {
    let account: String
    let calendar: String
    let uid: String
    let subject: String
    let start: String
    let end: String
    let all_day: Bool
    let location: String
    let description: String
}

// MARK: - Calendar Backend

@MainActor
final class CalendarBackend {
    private let decoder = JSONDecoder()
    private let baseURL = URL(string: "http://localhost:9723/api/v1")!

    /// The account's calendars (name, color, event count).
    func listCalendars(account: String) async -> [CalendarWire] {
        let resp: CalendarsResponse? = await request("/calendars", [
            URLQueryItem(name: "account", value: account),
        ])
        return resp?.calendars ?? []
    }

    /// Events of the account. With `query`, a full-text search across all
    /// events; otherwise the events starting within [from, to).
    func listEvents(account: String, from: Date? = nil, to: Date? = nil,
                    calendar: String? = nil, query: String? = nil) async -> [CalendarEventWire]
    {
        var items = [URLQueryItem(name: "account", value: account)]
        if let query, !query.isEmpty {
            items.append(URLQueryItem(name: "q", value: query))
        } else {
            if let from { items.append(URLQueryItem(name: "from", value: Self.rfc3339(from))) }
            if let to { items.append(URLQueryItem(name: "to", value: Self.rfc3339(to))) }
        }
        if let calendar, !calendar.isEmpty {
            items.append(URLQueryItem(name: "calendar", value: calendar))
        }
        let resp: CalendarEventsResponse? = await request("/calendars/events", items)
        return resp?.events ?? []
    }

    /// One event in detail, by reference (iCalUID or unique subject substring).
    func event(account: String, ref: String, calendar: String? = nil) async -> CalendarEventWire? {
        var items = [
            URLQueryItem(name: "account", value: account),
            URLQueryItem(name: "ref", value: ref),
        ]
        if let calendar, !calendar.isEmpty {
            items.append(URLQueryItem(name: "calendar", value: calendar))
        }
        let resp: CalendarEventResponse? = await request("/calendars/event", items)
        return resp?.event
    }

    /// Creates or updates a local event (empty uid = create). Local-first: this
    /// only writes the vdir; Outlook is updated on the next sync.
    func putEvent(_ write: CalendarEventWrite) async -> CalendarEventWire? {
        guard let data = try? JSONEncoder().encode(write) else { return nil }
        let resp: CalendarEventResponse? = await request("/calendars/event", method: "PUT", bodyData: data)
        return resp?.event
    }

    /// Deletes a local event by reference. Returns whether it succeeded.
    func deleteEvent(account: String, ref: String, calendar: String?) async -> Bool {
        var items = [
            URLQueryItem(name: "account", value: account),
            URLQueryItem(name: "ref", value: ref),
        ]
        if let calendar, !calendar.isEmpty {
            items.append(URLQueryItem(name: "calendar", value: calendar))
        }
        struct OKResponse: Decodable { let ok: Bool }
        let resp: OKResponse? = await request("/calendars/event", items, method: "DELETE")
        return resp?.ok ?? false
    }

    // MARK: - Request pipeline (mirrors EmailBackend.performRequest)

    private func request<T: Decodable>(
        _ path: String,
        _ queryItems: [URLQueryItem] = [],
        method: String = "GET",
        bodyData: Data? = nil
    ) async -> T? {
        guard var comps = URLComponents(string: "\(baseURL)\(path)") else { return nil }
        if !queryItems.isEmpty { comps.queryItems = queryItems }
        guard let url = comps.url else { return nil }

        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 10
        if let token = EmailBackend.authToken {
            req.addValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        if let bodyData {
            req.addValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = bodyData
        }

        do {
            let (data, _) = try await URLSession.shared.data(for: req)
            return try decoder.decode(T.self, from: data)
        } catch is CancellationError {
            return nil
        } catch let error as URLError where error.code == .cancelled {
            return nil
        } catch {
            Log.error("CALENDAR", "Request to \(path) failed: \(error)")
            return nil
        }
    }

    /// Formats a date as RFC3339 UTC, which the API's ParseWhen accepts.
    private static func rfc3339(_ date: Date) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        f.timeZone = TimeZone(identifier: "UTC")
        return f.string(from: date)
    }
}
