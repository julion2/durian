//! Owns the platform helper process and its line-delimited JSON transport, not render policy.
//! macOS packages WKWebView, Linux WebKitGTK; an explicit MAILRENDER_BIN can supply an
//! implementation of the same protocol. Neither engine runs inside GPUI.

use std::io::{self, BufRead, BufReader, Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::sync::mpsc::{SyncSender, TrySendError, sync_channel};

use futures::SinkExt;
use futures::channel::mpsc;

use super::protocol::{MAX_REPLY_BYTES, Reply, Request};

// Replies contain geometry and file paths, not HTML or image bytes. Bound a broken helper's
// output without allocating an arbitrary line. Each of the two I/O senders has one extra
// channel slot (five buffered events total).
const BUFFERED_REPLIES: usize = 3;

pub(super) enum Event {
    Reply(Reply),
    Closed,
}

struct Process(Child);

impl Drop for Process {
    fn drop(&mut self) {
        // Linux WebKit owns sandbox/renderer children as well as the helper.
        // This group belongs only to this spawn; macOS lifecycle is unchanged.
        #[cfg(target_os = "linux")]
        unsafe { libc::kill(-(self.0.id() as i32), libc::SIGKILL); }
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

/// Starting early warms WebContent while the GPUI window comes up. Ownership transfers only
/// once, and replacing or abandoning a prefetched process kills and reaps it.
static EARLY: Mutex<Option<Process>> = Mutex::new(None);

pub(super) fn start_early(pool_size: usize) {
    if let Ok(mut early) = EARLY.lock() {
        *early = spawn(pool_size);
    }
}

pub(super) fn stop_early() {
    if let Ok(mut early) = EARLY.lock() {
        *early = None;
    }
}

fn spawn(pool_size: usize) -> Option<Process> {
    let explicit = std::env::var_os("MAILRENDER_BIN").map(PathBuf::from);
    // Only an explicit override or the packaged helper, never a PATH lookup.
    let bin = explicit.or_else(packaged_binary)?;
    spawn_command(Command::new(bin).arg(pool_size.to_string())).inspect_err(|err| eprintln!("[HTML] failed to spawn renderer: {err}")).ok()
}

fn packaged_binary() -> Option<PathBuf> {
    #[cfg(any(target_os = "macos", target_os = "linux"))]
    {
        std::env::current_exe()
            .ok()
            .and_then(|p| p.parent().map(|p| p.join("mailrender")))
            .filter(|p| p.is_file())
            .or_else(|| option_env!("MAILRENDER_BIN").map(PathBuf::from))
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    {
        None
    }
}

fn spawn_command(command: &mut Command) -> io::Result<Process> {
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::process::CommandExt;
        command.process_group(0);
    }
    command.stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::inherit()).spawn().map(Process)
}

pub(super) struct Helper {
    requests: SyncSender<Vec<u8>>,
    _process: Process,
}

impl Helper {
    pub fn start(pool_size: usize) -> Option<(Self, mpsc::Receiver<Event>)> {
        let early = EARLY.lock().ok().and_then(|mut early| early.take());
        Some(Self::connect(early.or_else(|| spawn(pool_size))?, pool_size))
    }

    fn connect(mut process: Process, pool_size: usize) -> (Self, mpsc::Receiver<Event>) {
        let mut stdin = process.0.stdin.take().expect("piped stdin");
        let stdout = process.0.stdout.take().expect("piped stdout");
        let (mut tx, rx) = mpsc::channel(BUFFERED_REPLIES);
        let (requests, outgoing) = sync_channel::<Vec<u8>>(pool_size);
        let mut errors = tx.clone();
        std::thread::spawn(move || {
            for frame in outgoing {
                if stdin.write_all(&frame).and_then(|_| stdin.flush()).is_err() {
                    let _ = futures::executor::block_on(errors.send(Event::Closed));
                    break;
                }
            }
        });
        std::thread::spawn(move || {
            let mut stdout = BufReader::new(stdout);
            loop {
                let mut line = Vec::new();
                // `take` bounds read_until, which otherwise grows until a newline or EOF.
                let read = Read::by_ref(&mut stdout).take((MAX_REPLY_BYTES + 1) as u64).read_until(b'\n', &mut line);
                match read {
                    Ok(0) | Err(_) => break,
                    Ok(_) if line.len() > MAX_REPLY_BYTES => break,
                    _ => {}
                }
                // Preserve tolerance of non-protocol stdout (e.g. diagnostics).
                let Ok(reply) = serde_json::from_slice::<Reply>(&line) else { continue };
                if futures::executor::block_on(tx.send(Event::Reply(reply))).is_err() {
                    return;
                }
            }
            let _ = futures::executor::block_on(tx.send(Event::Closed));
        });
        (Self { requests, _process: process }, rx)
    }

    pub fn send(&mut self, request: &Request<'_>) -> io::Result<()> {
        let mut frame = serde_json::to_vec(request)?;
        frame.push(b'\n');
        self.requests.try_send(frame).map_err(|error| match error {
            TrySendError::Full(_) => io::Error::new(io::ErrorKind::WouldBlock, "renderer input queue full"),
            TrySendError::Disconnected(_) => io::Error::new(io::ErrorKind::BrokenPipe, "renderer input closed"),
        })
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use std::time::{Duration, Instant};

    // Re-exec this Rust test binary as a real helper, without Python, shell tools, WebKit or
    // public network services. Every fixture has a hard lifetime even if a client misbehaves.
    pub(in crate::html_render) fn connect(mode: &str) -> (Helper, mpsc::Receiver<Event>) {
        let mut command = Command::new(std::env::current_exe().unwrap());
        command.args(["--exact", "html_render::helper::tests::fixture", "--ignored", "--nocapture"]);
        command.env("MAILRENDER_FIXTURE", mode);
        Helper::connect(spawn_command(&mut command).unwrap(), 3)
    }

    pub(in crate::html_render) fn receive(rx: &mut mpsc::Receiver<Event>) -> Event {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            match rx.try_recv() {
                Ok(event) => return event,
                Err(mpsc::TryRecvError::Closed) => panic!("helper closed without terminal event"),
                Err(mpsc::TryRecvError::Empty) => {}
            }
            assert!(Instant::now() < deadline, "helper event timed out");
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    #[test]
    #[ignore = "subprocess fixture, invoked by transport tests"]
    fn fixture() {
        let Ok(mode) = std::env::var("MAILRENDER_FIXTURE") else { return };
        std::thread::spawn(|| {
            std::thread::sleep(Duration::from_secs(10));
            std::process::exit(2);
        });
        if mode == "silent" {
            std::thread::sleep(Duration::from_secs(30)); // killed by owner or fixture deadline
            return;
        }
        if mode == "oversized" {
            io::stdout().write_all(&vec![b'x'; MAX_REPLY_BYTES + 1]).unwrap();
            io::stdout().flush().unwrap();
        } else if mode == "refused" {
            println!(r#"{{"ready":false,"error":"content rules unavailable"}}"#);
        } else {
            println!("not JSON: helper diagnostic");
            println!(r#"{{"ready":true,"pool":3}}"#);
        }
        if matches!(mode.as_str(), "oversized" | "refused" | "hold") {
            std::thread::sleep(Duration::from_secs(30)); // killed by owner or fixture deadline
            return;
        }
        let mut line = String::new();
        io::stdin().read_line(&mut line).unwrap();
        if mode == "exit" {
            std::process::exit(7);
        }
        let request: serde_json::Value = serde_json::from_str(&line).unwrap();
        // This expected input is independent of Request's serializer, including newline and
        // quoting. A split JSON frame, lost field or wrong layout width fails the contract.
        assert_eq!(request["width"], 664.);
        let html = request["html"].as_str().unwrap();
        assert!(html.contains("<p>One\n\"two\"</p>") || html.contains("<p>One\n&quot;two&quot;</p>"));
        let out = PathBuf::from(request["out"].as_str().unwrap());
        let path = if mode == "wrong-path" { PathBuf::from("/etc/passwd") } else { out.with_extension("0.jpg") };
        if !matches!(mode.as_str(), "ok" | "missing-file") {
            // A genuine image artifact, not a renderer: deterministic fixture pixels only.
            let image = image::RgbImage::from_fn(664, 211, |x, y| if x > y { image::Rgb([180, 20, 60]) } else { image::Rgb([10, 90, 150]) });
            image.save_with_format(out.with_extension("0.jpg"), image::ImageFormat::Jpeg).unwrap();
        }
        let mut reply = serde_json::json!({
            "id": request["id"], "ok": true, "width": 664., "height": 211., "clipped": false,
            "strips": [{"path": path, "y": 0., "height": 211.}],
            "links": [{"x": 13., "y": 27., "w": 41., "h": 19., "href": "https://example.org/a?b=c"}],
            "ms": 3.5
        });
        match mode.as_str() {
            "wrong-width" => reply["width"] = 900.into(),
            "wrong-height" => reply["height"] = 16001.into(),
            "wrong-link" => reply["links"][0]["w"] = (-1).into(),
            "gap" => reply["strips"][0]["y"] = 5.into(),
            "failure" => reply = serde_json::json!({"id": request["id"], "ok": false, "error": "timed out"}),
            _ => {}
        }
        println!("{reply}");
    }

    #[test]
    fn real_helper_protocol_survives_framing_and_process_exit() {
        let (mut helper, mut rx) = connect("ok");
        assert!(matches!(receive(&mut rx), Event::Reply(Reply { ready: Some(true), .. })));
        helper.send(&Request { id: "message-42", html: "<p>One\n\"two\"</p>", width: 664., out: std::path::Path::new("/tmp/doc.jpg") }).unwrap();
        let Event::Reply(reply) = receive(&mut rx) else { panic!("expected render reply") };
        assert_eq!(reply.id.as_deref(), Some("message-42"));
        assert_eq!((reply.width, reply.height, reply.ms), (Some(664.), Some(211.), Some(3.5)));
        assert_eq!(reply.strips[0].path, PathBuf::from("/tmp/doc.0.jpg"));
        let link = &reply.links[0];
        assert_eq!((link.x, link.y, link.w, link.h), (13., 27., 41., 19.));
        assert_eq!(link.href, "https://example.org/a?b=c");
        assert!(matches!(receive(&mut rx), Event::Closed));
    }

    #[test]
    fn bounded_output_and_owner_drop_terminate_helpers() {
        for mode in ["hold", "refused", "oversized"] {
            let (mut helper, mut rx) = connect(mode);
            let pid = helper._process.0.id() as i32;
            let event = receive(&mut rx);
            match mode {
                "hold" => assert!(matches!(event, Event::Reply(Reply { ready: Some(true), .. }))),
                "refused" => assert!(matches!(event, Event::Reply(Reply { ready: Some(false), .. }))),
                _ => assert!(matches!(event, Event::Closed)),
            }
            if mode == "hold" {
                // Much larger than a pipe buffer. The fixture never reads stdin; enqueueing
                // and teardown must still return instead of hanging the UI behind write().
                helper.send(&Request { id: "blocked", html: &"x".repeat(1 << 20), width: 664., out: std::path::Path::new("unused.jpg") }).unwrap();
            }
            drop(rx);
            drop(helper);
            // Observable OS lifetime, not a mock assertion about kill/wait call order.
            assert_eq!(unsafe { libc::kill(pid, 0) }, -1, "helper {pid} is still alive");
            assert_eq!(io::Error::last_os_error().raw_os_error(), Some(libc::ESRCH));
        }
    }
}
