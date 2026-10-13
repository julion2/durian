//! Mail client views on GPUI: a keyboard-driven, tag-based mail UI. The app hands in
//! [`model::MailData`] and opens a [`MailWorkspace`]; the rest (commands, search, composer,
//! HTML rendering) lives here. Call [`bind_keys`] once at startup, after `ui::init`.

pub mod attachments;
pub mod app_menu;
pub mod avatars;
pub mod commands;
mod calendar_menus;
pub mod context_menus;
pub mod compose;
pub mod durian;
mod drafts_view;
mod event_compose;
mod help;
mod invite;
pub mod keymaps;
mod keymap_actions;
pub mod html_doc;
pub mod html_render;
mod mime;
pub mod query;
mod quote;
pub mod model;
mod sidebar;
mod storage;
mod thread_list;
mod thread_view;
#[cfg(target_os = "macos")]
pub mod webview;

pub use storage::{clear_session, default_cache_root, init_session, session_dir};

use gpui::{
    AnimationExt, AnyView, App, Context, KeyDownEvent, SharedString, StyleRefinement, Subscription, Entity, FocusHandle, Focusable, IntoElement, Render, Window, actions, div, prelude::*,
    px,
};

use ui::{SIDEBAR_WIDTH, TITLEBAR_HEIGHT};
use ui::theme::{ActiveTheme, Appearance, typography::*};
use ui::{Avatar, ContextMenu, ContextMenuEvent, Kbd, Picker, PickerDelegate, PickerEvent, StyledExt, Toast};
use drafts_view::{DraftsView, DraftsViewEvent};
use model::DraftKind;
use sidebar::SidebarEvent;
use html_render::HtmlRenderer;
use model::{Contact, Mailbox, MailboxEvent};
use sidebar::MailSidebar;
use thread_list::{LIST_WIDTH, Layout, ThreadList, ThreadListEvent};
use compose::{ComposeDialog, ComposeEvent, ComposeSeed};
use thread_view::{ThreadView, ThreadViewEvent};

actions!(mail, [SelectNext, SelectPrev, Archive, ToggleStar, ToggleFocusMode, ExitFocusMode, ToggleExpandAll, OpenCommandMenu, OpenTagPicker, Reply, ReplyAll, NewMessage, Forward, GoToDrafts, GoToInbox, PreviewAttachment, ToggleSidebar, ShowAllAccounts, ShowAccount1, ShowAccount2, ShowAccount3, ShowAccount4, ShowAccount5, ShowAccount6, ShowAccount7, ShowAccount8, ShowAccount9, ShowAccount10, ToggleDarkMode, UseSystemAppearance, ShowContextMenu, ToggleRead, FocusSearch, ToggleShortcutHelp, ToggleListLayout, OpenThread, GoToCalendar, NewEvent, ComposeHere, EventFromThread, CopySelection, SelectAllText, Retry]);
actions!(keymaps, [ReloadKeymaps]);

/// Menu requested by a view (see `context_menus::open`), picked up by the workspace.
pub struct PendingMenu(pub Option<(gpui::Point<gpui::Pixels>, Vec<ui::MenuItem>)>);

impl gpui::Global for PendingMenu {}


/// `MAIL_TRACE=1 cargo run -- mail` logs input → render → paint latency per action.
pub mod trace {
    use std::cell::Cell;
    use std::time::Instant;

    thread_local!(static INPUT_AT: Cell<Option<(Instant, &'static str)>> = const { Cell::new(None) });

    pub fn enabled() -> bool {
        std::env::var_os("MAIL_TRACE").is_some()
    }

    pub fn input(name: &'static str) {
        if enabled() {
            // wall clock so an external event injector can compute OS → app dispatch latency
            let wall = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap_or_default();
            eprintln!("[trace] {name:>6} dispatched at {:.6}", wall.as_secs_f64());
            if let Some(age) = os_event_age() {
                eprintln!("[trace] {name:>6} → OS event age  {:>6.2}ms", age * 1000.);
            }
            INPUT_AT.set(Some((Instant::now(), name)));
        }
    }

    /// Seconds between the OS stamping the current NSEvent and now.
    #[cfg(target_os = "macos")]
    fn os_event_age() -> Option<f64> {
        let mtm = objc2::MainThreadMarker::new()?;
        let event = objc2_app_kit::NSApplication::sharedApplication(mtm).currentEvent()?;
        Some(objc2_foundation::NSProcessInfo::processInfo().systemUptime() - event.timestamp())
    }

    #[cfg(not(target_os = "macos"))]
    fn os_event_age() -> Option<f64> {
        None
    }

    pub fn mark(stage: &str) {
        if let Some((at, name)) = INPUT_AT.get() {
            eprintln!("[trace] {name:>6} → {stage:<14} {:>7.2?}", at.elapsed());
        }
    }

    pub fn note(message: &str) {
        if enabled() {
            eprintln!("[trace] {:>7.0?} {message}", started().elapsed());
        }
    }

    /// When the process started (first call; the app calls it first thing).
    pub fn started() -> Instant {
        static START: std::sync::OnceLock<Instant> = std::sync::OnceLock::new();
        *START.get_or_init(Instant::now)
    }

    /// Log a render that took longer than a frame: `what` names it, never its content.
    pub fn slow(what: &str, at: Instant) {
        let took = at.elapsed();
        if enabled() && took.as_secs_f64() > 1. / 120. {
            note(&format!("slow {what}: {took:.1?}"));
        }
    }

    pub fn finish() {
        mark("painted");
        INPUT_AT.set(None);
    }
}

/// Avatar for a contact: pre-downloaded gravatar or company logo, else initials.
static CACHE_ROOT: std::sync::OnceLock<std::path::PathBuf> = std::sync::OnceLock::new();

/// Persistent configuration and demo caches. Decrypted live mail uses `session_dir` instead.
pub fn set_cache_root(root: std::path::PathBuf) {
    let _ = CACHE_ROOT.set(root);
}

pub fn cache_dir(sub: &str) -> std::path::PathBuf {
    CACHE_ROOT.get_or_init(default_cache_root).join(sub)
}

/// Whether HTML mail is shown as pictures from the `mailrender` helper – everywhere but on
/// macOS with live views (`MAIL_HTML=snapshot` forces pictures there too).
pub fn uses_snapshots() -> bool {
    #[cfg(target_os = "macos")]
    return !webview::available();
    #[cfg(not(target_os = "macos"))]
    return true;
}

pub fn contact_avatar(contact: &Contact) -> Avatar {
    let avatar = Avatar::new(contact.name.clone());
    match &contact.avatar {
        Some(a) if a.logo => avatar.logo(a.path.clone()),
        Some(a) => avatar.image(a.path.clone()),
        // a live mailbox: photo or logo from the web, once it's arrived
        None => match avatars::lookup(&contact.email) {
            Some((image, true)) => avatar.logo(image),
            Some((image, false)) => avatar.image(image),
            None => avatar,
        },
    }
}

pub fn bind_keys(cx: &mut App) {
    if !cx.has_global::<keymaps::Keymaps>() { cx.set_global(keymaps::Keymaps::default()); }
    commands::bind_keys(cx);
    compose::bind_keys(cx);
    thread_list::bind_search_keys(cx);
    help::bind_keys(cx);
    cx.global::<keymaps::Keymaps>().clone().bind(cx);
}

pub struct MailWorkspace {
    mailbox: Entity<Mailbox>,
    sidebar: Entity<MailSidebar>,
    list: Entity<ThreadList>,
    thread: Entity<ThreadView>,
    focus_handle: FocusHandle,
    /// Thread takes the whole panel (Superhuman-style); the list stays mounted for instant return.
    focus_mode: bool,
    /// `Split`: list + thread side by side. `List`: the list alone; the thread opens full width.
    layout: Layout,
    renderer: Entity<HtmlRenderer>,
    key_engine: keymaps::Engine,
    key_timer: Option<gpui::Task<()>>,
    key_load: Option<gpui::Task<()>>,
    key_focus: Option<FocusHandle>,
    keymaps_ready: bool,
    /// last direction of travel, so prefetching looks ahead where the user is going
    direction: isize,
    /// open ⌘K / tag picker; owns the keyboard while present
    picker: Option<(AnyView, Subscription)>,
    tag_picker: bool,
    compose: Option<(Entity<ComposeDialog>, Subscription)>,
    event_compose: Option<(Entity<event_compose::EventComposer>, Subscription)>,
    confirm_delete: Option<Vec<SharedString>>,
    confirm_focus: FocusHandle,
    context_menu: Option<(Entity<ContextMenu>, Subscription)>,
    help: Option<(Entity<help::ShortcutHelp>, Subscription)>,
    drafts: Entity<DraftsView>,
    schedule: Entity<calendar::model::Schedule>,
    calendar: Entity<calendar::CalendarView>,
    mode: Mode,
    sidebar_open: bool,
    /// pane widths, dragged at their right edge and remembered across launches
    widths: PaneWidths,
    /// what the panes got in the last frame (narrow windows squeeze the remembered widths)
    shown: PaneWidths,
    resizing: Option<Resize>,
    toast: Option<(SharedString, ToastAction)>,
    _toast_timer: Option<gpui::Task<()>>,
    /// the server the threads come from (None: the data handed to `new`)
    live: Option<Entity<durian::Live>>,
    calendar_writes: Option<Entity<durian::calendar_write::CalendarWrites>>,
    _subscriptions: Vec<Subscription>,
}

#[derive(Clone)]
enum ToastAction {
    /// put back what was just deleted
    RestoreEvents(Vec<calendar::model::Event>),
    /// jump to the calendar, where the event is already selected
    ShowEvent(SharedString),
    OpenDraft(usize),
    UndoSend(usize),
    Reveal(std::path::PathBuf),
    /// just the message
    None,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Pane {
    Sidebar,
    List,
}

#[derive(Clone, Copy, Debug, serde::Serialize, serde::Deserialize)]
struct PaneWidths {
    sidebar: f32,
    list: f32,
}

impl PaneWidths {
    const DEFAULT: PaneWidths = PaneWidths { sidebar: SIDEBAR_WIDTH, list: LIST_WIDTH };

    fn path() -> std::path::PathBuf {
        cache_dir("layout.json")
    }

    fn load() -> Self {
        std::fs::read(Self::path()).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or(Self::DEFAULT).clamped()
    }

    fn save(&self) {
        if let Ok(json) = serde_json::to_vec(self) {
            let _ = std::fs::create_dir_all(Self::path().parent().unwrap_or(std::path::Path::new(".")));
            let _ = std::fs::write(Self::path(), json);
        }
    }

    fn clamped(self) -> Self {
        Self { sidebar: self.sidebar.clamp(180., 360.), list: self.list.clamp(LIST_MIN, 680.) }
    }

    fn get(&self, pane: Pane) -> f32 {
        match pane {
            Pane::Sidebar => self.sidebar,
            Pane::List => self.list,
        }
    }

    fn set(&mut self, pane: Pane, width: f32) {
        match pane {
            Pane::Sidebar => self.sidebar = width,
            Pane::List => self.list = width,
        }
        *self = self.clamped();
    }
}

/// Narrowest the thread list gets (its search field needs the room).
const LIST_MIN: f32 = 320.;
/// The thread keeps at least this much; sidebar and list give way in narrow windows.
const THREAD_MIN: f32 = 380.;

/// A pane edge being dragged: where the mouse went down and the width it had then.
#[derive(Clone, Copy)]
struct Resize {
    pane: Pane,
    start_x: gpui::Pixels,
    start_width: f32,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Inbox,
    Drafts,
    Calendar,
}

/// How far HTML prefetch looks along / against the direction of travel. Ahead covers `5j`.
const PREFETCH_AHEAD: isize = 6;
const PREFETCH_BEHIND: isize = 2;

impl MailWorkspace {
    pub fn new(data: model::MailData, calendar_data: calendar::model::CalendarData, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let renderer = cx.new(HtmlRenderer::new);
        Self::build(Mailbox::new(data), calendar_data, renderer, window, cx)
    }

    /// Mail from a durian server: the window opens at once, the lists fill in as
    /// the server answers.
    pub fn live(connect: durian::live::Preconnect, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let empty = model::MailData {
            me: Contact { name: "Me".into(), email: SharedString::default(), avatar: None },
            accounts: Vec::new(),
            tags: Vec::new(),
            views: Vec::new(),
            threads: Vec::new(),
        };
        let calendar = calendar::model::CalendarData { calendars: Vec::new(), events: Vec::new(), me: Vec::new() };
        // rendered HTML only for this session: it's decrypted mail
        let renderer = cx.new(|cx| HtmlRenderer::in_dir(session_dir("html"), cx));
        avatars::init(cx);
        cx.on_app_quit(|_, _| async { clear_session() }).detach();
        let mut this = Self::build(Mailbox::remote(empty, durian::live::initial_remote()), calendar, renderer, window, cx);
        this.schedule.update(cx, |s, _| {
            // events come in windows, nothing known before the first
            s.loaded = Some(Vec::new());
        });
        let (mailbox, schedule) = (this.mailbox.clone(), this.schedule.clone());
        let live = cx.new(|cx| durian::Live::new(connect, mailbox, schedule, cx));
        let writes = cx.new(|cx| durian::calendar_write::CalendarWrites::new(live.clone(), this.schedule.clone(), cx));
        cx.observe(&writes, |_, _, cx| cx.notify()).detach();
        cx.subscribe(&writes, |this, _, event, cx| match event {
            durian::calendar_write::WriteEvent::Notice(message) => this.show_toast(message.clone(), ToastAction::None, cx),
        }).detach();
        this.calendar_writes = Some(writes);
        cx.observe(&live, |_, _, cx| cx.notify()).detach();
        cx.subscribe_in(&live, window, |this, _, event, window, cx| match event {
            durian::live::LiveEvent::Problem(message) => this.show_toast(message.clone(), ToastAction::None, cx),
            durian::live::LiveEvent::Downloaded(path) => this.show_toast("Saved to Downloads".into(), ToastAction::Reveal(path.clone()), cx),
            durian::live::LiveEvent::Queued { draft_id, item } => {
                this.show_queued(*draft_id, item.send_after, cx);
            }
            durian::live::LiveEvent::Undone(id) => this.show_toast("Delivery cancelled; draft restored".into(), ToastAction::OpenDraft(*id), cx),
            durian::live::LiveEvent::Delivery { item_id, state } => {
                let id = this.mailbox.read(cx).drafts.iter().find(|d| d.queued.as_ref().is_some_and(|q| q.id == *item_id)).map(|d| d.id);
                this.show_toast(state.message().into(), id.map(ToastAction::OpenDraft).unwrap_or(ToastAction::None), cx);
            }
            durian::live::LiveEvent::ForwardReady(id) => this.open_draft(*id, window, cx),
        })
        .detach();
        // attachments live on the server: opening one fetches it first
        let weak = live.downgrade();
        cx.set_global(attachments::Fetch(std::rc::Rc::new(move |attachment, use_, cx| {
            if let Some(live) = weak.upgrade() {
                live.update(cx, |live, cx| live.fetch_attachment(attachment, use_, cx));
            }
        })));
        this.live = Some(live);
        this.keymaps_ready = false;
        this.reload_keymaps(false, cx);
        this
    }

    pub fn reconnect(&mut self, preconnect: durian::live::Preconnect, _window: &mut Window, cx: &mut Context<Self>) {
        if let Some(live) = &self.live { live.update(cx, |live, cx| live.reconnect(preconnect, cx)); }
        self.reload_keymaps(false, cx);
        if let Some((dialog, _)) = &self.compose {
            let client = self.live.as_ref().map(|live| live.read(cx).client());
            dialog.update(cx, |dialog, cx| dialog.set_contact_client(client, cx));
        }
        cx.notify();
    }

    pub fn backend_unavailable(&mut self, message: SharedString, cx: &mut Context<Self>) {
        if let Some(live) = &self.live { live.update(cx, |live, cx| live.backend_unavailable(message, cx)); }
        if let Some((dialog, _)) = &self.compose {
            dialog.update(cx, |dialog, cx| dialog.set_contact_client(None, cx));
        }
        cx.notify();
    }

    fn build(mailbox: Mailbox, calendar_data: calendar::model::CalendarData, renderer: Entity<HtmlRenderer>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mailbox = cx.new(|_| mailbox);
        let today = chrono::Local::now().date_naive();
        let schedule = cx.new(|_| calendar::model::Schedule::new(calendar_data, today));
        let calendar = cx.new(|cx| calendar::CalendarView::new(schedule.clone(), cx));
        // a slot dragged out in the grid opens the event composer with that time; right-clicks
        // get their menu here, where composer, mail and toasts live
        cx.subscribe_in(&calendar, window, |this, _, event, window, cx| match event {
            calendar::CalendarViewEvent::ContextMenu { target, position } => {
                let items = calendar_menus::build(target, &this.schedule, cx.entity().downgrade(), cx);
                if !items.is_empty() {
                    context_menus::open(*position, items, window, cx);
                }
            }
            calendar::CalendarViewEvent::CreateRequested { start, end, all_day } => {
                this.open_event_compose(event_compose::EventSeed::slot(*start, *end, *all_day), window, cx)
            }
        })
        .detach();
        cx.subscribe_in(&schedule, window, |this, _, event, window, cx| {
            match event {
                calendar::model::ScheduleEvent::ReadOnly => this.refused(cx),
                calendar::model::ScheduleEvent::EditReady(event) => {
                    let me = this.schedule.read(cx).me.clone();
                    this.open_event_compose(event_compose::EventSeed::edit(event.clone(), &me), window, cx);
                }
                _ => {}
            }
        })
        .detach();
        cx.subscribe(&mailbox, |this, mailbox, event, cx| match event {
            MailboxEvent::SelectionChanged => {
                // reading = viewing: a selected thread is read once it's on screen
                if this.thread_visible() {
                    mailbox.update(cx, |m, cx| m.mark_selected_read(cx));
                }
                this.prefetch(cx);
            }
            // the open thread arrived in full: read it, render its HTML
            MailboxEvent::ThreadLoaded(id) => {
                if mailbox.read(cx).selected_thread().is_some_and(|t| t.id == *id) {
                    if this.thread_visible() {
                        mailbox.update(cx, |m, cx| m.mark_selected_read(cx));
                    }
                    this.prefetch(cx);
                }
            }
            MailboxEvent::ReadOnly => this.refused(cx),
            MailboxEvent::WriteProblem(message) => this.show_toast(message.clone(), ToastAction::None, cx),
            MailboxEvent::QueryChanged | MailboxEvent::MoreWanted | MailboxEvent::WriteRequested(_) | MailboxEvent::DraftSaved(_) | MailboxEvent::DraftDeleted(_) | MailboxEvent::SendFinished { .. } => {}
        })
        .detach();
        trace::note(&format!("appearance at start: {:?} → dark theme: {}", cx.window_appearance(), cx.theme().is_dark()));
        // follow macOS light/dark unless the user pinned one
        cx.observe_window_appearance(window, |_, _, cx| {
            if cx.try_global::<Appearance>().is_none_or(|a| *a == Appearance::System) {
                ui::theme::apply_appearance(cx);
            }
        })
        .detach();
        let focus_handle = cx.focus_handle();
        focus_handle.focus(window, cx);
        let thread = cx.new(|cx| ThreadView::new(mailbox.clone(), renderer.clone(), schedule.clone(), cx));
        let thread_subscription = cx.subscribe_in(&thread, window, |this, _, event, window, cx| match event {
            ThreadViewEvent::ReleaseFocus => this.focus_area(window, cx),
            ThreadViewEvent::OpenEvent(id, day) => {
                let (id, day) = (id.clone(), *day);
                this.schedule.update(cx, |s, cx| {
                    // the calendar's day, if it has the event (it may have moved); else the mail's
                    s.anchor = s.event(&id).map_or(day, |e| e.start.date());
                    s.select(Some(id), cx);
                });
                this.set_mode(Mode::Calendar, window, cx);
            }
            ThreadViewEvent::PopOut(seed) => this.open_compose(seed.clone(), window, cx),
            ThreadViewEvent::Downloaded(path) => this.show_toast("Saved to Downloads".into(), ToastAction::Reveal(path.clone()), cx),
        });
        let sidebar = cx.new(|cx| MailSidebar::new(mailbox.clone(), cx));
        // the sidebar is a cached view: repaint it when the window (de)activates, its active
        // item dims in the background
        cx.observe_window_activation(window, |this, _, cx| {
            this.clear_key_sequence();
            this.sidebar.update(cx, |_, cx| cx.notify());
        }).detach();
        cx.observe_global::<keymaps::Keymaps>(|this, cx| {
            this.clear_key_sequence();
            this.list.update(cx, |_, cx| cx.notify());
            this.thread.update(cx, |_, cx| cx.notify());
            this.sidebar.update(cx, |_, cx| cx.notify());
            if let Some((help, _)) = &this.help { help.update(cx, |_, cx| cx.notify()); }
            cx.notify();
        }).detach();
        let sidebar_subscription = cx.subscribe_in(&sidebar, window, |this, _, event, window, cx| match event {
            SidebarEvent::Navigate(key) => this.navigate(key.clone(), window, cx),
            SidebarEvent::ToggleSidebar => {
                this.sidebar_open = !this.sidebar_open;
                cx.notify();
            }
        });
        let drafts = cx.new(|cx| DraftsView::new(mailbox.clone(), cx));
        let drafts_subscription = cx.subscribe_in(&drafts, window, |this, _, event, window, cx| match event {
            DraftsViewEvent::Open(id) => this.open_draft(*id, window, cx),
        });
        let list = cx.new(|cx| ThreadList::new(mailbox.clone(), cx));
        let list_subscription = cx.subscribe_in(&list, window, |this, _, ThreadListEvent::ReleaseFocus, window, cx| {
            this.focus_area(window, cx);
        });
        let mut this = Self {
            sidebar,
            list,
            thread,
            compose: None,
            event_compose: None,
            confirm_delete: None,
            confirm_focus: cx.focus_handle(),
            context_menu: None,
            help: None,
            drafts,
            schedule,
            calendar,
            mode: Mode::Inbox,
            sidebar_open: true,
            widths: PaneWidths::load(),
            shown: PaneWidths::DEFAULT,
            resizing: None,
            toast: None,
            _toast_timer: None,
            live: None,
            calendar_writes: None,
            _subscriptions: vec![thread_subscription, sidebar_subscription, drafts_subscription, list_subscription],
            mailbox,
            focus_handle,
            focus_mode: false,
            layout: Layout::Split,
            renderer,
            key_engine: keymaps::Engine::default(),
            key_timer: None,
            key_load: None,
            key_focus: None,
            keymaps_ready: true,
            direction: 1,
            picker: None,
            tag_picker: false,
        };
        this.prefetch(cx);
        this
    }

    /// In the `List` layout the thread is only on screen once opened.
    fn thread_visible(&self) -> bool {
        self.layout == Layout::Split || self.focus_mode
    }

    fn set_focus_mode(&mut self, on: bool, cx: &mut Context<Self>) {
        self.clear_key_sequence();
        if on && !self.focus_mode {
            self.mailbox.update(cx, |m, cx| m.clear_selection(cx));
            self.thread.update(cx, |t, cx| t.open_latest(cx));
        }
        self.focus_mode = on;
        if self.thread_visible() {
            self.mailbox.update(cx, |m, cx| m.mark_selected_read(cx));
        }
        cx.notify();
    }

    /// Wish list for the HTML renderer: open thread first, then along the direction of travel,
    /// then a little behind. Rebuilt on every selection change; stale jobs fall off.
    fn prefetch(&mut self, cx: &mut Context<Self>) {
        // live views render HTML themselves: no pictures to prepare
        if !uses_snapshots() {
            return;
        }
        let mailbox = self.mailbox.read(cx);
        let Some(selected) = mailbox.selected else { return };
        let len = mailbox.threads.len() as isize;
        let (sel, dir) = (selected as isize, self.direction);
        let order = std::iter::once(sel)
            .chain((1..=PREFETCH_AHEAD).map(|i| sel + dir * i))
            .chain((1..=PREFETCH_BEHIND).map(|i| sel - dir * i))
            .filter(|ix| (0..len).contains(ix));
        // the open thread still loading: hold the neighbors back, its HTML goes first
        let jobs: Vec<_> = if mailbox.threads[selected].partial { Vec::new() } else { order.flat_map(|ix| mailbox.threads[ix as usize].html_jobs()).collect() };
        let current: Vec<_> = mailbox.threads[selected].html_jobs().into_iter().map(|j| j.key).collect();
        self.renderer.update(cx, |r, cx| {
            r.prioritize(jobs, current.len(), cx);
            for key in current {
                let state = match r.state(&key) {
                    Some(html_render::RenderState::Ready(_)) => "ready",
                    Some(html_render::RenderState::Failed(_)) => "failed",
                    _ => "pending",
                };
                trace::note(&format!("html {key} on open: {state}"));
            }
        });
    }

    fn open_picker<D: PickerDelegate>(&mut self, delegate: D, width: f32, window: &mut Window, cx: &mut Context<Self>) {
        let picker = cx.new(|cx| Picker::new(delegate, width, window, cx));
        let subscription = cx.subscribe_in(&picker, window, |this, _, event, window, cx| match event {
            PickerEvent::Dismissed => this.close_picker(window, cx),
        });
        self.clear_key_sequence();
        self.tag_picker = false;
        trace::note(&format!("picker open: {}", std::any::type_name::<D>().rsplit("::").next().unwrap_or("?")));
        self.picker = Some((picker.into(), subscription));
        cx.notify();
    }

    fn close_picker(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        trace::note("picker closed");
        self.picker = None;
        self.focus_area(window, cx);
        cx.notify();
    }

    fn open_compose(&mut self, seed: ComposeSeed, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(writes) = &self.calendar_writes { writes.update(cx, |writes, _| writes.cancel_edit()); }
        if self.compose.is_some() || self.event_compose.is_some() {
            self.show_toast("Finish or close the current editor before opening another.".into(), ToastAction::None, cx);
            return;
        }
        let dialog = cx.new(|cx| ComposeDialog::new(self.mailbox.clone(), seed, window, cx));
        let client = self.live.as_ref().map(|live| live.read(cx).client());
        dialog.update(cx, |dialog, cx| dialog.set_contact_client(client, cx));
        let subscription = cx.subscribe_in(&dialog, window, |this, _, event, window, cx| match event {
            ComposeEvent::Close => {
                this.compose = None;
                this.focus_area(window, cx);
                cx.notify();
            }
            ComposeEvent::DraftSaved(id) => this.show_toast("Draft saved".into(), ToastAction::OpenDraft(*id), cx),
            ComposeEvent::UndoSend(id) => {
                if let Some(live) = &this.live { live.update(cx, |live, cx| live.undo_send(*id, cx)); }
            }
        });
        self.compose = Some((dialog, subscription));
        cx.notify();
    }

    /// New event, prefilled with a slot (dragged out in the grid, or the next full hour).
    fn open_event_compose(&mut self, seed: event_compose::EventSeed, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(writes) = &self.calendar_writes { writes.update(cx, |writes, _| writes.cancel_edit()); }
        if self.event_compose.is_some() || self.compose.is_some() {
            self.show_toast("Finish or close the current editor before opening another.".into(), ToastAction::None, cx);
            return;
        }
        let dialog = cx.new(|cx| event_compose::EventComposer::new(self.mailbox.clone(), self.schedule.clone(), seed, window, cx));
        let subscription = cx.subscribe_in(&dialog, window, |this, _, event, window, cx| {
            this.event_compose = None;
            if let event_compose::EventComposeEvent::Saved(id) = event
                && this.mode != Mode::Calendar
            {
                this.show_toast("Event saved locally".into(), ToastAction::ShowEvent(id.clone()), cx);
            }
            this.focus_area(window, cx);
            cx.notify();
        });
        self.event_compose = Some((dialog, subscription));
        cx.notify();
    }

    fn request_delete(&mut self, ids: Vec<SharedString>, window: &mut Window, cx: &mut Context<Self>) {
        if self.live.is_none() {
            return self.remove_events(ids, "Event deleted", cx);
        }
        self.confirm_delete = Some(ids);
        self.confirm_focus.focus(window, cx);
        cx.notify();
    }

    /// Delete / cancel / remove events. Only demo-local deletion offers Undo.
    fn remove_events(&mut self, ids: Vec<SharedString>, message: &str, cx: &mut Context<Self>) {
        let removed = self.schedule.update(cx, |s, cx| s.remove_events(&ids, cx));
        if !removed.is_empty() {
            self.show_toast(message.to_string().into(), ToastAction::RestoreEvents(removed), cx);
        }
    }

    /// A new mail to these people about something (event guests, the organizer).
    fn email_people(&mut self, to: Vec<SharedString>, subject: SharedString, window: &mut Window, cx: &mut Context<Self>) {
        self.open_compose(ComposeSeed { to, subject, ..Default::default() }, window, cx);
    }

    /// The thread whose message carries this invitation.
    fn invitation_thread(&self, event_id: &str, cx: &App) -> Option<SharedString> {
        self.mailbox
            .read(cx)
            .threads
            .iter()
            .find(|t| t.messages().any(|m| m.invite.as_deref() == Some(event_id) || m.invitation.as_ref().is_some_and(|i| i.event.id == event_id)))
            .map(|t| t.id.clone())
    }

    fn show_invitation(&mut self, event_id: &str, window: &mut Window, cx: &mut Context<Self>) {
        let Some(thread) = self.invitation_thread(event_id, cx) else { return };
        self.set_mode(Mode::Inbox, window, cx);
        self.mailbox.update(cx, |m, cx| m.select_thread_id(&thread, cx));
    }

    /// The draggable right edge of a pane: an 8px hit area over the border, a line on hover,
    /// the accent while dragging; double-click restores the default width.
    fn resize_handle(&self, pane: Pane, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let active = self.resizing.is_some_and(|r| r.pane == pane);
        let (line, hover) = (theme.accent, theme.border_chip);
        div().relative().w_0().h_full().flex_none().child(
            div()
                .id(match pane {
                    Pane::Sidebar => "resize-sidebar",
                    Pane::List => "resize-list",
                })
                .group("resize-handle")
                .absolute()
                .left(px(-4.))
                .w(px(8.))
                .top_0()
                .bottom_0()
                .cursor_col_resize()
                .child(
                    div()
                        .absolute()
                        .left(px(3.5))
                        .w(px(1.))
                        .top_0()
                        .bottom_0()
                        .when(active, |l| l.bg(line))
                        .when(!active, |l| l.group_hover("resize-handle", |l| l.bg(hover))),
                )
                .on_mouse_down(gpui::MouseButton::Left, cx.listener(move |this, event: &gpui::MouseDownEvent, _, cx| {
                    cx.stop_propagation();
                    if event.click_count >= 2 {
                        this.widths.set(pane, PaneWidths::DEFAULT.get(pane));
                        this.widths.save();
                    } else {
                        // start from what's on screen, which may be squeezed below the remembered width
                        this.resizing = Some(Resize { pane, start_x: event.position.x, start_width: this.shown.get(pane) });
                    }
                    cx.notify();
                })),
        )
    }

    /// Sidebar destinations are queries over the one mailbox (same language as the CLI).
    /// Another profile: it opens at its first folder, with an empty search.
    fn switch_profile(&mut self, ix: usize, cx: &mut Context<Self>) {
        self.mode = Mode::Inbox;
        self.mailbox.update(cx, |m, cx| m.set_profile(ix, cx));
        let title = self.mailbox.read(cx).view_title.clone();
        self.list.update(cx, |list, cx| list.set_search(SharedString::default(), Some(title), cx));
        self.sidebar.update(cx, |s, cx| s.set_active("folder:0", cx));
        cx.notify();
    }

    fn navigate(&mut self, key: SharedString, window: &mut Window, cx: &mut Context<Self>) {
        // a profile's folder: its query is the list's base, the search starts empty
        if let Some(ix) = key.strip_prefix("folder:").and_then(|n| n.parse::<usize>().ok()) {
            self.mode = Mode::Inbox;
            self.mailbox.update(cx, |m, cx| m.open_folder(ix, cx));
            let title = self.mailbox.read(cx).view_title.clone();
            self.list.update(cx, |list, cx| list.set_search(SharedString::default(), Some(title), cx));
            self.focus_area(window, cx);
            return cx.notify();
        }
        if let Some(ix) = key.strip_prefix("profile:").and_then(|n| n.parse::<usize>().ok()) {
            self.switch_profile(ix, cx);
            return self.focus_area(window, cx);
        }
        if key == "drafts" {
            return self.set_mode(Mode::Drafts, window, cx);
        }
        if key == "calendar" {
            return self.set_mode(Mode::Calendar, window, cx);
        }
        let (query, title): (String, String) = match key.as_ref() {
            "inbox" => (String::new(), "Inbox".into()),
            "starred" => ("tag:flagged".into(), "Starred".into()),
            "snoozed" => ("tag:snoozed".into(), "Snoozed".into()),
            "sent" => ("from:me".into(), "Sent".into()),
            "all" => (String::new(), "All mail".into()),
            other => match other.split_once(':') {
                Some(("tag", name)) => (format!("tag:{name}"), name.into()),
                Some(("view", name)) => {
                    let query = self.mailbox.read(cx).views.iter().find(|v| v.name == name).map(|v| v.query.to_string()).unwrap_or_default();
                    (query, name.into())
                }
                _ => (String::new(), "Inbox".into()),
            },
        };
        self.mode = Mode::Inbox;
        // Sidebar views/tags are destinations, not searches inside the old folder.
        // The profile/account remains the scope; typed search still keeps its folder.
        self.mailbox.update(cx, |m, _| m.folder = None);
        self.list.update(cx, |list, cx| list.apply_query(query.into(), Some(title.into()), cx));
        self.sidebar.update(cx, |s, cx| s.set_active(key, cx));
        self.focus_area(window, cx);
        cx.notify();
    }

    fn set_mode(&mut self, mode: Mode, window: &mut Window, cx: &mut Context<Self>) {
        let key = match mode {
            Mode::Inbox => "inbox",
            Mode::Drafts => "drafts",
            Mode::Calendar => "calendar",
        };
        trace::note(&format!("mode: {key}"));
        self.mode = mode;
        self.sidebar.update(cx, |s, cx| s.set_active(key, cx));
        self.focus_area(window, cx);
        cx.notify();
    }

    /// The area's keyboard home: the calendar view in the calendar (its own context carries the
    /// calendar keys), the workspace root everywhere else. Overlays hand focus back here.
    fn focus_area(&self, window: &mut Window, cx: &mut App) {
        if self.mode == Mode::Calendar {
            self.calendar.read(cx).focus_handle(cx).focus(window, cx);
        } else {
            self.focus_handle.focus(window, cx);
        }
    }

    fn scope(&self) -> commands::Scope {
        if self.mode == Mode::Calendar { commands::Scope::Calendar } else { commands::Scope::Mail }
    }

    /// Reopen a draft: new messages in the composer, replies in their thread.
    fn open_draft(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Some(draft) = self.mailbox.read(cx).drafts.iter().find(|d| d.id == id).cloned() else { return };
        match draft.kind {
            DraftKind::New { account, from, to, cc, bcc: _, subject } => {
                let seed = ComposeSeed { draft_id: Some(id), account: Some(account), from: Some(from), to, cc, subject, body: draft.body };
                self.open_compose(seed, window, cx);
            }
            DraftKind::Reply { thread_id, all } => {
                self.set_mode(Mode::Inbox, window, cx);
                self.mailbox.update(cx, |m, cx| m.select_thread_id(&thread_id, cx));
                self.thread.update(cx, |t, cx| t.open_reply(all, window, cx));
            }
        }
    }

    /// A change while the data is only read: say so, change nothing.
    fn refused(&mut self, cx: &mut Context<Self>) {
        self.show_toast("Read-only: changes aren't sent to durian yet".into(), ToastAction::None, cx);
    }

    fn show_toast(&mut self, message: SharedString, action: ToastAction, cx: &mut Context<Self>) {
        self.toast = Some((message, action));
        self._toast_timer = Some(cx.spawn(async move |this, cx| {
            cx.background_executor().timer(std::time::Duration::from_secs(4)).await;
            let _ = this.update(cx, |this, cx| {
                this.toast = None;
                cx.notify();
            });
        }));
        cx.notify();
    }

    fn show_queued(&mut self, draft_id: usize, send_after: i64, cx: &mut Context<Self>) {
        let remaining = send_after - chrono::Utc::now().timestamp();
        if remaining <= 0 {
            self.show_toast("The original delivery deadline has passed; status must be verified".into(), ToastAction::OpenDraft(draft_id), cx);
            return;
        }
        self.show_toast(format!("Queued for delivery in {remaining}s — not yet sent").into(), ToastAction::UndoSend(draft_id), cx);
        self._toast_timer = Some(cx.spawn(async move |this, cx| loop {
            cx.background_executor().timer(std::time::Duration::from_secs(1)).await;
            let keep = this.update(cx, |this, cx| {
                if !matches!(this.toast, Some((_, ToastAction::UndoSend(id))) if id == draft_id) { return false }
                let remaining = send_after - chrono::Utc::now().timestamp();
                if remaining <= 0 {
                    this.toast = Some(("Delivery deadline passed — status unconfirmed".into(), ToastAction::OpenDraft(draft_id)));
                    if let Some(live) = &this.live { live.update(cx, |live, cx| live.reconcile_outbox(cx)); }
                } else {
                    this.toast = Some((format!("Queued for delivery in {remaining}s — not yet sent").into(), ToastAction::UndoSend(draft_id)));
                }
                cx.notify();
                remaining > 0
            }).unwrap_or(false);
            if !keep { break }
        }));
    }

    fn forward_seed(&self, cx: &App) -> Option<ComposeSeed> {
        let thread = self.mailbox.read(cx).selected_thread()?;
        let latest = thread.latest();
        Some(ComposeSeed {
            subject: format!("Fwd: {}", thread.subject).into(),
            body: format!(
                "\n\n---------- Forwarded message ----------\nFrom: {} <{}>\nDate: {}\nSubject: {}\n\n{}",
                latest.from.name, latest.from.email, latest.at, thread.subject, latest.body
            )
            .into(),
            ..Default::default()
        })
    }

    fn show_account(&mut self, ix: Option<usize>, window: &mut Window, cx: &mut Context<Self>) {
        // With profiles, digit shortcuts pick the same ordered slots as the menu.
        // ShowAllAccounts remains a separate command, not the tenth slot.
        let profiles = &self.mailbox.read(cx).profiles;
        if !profiles.is_empty() {
            let target = match ix {
                Some(ix) => (ix < profiles.len()).then_some(ix),
                None => profiles.iter().position(|p| p.accounts.iter().any(|a| a == "*")),
            };
            if let Some(ix) = target {
                self.switch_profile(ix, cx);
                self.focus_area(window, cx);
            }
            return;
        }
        self.mailbox.update(cx, |m, cx| {
            let id = ix.and_then(|ix| m.accounts.get(ix)).map(|a| a.id.clone());
            if ix.is_none() || id.is_some() {
                m.set_account_filter(id, cx);
            }
        });
    }

}

impl Focusable for MailWorkspace {
    fn focus_handle(&self, _cx: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for MailWorkspace {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        // live mail views are AppKit children above GPUI's surface: while anything overlays
        // the window they show a picture of themselves instead
        #[cfg(target_os = "macos")]
        webview::frame(
            window,
            self.picker.is_some() || self.compose.is_some() || self.event_compose.is_some() || self.confirm_delete.is_some() || self.context_menu.is_some() || self.help.is_some() || self.toast.is_some(),
            self.mode == Mode::Inbox && self.thread_visible(),
        );
        // what the panes actually get: the remembered widths, squeezed so the thread keeps
        // THREAD_MIN in a narrow window (the list gives way first, then the sidebar)
        let available = f32::from(window.viewport_size().width) - 24.;
        let sidebar_w = if self.sidebar_open { self.widths.sidebar } else { 0. };
        let list_w = self.widths.list.min(available - sidebar_w - THREAD_MIN).max(LIST_MIN);
        let sidebar_w = sidebar_w.min(available - list_w - THREAD_MIN).max(180.);
        self.shown = PaneWidths { sidebar: sidebar_w, list: list_w };
        // built before the theme is borrowed below
        let (sidebar_handle, list_handle) = (self.resize_handle(Pane::Sidebar, cx), self.resize_handle(Pane::List, cx));
        let view = self.app_menu_handlers(div(), window, cx);
        let theme = cx.theme();
        let (bg, panel, border, shadow, font, fg) =
            (theme.bg_app, theme.bg_panel, theme.border_panel, theme.panel_shadow(), theme.font_family, theme.text_primary);

        view
            // "Workspace" carries the shortcuts that work everywhere, "Mail" the mail area's; the
            // calendar view adds its own "Calendar" context below
            .key_context(if self.mode == Mode::Calendar { "Workspace" } else if self.focus_mode { "Workspace Mail ThreadKeys" } else { "Workspace Mail" })
            .track_focus(&self.focus_handle)
            .capture_any_mouse_down(cx.listener(|this, _, _, cx| { this.clear_key_sequence(); cx.notify(); }))
            .on_key_down(cx.listener(|this, _: &KeyDownEvent, _, cx| {
                // An unbound key breaks a sequence; it must not leave a destructive prefix armed.
                if !this.key_engine.display().is_empty() {
                    this.clear_key_sequence();
                    cx.notify();
                }
            }))
            .on_action(cx.listener(Self::key_stroke))
            .on_action(cx.listener(|this, _: &ReloadKeymaps, _, cx| this.reload_keymaps(true, cx)))
            .on_action(cx.listener(|this, _: &SelectNext, _, cx| {
                trace::input("j");
                if this.mode == Mode::Calendar {
                    return this.schedule.update(cx, |s, cx| s.select_relative(1, cx));
                }
                this.direction = 1;
                this.mailbox.update(cx, |m, cx| m.select_relative(1, cx));
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &SelectPrev, _, cx| {
                trace::input("k");
                if this.mode == Mode::Calendar {
                    return this.schedule.update(cx, |s, cx| s.select_relative(-1, cx));
                }
                this.direction = -1;
                this.mailbox.update(cx, |m, cx| m.select_relative(-1, cx));
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &Retry, _, cx| {
                if let Some(live) = &this.live {
                    live.update(cx, |live, cx| live.retry(cx));
                }
            }))
            .on_action(cx.listener(|this, _: &Archive, _, cx| this.mailbox.update(cx, |m, cx| m.archive_selected(cx))))
            .on_action(cx.listener(|this, _: &ToggleStar, _, cx| this.mailbox.update(cx, |m, cx| m.toggle_star_selected(cx))))
            .on_action(cx.listener(|this, _: &ToggleExpandAll, _, cx| this.thread.update(cx, |t, cx| t.toggle_expand_all(cx))))
            .on_action(cx.listener(|this, _: &ToggleFocusMode, _, cx| this.set_focus_mode(!this.focus_mode, cx)))
            .on_action(cx.listener(|this, _: &OpenThread, _, cx| this.set_focus_mode(true, cx)))
            .on_action(cx.listener(|this, _: &ToggleListLayout, _, cx| {
                this.layout = if this.layout == Layout::Split { Layout::List } else { Layout::Split };
                trace::note(&format!("layout: {:?}", this.layout));
                let layout = this.layout;
                this.list.update(cx, |list, cx| list.set_layout(layout, cx));
                this.set_focus_mode(false, cx);
            }))
            .on_action(cx.listener(|this, _: &Reply, window, cx| this.thread.update(cx, |t, cx| t.open_reply(false, window, cx))))
            .on_action(cx.listener(|this, _: &ReplyAll, window, cx| this.thread.update(cx, |t, cx| t.open_reply(true, window, cx))))
            .on_action(cx.listener(|this, _: &NewMessage, window, cx| this.open_compose(ComposeSeed::default(), window, cx)))
            // the sidebar's compose button: whatever "new" means in the area you're in
            .on_action(cx.listener(|this, _: &ComposeHere, window, cx| {
                if this.mode == Mode::Calendar {
                    let day = this.schedule.read(cx).anchor;
                    this.open_event_compose(event_compose::EventSeed::next_hour(day), window, cx);
                } else {
                    this.open_compose(ComposeSeed::default(), window, cx);
                }
            }))
            .on_action(cx.listener(|this, _: &Forward, window, cx| {
                if let Some(live) = &this.live { live.update(cx, |live, cx| live.forward_selected(cx)); }
                else if let Some(seed) = this.forward_seed(cx) {
                    this.open_compose(seed, window, cx);
                }
            }))
            .on_action(cx.listener(|this, _: &OpenCommandMenu, window, cx| {
                let workspace = cx.entity().downgrade();
                let on_event_created = std::rc::Rc::new(move |id: SharedString, _: &mut Window, cx: &mut App| {
                    // in the calendar the new event is selected in place; elsewhere say where it went
                    let _ = workspace.update(cx, |this, cx| {
                        if this.mode != Mode::Calendar {
                            this.show_toast("Event created".into(), ToastAction::ShowEvent(id), cx);
                        }
                    });
                });
                let key_context = if this.mode == Mode::Calendar { keymaps::Context::Calendar } else if this.focus_mode { keymaps::Context::Thread } else { keymaps::Context::List };
                let delegate = commands::CommandMenu { mailbox: this.mailbox.clone(), schedule: this.schedule.clone(), scope: this.scope(), key_context, on_event_created };
                this.open_picker(delegate, 640., window, cx);
            }))
            .on_action(cx.listener(|this, _: &OpenTagPicker, window, cx| {
                if this.mailbox.read(cx).selected_thread().is_some() {
                    let delegate = commands::TagPicker { mailbox: this.mailbox.clone() };
                    this.open_picker(delegate, 440., window, cx);
                    this.tag_picker = true;
                }
            }))
            .on_action(cx.listener(|this, _: &GoToDrafts, window, cx| this.set_mode(Mode::Drafts, window, cx)))
            .on_action(cx.listener(|this, _: &ShowContextMenu, window, cx| {
                if !cx.has_global::<PendingMenu>() {
                    return;
                }
                let Some((position, items)) = cx.global_mut::<PendingMenu>().0.take() else { return };
                trace::note(&format!("context menu: {} items", items.len()));
                let menu = cx.new(|cx| ContextMenu::new(position, items, window, cx));
                let subscription = cx.subscribe_in(&menu, window, |this, _, ContextMenuEvent::Dismissed, window, cx| {
                    this.context_menu = None;
                    this.focus_area(window, cx);
                    cx.notify();
                });
                this.context_menu = Some((menu, subscription));
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &ToggleRead, _, cx| this.mailbox.update(cx, |m, cx| m.toggle_read_selected(cx))))
            .on_action(cx.listener(|this, _: &ToggleShortcutHelp, window, cx| {
                if this.help.take().is_some() {
                    this.focus_area(window, cx);
                } else {
                    trace::note("shortcut help: open");
                    let scope = this.scope();
                    let panel = cx.new(|cx| help::ShortcutHelp::new(scope, window, cx));
                    let subscription = cx.subscribe_in(&panel, window, |this, _, help::ShortcutHelpEvent::Close, window, cx| {
                        trace::note("shortcut help: closed");
                        this.help = None;
                        this.focus_area(window, cx);
                        cx.notify();
                    });
                    this.help = Some((panel, subscription));
                }
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &FocusSearch, window, cx| {
                if this.mode == Mode::Drafts {
                    this.set_mode(Mode::Inbox, window, cx);
                }
                this.focus_mode = false;
                this.list.update(cx, |list, cx| list.focus_search(window, cx));
            }))
            .on_action(cx.listener(|_, _: &ToggleDarkMode, _, cx| {
                let next = if cx.theme().is_dark() { Appearance::Light } else { Appearance::Dark };
                trace::note(&format!("appearance: {next:?}"));
                cx.set_global(next);
                ui::theme::apply_appearance(cx);
            }))
            .on_action(cx.listener(|_, _: &UseSystemAppearance, _, cx| {
                cx.set_global(Appearance::System);
                ui::theme::apply_appearance(cx);
            }))
            .on_action(cx.listener(|this, _: &ShowAllAccounts, window, cx| this.show_account(None, window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount1, window, cx| this.show_account(Some(0), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount2, window, cx| this.show_account(Some(1), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount3, window, cx| this.show_account(Some(2), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount4, window, cx| this.show_account(Some(3), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount5, window, cx| this.show_account(Some(4), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount6, window, cx| this.show_account(Some(5), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount7, window, cx| this.show_account(Some(6), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount8, window, cx| this.show_account(Some(7), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount9, window, cx| this.show_account(Some(8), window, cx)))
            .on_action(cx.listener(|this, _: &ShowAccount10, window, cx| this.show_account(Some(9), window, cx)))
            .on_action(cx.listener(|this, _: &ToggleSidebar, _, cx| {
                this.sidebar_open = !this.sidebar_open;
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &PreviewAttachment, _, cx| {
                if let Some(attachment) = this.thread.read(cx).attachment_to_preview(cx) {
                    attachments::quick_look(&attachment, cx);
                }
            }))
            .on_action(cx.listener(|this, _: &GoToInbox, window, cx| this.set_mode(Mode::Inbox, window, cx)))
            .on_action(cx.listener(|this, _: &GoToCalendar, window, cx| this.set_mode(Mode::Calendar, window, cx)))
            // the open thread as an event: its subject, its people (without you)
            .on_action(cx.listener(|this, _: &EventFromThread, window, cx| {
                let Some((subject, guests)) = this.mailbox.read(cx).selected_thread().map(|t| {
                    let me = this.mailbox.read(cx).me.name.clone();
                    let mut people: Vec<SharedString> = Vec::new();
                    for m in t.messages() {
                        for name in std::iter::once(&m.from.name).chain(&m.to).chain(&m.cc) {
                            if *name != me && !people.contains(name) {
                                people.push(name.clone());
                            }
                        }
                    }
                    // "Re: Fwd: Lunch Thursday?" → "Lunch Thursday?"
                    let mut subject = t.subject.as_ref();
                    while let Some(rest) = ["Re: ", "RE: ", "Fwd: ", "AW: ", "WG: ", "Invitation: "].iter().find_map(|p| subject.strip_prefix(p)) {
                        subject = rest;
                    }
                    let subject = subject.to_string();
                    (subject, people)
                }) else {
                    return;
                };
                let mut seed = event_compose::EventSeed::next_hour(chrono::Local::now().date_naive());
                seed.title = Some(subject.into());
                seed.guests = guests;
                this.open_event_compose(seed, window, cx);
            }))
            // ⌘C / ⌘A on read-only text: the open thread, or the event details in the calendar
            // (text fields handle their own – their bindings are deeper in the key context)
            // The native Edit menu uses the editor's action types. Focused fields consume
            // them first; only unhandled actions reach this read-only fallback.
            .on_action(cx.listener(|_, _: &ui::Copy, window, cx| window.dispatch_action(Box::new(CopySelection), cx)))
            .on_action(cx.listener(|_, _: &ui::SelectAll, window, cx| window.dispatch_action(Box::new(SelectAllText), cx)))
            .on_action(cx.listener(|this, _: &CopySelection, _, cx| {
                let selection = if this.mode == Mode::Calendar { this.calendar.read(cx).selection.clone() } else { this.thread.read(cx).selection.clone() };
                if let Some(text) = selection.selected_text() {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(text));
                }
            }))
            .on_action(cx.listener(|this, _: &SelectAllText, _, cx| {
                let selection = if this.mode == Mode::Calendar { this.calendar.read(cx).selection.clone() } else { this.thread.read(cx).selection.clone() };
                selection.select_all(cx);
            }))
            // the selected event: edit (yours), delete / cancel / remove, duplicate
            .on_action(cx.listener(|this, _: &calendar::EditEvent, window, cx| {
                let s = this.schedule.read(cx);
                let Some(event) = s.selected_event().cloned() else { return };
                if this.live.is_some() {
                    return this.schedule.update(cx, |_, cx| cx.emit(calendar::model::ScheduleEvent::EditRequested(event.id)));
                }
                if !s.is_own(&event) { return; }
                let me = s.me.clone();
                this.open_event_compose(event_compose::EventSeed::edit(event, &me), window, cx);
            }))
            .on_action(cx.listener(|this, _: &calendar::DeleteEvent, window, cx| {
                let s = this.schedule.read(cx);
                let Some(event) = s.selected_event().cloned() else { return };
                this.request_delete(vec![event.id], window, cx);
            }))
            .on_action(cx.listener(|this, _: &calendar::DuplicateEvent, _, cx| {
                let Some(id) = this.schedule.read(cx).selected.clone() else { return };
                let copy: SharedString = uuid::Uuid::new_v4().simple().to_string().into();
                this.schedule.update(cx, |s, cx| s.duplicate(&id, copy, cx));
            }))
            .on_action(cx.listener(|this, _: &NewEvent, window, cx| {
                let day = this.schedule.read(cx).anchor;
                this.open_event_compose(event_compose::EventSeed::next_hour(day), window, cx);
            }))
            .on_action(cx.listener(|this, _: &ExitFocusMode, window, cx| {
                if this.mode == Mode::Calendar {
                    return this.schedule.update(cx, |s, cx| s.select(None, cx));
                }
                if this.mode == Mode::Drafts {
                    this.set_mode(Mode::Inbox, window, cx);
                }
                this.focus_mode = false;
                this.clear_key_sequence();
                cx.notify();
            }))
            // pane resizing: follow the mouse anywhere in the window, settle on release
            .on_mouse_move(cx.listener(|this, event: &gpui::MouseMoveEvent, _, cx| {
                let Some(r) = this.resizing else { return };
                let width = r.start_width + f32::from(event.position.x - r.start_x);
                // dragging the sidebar far enough in closes it, like Linear
                if r.pane == Pane::Sidebar && width < 140. {
                    this.resizing = None;
                    this.sidebar_open = false;
                } else {
                    this.widths.set(r.pane, width);
                }
                cx.notify();
            }))
            .on_mouse_up(gpui::MouseButton::Left, cx.listener(|this, _, _, cx| {
                if this.resizing.take().is_some() {
                    this.widths.save();
                    cx.notify();
                }
            }))
            .flex()
            .size_full()
            .bg(bg)
            .font_family(font)
            .text_color(fg)
            .text_size(px(SIZE_SMALL))
            // cached views (Zed's trick): a subtree is only rebuilt when its entity notifies,
            // so e.g. a link hover in the thread doesn't re-layout sidebar and list
            .when(self.sidebar_open, |s| {
                s.child(AnyView::from(self.sidebar.clone()).cached(StyleRefinement::default().w(px(sidebar_w)).h_full()))
                    .child(sidebar_handle)
            })
            // trace probe: its paint runs after every other element of this frame was painted
            .when(trace::enabled(), |s| {
                s.child(gpui::canvas(|_, _, _| trace::mark("prepainted"), |_, _, _, _| trace::finish()).absolute().size_0())
            })
            .child(
                div()
                    .flex()
                    .flex_col()
                    .flex_1()
                    .min_w_0()
                    // with the sidebar collapsed the toggle moves into the panel's title strip
                    .map(|s| {
                        if self.sidebar_open {
                            s.child(div().h(px(TITLEBAR_HEIGHT)).flex_none().window_control_area(gpui::WindowControlArea::Drag))
                        } else {
                            s.child(sidebar::mail_titlebar(false, cx.listener(|this, _, _, cx| {
                                this.sidebar_open = true;
                                cx.notify();
                            })))
                        }
                    })
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .flex_1()
                            .min_h_0()
                            .relative()
                            .when(!self.sidebar_open, |s| s.ml(px(8.)))
                            .mr(px(8.))
                            .mb(px(8.))
                            .rounded(theme.radius.xl)
                            .bg(panel)
                            .border_hairline()
                            .border_color(border)
                            .shadow(shadow)
                            .overflow_hidden()
                            .child(
                                div()
                                    .flex()
                                    .flex_1()
                                    .min_h_0()
                                    .map(|s| match self.mode {
                                        Mode::Drafts => s.child(div().size_full().child(self.drafts.clone())),
                                        Mode::Calendar => s.child(AnyView::from(self.calendar.clone()).cached(StyleRefinement::default().size_full())),
                                        Mode::Inbox => {
                                            let list = AnyView::from(self.list.clone());
                                            let thread = AnyView::from(self.thread.clone()).cached(StyleRefinement::default().flex_1().min_w_0().h_full());
                                            match (self.layout, self.focus_mode) {
                                                (_, true) => s.child(thread),
                                                (Layout::Split, false) => s
                                                    .child(list.cached(StyleRefinement::default().w(px(list_w)).h_full().flex_none()))
                                                    .child(list_handle)
                                                    .child(thread),
                                                (Layout::List, false) => s.child(list.cached(StyleRefinement::default().size_full())),
                                            }
                                        }
                                    }),
                            )
                            // pending count/sequence, like vim's showcmd
                            .children((!self.key_engine.display().is_empty()).then(|| {
                                div().absolute().right(px(12.)).bottom(px(12.)).child(Kbd::new(self.key_engine.display()))
                            })),
                    ),
            )
            .children(self.toast.clone().map(|(message, action)| {
                let toast = Toast::new(message, cx.listener(|this, _, _, cx| {
                    this.toast = None;
                    cx.notify();
                }));
                let toast = match action {
                    ToastAction::RestoreEvents(events) => toast.action("Undo", cx.listener(move |this, _, _, cx| {
                        this.toast = None;
                        let events = events.clone();
                        this.schedule.update(cx, |s, cx| s.restore_events(events, cx));
                        cx.notify();
                    })),
                    ToastAction::ShowEvent(id) => toast.action("Show", cx.listener(move |this, _, window, cx| {
                        this.toast = None;
                        let id = id.clone();
                        this.schedule.update(cx, |s, cx| {
                            if let Some(day) = s.event(&id).map(|e| e.start.date()) {
                                s.anchor = day;
                            }
                            s.select(Some(id), cx);
                        });
                        this.set_mode(Mode::Calendar, window, cx);
                    })),
                    ToastAction::OpenDraft(id) => toast.action("Open draft", cx.listener(move |this, _, window, cx| {
                        this.toast = None;
                        this.open_draft(id, window, cx);
                    })),
                    ToastAction::UndoSend(id) => toast.action("Undo", cx.listener(move |this, _, _, cx| {
                        if let Some(live) = &this.live { live.update(cx, |live, cx| live.undo_send(id, cx)); }
                    })),
                    ToastAction::Reveal(path) => toast.action("Show in Finder", cx.listener(move |this, _, _, cx| {
                        this.toast = None;
                        attachments::reveal_in_finder(&path);
                        cx.notify();
                    })),
                    ToastAction::None => toast,
                };
                div().absolute().inset_0().child(toast).with_animation("toast-in", ui::entrance(180), |s, t| s.opacity(t).top(px(10. * (1. - t))))
            }))
            // the shortcut sheet slides in a little from the right edge
            .children(self.help.as_ref().map(|(panel, _)| {
                div()
                    .absolute()
                    .inset_0()
                    .child(panel.clone())
                    .with_animation("help-in", ui::entrance(160), |s, t| s.opacity(t).left(px(24. * (1. - t))))
            }))
            .children(self.context_menu.as_ref().map(|(menu, _)| menu.clone()))
            // composer: dimmed backdrop
            .children(self.compose.as_ref().map(|(dialog, _)| {
                div()
                    .id("compose-backdrop")
                    .absolute()
                    .inset_0()
                    .occlude()
                    .bg(cx.theme().backdrop)
                    .flex()
                    .justify_center()
                    .items_start()
                    .pt(px(106.))
                    .child(dialog.clone())
                    // backdrop fades, the card rises a few pixels into place
                    .with_animation("compose-in", ui::entrance(160), |s, t| s.opacity(t).pt(px(106. + 8. * (1. - t))))
            }))
            // the event composer: same stage as the mail composer
            .children(self.event_compose.as_ref().map(|(dialog, _)| {
                div()
                    .id("event-compose-backdrop")
                    .absolute()
                    .inset_0()
                    .occlude()
                    .bg(cx.theme().backdrop)
                    .flex()
                    .justify_center()
                    .items_start()
                    .pt(px(106.))
                    .child(dialog.clone())
                    .with_animation("event-compose-in", ui::entrance(160), |s, t| s.opacity(t).pt(px(106. + 8. * (1. - t))))
            }))
            .children(self.confirm_delete.as_ref().map(|ids| {
                let series = ids.iter().any(|id| self.schedule.read(cx).event(id).is_some_and(|event| event.series.is_some()));
                ui::ConfirmDialog::new(
                    if series { "Delete the entire series?" } else { "Delete this event?" },
                    "This deletes the stored event and syncs the change to its account. A recurring event is deleted for all dates; guests may be notified. This cannot be undone here.",
                    "Delete",
                    cx.listener(|this, _, window, cx| {
                        if let Some(ids) = this.confirm_delete.take() { this.remove_events(ids, "Event deleted", cx); }
                        this.focus_area(window, cx);
                        cx.notify();
                    }),
                    cx.listener(|this, _, window, cx| {
                        this.confirm_delete = None;
                        this.focus_area(window, cx);
                        cx.notify();
                    }),
                ).track_focus(&self.confirm_focus)
            }))
            .children(self.calendar_writes.as_ref().filter(|writes| writes.read(cx).recovery_count() > 0).map(|writes| {
                let writes = writes.clone();
                div().absolute().bottom(px(16.)).left(px(16.)).child(
                    ui::PrimaryButton::new("recover-calendar", "Recover calendar changes").on_click(cx.listener(move |this, _, window, cx| {
                        writes.update(cx, |writes, cx| writes.recover(cx));
                        this.focus_area(window, cx);
                    }))
                )
            }))
            // picker overlay: transparent backdrop (no dimming), click outside closes
            .children(self.picker.as_ref().map(|(view, _)| {
                div()
                    .key_context(if self.tag_picker { "TagKeys" } else { "SearchKeys" })
                    .id("picker-backdrop")
                    .absolute()
                    .inset_0()
                    .occlude()
                    .flex()
                    .justify_center()
                    // don't stretch the card to the window height (flex rows default to `stretch`)
                    .items_start()
                    .pt(px(TITLEBAR_HEIGHT + 64.))
                    .on_mouse_down(gpui::MouseButton::Left, cx.listener(|this, _, window, cx| this.close_picker(window, cx)))
                    .child(view.clone())
                    .with_animation("picker-in", ui::entrance(130), |s, t| s.opacity(t).pt(px(TITLEBAR_HEIGHT + 64. + 6. * (1. - t))))
            }))
    }
}
