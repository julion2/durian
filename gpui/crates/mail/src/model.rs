//! Mail domain model. Tag-based: a thread has no folder, only tags. "Inbox" and "archive" are
//! just the presence/absence of a tag, which keeps the app and the CLI on the same rule engine.

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
/// arrive or the thread is refetched; the item index for local data), plus the HTML's length
/// so a replaced body renders again.
pub fn message_key(thread: &Thread, item_ix: usize) -> SharedString {
    match thread.items.get(item_ix) {
        Some(ThreadItem::Message(m)) if !m.id.is_empty() => format!("{}#{}#{}", thread.id, m.id, m.html.as_ref().map_or(0, |h| h.len())).into(),
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
}

impl EventEmitter<MailboxEvent> for Mailbox {}

impl Mailbox {
    pub fn new(data: MailData) -> Self {
        let MailData { me, accounts, tags, views, threads } = data;
        let selected = (!threads.is_empty()).then_some(0);
        Self { me, accounts, account_filter: None, query_text: SharedString::default(), query: Default::default(), view_title: "Inbox".into(), tags, views, threads, selected, drafts: Vec::new(), unread_on_open: None, remote: None, profiles: Vec::new(), profile: None, folder: None, next_draft_id: 1 }
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
    pub fn replace_threads(&mut self, threads: Vec<Thread>, cx: &mut Context<Self>) {
        let selected = self.selected_thread().map(|t| t.id.clone());
        self.threads = threads;
        self.selected = selected.and_then(|id| self.threads.iter().position(|t| t.id == id));
        if let Some(remote) = self.remote.as_mut() {
            remote.threads.retain(|id, _| self.threads.iter().any(|t| t.id == *id));
        }
        if self.selected.is_none() {
            self.keep_selection_visible(cx);
        }
        cx.notify();
    }

    /// A full thread arrived for a list row.
    pub fn fill_thread(&mut self, thread: Thread, cx: &mut Context<Self>) {
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

    /// Threads come from a server that this client only reads (for now): no change may be
    /// made, not even locally – the list would show a state the server doesn't have.
    pub fn read_only(&self) -> bool {
        self.remote.is_some()
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
                self.folder = self.folder.filter(|f| *f < self.profiles[ix].folders.len()).or(Some(0));
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
    }

    pub fn set_account_filter(&mut self, filter: Option<SharedString>, cx: &mut Context<Self>) {
        crate::trace::note(&format!("account filter: {filter:?}"));
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
        let read_only = self.read_only();
        if let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) {
            let mut unread = Vec::new();
            for (ix, item) in thread.items.iter_mut().enumerate() {
                if let ThreadItem::Message(m) = item {
                    if !m.read {
                        unread.push(ix);
                    }
                    // read-only: the unread ones still open expanded, they just stay unread
                    m.read |= !read_only;
                }
            }
            // keep the first snapshot of this thread; a second call (right-click) finds nothing new
            if self.unread_on_open.as_ref().is_none_or(|(id, _)| *id != thread.id) || !unread.is_empty() {
                self.unread_on_open = Some((thread.id.clone(), unread));
            }
            cx.notify();
        }
    }

    /// "Archive" = drop the thread from the inbox view (in the real app: remove `tag:inbox`).
    pub fn archive_selected(&mut self, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        let Some(ix) = self.selected else { return };
        let removed = self.threads.remove(ix);
        crate::trace::note(&format!("done: {}", removed.id));
        // next visible thread below, else the one above
        let visible: Vec<usize> = self.visible().collect();
        self.selected = visible.iter().find(|&&v| v >= ix).or(visible.last()).copied();
        cx.emit(MailboxEvent::SelectionChanged);
        cx.notify();
    }

    pub fn toggle_tag_selected(&mut self, name: &SharedString, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        if let Some(thread) = self.selected.and_then(|ix| self.threads.get_mut(ix)) {
            if let Some(pos) = thread.tags.iter().position(|t| t == name) {
                thread.tags.remove(pos);
            } else {
                thread.tags.push(name.clone());
            }
            crate::trace::note(&format!("tags of {}: {:?}", thread.id, thread.tags));
            cx.notify();
        }
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

    /// Autosave for inline replies: empty text deletes the draft.
    pub fn save_reply_draft(&mut self, thread_id: SharedString, all: bool, body: SharedString, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        let existing = self.drafts.iter().position(|d| matches!(&d.kind, DraftKind::Reply { thread_id: t, .. } if *t == thread_id));
        if body.trim().is_empty() {
            if let Some(ix) = existing {
                self.drafts.remove(ix);
                cx.notify();
            }
            return;
        }
        crate::trace::note(&format!("reply draft on {thread_id}: {body:?}"));
        let kind = DraftKind::Reply { thread_id, all };
        match existing {
            Some(ix) => {
                let draft = &mut self.drafts[ix];
                (draft.kind, draft.body, draft.saved_at) = (kind, body, std::time::Instant::now());
            }
            None => {
                let id = self.next_draft_id;
                self.next_draft_id += 1;
                self.drafts.push(Draft { id, kind, body, saved_at: std::time::Instant::now() });
            }
        }
        cx.notify();
    }

    /// Saves (or updates, when `id` is given) a new-message draft and returns its id (None:
    /// refused, read-only).
    pub fn save_new_draft(&mut self, id: Option<usize>, kind: DraftKind, body: SharedString, cx: &mut Context<Self>) -> Option<usize> {
        if self.refuse(cx) {
            return None;
        }
        let id = match id.and_then(|id| self.drafts.iter_mut().find(|d| d.id == id)) {
            Some(draft) => {
                (draft.kind, draft.body, draft.saved_at) = (kind, body, std::time::Instant::now());
                draft.id
            }
            None => {
                let id = self.next_draft_id;
                self.next_draft_id += 1;
                self.drafts.push(Draft { id, kind, body, saved_at: std::time::Instant::now() });
                id
            }
        };
        crate::trace::note(&format!("draft {id} saved ({} drafts)", self.drafts.len()));
        cx.notify();
        Some(id)
    }

    pub fn discard_draft(&mut self, id: usize, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        self.drafts.retain(|d| d.id != id);
        crate::trace::note(&format!("draft {id} discarded ({} drafts)", self.drafts.len()));
        cx.notify();
    }

    pub fn select_thread_id(&mut self, thread_id: &str, cx: &mut Context<Self>) {
        if let Some(ix) = self.threads.iter().position(|t| t.id == thread_id) {
            self.select(ix, cx);
        }
    }

    /// `u`: unread threads become read; read threads get their latest message marked unread.
    pub fn toggle_read_selected(&mut self, cx: &mut Context<Self>) {
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
