//! Mail domain model. Tag-based: a thread has no folder, only tags. "Inbox" and "archive" are
//! just the presence/absence of a tag, which keeps the app and the CLI on the same rule engine.

use std::collections::HashSet;

use gpui::{Context, EventEmitter, Hsla, SharedString};
use serde::{Deserialize, Deserializer};

use ui::theme::parse_css_color;

fn color<'de, D: Deserializer<'de>>(d: D) -> Result<Hsla, D::Error> {
    let s = String::deserialize(d)?;
    parse_css_color(&s).ok_or_else(|| serde::de::Error::custom(format!("bad color {s}")))
}

/// Pre-downloaded picture (`tools/fetch_avatars.mjs`): a gravatar photo or a sender's logo.
#[derive(Deserialize, Clone)]
pub struct AvatarImage {
    pub path: SharedString,
    pub logo: bool,
}

#[derive(Deserialize, Clone)]
pub struct Contact {
    pub name: SharedString,
    pub email: SharedString,
    #[serde(default)]
    pub avatar: Option<AvatarImage>,
}

#[derive(Deserialize, Clone)]
pub struct Tag {
    pub name: SharedString,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

/// One mail account. The app shows all accounts in one inbox; the switcher filters.
#[derive(Deserialize, Clone)]
pub struct Account {
    pub id: SharedString,
    pub name: SharedString,
    pub email: SharedString,
    /// Extra sending addresses of this account.
    #[serde(default)]
    pub aliases: Vec<SharedString>,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

/// A folder of a profile: a named query in the data source's language.
#[derive(Clone, Debug, PartialEq)]
pub struct Folder {
    pub name: SharedString,
    /// an SF Symbols name ("tray", "paperplane", …), as durian's config writes them
    pub icon: SharedString,
    pub query: SharedString,
}

/// A named group of accounts with its own folders (durian's profiles). The account switcher
/// shows profiles when the data source has them.
#[derive(Clone, Debug, PartialEq)]
pub struct Profile {
    pub name: SharedString,
    /// account names, aliases or addresses; `*` = all
    pub accounts: Vec<SharedString>,
    pub default: bool,
    pub color: Option<Hsla>,
    pub folders: Vec<Folder>,
}

impl Profile {
    /// Where the profile opens: its first folder (durian's standard sets start with Inbox).
    pub fn home(&self) -> Option<&Folder> {
        self.folders.first()
    }
}

/// The profile the app starts in: the one marked default, else the first.
pub fn default_profile(profiles: &[Profile]) -> Option<usize> {
    profiles.iter().position(|p| p.default).or((!profiles.is_empty()).then_some(0))
}

#[derive(Deserialize, Clone)]
pub struct SavedView {
    pub name: SharedString,
    pub query: SharedString,
}

#[derive(Deserialize, Clone, Debug, PartialEq)]
pub struct Attachment {
    pub name: SharedString,
    pub size: SharedString,
    /// The file on disk, for Quick Look / download / Finder. Filled in by whoever provides the
    /// data (the backend, or the app's demo fixture).
    #[serde(default)]
    pub path: Option<std::path::PathBuf>,
    /// Where the data source can fetch it, when it isn't on disk yet.
    #[serde(skip)]
    pub remote: Option<RemotePart>,
}

/// An attachment on a server: the message it's part of and its MIME part.
#[derive(Clone, Debug, PartialEq)]
pub struct RemotePart {
    pub message: SharedString,
    pub part: i64,
}

#[derive(Deserialize, Clone)]
pub struct Message {
    /// The data source's id for it (opaque); empty for mail written here.
    #[serde(default)]
    pub id: SharedString,
    /// RFC threading metadata; never substitute the opaque row id above.
    #[serde(default)] pub message_id: Option<SharedString>,
    #[serde(default)] pub in_reply_to: Option<SharedString>,
    #[serde(default)] pub references: Option<SharedString>,
    #[serde(default)] pub account: Option<SharedString>,
    pub from: Contact,
    pub to: Vec<SharedString>,
    #[serde(default)]
    pub cc: Vec<SharedString>,
    /// Only ever set on mail we sent – recipients never see Bcc.
    #[serde(default)]
    pub bcc: Vec<SharedString>,
    pub at: SharedString,
    pub ago: SharedString,
    pub read: bool,
    pub body: SharedString,
    /// `body` is mail's text/plain as written, not Markdown (mail from elsewhere; the demo
    /// data and mail written here are Markdown).
    #[serde(default)]
    pub plain: bool,
    /// Quoted history / signature, already separated from `body` in the fixture.
    #[serde(default)]
    pub quoted: Option<SharedString>,
    #[serde(default)]
    pub attachments: Vec<Attachment>,
    /// Asset path of the (backend-sanitized) HTML part; `body` holds the text/plain alternative.
    #[serde(default)]
    pub html_source: Option<SharedString>,
    /// A calendar invitation carried by this message: the event's id in the calendar data.
    #[serde(default)]
    pub invite: Option<SharedString>,
    /// An invitation carried with its event (from durian): shown even when the calendar
    /// hasn't loaded that event.
    #[serde(skip)]
    pub invitation: Option<crate::invite::Invitation>,
    /// Loaded HTML, filled from `html_source` at startup.
    #[serde(skip)]
    pub html: Option<SharedString>,
    /// For mail we sent: the RFC 5322 message as written to the outbox.
    #[serde(skip)]
    pub eml: Option<std::path::PathBuf>,
}

#[derive(Deserialize, Clone, Copy, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum EventIcon {
    Label,
    Clock,
    Forward,
}

#[derive(Deserialize, Clone)]
pub struct Event {
    pub actor: SharedString,
    pub text: SharedString,
    pub target: SharedString,
    pub icon: EventIcon,
    pub ago: SharedString,
}

/// A thread is a timeline: messages interleaved with what happened to the thread.
#[derive(Deserialize, Clone)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ThreadItem {
    Message(Message),
    Event(Event),
}

#[derive(Deserialize, Clone)]
pub struct Thread {
    pub id: SharedString,
    pub subject: SharedString,
    pub tags: Vec<SharedString>,
    #[serde(default)]
    pub starred: bool,
    /// Account id the thread arrived in.
    #[serde(default)]
    pub account: SharedString,
    pub items: Vec<ThreadItem>,
    /// Only a summary so far (a search row): the full thread loads when it's opened.
    #[serde(default)]
    pub partial: bool,
}

impl Message {
    /// Body flattened to one line for previews. A row shows a line at most, and this runs per
    /// row per frame: only the body's start is read, whatever its length.
    pub fn preview(&self) -> SharedString {
        const READ: usize = 600;
        let end = (0..=READ.min(self.body.len())).rev().find(|&i| self.body.is_char_boundary(i)).unwrap_or(0);
        let start = &self.body[..end];
        let text = if self.plain { start.to_string() } else { ui::markdown::plain_text(start) };
        text.split_whitespace().collect::<Vec<_>>().join(" ").into()
    }
}

/// Identity of a message's rendered HTML: its id from the data source (stable when messages
/// arrive or the thread is refetched; the item index for local data), plus the HTML's digest
/// so a replaced body renders again.
pub fn message_key(thread: &Thread, item_ix: usize) -> SharedString {
    match thread.items.get(item_ix) {
        Some(ThreadItem::Message(m)) if !m.id.is_empty() => format!("{}#{}#{:x}", thread.id, m.id, md5::compute(m.html.as_deref().unwrap_or("").as_bytes())).into(),
        _ => format!("{}#{item_ix}", thread.id).into(),
    }
}

impl Thread {
    /// Render jobs for every HTML message in this thread, latest first.
    pub fn html_jobs(&self) -> Vec<crate::html_render::HtmlJob> {
        self.items
            .iter()
            .enumerate()
            .rev()
            .filter_map(|(ix, item)| match item {
                ThreadItem::Message(Message { html: Some(html), .. }) => {
                    Some(crate::html_render::HtmlJob { key: message_key(self, ix), html: html.clone() })
                }
                _ => None,
            })
            .collect()
    }

    pub fn messages(&self) -> impl DoubleEndedIterator<Item = &Message> {
        self.items.iter().filter_map(|i| match i {
            ThreadItem::Message(m) => Some(m),
            ThreadItem::Event(_) => None,
        })
    }

    pub fn latest(&self) -> &Message {
        self.messages().next_back().expect("thread without messages")
    }

    pub fn unread(&self) -> bool {
        self.messages().any(|m| !m.read)
    }

    /// Distinct senders in order of first appearance – shown as "Mara, Tobias, me".
    pub fn senders(&self) -> Vec<SharedString> {
        let mut out: Vec<SharedString> = Vec::new();
        for m in self.messages() {
            if !out.contains(&m.from.name) {
                out.push(m.from.name.clone());
            }
        }
        out
    }

    /// Everyone on the thread: senders, recipients and cc.
    pub fn people_count(&self) -> usize {
        let mut all: Vec<&SharedString> = Vec::new();
        for m in self.messages() {
            for name in std::iter::once(&m.from.name).chain(&m.to).chain(&m.cc) {
                if !all.contains(&name) {
                    all.push(name);
                }
            }
        }
        all.len()
    }
}

/// Everything the mail UI shows, as handed over by the data source: the user, their accounts,
/// tags, saved views and threads (with HTML bodies and attachment paths resolved).
#[derive(Deserialize)]
pub struct MailData {
    pub me: Contact,
    pub accounts: Vec<Account>,
    pub tags: Vec<Tag>,
    pub views: Vec<SavedView>,
    pub threads: Vec<Thread>,
}

/// Unsent mail. Replies autosave per thread; new messages are
/// saved explicitly when the composer closes with content.
#[derive(Clone)]
pub enum DraftKind {
    New { account: SharedString, from: SharedString, to: Vec<SharedString>, cc: Vec<SharedString>, bcc: Vec<SharedString>, subject: SharedString },
    Reply { thread_id: SharedString, all: bool },
}

#[derive(Clone)]
pub struct Draft {
    pub id: usize,
    pub kind: DraftKind,
    pub body: SharedString,
    pub saved_at: std::time::Instant,
    pub wire: crate::durian::api::DraftContent,
    pub sending: bool,
    pub deleting: bool,
    pub queued: Option<crate::durian::api::Enqueued>,
}

impl Draft {
    pub fn age(&self) -> SharedString {
        let secs = self.saved_at.elapsed().as_secs();
        match secs {
            0..=59 => "now".into(),
            60..=3599 => format!("{}min", secs / 60).into(),
            _ => format!("{}h", secs / 3600).into(),
        }
    }
}

/// Whether a list or thread is still coming from the server, or why it didn't.
#[derive(Clone, Debug, PartialEq)]
pub enum Loading {
    Pending,
    Failed(SharedString),
}

/// The list as a server's answer: `threads` holds what the server returned for `server_query`
/// (newest first, at most `limit`), and the client narrows it further by `refine`. The I/O
/// lives in `durian::Live`; this is only what the UI shows.
pub struct Remote {
    pub server_query: String,
    pub refine: crate::query::Query,
    pub limit: usize,
    /// None: the list is current
    pub loading: Option<Loading>,
    /// The server had no more rows than asked for: there is nothing further to page in.
    pub complete: bool,
    /// Full threads being fetched (or that failed) by id. A `partial` thread missing here
    /// hasn't been asked for yet.
    pub threads: std::collections::HashMap<SharedString, Loading>,
}

/// Selection semantics, independent of the keys that invoke them. Ranges derive from
/// the current visible order; explicit marks follow stable IDs across refresh/paging.
#[derive(Default)]
enum ListSelection {
    #[default]
    Single,
    Range { anchor: SharedString },
    Toggle { marked: HashSet<SharedString> },
}

/// Shared mailbox state. Views hold an `Entity<Mailbox>` and re-render on `cx.notify()`.
pub struct Mailbox {
    pub me: Contact,
    pub accounts: Vec<Account>,
    /// None = all accounts.
    pub account_filter: Option<SharedString>,
    /// Active search / view query (same language as the CLI), and what to call it in the header.
    pub query_text: SharedString,
    pub query: crate::query::Query,
    pub view_title: SharedString,
    pub tags: Vec<Tag>,
    pub views: Vec<SavedView>,
    pub threads: Vec<Thread>,
    pub selected: Option<usize>,
    selection: ListSelection,
    pub drafts: Vec<Draft>,
    /// The messages that were unread when their thread was opened (thread id, item indices):
    /// they stay expanded while you read, even though opening marked them read.
    pub unread_on_open: Option<(SharedString, Vec<usize>)>,
    /// Set when the threads come from a server rather than from local data.
    pub remote: Option<Remote>,
    /// The data source's profiles (none: the switcher shows accounts), the active one, and
    /// the open folder in it. A server-backed list searches within the folder's query.
    pub profiles: Vec<Profile>,
    pub profile: Option<usize>,
    pub folder: Option<usize>,
    /// Authoritative unread thread counts, keyed by the exact scoped API query.
    /// Missing means unknown, not zero. Search pages never populate this cache.
    pub server_counts: std::collections::HashMap<String, u64>,
    /// Last desired operations remain overlaid on periodic reads until writes drain.
    pub pending_tags: std::collections::HashMap<SharedString, String>,
    pub confirmed_tags: std::collections::HashMap<SharedString, Thread>,
    pub writes_available: bool,
    pub unread_by_user: Option<SharedString>,
    next_draft_id: usize,
}

pub enum MailboxEvent {
    SelectionChanged,
    /// The query text changed (a server-backed list has to ask again).
    QueryChanged,
    /// A thread was swapped for its full version (HTML to render, a view to repaint).
    ThreadLoaded(SharedString),
    /// The list was scrolled near its end: a server-backed list fetches the next page.
    MoreWanted,
    /// A change was asked for, but the data is only read from a server (nothing changed).
    ReadOnly,
    WriteRequested(crate::durian::writes::Intent),
    DraftSaved(usize),
    DraftDeleted(usize),
    WriteProblem(SharedString),
    SendFinished { draft_id: usize, result: crate::durian::client::Result<crate::durian::api::Enqueued> },
}

impl EventEmitter<MailboxEvent> for Mailbox {}

impl Mailbox {
    pub fn new(data: MailData) -> Self {
        let MailData { me, accounts, tags, views, threads } = data;
        let selected = (!threads.is_empty()).then_some(0);
        Self { me, accounts, account_filter: None, query_text: SharedString::default(), query: Default::default(), view_title: "Inbox".into(), tags, views, threads, selected, selection: ListSelection::Single, drafts: Vec::new(), unread_on_open: None, remote: None, profiles: Vec::new(), profile: None, folder: None, server_counts: std::collections::HashMap::new(), pending_tags: Default::default(), confirmed_tags: Default::default(), writes_available: false, unread_by_user: None, next_draft_id: 1 }
    }

    /// An empty mailbox whose threads a server provides.
    pub fn remote(data: MailData, remote: Remote) -> Self {
        Self { remote: Some(remote), ..Self::new(data) }
    }

    /// The clauses `visible()` applies: everything locally, or what the server couldn't.
    fn filter(&self) -> &crate::query::Query {
        self.remote.as_ref().map_or(&self.query, |r| &r.refine)
    }

    /// A server-backed list narrows differently now (the query changed, but the server's
    /// part didn't – or a new answer is on its way).
    pub fn set_refine(&mut self, refine: crate::query::Query, cx: &mut Context<Self>) {
        if let Some(remote) = self.remote.as_mut() {
            remote.refine = refine;
            self.keep_selection_visible(cx);
            cx.notify();
        }
    }

    /// The server's answer replaces the list. The selection follows its thread (by id), so a
    /// refresh or a page more doesn't move you; if it's gone, the first visible thread.
    pub fn replace_threads(&mut self, mut threads: Vec<Thread>, cx: &mut Context<Self>) {
        let selected = self.selected_thread().map(|t| t.id.clone());
        for thread in &mut threads {
            if let Some(ops) = self.pending_tags.get(&thread.id) { crate::durian::writes::apply_tags(thread, ops); }
        }
        self.threads = threads;
        self.selected = selected.and_then(|id| self.threads.iter().position(|t| t.id == id));
        if let Some(remote) = self.remote.as_mut() {
            remote.threads.retain(|id, _| self.threads.iter().any(|t| t.id == *id));
        }
        if self.selected.is_none() {
            self.keep_selection_visible(cx);
        }
        self.reconcile_selection();
        cx.notify();
    }

    /// A full thread arrived for a list row.
    pub fn fill_thread(&mut self, mut thread: Thread, cx: &mut Context<Self>) {
        if let Some(ops) = self.pending_tags.get(&thread.id) { crate::durian::writes::apply_tags(&mut thread, ops); }
        if let Some(remote) = self.remote.as_mut() {
            remote.threads.remove(&thread.id);
        }
        if let Some(slot) = self.threads.iter_mut().find(|t| t.id == thread.id) {
            let id = thread.id.clone();
            *slot = thread;
            cx.emit(MailboxEvent::ThreadLoaded(id));
            cx.notify();
        }
    }

    /// The list shows its last rows. Asks once per page (the source marks itself loading).
    pub fn want_more(&mut self, cx: &mut Context<Self>) {
        if self.remote.as_ref().is_some_and(|r| !r.complete && r.loading.is_none()) {
            cx.emit(MailboxEvent::MoreWanted);
        }
    }

    /// Mail writes are routed to the live controller when remote is set.
    pub fn read_only(&self) -> bool {
        false
    }

    /// Guard for every change: true (and an explanation for the user) when it's refused.
    fn refuse(&self, cx: &mut Context<Self>) -> bool {
        if self.read_only() {
            cx.emit(MailboxEvent::ReadOnly);
        }
        self.read_only()
    }

    /// A message's HTML arrived after its thread (its embedded images had to be fetched).
    pub fn set_html(&mut self, thread_id: &str, message_id: &str, html: SharedString, cx: &mut Context<Self>) {
        let Some(thread) = self.threads.iter_mut().find(|t| t.id == thread_id) else { return };
        for item in &mut thread.items {
            if let ThreadItem::Message(m) = item
                && m.id == message_id
            {
                m.html = Some(html.clone());
            }
        }
        cx.emit(MailboxEvent::ThreadLoaded(thread.id.clone()));
        cx.notify();
    }

    /// A server attachment now has a local copy (fetched by the data source, not a change).
    pub fn set_attachment_path(&mut self, part: &RemotePart, path: std::path::PathBuf, cx: &mut Context<Self>) {
        let messages = self.threads.iter_mut().flat_map(|t| t.items.iter_mut()).filter_map(|i| match i {
            ThreadItem::Message(m) => Some(m),
            ThreadItem::Event(_) => None,
        });
        for a in messages.flat_map(|m| m.attachments.iter_mut()).filter(|a| a.remote.as_ref() == Some(part)) {
            a.path = Some(path.clone());
        }
        cx.notify();
    }

    pub fn active_profile(&self) -> Option<&Profile> {
        self.profile.and_then(|ix| self.profiles.get(ix))
    }

    /// The open folder of the active profile.
    pub fn active_folder(&self) -> Option<&Folder> {
        self.active_profile().zip(self.folder).and_then(|(p, ix)| p.folders.get(ix))
    }

    /// The data source's profiles arrived (or changed): keep the active one by name, else
    /// open the default one at its first folder.
    pub fn set_profiles(&mut self, profiles: Vec<Profile>, cx: &mut Context<Self>) {
        let current = self.active_profile().map(|p| p.name.clone());
        let keep = current.and_then(|name| profiles.iter().position(|p| p.name == name));
        self.profiles = profiles;
        match keep {
            Some(ix) => {
                self.profile = Some(ix);
                // None is an explicit tag/view destination, not an uninitialized folder.
                self.folder = self.folder.map(|f| if f < self.profiles[ix].folders.len() { f } else { 0 });
                cx.emit(MailboxEvent::QueryChanged);
                cx.notify();
            }
            None => match default_profile(&self.profiles) {
                Some(ix) => self.set_profile(ix, cx),
                None => cx.notify(),
            },
        }
    }

    /// Switch profile; it opens at its first folder.
    pub fn set_profile(&mut self, ix: usize, cx: &mut Context<Self>) {
        if ix < self.profiles.len() {
            self.profile = Some(ix);
            self.open_folder(0, cx);
        }
    }

    /// Open a folder of the active profile (its query becomes the list's base).
    pub fn open_folder(&mut self, ix: usize, cx: &mut Context<Self>) {
        let Some(folder) = self.active_profile().and_then(|p| p.folders.get(ix)).cloned() else { return };
        self.selection = ListSelection::Single;
        self.folder = Some(ix);
        self.view_title = folder.name;
        cx.emit(MailboxEvent::QueryChanged);
        cx.notify();
    }

    pub fn thread_loading(&self, id: &str) -> Option<&Loading> {
        self.remote.as_ref().and_then(|r| r.threads.get(id))
    }

    pub fn tag(&self, name: &str) -> Option<&Tag> {
        self.tags.iter().find(|t| t.name == name)
    }

    /// Sidebar scopes don't include the open folder or typed search. Each badge
    /// counts its own base query, independently of the list's page and refinement.
    pub fn sidebar_count_query(&self, base: &str) -> String {
        let scope = match (self.active_profile(), self.account_filter.as_deref()) {
            (Some(profile), _) => crate::durian::profiles::scope(profile),
            (None, Some(account)) => Some(crate::durian::translate::account_scope(account)),
            (None, None) => None,
        };
        unread_query(scope.as_deref(), base)
    }

    pub fn count_for(&self, query: &str) -> Option<u64> {
        self.server_counts.get(query).copied()
    }

    /// Every configured profile's folders, account inboxes, and the visible tags.
    /// Deduplication uses the query itself, not folder names that repeat in profiles.
    pub fn count_queries(&self) -> Vec<String> {
        if self.remote.is_none() {
            return Vec::new(); // Demo counts still come from its complete local data.
        }
        let mut queries = vec![account_inbox_query(None), self.sidebar_count_query("tag:inbox")];
        queries.extend(self.accounts.iter().map(|a| account_inbox_query(Some(&a.id))));
        queries.extend(self.profiles.iter().flat_map(|p| p.folders.iter().map(move |f| folder_unread_query(p, f))));
        queries.extend(self.tags.iter().map(|t| self.sidebar_count_query(&format!("tag:{}", crate::durian::translate::quoted(&t.name)))));
        queries.sort();
        queries.dedup();
        queries
    }

    /// A failed request leaves the last known count alone. A successful zero is
    /// known zero; a failed first request stays absent/unknown.
    pub fn update_counts(&mut self, counts: impl IntoIterator<Item = (String, Option<u64>)>) {
        self.server_counts.extend(counts.into_iter().filter_map(|(query, count)| Some((query, count?))));
    }

    /// Unread visible threads, in total and per tag – one pass for the whole sidebar.
    pub fn unread_counts(&self) -> (usize, std::collections::HashMap<SharedString, usize>) {
        let mut per_tag = std::collections::HashMap::new();
        let mut total = 0;
        for thread in self.visible().map(|ix| &self.threads[ix]).filter(|t| t.unread()) {
            total += 1;
            for tag in &thread.tags {
                *per_tag.entry(tag.clone()).or_default() += 1;
            }
        }
        (total, per_tag)
    }

    /// Unread threads carrying a tag – totals are noise in a mail sidebar.
    pub fn tag_unread_count(&self, name: &str) -> usize {
        self.visible().map(|ix| &self.threads[ix]).filter(|t| t.unread() && t.tags.iter().any(|n| n == name)).count()
    }

    pub fn unread_count(&self) -> usize {
        self.visible().filter(|&ix| self.threads[ix].unread()).count()
    }

    pub fn account_unread(&self, id: &str) -> usize {
        self.threads.iter().filter(|t| t.account == id && t.unread()).count()
    }

    pub fn account(&self, id: &str) -> Option<&Account> {
        self.accounts.iter().find(|a| a.id == id)
    }

    /// Thread indices that pass the account filter, in list order.
    pub fn visible(&self) -> impl Iterator<Item = usize> + '_ {
        let matcher = self.filter().matcher(self);
        (0..self.threads.len()).filter(move |&ix| {
            let thread = &self.threads[ix];
            // a server-backed list is already scoped to the account (its rows don't name one)
            (self.remote.is_some() || self.account_filter.as_ref().is_none_or(|a| thread.account == *a)) && matcher.matches(thread)
        })
    }

    /// Apply a query (search bar, view, tag click). `title` names it in the list header.
    pub fn set_query(&mut self, text: SharedString, title: Option<SharedString>, cx: &mut Context<Self>) {
        if self.query_text != text { self.selection = ListSelection::Single; }
        self.query = crate::query::Query::parse(&text);
        self.view_title = title.unwrap_or_else(|| if text.trim().is_empty() { "Inbox".into() } else { "Search".into() });
        self.query_text = text;
        if self.remote.is_some() {
            // the server part goes out, the rest narrows here (`set_refine`)
            cx.emit(MailboxEvent::QueryChanged);
            cx.notify();
            return;
        }
        crate::trace::note(&format!("query {:?} ({}) → {} threads", self.query_text, self.view_title, self.visible().count()));
        self.keep_selection_visible(cx);
        cx.notify();
    }

    /// Pin the current query as a sidebar view.
    pub fn save_view(&mut self, name: SharedString, cx: &mut Context<Self>) {
        if !self.query_text.trim().is_empty() && !self.views.iter().any(|v| v.query == self.query_text) {
            self.views.push(SavedView { name: name.clone(), query: self.query_text.trim().to_string().into() });
            self.view_title = name;
            cx.notify();
        }
    }

    fn keep_selection_visible(&mut self, cx: &mut Context<Self>) {
        let visible: Vec<usize> = self.visible().collect();
        if !self.selected.is_some_and(|s| visible.contains(&s)) {
            self.selected = None;
            if let Some(&first) = visible.first() {
                self.select(first, cx);
            } else {
                cx.emit(MailboxEvent::SelectionChanged);
            }
        }
        self.reconcile_selection();
    }

    pub fn set_account_filter(&mut self, filter: Option<SharedString>, cx: &mut Context<Self>) {
        crate::trace::note(&format!("account filter: {filter:?}"));
        if self.account_filter != filter { self.selection = ListSelection::Single; }
        self.account_filter = filter;
        if self.remote.is_some() {
            cx.emit(MailboxEvent::QueryChanged);
            cx.notify();
            return;
        }
        // keep the selection if it's still visible, else jump to the first visible thread
        self.keep_selection_visible(cx);
        cx.notify();
    }

    /// Addresses the composer may send from: the active profile's own addresses (primary +
    /// aliases), or every account's in All accounts.
    pub fn sender_options(&self) -> Vec<(SharedString, SharedString)> {
        self.accounts
            .iter()
            .filter(|a| self.account_filter.as_ref().is_none_or(|f| a.id == *f))
            .flat_map(|a| std::iter::once(a.email.clone()).chain(a.aliases.iter().cloned()).map(move |e| (a.id.clone(), e)))
            .collect()
    }

    /// The identity to send from: the thread's account for replies, else the filtered account,
    /// else the first (work) account.
    pub fn sending_account(&self, thread: Option<&Thread>) -> Option<&Account> {
        thread
            .and_then(|t| self.account(&t.account))
            .or_else(|| self.account_filter.as_ref().and_then(|id| self.account(id)))
            .or_else(|| self.accounts.first())
    }

    pub fn is_me(&self, contact: &Contact) -> bool {
        contact.email == self.me.email || self.accounts.iter().any(|a| a.email == contact.email)
    }

    pub fn selected_thread(&self) -> Option<&Thread> {
        self.selected.and_then(|ix| self.threads.get(ix))
    }

    pub fn selection_mode(&self) -> Option<&'static str> {
        match self.selection {
            ListSelection::Single => None,
            ListSelection::Range { .. } => Some("Visual"),
            ListSelection::Toggle { .. } => Some("Toggle"),
        }
    }

    /// The action target, in list order. Hidden rows are never acted on. Counts
    /// extend a single cursor only; a marked selection always means exactly its IDs.
    pub fn selected_ids(&self, count: usize) -> Vec<SharedString> {
        let Some(cursor) = self.selected else { return Vec::new() };
        let visible: Vec<_> = self.visible().collect();
        match &self.selection {
            ListSelection::Single => visible.into_iter().skip_while(|&ix| ix != cursor).take(count).map(|ix| self.threads[ix].id.clone()).collect(),
            ListSelection::Range { anchor } => {
                let start = visible.iter().position(|&ix| self.threads[ix].id == *anchor);
                let end = visible.iter().position(|&ix| ix == cursor);
                match start.zip(end) {
                    Some((a, b)) => visible[a.min(b)..=a.max(b)].iter().map(|&ix| self.threads[ix].id.clone()).collect(),
                    None => Vec::new(),
                }
            }
            ListSelection::Toggle { marked } => visible.into_iter().filter_map(|ix| {
                let id = &self.threads[ix].id;
                marked.contains(id).then(|| id.clone())
            }).collect(),
        }
    }

    pub fn begin_selection(&mut self, toggle: bool, cx: &mut Context<Self>) {
        if self.selection_mode().is_some() { return }
        let Some(id) = self.selected_ids(1).pop() else { return };
        self.selection = if toggle { ListSelection::Toggle { marked: [id].into() } } else { ListSelection::Range { anchor: id } };
        cx.notify();
    }

    pub fn clear_selection(&mut self, cx: &mut Context<Self>) {
        self.selection = ListSelection::Single;
        cx.notify();
    }

    pub fn toggle_selection(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self.selected_thread().map(|t| t.id.clone()) else { return };
        let ListSelection::Toggle { marked } = &mut self.selection else { return };
        if !marked.contains(&id) { marked.insert(id); }
        else if marked.len() > 1 { marked.remove(&id); }
        self.select_relative(1, cx);
        cx.notify();
    }

    fn reconcile_selection(&mut self) {
        if self.selection_mode().is_none() { return }
        let visible: HashSet<_> = self.visible().map(|ix| self.threads[ix].id.clone()).collect();
        let reset = match &mut self.selection {
            ListSelection::Single => false,
            ListSelection::Range { anchor } => !visible.contains(anchor),
            ListSelection::Toggle { marked } => {
                marked.retain(|id| visible.contains(id));
                marked.is_empty()
            }
        };
        if reset { self.selection = ListSelection::Single; }
    }

    pub fn select(&mut self, ix: usize, cx: &mut Context<Self>) {
        if ix < self.threads.len() && self.selected != Some(ix) {
            self.selected = Some(ix);
            crate::trace::note(&format!("selected {ix}: {}", self.threads[ix].id));
            cx.emit(MailboxEvent::SelectionChanged);
            cx.notify();
        }
    }

    /// j/k move through the visible (account-filtered) list.
    pub fn select_relative(&mut self, delta: isize, cx: &mut Context<Self>) {
        let visible: Vec<usize> = self.visible().collect();
        if visible.is_empty() {
            return;
        }
        let current = self.selected.and_then(|s| visible.iter().position(|&v| v == s)).map_or(-1, |p| p as isize);
        let target = (current + delta).clamp(0, visible.len() as isize - 1) as usize;
        self.select(visible[target], cx);
    }

    /// Opening a thread reads it.
    pub fn mark_selected_read(&mut self, cx: &mut Context<Self>) {
        // Moving the multi-select cursor is selection, not an instruction to read
        // every passing thread (or undo a bulk mark-unread operation).
        if self.selection_mode().is_some() { return }
        // Filtering can temporarily remove selection. That isn't viewing a
        // different thread and must not undo an explicit mark-unread intent.
        let Some(selected_id) = self.selected_thread().map(|t| t.id.clone()) else { return };
        let remote = self.remote.is_some();
        if remote && (!self.writes_available || self.unread_by_user.as_ref() == Some(&selected_id)) { return }
        self.unread_by_user = None;
        let mut mark = None;
        if let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) {
            let mut unread = Vec::new();
            for (ix, item) in thread.items.iter_mut().enumerate() {
                if let ThreadItem::Message(m) = item {
                    if !m.read {
                        unread.push(ix);
                    }
                    if remote && !m.read { mark = Some(thread.id.clone()); }
                    m.read |= !remote;
                }
            }
            // keep the first snapshot of this thread; a second call (right-click) finds nothing new
            if self.unread_on_open.as_ref().is_none_or(|(id, _)| *id != thread.id) || !unread.is_empty() {
                self.unread_on_open = Some((thread.id.clone(), unread));
            }
            cx.notify();
        }
        if let Some(id) = mark { self.request_tags(id, "-unread".into(), cx); }
    }

    /// "Archive" = drop the thread from the inbox view (in the real app: remove `tag:inbox`).
    pub fn archive_selected(&mut self, cx: &mut Context<Self>) {
        if self.remote.is_some() {
            self.tag_selected("+archive -inbox -trash", 1, cx);
            return;
        }
        if self.refuse(cx) {
            return;
        }
        let ids = self.selected_ids(1);
        let Some(ix) = self.threads.iter().position(|t| ids.contains(&t.id)) else { return };
        self.threads.retain(|t| !ids.contains(&t.id));
        self.selection = ListSelection::Single;
        // next visible thread below, else the one above
        let visible: Vec<usize> = self.visible().collect();
        self.selected = visible.iter().find(|&&v| v >= ix).or(visible.last()).copied();
        cx.emit(MailboxEvent::SelectionChanged);
        cx.notify();
    }

    /// A keymap tag_op or counted delete targets stable IDs, not a moving selection.
    pub fn tag_selected(&mut self, ops: &str, count: usize, cx: &mut Context<Self>) {
        if ops.trim().is_empty() || ops.split_whitespace().any(|op| op.len() < 2 || !op.starts_with(['+', '-']) || op[1..].starts_with(['+', '-'])) {
            cx.emit(MailboxEvent::WriteProblem("tag_op needs operations such as +todo -inbox".into()));
            return;
        }
        if self.refuse(cx) { return }
        if self.remote.is_some() && !self.writes_available {
            cx.emit(MailboxEvent::WriteProblem("Backend unavailable; no tags changed".into()));
            return;
        }
        let ids = self.selected_ids(count);
        let removes = ops.split_whitespace().any(|op| matches!(op, "-inbox" | "+trash" | "+archive"));
        let next = removes.then(|| {
            let visible: Vec<_> = self.visible().collect();
            let first = visible.iter().position(|&ix| ids.contains(&self.threads[ix].id))?;
            visible[first..].iter().chain(visible[..first].iter().rev()).copied().find(|&ix| !ids.contains(&self.threads[ix].id))
        }).flatten();
        let unread_op = ops.split_whitespace().filter(|op| matches!(*op, "+unread" | "-unread")).last();
        if self.selected_thread().is_some_and(|t| ids.contains(&t.id)) && unread_op.is_some() {
            self.unread_by_user = (unread_op == Some("+unread")).then(|| self.selected_thread().unwrap().id.clone());
        }
        for id in ids {
            if self.remote.is_some() { self.request_tags(id, ops.into(), cx); }
            else if let Some(thread) = self.threads.iter_mut().find(|t| t.id == id) { crate::durian::writes::apply_tags(thread, ops); }
        }
        if removes {
            self.selection = ListSelection::Single;
            if let Some(ix) = next { self.select(ix, cx); }
        }
        cx.notify();
    }

    pub fn toggle_tag_selected(&mut self, name: &SharedString, cx: &mut Context<Self>) {
        if name.is_empty() || name.chars().any(char::is_whitespace) || name.starts_with(['+', '-']) {
            cx.emit(MailboxEvent::WriteProblem("Tag names cannot contain spaces or start with + or -".into()));
            return;
        }
        let ids = self.selected_ids(1);
        let all = !ids.is_empty() && self.threads.iter().filter(|t| ids.contains(&t.id)).all(|t| t.tags.contains(name));
        // Mixed selections converge: add to all unless all already have the tag.
        self.tag_selected(&format!("{}{name}", if all { '-' } else { '+' }), 1, cx);
    }

    /// New tags get a color from a fixed palette, picked by name so it is stable.
    pub fn create_tag(&mut self, name: SharedString, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        const PALETTE: [(f32, f32, f32); 6] =
            [(55., 60., 290.), (62., 55., 150.), (65., 70., 60.), (58., 65., 25.), (60., 50., 230.), (66., 55., 330.)];
        let (l, c, h) = PALETTE[name.bytes().map(usize::from).sum::<usize>() % PALETTE.len()];
        self.tags.push(Tag { name, color: ui::theme::lch(l, c, h) });
        cx.notify();
    }

    /// Everyone we have an address for, for recipient suggestions.
    pub fn contacts(&self) -> Vec<Contact> {
        let mut out: Vec<Contact> = Vec::new();
        for m in self.threads.iter().flat_map(|t| t.messages()) {
            if !out.iter().any(|c| c.email == m.from.email) && m.from.email != self.me.email {
                out.push(m.from.clone());
            }
        }
        out
    }

    fn outgoing(&self, from_email: Option<SharedString>, to: Vec<SharedString>, cc: Vec<SharedString>, body: SharedString) -> Message {
        let mut from = self.me.clone();
        if let Some(email) = from_email {
            from.email = email;
        }
        Message {
            id: SharedString::default(),
            message_id: None, in_reply_to: None, references: None,
            account: None,
            plain: false,
            from,
            to,
            cc,
            bcc: Vec::new(),
            at: "Just now".into(),
            ago: "now".into(),
            read: true,
            body,
            quoted: None,
            attachments: Vec::new(),
            html_source: None,
            invite: None,
            invitation: None,
            html: None,
            eml: None,
        }
    }

    fn address(&self, name: &SharedString) -> crate::mime::Address {
        let email = if name.contains('@') {
            Some(name.to_string())
        } else if *name == self.me.name {
            Some(self.me.email.to_string())
        } else {
            self.contacts().into_iter().find(|c| c.name == *name).map(|c| c.email.to_string())
        };
        crate::mime::Address { name: name.to_string(), email }
    }

    /// Builds the multipart/alternative message and drops it in the outbox (the "send").
    fn write_outbox(&self, message: &mut Message, subject: &str) {
        assert!(self.remote.is_none(), "live sending must use the encrypted HTTP outbox");
        let addresses = |names: &[SharedString]| names.iter().map(|n| self.address(n)).collect::<Vec<_>>();
        let raw = crate::mime::build(&crate::mime::Outgoing {
            from: crate::mime::Address { name: message.from.name.to_string(), email: Some(message.from.email.to_string()) },
            to: addresses(&message.to),
            cc: addresses(&message.cc),
            bcc: addresses(&message.bcc),
            subject,
            in_reply_to: None,
            markdown: &message.body,
        });
        let dir = crate::cache_dir("outbox");
        let path = dir.join(format!("{}.eml", uuid::Uuid::new_v4().simple()));
        if std::fs::create_dir_all(&dir).and_then(|_| std::fs::write(&path, raw)).is_ok() {
            crate::trace::note(&format!("outbox: {}", path.display()));
            message.eml = Some(path);
        }
    }

    pub fn send_reply(&mut self, to: Vec<SharedString>, cc: Vec<SharedString>, body: SharedString, cx: &mut Context<Self>) {
        if self.remote.is_some() {
            let Some(thread_id) = self.selected_thread().map(|t| t.id.clone()) else { return };
            let all = self.reply_draft(&thread_id).is_none_or(|d| matches!(d.kind, DraftKind::Reply { all: true, .. }));
            self.save_reply_draft(thread_id.clone(), all, body, cx);
            if let Some(id) = self.reply_draft(&thread_id).map(|d| d.id) { self.submit_draft(id, Some(all), cx); }
            return;
        }
        if self.refuse(cx) {
            return;
        }
        let from = self.sending_account(self.selected_thread()).map(|a| a.email.clone());
        let mut message = self.outgoing(from, to, cc, body);
        if let Some(subject) = self.selected_thread().map(|t| t.subject.clone()) {
            let subject = if subject.starts_with("Re:") { subject.to_string() } else { format!("Re: {subject}") };
            self.write_outbox(&mut message, &subject);
        }
        if let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) {
            crate::trace::note(&format!("sent reply on {}: {:?}", thread.id, message.body));
            thread.items.push(ThreadItem::Message(message));
            cx.notify();
        }
    }

    /// A new conversation goes to the top of the list and gets selected.
    pub fn send_new(
        &mut self,
        account: SharedString,
        from_email: SharedString,
        subject: SharedString,
        to: Vec<SharedString>,
        cc: Vec<SharedString>,
        bcc: Vec<SharedString>,
        body: SharedString,
        cx: &mut Context<Self>,
    ) {
        if self.remote.is_some() {
            let kind = DraftKind::New { account, from: from_email, to, cc, bcc, subject };
            if let Some(id) = self.save_new_draft(None, kind, body, cx) { self.submit_draft(id, None, cx); }
            return;
        }
        if self.refuse(cx) {
            return;
        }
        let mut message = self.outgoing(Some(from_email), to, cc, body);
        message.bcc = bcc;
        self.write_outbox(&mut message, &subject);
        let id: SharedString = format!("sent-{}", self.threads.len()).into();
        crate::trace::note(&format!("sent new thread {id}: {subject} / {:?}", message.body));
        self.threads.insert(0, Thread { id, subject, tags: Vec::new(), starred: false, account, partial: false, items: vec![ThreadItem::Message(message)] });
        self.selected = None;
        self.select(0, cx);
    }

    pub fn reply_draft(&self, thread_id: &str) -> Option<&Draft> {
        self.drafts.iter().find(|d| matches!(&d.kind, DraftKind::Reply { thread_id: t, .. } if t == thread_id))
    }

    /// Capture the full original reply target before the editor is shown. Profile
    /// changes, new mail and autosaves never choose another sender or Message-ID.
    pub fn begin_reply(&mut self, thread_id: SharedString, all: bool, cx: &mut Context<Self>) -> Option<usize> {
        if let Some(draft) = self.reply_draft(&thread_id).cloned() {
            if draft.sending || draft.deleting || draft.queued.is_some() || draft.wire.extra.contains_key(crate::durian::writes::PENDING_SEND) { return Some(draft.id) }
            if matches!(draft.kind, DraftKind::Reply { all: previous, .. } if previous == all) { return Some(draft.id) }
            let mut wire = draft.wire;
            let captured = crate::durian::api::ThreadContent { thread_id: thread_id.to_string(), subject: wire.subject.clone(), messages: Vec::new() };
            if self.remote.is_some() {
                if let Err(error) = crate::durian::writes::prepare_reply(&mut wire, &captured, all) {
                    cx.emit(MailboxEvent::WriteProblem(error.to_string().into())); return None;
                }
            }
            return self.save_draft_content(Some(draft.id), DraftKind::Reply { thread_id, all }, draft.body, wire, cx);
        }
        let thread = self.threads.iter().find(|t| t.id == thread_id)?;
        if self.remote.is_some() && thread.partial {
            cx.emit(MailboxEvent::WriteProblem("Wait for the full thread before starting a reply.".into()));
            return None;
        }
        let target = thread.latest();
        let account = target.account.as_ref().and_then(|id| self.account(id)).or_else(|| self.sending_account(Some(thread)))?;
        let now = chrono::Utc::now().to_rfc3339();
        let mut wire = crate::durian::api::DraftContent { id: uuid::Uuid::new_v4().to_string(), from: account.email.to_string(),
            account_id: Some(account.id.to_string()), reply_thread_id: Some(thread_id.to_string()), created_at: now.clone(), modified_at: now, ..Default::default() };
        let own: Vec<String> = self.accounts.iter().flat_map(|a| std::iter::once(&a.email).chain(&a.aliases)).map(ToString::to_string).collect();
        wire.extra.insert(crate::durian::writes::OWN_ADDRESSES.into(), serde_json::json!(own));
        let content = crate::durian::api::ThreadContent { thread_id: thread_id.to_string(), subject: thread.subject.to_string(), messages: vec![serde_json::from_value(serde_json::json!({
            "id": target.id, "account": target.account, "from": format!("{} <{}>", target.from.name, target.from.email),
            "to": target.to.iter().map(ToString::to_string).collect::<Vec<_>>().join(", "), "cc": target.cc.iter().map(ToString::to_string).collect::<Vec<_>>().join(", "),
            "message_id": target.message_id, "references": target.references, "timestamp": 0
        })).ok()?] };
        if let Err(e) = crate::durian::writes::prepare_reply(&mut wire, &content, all) {
            cx.emit(MailboxEvent::WriteProblem(e.to_string().into())); return None;
        }
        self.save_draft_content(None, DraftKind::Reply { thread_id, all }, SharedString::default(), wire, cx)
    }

    /// Autosave keeps the original target even when live reply text is emptied.
    pub fn save_reply_draft(&mut self, thread_id: SharedString, all: bool, body: SharedString, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        let existing = self.drafts.iter().position(|d| matches!(&d.kind, DraftKind::Reply { thread_id: t, .. } if *t == thread_id));
        if existing.is_some_and(|ix| self.drafts[ix].sending || self.drafts[ix].deleting || self.drafts[ix].queued.is_some() || self.drafts[ix].wire.extra.contains_key(crate::durian::writes::PENDING_SEND)) { return; }
        if self.remote.is_none() && body.trim().is_empty() {
            if let Some(ix) = existing {
                let id = self.drafts[ix].id;
                self.discard_draft(id, cx);
            }
            return;
        }
        if self.remote.is_some() && existing.is_none() && self.begin_reply(thread_id.clone(), all, cx).is_none() { return }
        let existing = self.reply_draft(&thread_id).map(|d| d.id);
        if let Some(draft) = self.drafts.iter_mut().find(|d| Some(d.id) == existing) {
            if draft.wire.extra.contains_key(crate::durian::writes::REPLY_TARGET) {
                let captured = crate::durian::api::ThreadContent { thread_id: thread_id.to_string(), subject: draft.wire.subject.clone(), messages: Vec::new() };
                if let Err(e) = crate::durian::writes::prepare_reply(&mut draft.wire, &captured, all) {
                    cx.emit(MailboxEvent::WriteProblem(e.to_string().into())); return;
                }
            }
        }
        let kind = DraftKind::Reply { thread_id, all };
        self.save_new_draft(existing, kind, body, cx);
    }

    /// Saves (or updates, when `id` is given) a new-message draft and returns its id (None:
    /// refused, read-only).
    pub fn save_new_draft(&mut self, id: Option<usize>, kind: DraftKind, body: SharedString, cx: &mut Context<Self>) -> Option<usize> {
        let wire = id.and_then(|id| self.drafts.iter().find(|d| d.id == id)).map(|d| d.wire.clone()).unwrap_or_else(|| {
            let now = chrono::Utc::now().to_rfc3339();
            crate::durian::api::DraftContent { id: uuid::Uuid::new_v4().to_string(), created_at: now.clone(), modified_at: now, ..Default::default() }
        });
        self.save_draft_content(id, kind, body, wire, cx)
    }

    pub fn save_draft_content(&mut self, id: Option<usize>, kind: DraftKind, body: SharedString, mut wire: crate::durian::api::DraftContent, cx: &mut Context<Self>) -> Option<usize> {
        if let Some(draft) = id.and_then(|id| self.drafts.iter().find(|d| d.id == id)) {
            if draft.sending || draft.deleting || draft.queued.is_some() || draft.wire.extra.contains_key(crate::durian::writes::PENDING_SEND) {
                cx.emit(MailboxEvent::WriteProblem("This send may already be queued. Retry its original request or Undo before editing.".into()));
                return None;
            }
        }
        match &kind {
            DraftKind::New { account, from, to, cc, bcc, subject } => {
                wire.account_id = Some(account.to_string()); wire.from = from.to_string();
                wire.to = to.iter().map(ToString::to_string).collect(); wire.cc = cc.iter().map(ToString::to_string).collect();
                wire.bcc = bcc.iter().map(ToString::to_string).collect(); wire.subject = subject.to_string();
            }
            DraftKind::Reply { thread_id, .. } => {
                wire.reply_thread_id = Some(thread_id.to_string());
                // begin_reply captured the sending identity, recipients and RFC
                // headers; autosave only updates the editable body and timestamp.
                if wire.body != body.as_ref() { wire.html_body = None; wire.is_html = false; }
            }
        }
        wire.body = body.to_string(); wire.modified_at = chrono::Utc::now().to_rfc3339();
        if self.remote.is_some() && !crate::durian::writes::draft_fits(&wire) {
            cx.emit(MailboxEvent::WriteProblem("Draft exceeds the server's 1 MiB recovery limit. Remove attachments; nothing was saved or sent.".into()));
            return None;
        }
        let id = id.unwrap_or_else(|| { let id = self.next_draft_id; self.next_draft_id += 1; id });
        let updated = Draft { id, kind, body, saved_at: std::time::Instant::now(), wire: wire.clone(), sending: false, deleting: false, queued: None };
        if let Some(draft) = self.drafts.iter_mut().find(|d| d.id == id) { *draft = updated; } else { self.drafts.push(updated); }
        if self.remote.is_some() { cx.emit(MailboxEvent::WriteRequested(crate::durian::writes::Intent::Save { draft: wire })); }
        cx.notify();
        Some(id)
    }

    pub fn discard_draft(&mut self, id: usize, cx: &mut Context<Self>) {
        if self.remote.is_some() {
            if !self.writes_available { cx.emit(MailboxEvent::WriteProblem("Backend unavailable; draft was not deleted".into())); return }
            let Some(draft) = self.drafts.iter_mut().find(|d| d.id == id) else { return };
            if draft.sending || draft.queued.is_some() || draft.wire.extra.contains_key(crate::durian::writes::PENDING_SEND) {
                cx.emit(MailboxEvent::WriteProblem("Undo the pending send before deleting its recovery draft.".into()));
                return;
            }
            draft.deleting = true;
            cx.emit(MailboxEvent::WriteRequested(crate::durian::writes::Intent::Delete { id: draft.wire.id.clone(), local_id: id }));
            return;
        }
        self.drafts.retain(|d| d.id != id);
        crate::trace::note(&format!("draft {id} discarded ({} drafts)", self.drafts.len()));
        cx.notify();
    }

    pub fn submit_draft(&mut self, id: usize, reply_all: Option<bool>, cx: &mut Context<Self>) -> bool {
        if !self.writes_available { cx.emit(MailboxEvent::WriteProblem("Backend unavailable; content retained, nothing sent".into())); return false }
        let accounts = self.accounts.clone();
        let Some(draft) = self.drafts.iter_mut().find(|d| d.id == id) else { return false };
        if draft.sending || draft.deleting || draft.queued.is_some() { return false }
        let from = crate::durian::parse_address(&draft.wire.from).email;
        if !accounts.iter().any(|a| Some(a.id.as_ref()) == draft.wire.account_id.as_deref() && (a.email.eq_ignore_ascii_case(&from) || a.aliases.iter().any(|alias| alias.eq_ignore_ascii_case(&from)))) {
            cx.emit(MailboxEvent::WriteProblem("Choose a configured sending account or alias.".into()));
            return false;
        }
        // The original full-thread target was captured before opening a reply.
        if !draft.wire.extra.contains_key(crate::durian::writes::PENDING_SEND) {
            match crate::durian::writes::outbox(&draft.wire) {
                Ok(payload) => {
                    let mut frozen = draft.wire.clone(); frozen.extra.insert(crate::durian::writes::PENDING_SEND.into(), serde_json::to_value(payload).unwrap());
                    if !crate::durian::writes::draft_fits(&frozen) {
                        cx.emit(MailboxEvent::WriteProblem("Draft including send recovery exceeds 1 MiB; remove attachments. Nothing queued.".into())); return false;
                    }
                    draft.wire = frozen;
                }
                Err(e) => { cx.emit(MailboxEvent::WriteProblem(e.to_string().into())); return false; }
            }
        }
        draft.sending = true;
        cx.emit(MailboxEvent::WriteRequested(crate::durian::writes::Intent::Send { draft: draft.wire.clone(), local_id: id, reply_all }));
        cx.notify();
        true
    }

    pub fn load_drafts(&mut self, entries: Vec<crate::durian::api::LocalDraft>, cx: &mut Context<Self>) {
        for entry in entries {
            // Unknown foreign draft formats stay untouched on the server.
            let Ok(mut wire) = serde_json::from_value::<crate::durian::api::DraftContent>(entry.draft_json) else { continue };
            wire.id = entry.id;
            if self.drafts.iter().any(|d| d.wire.id == wire.id) { continue }
            let kind = DraftKind::New { account: wire.account_id.clone().unwrap_or_default().into(), from: wire.from.clone().into(), to: wire.to.iter().cloned().map(Into::into).collect(), cc: wire.cc.iter().cloned().map(Into::into).collect(), bcc: wire.bcc.iter().cloned().map(Into::into).collect(), subject: wire.subject.clone().into() };
            let queued = wire.extra.get(crate::durian::writes::QUEUED_SEND).and_then(|v| serde_json::from_value(v.clone()).ok());
            let id = self.next_draft_id; self.next_draft_id += 1;
            self.drafts.push(Draft { id, kind, body: wire.body.clone().into(), saved_at: std::time::Instant::now(), wire, sending: false, deleting: false, queued });
        }
        cx.notify();
    }

    fn request_tags(&mut self, id: SharedString, ops: String, cx: &mut Context<Self>) {
        if !self.writes_available { cx.emit(MailboxEvent::WriteProblem("Backend unavailable; no tags changed".into())); return }
        let Some(thread) = self.threads.iter_mut().find(|t| t.id == id) else { return };
        self.confirmed_tags.entry(id.clone()).or_insert_with(|| thread.clone());
        self.pending_tags.entry(id.clone()).and_modify(|all| { all.push(' '); all.push_str(&ops); }).or_insert_with(|| ops.clone());
        crate::durian::writes::apply_tags(thread, &ops);
        cx.emit(MailboxEvent::WriteRequested(crate::durian::writes::Intent::Tags { thread: id.to_string(), ops }));
        cx.notify();
    }

    pub fn select_thread_id(&mut self, thread_id: &str, cx: &mut Context<Self>) {
        if let Some(ix) = self.threads.iter().position(|t| t.id == thread_id) {
            self.select(ix, cx);
        }
    }

    /// `u`: unread threads become read; read threads get their latest message marked unread.
    pub fn toggle_read_selected(&mut self, cx: &mut Context<Self>) {
        if self.remote.is_some() || self.selection_mode().is_some() {
            if self.selection_mode().is_some() {
                let ids = self.selected_ids(1);
                let any_unread = self.threads.iter().any(|t| ids.contains(&t.id) && t.unread());
                self.tag_selected(if any_unread { "-unread" } else { "+unread" }, 1, cx);
                return;
            }
            if let Some(thread) = self.selected_thread() {
                let ops = if thread.unread() { "-unread" } else { "+unread" };
                let id = thread.id.clone();
                self.unread_by_user = (ops == "+unread").then(|| id.clone());
                self.request_tags(id, ops.into(), cx);
            }
            return;
        }
        if self.refuse(cx) {
            return;
        }
        let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) else { return };
        let unread = thread.unread();
        let last = thread.items.iter().rposition(|i| matches!(i, ThreadItem::Message(_)));
        for (ix, item) in thread.items.iter_mut().enumerate() {
            if let ThreadItem::Message(m) = item {
                m.read = if unread { true } else { Some(ix) != last };
            }
        }
        crate::trace::note(&format!("{} is now {}", thread.id, if unread { "read" } else { "unread" }));
        cx.notify();
    }

    pub fn toggle_star_selected(&mut self, cx: &mut Context<Self>) {
        if self.remote.is_some() || self.selection_mode().is_some() {
            let ids = self.selected_ids(1);
            let all = !ids.is_empty() && self.threads.iter().filter(|t| ids.contains(&t.id)).all(|t| t.starred);
            self.tag_selected(if all { "-flagged" } else { "+flagged" }, 1, cx);
            return;
        }
        if self.refuse(cx) {
            return;
        }
        if let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) {
            thread.starred = !thread.starred;
            cx.notify();
        }
    }
}

pub fn first_name(name: &str) -> &str {
    name.split_whitespace().next().unwrap_or(name)
}

fn unread_query(scope: Option<&str>, base: &str) -> String {
    format!("({}) AND tag:unread", crate::durian::live::compose(scope, Some(base), ""))
}

pub fn folder_unread_query(profile: &Profile, folder: &Folder) -> String {
    unread_query(crate::durian::profiles::scope(profile).as_deref(), &folder.query)
}

pub fn account_inbox_query(account: Option<&str>) -> String {
    unread_query(account.map(crate::durian::translate::account_scope).as_deref(), "tag:inbox")
}

#[cfg(test)]
mod count_tests {
    use super::*;

    fn mailbox(live: bool) -> Mailbox {
        let profiles = crate::durian::profiles::parse(r#"{"profiles":[
            {"name":"Work","accounts":["work"],"folders":[{"name":"Inbox","query":"tag:inbox AND NOT tag:trash"},{"name":"Waiting","query":"tag:waiting"}]},
            {"name":"Home","accounts":["home"],"folders":[{"name":"Inbox","query":"tag:inbox AND NOT tag:trash"}]},
            {"name":"Team","accounts":["work","home"],"folders":[{"name":"Inbox","query":"tag:inbox"}]},
            {"name":"All","accounts":["*"],"folders":[{"name":"Inbox","query":"tag:inbox"}]}]}"#).unwrap();
        let accounts = ["work", "home"].map(|id| Account { id: id.into(), name: id.into(), email: format!("me@{id}.test").into(), aliases: Vec::new(), color: gpui::black() });
        let threads = ["work", "home"].into_iter().map(|id| {
            let row = serde_json::from_str(&format!(r#"{{"thread_id":"{id}","tags":"inbox,unread,waiting"}}"#)).unwrap();
            Thread { account: id.into(), ..crate::durian::summary(&row, 0) }
        }).collect();
        let data = MailData { me: Contact { name: "Me".into(), email: "me@work.test".into(), avatar: None }, accounts: accounts.to_vec(), tags: vec![Tag { name: "waiting".into(), color: gpui::black() }], views: Vec::new(), threads };
        let mut mailbox = if live { Mailbox::remote(data, crate::durian::live::initial_remote()) } else { Mailbox::new(data) };
        mailbox.profiles = profiles;
        mailbox.profile = Some(0);
        mailbox.folder = Some(0);
        mailbox
    }

    #[test]
    fn counts_are_scoped_by_query_not_folder_name_search_or_page() {
        let mut m = mailbox(true);
        // Independent expected scopes, with unequal counts beyond the first 200 rows.
        let work = "((path:work/**) (tag:inbox AND NOT tag:trash)) AND tag:unread";
        let home = "((path:home/**) (tag:inbox AND NOT tag:trash)) AND tag:unread";
        let team = "(((path:work/** OR path:home/**)) (tag:inbox)) AND tag:unread";
        let all = "(tag:inbox) AND tag:unread";
        let queries = m.count_queries();
        for query in [work, home, team, all, "((path:work/) (tag:inbox)) AND tag:unread", "((path:home/) (tag:inbox)) AND tag:unread"] {
            assert!(queries.iter().any(|q| q == query), "missing {query}");
        }
        assert_eq!(queries.iter().filter(|q| q.as_str() == all).count(), 1, "All/profile/account queries deduplicate exactly");
        m.update_counts([(work.into(), Some(501)), (home.into(), Some(7)), (team.into(), Some(943)), (all.into(), Some(1026))]);
        let expected = [501, 7, 943, 1026];
        for (profile, count) in m.profiles.iter().zip(expected) {
            assert_eq!(m.count_for(&folder_unread_query(profile, &profile.folders[0])), Some(count));
        }
        m.query_text = "from:guest invoice".into();
        m.query = crate::query::Query::parse(&m.query_text);
        let remote = m.remote.as_mut().unwrap();
        remote.server_query = "tag:inbox from:guest invoice".into();
        remote.refine = crate::query::Query::parse("missing");
        remote.limit = 400;
        m.folder = Some(1);
        m.threads.truncate(1);
        assert_eq!(m.count_queries(), queries, "no new count queries for typing, local refinement, folder navigation or paging");
        assert_eq!(m.count_for(work), Some(501), "a narrowed/partial page cannot replace the authoritative badge");
        m.profile = Some(1);
        assert_eq!(m.count_for(&folder_unread_query(m.active_profile().unwrap(), m.active_folder().unwrap_or(&m.profiles[1].folders[0]))), Some(7));
    }

    #[test]
    fn failed_counts_preserve_known_values_and_unknown_is_not_zero() {
        let mut m = mailbox(true);
        let work = folder_unread_query(&m.profiles[0], &m.profiles[0].folders[0]);
        let home = folder_unread_query(&m.profiles[1], &m.profiles[1].folders[0]);
        let waiting = folder_unread_query(&m.profiles[0], &m.profiles[0].folders[1]);
        assert_eq!(m.count_for(&work), None);
        m.update_counts([(work.clone(), Some(501)), (waiting.clone(), Some(19))]);
        m.update_counts([(work.clone(), None), (home.clone(), None), (waiting.clone(), Some(0))]);
        assert_eq!(m.count_for(&work), Some(501));
        assert_eq!(m.count_for(&home), None, "a first failure must not manufacture a zero");
        assert_eq!(m.count_for(&waiting), Some(0));
        m.profiles[0].folders[0].query = "tag:sent".into();
        assert_eq!(m.count_for(&folder_unread_query(&m.profiles[0], &m.profiles[0].folders[0])), None, "a changed query cannot reuse an index/name-keyed count");
    }

    #[test]
    fn demo_counts_still_use_complete_local_threads() {
        let mut m = mailbox(false);
        assert!(m.count_queries().is_empty());
        assert_eq!((m.unread_count(), m.account_unread("work"), m.account_unread("home")), (2, 1, 1));
        m.update_counts([(account_inbox_query(None), Some(900))]);
        m.account_filter = Some("home".into());
        assert_eq!(m.unread_count(), 1);
        let (total, tags) = m.unread_counts();
        assert_eq!((total, tags.get("inbox").copied(), tags.get("waiting").copied()), (1, Some(1), Some(1)));
    }

    #[test]
    fn replacement_html_with_equal_length_gets_a_new_render_identity() {
        let a: crate::durian::api::ThreadContent = serde_json::from_str(r#"{"thread_id":"global","subject":"x","messages":[{"id":"local:7","from":"a@test.invalid","html":"<p>first</p>"}]}"#).unwrap();
        let mut b = a.clone(); b.messages[0].html = Some("<p>other</p>".into());
        assert_eq!(a.messages[0].html.as_ref().unwrap().len(), b.messages[0].html.as_ref().unwrap().len());
        let first = crate::durian::thread(&a, 0, Default::default()).unwrap();
        let other = crate::durian::thread(&b, 0, Default::default()).unwrap();
        assert_ne!(first.html_jobs()[0].key, other.html_jobs()[0].key);
        let mut tagged = first.clone(); tagged.tags.push("flagged".into());
        assert_eq!(first.html_jobs()[0].key, tagged.html_jobs()[0].key);
    }
}
