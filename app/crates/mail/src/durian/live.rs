//! The connection to durian at runtime. Every list is a server search: the view's base query
//! plus the translated search, refined locally (`translate`). The UI thread never waits on the
//! network – requests run on the background executor and their answers are applied when
//! they're still wanted:
//!
//! - **searches** carry a generation; an answer for anything but the latest is dropped.
//!   Keystrokes in quick succession are debounced, a single change goes out at once.
//! - **paging**: the list asks for `PAGE` more rows when the selection or the scroll gets
//!   near its end (until durian has cursors this re-requests the longer list, see
//!   `docs/durian-api-notes.md`).
//! - **threads** load when opened, a few ahead are prefetched, at most `IN_FLIGHT` at once,
//!   never twice. Full threads stay in a bounded cache, checked against the search row.
//! - **calendar**: the shown period's events load with a margin around it, again when the
//!   view leaves it. An event's guests and organizer (whose event is it, what did you answer)
//!   load when it's selected, once per series.
//!
//! Logging names counts, ids and timings – never subjects, addresses or bodies.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, atomic::{AtomicBool, Ordering}};
use std::time::{Duration, Instant};

use calendar::model::{Attendee, CalendarInfo, Rsvp, Schedule, ScheduleEvent};
use chrono::{Local, NaiveDate, TimeZone};
use gpui::{Context, Entity, EventEmitter, SharedString, Subscription, Task};

use super::cache::{Stamp, ThreadCache};
use super::client::{self, Client};
use super::events;
use super::writes::{self, Intent, Outcome};
use super::translate::{account_scope, combine, translate};
use super::profiles::{self, Profile};
use super::{EventRef, api, calendar_event, calendar_id, event_detail, hashed_color, parse_address};
use crate::attachments::{self, Use, safe_name};
use crate::model::{Account, Attachment, Contact, Loading, Mailbox, MailboxEvent, Remote, Tag, Thread};
use crate::query::{Query, Term};
use crate::trace;

/// What an empty search shows.
pub const INBOX: &str = "tag:inbox";
/// Several screens with previews, rather than a large, mostly empty first page.
pub const PAGE: usize = 50;
/// Full threads kept in memory.
const CACHE: usize = 300;
/// Thread requests at once: the open one always goes, prefetch fills up to this.
const IN_FLIGHT: usize = 4;
/// Threads below the selection loaded ahead of time.
const PREFETCH: usize = 3;
/// Rows from the end of the list at which the next page is requested.
const PAGE_AHEAD: usize = 30;
/// Query changes closer together than this are one search (typing).
const DEBOUNCE: Duration = Duration::from_millis(150);
/// A word left unfinished this long counts as complete: the server only matches whole words.
const SETTLE: Duration = Duration::from_millis(600);
/// Days of events loaded before and after the shown period (navigating a few weeks either
/// way needs no request).
const CALENDAR_BEHIND: i64 = 28;
const CALENDAR_AHEAD: i64 = 56;
/// SSE drops frames and external tag edits don't broadcast. Reconcile even a
/// healthy stream every 60 seconds; postpone while a list request is active.
const RECONCILE: Duration = Duration::from_secs(60);
/// Count requests at once, independently of the mail list's search/prefetch.
const COUNT_IN_FLIGHT: usize = 4;

#[derive(Clone, Debug, PartialEq)]
pub enum Status {
    Connecting,
    Connected { version: SharedString },
    Failed(SharedString),
}

/// What the user should hear about that isn't a list's or a thread's state.
pub enum LiveEvent {
    Problem(SharedString),
    Queued { draft_id: usize, item: api::Enqueued },
    Undone(usize),
    Delivery { item_id: i64, state: writes::Delivery },
    ForwardReady(usize),
    /// an attachment went to ~/Downloads, here
    Downloaded(std::path::PathBuf),
}

/// An event's detail: description, guests, your answer, explicit organizer authority.
type Detail = (Option<SharedString>, Vec<Attendee>, Rsvp, bool);

/// The connect, started before the window exists (`Live::preconnect` at the top of `main`):
/// window bring-up (~150–450 ms) and the first requests overlap instead of adding up.
pub struct Preconnect {
    client: Arc<Client>,
    identity: Receiver<client::Result<Identity>>,
    /// what the start thread reports, in order
    start: Receiver<Start>,
    started: Instant,
}

/// The start thread's reports, in this order: its query and profiles (the server's, else
/// profiles.pkl from the cache), the start folder's first page (and the query it answers), and
/// – only when profiles.pkl had to be evaluated – the evaluated profiles. One task applies
/// them in order, so a page never arrives before its profiles.
enum Start {
    Searching(String, Option<Vec<Profile>>),
    Profiles(Vec<Profile>),
    Page(String, client::Result<api::SearchResponse>),
    Problem(String),
}

type Receiver<T> = std::sync::mpsc::Receiver<T>;

/// The speculative startup request owns its query until it answers or a refresh
/// invalidates it. A different user query need not wait for that request.
enum FirstPage {
    Planning,
    Pending(String),
    Ready(String, api::SearchResponse),
    Done,
}

impl FirstPage {
    fn pending(&self) -> bool {
        matches!(self, Self::Planning | Self::Pending(_))
    }
}

pub struct Live {
    client: Arc<Client>,
    /// the connect started before the window, until it's been waited for
    preconnect: Option<Preconnect>,
    /// applies the start thread's reports
    start_task: Option<Task<()>>,
    /// MAIL_FOLDER applies to the first profiles only
    profiles_applied: bool,
    mailbox: Entity<Mailbox>,
    schedule: Entity<Schedule>,
    pub status: Status,
    /// your addresses on the server, for `from:me` / `to:me`
    me: Vec<SharedString>,
    generation: u64,
    /// Independent of search generations: paging doesn't invalidate full threads.
    thread_epoch: u64,
    calendar_epoch: u64,
    mail_refreshing: bool,
    events: Option<events::Worker>,
    event_task: Option<Task<()>>,
    reconciled_at: Instant,
    count_context: Option<(Option<usize>, Option<SharedString>, Vec<String>)>,
    count_generation: u64,
    count_pending: bool,
    count_task: Option<Task<()>>,
    count_cancel: Option<Arc<AtomicBool>>,
    write_queue: writes::Queue,
    write_tasks: HashMap<String, Task<()>>,
    draft_task: Option<Task<()>>,
    compose_settings_task: Option<Task<()>>,
    outbox_task: Option<Task<()>>,
    delivery: HashMap<i64, writes::Delivery>,
    forward_task: Option<Task<()>>,
    last_change: Option<Instant>,
    /// the search in flight (dropping it drops its answer)
    search: Option<Task<()>>,
    /// Also stop superseded preview batches still running off the UI thread.
    search_cancel: Option<Arc<AtomicBool>>,
    /// waits for typing to pause, then searches for the unfinished word too
    settle: Option<Task<()>>,
    connect: Option<Task<()>>,
    fetches: HashMap<SharedString, Task<()>>,
    cache: ThreadCache,
    /// the latest search row of every listed thread (what a fetched thread is cached under)
    stamps: HashMap<SharedString, Stamp>,
    /// days whose events are listed [from, to), and the window on its way
    calendar_loaded: Option<(NaiveDate, NaiveDate)>,
    calendar_pending: Option<(NaiveDate, NaiveDate)>,
    calendar_task: Option<Task<()>>,
    /// where each listed event lives on the server
    refs: HashMap<SharedString, EventRef>,
    /// details by account/calendar/iCalUID: loaded and in flight
    details: HashMap<EventRef, Detail>,
    detail_tasks: HashMap<EventRef, Task<()>>,
    /// attachments on their way, by (message, part)
    attachment_tasks: HashMap<(SharedString, i64), Task<()>>,
    /// embedded images being fetched, per thread
    image_tasks: HashMap<SharedString, Task<()>>,
    first_page: FirstPage,
    /// how mail HTML is shown, from the server's settings once connected
    policy: crate::html_doc::Policy,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<LiveEvent> for Live {}

impl Drop for Live {
    fn drop(&mut self) {
        if let Some(cancel) = &self.search_cancel {
            cancel.store(true, Ordering::Relaxed);
        }
        if let Some(cancel) = &self.count_cancel {
            cancel.store(true, Ordering::Relaxed);
        }
    }
}

/// The empty, loading list a live mailbox starts with.
pub fn initial_remote() -> Remote {
    Remote { server_query: String::new(), refine: Query::default(), limit: PAGE, loading: Some(Loading::Pending), complete: false, threads: HashMap::new() }
}

impl Live {
    /// Start connecting on threads of their own, before GPUI or the window exist: the inbox's
    /// first page and who you are, independently – the list shows as soon as its page is in.
    ///
    /// The start folder is the default profile's first one – known right away when the
    /// profiles come from the cache; else the plain inbox until they're evaluated.
    pub fn preconnect(client: Client) -> Preconnect {
        let started = Instant::now();
        let client = Arc::new(client);
        let (start_tx, start) = std::sync::mpsc::channel();
        let (identity_tx, identity) = std::sync::mpsc::channel();
        let (profiles_tx, profiles_rx) = std::sync::mpsc::channel();
        let (c1, c2) = (client.clone(), client.clone());
        std::thread::spawn(move || {
            let _ = identity_tx.send(identify(&c1, || profiles_rx.recv().unwrap_or_else(|_| Err(client::Error::Decode("profiles thread ended".into())))));
        });
        std::thread::spawn(move || {
            // profiles: the server's; from a server without /profiles (or none reachable),
            // profiles.pkl – cached, or evaluated after the first page (pkl takes 1–2 s)
            let cache = crate::cache_dir("durian");
            let mut evaluate = None;
            // Scope and identity must use the same configuration answer, not two
            // concurrent evaluations which can even observe different file revisions.
            let response = c2.profiles();
            let _ = profiles_tx.send(response.clone());
            let profiles = match response {
                Ok(response) => Some(profiles::from_api(&response)),
                Err(_) => {
                    let config = profiles::config_dir();
                    let cached = config.as_deref().and_then(|config| profiles::cached(config, &cache));
                    if cached.is_none() {
                        evaluate = config;
                    }
                    cached
                }
            };
            let home = profiles.as_deref().and_then(|p| profiles::default_index(p).map(|ix| &p[ix]));
            let first = compose(home.and_then(profiles::scope).as_deref(), home.and_then(|p| p.home()).map(|f| f.query.as_ref()), "");
            let _ = start_tx.send(Start::Searching(first.clone(), profiles));
            let page = c2.search(&first, PAGE, PAGE);
            let _ = start_tx.send(Start::Page(first, page));
            if let Some(config) = evaluate {
                match profiles::evaluate(&config, &cache) {
                    Ok(Some(profiles)) => drop(start_tx.send(Start::Profiles(profiles))),
                    Ok(None) => {}
                    Err(message) => drop(start_tx.send(Start::Problem(message))),
                }
            }
        });
        Preconnect { client, identity, start, started }
    }

    pub fn new(preconnect: Preconnect, mailbox: Entity<Mailbox>, schedule: Entity<Schedule>, cx: &mut Context<Self>) -> Self {
        let subscriptions = vec![
            // the shown period moved (or anything else changed): load its events if needed
            cx.observe(&schedule, |this, _, cx| this.ensure_calendar(cx)),
            cx.subscribe(&schedule, |this, _, event, cx| {
                match event {
                    ScheduleEvent::SelectionChanged => this.event_selected(cx),
                    ScheduleEvent::TimeZoneChanged => this.refresh_calendar(cx),
                    _ => {}
                }
            }),
            cx.subscribe(&mailbox, Self::mailbox_event),
        ];
        let mut this = Self {
            client: preconnect.client.clone(),
            preconnect: Some(preconnect),
            start_task: None,
            profiles_applied: false,
            mailbox,
            schedule,
            status: Status::Connecting,
            me: Vec::new(),
            generation: 0,
            thread_epoch: 0,
            calendar_epoch: 0,
            mail_refreshing: false,
            events: None,
            event_task: None,
            reconciled_at: Instant::now(),
            count_context: None,
            count_generation: 0,
            count_pending: false,
            count_task: None,
            count_cancel: None,
            write_queue: Default::default(),
            write_tasks: HashMap::new(),
            draft_task: None,
            compose_settings_task: None,
            outbox_task: None,
            delivery: HashMap::new(),
            forward_task: None,
            last_change: None,
            search: None,
            search_cancel: None,
            settle: None,
            connect: None,
            fetches: HashMap::new(),
            cache: ThreadCache::new(CACHE),
            stamps: HashMap::new(),
            calendar_loaded: None,
            calendar_pending: None,
            calendar_task: None,
            refs: HashMap::new(),
            details: HashMap::new(),
            detail_tasks: HashMap::new(),
            attachment_tasks: HashMap::new(),
            image_tasks: HashMap::new(),
            first_page: FirstPage::Planning,
            policy: Default::default(),
            _subscriptions: subscriptions,
        };
        this.connect(cx);
        this
    }

    fn mailbox_event(&mut self, _: Entity<Mailbox>, event: &MailboxEvent, cx: &mut Context<Self>) {
        match event {
            MailboxEvent::QueryChanged => {
                self.query_changed(cx);
                // The count context excludes typed text and the selected folder:
                // keystrokes/paging don't request counts, scope changes do.
                self.refresh_counts(false, cx);
            }
            MailboxEvent::SelectionChanged => self.selection_changed(cx),
            MailboxEvent::MoreWanted => self.load_more(cx),
            MailboxEvent::WriteRequested(intent) => {
                self.queue_write(intent.clone(), cx);
                self.start_writes(cx);
            }
            MailboxEvent::ReloadComposeSettings => self.refresh_compose_settings(cx),
            MailboxEvent::ThreadLoaded(_) | MailboxEvent::ReadOnly | MailboxEvent::DraftSaved(_) | MailboxEvent::DraftDeleted(_) | MailboxEvent::WriteProblem(_) | MailboxEvent::SendFinished { .. } => {}
        }
    }

    pub fn client(&self) -> Arc<Client> { self.client.clone() }
    pub fn backend_available(&self) -> bool { matches!(self.status, Status::Connected { .. }) }
    pub fn delivery(&self, item_id: i64) -> Option<&writes::Delivery> { self.delivery.get(&item_id) }

    fn queue_write(&mut self, intent: Intent, cx: &mut Context<Self>) {
        if let Err(error) = self.write_queue.push(intent.clone()) { self.write_finished(intent, Err(error), cx); }
    }

    /// Keep writer tasks/queue and mailbox/editors alive across backend replacement.
    /// Tasks already dispatched finish against their captured client; new requests
    /// use the replacement. Unknown sends keep their durable original key/payload.
    pub fn reconnect(&mut self, preconnect: Preconnect, cx: &mut Context<Self>) {
        self.backend_unavailable("Reconnecting to durian".into(), cx);
        self.client = preconnect.client.clone();
        self.preconnect = Some(preconnect);
        self.connect(cx);
    }

    pub fn backend_unavailable(&mut self, message: SharedString, cx: &mut Context<Self>) {
        self.status = Status::Failed(message.clone());
        self.connect = None;
        self.start_task = None;
        self.events = None;
        self.event_task = None;
        self.search = None;
        if let Some(cancel) = self.search_cancel.take() { cancel.store(true, Ordering::Relaxed); }
        self.settle = None;
        self.mail_refreshing = true;
        self.generation += 1;
        self.invalidate_threads(cx);
        self.refresh_calendar(cx);
        self.count_generation += 1;
        self.count_pending = false;
        if let Some(cancel) = &self.count_cancel { cancel.store(true, Ordering::Relaxed); }
        self.draft_task = None;
        self.compose_settings_task = None;
        self.outbox_task = None;
        self.update_remote(cx, |remote| remote.loading = Some(Loading::Failed(message)));
        self.mailbox.update(cx, |m, cx| { m.writes_available = false; cx.notify(); });
        cx.notify();
    }

    fn start_writes(&mut self, cx: &mut Context<Self>) {
        if !matches!(self.status, Status::Connected { .. }) { return }
        while let Some((key, mut intent)) = self.write_queue.next() {
            let client = self.client.clone();
            let executor = cx.background_executor().clone();
            let task_key = key.clone();
            let task = cx.spawn(async move |this, cx| {
                let (intent, result) = executor.spawn(async move {
                    let result = writes::execute(&client, &mut intent);
                    (intent, result)
                }).await;
                this.update(cx, |this, cx| {
                    this.write_tasks.remove(&task_key);
                    this.write_queue.finished(&task_key);
                    this.write_finished(intent, result, cx);
                    this.start_writes(cx);
                }).ok();
            });
            self.write_tasks.insert(key, task);
        }
    }

    fn write_finished(&mut self, intent: Intent, result: client::Result<Outcome>, cx: &mut Context<Self>) {
        let problem = result.as_ref().err().map(ToString::to_string);
        match intent {
            Intent::Save { draft } => {
                if result.is_ok() {
                    self.mailbox.update(cx, |m, cx| {
                        if let Some(d) = m.drafts.iter().find(|d| d.wire == draft) { cx.emit(MailboxEvent::DraftSaved(d.id)); }
                    });
                }
            }
            Intent::Delete { local_id, .. } => {
                self.mailbox.update(cx, |m, cx| {
                    if result.is_ok() { m.drafts.retain(|d| d.id != local_id); cx.emit(MailboxEvent::DraftDeleted(local_id)); }
                    else if let Some(d) = m.drafts.iter_mut().find(|d| d.id == local_id) { d.deleting = false; }
                    cx.notify();
                });
            }
            Intent::Send { draft, local_id, .. } => {
                let sent = match result { Ok(Outcome::Sent(item)) => Ok(item), Err(e) => Err(e), _ => unreachable!() };
                self.mailbox.update(cx, |m, cx| {
                    if let Some(d) = m.drafts.iter_mut().find(|d| d.id == local_id) {
                        d.wire = draft;
                        d.sending = false;
                        d.queued = sent.as_ref().ok().cloned();
                        if d.queued.is_some() {
                            // The receipt remains recoverable in Drafts, but is
                            // no longer the inline editor for the next reply.
                            d.kind = crate::model::DraftKind::New {
                                account: d.wire.account_id.clone().unwrap_or_default().into(), from: d.wire.from.clone().into(),
                                to: d.wire.to.iter().cloned().map(Into::into).collect(), cc: d.wire.cc.iter().cloned().map(Into::into).collect(),
                                bcc: d.wire.bcc.iter().cloned().map(Into::into).collect(), subject: d.wire.subject.clone().into(),
                            };
                        }
                    }
                    cx.emit(MailboxEvent::SendFinished { draft_id: local_id, result: sent.clone() });
                    cx.notify();
                });
                if let Ok(item) = sent {
                    let state = self.delivery.entry(item.id).or_insert(writes::Delivery::Queued).clone();
                    if state == writes::Delivery::Queued { cx.emit(LiveEvent::Queued { draft_id: local_id, item }); }
                    else { cx.emit(LiveEvent::Delivery { item_id: item.id, state }); }
                    self.reconcile_outbox(cx);
                }
            }
            Intent::Undo { id, draft, local_id } => {
                if let Ok(Outcome::Undone { persisted }) = result {
                    self.delivery.remove(&id);
                    self.mailbox.update(cx, |m, cx| {
                        if let Some(d) = m.drafts.iter_mut().find(|d| d.id == local_id) { d.wire = draft; d.queued = None; d.sending = false; }
                        cx.notify();
                    });
                    cx.emit(LiveEvent::Undone(local_id));
                    if !persisted { cx.emit(LiveEvent::Problem("Delivery cancelled, but draft recovery save failed; keep this editor open and save again.".into())); }
                }
            }
            Intent::Tags { thread, ops } => {
                self.mailbox.update(cx, |m, _| {
                    if result.is_ok() {
                        if let Some(confirmed) = m.confirmed_tags.get_mut(thread.as_str()) { writes::apply_tags(confirmed, &ops); }
                    }
                });
                if !self.write_queue.pending_thread(&thread) {
                    self.mailbox.update(cx, |m, cx| {
                        m.pending_tags.remove(thread.as_str());
                        if let Some(confirmed) = m.confirmed_tags.remove(thread.as_str()) {
                            if let Some(shown) = m.threads.iter_mut().find(|t| t.id == thread) { *shown = confirmed; }
                        }
                        cx.notify();
                    });
                    self.refresh_mail(cx);
                }
            }
        }
        if let Some(problem) = problem {
            self.mailbox.update(cx, |_, cx| cx.emit(MailboxEvent::WriteProblem(problem.clone().into())));
        }
    }

    pub fn undo_send(&mut self, local_id: usize, cx: &mut Context<Self>) {
        if !matches!(self.status, Status::Connected { .. }) { cx.emit(LiveEvent::Problem("Backend unavailable; cancellation not confirmed".into())); return }
        let Some(draft) = self.mailbox.read(cx).drafts.iter().find(|d| d.id == local_id).cloned() else { return };
        let Some(item) = draft.queued else { return };
        self.queue_write(Intent::Undo { id: item.id, draft: draft.wire, local_id }, cx);
        self.start_writes(cx);
    }

    pub fn forward_selected(&mut self, cx: &mut Context<Self>) {
        if !self.backend_available() { cx.emit(LiveEvent::Problem("Backend unavailable; forward not loaded".into())); return }
        if self.forward_task.is_some() { return }
        let mailbox = self.mailbox.read(cx);
        let Some(thread) = mailbox.selected_thread() else { return };
        if thread.partial { cx.emit(LiveEvent::Problem("Wait for the full thread before forwarding".into())); return }
        let message = thread.latest();
        let Some(account) = message.account.as_ref().and_then(|id| mailbox.account(id)).or_else(|| mailbox.sending_account(Some(thread))) else { return };
        let (thread_id, target, account, from) = (thread.id.to_string(), message.id.to_string(), account.id.to_string(), account.email.to_string());
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        self.forward_task = Some(cx.spawn(async move |this, cx| {
            let result = executor.spawn(async move { writes::forward(&client, &thread_id, &target, account, from) }).await;
            this.update(cx, |this, cx| {
                this.forward_task = None;
                match result {
                    Ok(mut wire) => {
                        this.mailbox.read(cx).initialize_signature(&mut wire);
                        let kind = crate::model::DraftKind::New { account: wire.account_id.clone().unwrap_or_default().into(), from: wire.from.clone().into(),
                            to: Vec::new(), cc: Vec::new(), bcc: Vec::new(), subject: wire.subject.clone().into() };
                        if let Some(id) = this.mailbox.update(cx, |m, cx| m.save_draft_content(None, kind, wire.body.clone().into(), wire, cx)) {
                            cx.emit(LiveEvent::ForwardReady(id));
                        }
                    }
                    Err(e) => cx.emit(LiveEvent::Problem(format!("Forward not created: {e}").into())),
                }
            }).ok();
        }));
    }

    fn refresh_compose_settings(&mut self, cx: &mut Context<Self>) {
        if self.compose_settings_task.is_some() || !self.backend_available() { return }
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        self.compose_settings_task = Some(cx.spawn(async move |this, cx| {
            let settings = executor.spawn(async move { client.compose_settings() }).await;
            this.update(cx, |this, cx| {
                this.compose_settings_task = None;
                this.mailbox.update(cx, |mailbox, cx| {
                    mailbox.compose_settings = match settings {
                        Ok(settings) => crate::signatures::Settings::Ready(settings),
                        Err(client::Error::NotFound(_)) => crate::signatures::Settings::Unavailable("Signature settings unavailable. Update the Durian CLI or choose None.".into()),
                        Err(_) => crate::signatures::Settings::Unavailable("Couldn't load signatures. Retry or choose None; saved signatures are retained.".into()),
                    };
                    cx.notify();
                });
            }).ok();
        }));
    }

    fn load_drafts(&mut self, cx: &mut Context<Self>) {
        if self.draft_task.is_some() { return }
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        self.draft_task = Some(cx.spawn(async move |this, cx| {
            let result = executor.spawn(async move { client.drafts() }).await;
            this.update(cx, |this, cx| {
                this.draft_task = None;
                match result {
                    Ok(drafts) => this.mailbox.update(cx, |m, cx| m.load_drafts(drafts, cx)),
                    Err(e) => cx.emit(LiveEvent::Problem(format!("Draft recovery unavailable: {e}").into())),
                }
                this.reconcile_outbox(cx);
            }).ok();
        }));
    }

    pub fn reconcile_outbox(&mut self, cx: &mut Context<Self>) {
        if self.outbox_task.is_some() || !self.backend_available() { return }
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        self.outbox_task = Some(cx.spawn(async move |this, cx| {
            let result = executor.spawn(async move { client.outbox() }).await;
            this.update(cx, |this, cx| {
                this.outbox_task = None;
                let Ok(entries) = result else { return }; // keep known status on transient errors
                let ids: Vec<i64> = this.mailbox.read(cx).drafts.iter().filter_map(|d| d.queued.as_ref().map(|q| q.id)).collect();
                for id in ids {
                    let state = writes::Delivery::reconcile(this.delivery.get(&id), entries.iter().find(|e| e.id == id));
                    this.apply_delivery(id, state, cx);
                }
            }).ok();
        }));
    }

    fn apply_delivery(&mut self, item_id: i64, state: writes::Delivery, cx: &mut Context<Self>) {
        if self.delivery.get(&item_id) == Some(&state) { return }
        if self.delivery.len() >= 128 && !self.delivery.contains_key(&item_id) {
            if let Some(id) = self.delivery.keys().next().copied() { self.delivery.remove(&id); }
        }
        self.delivery.insert(item_id, state.clone());
        let tracked = self.mailbox.read(cx).drafts.iter().any(|d| d.queued.as_ref().is_some_and(|q| q.id == item_id));
        if tracked { cx.emit(LiveEvent::Delivery { item_id, state }); }
    }

    fn update_remote(&self, cx: &mut Context<Self>, f: impl FnOnce(&mut Remote)) {
        self.mailbox.update(cx, |m, cx| {
            if let Some(remote) = m.remote.as_mut() {
                f(remote);
                cx.notify();
            }
        });
    }

    /// Who you are and what there is: version, tags, accounts, your addresses. Then the
    /// first search.
    fn connect(&mut self, cx: &mut Context<Self>) {
        self.status = Status::Connecting;
        self.update_remote(cx, |r| r.loading = Some(Loading::Pending));
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let ended = || client::Error::Decode("connect thread ended".into());
        match self.preconnect.take() {
            // the first time: the answers of the connect already under way
            Some(Preconnect { identity, start, started, .. }) => {
                let reports = executor.clone();
                self.start_task = Some(cx.spawn(async move |this, cx| {
                    let mut start = start;
                    loop {
                        let (report, back) = reports
                            .spawn(async move {
                                let report = start.recv().ok();
                                (report, start)
                            })
                            .await;
                        start = back;
                        let Some(report) = report else { break };
                        let applied = this.update(cx, |this, cx| match report {
                            Start::Searching(query, profiles) => {
                                // Install ownership before applying profiles: their
                                // QueryChanged event can otherwise dispatch a duplicate.
                                if matches!(this.first_page, FirstPage::Planning) {
                                    this.first_page = FirstPage::Pending(query);
                                }
                                if let Some(profiles) = profiles {
                                    this.apply_profiles(profiles, started.elapsed(), cx);
                                }
                                this.plan(false, Duration::ZERO, cx);
                            }
                            Start::Profiles(profiles) => this.apply_profiles(profiles, started.elapsed(), cx),
                            Start::Page(query, page) => this.early_page(query, page, started.elapsed(), cx),
                            Start::Problem(message) => cx.emit(LiveEvent::Problem(message.into())),
                        });
                        if applied.is_err() {
                            break;
                        }
                    }
                    this.update(cx, |this, cx| {
                        this.start_task = None;
                        // A worker ending without its page must not leave a loading
                        // list forever. Normal errors arrive through Start::Page.
                        if this.first_page.pending() {
                            this.first_page = FirstPage::Done;
                            this.plan(false, Duration::ZERO, cx);
                            if this.backend_available() && this.search.is_none() {
                                this.search(Duration::ZERO, cx);
                            }
                        }
                    }).ok();
                }));
                self.connect = Some(cx.spawn(async move |this, cx| {
                    let identity = executor.spawn(async move { identity.recv().unwrap_or_else(|_| Err(ended())) }).await;
                    this.update(cx, |this, cx| this.connected(identity, started.elapsed(), cx)).ok();
                }));
            }
            // a retry asks again
            None => {
                self.first_page = FirstPage::Done;
                self.connect = Some(cx.spawn(async move |this, cx| {
                    let started = Instant::now();
                    let identity = executor.spawn(async move { identify(&client, || client.profiles()) }).await;
                    this.update(cx, |this, cx| this.connected(identity, started.elapsed(), cx)).ok();
                }));
            }
        }
        cx.notify();
    }

    /// The inbox's first page from the early connect: shown right away if nothing was searched
    /// yet and the inbox is what's asked for; else kept for the first search to use.
    fn early_page(&mut self, query: String, page: client::Result<api::SearchResponse>, took: Duration, cx: &mut Context<Self>) {
        // A reconnect refresh (or new HTML policy) supersedes the startup snapshot too.
        if !matches!(&self.first_page, FirstPage::Pending(pending) if *pending == query) {
            return;
        }
        self.first_page = FirstPage::Done;
        // the list may have moved on (a search typed, another profile, fresh profiles)
        if !self.planned(false, cx).is_ok_and(|(server, _)| server == query) {
            if let Ok(page) = page {
                self.first_page = FirstPage::Ready(query, page);
            }
            return;
        }
        // Paging or a settled search must not be replaced by an older first page.
        if self.mailbox.read(cx).remote.as_ref().is_some_and(|r| r.limit != PAGE || (!r.server_query.is_empty() && r.server_query != query)) {
            return;
        }
        trace::note(&format!("durian: start page in {took:.0?}"));
        self.update_remote(cx, |r| {
            r.server_query = query;
            r.limit = PAGE;
        });
        self.generation += 1;
        self.searched(self.generation, PAGE, page, Duration::ZERO, cx);
    }

    /// Profiles from the start thread (the server's, or profiles.pkl): the switcher and folders
    /// follow. Applied again only if they changed (an evaluation after the cache missed).
    fn apply_profiles(&mut self, profiles: Vec<Profile>, took: Duration, cx: &mut Context<Self>) {
        trace::note(&format!("durian: {} profiles in {took:.0?}", profiles.len()));
        let first = !self.profiles_applied;
        self.profiles_applied = true;
        if self.mailbox.read(cx).profiles == profiles {
            return;
        }
        self.mailbox.update(cx, |m, cx| {
            m.set_profiles(profiles, cx);
            // MAIL_FOLDER=<name>: start in that folder (testing against a real mailbox)
            if first
                && let Ok(name) = std::env::var("MAIL_FOLDER")
                && let Some(ix) = m.active_profile().and_then(|p| p.folders.iter().position(|f| f.name.eq_ignore_ascii_case(&name)))
            {
                m.open_folder(ix, cx);
            }
        });
    }

    /// The server query and local refinement for the current list. `settled`: the word being
    /// typed counts as complete.
    fn planned(&self, settled: bool, cx: &Context<Self>) -> Result<(String, Query), String> {
        let mailbox = self.mailbox.read(cx);
        let query = if settled { settle(&mailbox.query) } else { complete_known_tags(&mailbox.query, &mailbox.tags) };
        let translated = translate(&query, &self.me)?;
        let scope = match (mailbox.active_profile(), &mailbox.account_filter) {
            (Some(profile), _) => profiles::scope(profile),
            (None, Some(account)) => Some(account_scope(account)),
            (None, None) => None,
        };
        Ok((compose(scope.as_deref(), mailbox.active_folder().map(|f| f.query.as_ref()), &translated.server), translated.refine))
    }

    fn connected(&mut self, identity: client::Result<Identity>, took: Duration, cx: &mut Context<Self>) {
        self.connect = None;
        let identity = match identity {
            Ok(identity) => identity,
            Err(e) => {
                trace::note(&format!("durian: connect failed after {took:.0?}: {}", kind(&e)));
                let message: SharedString = e.to_string().into();
                self.status = Status::Failed(message.clone());
                self.update_remote(cx, |r| r.loading = Some(Loading::Failed(message)));
                cx.notify();
                return;
            }
        };
        trace::note(&format!("durian {}: {} accounts, {} addresses, {} tags in {took:.0?}", identity.version, identity.accounts.len(), identity.addresses.len(), identity.tags.len()));
        self.status = Status::Connected { version: identity.version };
        self.apply_settings(&identity.settings, cx);
        self.me = identity.addresses;
        // now pictures may be looked up: whose not to is known
        crate::avatars::set_own(self.me.iter().map(|a| a.as_ref()));
        // the early inbox page is already listed: the plan below keeps it unless the query
        // changed meanwhile
        let shown = self.generation != 0;
        let me = self.me.clone();
        self.schedule.update(cx, |s, cx| {
            s.me = me;
            cx.notify();
        });
        self.mailbox.update(cx, |m, cx| {
            m.me = identity.me;
            m.accounts = identity.accounts;
            m.tags = identity.tags;
            m.writes_available = true;
            if let Some(remote) = m.remote.as_mut().filter(|_| !shown) {
                // nothing listed yet: whatever was planned before, the first search goes out now
                remote.server_query.clear();
            }
            cx.notify();
        });
        self.plan(false, Duration::ZERO, cx);
        // Replacing the server keeps the current query and list. That unchanged
        // query still needs a fresh answer; plan() only dispatches changed ones.
        if !self.first_page.pending() && self.search.is_none() && self.mailbox.read(cx).remote.as_ref().is_some_and(|r| r.loading == Some(Loading::Pending)) {
            self.refresh_mail(cx);
        } else {
            self.refresh_counts(true, cx);
        }
        self.ensure_calendar(cx);
        self.start_events(cx);
        self.refresh_compose_settings(cx);
        self.load_drafts(cx);
        self.start_writes(cx);
        cx.notify();
    }

    fn start_events(&mut self, cx: &mut Context<Self>) {
        if self.events.is_some() {
            return;
        }
        match events::Worker::start(self.client.clone()) {
            Ok(worker) => self.events = Some(worker),
            Err(e) => {
                cx.emit(LiveEvent::Problem(e.to_string().into()));
                return;
            }
        }
        let executor = cx.background_executor().clone();
        self.event_task = Some(cx.spawn(async move |this, cx| loop {
            executor.timer(Duration::from_millis(100)).await;
            if this.update(cx, |this, cx| {
                let Some(worker) = &this.events else { return };
                let changes = worker.changes();
                if let Some(e) = changes.failure {
                    trace::note(&format!("durian: event stream retry: {}", kind(&e)));
                }
                if changes.connected {
                    trace::note("durian: event stream connected; refreshing without replay");
                }
                for update in changes.outbox {
                    if let Some(state) = writes::Delivery::from_update(&update) { this.apply_delivery(update.item_id, state, cx); }
                }
                if changes.mail {
                    this.refresh_mail(cx);
                    this.reconcile_outbox(cx);
                }
                if changes.calendar {
                    this.refresh_calendar(cx);
                }
                if this.reconciled_at.elapsed() >= RECONCILE && this.search.is_none() {
                    this.reconcile(cx);
                }
            }).is_err() {
                break;
            }
        }));
    }

    fn invalidate_threads(&mut self, cx: &mut Context<Self>) {
        self.first_page = FirstPage::Done;
        self.thread_epoch += 1;
        self.cache.clear();
        self.fetches.clear();
        self.image_tasks.clear();
        self.update_remote(cx, |r| r.threads.clear());
    }

    /// Ask for the current query, even if its string and row stamps haven't changed.
    /// The old list stays visible until a successful answer replaces it.
    fn refresh_mail(&mut self, cx: &mut Context<Self>) {
        if let Err(message) = self.planned(true, cx) {
            self.reject_query(message, cx);
            return;
        }
        self.mail_refreshing = true;
        self.stamps.clear();
        self.invalidate_threads(cx);
        self.update_remote(cx, |r| r.loading = Some(Loading::Pending));
        self.search(Duration::ZERO, cx);
        self.refresh_counts(true, cx);
    }

    /// Fixed low-rate reconciliation and manual refresh share the same path. It
    /// doesn't change the query, limit or selection; replacements follow thread ids.
    fn reconcile(&mut self, cx: &mut Context<Self>) {
        self.reconciled_at = Instant::now();
        self.reconcile_outbox(cx);
        if self.search.is_none() {
            self.refresh_mail(cx);
        } else {
            self.refresh_counts(true, cx);
        }
        if self.calendar_task.is_none() {
            self.refresh_calendar(cx);
        }
    }

    /// One bounded batch at a time. A changed scope supersedes old replies;
    /// refreshes of the same queries coalesce into one follow-up batch.
    fn refresh_counts(&mut self, force: bool, cx: &mut Context<Self>) {
        if !matches!(self.status, Status::Connected { .. }) {
            return;
        }
        let m = self.mailbox.read(cx);
        let context = (m.profile, m.account_filter.clone(), m.count_queries());
        if self.count_context.as_ref() == Some(&context) {
            if !force { return; }
            if self.count_task.is_some() {
                // A large paced sidebar may take longer than the reconciliation
                // interval. Restarting it for every hint would starve its tail.
                self.count_pending = true;
                return;
            }
        }
        self.count_context = Some(context);
        self.count_generation += 1;
        self.count_pending = true;
        if let Some(cancel) = &self.count_cancel {
            cancel.store(true, Ordering::Relaxed);
        }
        if self.count_task.is_none() {
            self.start_counts(cx);
        }
    }

    fn start_counts(&mut self, cx: &mut Context<Self>) {
        let Some((_, _, queries)) = &self.count_context else { return };
        // The Go store has one SQLite connection. A sidebar fanout before the
        // first page competes with useful mail (especially on a cold database).
        // Keep the newest count context queued; searched() releases it on either
        // success or failure, including an empty inbox. Later refreshes stay live.
        let mailbox = self.mailbox.read(cx);
        if mailbox.threads.is_empty() && mailbox.remote.as_ref().is_some_and(|r| r.loading == Some(Loading::Pending)) {
            return;
        }
        let queries = queries.clone();
        self.count_pending = false;
        let generation = self.count_generation;
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let cancel = Arc::new(AtomicBool::new(false));
        self.count_cancel = Some(cancel.clone());
        self.count_task = Some(cx.spawn(async move |this, cx| {
            let results = executor.spawn(async move { counts(&client, &queries, &cancel) }).await;
            this.update(cx, |this, cx| {
                this.count_task = None;
                if generation == this.count_generation {
                    let failed = results.iter().filter(|(_, r)| r.is_err()).count();
                    trace::note(&format!("durian: {} sidebar counts, {failed} failed", results.len()));
                    this.mailbox.update(cx, |m, cx| {
                        m.update_counts(results.into_iter().map(|(q, r)| (q, r.ok())));
                        cx.notify();
                    });
                }
                if this.count_pending {
                    this.start_counts(cx);
                }
            }).ok();
        }));
    }

    pub(super) fn refresh_calendar(&mut self, cx: &mut Context<Self>) {
        self.calendar_epoch += 1;
        self.calendar_loaded = None;
        self.calendar_pending = None;
        self.calendar_task = None;
        self.details.clear();
        self.detail_tasks.clear();
        self.refs.clear();
        // Old events may remain visible on failure, but no range is known fresh.
        self.schedule.update(cx, |s, cx| {
            s.loaded = Some(Vec::new());
            cx.notify();
        });
        self.ensure_calendar(cx);
    }

    /// Follow the server's settings. Threads loaded before they were known (the open one and
    /// those prefetched after it, while connecting) were shown with the defaults; if those
    /// differ, the cache is dropped and those threads are fetched again.
    fn apply_settings(&mut self, settings: &api::Settings, cx: &mut Context<Self>) {
        let policy = super::html_policy(settings);
        self.mailbox.update(cx, |m, cx| { m.signature_policy = policy; cx.notify(); });
        if policy == self.policy {
            return;
        }
        self.policy = policy;
        let loaded: Vec<SharedString> = self.mailbox.read(cx).threads.iter().filter(|t| !t.partial).map(|t| t.id.clone()).collect();
        self.invalidate_threads(cx);
        for id in loaded {
            self.fetch(id, cx);
        }
        self.selection_changed(cx);
    }

    /// Retry a failed connection, or authoritatively refresh a connected mailbox.
    pub fn retry(&mut self, cx: &mut Context<Self>) {
        match self.status {
            Status::Failed(_) => self.connect(cx),
            Status::Connecting => {}
            Status::Connected { .. } => { self.refresh_compose_settings(cx); self.reconcile(cx); }
        }
    }

    fn query_changed(&mut self, cx: &mut Context<Self>) {
        let now = Instant::now();
        let typing = self.last_change.is_some_and(|at| now - at < DEBOUNCE);
        self.last_change = Some(now);
        self.settle = None;
        self.plan(false, if typing { DEBOUNCE } else { Duration::ZERO }, cx);
        // an unfinished word only narrows locally; once typing pauses it goes out as a word
        let query = self.mailbox.read(cx).query.clone();
        if settle(&query) != query {
            let executor = cx.background_executor().clone();
            self.settle = Some(cx.spawn(async move |this, cx| {
                executor.timer(SETTLE).await;
                this.update(cx, |this, cx| this.plan(true, Duration::ZERO, cx)).ok();
            }));
        }
    }

    /// Split the current query into what the server answers and what's refined here; search
    /// again if the server's part changed. `settled`: the word being typed counts as complete.
    fn plan(&mut self, settled: bool, delay: Duration, cx: &mut Context<Self>) {
        if !matches!(self.status, Status::Connected { .. }) || matches!(self.first_page, FirstPage::Planning) {
            // Wait for identity and startup scope, not the startup search result.
            return;
        }
        let (server, refine) = match self.planned(settled, cx) {
            Ok(plan) => plan,
            Err(message) => return self.reject_query(message, cx),
        };
        let changed = self.mailbox.read(cx).remote.as_ref().is_some_and(|r| r.server_query != server || matches!(r.loading, Some(Loading::Failed(_))));
        self.mailbox.update(cx, |m, cx| {
            if changed && let Some(remote) = m.remote.as_mut() {
                remote.server_query = server;
                remote.limit = PAGE;
                remote.complete = false;
                remote.loading = Some(Loading::Pending);
            }
            m.set_refine(refine, cx);
        });
        if changed {
            self.search(delay, cx);
        }
    }

    /// Never turn a malformed filter into an unconstrained search, including on retry or SSE.
    fn reject_query(&mut self, message: String, cx: &mut Context<Self>) {
        self.first_page = FirstPage::Done;
        self.generation += 1;
        self.search = None;
        if let Some(cancel) = self.search_cancel.take() { cancel.store(true, Ordering::Relaxed); }
        self.mail_refreshing = false;
        self.update_remote(cx, |r| r.loading = Some(Loading::Failed(message.into())));
    }

    /// Ask for the current server query; the answer replaces the list if nothing newer was
    /// asked meanwhile.
    fn search(&mut self, delay: Duration, cx: &mut Context<Self>) {
        let Some(remote) = self.mailbox.read(cx).remote.as_ref() else { return };
        let (query, limit) = (remote.server_query.clone(), remote.limit);
        if let Some(cancel) = self.search_cancel.take() { cancel.store(true, Ordering::Relaxed); }
        if limit == PAGE && matches!(&self.first_page, FirstPage::Pending(pending) if *pending == query) {
            // Returning to the startup query also supersedes another search.
            self.generation += 1;
            self.search = None;
            return;
        }
        // the page fetched while connecting answers the first search, if that's still the inbox
        if matches!(self.first_page, FirstPage::Ready(..))
            && let FirstPage::Ready(cached, page) = std::mem::replace(&mut self.first_page, FirstPage::Done)
            && cached == query && limit == PAGE
        {
            self.generation += 1;
            self.search = None;
            return self.searched(self.generation, limit, Ok(page), Duration::ZERO, cx);
        }
        // Reuse only actual content, not summary stand-ins. A freshness refresh clears
        // stamps, so same-date/same-tag edits still fetch new previews.
        let known: HashMap<SharedString, Stamp> = self.mailbox.read(cx).threads.iter()
            .filter(|t| !t.latest().id.is_empty())
            .filter_map(|t| self.stamps.get(&t.id).map(|stamp| (t.id.clone(), stamp.clone())))
            .collect();
        self.generation += 1;
        let generation = self.generation;
        let client = self.client.clone();
        let cancel = Arc::new(AtomicBool::new(false));
        self.search_cancel = Some(cancel.clone());
        let executor = cx.background_executor().clone();
        self.search = Some(cx.spawn(async move |this, cx| {
            if !delay.is_zero() {
                executor.timer(delay).await;
            }
            let started = Instant::now();
            let result = executor.spawn(async move { search_page(&client, &query, limit, &known, &cancel) }).await;
            this.update(cx, |this, cx| this.searched(generation, limit, result, started.elapsed(), cx)).ok();
        }));
    }

    fn searched(&mut self, generation: u64, limit: usize, result: client::Result<api::SearchResponse>, took: Duration, cx: &mut Context<Self>) {
        if generation != self.generation {
            trace::note(&format!("durian: dropped search #{generation} (#{} is current)", self.generation));
            return;
        }
        self.search = None;
        if let Some(cancel) = self.search_cancel.take() { cancel.store(true, Ordering::Relaxed); }
        let response = match result {
            Ok(response) => response,
            Err(e) => {
                trace::note(&format!("durian: search #{generation} failed after {took:.0?}: {}", kind(&e)));
                // what's listed stays; the list says the refresh failed
                let message: SharedString = e.to_string().into();
                self.update_remote(cx, |r| r.loading = Some(Loading::Failed(message)));
                if self.count_pending && self.count_task.is_none() { self.start_counts(cx); }
                return;
            }
        };
        let now = Local::now().timestamp();
        let stamps: HashMap<SharedString, Stamp> = response.results.iter().map(|r| (r.thread_id.clone().into(), Stamp::new(r.timestamp, &r.tags))).collect();
        // a full thread we have beats the content that came along: search content is a
        // preview (durian sends no HTML with it), good for the list, opened via /threads
        let previous: HashMap<_, _> = self.mailbox.read(cx).threads.iter().map(|t| (t.id.as_ref(), t)).collect();
        let threads: Vec<_> = response
            .results
            .iter()
            .map(|r| {
                self.cache.get(&r.thread_id, &stamps[r.thread_id.as_str()]).unwrap_or_else(|| {
                    let preview = response.threads.get(&r.thread_id).and_then(|content| super::thread(content, now, self.policy));
                    preview.map(|t| Thread { partial: true, ..t }).or_else(|| {
                        (self.stamps.get(r.thread_id.as_str()) == stamps.get(r.thread_id.as_str()))
                            .then(|| previous.get(r.thread_id.as_str()).map(|t| (*t).clone())).flatten()
                    }).unwrap_or_else(|| super::summary(r, now))
                })
            })
            .collect();
        let full = threads.iter().filter(|t| !t.partial).count();
        trace::note(&format!("durian: search #{generation}: {} rows ({full} full, {} cached) in {took:.0?}", threads.len(), self.cache.len()));
        self.stamps = stamps;
        self.mail_refreshing = false;
        let complete = threads.len() < limit;
        self.mailbox.update(cx, |m, cx| {
            if let Some(remote) = m.remote.as_mut() {
                remote.loading = None;
                remote.complete = complete;
            }
            m.replace_threads(threads, cx);
        });
        self.selection_changed(cx);
        if self.count_pending && self.count_task.is_none() { self.start_counts(cx); }
    }

    /// The next page, unless everything is listed or a search is on its way.
    pub fn load_more(&mut self, cx: &mut Context<Self>) {
        let ready = matches!(self.status, Status::Connected { .. });
        let Some(remote) = self.mailbox.read(cx).remote.as_ref() else { return };
        if !ready || remote.complete || remote.loading.is_some() {
            return;
        }
        self.update_remote(cx, |r| {
            r.limit += PAGE;
            r.loading = Some(Loading::Pending);
        });
        self.search(Duration::ZERO, cx);
    }

    /// The open thread loads (if it's only a row), the next few are prefetched; near the end
    /// of the list, the next page.
    fn selection_changed(&mut self, cx: &mut Context<Self>) {
        if self.mail_refreshing {
            return; // Don't start new fetches from the pre-refresh snapshot.
        }
        let mailbox = self.mailbox.read(cx);
        let visible: Vec<usize> = mailbox.visible().collect();
        let Some(position) = mailbox.selected.and_then(|s| visible.iter().position(|&v| v == s)) else { return };
        let wanted: Vec<SharedString> = visible[position..]
            .iter()
            .take(1 + PREFETCH)
            .map(|&ix| &mailbox.threads[ix])
            .filter(|t| t.partial && mailbox.thread_loading(&t.id).is_none())
            .map(|t| t.id.clone())
            .collect();
        let selected_id = mailbox.threads[visible[position]].id.clone();
        let near_end = visible.len() - position <= PAGE_AHEAD;
        for id in wanted {
            // the open thread always goes; prefetch only into free slots
            if id == selected_id || self.fetches.len() < IN_FLIGHT {
                self.fetch(id, cx);
            }
        }
        if near_end {
            self.load_more(cx);
        }
    }

    fn fetch(&mut self, id: SharedString, cx: &mut Context<Self>) {
        if self.fetches.contains_key(&id) {
            return;
        }
        self.update_remote(cx, |r| {
            r.threads.insert(id.clone(), Loading::Pending);
        });
        let stamp = self.stamps.get(&id).cloned();
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let key = id.clone();
        let epoch = self.thread_epoch;
        let task = cx.spawn(async move |this, cx| {
            let started = Instant::now();
            let result = executor
                .spawn({
                    let key = key.clone();
                    async move { client.thread(&key) }
                })
                .await;
            this.update(cx, |this, cx| {
                if epoch == this.thread_epoch {
                    this.fetched(key, stamp, result, started.elapsed(), cx);
                }
            }).ok();
        });
        self.fetches.insert(id, task);
    }

    fn fetched(&mut self, id: SharedString, stamp: Option<Stamp>, result: client::Result<api::ThreadContent>, took: Duration, cx: &mut Context<Self>) {
        self.fetches.remove(&id);
        let thread = result.and_then(|content| {
            let thread = super::thread(&content, Local::now().timestamp(), self.policy).ok_or_else(|| client::Error::Decode("a thread without messages".into()))?;
            Ok((thread, content))
        });
        match thread {
            Ok((thread, content)) => {
                trace::note(&format!("durian: thread {id} in {took:.0?}"));
                // shown now; messages embedding images get their HTML once those are in
                let waiting = content.messages.iter().any(|m| !super::embedded_parts(m).is_empty());
                if !waiting && let Some(stamp) = stamp.clone() {
                    self.cache.insert(stamp, thread.clone());
                }
                self.mailbox.update(cx, |m, cx| m.fill_thread(thread, cx));
                if waiting {
                    self.fetch_images(id.clone(), content, stamp, cx);
                }
            }
            Err(e) => {
                trace::note(&format!("durian: thread {id} failed after {took:.0?}: {}", kind(&e)));
                let message: SharedString = e.to_string().into();
                self.update_remote(cx, |r| {
                    r.threads.insert(id, Loading::Failed(message));
                });
            }
        }
        // a slot is free: keep prefetching
        self.selection_changed(cx);
    }
}

impl Live {
    /// Load the events around the shown period, unless they're listed or on their way.
    fn ensure_calendar(&mut self, cx: &mut Context<Self>) {
        if !matches!(self.status, Status::Connected { .. }) {
            return;
        }
        let days = self.schedule.read(cx).days();
        let (Some(&first), Some(&last)) = (days.first(), days.last()) else { return };
        let covers = |window: Option<(NaiveDate, NaiveDate)>| window.is_some_and(|(from, to)| from <= first && last < to);
        if covers(self.calendar_loaded) || covers(self.calendar_pending) {
            return;
        }
        let mut accounts: Vec<SharedString> = self.mailbox.read(cx).accounts.iter().map(|a| a.id.clone()).collect();
        // Local-only calendars belong to no mail account (also works with zero accounts).
        if !accounts.iter().any(|a| a == "local") {
            accounts.push("local".into());
        }
        let (from, to) = (first - chrono::Duration::days(CALENDAR_BEHIND), last + chrono::Duration::days(1 + CALENDAR_AHEAD));
        self.calendar_pending = Some((from, to));
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let epoch = self.calendar_epoch;
        self.calendar_task = Some(cx.spawn(async move |this, cx| {
            let started = Instant::now();
            let result = executor.spawn(async move { calendars(&client, &accounts, from, to) }).await;
            this.update(cx, |this, cx| {
                if epoch == this.calendar_epoch {
                    this.calendar_loaded(from, to, result, started.elapsed(), cx);
                }
            }).ok();
        }));
    }

    fn calendar_loaded(&mut self, from: NaiveDate, to: NaiveDate, result: client::Result<Vec<AccountCalendars>>, took: Duration, cx: &mut Context<Self>) {
        self.calendar_pending = None;
        self.calendar_task = None;
        let accounts = match result {
            Ok(accounts) => accounts,
            Err(e) => {
                trace::note(&format!("durian: calendar {from}..{to} failed after {took:.0?}: {}", kind(&e)));
                cx.emit(LiveEvent::Problem(format!("Calendar didn't load: {e}").into()));
                return;
            }
        };
        let (mut calendars, mut events) = (Vec::new(), Vec::new());
        for (account, list, found) in accounts {
            for c in list {
                let color = c.color.as_deref().and_then(ui::theme::parse_css_color).unwrap_or_else(|| hashed_color(&c.name));
                calendars.push(CalendarInfo { id: calendar_id(&account, &c.name), name: c.name.into(), account: account.clone(), color });
            }
            for e in &found {
                let Some((mut event, at)) = calendar_event(&account, e) else { continue };
                // guests already known survive the reload
                if let Some((description, attendees, rsvp, is_organizer)) = self.details.get(&at) {
                    (event.description, event.attendees, event.rsvp, event.is_organizer) = (description.clone(), attendees.clone(), *rsvp, *is_organizer);
                }
                self.refs.insert(event.id.clone(), at);
                events.push(event);
            }
        }
        trace::note(&format!("durian: calendar {from}..{to}: {} calendars, {} events in {took:.0?}", calendars.len(), events.len()));
        self.schedule.update(cx, |s, cx| s.load_window(calendars, from, to, events, cx));
        let listed: HashSet<SharedString> = self.schedule.read(cx).events.iter().map(|e| e.id.clone()).collect();
        self.refs.retain(|id, _| listed.contains(id));
        self.calendar_loaded = Some((from, to));
        self.event_selected(cx);
    }

    /// The selected event's guests and organizer, once per series.
    fn event_selected(&mut self, cx: &mut Context<Self>) {
        let Some(at) = self.schedule.read(cx).selected.as_ref().and_then(|id| self.refs.get(id)).cloned() else { return };
        if self.details.contains_key(&at) || self.detail_tasks.contains_key(&at) {
            return;
        }
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let key = at.clone();
        let epoch = self.calendar_epoch;
        let task = cx.spawn(async move |this, cx| {
            let request = at.clone();
            let result = executor.spawn(async move { client.event(&request.account, &request.calendar, &request.uid) }).await;
            this.update(cx, |this, cx| {
                if epoch == this.calendar_epoch {
                    this.detail_loaded(at, result, cx);
                }
            }).ok();
        });
        self.detail_tasks.insert(key, task);
    }

    fn detail_loaded(&mut self, at: EventRef, result: client::Result<api::CalendarEvent>, cx: &mut Context<Self>) {
        self.detail_tasks.remove(&at);
        let detail = match result {
            Ok(detail) => detail,
            Err(e) => {
                // the summary stays: it's what the event looked like before
                trace::note(&format!("durian: event detail failed: {}", kind(&e)));
                return;
            }
        };
        let ids: Vec<SharedString> = self.refs.iter().filter(|(_, r)| **r == at).map(|(id, _)| id.clone()).collect();
        let fallback = ids.first().and_then(|id| self.schedule.read(cx).event(id)).map_or(Rsvp::Accepted, |e| e.rsvp);
        let (description, attendees, rsvp, is_organizer) = event_detail(&detail, &self.me, fallback);
        trace::note(&format!("durian: event detail: {} guests, {} occurrences", attendees.len(), ids.len()));
        self.details.insert(at, (description.clone(), attendees.clone(), rsvp, is_organizer));
        self.schedule.update(cx, |s, cx| s.fill_detail(&ids, description, attendees, rsvp, is_organizer, cx));
    }
}

impl Live {
    /// Bring an attachment into the session folder, remember the copy, then use it. A second
    /// request while it's on its way is ignored.
    pub fn fetch_attachment(&mut self, attachment: Attachment, use_: Use, cx: &mut Context<Self>) {
        let Some(part) = attachment.remote.clone() else { return };
        let key = (part.message.clone(), part.part);
        if self.attachment_tasks.contains_key(&key) {
            return;
        }
        // one folder per part: two attachments may share a name
        let folder: String = part.message.chars().map(|c| if c.is_ascii_alphanumeric() { c } else { '_' }).collect();
        let path = crate::session_dir(&format!("attachments/{folder}-{}", part.part)).join(safe_name(&attachment.name));
        let client = self.client.clone();
        let executor = cx.background_executor().clone();
        let name = attachment.name.clone();
        let task = cx.spawn(async move |this, cx| {
            let started = Instant::now();
            let request = part.clone();
            let target = path.clone();
            let result = executor
                .spawn(async move {
                    let bytes = client.attachment(&request.message, request.part).map_err(|e| e.to_string())?;
                    std::fs::write(&target, &bytes).map_err(|e| e.to_string())?;
                    Ok::<usize, String>(bytes.len())
                })
                .await;
            this.update(cx, |this, cx| {
                this.attachment_tasks.remove(&(part.message.clone(), part.part));
                match result {
                    Ok(bytes) => {
                        trace::note(&format!("durian: attachment {} part {} ({bytes} bytes) in {:.0?}", part.message, part.part, started.elapsed()));
                        this.mailbox.update(cx, |m, cx| m.set_attachment_path(&part, path.clone(), cx));
                        match use_ {
                            Use::QuickLook => attachments::quick_look_path(&path),
                            Use::Download => match attachments::copy_to_downloads(&path, &name) {
                                Some(saved) => cx.emit(LiveEvent::Downloaded(saved)),
                                None => cx.emit(LiveEvent::Problem("Couldn't save to Downloads".into())),
                            },
                        }
                    }
                    Err(e) => cx.emit(LiveEvent::Problem(format!("Attachment didn't load: {e}").into())),
                }
            })
            .ok();
        });
        self.attachment_tasks.insert(key, task);
    }
}

/// One account's calendars and their events in the window.
type AccountCalendars = (SharedString, Vec<api::Calendar>, Vec<api::CalendarEvent>);

/// The first page arrives in one request. Later pages fetch the lightweight row prefix,
/// then only missing/changed content, at most PAGE conversations per HTTP response.
/// Never raise the response byte cap to accommodate a growing prefix of message bodies.
fn search_page(client: &Client, query: &str, limit: usize, known: &HashMap<SharedString, Stamp>, cancel: &AtomicBool) -> client::Result<api::SearchResponse> {
    if limit <= PAGE {
        return client.search_cancellable(query, limit, limit, cancel);
    }
    let mut page = client.search_cancellable(query, limit, 0, cancel)?;
    let missing: Vec<_> = page.results.iter()
        .filter(|row| known.get(row.thread_id.as_str()) != Some(&Stamp::new(row.timestamp, &row.tags)))
        .map(|row| row.thread_id.as_str()).collect();
    let mut remaining = missing.as_slice();
    while !remaining.is_empty() {
        let mut query = String::new();
        let mut count = 0;
        for id in remaining.iter().take(PAGE) {
            let term = format!("thread:{}", super::translate::quoted(id));
            let separator = if query.is_empty() { "" } else { " OR " };
            // The existing Go search contract caps decoded queries at 1024 bytes.
            if query.len() + separator.len() + term.len() > 1024 { break }
            query.push_str(separator);
            query.push_str(&term);
            count += 1;
        }
        if count == 0 { return Err(client::Error::Decode("thread id exceeds search query limit".into())) }
        let (batch, rest) = remaining.split_at(count);
        let mut response = client.search_cancellable(&query, batch.len(), batch.len(), cancel)?;
        for id in batch {
            let content = response.threads.remove(*id).ok_or_else(|| client::Error::Decode("search preview missing; refresh to retry".into()))?;
            page.threads.insert((*id).to_string(), content);
        }
        remaining = rest;
    }
    Ok(page)
}

/// A failure affects only its query; callers retain that query's last known count.
/// At most four GETs are active; the client paces admissions with foreground priority.
/// Cancellation also stops workers still waiting for a rate slot or server cooldown.
fn counts(client: &Client, queries: &[String], cancel: &AtomicBool) -> Vec<(String, client::Result<u64>)> {
    queries.chunks(COUNT_IN_FLIGHT).take_while(|_| !cancel.load(Ordering::Relaxed)).flat_map(|batch| {
        std::thread::scope(|s| {
            let handles: Vec<_> = batch.iter().map(|q| s.spawn(move || {
                (q.clone(), client.count_cancellable(q, cancel))
            })).collect();
            handles.into_iter().map(|h| h.join().expect("count worker panicked")).collect::<Vec<_>>()
        })
    }).collect()
}

/// Every account's calendars and events in [from, to). An account without calendars (or one
/// whose calendar isn't synced yet: 404 / 409) is skipped, any other failure fails the load.
fn calendars(client: &Client, accounts: &[SharedString], from: NaiveDate, to: NaiveDate) -> client::Result<Vec<AccountCalendars>> {
    // Date-only bounds mean UTC to Go. Each local midnight needs its own offset:
    // a window crossing DST is not a fixed multiple of 24 elapsed hours.
    let boundary = |day: NaiveDate| {
        let midnight = day.and_time(chrono::NaiveTime::MIN);
        // Some zones skip midnight, or an entire civil day. Use the first actual
        // instant at/after that boundary; repeated midnights use the earlier one.
        (0..=172800).find_map(|seconds| Local.from_local_datetime(&(midnight + chrono::Duration::seconds(seconds))).earliest())
            .map(|time| time.to_rfc3339()).ok_or_else(|| client::Error::Decode("Could not resolve the calendar's local-day boundary.".into()))
    };
    let (from, to) = (boundary(from)?, boundary(to)?);
    let one = |account: &SharedString| -> client::Result<Option<AccountCalendars>> {
        let list = match client.calendars(account) {
            Ok(list) => list,
            Err(client::Error::NotFound(_) | client::Error::Server { status: 409, .. }) => return Ok(None),
            Err(e) => return Err(e),
        };
        Ok(Some((account.clone(), list, client.events(account, &from, &to)?)))
    };
    // accounts at once: each request is mostly the server's fixed cost
    let results: Vec<_> = std::thread::scope(|s| {
        let handles: Vec<_> = accounts.iter().map(|a| s.spawn(move || one(a))).collect();
        handles.into_iter().map(joined).collect()
    });
    results.into_iter().filter_map(Result::transpose).collect()
}

impl Live {
    /// The images a thread's HTML embeds (`cid:`), all at once, then each message's HTML with
    /// them inlined – the renderer has no network. Best effort: one that fails stays a broken
    /// image. The finished thread goes into the cache.
    fn fetch_images(&mut self, thread_id: SharedString, content: api::ThreadContent, stamp: Option<Stamp>, cx: &mut Context<Self>) {
        let client = self.client.clone();
        let policy = self.policy;
        let executor = cx.background_executor().clone();
        let key = thread_id.clone();
        let epoch = self.thread_epoch;
        let task = cx.spawn(async move |this, cx| {
            let started = Instant::now();
            let htmls: Vec<(String, Option<String>, usize)> = executor
                .spawn(async move {
                    std::thread::scope(|s| {
                        let messages: Vec<_> = content
                            .messages
                            .iter()
                            .filter(|m| !super::embedded_parts(m).is_empty())
                            .map(|m| {
                                let parts: Vec<_> = super::embedded_parts(m)
                                    .into_iter()
                                    .map(|part| (part, s.spawn(|| client.attachment(&m.id, part.part_id))))
                                    .collect();
                                (m, parts)
                            })
                            .collect();
                        messages
                            .into_iter()
                            .map(|(m, parts)| {
                                let images: Vec<_> = parts.into_iter().filter_map(|(part, handle)| Some((part, joined(handle).ok()?))).collect();
                                (m.id.clone(), super::prepared_html(m, &images, policy), images.len())
                            })
                            .collect()
                    })
                })
                .await;
            this.update(cx, |this, cx| {
                if epoch != this.thread_epoch {
                    return; // Late HTML must never patch or cache a refreshed thread.
                }
                let images: usize = htmls.iter().map(|(_, _, n)| n).sum();
                trace::note(&format!("durian: {images} embedded images for {thread_id} in {:.0?}", started.elapsed()));
                this.mailbox.update(cx, |m, cx| {
                    for (message, html, _) in htmls {
                        if let Some(html) = html {
                            m.set_html(&thread_id, &message, html.into(), cx);
                        }
                    }
                });
                // only the full thread (a search may have put its list row back meanwhile)
                let thread = this.mailbox.read(cx).threads.iter().find(|t| t.id == thread_id && !t.partial).cloned();
                if let (Some(stamp), Some(thread)) = (stamp, thread) {
                    this.cache.insert(stamp, thread);
                }
                this.image_tasks.remove(&thread_id);
            })
            .ok();
        });
        self.image_tasks.insert(key, task);
    }
}

/// The error's kind for logs (its message may quote the server, which may quote the query).
fn kind(e: &client::Error) -> String {
    match e {
        client::Error::Unreachable(_) => "unreachable".into(),
        client::Error::Unauthorized => "unauthorized".into(),
        client::Error::NotFound(path) => format!("not found ({path})"),
        client::Error::Server { status, .. } => format!("server error {status}"),
        client::Error::Decode(_) => "unexpected answer".into(),
    }
}

/// The server query for a list: the profile's scope, then the open folder's query with the
/// translated search inside it. Without folders (a server without profiles) an empty search
/// is the inbox and a search covers all mail.
pub fn compose(scope: Option<&str>, folder: Option<&str>, search: &str) -> String {
    let base = match folder {
        Some(folder) => combine(folder, search),
        None if search.trim().is_empty() => INBOX.to_string(),
        None => search.to_string(),
    };
    match scope {
        Some(scope) => combine(scope, &base),
        None => base,
    }
}

/// The query with its last word counted as typed out, once typing pauses – for the server,
/// which can't match a word by its beginning. Every kind of clause: a `tag:` or `from:` left
/// unfinished would otherwise only ever narrow what's already listed, never search.
pub fn settle(query: &Query) -> Query {
    let mut query = query.clone();
    if let Some(last) = query.clauses.last_mut() {
        last.prefix = false;
    }
    query
}

/// A `tag:` naming a tag there is is complete as typed: it searches right away instead of
/// after the pause.
pub fn complete_known_tags(query: &Query, tags: &[Tag]) -> Query {
    let mut query = query.clone();
    if let Some(last) = query.clauses.last_mut().filter(|c| c.prefix)
        && let Term::Tag(name) = &last.term
        && (tags.iter().any(|t| t.name.eq_ignore_ascii_case(name)) || super::SYSTEM_TAGS.iter().any(|t| t.eq_ignore_ascii_case(name)))
    {
        last.prefix = false;
    }
    query
}

/// Who the user is on this server.
struct Identity {
    version: SharedString,
    settings: api::Settings,
    me: Contact,
    /// every address you send from, for `from:me` / `to:me`
    addresses: Vec<SharedString>,
    accounts: Vec<Account>,
    tags: Vec<Tag>,
}

/// Sent mail is the only place durian tells who you are (there's no `/accounts` yet).
const SENT_SAMPLE: usize = 50;

/// A request run on its own thread (a panic there is an error here, not a crash).
fn joined<T>(handle: std::thread::ScopedJoinHandle<'_, client::Result<T>>) -> client::Result<T> {
    handle.join().unwrap_or_else(|_| Err(client::Error::Decode("request thread panicked".into())))
}

/// Who you are on this server: version, tags, and – from sent mail – accounts and addresses.
/// After the version check the requests run at once (each server query costs ~100–400 ms).
fn identify(client: &Client, profiles: impl FnOnce() -> client::Result<api::ProfilesResponse>) -> client::Result<Identity> {
    // version first: it answers "reachable, and the token works?" cheaply
    let version = client.version()?.version.into();
    let (tags, who, settings) = std::thread::scope(|s| {
        let tags = s.spawn(|| client.tags());
        let settings = s.spawn(|| client.settings());
        // the configured accounts, where the server serves them; else guessed from sent mail
        let who = match profiles() {
            Ok(response) => Ok(configured_identities(&response)),
            Err(client::Error::NotFound(_)) => client.search("tag:sent", SENT_SAMPLE, SENT_SAMPLE).map(|sent| identities(sent.threads.values())),
            Err(e) => Err(e),
        };
        (joined(tags), who, joined(settings))
    });
    // settings are optional: a server without them (or failing them) gets the defaults
    let settings = settings.unwrap_or_else(|e| {
        if !matches!(e, client::Error::NotFound(_)) {
            trace::note(&format!("durian: settings failed: {}", kind(&e)));
        }
        api::Settings::default()
    });
    let tags = tags?.into_iter().filter(|t| !super::SYSTEM_TAGS.contains(&t.as_str())).map(|t| Tag { color: hashed_color(&t), name: t.into() }).collect();
    let (me, addresses, accounts) = who?;
    Ok(Identity { version, settings, me, addresses, accounts, tags })
}

/// You and your accounts as configured (`GET /profiles`): the default account is you; every
/// account's address is yours. Account ids are durian's (the lowercased name).
fn configured_identities(response: &api::ProfilesResponse) -> (Contact, Vec<SharedString>, Vec<Account>) {
    let accounts: Vec<Account> = response
        .accounts
        .iter()
        .map(|a| Account {
            id: a.name.to_lowercase().into(),
            name: a.name.clone().into(),
            email: a.email.clone().into(),
            aliases: Vec::new(),
            color: hashed_color(&a.name.to_lowercase()),
        })
        .collect();
    let addresses: Vec<SharedString> = response.accounts.iter().filter(|a| !a.email.is_empty()).map(|a| a.email.clone().into()).collect();
    let me = response.accounts.iter().find(|a| a.default).or(response.accounts.first()).map_or_else(
        || Contact { name: "Me".into(), email: SharedString::default(), avatar: None },
        |a| Contact { name: if a.display_name.is_empty() { a.name.clone() } else { a.display_name.clone() }.into(), email: a.email.clone().into(), avatar: None },
    );
    (me, addresses, accounts)
}

/// From your sent mail: you (the address you send from most), all your addresses, and per
/// account the address it sends from most (the others are its aliases).
fn identities<'a>(sent: impl Iterator<Item = &'a api::ThreadContent>) -> (Contact, Vec<SharedString>, Vec<Account>) {
    // (account, address) → (contact, messages); first-seen order keeps the result stable
    let mut seen: Vec<(String, Contact, usize)> = Vec::new();
    for m in sent.flat_map(|t| &t.messages).filter(|m| m.tags.iter().any(|t| t == "sent")) {
        let contact = parse_address(&m.from);
        let account = m.account.clone().unwrap_or_default();
        match seen.iter_mut().find(|(a, c, _)| *a == account && c.email.eq_ignore_ascii_case(&contact.email)) {
            Some(entry) => entry.2 += 1,
            None => seen.push((account, contact, 1)),
        }
    }
    let me = seen.iter().max_by_key(|(_, _, n)| *n).map(|(_, c, _)| c.clone()).unwrap_or_else(|| Contact { name: "Me".into(), email: SharedString::default(), avatar: None });
    let mut addresses: Vec<SharedString> = Vec::new();
    for (_, c, _) in &seen {
        if !addresses.iter().any(|a| a.eq_ignore_ascii_case(&c.email)) {
            addresses.push(c.email.clone());
        }
    }
    let mut accounts: Vec<Account> = Vec::new();
    for (name, ..) in seen.iter().filter(|(a, ..)| !a.is_empty()) {
        if accounts.iter().any(|a| a.id == name.as_str()) {
            continue;
        }
        let mut own: Vec<&(String, Contact, usize)> = seen.iter().filter(|(a, ..)| a == name).collect();
        own.sort_by_key(|(_, _, n)| std::cmp::Reverse(*n));
        accounts.push(Account {
            id: name.clone().into(),
            name: name.clone().into(),
            email: own[0].1.email.clone(),
            aliases: own[1..].iter().map(|(_, c, _)| c.email.clone()).collect(),
            color: hashed_color(name),
        });
    }
    (me, addresses, accounts)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lists_are_folder_and_search_within_the_profile() {
        assert_eq!(compose(None, None, ""), INBOX);
        assert_eq!(compose(None, None, "from:mara"), "from:mara", "no folders: a search covers all mail");
        assert_eq!(compose(Some("path:work/**"), Some("tag:inbox AND NOT tag:sent"), ""), "(path:work/**) (tag:inbox AND NOT tag:sent)");
        assert_eq!(compose(Some("path:work/**"), Some("tag:sent"), "from:mara"), "(path:work/**) ((tag:sent) (from:mara))");
    }

    #[test]
    fn the_last_clause_settles_whatever_it_is() {
        // a field typed without a trailing space must reach the server too, not only narrow
        // what's listed
        for q in ["invoi", "tag:newsletter", "from:mara", "to:jo", "tag:unread"] {
            let settled = settle(&Query::parse(q));
            assert!(!settled.clauses[0].prefix, "{q}");
            assert!(!translate(&settled, &[]).unwrap().server.is_empty(), "{q} goes to the server");
        }
        assert_eq!(settle(&Query::parse("invoice ")), Query::parse("invoice "));
    }

    #[test]
    fn a_known_tag_searches_without_waiting() {
        let tags = [Tag { name: "Newsletter".into(), color: gpui::black() }];
        let q = complete_known_tags(&Query::parse("tag:newsletter"), &tags);
        assert!(!q.clauses[0].prefix);
        assert_eq!(translate(&q, &[]).unwrap().server, "tag:newsletter");
        assert!(!complete_known_tags(&Query::parse("tag:unread"), &[]).clauses[0].prefix);
        // still being typed: narrows locally until the pause
        assert!(complete_known_tags(&Query::parse("tag:news"), &tags).clauses[0].prefix);
    }

    #[test]
    fn identities_come_from_the_configuration_when_served() {
        let json = r#"{"ok":true,"profiles":[],"accounts":[
            {"name":"Home","email":"ada@home.org"},
            {"name":"Work","display_name":"Ada Lovelace","email":"ada@work.dev","default":true},
            {"name":"Shared"}]}"#;
        let response: api::ProfilesResponse = serde_json::from_str(json).unwrap();
        let (me, addresses, accounts) = configured_identities(&response);
        assert_eq!((me.name.as_ref(), me.email.as_ref()), ("Ada Lovelace", "ada@work.dev"), "the default account is you");
        assert_eq!(addresses, ["ada@home.org", "ada@work.dev"], "accounts without an address add none");
        assert_eq!(accounts.iter().map(|a| a.id.as_ref()).collect::<Vec<_>>(), ["home", "work", "shared"], "durian's account ids");
    }

    #[test]
    fn identities_come_from_sent_mail() {
        let json = r#"{"a":{"thread_id":"a","messages":[
            {"id":"1","from":"Ada <ada@work.dev>","account":"work","tags":["sent"]},
            {"id":"2","from":"Ada <ada@work.dev>","account":"work","tags":["sent"]},
            {"id":"3","from":"Ada <hello@work.dev>","account":"work","tags":["sent"]},
            {"id":"4","from":"ada@home.org","account":"home","tags":["sent"]},
            {"id":"5","from":"Bob <bob@x>","account":"work","tags":["inbox"]}]}}"#;
        let threads: HashMap<String, api::ThreadContent> = serde_json::from_str(json).unwrap();
        let (me, addresses, accounts) = identities(threads.values());
        assert_eq!(me.email.as_ref(), "ada@work.dev");
        assert_eq!(addresses, ["ada@work.dev", "hello@work.dev", "ada@home.org"]);
        let work = &accounts[0];
        assert_eq!((work.id.as_ref(), work.email.as_ref()), ("work", "ada@work.dev"));
        assert_eq!(work.aliases, ["hello@work.dev"]);
        assert_eq!(accounts[1].email.as_ref(), "ada@home.org");
    }

    #[test]
    fn count_transport_is_bounded_preserves_errors_and_stops_superseded_batches() {
        use std::io::{BufRead, BufReader, Write};
        use std::net::TcpListener;
        use std::sync::{Barrier, atomic::AtomicUsize};

        for supersede in [false, true] {
            let queries: Vec<String> = ["tag:failed", "tag:unknown", "path:home/ tag:inbox", "tag:inbox", "tag:zero", "path:work/ tag:inbox", "tag:waiting", "tag:limited"].map(String::from).to_vec();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let address = format!("http://{}", listener.local_addr().unwrap());
            let cancel = Arc::new(AtomicBool::new(false));
            let stop = cancel.clone();
            let expected = if supersede { 4 } else { queries.len() };
            let server = std::thread::spawn(move || {
                let active = AtomicUsize::new(0);
                let peak = AtomicUsize::new(0);
                let limited = AtomicUsize::new(0);
                let barrier = Barrier::new(4);
                std::thread::scope(|s| {
                    for _ in 0..(expected + usize::from(!supersede)) {
                        let deadline = Instant::now() + Duration::from_secs(5);
                        let mut stream = loop {
                            match listener.accept() {
                                Ok((stream, _)) => break stream,
                                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock && Instant::now() < deadline => std::thread::sleep(Duration::from_millis(5)),
                                Err(e) => panic!("count accept: {e}"),
                            }
                        };
                        let (active, peak, barrier, stop, limited) = (&active, &peak, &barrier, &stop, &limited);
                        s.spawn(move || {
                            stream.set_read_timeout(Some(Duration::from_secs(3))).unwrap();
                            let mut reader = BufReader::new(stream.try_clone().unwrap());
                            let mut request = String::new();
                            loop {
                                let mut line = String::new();
                                assert_ne!(reader.read_line(&mut line).unwrap(), 0);
                                if line == "\r\n" { break }
                                request.push_str(&line);
                            }
                            assert!(request.starts_with("GET /api/v1/search/count?query="));
                            assert!(request.to_lowercase().contains("authorization: bearer count-test"));
                            let target = request.lines().next().unwrap().split_whitespace().nth(1).unwrap();
                            // Inspect the actual encoded transport query, independently
                            // of the model's query builders.
                            let (status, count) = if target.contains("tag%3Afailed") { (500, 0) }
                                else if target.contains("tag%3Aunknown") { (401, 0) }
                                else if target.contains("path%3Ahome") { (200, 7) }
                                else if target.contains("path%3Awork") { (200, 501) }
                                else if target.contains("tag%3Azero") { (200, 0) }
                                else if target.contains("tag%3Awaiting") { (200, 19) }
                                else if target.contains("tag%3Alimited") {
                                    if limited.fetch_add(1, Ordering::SeqCst) == 0 { (429, 0) } else { (200, 43) }
                                }
                                else { (200, 1026) };
                            let n = active.fetch_add(1, Ordering::SeqCst) + 1;
                            peak.fetch_max(n, Ordering::SeqCst);
                            if supersede {
                                barrier.wait(); // All four are already active before cancellation.
                                stop.store(true, Ordering::Relaxed);
                            }
                            std::thread::sleep(Duration::from_millis(40));
                            let body = format!(r#"{{"ok":true,"count":{count}}}"#);
                            write!(stream, "HTTP/1.1 {status} X\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
                            active.fetch_sub(1, Ordering::SeqCst);
                        });
                    }
                });
                assert!(matches!(listener.accept(), Err(e) if e.kind() == std::io::ErrorKind::WouldBlock), "a superseded batch must not issue its remaining queries");
                peak.load(Ordering::SeqCst)
            });
            let client = Client::new(&address, Some("count-test".into()));
            let result = counts(&client, &queries, &cancel);
            assert_eq!(result.len(), expected);
            let peak = server.join().unwrap();
            assert!((1..=4).contains(&peak), "count concurrency was {peak}, expected bounded requests");
            let result: HashMap<_, _> = result.into_iter().collect();
            assert!(matches!(result["tag:failed"], Err(client::Error::Server { status: 500, .. })));
            assert!(matches!(result["tag:unknown"], Err(client::Error::Unauthorized)));
            assert_eq!(result["path:home/ tag:inbox"], Ok(7));
            assert_eq!(result["tag:inbox"], Ok(1026));
            if !supersede {
                assert_eq!(result["tag:zero"], Ok(0));
                assert_eq!(result["path:work/ tag:inbox"], Ok(501));
                assert_eq!(result["tag:waiting"], Ok(19));
                assert_eq!(result["tag:limited"], Ok(43), "a transient rate limit must not leave a sidebar count unknown");
            }
        }
    }
}

/// Writes a thread's raw and prepared HTML for renderer benchmarks:
/// `DURIAN_URL=… DURIAN_THREAD=<id> DURIAN_OUT=/tmp/dir cargo test -p mail dump_html -- --ignored`
/// (private mail: delete the output afterwards).
#[cfg(test)]
mod dump {
    #[test]
    #[ignore]
    fn dump_html() {
        let (Ok(url), Ok(id), Ok(out)) = (std::env::var("DURIAN_URL"), std::env::var("DURIAN_THREAD"), std::env::var("DURIAN_OUT")) else { return };
        let content = super::Client::new(&url, None).thread(&id).unwrap();
        for (n, m) in content.messages.iter().enumerate() {
            if let Some(raw) = &m.html {
                std::fs::write(format!("{out}/{n}.raw.html"), raw).unwrap();
                std::fs::write(format!("{out}/{n}.prepared.html"), super::super::prepared_html(m, &[], Default::default()).unwrap_or_default()).unwrap();
            }
        }
    }
}
