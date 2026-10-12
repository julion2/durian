//! An owned, read-only SSE connection. Payloads are freshness hints, not mailbox data:
//! the Go hub has no replay and drops events, so every connection invalidates both views.
//! One worker, bounded parser and coalesced flags keep an event burst bounded in memory.

use std::io::{self, Read};
use std::collections::VecDeque;
use std::sync::{Arc, Condvar, Mutex, atomic::{AtomicBool, Ordering}};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use ureq::unversioned::resolver::DefaultResolver;
use ureq::unversioned::transport::{Buffers, ConnectionDetails, Connector, NextTimeout, RustlsConnector, TcpConnector, Transport, time::Duration as TransportDuration};

use super::client::{Client, Error, Result};

const FRAME_LIMIT: usize = 64 << 10;
const POLL: Duration = Duration::from_millis(100);
const IDLE: Duration = Duration::from_secs(45); // Go sends a heartbeat every 30 seconds.
const RETRY_MIN: Duration = Duration::from_millis(250);
const RETRY_MAX: Duration = Duration::from_secs(30);
const DEBOUNCE: Duration = Duration::from_millis(500);
const BURST_MAX: Duration = Duration::from_secs(2);

#[derive(Debug, PartialEq, Eq)]
enum Notice {
    Mail,
    Calendar,
    Outbox(super::api::OutboxUpdate),
    Retry(Duration),
}

#[derive(Default)]
struct Parser {
    line: Vec<u8>,
    event: String,
    has_data: bool,
    data: String,
    frame_bytes: usize,
    after_cr: bool,
}

impl Parser {
    fn feed(&mut self, bytes: &[u8], mut emit: impl FnMut(Notice)) -> Result<()> {
        for &byte in bytes {
            if self.after_cr && byte == b'\n' {
                self.after_cr = false;
                continue;
            }
            self.after_cr = byte == b'\r';
            self.frame_bytes += 1;
            if self.frame_bytes > FRAME_LIMIT {
                return Err(Error::Decode("SSE frame exceeds 64 KiB".into()));
            }
            if byte == b'\r' || byte == b'\n' {
                self.line(&mut emit)?;
                self.line.clear();
            } else {
                self.line.push(byte);
            }
        }
        Ok(())
    }

    fn line(&mut self, emit: &mut impl FnMut(Notice)) -> Result<()> {
        let line = std::str::from_utf8(&self.line).map_err(|_| Error::Decode("invalid SSE UTF-8".into()))?;
        if line.is_empty() {
            // SSE dispatches only terminated frames. EOF discards an unfinished frame;
            // reconnecting will authoritatively refresh anything that was missed.
            if self.has_data {
                match self.event.as_str() {
                    "new_mail" => emit(Notice::Mail),
                    "outbox_update" => {
                        emit(Notice::Mail);
                        if let Ok(mut update) = serde_json::from_str::<super::api::OutboxUpdate>(&self.data) {
                            if matches!(update.status.as_str(), "queued" | "sent" | "failed" | "reconciliation_required" | "delivered_with_warning") {
                                update.error = update.error.chars().take(300).collect();
                                emit(Notice::Outbox(update));
                            }
                        }
                    }
                    "calendar_updated" => emit(Notice::Calendar),
                    _ => {}
                }
            }
            self.event.clear();
            self.has_data = false;
            self.data.clear();
            self.frame_bytes = 0;
        } else if !line.starts_with(':') {
            let (field, value) = line.split_once(':').unwrap_or((line, ""));
            let value = value.strip_prefix(' ').unwrap_or(value);
            match field {
                "event" => {
                    self.event.clear();
                    self.event.push_str(value);
                }
                "data" => {
                    // SSE fields can arrive in any order. Frame size bounds this
                    // multiline JSON; only outbox id/status/error survive dispatch.
                    if self.has_data { self.data.push('\n'); }
                    self.data.push_str(value);
                    self.has_data = true;
                }
                "retry" if !value.is_empty() && value.bytes().all(|b| b.is_ascii_digit()) => {
                    if let Ok(ms) = value.parse::<u64>() {
                        emit(Notice::Retry(Duration::from_millis(ms).clamp(RETRY_MIN, RETRY_MAX)));
                    }
                }
                _ => {} // Comments and unknown fields never dispatch or split a frame.
            }
        }
        Ok(())
    }
}

#[derive(Default)]
struct Burst {
    times: Option<(Instant, Instant)>,
}

impl Burst {
    fn mark(&mut self, now: Instant) {
        self.times = Some((self.times.map_or(now, |(first, _)| first), now));
    }

    fn take(&mut self, now: Instant, force: bool) -> bool {
        let ready = self.times.is_some_and(|(first, last)| force || now - last >= DEBOUNCE || now - first >= BURST_MAX);
        if ready {
            self.times = None;
        }
        ready
    }
}

#[derive(Default)]
struct Pending {
    mail: Burst,
    calendar: Burst,
    connected: bool,
    failure: Option<Error>,
    outbox: VecDeque<super::api::OutboxUpdate>,
}

impl Pending {
    fn outbox(&mut self, update: super::api::OutboxUpdate) {
        if let Some(ix) = self.outbox.iter().position(|old| old.item_id == update.item_id) { self.outbox.remove(ix); }
        if self.outbox.len() == 64 { self.outbox.pop_front(); }
        self.outbox.push_back(update);
    }
}

#[derive(Default)]
pub struct Changes {
    pub mail: bool,
    pub calendar: bool,
    pub connected: bool,
    pub failure: Option<Error>,
    pub outbox: Vec<super::api::OutboxUpdate>,
}

struct State {
    stop: Arc<AtomicBool>,
    pending: Mutex<Pending>,
    wake: Condvar,
}

/// The handle owns the only worker. Dropping it cancels reads/backoff and joins it;
/// no detached readers or unbounded event queue survive the Live entity.
pub struct Worker {
    state: Arc<State>,
    thread: Option<JoinHandle<()>>,
}

impl Worker {
    pub fn start(client: Arc<Client>) -> Result<Self> {
        let state = Arc::new(State { stop: Arc::new(AtomicBool::new(false)), pending: Mutex::new(Pending::default()), wake: Condvar::new() });
        let run = state.clone();
        let thread = std::thread::Builder::new().name("durian-events".into()).spawn(move || run.run(&client))
            .map_err(|_| Error::Unreachable("couldn't start SSE worker".into()))?;
        Ok(Self { state, thread: Some(thread) })
    }

    /// Called on GPUI's timer, never waits for network I/O. Bursts debounce, with a
    /// maximum wait so a busy mailbox doesn't starve refresh forever.
    pub fn changes(&self) -> Changes {
        let mut pending = self.state.pending.lock().unwrap();
        let now = Instant::now();
        let connected = std::mem::take(&mut pending.connected);
        Changes {
            mail: pending.mail.take(now, connected) || connected,
            calendar: pending.calendar.take(now, connected) || connected,
            connected,
            failure: pending.failure.take(),
            outbox: pending.outbox.drain(..).collect(),
        }
    }
}

impl Drop for Worker {
    fn drop(&mut self) {
        // Lock around notifying so cancellation can't race entry into the backoff wait.
        let guard = self.state.pending.lock().unwrap();
        self.state.stop.store(true, Ordering::Relaxed);
        self.state.wake.notify_all();
        drop(guard);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

impl State {
    fn run(&self, client: &Client) {
        let mut retry = RETRY_MIN;
        let mut delay = retry;
        while !self.stop.load(Ordering::Relaxed) {
            let started = Instant::now();
            let result = self.read(client, &mut retry);
            if self.stop.load(Ordering::Relaxed) {
                break;
            }
            let mut pending = self.pending.lock().unwrap();
            pending.failure = result.err();
            // Short-lived 200/EOF streams must back off too, not reset on every header.
            if started.elapsed() >= RETRY_MAX {
                delay = retry;
            }
            delay = delay.max(retry);
            let (guard, _) = self.wake.wait_timeout_while(pending, delay, |_| !self.stop.load(Ordering::Relaxed)).unwrap();
            drop(guard);
            delay = (delay * 2).min(RETRY_MAX);
        }
    }

    fn read(&self, client: &Client, retry: &mut Duration) -> Result<()> {
        let mut reader = client.event_stream(self.stop.clone())?;
        self.pending.lock().unwrap().connected = true;
        let mut parser = Parser::default();
        let mut bytes = [0; 4096];
        while !self.stop.load(Ordering::Relaxed) {
            let n = reader.read(&mut bytes).map_err(|_| Error::Unreachable("SSE read ended".into()))?;
            if n == 0 {
                return Ok(());
            }
            parser.feed(&bytes[..n], |notice| {
                let mut pending = self.pending.lock().unwrap();
                match notice {
                    Notice::Mail => pending.mail.mark(Instant::now()),
                    Notice::Calendar => pending.calendar.mark(Instant::now()),
                    Notice::Outbox(update) => pending.outbox(update),
                    Notice::Retry(value) => *retry = value,
                }
            })?;
        }
        Ok(())
    }
}

/// Poll below TLS/HTTP decoding: a socket timeout is retried here, never exposed to
/// the decoders as EOF or a failed partial frame. A silent peer is retried after 45s.
/// Uses ureq's unversioned connector API (covered by loopback transport tests).
pub(super) fn agent(stop: Arc<AtomicBool>) -> ureq::Agent {
    let config = ureq::Agent::config_builder()
        .proxy(None) // durian is a direct server, not an environment HTTP proxy.
        .http_status_as_error(false)
        .max_redirects(0)
        .max_idle_connections(0)
        .timeout_resolve(Some(Duration::from_secs(3)))
        .timeout_connect(Some(Duration::from_secs(3)))
        .timeout_send_request(Some(Duration::from_secs(3)))
        .build();
    let connector = ().chain(TcpConnector::default()).chain(super::transport::InterruptedConnector).chain(PollConnector(stop)).chain(RustlsConnector::default());
    ureq::Agent::with_parts(config, connector, DefaultResolver::default())
}

#[derive(Debug)]
struct PollConnector(Arc<AtomicBool>);

#[derive(Debug)]
struct PollTransport<T> {
    inner: T,
    stop: Arc<AtomicBool>,
}

fn check(stop: &AtomicBool) -> std::result::Result<(), ureq::Error> {
    if stop.load(Ordering::Relaxed) {
        Err(ureq::Error::Io(io::Error::other("SSE cancelled")))
    } else {
        Ok(())
    }
}

impl<T: Transport> Connector<T> for PollConnector {
    type Out = PollTransport<T>;

    fn connect(&self, _: &ConnectionDetails, chained: Option<T>) -> std::result::Result<Option<Self::Out>, ureq::Error> {
        check(&self.0)?;
        Ok(chained.map(|inner| PollTransport { inner, stop: self.0.clone() }))
    }
}

impl<T: Transport> Transport for PollTransport<T> {
    fn buffers(&mut self) -> &mut dyn Buffers {
        self.inner.buffers()
    }

    fn transmit_output(&mut self, amount: usize, timeout: NextTimeout) -> std::result::Result<(), ureq::Error> {
        check(&self.stop)?;
        self.inner.transmit_output(amount, timeout)
    }

    fn maybe_await_input(&mut self, timeout: NextTimeout) -> std::result::Result<bool, ureq::Error> {
        check(&self.stop)?;
        if self.inner.buffers().can_use_input() { Ok(true) } else { self.await_input(timeout) }
    }

    fn await_input(&mut self, timeout: NextTimeout) -> std::result::Result<bool, ureq::Error> {
        let started = Instant::now();
        let budget = match timeout.after {
            TransportDuration::NotHappening => IDLE,
            TransportDuration::Exact(budget) => budget.min(IDLE),
        };
        loop {
            check(&self.stop)?;
            let left = budget.saturating_sub(started.elapsed());
            if left.is_zero() {
                return Err(ureq::Error::Timeout(timeout.reason));
            }
            let slice = NextTimeout { after: left.min(POLL).into(), reason: timeout.reason };
            match self.inner.await_input(slice) {
                Err(ureq::Error::Timeout(_)) => continue,
                result => {
                    check(&self.stop)?;
                    return result;
                }
            }
        }
    }

    fn is_open(&mut self) -> bool {
        !self.stop.load(Ordering::Relaxed) && self.inner.is_open()
    }

    fn is_tls(&self) -> bool {
        self.inner.is_tls()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::net::{TcpListener, TcpStream};
    use std::sync::mpsc;

    fn listener() -> (TcpListener, Arc<Client>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client = Arc::new(Client::new(&format!("http://{}", listener.local_addr().unwrap()), Some("test-token".into())));
        (listener, client)
    }

    fn accept(listener: &TcpListener) -> TcpStream {
        listener.set_nonblocking(true).unwrap();
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            match listener.accept() {
                Ok((stream, _)) => {
                    stream.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
                    stream.set_write_timeout(Some(Duration::from_secs(3))).unwrap();
                    let mut reader = BufReader::new(stream.try_clone().unwrap());
                    let mut request = String::new();
                    loop {
                        let mut line = String::new();
                        assert_ne!(reader.read_line(&mut line).unwrap(), 0);
                        if line == "\r\n" { break }
                        request.push_str(&line);
                    }
                    assert!(request.starts_with("GET /api/v1/events HTTP/1.1\r\n"), "{request}");
                    assert!(request.to_lowercase().contains("authorization: bearer test-token\r\n"));
                    assert!(request.to_lowercase().contains("accept: text/event-stream\r\n"));
                    assert!(!request.to_lowercase().contains("last-event-id:"), "Go has no replay");
                    return stream;
                }
                Err(e) if e.kind() == io::ErrorKind::WouldBlock && Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
                Err(e) => panic!("accept: {e}"),
            }
        }
    }

    fn headers(stream: &mut TcpStream) {
        stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: Text/Event-Stream; charset=utf-8\r\nTransfer-Encoding: chunked\r\n\r\n").unwrap();
    }

    fn chunk(stream: &mut TcpStream, data: &[u8]) {
        write!(stream, "{:X}\r\n", data.len()).unwrap();
        stream.write_all(data).unwrap();
        stream.write_all(b"\r\n").unwrap();
    }

    fn wait(worker: &Worker, predicate: impl Fn(&Changes) -> bool) -> Changes {
        let deadline = Instant::now() + Duration::from_secs(4);
        loop {
            let changes = worker.changes();
            if predicate(&changes) { return changes }
            assert!(Instant::now() < deadline, "freshness notice didn't arrive");
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    #[test]
    fn stream_requires_authenticated_200_and_event_content_type() {
        for (status, content_type) in [(401, "text/event-stream"), (403, "text/event-stream"), (404, "text/plain"), (500, "application/json"), (204, "text/event-stream"), (302, "text/event-stream"), (200, "application/json")] {
            let (listener, client) = listener();
            let server = std::thread::spawn(move || {
                let mut stream = accept(&listener);
                write!(stream, "HTTP/1.1 {status} X\r\nContent-Type: {content_type}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
            });
            let result = client.event_stream(Arc::new(AtomicBool::new(false)));
            match status {
                401 | 403 => assert!(matches!(result, Err(Error::Unauthorized))),
                404 => assert!(matches!(result, Err(Error::NotFound(_)))),
                200 => assert!(matches!(result, Err(Error::Decode(_)))),
                _ => assert!(result.is_err(), "status {status} isn't an SSE stream"),
            }
            server.join().unwrap();
        }
    }

    #[test]
    fn chunked_frames_handle_crlf_multiline_comments_and_debounce_bursts() {
        let (listener, client) = listener();
        let (send, receive) = mpsc::channel::<Vec<u8>>();
        let (written, writes) = mpsc::channel();
        let server = std::thread::spawn(move || {
            let mut stream = accept(&listener);
            headers(&mut stream);
            for frame in receive {
                // Split CRLF, fields and UTF-8 across HTTP chunks too.
                for byte in frame { chunk(&mut stream, &[byte]); }
                written.send(()).unwrap();
            }
        });
        let worker = Worker::start(client).unwrap();
        let connected = wait(&worker, |c| c.connected);
        assert!(connected.mail && connected.calendar, "first connection refreshes both views");
        send.send(b"event: new_mail\r\ndata: {\r\n: comment isn't a delimiter\r\ndata: \"account\":\"w\xC3\xB6rk\"}\r\n".to_vec()).unwrap();
        writes.recv_timeout(Duration::from_secs(3)).unwrap();
        std::thread::sleep(Duration::from_millis(650));
        let changes = worker.changes();
        assert!(!changes.mail && !changes.calendar && !changes.connected, "comments must not dispatch an unfinished frame");
        send.send(b"\r\n".to_vec()).unwrap();
        let mail = wait(&worker, |c| c.mail);
        assert!(!mail.calendar && !mail.connected);
        writes.recv_timeout(Duration::from_secs(3)).unwrap();

        // Hundreds of new-mail/outbox events are one coalesced refresh.
        let burst = b"event: new_mail\ndata: {}\n\nevent: outbox_update\ndata: {}\n\n".repeat(200);
        // Send this large burst as one chunk; the byte-split coverage is above.
        drop(send);
        drop(worker);
        server.join().unwrap();
        burst_test(burst);
    }

    fn burst_test(burst: Vec<u8>) {
        let (listener, client) = listener();
        let (send, receive) = mpsc::channel::<Vec<u8>>();
        let (written, writes) = mpsc::channel();
        let server = std::thread::spawn(move || {
            let mut stream = accept(&listener);
            headers(&mut stream);
            for bytes in receive {
                chunk(&mut stream, &bytes);
                written.send(()).unwrap();
            }
        });
        let worker = Worker::start(client).unwrap();
        wait(&worker, |c| c.connected);
        send.send(burst).unwrap();
        writes.recv_timeout(Duration::from_secs(3)).unwrap();
        std::thread::sleep(Duration::from_millis(200));
        assert!(!worker.changes().mail, "refresh waits for the burst to settle");
        let changes = wait(&worker, |c| c.mail);
        assert!(!changes.calendar && !changes.connected);
        std::thread::sleep(Duration::from_millis(600));
        assert!(!worker.changes().mail, "a burst produces only one refresh");
        send.send(b"event: calendar_updated\rdata: {}\r\revent: unknown\ndata: {}\n\ndata: {}\n\n".to_vec()).unwrap();
        writes.recv_timeout(Duration::from_secs(3)).unwrap();
        let changes = wait(&worker, |c| c.calendar);
        assert!(!changes.mail, "calendar hints don't refresh mail; unknown/default types are ignored");
        drop(worker);
        drop(send);
        server.join().unwrap();
    }

    #[test]
    fn outbox_stream_keeps_multiline_status_and_bounds_coalesced_payloads() {
        let (listener, client) = listener();
        let (send, receive) = mpsc::channel();
        let server = std::thread::spawn(move || {
            let mut stream = accept(&listener); headers(&mut stream);
            receive.recv().unwrap();
            let mut burst = String::new();
            for id in 0..80 {
                burst.push_str(&format!("event: outbox_update\r\ndata: {{\"item_id\":{id},\r\n: comment\r\ndata: \"status\":\"failed\",\"error\":\"{}\",\"subject\":\"discard private subject\"}}\r\n\r\n", "e".repeat(400)));
            }
            burst.push_str("event: outbox_update\ndata: {\"item_id\":79,\"status\":\"reconciliation_required\"}\n\n");
            burst.push_str("event: outbox_update\ndata: {\"item_id\":80,\"status\":\"delivered_with_warning\",\"error\":\"Sent filing failed\"}\n\n");
            chunk(&mut stream, burst.as_bytes());
            receive.recv().unwrap();
        });
        let worker = Worker::start(client).unwrap(); wait(&worker, |c| c.connected);
        send.send(()).unwrap();
        // Mail refresh is debounced until parsing the complete burst; status
        // payloads remain pending for this single public drain.
        std::thread::sleep(Duration::from_millis(700));
        let changes = worker.changes();
        assert!(changes.mail);
        assert_eq!(changes.outbox.len(), 64);
        assert_eq!(changes.outbox[0].item_id, 17);
        assert_eq!(changes.outbox[0].error.len(), 300);
        assert_eq!(changes.outbox[62].item_id, 79);
        assert_eq!(changes.outbox[62].status, "reconciliation_required");
        assert_eq!(changes.outbox[63].status, "delivered_with_warning");
        assert_eq!(changes.outbox[63].error, "Sent filing failed");
        assert!(worker.changes().outbox.is_empty());
        drop(worker); send.send(()).unwrap(); server.join().unwrap();
    }

    #[test]
    fn every_reconnect_refreshes_without_events_and_short_eof_backs_off() {
        let (listener, client) = listener();
        let (send, receive) = mpsc::channel();
        let (accepted, accepts) = mpsc::channel();
        let server = std::thread::spawn(move || {
            for attempt in 0..3 {
                let mut stream = accept(&listener);
                accepted.send(Instant::now()).unwrap();
                if attempt == 1 {
                    receive.recv_timeout(Duration::from_secs(4)).unwrap();
                }
                headers(&mut stream);
                receive.recv_timeout(Duration::from_secs(4)).unwrap();
                // Truncated frames are not dispatched on EOF and don't carry into retry.
                if attempt == 0 { chunk(&mut stream, b"event: new_mail\ndata: unfinished"); }
                stream.write_all(b"0\r\n\r\n").unwrap();
            }
        });
        let worker = Worker::start(client).unwrap();
        let mut last = None;
        for attempt in 0..3 {
            let at = accepts.recv_timeout(Duration::from_secs(4)).unwrap();
            if let Some(last) = last {
                assert!(at - last >= Duration::from_millis(if attempt == 1 { 240 } else { 490 }), "200/EOF must not reset backoff");
            }
            if attempt == 1 {
                // Keep the second response waiting for headers so a mistaken EOF
                // dispatch from the previous stream can't hide under its refresh.
                std::thread::sleep(Duration::from_millis(650));
                let changes = worker.changes();
                assert!(!changes.mail && !changes.calendar && !changes.connected, "EOF must discard an unfinished frame");
                send.send(()).unwrap();
            }
            let changes = wait(&worker, |c| c.connected);
            assert!(changes.mail && changes.calendar, "every reconnect is a freshness boundary");
            assert!(!worker.changes().connected);
            send.send(()).unwrap();
            last = Some(at);
        }
        drop(worker);
        server.join().unwrap();
    }

    #[test]
    fn dropping_worker_cancels_before_headers_and_during_body_reads() {
        for send_headers in [false, true] {
            let (listener, client) = listener();
            let (accepted, accepts) = mpsc::channel();
            let server = std::thread::spawn(move || {
                let mut stream = accept(&listener);
                if send_headers { headers(&mut stream); }
                accepted.send(()).unwrap();
                let mut byte = [0];
                assert_eq!(stream.read(&mut byte).unwrap(), 0, "cancellation closes the owned socket");
                assert!(matches!(listener.accept(), Err(e) if e.kind() == io::ErrorKind::WouldBlock), "no duplicate readers");
            });
            let worker = Worker::start(client).unwrap();
            accepts.recv_timeout(Duration::from_secs(3)).unwrap();
            if send_headers { wait(&worker, |c| c.connected); }
            let started = Instant::now();
            drop(worker);
            assert!(started.elapsed() < Duration::from_secs(1), "silent reads must cancel promptly");
            server.join().unwrap();
        }
    }

    #[test]
    fn oversized_unterminated_frame_reconnects_instead_of_growing_memory() {
        // A bound on single lines alone misses an oversized multiline frame.
        for oversized in [vec![b'x'; FRAME_LIMIT + 1], b"data: x\n".repeat(FRAME_LIMIT / 8 + 1)] {
            let (listener, client) = listener();
            let (send, receive) = mpsc::channel();
            let server = std::thread::spawn(move || {
                let mut stream = accept(&listener);
                headers(&mut stream);
                receive.recv_timeout(Duration::from_secs(3)).unwrap();
                chunk(&mut stream, &oversized);
                drop(stream);
                let mut stream = accept(&listener);
                headers(&mut stream);
                receive.recv_timeout(Duration::from_secs(3)).unwrap();
            });
            let worker = Worker::start(client).unwrap();
            wait(&worker, |c| c.connected);
            send.send(()).unwrap();
            let changes = wait(&worker, |c| c.failure.is_some());
            assert!(matches!(changes.failure, Some(Error::Decode(_))));
            let changes = wait(&worker, |c| c.connected);
            assert!(changes.mail && changes.calendar);
            drop(worker);
            send.send(()).unwrap();
            server.join().unwrap();
        }
    }

    #[test]
    fn rejected_stream_retries_and_backoff_is_cancellable() {
        for recover in [false, true] {
            let (listener, client) = listener();
            let server = std::thread::spawn(move || {
                let mut stream = accept(&listener);
                stream.write_all(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                drop(stream);
                if recover {
                    let mut stream = accept(&listener);
                    headers(&mut stream);
                    assert_eq!(stream.read(&mut [0]).unwrap(), 0);
                }
                // Wait past the first retry: cancellation must not open another socket.
                std::thread::sleep(Duration::from_millis(650));
                assert!(matches!(listener.accept(), Err(e) if e.kind() == io::ErrorKind::WouldBlock));
            });
            let worker = Worker::start(client).unwrap();
            let changes = wait(&worker, |c| c.failure.is_some());
            assert!(matches!(changes.failure, Some(Error::Unauthorized)));
            assert!(!changes.connected && !changes.mail && !changes.calendar);
            if recover {
                let changes = wait(&worker, |c| c.connected);
                assert!(changes.mail && changes.calendar);
            }
            let started = Instant::now();
            drop(worker);
            assert!(started.elapsed() < Duration::from_secs(1));
            server.join().unwrap();
        }
    }

    #[test]
    fn continuous_burst_has_a_maximum_debounce_and_retry_is_clamped() {
        let start = Instant::now();
        let mut burst = Burst::default();
        burst.mark(start);
        burst.mark(start + Duration::from_millis(1900));
        assert!(!burst.take(start + Duration::from_millis(1999), false));
        assert!(burst.take(start + Duration::from_millis(2000), false));
        assert!(!burst.take(start + Duration::from_millis(2500), false));
        let mut notices = Vec::new();
        Parser::default().feed(b"retry: 1\nretry: 999999\nretry: -1\nretry: nope\n\n", |n| notices.push(n)).unwrap();
        assert_eq!(notices, [Notice::Retry(RETRY_MIN), Notice::Retry(RETRY_MAX)]);
    }
}
