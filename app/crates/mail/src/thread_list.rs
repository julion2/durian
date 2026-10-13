//! Thread list, in two layouts:
//! - `Split`: a 400px column next to the thread – 55px two-line rows, 32px avatar, 8px inset
//!   selection with 8px radius, read rows dimmed to 80% opacity.
//! - `List`: full width, no thread beside it – 44px one-line rows in columns (sender, subject +
//!   preview, tag pills, date) under collapsible date groups. Enter opens the thread full width.

use std::collections::HashSet;

use gpui::{
    AnyElement, App, Context, Div, Entity, EventEmitter, Focusable, IntoElement, KeyBinding, ListAlignment, ListState, MouseButton, Render,
    ScrollStrategy, SharedString, Stateful, Subscription, UniformListScrollHandle, Window, actions, deferred, div, list, prelude::*, px,
    uniform_list,
};

use crate::model::{Loading, Mailbox, MailboxEvent, Thread, first_name};
use ui::theme::{ActiveTheme, Theme, typography::*};
use crate::{contact_avatar, context_menus};
use crate::query;
use ui::{EmptyState, Icon, IconButton, IconName, Kbd, StyledExt, TextInput, TextInputEvent};

pub const LIST_WIDTH: f32 = 400.;
const ROW_HEIGHT: f32 = 55.;
const AVATAR: f32 = 32.;
const UNREAD_DOT: f32 = 7.;
/// Ring in the row's background color that separates the badge from the avatar.
const UNREAD_RING: f32 = 2.;
const WIDE_ROW_HEIGHT: f32 = 44.;
const GROUP_HEIGHT: f32 = 36.;
const SENDER_WIDTH: f32 = 160.;
const DATE_WIDTH: f32 = 44.;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Layout {
    Split,
    List,
}

/// Date groups of the `List` layout, in display order.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub enum Bucket {
    Today,
    Yesterday,
    ThisWeek,
    Earlier,
}

impl Bucket {
    const ALL: [Bucket; 4] = [Bucket::Today, Bucket::Yesterday, Bucket::ThisWeek, Bucket::Earlier];

    fn title(self) -> &'static str {
        match self {
            Bucket::Today => "Today",
            Bucket::Yesterday => "Yesterday",
            Bucket::ThisWeek => "Last 7 days",
            Bucket::Earlier => "Earlier",
        }
    }
}

/// Which date group a thread falls into, from its latest message. Today / Yesterday are
/// calendar days (last night's 23:00 mail is "Yesterday" in the morning); after that a rolling
/// week, so the group isn't empty on Mondays. The fixture only knows relative ages ("8m", "1d"),
/// which `query::ago_hours` turns into hours.
pub fn bucket(thread: &Thread) -> Bucket {
    let hours = query::ago_hours(&thread.latest().ago);
    let now = chrono::Local::now();
    let sent = now - chrono::Duration::minutes((hours * 60.) as i64);
    match (now.date_naive() - sent.date_naive()).num_days() {
        0 => Bucket::Today,
        1 => Bucket::Yesterday,
        2..=7 => Bucket::ThisWeek,
        _ => Bucket::Earlier,
    }
}

/// One line of the `List` layout.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum Row {
    Group { bucket: Bucket, count: usize },
    Thread(usize),
}

actions!(search, [SearchSubmit, SearchCancel, SearchComplete, SearchNext, SearchPrev]);

pub fn bind_search_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("enter", SearchSubmit, Some("SearchBar")),
        KeyBinding::new("escape", SearchCancel, Some("SearchBar")),
        KeyBinding::new("tab", SearchComplete, Some("SearchBar")),
        KeyBinding::new("down", SearchNext, Some("SearchBar")),
        KeyBinding::new("up", SearchPrev, Some("SearchBar")),
    ]);
}

pub enum ThreadListEvent {
    /// the search bar gave up the keyboard (Enter / Esc on an empty query)
    ReleaseFocus,
}

impl EventEmitter<ThreadListEvent> for ThreadList {}

pub struct ThreadList {
    mailbox: Entity<Mailbox>,
    layout: Layout,
    scroll: UniformListScrollHandle,
    /// `List` layout: grouped rows and their (variable-height) list state
    rows: Vec<Row>,
    list_state: ListState,
    collapsed: HashSet<Bucket>,
    search: Entity<TextInput>,
    completions: Vec<(SharedString, SharedString)>,
    completion_ix: usize,
    /// the visible threads (indices into the mailbox), recomputed when the mailbox changes –
    /// not on every frame: filtering runs the query over every thread
    visible: std::rc::Rc<Vec<usize>>,
    _search_subscription: Subscription,
}

impl ThreadList {
    /// Ctrl-d/u move half the visible pane, in the current row density.
    pub fn page_size(&self, window: &Window) -> usize {
        let height = f32::from(window.viewport_size().height) - ui::TITLEBAR_HEIGHT - 96.;
        let row_height = if self.layout == Layout::Split { ROW_HEIGHT } else { WIDE_ROW_HEIGHT };
        (height / row_height / 2.).max(1.) as usize
    }

    pub fn new(mailbox: Entity<Mailbox>, cx: &mut Context<Self>) -> Self {
        cx.observe(&mailbox, |this, _, cx| {
            this.refresh_visible(cx);
            this.rebuild_rows(cx);
            cx.notify();
        })
        .detach();
        cx.subscribe(&mailbox, |this, mailbox, event, cx| match event {
            MailboxEvent::SelectionChanged => {
                // the event can arrive before the change notification (new list + selection)
                this.refresh_visible(cx);
                let selected = mailbox.read(cx).selected;
                match this.layout {
                    Layout::Split => {
                        // the list shows the filtered threads: scroll to the selection's row, not its index
                        if let Some(row) = selected.and_then(|ix| this.visible.iter().position(|&v| v == ix)) {
                            this.scroll.scroll_to_item(row, ScrollStrategy::Center);
                        }
                    }
                    Layout::List => {
                        // j/k into a collapsed group opens it
                        if let Some(ix) = selected {
                            let group = bucket(&mailbox.read(cx).threads[ix]);
                            if this.collapsed.remove(&group) {
                                this.rebuild_rows(cx);
                            }
                        }
                        if let Some(row) = this.rows.iter().position(|r| Some(*r) == selected.map(Row::Thread)) {
                            this.list_state.scroll_to_reveal_item(row);
                        }
                    }
                }
            }
            _ => {}
        })
        .detach();
        let search = cx.new(|cx| TextInput::new("Search…  tag:unread  from:", cx));
        // live: every keystroke re-filters the list and refreshes completions
        let subscription = cx.subscribe(&search, |this, _, event, cx| match event {
            TextInputEvent::Changed(text) => {
                this.completions = query::completions(text, this.mailbox.read(cx));
                this.completion_ix = 0;
                let text = text.clone();
                this.mailbox.update(cx, |m, cx| m.set_query(text, None, cx));
            }
        });
        let list_state = ListState::new(0, ListAlignment::Top, px(400.));
        // the wide list pages in like the column (whose rows ask while rendering)
        let weak = mailbox.downgrade();
        list_state.set_scroll_handler(move |event, _, cx| {
            if event.visible_range.end + 30 >= event.count
                && let Some(mailbox) = weak.upgrade()
            {
                cx.defer(move |cx| mailbox.update(cx, |m, cx| m.want_more(cx)));
            }
        });
        let visible: Vec<usize> = mailbox.read(cx).visible().collect();
        Self {
            mailbox,
            layout: Layout::Split,
            scroll: UniformListScrollHandle::new(),
            rows: Vec::new(),
            list_state,
            collapsed: HashSet::new(),
            search,
            completions: Vec::new(),
            completion_ix: 0,
            visible: visible.into(),
            _search_subscription: subscription,
        }
    }

    fn refresh_visible(&mut self, cx: &App) {
        self.visible = std::rc::Rc::new(self.mailbox.read(cx).visible().collect());
    }

    pub fn set_layout(&mut self, layout: Layout, cx: &mut Context<Self>) {
        self.layout = layout;
        self.rebuild_rows(cx);
        cx.notify();
    }

    /// Regroup the visible threads. Only the changed middle of the row list is spliced, so
    /// archiving a thread or reading one doesn't throw away the scroll position.
    fn rebuild_rows(&mut self, cx: &App) {
        if self.layout != Layout::List {
            return;
        }
        let mailbox = self.mailbox.read(cx);
        let visible = self.visible.clone();
        let mut rows = Vec::new();
        for group in Bucket::ALL {
            let threads: Vec<usize> = visible.iter().copied().filter(|ix| bucket(&mailbox.threads[*ix]) == group).collect();
            if threads.is_empty() {
                continue;
            }
            rows.push(Row::Group { bucket: group, count: threads.len() });
            if !self.collapsed.contains(&group) {
                rows.extend(threads.into_iter().map(Row::Thread));
            }
        }
        let old = &self.rows;
        if *old != rows {
            let prefix = old.iter().zip(&rows).take_while(|(a, b)| a == b).count();
            let suffix = old[prefix..].iter().rev().zip(rows[prefix..].iter().rev()).take_while(|(a, b)| a == b).count();
            self.list_state.splice(prefix..old.len() - suffix, rows.len() - prefix - suffix);
            self.rows = rows;
        }
    }

    fn toggle_group(&mut self, group: Bucket, cx: &mut Context<Self>) {
        if !self.collapsed.remove(&group) {
            self.collapsed.insert(group);
        }
        self.rebuild_rows(cx);
        cx.notify();
    }

    /// `/`: put the cursor in the search bar.
    pub fn focus_search(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus_handle(cx).focus(window, cx);
        cx.notify();
    }

    /// A query from a click (sidebar view, tag, the unread toggle): complete as it is, so it
    /// searches at once – a trailing space ends its last token, which would otherwise count
    /// as still being typed and only narrow what's listed.
    pub fn apply_query(&mut self, text: SharedString, title: Option<SharedString>, cx: &mut Context<Self>) {
        self.set_search(finished(&text), title, cx);
    }

    /// Sidebar views / tags: show their query in the bar and apply it.
    pub fn set_search(&mut self, text: SharedString, title: Option<SharedString>, cx: &mut Context<Self>) {
        self.search.update(cx, |input, cx| {
            input.set_text(text.clone());
            cx.notify();
        });
        self.completions.clear();
        self.mailbox.update(cx, |m, cx| m.set_query(text, title, cx));
        cx.notify();
    }

    fn accept_completion(&mut self, cx: &mut Context<Self>) -> bool {
        let Some((completion, _)) = self.completions.get(self.completion_ix).cloned() else { return false };
        let text: SharedString = query::apply_completion(self.search.read(cx).text(), &completion).into();
        self.set_search(text.clone(), None, cx);
        // a finished key ("tag:") keeps completing its values
        self.completions = query::completions(&text, self.mailbox.read(cx));
        true
    }

    fn render_search(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme().clone();
        let (text, title, saved) = {
            let m = self.mailbox.read(cx);
            let text = m.query_text.clone();
            let saved = m.views.iter().any(|v| v.query.trim() == text.trim());
            (text, m.view_title.clone(), saved)
        };
        let has_query = !text.trim().is_empty();
        let completions = (!self.completions.is_empty()).then(|| {
            deferred(
                div()
                    .absolute()
                    .top(px(40.))
                    .left(px(8.))
                    .right(px(8.))
                    .p(px(4.))
                    .bg(theme.bg_menu)
                    .border_hairline()
                    .border_color(theme.border_menu)
                    .rounded(theme.radius.lg)
                    .shadow(theme.menu_shadow())
                    .children(self.completions.iter().enumerate().map(|(ix, (completion, detail))| {
                        div()
                            .id(("completion", ix))
                            .flex()
                            .items_center()
                            .gap(px(8.))
                            .h(px(30.))
                            .px(px(8.))
                            .rounded(theme.radius.sm)
                            .when(ix == self.completion_ix, |s| s.bg(theme.bg_option))
                            .child(div().font_family(theme.font_mono).text_size(px(SIZE_MINI)).text_color(theme.text_primary).child(completion.clone()))
                            .child(div().flex_1().min_w_0().truncate().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(detail.clone()))
                            .on_mouse_down(MouseButton::Left, cx.listener(move |this, _, _, cx| {
                                this.completion_ix = ix;
                                this.accept_completion(cx);
                            }))
                    })),
            )
            .with_priority(2)
        });

        div()
            .relative()
            .flex()
            .items_center()
            .h(px(44.))
            .px(px(8.))
            .child(
                div()
                    .key_context("SearchBar")
                    .on_action(cx.listener(|this, _: &SearchComplete, _, cx| {
                        this.accept_completion(cx);
                    }))
                    .on_action(cx.listener(|this, _: &SearchSubmit, _, cx| {
                        // accept a completion only if it adds something to what's typed
                        let last = this.search.read(cx).text().rsplit(char::is_whitespace).next().unwrap_or("").to_string();
                        let adds = this.completions.get(this.completion_ix).is_some_and(|(c, _)| c.as_ref() != last);
                        if adds && this.accept_completion(cx) {
                            return;
                        }
                        // Enter: results are live already – hand the keyboard to the list
                        this.completions.clear();
                        cx.emit(ThreadListEvent::ReleaseFocus);
                    }))
                    .on_action(cx.listener(|this, _: &SearchCancel, _, cx| {
                        if !this.completions.is_empty() {
                            this.completions.clear();
                            cx.notify();
                        } else if !this.search.read(cx).text().is_empty() {
                            this.set_search(SharedString::default(), None, cx);
                        } else {
                            cx.emit(ThreadListEvent::ReleaseFocus);
                        }
                    }))
                    .on_action(cx.listener(|this, _: &SearchNext, _, cx| {
                        if !this.completions.is_empty() {
                            this.completion_ix = (this.completion_ix + 1) % this.completions.len();
                            cx.notify();
                        }
                    }))
                    .on_action(cx.listener(|this, _: &SearchPrev, _, cx| {
                        if !this.completions.is_empty() {
                            this.completion_ix = (this.completion_ix + this.completions.len() - 1) % this.completions.len();
                            cx.notify();
                        }
                    }))
                    .flex()
                    .flex_1()
                    .items_center()
                    .gap(px(8.))
                    .h(px(30.))
                    .px(px(10.))
                    .rounded(theme.radius.md)
                    .bg(theme.bg_raised)
                    .shadow(theme.raised_shadow(false))
                    .child(Icon::new(IconName::Search).size(13.).color(theme.text_tertiary))
                    .child(div().flex_1().min_w_0().text_size(px(SIZE_SMALL)).text_color(theme.text_primary).child(self.search.clone()))
                    .when(has_query, |s| {
                        s.child(IconButton::new("clear-search", IconName::CloseSmall).size(20.).on_click(cx.listener(|this, _, _, cx| {
                            this.set_search(SharedString::default(), None, cx);
                        })))
                    })
                    .when(!has_query, |s| s.children(crate::commands::keys_for(&crate::FocusSearch, &[crate::keymaps::Context::List], cx).into_iter().map(Kbd::new))),
            )
            .when(has_query && !saved, |s| {
                s.child(
                    div()
                        .id("save-view")
                        .ml(px(6.))
                        .h(px(28.))
                        .px(px(10.))
                        .flex()
                        .items_center()
                        .rounded(theme.radius.pill)
                        .shadow(theme.raised_shadow(false))
                        .text_size(px(SIZE_MINI))
                        .font_weight(WEIGHT_MEDIUM)
                        .text_color(theme.text_secondary)
                        .hover(|s| s.bg(theme.bg_raised_hover))
                        .child("Save view")
                        .on_click(cx.listener(move |this, _, _, cx| {
                            let name: SharedString = if title.as_ref() == "Search" { text.trim().to_string().into() } else { title.clone() };
                            this.mailbox.update(cx, |m, cx| m.save_view(name, cx));
                        })),
                )
            })
            .children(completions)
    }

    fn render_header(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let title = self.mailbox.read(cx).view_title.clone();
        let mailbox = self.mailbox.read(cx);
        let selection = mailbox.selection_mode().map(|mode| format!("{mode} · {}", mailbox.selected_ids(1).len()));
        div()
            .flex()
            .flex_col()
            .flex_none()
            .child(
                div()
                    .flex()
                    .items_center()
                    .h(px(44.))
                    .px(px(8.))
                    .gap(px(4.))
                    .border_b_hairline()
                    .border_color(theme.border_panel)
                    .child(
                        div()
                            .pl(px(10.))
                            .text_size(px(SIZE_SMALL))
                            .font_weight(WEIGHT_MEDIUM)
                            .text_color(theme.text_secondary)
                            .child(title),
                    )
                    .child(IconButton::new("inbox-options", IconName::More))
                    .child(div().flex_1())
                    .children(selection.map(|label| div().text_size(px(SIZE_MINI)).text_color(theme.accent).child(label)))
                    .child(IconButton::new("unread-only", IconName::MailUnreadOnly).on_click(cx.listener(|this, _, _, cx| {
                        // Toggle the same unread tag that read/unread actions write.
                        let text = this.search.read(cx).text().to_string();
                        let next = if text.split_whitespace().any(|t| t == "tag:unread") {
                            text.split_whitespace().filter(|t| *t != "tag:unread").collect::<Vec<_>>().join(" ")
                        } else {
                            format!("{} tag:unread", text.trim()).trim().to_string()
                        };
                        this.apply_query(next.into(), None, cx);
                    })))
                    .child(IconButton::new("inbox-filter", IconName::Filter).on_click(cx.listener(|this, _, window, cx| this.focus_search(window, cx))))
                    .child(IconButton::new("inbox-display", IconName::Display).on_click(|_, window, cx| {
                        window.dispatch_action(Box::new(crate::ToggleListLayout), cx)
                    })),
            )
            .child(self.render_search(cx))
    }
}

/// "Mara, Tobias, me (5)". Long threads show the opener and the two most recent senders,
/// Gmail-style: "Tobias … Emma, Tobias (10)".
fn people(thread: &Thread, mailbox: &Mailbox) -> SharedString {
    let short = |n: &SharedString| if *n == mailbox.me.name { "me".to_string() } else { first_name(n).to_string() };
    let senders = thread.senders();
    let count = thread.messages().count();
    let joined = if senders.len() > 3 {
        let mut recent: Vec<&SharedString> = Vec::new();
        for m in thread.messages().rev() {
            let name = &m.from.name;
            if !recent.contains(&name) {
                recent.push(name);
            }
            if recent.len() == 2 {
                break;
            }
        }
        // Match senders()' identity before shortening labels. Several distinct
        // people can share a first name, which must not collapse this pair.
        format!("{} … {}, {}", short(&senders[0]), short(recent[1]), short(recent[0]))
    } else {
        senders.iter().map(short).collect::<Vec<_>>().join(", ")
    };
    if count > 1 { format!("{joined} ({count})").into() } else { joined.into() }
}

fn render_row(ix: usize, thread: &Thread, mailbox: &Mailbox, selected: bool, theme: &Theme) -> Stateful<Div> {
    let latest = thread.latest();
    let contact = mailbox.list_contact(thread);
    let label = if mailbox.is_me(&latest.from) && !mailbox.is_me(&contact) {
        let count = thread.messages().count();
        let name = first_name(&contact.name);
        if count > 1 { format!("To {name} ({count})").into() } else { format!("To {name}").into() }
    } else {
        people(thread, mailbox)
    };
    let unread = thread.unread();
    let fg = if unread { theme.text_primary } else { theme.text_tertiary };
    let opacity = if unread { 1. } else { 0.8 };
    let tag_dots: Vec<_> = thread.tags.iter().filter_map(|t| mailbox.tag(t)).map(|t| t.color).collect();
    let (hover_bg, selected_bg, accent) = (theme.bg_group, theme.bg_selected, theme.accent);
    let row_bg = if selected { selected_bg } else { theme.bg_panel };
    let snippet = latest.preview();

    div().id(("thread", ix)).h(px(ROW_HEIGHT)).w_full().child(
        div()
            .group("thread-row")
            .relative()
            .h_full()
            .mx(px(8.))
            .px(px(8.))
            .rounded(theme.radius.md)
            .map(|s| if selected { s.bg(selected_bg) } else { s.hover(|s| s.bg(hover_bg)) })
            .border_l_2()
            .border_color(if mailbox.selection_mode().is_some() && mailbox.selected == Some(ix) { accent } else { gpui::transparent_black() })
            .flex()
            .items_start()
            .gap(px(12.))
            // unread marker lives in the row's left gutter so it never mixes with tag dots
            .child(
                div().pt(px(12.)).flex_none().child(
                    div()
                        .relative()
                        .child(contact_avatar(&contact).size(AVATAR))
                        // unread badge: centered on the avatar's rim at 10:30, ringed in the row color
                        .when(unread, |s| {
                            let rim = AVATAR / 2. * (1. - std::f32::consts::FRAC_1_SQRT_2);
                            let size = UNREAD_DOT + 2. * UNREAD_RING;
                            s.child(
                                div()
                                    .absolute()
                                    .left(px(rim - size / 2.))
                                    .top(px(rim - size / 2.))
                                    .size(px(size))
                                    .rounded_full()
                                    .bg(accent)
                                    .border(px(UNREAD_RING))
                                    .border_color(row_bg)
                                    .when(!selected, |s| s.group_hover("thread-row", |s| s.border_color(hover_bg))),
                            )
                        }),
                ),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .flex_1()
                    .min_w_0()
                    .gap(px(2.))
                    .py(px(10.))
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .h(px(17.))
                            .gap(px(6.))
                            .child(
                                div()
                                    .flex_1()
                                    .min_w_0()
                                    .truncate()
                                    .text_size(px(SIZE_SMALL))
                                    .font_weight(WEIGHT_MEDIUM)
                                    .text_color(fg)
                                    .opacity(opacity)
                                    .child(thread.subject.clone()),
                            )
                            .children(tag_dots.into_iter().map(|c| div().size(px(6.)).rounded_full().bg(c).flex_none()))
                            .when(thread.starred, |s| {
                                s.child(Icon::new(IconName::MailStarred).size(12.).color(gpui::rgb(0xf2c94c).into()))
                            }),
                    )
                    .child(
                        div()
                            .flex()
                            .items_end()
                            .gap(px(6.))
                            .text_size(px(SIZE_MINI))
                            .font_weight(WEIGHT_NORMAL)
                            .text_color(theme.text_tertiary)
                            .opacity(0.8)
                            .child(
                                div()
                                    .flex_1()
                                    .min_w_0()
                                    .truncate()
                                    .child(format!("{label} · {snippet}")),
                            )
                            .child(div().flex_none().child(latest.ago.clone())),
                    ),
            ),
    )
}

/// Pointer behaviour shared by both layouts: select on mouse *down* (a click waits for mouse up
/// and feels laggy), right-click selects + reads, then offers the thread menu.
fn row_mouse(row: Stateful<Div>, ix: usize, mailbox: &Entity<Mailbox>) -> Stateful<Div> {
    let (select, menu) = (mailbox.clone(), mailbox.clone());
    row.on_mouse_down(MouseButton::Left, move |_, _, cx| select.update(cx, |m, cx| m.select(ix, cx)))
        .on_mouse_down(MouseButton::Right, move |event, window, cx| {
            // selecting reads the thread; do it now so the menu's labels match its state
            menu.update(cx, |m, cx| {
                // This menu names one thread; never silently act on other marks.
                m.clear_selection(cx);
                m.select(ix, cx);
                m.mark_selected_read(cx);
            });
            let items = context_menus::thread(&menu.read(cx).threads[ix], cx);
            context_menus::open(event.position, items, window, cx);
        })
}

fn render_group(group: Bucket, count: usize, collapsed: bool, theme: &Theme) -> Stateful<Div> {
    div().id(("group", group as usize)).w_full().pt(px(4.)).pb(px(2.)).child(
        div()
            .flex()
            .items_center()
            .h(px(GROUP_HEIGHT))
            .mx(px(8.))
            .pl(px(8.))
            .pr(px(8.))
            .gap(px(8.))
            .rounded(theme.radius.md)
            .bg(theme.bg_group)
            .child(Icon::new(IconName::GroupCaret).size(16.).color(theme.icon_caret).rotate(if collapsed { 0. } else { std::f32::consts::FRAC_PI_2 }))
            .child(div().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_heading).child(group.title()))
            .child(div().text_size(px(SIZE_SMALL)).text_color(theme.text_muted).child(count.to_string())),
    )
}

/// One-line row of the `List` layout. Columns: unread dot · avatar · sender · subject + preview ·
/// tag pills · attachment / star · date.
fn render_wide_row(ix: usize, thread: &Thread, mailbox: &Mailbox, selected: bool, theme: &Theme) -> Stateful<Div> {
    let latest = thread.latest();
    let contact = mailbox.list_contact(thread);
    let unread = thread.unread();
    let (subject_fg, sender_fg) = if unread { (theme.text_primary, theme.text_primary) } else { (theme.text_secondary, theme.text_tertiary) };
    let sender = if mailbox.is_me(&contact) { "me".into() } else if mailbox.is_me(&latest.from) { format!("To {}", contact.name).into() } else { contact.name.clone() };
    let count = thread.messages().count();
    let attachment = thread.messages().any(|m| !m.attachments.is_empty());
    let tags: Vec<_> = thread.tags.iter().filter_map(|t| mailbox.tag(t)).take(3).cloned().collect();

    div().id(("thread", ix)).h(px(WIDE_ROW_HEIGHT)).w_full().child(
        div()
            .h_full()
            .mx(px(8.))
            .pl(px(8.))
            .pr(px(12.))
            .rounded(theme.radius.md)
            .map(|s| if selected { s.bg(theme.bg_selected) } else { s.hover(|s| s.bg(theme.bg_group)) })
            .border_l_2()
            .border_color(if mailbox.selection_mode().is_some() && mailbox.selected == Some(ix) { theme.accent } else { gpui::transparent_black() })
            .flex()
            .items_center()
            .gap(px(10.))
            .text_size(px(SIZE_SMALL))
            .child(div().w(px(UNREAD_DOT)).flex_none().when(unread, |s| s.size(px(UNREAD_DOT)).rounded_full().bg(theme.accent)))
            .child(contact_avatar(&contact).size(20.))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(4.))
                    .w(px(SENDER_WIDTH))
                    .flex_none()
                    .min_w_0()
                    .child(
                        div()
                            .min_w_0()
                            .truncate()
                            .font_weight(if unread { WEIGHT_MEDIUM } else { WEIGHT_NORMAL })
                            .text_color(sender_fg)
                            .child(sender),
                    )
                    .when(count > 1, |s| s.child(div().flex_none().text_color(theme.text_quaternary).child(count.to_string()))),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .flex_1()
                    .min_w_0()
                    .child(div().flex_none().max_w(px(480.)).truncate().font_weight(WEIGHT_MEDIUM).text_color(subject_fg).child(thread.subject.clone()))
                    .child(div().flex_1().min_w_0().truncate().text_color(theme.text_tertiary).child(latest.preview())),
            )
            .children(tags.into_iter().map(|tag| {
                div()
                    .flex()
                    .flex_none()
                    .items_center()
                    .gap(px(6.))
                    .h(px(24.))
                    .px(px(8.))
                    .rounded(theme.radius.pill)
                    .border_hairline()
                    .border_color(theme.border_chip)
                    .text_size(px(SIZE_MINI))
                    .text_color(theme.text_secondary)
                    .child(div().size(px(8.)).rounded_full().bg(tag.color))
                    .child(tag.name.clone())
            }))
            .when(attachment, |s| s.child(Icon::new(IconName::MailAttach).size(14.).color(theme.text_tertiary)))
            .when(thread.starred, |s| s.child(Icon::new(IconName::MailStarred).size(13.).color(gpui::rgb(0xf2c94c).into())))
            .child(div().w(px(DATE_WIDTH)).flex_none().flex().justify_end().text_color(theme.text_tertiary).child(latest.ago.clone())),
    )
}

fn retry_button(id: &'static str) -> ui::SecondaryButton {
    ui::SecondaryButton::new(id, "Try again").on_click(|_, window, cx| window.dispatch_action(Box::new(crate::Retry), cx))
}

/// "Updating…" while a newer answer is on its way; what failed, with a retry, if it didn't come.
fn status_pill(loading: Loading, theme: &Theme) -> impl IntoElement {
    let pill = div()
        .flex()
        .items_center()
        .gap(px(8.))
        .h(px(28.))
        .pl(px(12.))
        .rounded(theme.radius.pill)
        .bg(theme.bg_card)
        .shadow(theme.raised_shadow(false))
        .text_size(px(SIZE_MINI))
        .text_color(theme.text_tertiary);
    let pill = match loading {
        Loading::Pending => pill.pr(px(12.)).child("Updating…"),
        Loading::Failed(message) => pill.pr(px(2.)).max_w(px(360.)).child(div().min_w_0().truncate().child(message)).child(retry_button("retry-refresh")),
    };
    div().absolute().bottom(px(12.)).left_0().right_0().flex().justify_center().child(pill)
}

impl Render for ThreadList {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let visible = self.visible.clone();
        let count = visible.len();
        let empty_query = (count == 0).then(|| self.mailbox.read(cx).query_text.clone());
        // a server-backed list: whether it's on its way, failed, or has more to page in
        let (loading, complete) = self.mailbox.read(cx).remote.as_ref().map_or((None, true), |r| (r.loading.clone(), r.complete));
        let border = cx.theme().border_panel;
        let mailbox = self.mailbox.clone();
        let body: AnyElement = match self.layout {
            Layout::Split => uniform_list("threads", count, move |range, _window, cx| {
                // the last rows are on screen: page in more before they run out
                if !complete && range.end + 30 >= count {
                    let mailbox = mailbox.clone();
                    cx.defer(move |cx| mailbox.update(cx, |m, cx| m.want_more(cx)));
                }
                let started = std::time::Instant::now();
                let mailbox_ref = mailbox.read(cx);
                let theme = cx.theme();
                let marked: std::collections::HashSet<_> = mailbox_ref.selected_ids(1).into_iter().collect();
                let rows = range
                    .filter_map(|row| visible.get(row).copied())
                    .map(|ix| row_mouse(render_row(ix, &mailbox_ref.threads[ix], mailbox_ref, marked.contains(&mailbox_ref.threads[ix].id), theme), ix, &mailbox))
                    .collect();
                crate::trace::slow("list rows", started);
                rows
            })
            .track_scroll(&self.scroll)
            .flex_1()
            .into_any_element(),
            Layout::List => {
                let this = cx.entity().downgrade();
                let marked: std::collections::HashSet<_> = self.mailbox.read(cx).selected_ids(1).into_iter().collect();
                list(self.list_state.clone(), move |row, _window, cx| {
                    let Some(this) = this.upgrade() else { return div().into_any_element() };
                    let (row, collapsed, mailbox) = {
                        let list = this.read(cx);
                        let row = list.rows[row];
                        let collapsed = matches!(row, Row::Group { bucket, .. } if list.collapsed.contains(&bucket));
                        (row, collapsed, list.mailbox.clone())
                    };
                    let theme = cx.theme().clone();
                    match row {
                        Row::Group { bucket, count } => render_group(bucket, count, collapsed, &theme)
                            .on_click(move |_, _, cx| this.update(cx, |list, cx| list.toggle_group(bucket, cx)))
                            .into_any_element(),
                        Row::Thread(ix) => {
                            let m = mailbox.read(cx);
                            let selecting = m.selection_mode().is_some();
                            row_mouse(render_wide_row(ix, &m.threads[ix], m, marked.contains(&m.threads[ix].id), &theme), ix, &mailbox)
                                // a click opens the thread full width (Enter does the same)
                                .on_click(move |_, window, cx| {
                                    if !selecting { window.dispatch_action(Box::new(crate::OpenThread), cx); }
                                })
                                .into_any_element()
                        }
                    }
                })
                .flex_1()
                .into_any_element()
            }
        };
        div()
            .relative()
            .flex()
            .flex_col()
            .map(|s| match self.layout {
                // the workspace decides the width (resizable)
                Layout::Split => s.w_full().border_r_1().border_color(border),
                Layout::List => s.size_full(),
            })
            .h_full()
            .child(self.render_header(cx))
            .child(body)
            .children(empty_query.map(|q| {
                let mono = cx.theme().font_mono;
                let empty = if loading == Some(Loading::Pending) {
                    EmptyState::new(IconName::MailInbox, "Loading…")
                } else if let Some(Loading::Failed(message)) = &loading {
                    EmptyState::new(IconName::MailInbox, "Couldn't load mail")
                        .detail(div().max_w(px(320.)).text_center().child(message.clone()))
                        .hint(retry_button("retry-list"))
                } else if q.trim().is_empty() {
                    EmptyState::new(IconName::MailInbox, "Inbox zero").detail("Nothing left to triage")
                } else {
                    EmptyState::new(IconName::Search, "No threads match")
                        .detail(div().font_family(mono).child(q))
                        .hint(div().flex().items_center().gap(px(4.)).child(Kbd::new("esc")).child("clears the search"))
                };
                // below the header, over the (empty) list
                div().absolute().top(px(88.)).bottom_0().left_0().right_0().child(empty)
            }))
            // over a filled list: a pill at the bottom, so nothing shifts while it comes and goes
            .when(count > 0, |s| s.children(loading.map(|loading| status_pill(loading, cx.theme()))))
    }
}

/// A query typed out: its last token ended by a space (none for an empty query).
fn finished(text: &str) -> SharedString {
    if text.trim().is_empty() || text.ends_with(char::is_whitespace) { text.to_string().into() } else { format!("{text} ").into() }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::query::Query;

    /// A clicked view or tag searches as it is: nothing in it counts as still being typed.
    #[test]
    fn clicked_queries_are_finished() {
        for q in ["tag:newsletter", "from:mara tag:unread", "tag:inbox "] {
            assert!(Query::parse(&finished(q)).clauses.iter().all(|c| !c.prefix), "{q}");
        }
        assert_eq!(finished(""), "");
        assert_eq!(finished("tag:inbox "), "tag:inbox ");
    }
}
