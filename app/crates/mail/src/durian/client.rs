//! Authenticated durian HTTP API. Blocking (ureq): call it off the UI thread.

use std::fmt;
use std::sync::{Arc, Mutex, atomic::{AtomicBool, Ordering}};
use std::time::{Duration, Instant};

use serde::de::DeserializeOwned;
use serde::Serialize;
use ureq::unversioned::resolver::DefaultResolver;
use ureq::unversioned::transport::{Connector, DefaultConnector};

use super::api::*;

/// What went wrong, in the terms the UI reacts to.
#[derive(Debug, Clone, PartialEq)]
pub enum Error {
    /// no server at that address, or it didn't answer in time
    Unreachable(String),
    /// 401/403: missing or wrong token
    Unauthorized,
    /// 404: the thread/event is gone (or this server is too old for the endpoint)
    NotFound(String),
    /// the server answered with an error (status, its message)
    Server { status: u16, message: String },
    /// the answer wasn't what the API describes
    Decode(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::Unreachable(detail) => write!(f, "durian isn't reachable ({detail})"),
            Error::Unauthorized => write!(f, "durian refused the token"),
            Error::NotFound(path) => write!(f, "not found: {path}"),
            Error::Server { status, message } => write!(f, "durian error {status}: {message}"),
            Error::Decode(detail) => write!(f, "unexpected answer from durian ({detail})"),
        }
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

/// Largest attachment read into memory (mail servers cap messages well below this).
const ATTACHMENT_LIMIT: u64 = 256 << 20;

/// Go shares 10 requests/s (burst 30) between search and count. Keep below that
/// sustained rate instead of spending the burst on background sidebar badges.
const READ_SPACING: Duration = Duration::from_millis(125);
const READ_WAIT_LIMIT: Duration = Duration::from_secs(20);

struct ReadBudget {
    next: Instant,
    paused_until: Instant,
    waiting_searches: usize,
}

pub struct Client {
    /// e.g. "http://127.0.0.1:9723/api/v1"
    base: String,
    /// bearer token printed by `durian serve` (none with `--no-auth`)
    token: Option<String>,
    agent: ureq::Agent,
    /// Shared by preconnect, interactive searches and every count worker.
    reads: Mutex<ReadBudget>,
}

/// Percent-encode one path segment (ids are opaque and may contain ':' or '/').
fn segment(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => (b as char).to_string(),
            _ => format!("%{b:02X}"),
        })
        .collect()
}

impl Client {
    /// `server` is "http://127.0.0.1:9723" (the `/api/v1` prefix is added).
    pub fn new(server: &str, token: Option<String>) -> Self {
        let config = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_secs(20)))
            .timeout_connect(Some(Duration::from_secs(3)))
            // statuses are ours to interpret: the body carries the server's message
            .http_status_as_error(false)
            .build();
        // Keep ureq's normal proxy/TLS setup. Resume only interrupted response
        // reads after connecting; a POST/PUT is never sent a second time.
        let connector = DefaultConnector::default().chain(super::transport::InterruptedConnector);
        let agent = ureq::Agent::with_parts(config, connector, DefaultResolver::default());
        let now = Instant::now();
        Self {
            base: format!("{}/api/v1", server.trim_end_matches('/')), token, agent,
            reads: Mutex::new(ReadBudget { next: now, paused_until: now, waiting_searches: 0 }),
        }
    }

    fn call(&self, path: &str, query: &[(&str, &str)]) -> Result<ureq::http::Response<ureq::Body>> {
        let mut request = self.agent.get(format!("{}{path}", self.base));
        for (k, v) in query {
            request = request.query(*k, *v);
        }
        if let Some(token) = &self.token {
            request = request.header("Authorization", format!("Bearer {token}"));
        }
        // errors name the endpoint, never the query or body: those carry mail content
        request.call().map_err(|e| Error::Unreachable(e.to_string()))
    }

    /// A non-2xx answer as an error: the envelope's message, else the start of the body.
    fn failure(path: &str, status: u16, text: &str) -> Error {
        let envelope = serde_json::from_str::<Envelope>(text).unwrap_or_default();
        let message = envelope.error.filter(|m| !m.is_empty()).unwrap_or_else(|| text.trim().chars().take(200).collect());
        match status {
            401 | 403 => Error::Unauthorized,
            404 => Error::NotFound(path.to_string()),
            _ => Error::Server { status, message },
        }
    }

    fn get<T: DeserializeOwned>(&self, path: &str, query: &[(&str, &str)]) -> Result<T> {
        let mut response = self.call(path, query)?;
        Self::decode(path, &mut response)
    }

    /// Wait off the UI thread, without reserving a queue of future count slots.
    /// A newly requested mail list goes before any counts still waiting for admission.
    /// Already-sent reads finish normally; superseded counts never start another GET.
    fn wait_read(&self, foreground: bool, cancel: Option<&AtomicBool>) -> Result<()> {
        let deadline = Instant::now() + READ_WAIT_LIMIT;
        let mut budget = self.reads.lock().unwrap();
        if foreground { budget.waiting_searches += 1; }
        loop {
            let now = Instant::now();
            let cancelled = cancel.is_some_and(|cancel| cancel.load(Ordering::Relaxed));
            if cancelled || now >= deadline {
                if foreground { budget.waiting_searches -= 1; }
                return Err(Error::Unreachable(if cancelled { "search refresh superseded" } else { "search admission timed out" }.into()));
            }
            let ready = budget.next.max(budget.paused_until);
            if now >= ready && (foreground || budget.waiting_searches == 0) {
                if foreground { budget.waiting_searches -= 1; }
                budget.next = now + READ_SPACING;
                return Ok(());
            }
            let delay = if now < ready { ready - now } else { Duration::from_millis(25) };
            drop(budget);
            std::thread::sleep(delay.min(Duration::from_millis(25)));
            budget = self.reads.lock().unwrap();
        }
    }

    /// Only explicit 429 responses from these idempotent reads retry. Share the
    /// server's cooldown, so four count workers cannot independently rush it again.
    /// Transport errors, other statuses and all mutations keep their original behavior.
    fn search_get<T: DeserializeOwned>(&self, path: &str, query: &[(&str, &str)], cancel: Option<&AtomicBool>) -> Result<T> {
        for attempt in 0..4 {
            self.wait_read(path == "/search", cancel)?;
            let mut response = self.call(path, query)?;
            if response.status().as_u16() != 429 {
                return Self::decode(path, &mut response);
            }
            let retry = response.headers().get("retry-after").and_then(|value| value.to_str().ok()).and_then(|value| {
                value.trim().parse::<u64>().ok().map(Duration::from_secs).or_else(|| {
                    chrono::DateTime::parse_from_rfc2822(value).ok()
                        .map(|date| (date.with_timezone(&chrono::Utc) - chrono::Utc::now()).to_std().unwrap_or_default())
                })
            }).unwrap_or(Duration::from_secs(1 << attempt));
            let Some(until) = Instant::now().checked_add(retry) else { return Self::decode(path, &mut response) };
            {
                let mut budget = self.reads.lock().unwrap();
                budget.paused_until = budget.paused_until.max(until);
            }
            crate::trace::note(&format!("durian: {path} rate limited; cooldown {retry:.1?}, attempt {} of 4", attempt + 1));
            // Do not wait indefinitely for an external server; nor shorten its
            // Retry-After to force an early retry. Return the actual 429 instead.
            if attempt == 3 || retry >= READ_WAIT_LIMIT {
                return Self::decode(path, &mut response);
            }
        }
        unreachable!("the last attempt returns its result")
    }

    /// Shared authenticated JSON transport for mail and calendar writes. Never retries
    /// a mutation implicitly: the caller owns its logical identity and reconciliation.
    pub(super) fn request_json<T: DeserializeOwned>(&self, method: &str, path: &str, query: &[(&str, &str)], body: &impl Serialize) -> Result<T> {
        let mut url = format!("{}{path}", self.base);
        if !query.is_empty() {
            url.push('?');
            url.push_str(&query.iter().map(|(k, v)| format!("{}={}", segment(k), segment(v))).collect::<Vec<_>>().join("&"));
        }
        let bytes = serde_json::to_vec(body).map_err(|e| Error::Decode(e.to_string()))?;
        let mut request = ureq::http::Request::builder().method(method).uri(url).header("Content-Type", "application/json");
        if let Some(token) = &self.token {
            request = request.header("Authorization", format!("Bearer {token}"));
        }
        let request = request.body(bytes).map_err(|e| Error::Decode(e.to_string()))?;
        let mut response = self.agent.run(request).map_err(|_| Error::Unreachable(format!("{method} {path} failed; outcome may be unknown")))?;
        Self::decode(path, &mut response)
    }

    fn decode<T: DeserializeOwned>(path: &str, response: &mut ureq::http::Response<ureq::Body>) -> Result<T> {
        let status = response.status().as_u16();
        let text = response.body_mut().read_to_string().map_err(|e| Error::Unreachable(format!("{path}: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(Self::failure(path, status, &text));
        }
        // ok:false inside a 2xx is an error too
        let envelope = serde_json::from_str::<Envelope>(&text).unwrap_or_default();
        if envelope.ok == Some(false) {
            return Err(Error::Server { status, message: envelope.error.unwrap_or_default() });
        }
        serde_json::from_str(&text).map_err(|e| Error::Decode(format!("{path}: {e}")))
    }

    /// A cancellable streaming GET. Its agent has no whole-response deadline:
    /// the events worker polls the socket underneath HTTP/TLS and owns its lifetime.
    pub(super) fn event_stream(&self, stop: Arc<AtomicBool>) -> Result<ureq::BodyReader<'static>> {
        let agent = super::events::agent(stop);
        let mut request = agent.get(format!("{}/events", self.base))
            .header("Accept", "text/event-stream");
        if let Some(token) = &self.token {
            request = request.header("Authorization", format!("Bearer {token}"));
        }
        let response = request.call().map_err(|_| Error::Unreachable("SSE connection failed".into()))?;
        let status = response.status().as_u16();
        // Only 200 is a stream (not 204, redirects or a JSON error envelope).
        if status != 200 {
            return Err(Self::failure("/events", status, "event stream unavailable"));
        }
        let content_type = response.headers().get("content-type").and_then(|v| v.to_str().ok()).unwrap_or("");
        if !content_type.split(';').next().unwrap_or("").trim().eq_ignore_ascii_case("text/event-stream") {
            return Err(Error::Decode("/events: expected text/event-stream".into()));
        }
        Ok(response.into_body().into_reader())
    }

    /// Raw bytes (attachments), up to `limit`.
    fn get_bytes(&self, path: &str, limit: u64) -> Result<Vec<u8>> {
        let mut response = self.call(path, &[])?;
        let status = response.status().as_u16();
        if !(200..300).contains(&status) {
            let text = response.body_mut().read_to_string().unwrap_or_default();
            return Err(Self::failure(path, status, &text));
        }
        response.body_mut().with_config().limit(limit).read_to_vec().map_err(|e| Error::Unreachable(format!("{path}: {e}")))
    }

    pub fn version(&self) -> Result<Version> {
        self.get("/version", &[])
    }

    /// Threads matching a durian query, newest first; the first `enrich` come with content.
    pub fn search(&self, query: &str, limit: usize, enrich: usize) -> Result<SearchResponse> {
        self.search_get("/search", &[("query", query), ("limit", &limit.to_string()), ("enrich", &enrich.to_string())], None)
    }

    pub(super) fn search_cancellable(&self, query: &str, limit: usize, enrich: usize, cancel: &AtomicBool) -> Result<SearchResponse> {
        self.search_get("/search", &[("query", query), ("limit", &limit.to_string()), ("enrich", &enrich.to_string())], Some(cancel))
    }

    pub fn count(&self, query: &str) -> Result<u64> {
        Ok(self.search_get::<SearchCount>("/search/count", &[("query", query)], None)?.count)
    }

    pub(super) fn count_cancellable(&self, query: &str, cancel: &AtomicBool) -> Result<u64> {
        Ok(self.search_get::<SearchCount>("/search/count", &[("query", query)], Some(cancel))?.count)
    }

    pub fn contacts(&self, query: &str) -> Result<Vec<Contact>> {
        self.get("/contacts/search", &[("query", query), ("limit", "10")])
    }

    pub fn thread(&self, thread_id: &str) -> Result<ThreadContent> {
        let path = format!("/threads/{}", segment(thread_id));
        self.get::<ThreadResponse>(&path, &[])?.thread.ok_or_else(|| Error::Decode(format!("{path}: no thread in the answer")))
    }

    /// One MIME part's bytes (`message`: a message's opaque id).
    pub fn attachment(&self, message: &str, part: i64) -> Result<Vec<u8>> {
        self.get_bytes(&format!("/messages/{}/attachments/{part}", segment(message)), ATTACHMENT_LIMIT)
    }

    pub(super) fn draft_attachment(&self, message: &str, part: i64) -> Result<Vec<u8>> {
        self.get_bytes(&format!("/messages/{}/attachments/{part}", segment(message)), 1 << 20)
    }

    /// Profiles with their accounts (servers before durian#… answer NotFound).
    pub fn profiles(&self) -> Result<ProfilesResponse> {
        self.get("/profiles", &[])
    }

    pub fn keymaps(&self) -> Result<Option<crate::keymaps::File>> {
        #[derive(serde::Deserialize)]
        struct Response { config: Option<crate::keymaps::File> }
        Ok(self.get::<Response>("/keymaps", &[])?.config)
    }

    /// The app settings (durian ≥ #439; older ones answer NotFound).
    pub fn settings(&self) -> Result<Settings> {
        Ok(self.get::<SettingsResponse>("/settings", &[])?.settings)
    }

    pub fn tags(&self) -> Result<Vec<String>> {
        Ok(self.get::<TagsResponse>("/tags", &[])?.tags)
    }

    pub fn tag_thread(&self, id: &str, tags: &str) -> Result<()> {
        let _: Envelope = self.request_json("POST", &format!("/threads/{}/tags", segment(id)), &[], &serde_json::json!({"tags": tags}))?;
        Ok(())
    }

    pub fn drafts(&self) -> Result<Vec<LocalDraft>> { self.get("/local-drafts", &[]) }

    pub fn save_draft(&self, draft: &DraftContent) -> Result<()> {
        if !super::writes::draft_fits(draft) { return Err(Error::Decode("Draft exceeds the server's 1 MiB recovery limit".into())); }
        let _: Envelope = self.request_json("PUT", &format!("/local-drafts/{}", segment(&draft.id)), &[], &serde_json::json!({"draft_json": draft}))?;
        Ok(())
    }

    pub fn delete_draft(&self, id: &str) -> Result<()> {
        let _: Envelope = self.request_json("DELETE", &format!("/local-drafts/{}", segment(id)), &[], &())?;
        Ok(())
    }

    pub fn enqueue(&self, draft: &OutboxDraft) -> Result<Enqueued> {
        self.request_json("POST", "/outbox/send", &[], draft)
    }

    pub fn outbox(&self) -> Result<Vec<OutboxEntry>> { self.get("/outbox", &[]) }

    pub fn cancel_send(&self, id: i64) -> Result<()> {
        let _: Envelope = self.request_json("DELETE", &format!("/outbox/{id}"), &[], &())?;
        Ok(())
    }

    pub fn calendars(&self, account: &str) -> Result<Vec<Calendar>> {
        Ok(self.get::<CalendarsResponse>("/calendars", &[("account", account)])?.calendars)
    }

    /// Occurrences overlapping [from, to) ("2026-10-01" dates are fine).
    pub fn events(&self, account: &str, from: &str, to: &str) -> Result<Vec<CalendarEvent>> {
        Ok(self.get::<CalendarEventsResponse>("/calendars/events", &[("account", account), ("from", from), ("to", to)])?.events)
    }

    /// One event by exact iCalUID; a removed event must not resolve to a prefix match.
    pub fn event(&self, account: &str, calendar: &str, uid: &str) -> Result<CalendarEvent> {
        Ok(self.get::<CalendarEventResponse>("/calendars/event", &[("account", account), ("calendar", calendar), ("uid", uid)])?.event)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::net::TcpListener;
    use std::sync::mpsc;

    /// A one-request HTTP server on a free loopback port: answers with `status` and `body`,
    /// hands back the request line and headers it received.
    fn serve_once(status: u16, body: &'static str) -> (String, mpsc::Receiver<String>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = format!("http://{}", listener.local_addr().unwrap());
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = String::new();
            let mut reader = BufReader::new(stream.try_clone().unwrap());
            loop {
                let mut line = String::new();
                if reader.read_line(&mut line).unwrap() == 0 || line == "\r\n" {
                    break;
                }
                request.push_str(&line);
            }
            let reply = format!("HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}", body.len());
            stream.write_all(reply.as_bytes()).unwrap();
            let _ = tx.send(request);
        });
        (address, rx)
    }

    #[test]
    fn ids_are_one_path_segment() {
        assert_eq!(segment("local:123"), "local%3A123");
        assert_eq!(segment("a/b c"), "a%2Fb%20c");
        assert_eq!(segment("9f86d081884c7d65"), "9f86d081884c7d65");
    }

    #[test]
    fn sends_the_token_and_encodes_the_query() {
        let (address, request) = serve_once(200, r#"{"ok":true,"results":[],"threads":{}}"#);
        let client = Client::new(&address, Some("secret".into()));
        let found = client.search("tag:inbox from:mara", 50, 5).unwrap();
        assert!(found.results.is_empty());
        let request = request.recv().unwrap();
        assert!(request.starts_with("GET /api/v1/search?query=tag%3Ainbox+from%3Amara&limit=50&enrich=5 ") || request.starts_with("GET /api/v1/search?query=tag%3Ainbox%20from%3Amara&limit=50&enrich=5 "), "{request}");
        assert!(request.to_lowercase().contains("authorization: bearer secret"));
    }

    #[test]
    fn settings_are_read_and_absent_on_older_servers() {
        let (address, request) = serve_once(200, r#"{"ok":true,"settings":{"theme":"dark","notifications_enabled":true,"load_remote_images":true}}"#);
        assert_eq!(Client::new(&address, None).settings().unwrap(), Settings { load_remote_images: true });
        assert!(request.recv().unwrap().starts_with("GET /api/v1/settings "));

        // durian before /settings: gorilla/mux's plain-text 404
        let (address, _) = serve_once(404, "404 page not found\n");
        assert!(matches!(Client::new(&address, None).settings().unwrap_err(), Error::NotFound(_)));
    }

    #[test]
    fn empty_attachment_is_success_not_a_missing_part() {
        let (address, request) = serve_once(200, "");
        assert_eq!(Client::new(&address, Some("token".into())).attachment("a/b", 2).unwrap(), Vec::<u8>::new());
        let request = request.recv().unwrap();
        assert!(request.starts_with("GET /api/v1/messages/a%2Fb/attachments/2 "));
        assert!(request.to_lowercase().contains("authorization: bearer token"));
    }

    #[test]
    fn statuses_become_errors_the_ui_can_tell_apart() {
        let (address, _) = serve_once(401, r#"{"ok":false,"error":"missing token"}"#);
        assert_eq!(Client::new(&address, None).tags().unwrap_err(), Error::Unauthorized);

        let (address, _) = serve_once(500, r#"{"ok":false,"error":"database is locked"}"#);
        assert_eq!(Client::new(&address, None).tags().unwrap_err(), Error::Server { status: 500, message: "database is locked".into() });

        // ok:false inside a 200 is an error too
        let (address, _) = serve_once(200, r#"{"ok":false,"error":"bad query"}"#);
        assert!(matches!(Client::new(&address, None).search("((", 1, 0).unwrap_err(), Error::Server { message, .. } if message == "bad query"));

        let (address, _) = serve_once(404, r#"{"ok":false,"error":"no such part"}"#);
        assert!(matches!(Client::new(&address, None).attachment("local:1", 9).unwrap_err(), Error::NotFound(path) if path == "/messages/local%3A1/attachments/9"));

        let (address, _) = serve_once(200, r#"{"ok":true,"results":"nope"}"#);
        assert!(matches!(Client::new(&address, None).search("x", 1, 0).unwrap_err(), Error::Decode(_)));

        // nothing listening
        let port = TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
        assert!(matches!(Client::new(&format!("http://127.0.0.1:{port}"), None).version().unwrap_err(), Error::Unreachable(_)));
    }

    #[test]
    fn search_preempts_counts_after_shared_retry_after_and_writes_never_retry() {
        use std::io::Read;

        for date_header in [false, true] {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let client = Client::new(&format!("http://{}", listener.local_addr().unwrap()), Some("budget-token".into()));
            let (limited_tx, limited_rx) = mpsc::channel();
            let server = std::thread::spawn(move || {
                let mut not_before = Instant::now();
                let mut previous_read = None;
                // Initial count 429, unrelated version, non-replayed write 429,
                // foreground search, then the retried count and three other counts.
                for index in 0..8 {
                    let deadline = Instant::now() + Duration::from_secs(5);
                    let mut stream = loop {
                        match listener.accept() {
                            Ok((stream, _)) => break stream,
                            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock && Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
                            Err(e) => panic!("read-budget accept: {e}"),
                        }
                    };
                    let arrived = Instant::now();
                    stream.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
                    let mut reader = BufReader::new(stream.try_clone().unwrap());
                    let mut request = String::new();
                    loop {
                        let mut line = String::new();
                        assert_ne!(reader.read_line(&mut line).unwrap(), 0);
                        if line == "\r\n" { break }
                        request.push_str(&line);
                    }
                    assert!(request.to_lowercase().contains("authorization: bearer budget-token"));
                    let (status, body) = match index {
                        0 => { assert!(request.starts_with("GET /api/v1/search/count?")); (429, r#"{"ok":false,"error":"rate limited"}"#) }
                        1 => { assert!(request.starts_with("GET /api/v1/version ")); (200, r#"{"ok":true,"version":"budget-test"}"#) }
                        2 => {
                            assert!(request.starts_with("POST /api/v1/threads/one/tags "));
                            let length: usize = request.lines().find_map(|line| line.to_ascii_lowercase().strip_prefix("content-length:").map(|n| n.trim().parse().unwrap())).unwrap();
                            let mut body = vec![0; length];
                            reader.read_exact(&mut body).unwrap();
                            assert_eq!(serde_json::from_slice::<serde_json::Value>(&body).unwrap(), serde_json::json!({"tags": "+flagged"}));
                            (429, r#"{"ok":false,"error":"write refused"}"#)
                        }
                        3 => {
                            assert!(request.starts_with("GET /api/v1/search?"), "queued counts must not take the foreground's slot: {request}");
                            (200, r#"{"ok":true,"results":[],"threads":{}}"#)
                        }
                        _ => { assert!(request.starts_with("GET /api/v1/search/count?")); (200, r#"{"ok":true,"count":7}"#) }
                    };
                    if index >= 3 {
                        assert!(arrived >= not_before, "a different reader ignored the shared Retry-After");
                        if let Some(previous) = previous_read {
                            assert!(arrived.duration_since(previous) >= Duration::from_millis(100), "background reads still burst after cooldown");
                        }
                        previous_read = Some(arrived);
                    }
                    let header = if index == 0 && date_header {
                        let at = chrono::Utc::now() + chrono::Duration::seconds(3);
                        // HTTP dates have second precision. Compare wire arrivals
                        // to the actual advertised instant, not the client's parser.
                        let delay = Duration::from_millis((at.timestamp() * 1000 - chrono::Utc::now().timestamp_millis()) as u64);
                        not_before = Instant::now() + delay;
                        format!("Retry-After: {}\r\n", at.format("%a, %d %b %Y %H:%M:%S GMT"))
                    } else if index == 0 {
                        not_before = Instant::now() + Duration::from_secs(1);
                        "Retry-After: 1\r\n".into()
                    } else { String::new() };
                    write!(stream, "HTTP/1.1 {status} X\r\n{header}Content-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
                    if index == 0 { limited_tx.send(()).unwrap(); }
                }
            });
            std::thread::scope(|scope| {
                let first = scope.spawn(|| client.count("tag:inbox"));
                limited_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                assert_eq!(client.version().unwrap().version, "budget-test", "unrelated reads must not wait for search cooldown");
                assert_eq!(client.tag_thread("one", "+flagged"), Err(Error::Server { status: 429, message: "write refused".into() }));
                let counts: Vec<_> = (0..3).map(|_| scope.spawn(|| client.count("tag:inbox"))).collect();
                let cancel = Arc::new(AtomicBool::new(false));
                let stop = cancel.clone();
                let cancellable_client = &client;
                let cancelled = scope.spawn(move || cancellable_client.count_cancellable("tag:cancelled", &cancel));
                std::thread::sleep(Duration::from_millis(50));
                stop.store(true, Ordering::Relaxed);
                assert!(cancelled.join().unwrap().is_err());
                assert!(client.search("tag:inbox", 7, 0).unwrap().results.is_empty());
                assert_eq!(first.join().unwrap(), Ok(7));
                for count in counts { assert_eq!(count.join().unwrap(), Ok(7)); }
            });
            server.join().unwrap();
        }
    }

    #[test]
    fn search_rate_limit_retries_are_bounded() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client = Client::new(&format!("http://{}", listener.local_addr().unwrap()), None);
        let server = std::thread::spawn(move || {
            for _ in 0..4 {
                let (mut stream, _) = listener.accept().unwrap();
                stream.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                loop {
                    let mut line = String::new();
                    assert_ne!(reader.read_line(&mut line).unwrap(), 0);
                    if line == "\r\n" { break }
                }
                let body = r#"{"ok":false,"error":"still limited"}"#;
                write!(stream, "HTTP/1.1 429 X\r\nRetry-After: 0\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            }
        });
        assert!(matches!(client.search("tag:inbox", 7, 0), Err(Error::Server { status: 429, message }) if message == "still limited"));
        server.join().unwrap();
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn signals_preserve_responses_without_replay_or_deadline_extension() {
        use std::io::Read;
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::time::Instant;

        // Signal dispositions are process-wide. Keep the real timed-socket
        // regression isolated from every other test, as with renderer fixtures.
        if std::env::var_os("DURIAN_SIGNAL_TEST").is_none() {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args(["--exact", std::thread::current().name().unwrap(), "--nocapture"])
                .env("DURIAN_SIGNAL_TEST", "1").output().unwrap();
            assert!(output.status.success(), "{}\n{}", String::from_utf8_lossy(&output.stdout), String::from_utf8_lossy(&output.stderr));
            return;
        }
        std::thread::spawn(|| {
            std::thread::sleep(Duration::from_secs(40));
            std::process::exit(2);
        });
        static SIGNALS: AtomicUsize = AtomicUsize::new(0);
        extern "C" fn signal(_: libc::c_int) { SIGNALS.fetch_add(1, Ordering::Relaxed); }
        unsafe {
            let mut action: libc::sigaction = std::mem::zeroed();
            action.sa_sigaction = signal as *const () as usize;
            action.sa_flags = libc::SA_RESTART;
            assert_eq!(libc::sigemptyset(&mut action.sa_mask), 0);
            assert_eq!(libc::sigaction(libc::SIGUSR1, &action, std::ptr::null_mut()), 0);
        }
        let reader_thread = unsafe { libc::pthread_self() };
        for case in ["headers", "partial headers", "body", "write acknowledgment", "deadline"] {
            SIGNALS.store(0, Ordering::Relaxed);
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let client = Client::new(&format!("http://{}", listener.local_addr().unwrap()), Some("signal-token".into()));
            let server = std::thread::spawn(move || {
                let (mut stream, _) = listener.accept().unwrap();
                stream.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut request = String::new();
                loop {
                    let mut line = String::new();
                    assert_ne!(reader.read_line(&mut line).unwrap(), 0);
                    if line == "\r\n" { break }
                    request.push_str(&line);
                }
                if case == "write acknowledgment" {
                    assert!(request.starts_with("POST /api/v1/threads/one/tags HTTP/1.1\r\n"));
                    let length: usize = request.lines().find_map(|line| line.to_ascii_lowercase().strip_prefix("content-length:").map(|n| n.trim().parse().unwrap())).unwrap();
                    let mut body = vec![0; length];
                    reader.read_exact(&mut body).unwrap();
                    assert_eq!(serde_json::from_slice::<serde_json::Value>(&body).unwrap(), serde_json::json!({"tags": "+flagged"}));
                } else {
                    assert!(request.starts_with("GET /api/v1/version HTTP/1.1\r\n"));
                }
                let body = if case == "write acknowledgment" { r#"{"ok":true}"# } else { r#"{"ok":true,"version":"signal-safe"}"# };
                let headers = format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", body.len());
                let response = format!("{headers}{body}");
                let split = match case { "partial headers" => 28, "body" => headers.len() + 5, _ => 0 };
                stream.write_all(&response.as_bytes()[..split]).unwrap();
                // The request has arrived once. Interrupt that same reader, never
                // replay it on a second connection or restart the global budget.
                let until = Instant::now() + if case == "deadline" { Duration::from_secs(21) } else { Duration::from_millis(200) };
                while Instant::now() < until {
                    std::thread::sleep(Duration::from_millis(25));
                    assert_eq!(unsafe { libc::pthread_kill(reader_thread, libc::SIGUSR1) }, 0);
                }
                let _ = stream.write_all(&response.as_bytes()[split..]);
                // Dropping the sole listener also makes a whole-request replay fail.
            });
            let started = Instant::now();
            let result = if case == "write acknowledgment" {
                client.tag_thread("one", "+flagged").map(|_| "signal-safe".to_string())
            } else {
                client.version().map(|version| version.version)
            };
            let took = started.elapsed();
            server.join().unwrap();
            assert!(SIGNALS.load(Ordering::Relaxed) >= 4, "the requesting thread must receive signals");
            if case == "deadline" {
                assert!(result.is_err(), "a silent peer must hit the original global deadline");
                assert!(took >= Duration::from_secs(19) && took < Duration::from_secs(21), "deadline changed: {took:?}");
            } else {
                assert_eq!(result, Ok("signal-safe".to_string()), "{case}");
            }
        }
    }
}

/// Request timings against a running server: `DURIAN_URL=http://127.0.0.1:9723 cargo test -p
/// mail timing -- --ignored --nocapture` (GET only).
#[cfg(test)]
mod timing {
    use std::time::Instant;

    #[test]
    #[ignore]
    fn timing() {
        let Ok(url) = std::env::var("DURIAN_URL") else { return };
        let client = super::Client::new(&url, None);
        for _ in 0..2 {
            let t = Instant::now();
            client.version().unwrap();
            let v = t.elapsed();
            let t = Instant::now();
            let n = client.search("tag:inbox", 200, 10).unwrap().results.len();
            println!("version {v:.1?}, search {n} rows {:.1?}", t.elapsed());
        }
    }
}
