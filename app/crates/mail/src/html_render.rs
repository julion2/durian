//! Client side of the HTML pipeline. The `mailrender` helper (helpers/mailrender, a pool of
//! offscreen WKWebViews) rasterizes sanitized HTML; this entity decides *what* to render *when*:
//!
//! - never blocks the UI: views ask for a key and show the text/plain part until it is `Ready`
//! - a wish list ordered by priority (open thread first, then the direction of travel); queued jobs
//!   that fall off the list are dropped, in-flight ones finish and land in the cache
//! - results persist on disk (~/Library/Caches/mail-prototype/html), so a second visit is free

mod helper;
mod protocol;

use std::collections::hash_map::DefaultHasher;
use std::collections::{HashMap, HashSet, VecDeque};
use std::hash::{Hash, Hasher};
use std::io::Read;
use std::path::PathBuf;
use std::time::{Duration, Instant};

use futures::{FutureExt, StreamExt};
use gpui::{Context, SharedString, Task};
use serde::{Deserialize, Serialize};

use crate::trace;
use helper::{Event, Helper};
use protocol::{Reply, Request};

pub use protocol::Strip as HtmlStrip;

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
const READY_TIMEOUT: Duration = Duration::from_secs(10);
/// Includes writing snapshot files after the helper's own 15-second render deadline.
const JOB_TIMEOUT: Duration = Duration::from_secs(20);

#[derive(Clone, Serialize, Deserialize)]
pub struct HtmlLink {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
    pub href: SharedString,
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

impl HtmlSnapshot {
    fn valid_output(&self, out: &std::path::Path, width: f32) -> bool {
        protocol::valid_snapshot(self.width, width, self.height, &self.strips, out)
            && self.links.len() <= protocol::MAX_LINKS
            && self.links.iter().all(|link| protocol::valid_link(link.x, link.y, link.w, link.h))
            && self.strips.iter().all(|strip| strip.path.symlink_metadata().is_ok_and(|metadata| metadata.is_file()))
    }
}

#[derive(Clone)]
pub enum RenderState {
    Pending,
    Ready(HtmlSnapshot),
    Failed(SharedString),
}

/// One render request: `key` identifies the message AND its prepared content in the UI.
/// A content change must use a new key, including equal-length changes. `html` is the
/// sanitized source; document policy belongs to html_doc, not the scheduling layer.
#[derive(Clone)]
pub struct HtmlJob {
    pub key: SharedString,
    pub html: SharedString,
}

pub struct HtmlRenderer {
    helper: Option<Helper>,
    replies: Option<Task<()>>,
    helper_ready: bool,
    workers: usize,
    width: f32,
    cache_dir: PathBuf,
    states: HashMap<SharedString, RenderState>,
    queue: VecDeque<HtmlJob>,
    in_flight: HashMap<SharedString, (HtmlJob, Instant)>,
    /// the open thread's jobs: they render before (and without) any other
    urgent: HashSet<SharedString>,
}

/// Start the HTML helper now (top of `main`); the first `HtmlRenderer` takes it over.
pub fn start_helper_early() {
    helper::start_early(MAX_IN_FLIGHT);
}

/// Startup can fail before a workspace takes ownership of the prefetched helper.
pub fn stop_helper_early() {
    helper::stop_early();
}

impl HtmlRenderer {
    /// Snapshots kept across launches (local, demo data).
    pub fn new(cx: &mut Context<Self>) -> Self {
        Self::in_dir(super::cache_dir("html"), cx)
    }

    /// Snapshots in `cache_dir` – e.g. a folder that goes away with the session.
    pub fn in_dir(cache_dir: PathBuf, cx: &mut Context<Self>) -> Self {
        Self::with_workers(cache_dir, crate::uses_snapshots().then_some(MAX_IN_FLIGHT), RENDER_WIDTH, cx)
    }

    /// Composer previews also use snapshots on macOS: no native view above a modal,
    /// and no changes to the open thread's render queue. Created only on demand.
    pub(crate) fn signature_preview(width: f32, cx: &mut Context<Self>) -> Self {
        // Two open editors must never encode the same snapshot file concurrently.
        Self::with_workers(crate::session_dir(&format!("signature-preview/{}", uuid::Uuid::new_v4())), Some(1), width.round(), cx)
    }

    fn with_workers(cache_dir: PathBuf, workers: Option<usize>, width: f32, cx: &mut Context<Self>) -> Self {
        let _ = std::fs::create_dir_all(&cache_dir);
        let cache_dir = std::fs::canonicalize(&cache_dir).unwrap_or(cache_dir);

        let mut this = Self {
            helper: None,
            replies: None,
            helper_ready: false,
            workers: workers.unwrap_or(MAX_IN_FLIGHT),
            width,
            cache_dir,
            states: HashMap::default(),
            queue: VecDeque::default(),
            in_flight: HashMap::default(),
            urgent: HashSet::default(),
        };
        if let Some(workers) = workers { this.spawn_helper(workers, cx); }
        this
    }

    fn spawn_helper(&mut self, workers: usize, cx: &mut Context<Self>) {
        // the one started before the window, if there is one (its WebViews are warm by now)
        let Some((helper, mut rx)) = Helper::start(workers) else { return };
        self.helper = Some(helper);
        let started = Instant::now();
        self.replies = Some(cx.spawn(async move |this, cx| {
            loop {
                let event = rx.next().fuse();
                let tick = cx.background_executor().timer(Duration::from_millis(250)).fuse();
                futures::pin_mut!(event, tick);
                let event = futures::select! {
                    event = event => Some(event.unwrap_or(Event::Closed)),
                    _ = tick => None,
                };
                let active = this
                    .update(cx, |this, cx| {
                        if this.expire(started, Instant::now()) {
                            cx.notify();
                            return false;
                        }
                        if let Some(event) = event {
                            match event {
                                Event::Reply(reply) => this.on_reply(reply),
                                Event::Closed => this.unavailable(),
                            }
                            cx.notify();
                        }
                        this.helper.is_some()
                    });
                if !matches!(active, Ok(true)) { break; }
            }
        }));
    }

    fn expire(&mut self, started: Instant, now: Instant) -> bool {
        if self.helper.is_some() && ((!self.helper_ready && now.duration_since(started) >= READY_TIMEOUT)
            || self.in_flight.values().any(|(_, sent)| now.duration_since(*sent) >= JOB_TIMEOUT))
        {
            trace::note("HTML renderer deadline exceeded; showing plain text");
            self.unavailable();
            return true;
        }
        false
    }

    fn unavailable(&mut self) {
        self.helper_ready = false;
        self.helper = None;
        for state in self.states.values_mut() {
            if matches!(state, RenderState::Pending) {
                *state = RenderState::Failed("HTML renderer unavailable; showing plain text".into());
            }
        }
        self.in_flight.clear();
        self.queue.clear();
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
            if self.helper.is_none() {
                self.states.insert(job.key.clone(), RenderState::Failed("HTML renderer unavailable; showing plain text".into()));
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
        // A broken input pipe can fail every pending job during pump.
        changed |= self.pump();
        if changed {
            cx.notify();
        }
    }

    fn pump(&mut self) -> bool {
        if !self.helper_ready {
            return false;
        }
        while self.in_flight.len() < self.workers {
            let urgent_queued = self.queue.iter().position(|j| self.urgent.contains(&j.key));
            let urgent_rendering = self.in_flight.keys().any(|k| self.urgent.contains(k));
            let next = match (urgent_queued, urgent_rendering) {
                (Some(ix), _) => ix,
                // the open thread's last one is rendering: the rest waits
                (None, true) => break,
                (None, false) => 0,
            };
            let Some(job) = self.queue.remove(next) else { break };
            let out = self.cache_path(&job.html, "jpg");
            let request = Request { id: job.key.as_ref(), html: job.html.as_ref(), width: self.width, out: &out };
            let Some(helper) = self.helper.as_mut() else { return false };
            if helper.send(&request).is_err() {
                self.unavailable();
                return true;
            }
            self.in_flight.insert(job.key.clone(), (job, Instant::now()));
        }
        false
    }

    fn on_reply(&mut self, reply: Reply) {
        // Either I/O worker can close the transport. Buffered replies arriving afterwards
        // must not resurrect a terminal session or populate its cache.
        if self.helper.is_none() {
            return;
        }
        if reply.ready == Some(false) {
            self.unavailable();
            return;
        }
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
                width: reply.width.unwrap_or(self.width),
                height: reply.height.unwrap_or_default(),
                clipped: reply.clipped,
                links: reply.links.into_iter().map(|link| HtmlLink { x: link.x, y: link.y, w: link.w, h: link.h, href: link.href.into() }).collect(),
            };
            if snapshot.valid_output(&self.cache_path(&job.html, "jpg"), self.width) {
                if let Ok(json) = serde_json::to_vec(&snapshot) {
                    let _ = std::fs::write(self.cache_path(&job.html, "json"), json);
                }
                trace::note(&format!("html {key} rendered in {:.0}ms (helper {:.0}ms)", sent_at.elapsed().as_secs_f64() * 1000., reply.ms.unwrap_or_default()));
                RenderState::Ready(snapshot)
            } else {
                RenderState::Failed("invalid HTML renderer output; showing plain text".into())
            }
        } else {
            RenderState::Failed(reply.error.unwrap_or_default().into())
        };
        self.states.insert(key, state);
        self.pump();
    }

    fn cache_key(&self, html: &str) -> u64 {
        let mut hasher = DefaultHasher::new();
        (CACHE_VERSION, self.width.to_bits(), html).hash(&mut hasher);
        hasher.finish()
    }

    fn cache_path(&self, html: &str, ext: &str) -> PathBuf {
        self.cache_dir.join(format!("{:016x}.{ext}", self.cache_key(html)))
    }

    fn load_cached(&self, html: &str) -> Option<HtmlSnapshot> {
        let mut json = Vec::new();
        std::fs::File::open(self.cache_path(html, "json")).ok()?.take((protocol::MAX_REPLY_BYTES + 1) as u64).read_to_end(&mut json).ok()?;
        if json.len() > protocol::MAX_REPLY_BYTES {
            return None;
        }
        let snapshot: HtmlSnapshot = serde_json::from_slice(&json).ok()?;
        snapshot.valid_output(&self.cache_path(html, "jpg"), self.width).then_some(snapshot)
    }
}

impl Drop for HtmlRenderer {
    fn drop(&mut self) {
        // Close the bounded result receiver before terminating its producer process.
        self.replies = None;
        self.helper = None;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct CacheDir(PathBuf);

    impl CacheDir {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!("mail-html-test-{}", uuid::Uuid::new_v4()));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
    }

    impl Drop for CacheDir {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    // Feed actual subprocess events into the same policy handler used by the GPUI task.
    // The fixture knows nothing about the scheduler's maps or cache-key implementation.
    fn renderer(mode: &str, dir: &CacheDir, keys: &[&str]) -> (HtmlRenderer, futures::channel::mpsc::Receiver<Event>, String) {
        let html = crate::html_doc::prepare("<p>One\n\"two\"</p>", &HashMap::new(), crate::html_doc::Policy::default()).unwrap();
        let (helper, rx) = helper::tests::connect(mode);
        let jobs: Vec<_> = keys.iter().map(|key| HtmlJob { key: (*key).to_string().into(), html: html.clone().into() }).collect();
        let renderer = HtmlRenderer {
            helper: Some(helper),
            replies: None,
            helper_ready: false,
            workers: MAX_IN_FLIGHT,
            width: RENDER_WIDTH,
            cache_dir: dir.0.clone(),
            states: jobs.iter().map(|job| (job.key.clone(), RenderState::Pending)).collect(),
            queue: jobs.into(),
            in_flight: HashMap::new(),
            urgent: HashSet::new(),
        };
        (renderer, rx, html)
    }

    fn deliver(renderer: &mut HtmlRenderer, rx: &mut futures::channel::mpsc::Receiver<Event>) {
        match helper::tests::receive(rx) {
            Event::Reply(reply) => renderer.on_reply(reply),
            Event::Closed => renderer.unavailable(),
        }
    }

    #[test]
    fn snapshots_are_bound_to_document_content_and_requested_files() {
        for mode in ["render", "wrong-path", "wrong-width", "wrong-height", "wrong-link", "gap", "failure", "missing-file"] {
            let dir = CacheDir::new();
            let (mut renderer, mut rx, html) = renderer(mode, &dir, &["message-42"]);
            deliver(&mut renderer, &mut rx); // helper readiness sends the queued job
            deliver(&mut renderer, &mut rx);
            if mode != "render" {
                assert!(matches!(renderer.state("message-42"), Some(RenderState::Failed(_))), "accepted {mode}");
                assert!(renderer.load_cached(&html).is_none(), "cached {mode}");
                continue;
            }
            let Some(RenderState::Ready(snapshot)) = renderer.state("message-42") else { panic!("fixture snapshot unavailable") };
            assert_eq!((snapshot.width, snapshot.height), (664., 211.));
            assert_eq!(snapshot.links[0].href.as_ref(), "https://example.org/a?b=c");
            let pixels = image::open(&snapshot.strips[0].path).unwrap().into_rgb8();
            assert_eq!(pixels.dimensions(), (664, 211));
            assert!(pixels.get_pixel(600, 20)[0] > 160 && pixels.get_pixel(10, 190)[2] > 130);
            let snapshot = snapshot.clone();
            assert!(renderer.load_cached(&html).is_some());
            let changed = html.replace("One", "Two");
            assert_eq!(html.len(), changed.len());
            assert!(renderer.load_cached(&changed).is_none(), "equal-length content must not reuse a snapshot");
            // A cache file is not trusted just because it deserialized and its path exists.
            let mut poisoned = snapshot.clone();
            poisoned.strips[0].path = dir.0.join("unrelated.jpg");
            std::fs::copy(&snapshot.strips[0].path, &poisoned.strips[0].path).unwrap();
            std::fs::write(renderer.cache_path(&html, "json"), serde_json::to_vec(&poisoned).unwrap()).unwrap();
            assert!(renderer.load_cached(&html).is_none());
            // EOF leaves completed pictures available.
            deliver(&mut renderer, &mut rx);
            assert!(matches!(renderer.state("message-42"), Some(RenderState::Ready(_))));
        }
    }

    #[test]
    fn helper_failure_never_leaves_pending_jobs() {
        for mode in ["exit", "refused", "oversized", "silent", "hold"] {
            let dir = CacheDir::new();
            let (mut renderer, mut rx, _) = renderer(mode, &dir, &["one", "two", "three", "four"]);
            let started = Instant::now();
            if mode == "silent" {
                assert!(!renderer.expire(started, started + READY_TIMEOUT - Duration::from_millis(1)));
                assert!(renderer.expire(started, started + READY_TIMEOUT));
            } else {
                deliver(&mut renderer, &mut rx);
                if mode == "exit" { deliver(&mut renderer, &mut rx); }
                if mode == "hold" {
                    assert!(!renderer.expire(started, Instant::now()));
                    assert!(renderer.expire(started, Instant::now() + JOB_TIMEOUT));
                }
            }
            for key in ["one", "two", "three", "four"] {
                assert!(matches!(renderer.state(key), Some(RenderState::Failed(_))), "{mode} left {key} pending");
            }
        }
    }
}
