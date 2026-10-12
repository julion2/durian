//! Ownership of a durian serve process, separate from the read-only HTTP client.
//! Never inspect or terminate a process belonging to MAIL_BACKEND.

use std::ffi::OsString;
use std::io::Read;
use std::net::{Ipv4Addr, SocketAddr, TcpListener};
use std::path::{Path, PathBuf};
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex, atomic::{AtomicBool, Ordering}, mpsc};
use std::time::{Duration, Instant};

const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);
const ATTEMPTS: usize = 3;
const LINE_LIMIT: usize = 16 * 1024;
const RESTART_BACKOFF: [Duration; 3] = [Duration::from_millis(250), Duration::from_millis(500), Duration::from_secs(1)];
const POLL: Duration = Duration::from_millis(20);

// Deliberately not Debug: neither the handshake nor the connection may log a token.
pub struct Connection {
    pub server: String,
    pub token: Option<String>,
}

pub enum Source {
    External(Connection),
    Owned(OsString),
}

pub enum Event {
    Connected(Connection),
    Reconnecting { attempt: usize },
    Failed(Error),
}

impl Source {
    pub fn from_env() -> Result<Self, Error> {
        if let Some(server) = std::env::var("MAIL_BACKEND").ok().filter(|s| !s.trim().is_empty()) {
            return Ok(Self::External(Connection {
                server,
                token: std::env::var("MAIL_TOKEN").ok().filter(|s| !s.trim().is_empty()),
            }));
        }
        let executable = std::env::current_exe().map_err(|_| Error::Spawn)?;
        resolve_binary(std::env::var_os("DURIAN_BIN").filter(|s| !s.is_empty()), &executable, std::env::var_os("PATH"))
            .map(Self::Owned)
    }
}

fn resolve_binary(explicit: Option<OsString>, executable: &Path, path: Option<OsString>) -> Result<OsString, Error> {
    let is_executable = |candidate: &Path| candidate.metadata().is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o111 != 0);
    // Canonical paths catch symlinks; inode identity also catches hard links to the GUI.
    let is_self = |candidate: &Path| {
        candidate.canonicalize().ok().zip(executable.canonicalize().ok()).is_some_and(|(a, b)| a == b)
            || candidate.metadata().ok().zip(executable.metadata().ok()).is_some_and(|(a, b)| a.dev() == b.dev() && a.ino() == b.ino())
    };
    let paths: Vec<_> = path.as_deref().map(std::env::split_paths).into_iter().flatten().collect();
    if let Some(explicit) = explicit {
        let candidate = PathBuf::from(explicit);
        let candidate = if candidate.components().count() == 1 {
            paths.iter().map(|dir| dir.join(&candidate)).find(|p| is_executable(p)).ok_or(Error::Spawn)?
        } else { candidate };
        if is_self(&candidate) { return Err(Error::SelfExecutable); }
        return if is_executable(&candidate) { Ok(candidate.into_os_string()) } else { Err(Error::Spawn) };
    }
    let siblings = executable.parent().into_iter().flat_map(|dir| [dir.join("durian-cli"), dir.join("durian")]);
    siblings.chain(paths.iter().map(|dir| dir.join("durian")))
        .find(|candidate| is_executable(candidate) && !is_self(candidate))
        .map(PathBuf::into_os_string).ok_or(Error::Spawn)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    Spawn,
    Port,
    Timeout,
    Eof,
    Protocol,
    Address,
    Token,
    Output,
    Exited,
    Cancelled,
    RestartLimit,
    SelfExecutable,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Spawn => "Could not start durian. Set DURIAN_BIN or install durian on PATH.",
            Self::Port => "Could not select a free loopback port for durian.",
            Self::Timeout => "Timed out waiting for durian READY. Check Durian's serve.log.",
            Self::Eof => "Durian closed stdout before READY. Check Durian's serve.log.",
            Self::Protocol => "Incompatible durian READY protocol. This app requires API 1.",
            Self::Address => "Durian READY reported an invalid or unexpected loopback address.",
            Self::Token => "Durian READY did not provide a valid authentication token.",
            Self::Output => "Durian startup output was invalid or exceeded the line limit.",
            Self::Exited => "Durian exited during startup. Check Durian's serve.log.",
            Self::Cancelled => "Durian startup was cancelled.",
            Self::RestartLimit => "Durian server restart limit reached. Check serve.log, then quit and reopen Durian.",
            Self::SelfExecutable => "DURIAN_BIN points to the Durian GUI itself. Select the durian CLI executable instead.",
        })
    }
}

#[derive(Clone, Default)]
pub struct Backend(Arc<Owner>);

#[derive(Default)]
struct Owner {
    child: Mutex<Option<Child>>,
    cancelled: AtomicBool,
}

impl Owner {
    fn reap(&self) {
        if let Some(mut child) = self.child.lock().unwrap().take() {
            // Only the child we spawned. wait also reaps an already-exited child.
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

impl Drop for Owner {
    fn drop(&mut self) {
        self.reap();
    }
}

impl Backend {
    /// Idempotent and safe while the startup worker is waiting for READY.
    pub fn stop(&self) {
        // Serialize cancellation with event delivery and spawning: no preconnect may
        // begin once shutdown has acquired the owner lock.
        let mut child = self.0.child.lock().unwrap();
        self.0.cancelled.store(true, Ordering::Release);
        if let Some(mut child) = child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }

    pub fn stopped(&self) -> bool { self.0.cancelled.load(Ordering::Acquire) }

    /// Run on the original startup worker. External connections are delivered once;
    /// only owned children are monitored/restarted. The lifetime budget never resets.
    pub fn run(&self, source: Source, mut emit: impl FnMut(Event) -> bool) {
        let binary = match source {
            Source::External(connection) => {
                if !self.publish(&mut emit, Event::Connected(connection)) { self.stop(); }
                return;
            }
            Source::Owned(binary) => binary,
        };
        let connection = match self.start(&binary, HANDSHAKE_TIMEOUT) {
            Ok(connection) => connection,
            Err(error) => { self.publish(&mut emit, Event::Failed(error)); return; }
        };
        if !self.publish(&mut emit, Event::Connected(connection)) { self.stop(); return; }
        let mut attempt = 0;
        loop {
            if !self.wait_for_exit() { return; }
            eprintln!("[BACKEND] owned durian exited; replacing server session");
            loop {
                let Some(delay) = RESTART_BACKOFF.get(attempt) else {
                    self.publish(&mut emit, Event::Failed(Error::RestartLimit));
                    return;
                };
                attempt += 1;
                if !self.publish(&mut emit, Event::Reconnecting { attempt }) { self.stop(); return; }
                if !self.pause(*delay) { return; }
                match self.start(&binary, HANDSHAKE_TIMEOUT) {
                    Ok(connection) => {
                        if !self.publish(&mut emit, Event::Connected(connection)) { self.stop(); return; }
                        break;
                    }
                    Err(Error::Cancelled) => return,
                    Err(error) => eprintln!("[BACKEND] replacement attempt {attempt} failed: {error}"),
                }
            }
        }
    }

    // emit may create preconnect/send a UI update, but must not reenter Backend.
    fn publish(&self, emit: &mut impl FnMut(Event) -> bool, event: Event) -> bool {
        let _child = self.0.child.lock().unwrap();
        !self.stopped() && emit(event)
    }

    fn pause(&self, delay: Duration) -> bool {
        let deadline = Instant::now() + delay;
        while !self.stopped() {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() { return true; }
            std::thread::sleep(POLL.min(remaining));
        }
        false
    }

    fn wait_for_exit(&self) -> bool {
        loop {
            {
                let mut child = self.0.child.lock().unwrap();
                if self.stopped() { return false; }
                if !child.as_mut().is_some_and(|child| matches!(child.try_wait(), Ok(None))) {
                    if let Some(mut child) = child.take() {
                        let _ = child.kill();
                        let _ = child.wait();
                    }
                    return true;
                }
            }
            if !self.pause(Duration::from_millis(100)) { return false; }
        }
    }

    // Single-launch contract exercised separately from the supervisor.
    #[cfg(test)]
    pub fn connect(&self, source: Source) -> Result<Connection, Error> {
        if self.0.cancelled.load(Ordering::Acquire) { return Err(Error::Cancelled); }
        match source {
            Source::External(connection) => Ok(connection),
            Source::Owned(binary) => self.start(&binary, HANDSHAKE_TIMEOUT),
        }
    }

    fn start(&self, binary: &OsString, timeout: Duration) -> Result<Connection, Error> {
        let deadline = Instant::now() + timeout;
        for attempt in 0..ATTEMPTS {
            // Go advertises the requested addr rather than listener.Addr(), so --port 0
            // yields unusable READY. Release a free port immediately before spawning;
            // retry the small bind race, never kill an existing listener.
            if self.0.cancelled.load(Ordering::Acquire) { return Err(Error::Cancelled); }
            let port = TcpListener::bind((Ipv4Addr::LOCALHOST, 0))
                .and_then(|listener| listener.local_addr()).map_err(|_| Error::Port)?.port();
            let result = self.spawn_and_wait(binary, port, deadline);
            match result {
                Ok(connection) => return Ok(connection),
                Err((error, conflict)) => {
                    self.0.reap();
                    if !conflict || error == Error::Cancelled || attempt + 1 == ATTEMPTS || Instant::now() >= deadline {
                        return Err(error);
                    }
                    eprintln!("[BACKEND] durian could not bind; retrying on a new loopback port");
                }
            }
        }
        unreachable!()
    }

    fn spawn_and_wait(&self, binary: &OsString, port: u16, deadline: Instant) -> Result<Connection, (Error, bool)> {
        let (tx, rx) = mpsc::channel();
        let conflict = Arc::new(AtomicBool::new(false));
        let (stdout, stderr) = {
            let mut owned = self.0.child.lock().unwrap();
            if self.0.cancelled.load(Ordering::Acquire) {
                return Err((Error::Cancelled, false));
            }
            let mut child = Command::new(binary)
                .args(["serve", "--port", &port.to_string(), "--exit-when-orphaned"])
                .stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped())
                .spawn().map_err(|error| {
                    eprintln!("[BACKEND] could not spawn durian (kind {:?}, OS code {:?})", error.kind(), error.raw_os_error());
                    (Error::Spawn, false)
                })?;
            let pipes = (child.stdout.take().unwrap(), child.stderr.take().unwrap());
            *owned = Some(child);
            pipes
        };
        eprintln!("[BACKEND] started owned durian server; waiting for API 1 READY");
        // Drain both pipes for the whole process lifetime, including after READY. Never
        // forward raw output: even an old/malformed server could print its token there.
        let stderr_conflict = conflict.clone();
        let (stderr_done_tx, stderr_done_rx) = mpsc::channel();
        std::thread::spawn(move || {
            let mut reported = false;
            drain_lines(stderr, |line| {
                if line.windows(b"could not listen on".len()).any(|w| w == b"could not listen on") {
                    stderr_conflict.store(true, Ordering::Release);
                }
                if !reported {
                    eprintln!("[BACKEND] durian wrote stderr; consult Durian's serve.log (output suppressed)");
                    reported = true;
                }
            });
            let _ = stderr_done_tx.send(());
        });
        std::thread::spawn(move || {
            let mut ready = false;
            let mut reported = false;
            drain_lines(stdout, |line| {
                if ready { return; }
                let parsed = std::str::from_utf8(line).map_err(|_| Error::Output)
                    .and_then(|line| parse_ready(line, port));
                match parsed {
                    Ok(None) => {
                        if !reported {
                            eprintln!("[BACKEND] durian wrote startup output (suppressed)");
                            reported = true;
                        }
                    }
                    result => {
                        ready = true;
                        let _ = tx.send(result.and_then(|c| c.ok_or(Error::Output)));
                    }
                }
            });
            // A failed child can close stdout before its stderr reader has observed the
            // bind failure. Allow a bounded drain, never join a possibly still-open pipe.
            if !ready {
                let _ = stderr_done_rx.recv_timeout(Duration::from_millis(50));
                let _ = tx.send(Err(Error::Eof));
            }
        });
        loop {
            let error = if self.0.cancelled.load(Ordering::Acquire) {
                Some(Error::Cancelled)
            } else if Instant::now() >= deadline {
                Some(Error::Timeout)
            } else {
                match rx.recv_timeout(Duration::from_millis(20)) {
                    Ok(Ok(connection)) => {
                        let mut owned = self.0.child.lock().unwrap();
                        if owned.as_mut().is_some_and(|child| matches!(child.try_wait(), Ok(None))) {
                            eprintln!("[BACKEND] owned durian server ready");
                            return Ok(connection);
                        }
                        Some(Error::Exited)
                    }
                    Ok(Err(error)) => Some(error),
                    Err(mpsc::RecvTimeoutError::Disconnected) => Some(Error::Eof),
                    Err(mpsc::RecvTimeoutError::Timeout) => None,
                }
            };
            if let Some(error) = error {
                // Quit can close the pipe during recv_timeout; cancellation wins over EOF.
                let error = if self.0.cancelled.load(Ordering::Acquire) { Error::Cancelled } else { error };
                return Err((error, conflict.load(Ordering::Acquire)));
            }
        }
    }
}

/// Buffer split reads, accept CRLF, and bound memory even for a child flooding stdout.
/// An overlong or unterminated line is reported as invalid, never partially parsed.
fn drain_lines(mut pipe: impl Read, mut line: impl FnMut(&[u8])) {
    let mut chunk = [0; 4096];
    let mut buffered = Vec::new();
    let mut overflow = false;
    loop {
        let count = match pipe.read(&mut chunk) {
            Ok(0) | Err(_) => {
                if overflow || !buffered.is_empty() { line(&[0xff]); }
                return;
            }
            Ok(count) => count,
        };
        for byte in &chunk[..count] {
            if *byte == b'\n' {
                if overflow { line(&[0xff]); } else { line(&buffered); }
                buffered.clear();
                overflow = false;
            } else if buffered.len() < LINE_LIMIT {
                buffered.push(*byte);
            } else {
                overflow = true;
            }
        }
    }
}

fn parse_ready(line: &str, port: u16) -> Result<Option<Connection>, Error> {
    let line = line.trim_end_matches('\r');
    if line != "READY" && !line.starts_with("READY ") { return Ok(None); }
    let mut api = None;
    let mut addr = None;
    let mut token = None;
    for field in line.split_whitespace().skip(1) {
        let Some((key, value)) = field.split_once('=') else { return Err(Error::Protocol); };
        let slot = match key {
            "api" => &mut api,
            "addr" => &mut addr,
            "token" => &mut token,
            _ => continue,
        };
        if slot.replace(value).is_some() { return Err(Error::Protocol); }
    }
    if api != Some("1") { return Err(Error::Protocol); }
    let address: SocketAddr = addr.and_then(|a| a.parse().ok()).ok_or(Error::Address)?;
    if address.ip() != Ipv4Addr::LOCALHOST || address.port() != port || port == 0 {
        return Err(Error::Address);
    }
    let token = token.filter(|t| t.len() == 64 && t.bytes().all(|b| b.is_ascii_hexdigit()))
        .ok_or(Error::Token)?;
    Ok(Some(Connection { server: format!("http://{address}"), token: Some(token.into()) }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::os::unix::fs::PermissionsExt;
    use std::sync::atomic::AtomicUsize;

    const TOKEN: &str = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    // A concurrent fork can inherit another fixture's open writer until exec, making
    // execution of that freshly-written script fail with ETXTBSY on Linux.
    static PROCESSES: Mutex<()> = Mutex::new(());

    struct Server { root: PathBuf, binary: OsString }

    impl Server {
        fn new(body: &str) -> Self {
            let root = std::env::temp_dir().join(format!("durian-startup-test-{}-{}", std::process::id(), NEXT.fetch_add(1, Ordering::Relaxed)));
            fs::create_dir(&root).unwrap();
            let binary = root.join("durian");
            let path = root.display().to_string().replace('\'', "'\\''");
            fs::write(&binary, format!("#!/bin/sh\ncd '{path}' || exit 1\nprintf '%s\\n' \"$$\" >> pids\nprintf '%s\\n' \"$*\" >> args\ntoken={TOKEN}\n{body}\n")).unwrap();
            fs::set_permissions(&binary, fs::Permissions::from_mode(0o700)).unwrap();
            Self { root, binary: binary.into_os_string() }
        }

        fn pids(&self) -> Vec<String> {
            fs::read_to_string(self.root.join("pids")).unwrap_or_default().lines().map(str::to_string).collect()
        }

        fn assert_reaped(&self, attempts: usize) {
            let pids = self.pids();
            assert_eq!(pids.len(), attempts);
            for pid in pids {
                assert!(!Command::new("kill").args(["-0", &pid]).stderr(Stdio::null()).status().unwrap().success(), "child {pid} is still alive or a zombie");
            }
        }

        fn wait_started(&self) {
            let deadline = Instant::now() + Duration::from_secs(2);
            while self.pids().is_empty() {
                assert!(Instant::now() < deadline, "fake server never started");
                std::thread::sleep(Duration::from_millis(5));
            }
        }
    }

    impl Drop for Server {
        fn drop(&mut self) { let _ = fs::remove_dir_all(&self.root); }
    }

    fn success() -> &'static str {
        "printf 'boot token=%s\\n' \"$token\"\nprintf 'stderr token=%s\\n' \"$token\" >&2\nprintf 'READY token=%s addr=127.' \"$token\"\nsleep 0.03\nprintf '0.0.1:%s api=1\\r\\n' \"$3\"\nprintf '%131072s\\n' ''\nprintf '%131072s\\n' '' >&2\ntouch drained\nexec sleep 30"
    }

    #[test]
    fn split_ready_and_owned_quit_reap() {
        let _serial = PROCESSES.lock().unwrap();
        let server = Server::new(success());
        let backend = Backend::default();
        let connection = backend.connect(Source::Owned(server.binary.clone())).unwrap();
        assert!(connection.token.as_deref() == Some(TOKEN));
        let args = fs::read_to_string(server.root.join("args")).unwrap();
        let port: u16 = args.split_whitespace().nth(2).unwrap().parse().unwrap();
        assert_ne!(port, 0);
        assert_eq!(args.trim(), format!("serve --port {port} --exit-when-orphaned"));
        assert_eq!(connection.server, format!("http://127.0.0.1:{port}"));
        // Output continues after READY and must not fill a pipe and stall the child.
        let deadline = Instant::now() + Duration::from_secs(2);
        while !server.root.join("drained").exists() {
            assert!(Instant::now() < deadline, "stdout/stderr stalled after READY");
            std::thread::sleep(Duration::from_millis(5));
        }
        backend.stop();
        backend.stop();
        server.assert_reaped(1);
    }

    #[test]
    fn handshake_failures_reap_real_children() {
        let _serial = PROCESSES.lock().unwrap();
        let cases = [
            ("printf 'READY token=%s addr=127.0.0.1:%s api=2\\n' \"$token\" \"$3\"; exec sleep 30", Error::Protocol),
            ("printf 'READY token=%s addr=127.0.0.1:%s\\n' \"$token\" \"$3\"; exec sleep 30", Error::Protocol),
            ("printf 'READY token= addr=127.0.0.1:%s api=1\\n' \"$3\"; exec sleep 30", Error::Token),
            ("printf 'READY token=not-hex addr=127.0.0.1:%s api=1\\n' \"$3\"; exec sleep 30", Error::Token),
            ("printf 'READY addr=127.0.0.1:%s api=1\\n' \"$3\"; exec sleep 30", Error::Token),
            ("printf 'READY token=%s addr=0.0.0.0:%s api=1\\n' \"$token\" \"$3\"; exec sleep 30", Error::Address),
            ("printf 'READY token=%s addr=127.0.0.1:0 api=1\\n' \"$token\"; exec sleep 30", Error::Address),
            ("printf 'READY token=%s addr=127.0.0.1:1 api=1\\n' \"$token\"; exec sleep 30", Error::Address),
            ("printf 'READY token=%s addr=127.0.0.1:%s api=1 api=2\\n' \"$token\" \"$3\"; exec sleep 30", Error::Protocol),
            ("printf 'READY token='; exit 4", Error::Output),
            ("printf '%17000s\\n' ''; exec sleep 30", Error::Output),
            ("printf 'startup failed\\n' >&2; exit 9", Error::Eof),
            ("exec 1>&-; exec sleep 30", Error::Eof),
        ];
        for (body, expected) in cases {
            let server = Server::new(body);
            let backend = Backend::default();
            assert_eq!(backend.start(&server.binary, Duration::from_secs(2)).err(), Some(expected));
            server.assert_reaped(1);
        }
    }

    #[test]
    fn timeout_and_quit_during_handshake_reap() {
        let _serial = PROCESSES.lock().unwrap();
        let server = Server::new("exec sleep 30");
        let backend = Backend::default();
        let began = Instant::now();
        assert_eq!(backend.start(&server.binary, Duration::from_millis(150)).err(), Some(Error::Timeout));
        assert!(began.elapsed() < Duration::from_secs(1));
        server.assert_reaped(1);

        let server = Server::new("exec sleep 30");
        let backend = Backend::default();
        let worker = backend.clone();
        let binary = server.binary.clone();
        let task = std::thread::spawn(move || worker.connect(Source::Owned(binary)));
        server.wait_started();
        backend.stop();
        assert_eq!(task.join().unwrap().err(), Some(Error::Cancelled));
        server.assert_reaped(1);
        assert_eq!(backend.connect(Source::Owned(server.binary.clone())).err(), Some(Error::Cancelled));
        server.assert_reaped(1);
    }

    #[test]
    fn retries_only_bind_failure_and_bounds_attempts() {
        let _serial = PROCESSES.lock().unwrap();
        let body = format!("if [ ! -f attempted ]; then touch attempted; printf 'Error: could not listen on 127.0.0.1:%s\\n' \"$3\" >&2; exit 1; fi\n{}", success());
        let server = Server::new(&body);
        let backend = Backend::default();
        backend.connect(Source::Owned(server.binary.clone())).unwrap();
        backend.stop();
        server.assert_reaped(2);

        let server = Server::new("printf 'Error: could not listen on 127.0.0.1:%s\\n' \"$3\" >&2; exit 1");
        let backend = Backend::default();
        assert_eq!(backend.start(&server.binary, Duration::from_secs(2)).err(), Some(Error::Eof));
        server.assert_reaped(ATTEMPTS);
    }

    #[test]
    fn external_server_is_not_owned() {
        let _serial = PROCESSES.lock().unwrap();
        let mut external = Command::new("sleep").arg("30").spawn().unwrap();
        let backend = Backend::default();
        let mut deliveries = 0;
        backend.run(Source::External(Connection {
            server: "http://127.0.0.1:9723".into(), token: Some("external-token".into()),
        }), |event| {
            let Event::Connected(connection) = event else { panic!("external server was supervised"); };
            deliveries += 1;
            assert_eq!(connection.server, "http://127.0.0.1:9723");
            assert!(connection.token.as_deref() == Some("external-token"));
            true
        });
        assert_eq!(deliveries, 1);
        backend.stop();
        drop(backend);
        let alive = matches!(external.try_wait(), Ok(None));
        external.kill().unwrap();
        external.wait().unwrap();
        assert!(alive, "external server was terminated");
    }

    #[test]
    fn restart_delivers_new_validated_endpoint_and_auth() {
        let _serial = PROCESSES.lock().unwrap();
        let server = Server::new("count=$(wc -l < pids)\ntoken=$(printf '%064x' \"$count\")\nprintf 'READY token=%s addr=127.0.0.1:%s api=1\\n' \"$token\" \"$3\"\nif [ \"$count\" -eq 1 ]; then exec sleep 0.15; fi\nexec sleep 30");
        let backend = Backend::default();
        let worker = backend.clone();
        let binary = server.binary.clone();
        let (tx, rx) = mpsc::channel();
        let task = std::thread::spawn(move || worker.run(Source::Owned(binary), |event| tx.send(event).is_ok()));
        let Event::Connected(first) = rx.recv_timeout(Duration::from_secs(2)).unwrap() else { panic!("initial connection missing"); };
        assert!(first.token.as_deref() == Some(format!("{:064x}", 1).as_str()));
        // Keep the old advertised port occupied so accidentally retaining its endpoint
        // is observably wrong even if the OS would otherwise choose the same free port.
        let _old_port = TcpListener::bind(first.server.trim_start_matches("http://")).unwrap();
        assert!(matches!(rx.recv_timeout(Duration::from_secs(2)).unwrap(), Event::Reconnecting { attempt: 1 }));
        let Event::Connected(second) = rx.recv_timeout(Duration::from_secs(2)).unwrap() else { panic!("replacement connection missing"); };
        assert!(second.token.as_deref() == Some(format!("{:064x}", 2).as_str()));
        assert_ne!(first.server, second.server);
        let args = fs::read_to_string(server.root.join("args")).unwrap();
        let second_port = args.lines().nth(1).unwrap().split_whitespace().nth(2).unwrap();
        assert_eq!(second.server, format!("http://127.0.0.1:{second_port}"));
        backend.stop();
        task.join().unwrap();
        assert!(rx.try_recv().is_err(), "delivered work after shutdown");
        server.assert_reaped(2);
    }

    #[test]
    fn restart_budget_does_not_reset_and_rejects_invalid_replacements() {
        let _serial = PROCESSES.lock().unwrap();
        for invalid in [false, true] {
            let server = Server::new(&format!("count=$(wc -l < pids)\napi=1\nif [ '{invalid}' = true ] && [ \"$count\" -gt 1 ]; then api=2; fi\nprintf 'READY token=%s addr=127.0.0.1:%s api=%s\\n' \"$token\" \"$3\" \"$api\"\nexec sleep 0.15"));
            let backend = Backend::default();
            let mut connected = 0;
            let mut retries = Vec::new();
            let mut failed = None;
            let began = Instant::now();
            backend.run(Source::Owned(server.binary.clone()), |event| {
                match event {
                    Event::Connected(_) => connected += 1,
                    Event::Reconnecting { attempt } => retries.push(attempt),
                    Event::Failed(error) => failed = Some(error),
                }
                true
            });
            assert_eq!(connected, if invalid { 1 } else { 4 });
            assert_eq!(retries, [1, 2, 3]);
            assert_eq!(failed, Some(Error::RestartLimit));
            assert!(began.elapsed() >= Duration::from_millis(1750), "backoff was skipped");
            assert!(began.elapsed() < Duration::from_secs(5), "retry budget was unbounded");
            server.assert_reaped(4);
        }
        let server = Server::new("printf 'READY api=2\\n'; exec sleep 30");
        let backend = Backend::default();
        let mut deliveries = 0;
        backend.run(Source::Owned(server.binary.clone()), |event| {
            assert!(matches!(event, Event::Failed(Error::Protocol)));
            deliveries += 1;
            true
        });
        assert_eq!(deliveries, 1, "failed initial startup should not be restarted");
        server.assert_reaped(1);
    }

    #[test]
    fn quit_during_restart_backoff_or_handshake_prevents_late_work() {
        let _serial = PROCESSES.lock().unwrap();
        for during_handshake in [false, true] {
            let server = Server::new("count=$(wc -l < pids)\nif [ \"$count\" -gt 1 ]; then exec sleep 30; fi\nprintf 'READY token=%s addr=127.0.0.1:%s api=1\\n' \"$token\" \"$3\"\nexec sleep 0.15");
            let backend = Backend::default();
            let worker = backend.clone();
            let binary = server.binary.clone();
            let (tx, rx) = mpsc::channel();
            let task = std::thread::spawn(move || worker.run(Source::Owned(binary), |event| tx.send(event).is_ok()));
            assert!(matches!(rx.recv_timeout(Duration::from_secs(2)).unwrap(), Event::Connected(_)));
            assert!(matches!(rx.recv_timeout(Duration::from_secs(2)).unwrap(), Event::Reconnecting { attempt: 1 }));
            if during_handshake {
                let deadline = Instant::now() + Duration::from_secs(2);
                while server.pids().len() < 2 {
                    assert!(Instant::now() < deadline);
                    std::thread::sleep(Duration::from_millis(5));
                }
            }
            let stopped = Instant::now();
            backend.stop();
            task.join().unwrap();
            assert!(stopped.elapsed() < Duration::from_millis(200), "shutdown waited for backoff/READY timeout");
            assert!(rx.try_recv().is_err(), "published connection/failure after quit");
            server.assert_reaped(if during_handshake { 2 } else { 1 });
            backend.run(Source::Owned(server.binary.clone()), |_| panic!("work after shutdown"));
            server.assert_reaped(if during_handshake { 2 } else { 1 });
        }
    }

    #[test]
    fn resolves_cli_sibling_and_rejects_gui_identity() {
        let _serial = PROCESSES.lock().unwrap();
        let self_exe = std::env::current_exe().unwrap();
        assert_eq!(resolve_binary(Some(self_exe.clone().into_os_string()), &self_exe, None).err(), Some(Error::SelfExecutable));
        let server = Server::new("exit 0");
        let gui = Path::new(&server.binary);
        let own_alias = server.root.join("own-alias");
        std::os::unix::fs::symlink(gui, &own_alias).unwrap();
        assert_eq!(resolve_binary(Some(own_alias.into_os_string()), gui, None).err(), Some(Error::SelfExecutable));
        let own_link = server.root.join("own-link");
        fs::hard_link(gui, &own_link).unwrap();
        assert_eq!(resolve_binary(Some(own_link.into_os_string()), gui, None).err(), Some(Error::SelfExecutable));
        // The GUI is named durian, exactly like the Bazel output. Skip it on PATH too.
        let fallback_dir = server.root.join("path");
        fs::create_dir(&fallback_dir).unwrap();
        let fallback = fallback_dir.join("durian");
        fs::copy(gui, &fallback).unwrap();
        let paths = std::env::join_paths([&server.root, &fallback_dir]).unwrap();
        assert_eq!(resolve_binary(None, gui, Some(paths.clone())).unwrap(), fallback.into_os_string());
        let sibling = server.root.join("durian-cli");
        fs::copy(gui, &sibling).unwrap();
        assert_eq!(resolve_binary(None, gui, Some(paths)).unwrap(), sibling.into_os_string());
        assert_eq!(resolve_binary(None, gui, None).unwrap(), server.root.join("durian-cli").into_os_string());
        assert_eq!(resolve_binary(Some("durian".into()), gui, Some(server.root.clone().into_os_string())).err(), Some(Error::SelfExecutable));
    }

    #[test]
    fn owner_drop_and_spawn_failure() {
        let _serial = PROCESSES.lock().unwrap();
        let server = Server::new(success());
        let backend = Backend::default();
        backend.connect(Source::Owned(server.binary.clone())).unwrap();
        drop(backend);
        server.assert_reaped(1);
        let backend = Backend::default();
        assert_eq!(backend.connect(Source::Owned(server.root.join("missing").into_os_string())).err(), Some(Error::Spawn));
    }

    #[test]
    fn startup_logs_never_contain_child_token() {
        // Capture the actual logger in a subprocess, not a mock/sanitizer-only test.
        if std::env::var_os("DURIAN_BACKEND_LOG_CHILD").is_some() {
            split_ready_and_owned_quit_reap();
            return;
        }
        let _serial = PROCESSES.lock().unwrap();
        let output = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", std::thread::current().name().unwrap(), "--nocapture"])
            .env("DURIAN_BACKEND_LOG_CHILD", "1").output().unwrap();
        assert!(output.status.success());
        let logs = String::from_utf8(output.stderr).unwrap();
        assert!(logs.contains("owned durian server ready"));
        assert!(logs.contains("output suppressed"));
        assert!(!logs.contains(TOKEN));
        assert!(!String::from_utf8(output.stdout).unwrap().contains(TOKEN));
    }
}
