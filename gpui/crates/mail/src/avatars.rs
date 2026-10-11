//! Avatars for senders, as durian's macOS app has them: a Gravatar photo for addresses at
//! personal mail providers, the company's logo (Brandfetch) for everyone else, initials when
//! neither exists.
//!
//! These lookups leave the machine – the photo services learn which addresses and domains
//! appear in the mailbox, as in the Swift app. Two limits it doesn't have:
//! - never for the user's own addresses (nothing is fetched until they're known)
//! - in memory only: no picture files, whose names would list the correspondents
//!
//! Requests run on a few worker threads; the UI asks during render ([`lookup`]) and repaints
//! when a picture arrives. A missing picture isn't asked for again this session.

use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, mpsc};
use std::time::Duration;

use futures::StreamExt;
use gpui::{App, RenderImage};

/// Address domains of personal mail providers: these get Gravatar, others a company logo.
const PERSONAL: &[&str] = &[
    "gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "live.com", "msn.com", "outlook.de", "yahoo.com", "yahoo.de", "ymail.com",
    "gmx.de", "gmx.net", "gmx.at", "gmx.ch", "web.de", "t-online.de", "freenet.de", "mail.de", "email.de", "icloud.com", "me.com", "mac.com",
    "aol.com", "protonmail.com", "proton.me", "pm.me", "posteo.de", "mailbox.org", "tutanota.com", "tutanota.de", "tuta.io",
];

/// durian's Brandfetch client id (the Swift app's).
const BRANDFETCH: &str = "1idWonATCJFIseiVHIH";

/// Brandfetch answers unknown domains with a generic picture instead of a 404.
const PLACEHOLDERS: &[&str] = &["38926c4ecbd5590c77a969ae5a516b49", "ad18dbe2bac10daa8e99cc27efed1607"];

const WORKERS: usize = 4;
const PIXELS: u32 = 128;

/// Where a sender's picture comes from, and whether it's a logo (drawn inset on a disc).
#[derive(Debug, PartialEq)]
pub enum Source {
    Gravatar(String),
    Logo(String),
}

impl Source {
    pub fn for_address(email: &str) -> Option<Self> {
        let email = email.trim().to_lowercase();
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

struct Store {
    states: HashMap<String, State>,
    /// the user's addresses: never looked up; until known (None), nothing is
    own: Option<HashSet<String>>,
    requests: mpsc::Sender<String>,
}

thread_local! {
    // the UI thread's: render asks, the result task fills
    static STORE: RefCell<Option<Store>> = const { RefCell::new(None) };
}

/// Start the workers. Pictures are looked up once [`set_own`] said whose not to.
pub fn init(cx: &mut App) {
    let (requests, incoming) = mpsc::channel::<String>();
    let incoming = Arc::new(Mutex::new(incoming));
    let (results, mut arrived) = futures::channel::mpsc::unbounded::<(String, Option<(Arc<RenderImage>, bool)>)>();
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(8)))
        // Brandfetch serves browsers only
        .user_agent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko)")
        .http_status_as_error(false)
        .build()
        .into();
    for _ in 0..WORKERS {
        let (incoming, results, agent) = (incoming.clone(), results.clone(), agent.clone());
        std::thread::spawn(move || {
            loop {
                let Ok(email) = incoming.lock().map_err(|_| ()).and_then(|rx| rx.recv().map_err(|_| ())) else { break };
                let picture = Source::for_address(&email).and_then(|source| fetch(&agent, &source).map(|image| (image, source.is_logo())));
                if results.unbounded_send((email, picture)).is_err() {
                    break;
                }
            }
        });
    }
    STORE.with_borrow_mut(|store| *store = Some(Store { states: HashMap::new(), own: None, requests }));
    cx.spawn(async move |cx| {
        while let Some((email, picture)) = arrived.next().await {
            STORE.with_borrow_mut(|store| {
                if let Some(store) = store {
                    store.states.insert(email, picture.map_or(State::Missing, |(image, logo)| State::Ready(image, logo)));
                }
            });
            cx.update(|cx| cx.refresh_windows());
        }
    })
    .detach();
}

/// The user's own addresses (never looked up). Until this is called, nothing is.
pub fn set_own(addresses: impl IntoIterator<Item = impl AsRef<str>>) {
    STORE.with_borrow_mut(|store| {
        if let Some(store) = store {
            store.own = Some(addresses.into_iter().map(|a| a.as_ref().trim().to_lowercase()).collect());
        }
    });
}

/// The picture for an address if there is one yet (asked for if not). None without `init`.
pub fn lookup(email: &str) -> Option<(Arc<RenderImage>, bool)> {
    let email = email.trim().to_lowercase();
    STORE.with_borrow_mut(|store| {
        let store = store.as_mut()?;
        let own = store.own.as_ref()?;
        if own.contains(&email) || Source::for_address(&email).is_none() {
            return None;
        }
        match store.states.get(&email) {
            Some(State::Ready(image, logo)) => Some((image.clone(), *logo)),
            Some(_) => None,
            None => {
                store.states.insert(email.clone(), State::Pending);
                let _ = store.requests.send(email);
                None
            }
        }
    })
}

/// Download and decode (GPUI wants BGRA). None: no picture there (or a placeholder).
fn fetch(agent: &ureq::Agent, source: &Source) -> Option<Arc<RenderImage>> {
    let mut response = agent.get(source.url()).call().ok()?;
    if response.status().as_u16() != 200 {
        return None;
    }
    let bytes = response.body_mut().with_config().limit(2 << 20).read_to_vec().ok()?;
    if source.is_logo() && PLACEHOLDERS.contains(&format!("{:x}", md5::compute(&bytes)).as_str()) {
        return None;
    }
    let mut rgba = image::load_from_memory(&bytes).ok()?.thumbnail(PIXELS, PIXELS).into_rgba8();
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

    #[test]
    fn personal_addresses_get_gravatar_others_a_logo() {
        assert_eq!(Source::for_address(" Ada@GMAIL.com"), Some(Source::Gravatar("ada@gmail.com".into())));
        assert_eq!(Source::for_address("billing@stripe.com"), Some(Source::Logo("stripe.com".into())));
        assert_eq!(Source::for_address("no-at-sign"), None);
        assert_eq!(Source::for_address("root@localhost"), None);
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
        let (requests, received) = mpsc::channel();
        STORE.with_borrow_mut(|store| *store = Some(Store { states: HashMap::new(), own: None, requests }));
        assert!(lookup("mara@example.org").is_none());
        assert!(received.try_recv().is_err(), "no request without the own addresses");
        set_own(["Me@Work.dev"]);
        assert!(lookup("me@work.dev").is_none());
        assert!(received.try_recv().is_err(), "never the user's own address");
        assert!(lookup("mara@example.org").is_none());
        assert_eq!(received.try_recv().as_deref(), Ok("mara@example.org"));
        assert!(lookup("mara@example.org").is_none());
        assert!(received.try_recv().is_err(), "asked once");
    }
}
