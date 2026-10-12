//! Avatars for senders, as durian's macOS app has them: a Gravatar photo for addresses at
//! personal mail providers, the company's logo (Brandfetch) for everyone else, initials when
//! neither exists.
//!
//! These lookups leave the machine – the photo services learn which addresses and domains
//! appear in the mailbox, as in the Swift app. Two limits it doesn't have:
//! - never for the user's own addresses (nothing is fetched until they're known)
//! - in memory only: no picture files, whose names would list the correspondents
//!
//! Requests run on four worker threads; the UI asks during render ([`lookup`]) and repaints
//! when a picture arrives. A bounded LRU remembers pictures and misses. Replacing the session
//! cancels queued work and discards old results. `MAIL_AVATARS=0` disables all requests.

use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::io::Cursor;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex, mpsc};
use std::time::Duration;

use futures::{SinkExt, StreamExt};
use gpui::{App, RenderImage, Task};

/// Address domains of personal mail providers: these get Gravatar, others a company logo.
const PERSONAL: &[&str] = &[
    "gmail.com",
    "googlemail.com",
    "outlook.com",
    "hotmail.com",
    "live.com",
    "msn.com",
    "outlook.de",
    "yahoo.com",
    "yahoo.de",
    "ymail.com",
    "gmx.de",
    "gmx.net",
    "gmx.at",
    "gmx.ch",
    "web.de",
    "t-online.de",
    "freenet.de",
    "mail.de",
    "email.de",
    "icloud.com",
    "me.com",
    "mac.com",
    "aol.com",
    "protonmail.com",
    "proton.me",
    "pm.me",
    "posteo.de",
    "mailbox.org",
    "tutanota.com",
    "tutanota.de",
    "tuta.io",
];

/// durian's Brandfetch client id (the Swift app's).
const BRANDFETCH: &str = "1idWonATCJFIseiVHIH";

/// Brandfetch answers unknown domains with a generic picture instead of a 404.
const PLACEHOLDERS: &[&str] = &["38926c4ecbd5590c77a969ae5a516b49", "ad18dbe2bac10daa8e99cc27efed1607"];

const WORKERS: usize = 4;
const PIXELS: u32 = 128;
const CACHE_ENTRIES: usize = 256;
const QUEUED_REQUESTS: usize = 32;
const MAX_ADDRESS_BYTES: usize = 254;
const MAX_DOWNLOAD_BYTES: u64 = 2 << 20;
const MAX_DECODE_DIMENSION: u32 = 2048;
const MAX_DECODE_ALLOC: u64 = 32 << 20;

type Picture = Option<(Arc<RenderImage>, bool)>;
type ResultMessage = (String, Picture);

/// Where a sender's picture comes from, and whether it's a logo (drawn inset on a disc).
#[derive(Debug, PartialEq)]
pub enum Source {
    Gravatar(String),
    Logo(String),
}

impl Source {
    pub fn for_address(email: &str) -> Option<Self> {
        let email = email.trim().to_lowercase();
        if email.len() > MAX_ADDRESS_BYTES {
            return None;
        }
        let (_, domain) = email.rsplit_once('@')?;
        if domain.is_empty() || !domain.contains('.') {
            return None;
        }
        Some(if PERSONAL.contains(&domain) { Source::Gravatar(email.clone()) } else { Source::Logo(domain.to_string()) })
    }

    fn url(&self) -> String {
        match self {
            // d=404: no photo, no default picture
            Source::Gravatar(email) => format!("https://gravatar.com/avatar/{:x}?d=404&s={PIXELS}", md5::compute(email.as_bytes())),
            Source::Logo(domain) => format!("https://cdn.brandfetch.io/{domain}?c={BRANDFETCH}"),
        }
    }

    fn is_logo(&self) -> bool {
        matches!(self, Source::Logo(_))
    }
}

enum State {
    Pending,
    Ready(Arc<RenderImage>, bool),
    Missing,
}

struct Entry {
    state: State,
    used: u64,
}

struct Session {
    cancelled: AtomicBool,
    /// Until the user's addresses are known, nothing may be fetched.
    own: Mutex<Option<HashSet<String>>>,
}

impl Session {
    fn allows(&self, email: &str) -> bool {
        !self.cancelled.load(Ordering::Acquire) && self.own.lock().ok().is_some_and(|own| own.as_ref().is_some_and(|own| !own.contains(email)))
    }
}

// Shared across replacements: old requests may still be finishing when a new
// session starts, but no more than four requests may run in total.
static FLIGHTS: (Mutex<usize>, Condvar) = (Mutex::new(0), Condvar::new());

struct Flight;

impl Flight {
    fn start(session: &Session, email: &str) -> Option<Self> {
        let mut active = FLIGHTS.0.lock().unwrap();
        while *active == WORKERS && !session.cancelled.load(Ordering::Acquire) {
            active = FLIGHTS.1.wait(active).unwrap();
        }
        if !session.allows(email) {
            return None;
        }
        *active += 1;
        Some(Self)
    }
}

impl Drop for Flight {
    fn drop(&mut self) {
        *FLIGHTS.0.lock().unwrap() -= 1;
        FLIGHTS.1.notify_all();
    }
}

struct Store {
    states: HashMap<String, Entry>,
    clock: u64,
    session: Arc<Session>,
    requests: mpsc::SyncSender<String>,
    /// Dropping the store cancels its result consumer, closing the result queue.
    _consumer: Option<Task<()>>,
}

impl Drop for Store {
    fn drop(&mut self) {
        // Serialize cancellation with admitting a request to the flight gate.
        let _active = FLIGHTS.0.lock().unwrap();
        self.session.cancelled.store(true, Ordering::Release);
        FLIGHTS.1.notify_all();
    }
}

thread_local! {
    // the UI thread's: render asks, the result task fills
    static STORE: RefCell<Option<Store>> = const { RefCell::new(None) };
}

/// Start the workers. Pictures are looked up once [`set_own`] said whose not to.
pub fn init(cx: &mut App) {
    STORE.with_borrow_mut(|store| *store = None);
    if std::env::var("MAIL_AVATARS").as_deref() == Ok("0") {
        return;
    }
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(8)))
        // Brandfetch serves browsers only
        .user_agent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko)")
        .http_status_as_error(false)
        .build()
        .into();
    let (mut store, mut arrived) =
        start_session(move |email| Source::for_address(email).and_then(|source| fetch(&agent, &source).map(|image| (image, source.is_logo()))));
    let session = store.session.clone();
    store._consumer = Some(cx.spawn(async move |cx| {
        while let Some((email, picture)) = arrived.next().await {
            if accept_result(&session, email, picture) {
                cx.update(|cx| cx.refresh_windows());
            }
        }
    }));
    STORE.with_borrow_mut(|current| *current = Some(store));
}

fn start_session(fetch: impl Fn(&str) -> Picture + Send + Sync + 'static) -> (Store, futures::channel::mpsc::Receiver<ResultMessage>) {
    let session = Arc::new(Session { cancelled: AtomicBool::new(false), own: Mutex::new(None) });
    let (requests, incoming) = mpsc::sync_channel::<String>(QUEUED_REQUESTS);
    let incoming = Arc::new(Mutex::new(incoming));
    // futures mpsc gives each sender one slot in addition to this buffer size:
    // zero buffer plus four worker senders means at most four queued results.
    let (results, arrived) = futures::channel::mpsc::channel::<ResultMessage>(0);
    let fetch = Arc::new(fetch);
    for _ in 0..WORKERS {
        let (incoming, mut results, session, fetch) = (incoming.clone(), results.clone(), session.clone(), fetch.clone());
        std::thread::spawn(move || {
            loop {
                let Ok(email) = incoming.lock().map_err(|_| ()).and_then(|rx| rx.recv().map_err(|_| ())) else { break };
                if session.cancelled.load(Ordering::Acquire) {
                    break;
                }
                let Some(flight) = Flight::start(&session, &email) else { continue };
                let picture = fetch(&email);
                drop(flight);
                if session.cancelled.load(Ordering::Acquire) || futures::executor::block_on(results.send((email, picture))).is_err() {
                    break;
                }
            }
        });
    }
    (Store { states: HashMap::new(), clock: 0, session, requests, _consumer: None }, arrived)
}

fn accept_result(session: &Arc<Session>, email: String, picture: Picture) -> bool {
    STORE.with_borrow_mut(|store| {
        let Some(store) = store else { return false };
        if !Arc::ptr_eq(&store.session, session) || !session.allows(&email) {
            return false;
        }
        let Some(entry) = store.states.get_mut(&email) else { return false };
        entry.state = picture.map_or(State::Missing, |(image, logo)| State::Ready(image, logo));
        true
    })
}

/// The user's own addresses (never looked up). Until this is called, nothing is.
pub fn set_own(addresses: impl IntoIterator<Item = impl AsRef<str>>) {
    STORE.with_borrow_mut(|store| {
        if let Some(store) = store {
            let own: HashSet<_> = addresses.into_iter().map(|a| a.as_ref().trim().to_lowercase()).collect();
            store.states.retain(|email, _| !own.contains(email));
            *store.session.own.lock().unwrap() = Some(own);
        }
    });
}

/// The picture for an address if there is one yet (asked for if not). None without `init`.
pub fn lookup(email: &str) -> Option<(Arc<RenderImage>, bool)> {
    if email.trim().len() > MAX_ADDRESS_BYTES {
        return None;
    }
    let email = email.trim().to_lowercase();
    STORE.with_borrow_mut(|store| {
        let store = store.as_mut()?;
        if !store.session.allows(&email) || Source::for_address(&email).is_none() {
            return None;
        }
        store.clock += 1;
        if let Some(entry) = store.states.get_mut(&email) {
            entry.used = store.clock;
            return match &entry.state {
                State::Ready(image, logo) => Some((image.clone(), *logo)),
                _ => None,
            };
        }
        let evict = if store.states.len() == CACHE_ENTRIES {
            // Never evict pending work: that would allow duplicate requests.
            Some(store.states.iter().filter(|(_, entry)| !matches!(entry.state, State::Pending)).min_by_key(|(_, entry)| entry.used)?.0.clone())
        } else {
            None
        };
        if store.requests.try_send(email.clone()).is_ok() {
            if let Some(evict) = evict {
                store.states.remove(&evict);
            }
            store.states.insert(email, Entry { state: State::Pending, used: store.clock });
        }
        // A full queue is retried on a later render, not remembered as a miss.
        None
    })
}

/// Download and decode (GPUI wants BGRA). None: no picture there (or a placeholder).
fn fetch(agent: &ureq::Agent, source: &Source) -> Option<Arc<RenderImage>> {
    let mut response = agent.get(source.url()).call().ok()?;
    if response.status().as_u16() != 200 {
        return None;
    }
    let bytes = response.body_mut().with_config().limit(MAX_DOWNLOAD_BYTES).read_to_vec().ok()?;
    if source.is_logo() && PLACEHOLDERS.contains(&format!("{:x}", md5::compute(&bytes)).as_str()) {
        return None;
    }
    decode(&bytes)
}

fn decode(bytes: &[u8]) -> Option<Arc<RenderImage>> {
    let mut reader = image::ImageReader::new(Cursor::new(bytes)).with_guessed_format().ok()?;
    let mut limits = image::Limits::default();
    limits.max_image_width = Some(MAX_DECODE_DIMENSION);
    limits.max_image_height = Some(MAX_DECODE_DIMENSION);
    limits.max_alloc = Some(MAX_DECODE_ALLOC);
    reader.limits(limits);
    let mut rgba = reader.decode().ok()?.thumbnail(PIXELS, PIXELS).into_rgba8();
    if blank(&rgba) {
        return None;
    }
    for pixel in rgba.pixels_mut() {
        pixel.0.swap(0, 2);
    }
    Some(Arc::new(RenderImage::new([image::Frame::new(rgba)])))
}

/// A picture with nothing to see – fully transparent, or one flat color (some logos come back
/// that way): initials say more.
fn blank(image: &image::RgbaImage) -> bool {
    let visible: Vec<&image::Rgba<u8>> = image.pixels().filter(|p| p.0[3] > 24).collect();
    let Some(first) = visible.first() else { return true };
    visible.iter().all(|p| p.0[..3].iter().zip(&first.0[..3]).all(|(a, b)| a.abs_diff(*b) < 12))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::time::Instant;

    fn png(width: u32, height: u32) -> Vec<u8> {
        let image = image::RgbaImage::from_fn(width, height, |x, y| {
            if x + y < (width + height) / 2 { image::Rgba([200, 30, 10, 255]) } else { image::Rgba([0, 80, 150, 255]) }
        });
        let mut bytes = Cursor::new(Vec::new());
        image::DynamicImage::ImageRgba8(image).write_to(&mut bytes, image::ImageFormat::Png).unwrap();
        bytes.into_inner()
    }

    // Exercise the same workers and session replacement as init without a native
    // window or public avatar services. Workers decode real deterministic PNGs.
    fn install(fetch: impl Fn(&str) -> Picture + Send + Sync + 'static) -> (Arc<Session>, futures::channel::mpsc::Receiver<ResultMessage>) {
        let (store, arrived) = start_session(fetch);
        let session = store.session.clone();
        STORE.with_borrow_mut(|current| *current = Some(store));
        (session, arrived)
    }

    fn receive(arrived: &mut futures::channel::mpsc::Receiver<ResultMessage>) -> ResultMessage {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Ok(result) = arrived.try_recv() {
                return result;
            }
            assert!(Instant::now() < deadline, "avatar result did not arrive");
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    #[test]
    fn personal_addresses_get_gravatar_others_a_logo() {
        assert_eq!(Source::for_address(" Ada@GMAIL.com"), Some(Source::Gravatar("ada@gmail.com".into())));
        assert_eq!(Source::for_address("billing@stripe.com"), Some(Source::Logo("stripe.com".into())));
        assert_eq!(Source::for_address("no-at-sign"), None);
        assert_eq!(Source::for_address("root@localhost"), None);
        assert!(Source::for_address(&format!("{}@example.org", "a".repeat(242))).is_some());
        assert!(Source::for_address(&format!("{}@example.org", "a".repeat(243))).is_none());
        // Gravatar's documented example: md5 of the trimmed, lowercased address
        assert_eq!(Source::Gravatar("myemailaddress@example.com".into()).url(), "https://gravatar.com/avatar/0bc83cb571cd1c50ba6f3e8a78ef1346?d=404&s=128");
    }

    #[test]
    fn blank_pictures_fall_back_to_initials() {
        let clear = image::RgbaImage::from_pixel(8, 8, image::Rgba([0, 0, 0, 0]));
        let white = image::RgbaImage::from_pixel(8, 8, image::Rgba([255, 255, 255, 255]));
        let mut logo = white.clone();
        logo.put_pixel(3, 3, image::Rgba([200, 30, 30, 255]));
        assert!(blank(&clear) && blank(&white));
        assert!(!blank(&logo));
    }

    #[test]
    fn nothing_is_looked_up_before_the_own_addresses_are_known() {
        let bytes = png(2, 1);
        let (session, mut arrived) = install(move |email| {
            assert_eq!(email, "mara@example.org", "only the permitted address may be fetched");
            decode(&bytes).map(|image| (image, true))
        });
        assert!(lookup("mara@example.org").is_none());
        set_own(["Me@Work.dev"]);
        assert!(lookup("me@work.dev").is_none());
        assert!(lookup(" Mara@EXAMPLE.org ").is_none());
        let (email, picture) = receive(&mut arrived);
        accept_result(&session, email, picture);
        let (image, logo) = lookup("mara@example.org").unwrap();
        assert!(logo);
        let pixels = image.as_bytes(0).unwrap();
        assert_eq!(&pixels[..4], [10, 30, 200, 255]);
        assert_eq!(&pixels[pixels.len() - 4..], [150, 80, 0, 255]);
        set_own(["mara@example.org"]);
        assert!(lookup("mara@example.org").is_none());
        STORE.with_borrow_mut(|store| *store = None);
    }

    #[test]
    fn old_results_cannot_fill_a_replacement_sessions_pending_lookup() {
        let bytes = png(2, 1);
        let (old, mut old_results) = install(move |_| decode(&bytes).map(|image| (image, true)));
        set_own(["me@example.org"]);
        lookup("sender@example.org");
        let old_result = receive(&mut old_results);

        let bytes = png(2, 1);
        let (current, mut current_results) = install(move |_| decode(&bytes).map(|image| (image, false)));
        set_own(["me@example.org"]);
        lookup("sender@example.org");
        accept_result(&old, old_result.0, old_result.1);
        assert!(lookup("sender@example.org").is_none(), "old photo must not appear in the new session");
        let (email, picture) = receive(&mut current_results);
        accept_result(&current, email, picture);
        assert!(!lookup("sender@example.org").unwrap().1, "only the new session's photo is visible");
        STORE.with_borrow_mut(|store| *store = None);
    }

    #[test]
    fn scrolling_is_bounded_and_replacement_cancels_queued_work() {
        let calls = Arc::new(AtomicUsize::new(0));
        let gate = Arc::new((Mutex::new(false), Condvar::new()));
        let (started, observed) = mpsc::sync_channel(4);
        let bytes = png(2, 1);
        let (_, mut old_results) = install({
            let (calls, gate) = (calls.clone(), gate.clone());
            move |_| {
                calls.fetch_add(1, Ordering::SeqCst);
                started.send(()).unwrap();
                let mut released = gate.0.lock().unwrap();
                while !*released {
                    released = gate.1.wait(released).unwrap();
                }
                decode(&bytes).map(|image| (image, false))
            }
        });
        set_own(["me@example.org"]);
        for i in 0..4 {
            lookup(&format!("sender{i}@example.org"));
        }
        for _ in 0..4 {
            observed.recv_timeout(Duration::from_secs(5)).unwrap();
        }
        for i in 4..1000 {
            lookup(&format!("sender{i}@example.org"));
        }
        STORE.with_borrow(|store| assert_eq!(store.as_ref().unwrap().states.len(), 36, "four active and 32 queued lookups"));

        let (started, observed) = mpsc::sync_channel(1);
        let bytes = png(2, 1);
        let (current, mut current_results) = install(move |_| {
            started.send(()).unwrap();
            decode(&bytes).map(|image| (image, false))
        });
        set_own(["me@example.org"]);
        lookup("next@example.org");
        assert_eq!(observed.recv_timeout(Duration::from_millis(100)), Err(mpsc::RecvTimeoutError::Timeout), "four old requests still occupy the shared limit");
        *gate.0.lock().unwrap() = true;
        gate.1.notify_all();
        let (email, picture) = receive(&mut current_results);
        accept_result(&current, email, picture);
        assert!(lookup("next@example.org").is_some());
        assert!(futures::executor::block_on(old_results.next()).is_none(), "cancelled results are discarded and the old workers exit");
        assert_eq!(calls.load(Ordering::SeqCst), 4, "no old queued requests were fetched");
        STORE.with_borrow_mut(|store| *store = None);
    }

    #[test]
    fn cache_keeps_recent_photos_and_evicted_addresses_can_be_retried() {
        let bytes = png(2, 1);
        let (session, mut arrived) = install(move |_| decode(&bytes).map(|image| (image, false)));
        set_own(["me@example.org"]);
        for i in 0..256 {
            assert!(lookup(&format!("sender{i}@example.org")).is_none());
            let (email, picture) = receive(&mut arrived);
            accept_result(&session, email, picture);
        }
        assert!(lookup("sender0@example.org").is_some());
        lookup("extra@example.org");
        let (email, picture) = receive(&mut arrived);
        accept_result(&session, email, picture);
        assert!(lookup("sender0@example.org").is_some(), "recently read photo survives eviction");
        assert!(lookup("sender1@example.org").is_none(), "the least recently read photo was evicted");
        let (email, picture) = receive(&mut arrived);
        accept_result(&session, email, picture);
        assert!(lookup("sender1@example.org").is_some(), "eviction must not permanently suppress lookup");
        STORE.with_borrow(|store| assert_eq!(store.as_ref().unwrap().states.len(), 256));
        STORE.with_borrow_mut(|store| *store = None);
    }

    #[test]
    fn compressed_images_must_fit_decoded_dimension_limits() {
        for (width, height) in [(2048, 1), (1, 2048)] {
            assert!(decode(&png(width, height)).is_some(), "dimension boundary is accepted");
        }
        for (width, height) in [(2049, 1), (1, 2049)] {
            let bytes = png(width, height);
            assert!(bytes.len() < 2 << 20, "encoded size alone would allow this image");
            assert!(decode(&bytes).is_none(), "oversized decoded dimensions are rejected");
        }
    }
}
