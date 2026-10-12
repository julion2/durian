//! Authenticated durian HTTP API. Blocking (ureq): call it off the UI thread.

use std::fmt;
use std::sync::{Arc, atomic::AtomicBool};
use std::time::Duration;

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

pub struct Client {
    /// e.g. "http://127.0.0.1:9723/api/v1"
    base: String,
    /// bearer token printed by `durian serve` (none with `--no-auth`)
    token: Option<String>,
    agent: ureq::Agent,
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
        Self { base: format!("{}/api/v1", server.trim_end_matches('/')), token, agent }
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
        self.get("/search", &[("query", query), ("limit", &limit.to_string()), ("enrich", &enrich.to_string())])
    }

    pub fn count(&self, query: &str) -> Result<u64> {
        Ok(self.get::<SearchCount>("/search/count", &[("query", query)])?.count)
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
