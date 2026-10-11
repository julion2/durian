//! Client side of the HTML pipeline. The `mailrender` helper (helpers/mailrender, a pool of
//! offscreen WKWebViews) rasterizes sanitized HTML; this entity decides *what* to render *when*:
//!
//! - never blocks the UI: views ask for a key and show the text/plain part until it is `Ready`
//! - a wish list ordered by priority (open thread first, then the direction of travel); queued jobs
//!   that fall off the list are dropped, in-flight ones finish and land in the cache
//! - results persist on disk (~/Library/Caches/mail-prototype/html), so a second visit is free

use std::collections::hash_map::DefaultHasher;
use std::collections::{HashMap, HashSet, VecDeque};
use std::hash::{Hash, Hasher};
use std::io::{BufRead, BufReader, Write};
use std::path::PathBuf;
use std::process::{Child, ChildStdin, Command, Stdio};
use std::time::Instant;

use futures::StreamExt;
use futures::channel::mpsc;
use gpui::{Context, SharedString};
use serde::{Deserialize, Serialize};

use crate::trace;

/// Matches the helper's pool size: more in-flight jobs would only queue inside the helper,
/// where we can no longer reorder them.
const MAX_IN_FLIGHT: usize = 3;
/// The width mail is laid out at: what the thread's message card fits (content column 760
/// minus margins). Above 600 on purpose – most newsletters switch to their phone layout at
/// `max-width: 600px`.
const RENDER_WIDTH: f32 = 664.;
/// Bump to invalidate cached snapshots when the renderer output changes.
const CACHE_VERSION: u32 = 3;
/// Larger HTML isn't rendered (its text part shows): inlined images count, so this is roomy.
const MAX_HTML: usize = 32 << 20;

#[derive(Clone, Serialize, Deserialize)]
pub struct HtmlLink {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
    pub href: SharedString,
}

/// One horizontal slice of a rendered page (GPU textures have a maximum height).
#[derive(Clone, Serialize, Deserialize)]
pub struct HtmlStrip {
    pub path: PathBuf,
    pub y: f32,
    pub height: f32,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct HtmlSnapshot {
    /// top to bottom
    pub strips: Vec<HtmlStrip>,
    pub width: f32,
    pub height: f32,
    /// the page was longer than the renderer's maximum and is cut
    pub clipped: bool,
    pub links: Vec<HtmlLink>,
}

#[derive(Clone)]
pub enum RenderState {
    Pending,
    Ready(HtmlSnapshot),
    Failed(SharedString),
}

/// One render request: `key` identifies the message in the UI, `html` is the sanitized source.
#[derive(Clone)]
pub struct HtmlJob {
    pub key: SharedString,
    pub html: SharedString,
}

#[derive(Deserialize)]
struct Reply {
    id: Option<String>,
    ok: Option<bool>,
    ready: Option<bool>,
    height: Option<f32>,
    width: Option<f32>,
    #[serde(default)]
    clipped: bool,
    #[serde(default)]
    strips: Vec<HtmlStrip>,
    #[serde(default)]
    links: Vec<HtmlLink>,
    ms: Option<f64>,
    error: Option<String>,
}

pub struct HtmlRenderer {
    stdin: Option<ChildStdin>,
    _child: Option<Child>,
    helper_ready: bool,
    cache_dir: PathBuf,
    states: HashMap<SharedString, RenderState>,
    queue: VecDeque<HtmlJob>,
    in_flight: HashMap<SharedString, (HtmlJob, Instant)>,
    /// the open thread's jobs: they render before (and without) any other
    urgent: HashSet<SharedString>,
}

/// The helper process, started ahead of the window by [`start_helper_early`]: launching its
/// WebContent processes takes ~0.5–1 s, better spent while GPUI and the window come up.
static EARLY: std::sync::Mutex<Option<Child>> = std::sync::Mutex::new(None);

/// Start the HTML helper now (top of `main`); the first `HtmlRenderer` takes it over.
pub fn start_helper_early() {
    if let (Some(child), Ok(mut early)) = (spawn_process(), EARLY.lock()) {
        *early = Some(child);
    }
}

fn spawn_process() -> Option<Child> {
    let bin = option_env!("MAILRENDER_BIN")?;
    Command::new(bin)
        .arg(MAX_IN_FLIGHT.to_string())
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .inspect_err(|err| eprintln!("mailrender: failed to spawn {bin}: {err}"))
        .ok()
}

impl HtmlRenderer {
    /// Snapshots kept across launches (local, demo data).
    pub fn new(cx: &mut Context<Self>) -> Self {
        Self::in_dir(super::cache_dir("html"), cx)
    }

    /// Snapshots in `cache_dir` – e.g. a folder that goes away with the session.
    pub fn in_dir(cache_dir: PathBuf, cx: &mut Context<Self>) -> Self {
        let _ = std::fs::create_dir_all(&cache_dir);

        let mut this = Self {
            stdin: None,
            _child: None,
            helper_ready: false,
            cache_dir,
            states: HashMap::default(),
            queue: VecDeque::default(),
            in_flight: HashMap::default(),
            urgent: HashSet::default(),
        };
        this.spawn_helper(cx);
        this
    }

    fn spawn_helper(&mut self, cx: &mut Context<Self>) {
        // the one started before the window, if there is one (its WebViews are warm by now)
        if !crate::uses_snapshots() {
            return;
        }
        let early = EARLY.lock().ok().and_then(|mut early| early.take());
        let Some(mut child) = early.or_else(spawn_process) else { return };
        self.stdin = child.stdin.take();
        let stdout = child.stdout.take().expect("piped stdout");
        self._child = Some(child);

        // blocking line reader on its own thread, results hop to the UI thread over a channel
        let (tx, mut rx) = mpsc::unbounded::<String>();
        std::thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                if tx.unbounded_send(line).is_err() {
                    break;
                }
            }
        });
        cx.spawn(async move |this, cx| {
            while let Some(line) = rx.next().await {
                if this.update(cx, |this, cx| this.on_reply(&line, cx)).is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    pub fn state(&self, key: &str) -> Option<&RenderState> {
        self.states.get(key)
    }

    /// Replace the wish list. `jobs` is in priority order; anything already rendered (in memory
    /// or on disk) is resolved immediately, queued jobs not on the list are dropped.
    ///
    /// The first `urgent` jobs (the open thread's) render alone: while any of them waits or
    /// renders, nothing else starts – WebContent processes compete for the same cores.
    pub fn prioritize(&mut self, jobs: Vec<HtmlJob>, urgent: usize, cx: &mut Context<Self>) {
        self.urgent = jobs.iter().take(urgent).map(|j| j.key.clone()).collect();
        let mut wanted: HashSet<SharedString> = HashSet::default();
        let mut queue = VecDeque::new();
        let mut changed = false;
        for job in jobs {
            if !wanted.insert(job.key.clone()) {
                continue;
            }
            match self.states.get(&job.key) {
                Some(RenderState::Ready(_) | RenderState::Failed(_)) => continue,
                _ if self.in_flight.contains_key(&job.key) => continue,
                _ => {}
            }
            if job.html.len() > MAX_HTML {
                self.states.insert(job.key.clone(), RenderState::Failed("too large to render".into()));
                changed = true;
                continue;
            }
            if let Some(snapshot) = self.load_cached(&job.html) {
                self.states.insert(job.key.clone(), RenderState::Ready(snapshot));
                changed = true;
                continue;
            }
            self.states.insert(job.key.clone(), RenderState::Pending);
            queue.push_back(job);
        }
        // dropped jobs go back to "unknown" so a later request re-queues them
        for job in self.queue.drain(..) {
            if !wanted.contains(&job.key) {
                self.states.remove(&job.key);
            }
        }
        self.queue = queue;
        self.pump();
        if changed {
            cx.notify();
        }
    }

    fn pump(&mut self) {
        if !self.helper_ready {
            return;
        }
        while self.in_flight.len() < MAX_IN_FLIGHT {
            let urgent_queued = self.queue.iter().position(|j| self.urgent.contains(&j.key));
            let urgent_rendering = self.in_flight.keys().any(|k| self.urgent.contains(k));
            let next = match (urgent_queued, urgent_rendering) {
                (Some(ix), _) => ix,
                // the open thread's last one is rendering: the rest waits
                (None, true) => break,
                (None, false) => 0,
            };
            let Some(job) = self.queue.remove(next) else { break };
            let request = serde_json::json!({
                "id": job.key.as_ref(),
                "html": job.html.as_ref(),
                "width": RENDER_WIDTH,
                "out": self.cache_path(&job.html, "jpg"),
            });
            let Some(stdin) = self.stdin.as_mut() else { return };
            if writeln!(stdin, "{request}").and_then(|_| stdin.flush()).is_err() {
                self.states.insert(job.key.clone(), RenderState::Failed("renderer unavailable".into()));
                continue;
            }
            self.in_flight.insert(job.key.clone(), (job, Instant::now()));
        }
    }

    fn on_reply(&mut self, line: &str, cx: &mut Context<Self>) {
        let Ok(reply) = serde_json::from_str::<Reply>(line) else { return };
        if reply.ready == Some(true) {
            self.helper_ready = true;
            self.pump();
            return;
        }
        let Some(id) = reply.id else { return };
        let key = SharedString::from(id);
        let Some((job, sent_at)) = self.in_flight.remove(&key) else { return };

        let state = if reply.ok == Some(true) {
            let snapshot = HtmlSnapshot {
                strips: reply.strips,
                width: reply.width.unwrap_or(RENDER_WIDTH),
                height: reply.height.unwrap_or_default(),
                clipped: reply.clipped,
                links: reply.links,
            };
            if let Ok(json) = serde_json::to_vec(&snapshot) {
                let _ = std::fs::write(self.cache_path(&job.html, "json"), json);
            }
            trace::note(&format!(
                "html {key} rendered in {:.0}ms (helper {:.0}ms)",
                sent_at.elapsed().as_secs_f64() * 1000.,
                reply.ms.unwrap_or_default()
            ));
            RenderState::Ready(snapshot)
        } else {
            RenderState::Failed(reply.error.unwrap_or_default().into())
        };
        self.states.insert(key, state);
        self.pump();
        cx.notify();
    }

    fn cache_key(html: &str) -> u64 {
        let mut hasher = DefaultHasher::new();
        (CACHE_VERSION, RENDER_WIDTH.to_bits(), html).hash(&mut hasher);
        hasher.finish()
    }

    fn cache_path(&self, html: &str, ext: &str) -> PathBuf {
        self.cache_dir.join(format!("{:016x}.{ext}", Self::cache_key(html)))
    }

    fn load_cached(&self, html: &str) -> Option<HtmlSnapshot> {
        let json = std::fs::read(self.cache_path(html, "json")).ok()?;
        let snapshot: HtmlSnapshot = serde_json::from_slice(&json).ok()?;
        snapshot.strips.iter().all(|s| s.path.exists()).then_some(snapshot)
    }
}
