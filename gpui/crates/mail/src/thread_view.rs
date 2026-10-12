//! Thread view:
//! - header bar with breadcrumb-style subject and actions
//! - 24/600 subject, property-chip row for tags
//! - a stacked timeline: message cards (comment card metrics), collapsed one-liners for older
//!   read mail, compact event lines (history entries) and a "show N more" fold
//! - the composer card at the bottom

use std::collections::HashSet;

use gpui::{
    AnyElement, App, Context, Entity, EventEmitter, Focusable, Hsla, IntoElement, Render, SharedString, Subscription, Window, div,
    img, prelude::*, px,
};

use crate::html_render::{HtmlRenderer, HtmlSnapshot, RenderState};
use crate::model::{Attachment, Draft, DraftKind, Event, EventIcon, Loading, Mailbox, MailboxEvent, Message, Thread, ThreadItem, first_name, message_key};
use ui::theme::{ActiveTheme, Theme, typography::*};
use crate::contact_avatar;
use crate::{attachments, context_menus};
use ui::markdown;
use crate::compose::ComposeSeed;
use ui::{ButtonStyle, Icon, IconButton, IconName, Kbd, PrimaryButton, StyledExt, TextArea, TextAreaEvent};

const CONTENT_MAX_WIDTH: f32 = 760.;
/// Own messages are nudged right – enough to scan "who said what", not a chat layout.
const OWN_INDENT: f32 = 32.;

/// What the timeline shows for one slot after folding.
enum Block {
    Card(usize),
    Collapsed(usize),
    Event(usize),
    Fold { hidden: usize },
}

pub struct ThreadView {
    mailbox: Entity<Mailbox>,
    renderer: Entity<HtmlRenderer>,
    schedule: Entity<calendar::model::Schedule>,
    /// text selection across subject and messages (⌘C copies it)
    pub(crate) selection: ui::Selection,
    /// message indices the user expanded/collapsed explicitly (toggles the default)
    toggled: HashSet<usize>,
    show_quoted: HashSet<usize>,
    /// Snapshots are pictures; the original text part remains selectable on demand.
    show_plain: HashSet<usize>,
    show_all: bool,
    /// URL under the pointer inside an HTML message, shown like a browser status bar.
    hovered_link: Option<SharedString>,
    /// inline quick reply under the thread
    reply: Option<ReplyDraft>,
    /// attachment chip under the pointer – Space previews it
    hovered_attachment: Option<Attachment>,
    scroll: gpui::ScrollHandle,
    /// Stable message identity, not a timeline index (events are not messages).
    focused_message: Option<SharedString>,
    message_anchor: gpui::ScrollAnchor,
    reveal_message: bool,
    /// live views of the open thread's expanded HTML messages, by message and theme
    #[cfg(target_os = "macos")]
    webviews: std::collections::HashMap<SharedString, Entity<crate::webview::MailWebView>>,
    /// views asked for, made after the next frame: WebKit's first view takes ~250 ms to set
    /// up, the text part shows meanwhile
    #[cfg(target_os = "macos")]
    webviews_wanted: std::collections::HashSet<SharedString>,
}

/// A message's live view: per rendered HTML and theme (a theme change loads the pristine
/// document again – the dark transform rewrites the DOM's colors).
#[cfg(target_os = "macos")]
fn webview_key(thread: &Thread, ix: usize, dark: bool) -> SharedString {
    format!("{}:{}", message_key(thread, ix), if dark { "dark" } else { "light" }).into()
}

struct ReplyDraft {
    thread_id: SharedString,
    submitting: Option<usize>,
    all: bool,
    editor: Entity<TextArea>,
    _subscription: Subscription,
}

struct Recipients {
    to: Vec<SharedString>,
    cc: Vec<SharedString>,
}

pub enum ThreadViewEvent {
    /// the composer gave up the keyboard; the workspace takes focus back
    ReleaseFocus,
    PopOut(ComposeSeed),
    Downloaded(std::path::PathBuf),
    /// "Open in calendar" on an invitation
    /// an event, and its day – the calendar may not have loaded it yet
    OpenEvent(SharedString, chrono::NaiveDate),
}

impl EventEmitter<ThreadViewEvent> for ThreadView {}

impl ThreadView {
    pub fn new(mailbox: Entity<Mailbox>, renderer: Entity<HtmlRenderer>, schedule: Entity<calendar::model::Schedule>, cx: &mut Context<Self>) -> Self {
        cx.observe(&mailbox, |_, _, cx| cx.notify()).detach();
        // answering an invitation here or in the calendar repaints the card
        cx.observe(&schedule, |_, _, cx| cx.notify()).detach();
        cx.observe(&renderer, |_, _, cx| cx.notify()).detach();
        cx.subscribe(&mailbox, |this, _, event, cx| match event {
            MailboxEvent::SelectionChanged => {
                this.selection.clear();
                this.toggled.clear();
                this.show_quoted.clear();
                this.show_plain.clear();
                this.show_all = false;
                this.hovered_link = None;
                this.focused_message = None;
                this.reveal_message = false;
                this.restore_reply(cx);
                cx.notify();
            }
            MailboxEvent::SendFinished { draft_id, result } if this.reply.as_ref().is_some_and(|draft| draft.submitting == Some(*draft_id)) => {
                if result.is_ok() {
                    this.reply = None;
                    cx.emit(ThreadViewEvent::ReleaseFocus);
                } else if let Some(draft) = &mut this.reply {
                    draft.submitting = None;
                }
                cx.notify();
            }
            _ => {}
        })
        .detach();
        let scroll = gpui::ScrollHandle::new();
        let message_anchor = gpui::ScrollAnchor::for_handle(scroll.clone());
        Self { mailbox, renderer, schedule, selection: ui::Selection::new(), reply: None, hovered_attachment: None, scroll, focused_message: None, message_anchor, reveal_message: false, toggled: HashSet::default(), show_quoted: HashSet::default(), show_plain: HashSet::default(), show_all: false, hovered_link: None, #[cfg(target_os = "macos")] webviews: Default::default(), #[cfg(target_os = "macos")] webviews_wanted: Default::default() }
    }

    pub fn scroll_by(&mut self, count: isize, page: bool, cx: &mut Context<Self>) {
        let step = if page { f32::from(self.scroll.bounds().size.height) / 2. } else { 40. };
        let mut offset = self.scroll.offset();
        offset.y -= px(step * count as f32);
        self.scroll.set_offset(offset);
        cx.notify();
    }

    pub fn message_edge(&mut self, last: bool, cx: &mut Context<Self>) {
        let Some(thread) = self.mailbox.read(cx).selected_thread().filter(|t| !t.partial) else { return };
        let id = if last { thread.messages().next_back() } else { thread.messages().next() }.map(|m| m.id.clone());
        if last && id.is_some() && self.focused_message == id {
            self.reveal_message = false;
            self.scroll.scroll_to_bottom();
        } else {
            self.focused_message = id;
            self.reveal_message = self.focused_message.is_some();
        }
        cx.notify();
    }

    pub fn navigate_message(&mut self, delta: isize, cx: &mut Context<Self>) {
        let Some(thread) = self.mailbox.read(cx).selected_thread().filter(|t| !t.partial) else { return };
        let messages: Vec<_> = thread.messages().collect();
        if messages.is_empty() { return }
        let current = messages.iter().position(|m| Some(&m.id) == self.focused_message.as_ref()).unwrap_or(0);
        let target = (current as isize + delta).clamp(0, messages.len() as isize - 1) as usize;
        self.focused_message = Some(messages[target].id.clone());
        self.reveal_message = true;
        cx.notify();
    }

    /// The hovered attachment, else the latest message's first one.
    pub fn attachment_to_preview(&self, cx: &App) -> Option<Attachment> {
        self.hovered_attachment.clone().or_else(|| {
            let thread = self.mailbox.read(cx).selected_thread()?;
            thread.messages().rev().find_map(|m| m.attachments.first()).cloned()
        })
    }

    /// `;` – expand every message, or if everything is open already, collapse all but the latest.
    pub fn toggle_expand_all(&mut self, cx: &mut Context<Self>) {
        let Some(thread) = self.mailbox.read(cx).selected_thread() else { return };
        let messages = message_indices(thread);
        let last = messages.last().copied();
        let all_open = messages.iter().all(|&ix| self.is_expanded(thread, ix, cx));
        // `toggled` stores deviations from the base state, so derive it from the target state
        let base: Vec<bool> = messages.iter().map(|&ix| self.base_expanded(thread, ix, cx)).collect();
        let toggled = if all_open {
            messages.iter().zip(&base).filter(|&(&ix, &b)| b != (Some(ix) == last)).map(|(&ix, _)| ix).collect()
        } else {
            messages.iter().zip(&base).filter(|&(_, &b)| !b).map(|(&ix, _)| ix).collect()
        };
        let (count, thread_id) = (messages.len(), thread.id.clone());
        self.show_all = !all_open;
        self.toggled = toggled;
        let thread = self.mailbox.read(cx).selected_thread().cloned();
        let open = thread.as_ref().map_or(0, |t| message_indices(t).into_iter().filter(|&ix| self.is_expanded(t, ix, cx)).count());
        crate::trace::note(&format!("{thread_id}: {open}/{count} messages open, fold {}", if self.show_all { "off" } else { "on" }));
        cx.notify();
    }

    /// Open unless toggled: the latest message, events, and whatever was unread *when the
    /// thread was opened* – reading marks it read, but it shouldn't fold away under you.
    fn base_expanded(&self, thread: &Thread, ix: usize, cx: &App) -> bool {
        let was_unread = match &self.mailbox.read(cx).unread_on_open {
            Some((id, unread)) if *id == thread.id => unread.contains(&ix),
            _ => false,
        };
        default_expanded(thread, ix) || was_unread
    }

    fn is_expanded(&self, thread: &Thread, ix: usize, cx: &App) -> bool {
        self.base_expanded(thread, ix, cx) ^ self.toggled.contains(&ix)
    }

    fn blocks(&self, thread: &Thread, cx: &App) -> Vec<Block> {
        let mut blocks: Vec<Block> = thread
            .items
            .iter()
            .enumerate()
            .map(|(ix, item)| match item {
                ThreadItem::Event(_) => Block::Event(ix),
                ThreadItem::Message(_) if self.is_expanded(thread, ix, cx) => Block::Card(ix),
                ThreadItem::Message(_) => Block::Collapsed(ix),
            })
            .collect();

        // Gmail-style fold: keep the first item and the one right before the first open card,
        // replace everything in between with "N more".
        if !self.show_all {
            let first_card = blocks.iter().position(|b| matches!(b, Block::Card(_))).unwrap_or(blocks.len());
            if first_card > 3 {
                let hidden = first_card - 2;
                blocks.splice(1..first_card - 1, [Block::Fold { hidden }]);
            }
        }
        blocks
    }

    fn toggle_message(&mut self, ix: usize, cx: &mut Context<Self>) {
        if !self.toggled.remove(&ix) {
            self.toggled.insert(ix);
        }
        cx.notify();
    }

    fn render_header(&self, thread: &Thread, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let mailbox = self.mailbox.clone();
        div()
            .flex()
            .flex_none()
            .items_center()
            .h(px(44.))
            .px(px(8.))
            .gap(px(4.))
            .border_b_hairline()
            .border_color(theme.border_panel)
            .child(
                div()
                    .min_w_0()
                    .pl(px(10.))
                    .pr(px(4.))
                    .truncate()
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_secondary)
                    .child(thread.subject.clone()),
            )
            .child(
                IconButton::new("star", if thread.starred { IconName::MailStarred } else { IconName::MailStar })
                    .on_click(move |_, _, cx| mailbox.update(cx, |m, cx| m.toggle_star_selected(cx))),
            )
            .child(IconButton::new("thread-options", IconName::More))
            .child(div().flex_1())
            .children(thread.messages().position(|m| Some(&m.id) == self.focused_message.as_ref()).map(|ix| {
                div().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(format!("Message {} of {}", ix + 1, thread.messages().count()))
            }))
            .child(IconButton::new("reply", IconName::MailReply))
            .child(IconButton::new("snooze", IconName::MailSnooze))
            .child(IconButton::new("archive", IconName::MailDone).on_click({
                let mailbox = self.mailbox.clone();
                move |_, _, cx| mailbox.update(cx, |m, cx| m.archive_selected(cx))
            }))
    }

    fn render_tags(&self, thread: &Thread, mailbox: &Mailbox, theme: &Theme) -> impl IntoElement + use<> {
        let participants = thread.people_count();
        let mut chips: Vec<AnyElement> = thread
            .tags
            .iter()
            .filter_map(|t| mailbox.tag(t))
            .map(|tag| {
                property_chip(SharedString::from(format!("tag-chip-{}", tag.name)), theme)
                    .child(div().size(px(8.)).rounded_full().bg(tag.color))
                    .child(div().text_color(theme.text_secondary).child(tag.name.clone()))
                    .into_any_element()
            })
            .collect();
        chips.push(
            property_chip("add-tag-chip", theme)
                .child(Icon::new(IconName::MailTagAdd).size(14.).color(theme.text_tertiary))
                .child("Tag")
                .into_any_element(),
        );
        chips.push(
            property_chip("people-chip", theme)
                .child(Icon::new(IconName::MailUsers).size(14.).color(theme.text_tertiary))
                .child(format!("{participants} people"))
                .into_any_element(),
        );
        div().flex().flex_wrap().items_center().gap(px(6.)).py(px(10.)).children(chips)
    }

    fn render_card(&self, thread: &Thread, ix: usize, message: &Message, mine: bool, theme: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let quoted_open = self.show_quoted.contains(&ix);
        let me = self.mailbox.read(cx).me.name.clone();
        let short = |names: &[SharedString]| {
            names.iter().map(|n| if *n == me { "me".to_string() } else { first_name(n).to_string() }).collect::<Vec<_>>()
        };
        let mut recipients = format!("to {}", short(&message.to).join(", "));
        if !message.cc.is_empty() {
            // long cc lists collapse to the first two names
            let cc = short(&message.cc);
            let shown = cc.iter().take(2).cloned().collect::<Vec<_>>().join(", ");
            recipients += &match cc.len() {
                0..=2 => format!(" · cc {shown}"),
                n => format!(" · cc {shown} +{}", n - 2),
            };
        }
        if !message.bcc.is_empty() {
            recipients += &format!(" · bcc {}", short(&message.bcc).join(", "));
        }
        let sender: SharedString = if mine { "You".into() } else { message.from.name.clone() };

        let header = div()
            .flex()
            .items_center()
            .gap(px(8.))
            .h(px(18.))
            .child(contact_avatar(&message.from).size(18.))
            .when(!message.read, |s| s.child(div().size(px(6.)).rounded_full().bg(theme.accent)))
            .child(
                div()
                    .flex()
                    .items_baseline()
                    .gap(px(5.))
                    .min_w_0()
                    .text_size(px(SIZE_SMALL))
                    .child(div().font_weight(WEIGHT_MEDIUM).text_color(theme.text_primary).child(sender))
                    .child(
                        div()
                            .min_w_0()
                            .truncate()
                            .font_weight(WEIGHT_NORMAL)
                            .text_color(theme.text_meta)
                            .child(recipients),
                    ),
            )
            .child(div().flex_1())
            .child(
                // hover actions
                div()
                    .flex()
                    .gap(px(2.))
                    .invisible()
                    .group_hover("message-card", |s| s.visible())
                    .child(IconButton::new(("msg-reply", ix), IconName::MailReply).size(24.))
                    .child(IconButton::new(("msg-forward", ix), IconName::MailForward).size(24.))
                    .child(IconButton::new(("msg-options", ix), IconName::MailOptions).size(24.)),
            )
            .child(
                div()
                    .flex_none()
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_NORMAL)
                    .text_color(theme.text_meta)
                    .child(message.at.clone()),
            );

        // nothing to show: say why instead of an empty card
        let empty = message.body.trim().is_empty() && message.html.is_none() && message.invite.is_none() && message.invitation.is_none();
        let invitation = thread.tags.iter().any(|t| t == "cal");
        let plain = || {
            if empty {
                return div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .text_size(px(SIZE_SMALL))
                    .text_color(theme.text_tertiary)
                    .child(Icon::new(if invitation { IconName::Calendar } else { IconName::MailEmail }).size(14.).color(theme.text_tertiary))
                    // durian keeps an invitation's event in the calendar, not in the message
                    .child(if invitation { "Calendar invitation – the event is in your calendar" } else { "No text in this message" });
            }
            div()
                .text_size(px(SIZE_REGULAR))
                .line_height(px(24.))
                .font_weight(WEIGHT_NORMAL)
                .text_color(theme.text_body)
                .child(if message.plain {
                    ui::plain::render_selectable(&message.body, theme.font_family, theme, &self.selection)
                } else {
                    markdown::render_selectable(format!("body-{ix}"), &message.body, theme.font_family, theme, &self.selection)
                })
        };
        // the live view, where there is one (else the snapshot renderer's picture)
        #[cfg(target_os = "macos")]
        let live = self.webviews.get(&webview_key(thread, ix, cx.theme().is_dark())).filter(|v| v.read(cx).error.is_none()).cloned();
        #[cfg(not(target_os = "macos"))]
        let live: Option<gpui::AnyView> = None;
        // only say so when something was: blocked images are placeholders in the prepared HTML
        let blocked = message.html.as_ref().is_some_and(|h| h.contains(BLOCKED_MARK));
        let body = match message.html.is_some().then(|| self.renderer.read(cx).state(&message_key(thread, ix)).cloned()) {
            _ if live.is_some() => div()
                .flex()
                .flex_col()
                .gap(px(8.))
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(6.))
                        .text_size(px(SIZE_MINI))
                        .text_color(theme.text_tertiary)
                        .child(Icon::new(IconName::MailEmail).size(12.).color(theme.text_tertiary))
                        .child(html_note(blocked, false)),
                )
                .children(live.map(|view| div().rounded(theme.radius.md).overflow_hidden().child(view)))
                .into_any_element(),
            Some(Some(RenderState::Ready(snapshot))) => {
                let show_plain = self.show_plain.contains(&ix);
                let content = if show_plain { plain().into_any_element() } else { self.render_html(ix, &snapshot, blocked, theme, cx).into_any_element() };
                div().flex().flex_col().gap(px(8.))
                    .child(div().flex().child(ui::SecondaryButton::new(("mail-layout", ix), if show_plain { "Show original layout" } else { "Show plain text" })
                        .on_click(cx.listener(move |this, _, _, cx| {
                            if !this.show_plain.remove(&ix) { this.show_plain.insert(ix); }
                            this.selection.clear();
                            this.hovered_link = None;
                            cx.notify();
                        }))))
                    .child(content).into_any_element()
            }
            // couldn't render (too large, timed out, crashed): the text part is the message
            Some(Some(RenderState::Failed(_))) => plain().into_any_element(),
            // not rendered yet: the text/plain part is readable right away, never a spinner
            Some(_) => div()
                .flex()
                .flex_col()
                .gap(px(10.))
                .child(plain())
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(6.))
                        .text_size(px(SIZE_MINI))
                        .text_color(theme.text_tertiary)
                        .child(Icon::new(IconName::MailEmail).size(12.).color(theme.text_tertiary))
                        .child("Preparing original layout…"),
                )
                .into_any_element(),
            None => plain().into_any_element(),
        };

        let quote_toggle = message.quoted.clone().map(|quoted| {
            div()
                .flex()
                .flex_col()
                .gap(px(8.))
                .child(
                    div()
                        .id(("quote-toggle", ix))
                        .flex()
                        .items_center()
                        .justify_center()
                        .w(px(28.))
                        .h(px(14.))
                        .rounded(theme.radius.sm)
                        // bg_option sits above the card in both modes (bg_group vanishes on dark cards)
                        .bg(theme.bg_option)
                        .hover(|s| s.bg(theme.bg_nav_active))
                        .text_size(px(SIZE_MICRO))
                        .line_height(px(10.))
                        .text_color(theme.text_meta)
                        .child("•••")
                        .on_click(cx.listener(move |this, _, _, cx| {
                            if !this.show_quoted.remove(&ix) {
                                this.show_quoted.insert(ix);
                            }
                            cx.notify();
                        })),
                )
                .when(quoted_open, |s| {
                    s.child(
                        div()
                            .pl(px(12.))
                            .border_l_2()
                            .border_color(theme.border_card)
                            .text_size(px(SIZE_SMALL))
                            .line_height(px(20.))
                            .text_color(theme.text_meta)
                            .child(if message.plain {
                                ui::plain::render_selectable(&quoted, theme.font_family, theme, &self.selection)
                            } else {
                                markdown::render_selectable(format!("quoted-{ix}"), &quoted, theme.font_family, theme, &self.selection)
                            }),
                    )
                })
        });

        let attachments = (!message.attachments.is_empty()).then(|| {
            div().flex().flex_wrap().gap(px(6.)).children(message.attachments.iter().map(|a| {
                let group: SharedString = format!("att-{ix}-{}", a.name).into();
                let (chip, preview, download) = (a.clone(), a.clone(), a.clone());
                property_chip(group.clone(), theme)
                    .group(group.clone())
                    .pr(px(4.))
                    .child(Icon::new(IconName::MailAttach).size(14.).color(theme.text_tertiary))
                    .child(div().text_color(theme.text_secondary).child(a.name.clone()))
                    .child(a.size.clone())
                    // download appears on hover; the chip itself previews (as does Space while hovered)
                    .child(
                        div()
                            .id(SharedString::from(format!("{group}-download")))
                            .flex()
                            .items_center()
                            .justify_center()
                            .size(px(20.))
                            .rounded(theme.radius.pill)
                            .invisible()
                            .group_hover(group, |s| s.visible())
                            .hover(|s| s.bg(theme.bg_nav_hover))
                            .child(Icon::new(IconName::MailDownload).size(12.).color(theme.text_tertiary))
                            .on_click(cx.listener(move |_, _, _, cx| {
                                cx.stop_propagation();
                                if let Some(path) = attachments::download(&download, cx) {
                                    cx.emit(ThreadViewEvent::Downloaded(path));
                                }
                            })),
                    )
                    .on_hover(cx.listener(move |this, hovered: &bool, _, _cx| {
                        if *hovered {
                            this.hovered_attachment = Some(chip.clone());
                        } else if this.hovered_attachment.as_ref() == Some(&chip) {
                            this.hovered_attachment = None;
                        }
                    }))
                    .on_click(move |_, _, cx| attachments::quick_look(&preview, cx))
                    .on_mouse_down(gpui::MouseButton::Right, {
                        let attachment = a.clone();
                        move |event, window, cx| {
                            cx.stop_propagation();
                            context_menus::open(event.position, context_menus::attachment(attachment.clone()), window, cx);
                        }
                    })
            }))
        });

        let invite = crate::invite::resolve(self.schedule.read(cx), message).map(|card| {
            let this = cx.entity().downgrade();
            let (id, day) = (card.0.id.clone(), card.0.start.date());
            crate::invite::render(
                &self.schedule,
                card,
                move |_, cx| {
                    let _ = this.update(cx, |_, cx| cx.emit(ThreadViewEvent::OpenEvent(id.clone(), day)));
                },
                theme,
                cx,
            )
        });
        let (menu_body, menu_sender, menu_email) = (message.body.clone(), message.from.name.clone(), message.from.email.clone());
        card(theme)
            .id(("card", ix))
            .on_mouse_down(gpui::MouseButton::Right, move |event, window, cx| {
                context_menus::open(event.position, context_menus::message(menu_body.clone(), menu_sender.clone(), menu_email.clone(), cx), window, cx);
            })
            .group("message-card")
            .when(mine, |s| s.ml(px(OWN_INDENT)))
            .flex()
            .flex_col()
            .gap(px(10.))
            .p(px(16.))
            .child(div().id(("card-header", ix)).child(header).on_click(cx.listener(move |this, _, _, cx| this.toggle_message(ix, cx))))
            .child(body)
            .children(invite)
            .children(quote_toggle)
            .children(attachments)
            .into_any_element()
    }

    /// The WebKit snapshot at 1:1 CSS px plus a transparent hit-box per link. The surround stays
    /// light in dark mode too: newsletters are designed for light backgrounds (inverting them is
    /// a separate project).
    fn render_html(&self, ix: usize, html: &HtmlSnapshot, blocked: bool, theme: &Theme, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let link_hover = theme.accent.opacity(0.12);
        // only links that may open get a hit-box: no hover promise for `file:`, schemes of
        // other apps, or in-page anchors (which resolve to about:blank#… here)
        let links = html.links.iter().enumerate().filter(|(_, link)| ui::link::allowed(&link.href)).map(|(link_ix, link)| {
            let href = link.href.clone();
            let open = link.href.clone();
            div()
                .id(SharedString::from(format!("link-{ix}-{link_ix}")))
                .absolute()
                .left(px(link.x - 2.))
                .top(px(link.y - 1.))
                .w(px(link.w + 4.))
                .h(px(link.h + 2.))
                .rounded(theme.radius.xs)
                .cursor_pointer()
                .hover(|s| s.bg(link_hover))
                .on_hover(cx.listener(move |this, hovered: &bool, _, cx| {
                    if *hovered {
                        this.hovered_link = Some(href.clone());
                    } else if this.hovered_link.as_ref() == Some(&href) {
                        this.hovered_link = None;
                    }
                    cx.notify();
                }))
                .on_click(move |_, _, cx| {
                    ui::link::open(&open, cx);
                })
                .on_mouse_down(gpui::MouseButton::Right, {
                    let url = link.href.clone();
                    move |event, window, cx| {
                        cx.stop_propagation();
                        context_menus::open(event.position, context_menus::link(url.clone()), window, cx);
                    }
                })
        });

        div()
            .flex()
            .flex_col()
            .gap(px(8.))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.))
                    .text_size(px(SIZE_MINI))
                    .text_color(theme.text_tertiary)
                    .child(Icon::new(IconName::MailEmail).size(12.).color(theme.text_tertiary))
                    .child(html_note(blocked, html.clipped)),
            )
            .child(
                div().flex().justify_center().rounded(theme.radius.md).overflow_hidden().bg(gpui::rgb(0xf4f4f6)).child(
                    div()
                        .relative()
                        .flex_none()
                        .w(px(html.width))
                        .h(px(html.height))
                        .children(html.strips.iter().map(|strip| img(strip.path.clone()).absolute().left_0().top(px(strip.y)).w(px(html.width)).h(px(strip.height))))
                        .children(links),
                ),
            )
    }

    fn render_collapsed(&self, ix: usize, message: &Message, mine: bool, theme: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let snippet = message.preview();
        card(theme)
            .id(("collapsed", ix))
            .when(mine, |s| s.ml(px(OWN_INDENT)))
            .flex()
            .items_center()
            .gap(px(8.))
            .h(px(40.))
            .px(px(16.))
            .hover(|s| s.bg(theme.bg_raised_hover))
            .child(contact_avatar(&message.from).size(18.))
            .child(
                div()
                    .flex_none()
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_primary)
                    .child(if mine { "You".into() } else { message.from.name.clone() }),
            )
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .truncate()
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_NORMAL)
                    .text_color(theme.text_tertiary)
                    .child(snippet),
            )
            .child(
                div().flex_none().text_size(px(SIZE_MINI)).text_color(theme.text_meta).child(message.ago.clone()),
            )
            .on_click(cx.listener(move |this, _, _, cx| this.toggle_message(ix, cx)))
            .into_any_element()
    }

    fn render_fold(&self, hidden: usize, theme: &Theme, cx: &mut Context<Self>) -> AnyElement {
        history_line(
            Icon::new(IconName::MailExpand).size(14.).color(theme.text_tertiary).into_any_element(),
            div().child(format!("Show {hidden} more")).into_any_element(),
            theme,
        )
        .id("fold")
        .rounded(theme.radius.md)
        .hover(|s| s.bg(theme.bg_nav_hover))
        .cursor_pointer()
        .on_click(cx.listener(|this, _, _, cx| {
            this.show_all = true;
            cx.notify();
        }))
        .into_any_element()
    }

    /// Creates the inline editor (optionally with restored draft text) without focusing it.
    fn ensure_reply(&mut self, all: bool, text: Option<SharedString>, cx: &mut Context<Self>) {
        let Some(thread_id) = self.mailbox.read(cx).selected_thread().map(|t| t.id.clone()) else { return };
        let live = self.mailbox.read(cx).remote.is_some();
        if live && self.mailbox.update(cx, |m, cx| m.begin_reply(thread_id.clone(), all, cx)).is_none() { return }
        let all = if live {
            self.mailbox.read(cx).reply_draft(&thread_id).and_then(|d| match d.kind { DraftKind::Reply { all, .. } => Some(all), _ => None }).unwrap_or(all)
        } else { all };
        if self.reply.is_some() {
            if let Some(draft) = self.reply.as_mut() {
                draft.all = all;
            }
            return;
        }
        let Some(thread) = self.mailbox.read(cx).selected_thread() else { return };
        if thread.partial && self.mailbox.read(cx).remote.is_some() && self.mailbox.read(cx).reply_draft(&thread.id).is_none() {
            self.mailbox.update(cx, |_, cx| cx.emit(MailboxEvent::WriteProblem("Wait for the complete thread before replying.".into())));
            return;
        }
        let thread_id = thread.id.clone();
        let submitting = self.mailbox.read(cx).reply_draft(&thread_id).filter(|draft| draft.sending).map(|draft| draft.id);
        let text = text.or_else(|| {
            let mailbox = self.mailbox.read(cx);
            let thread = mailbox.selected_thread()?;
            mailbox.reply_draft(&thread.id).map(|d| d.body.clone())
        });
        let editor = cx.new(|cx| {
            let mut area = TextArea::new("Write a reply…", 3, cx).markdown(true);
            if let Some(text) = text {
                area.set_text(text, cx);
            }
            area
        });
        let subscription = cx.subscribe(&editor, |this, editor, event, cx| match event {
            TextAreaEvent::Submit => this.send_reply(cx),
            TextAreaEvent::Cancel => {
                // Esc: the draft is already saved; drop an empty composer, give the keyboard back
                if editor.read(cx).is_empty() {
                    this.reply = None;
                }
                cx.emit(ThreadViewEvent::ReleaseFocus);
                cx.notify();
            }
            // autosave per thread
            TextAreaEvent::Changed => {
                let body = editor.read(cx).text().clone();
                let all = this.reply.as_ref().is_none_or(|d| d.all);
                if let Some(thread_id) = this.reply.as_ref().map(|draft| draft.thread_id.clone()) {
                    if let Some(frozen) = this.mailbox.read(cx).reply_draft(&thread_id).filter(|d| d.sending || d.queued.is_some() || d.wire.extra.contains_key(crate::durian::writes::PENDING_SEND)) {
                        let original = frozen.body.clone();
                        if body != original { editor.update(cx, |editor, cx| editor.set_text(original, cx)); }
                        return;
                    }
                    this.mailbox.update(cx, |m, cx| m.save_reply_draft(thread_id, all, body, cx));
                }
                cx.notify();
            }
        });
        self.reply = Some(ReplyDraft { thread_id, submitting, all, editor, _subscription: subscription });
    }

    /// Restore the saved reply draft of the newly selected thread, if any.
    fn restore_reply(&mut self, cx: &mut Context<Self>) {
        self.reply = None;
        let mailbox = self.mailbox.read(cx);
        let Some(thread) = mailbox.selected_thread() else { return };
        if let Some(Draft { kind: DraftKind::Reply { all, .. }, body, .. }) = mailbox.reply_draft(&thread.id).filter(|draft| draft.queued.is_none()).cloned() {
            self.ensure_reply(all, Some(body), cx);
        }
    }

    /// `r` / `a` / click: open the inline reply under the thread and focus it.
    pub fn open_reply(&mut self, all: bool, window: &mut Window, cx: &mut Context<Self>) {
        if self.mailbox.read(cx).selected_thread().is_none() {
            return;
        }
        self.ensure_reply(all, None, cx);
        if let Some(draft) = &self.reply {
            let frozen = self.mailbox.read(cx).reply_draft(&draft.thread_id).is_some_and(|d| d.sending || d.queued.is_some() || d.wire.extra.contains_key(crate::durian::writes::PENDING_SEND));
            if frozen { cx.emit(ThreadViewEvent::ReleaseFocus); }
            else { draft.editor.read(cx).focus_handle(cx).focus(window, cx); }
        }
        // the composer sits under the thread – bring it into view
        self.scroll.scroll_to_bottom();
        cx.notify();
    }

    fn recipients(&self, all: bool, cx: &App) -> Option<Recipients> {
        let mailbox = self.mailbox.read(cx);
        let thread = mailbox.selected_thread()?;
        if mailbox.remote.is_some() {
            if let Some(draft) = mailbox.reply_draft(&thread.id).filter(|draft| !draft.wire.to.is_empty()) {
                return Some(Recipients { to: draft.wire.to.iter().cloned().map(Into::into).collect(), cc: draft.wire.cc.iter().cloned().map(Into::into).collect() });
            }
            let latest = thread.latest();
            let own = |raw: &str| {
                let contact = crate::durian::parse_address(raw);
                mailbox.is_me(&contact) || mailbox.accounts.iter().any(|a| a.aliases.iter().any(|alias| alias.eq_ignore_ascii_case(&contact.email)))
            };
            let mut seen = HashSet::new();
            let mut collect = |raw: &[SharedString]| raw.iter().filter(|address| {
                !own(address) && seen.insert(crate::durian::parse_address(address).email.to_lowercase())
            }).cloned().collect::<Vec<_>>();
            let sender: SharedString = format!("{} <{}>", latest.from.name, latest.from.email).into();
            let mine = own(&sender);
            let mut to = if mine { collect(&latest.to) } else { collect(&[sender]) };
            if all && !mine { to.extend(collect(&latest.to)); }
            let cc = if all { collect(&latest.cc) } else { Vec::new() };
            return Some(Recipients { to, cc });
        }
        let latest = thread.latest();
        let me = &mailbox.me.name;
        let mut to: Vec<SharedString> = Vec::new();
        let push = |list: &mut Vec<SharedString>, name: &SharedString| {
            if name != me && !list.contains(name) {
                list.push(name.clone());
            }
        };
        // replying to my own last message goes to its recipients
        if latest.from.name == *me {
            latest.to.iter().for_each(|n| push(&mut to, n));
        } else {
            push(&mut to, &latest.from.name);
            if all {
                latest.to.iter().for_each(|n| push(&mut to, n));
            }
        }
        let mut cc = Vec::new();
        if all {
            latest.cc.iter().filter(|n| !to.contains(*n)).for_each(|n| push(&mut cc, n));
        }
        Some(Recipients { to, cc })
    }

    fn send_reply(&mut self, cx: &mut Context<Self>) {
        let Some(draft) = &self.reply else { return };
        if draft.submitting.is_some() { return; }
        let body = draft.editor.read(cx).text().trim().to_string();
        if body.is_empty() {
            return;
        }
        let Some(Recipients { to, cc }) = self.recipients(draft.all, cx) else { return };
        let all = draft.all;
        if self.mailbox.read(cx).remote.is_some() {
            let thread_id = draft.thread_id.clone();
            let submitting = self.mailbox.update(cx, |m, cx| {
                m.save_reply_draft(thread_id.clone(), all, body.into(), cx);
                let id = m.reply_draft(&thread_id)?.id;
                m.submit_draft(id, Some(all), cx).then_some(id)
            });
            if let Some(draft) = &mut self.reply { draft.submitting = submitting; }
            // The editor and encrypted recovery draft remain until enqueue is acknowledged.
            if submitting.is_some() { cx.emit(ThreadViewEvent::ReleaseFocus); }
            cx.notify();
            return;
        }
        let thread_id = self.mailbox.read(cx).selected_thread().map(|t| t.id.clone());
        self.mailbox.update(cx, |m, cx| {
            m.send_reply(to, cc, body.into(), cx);
            if let Some(thread_id) = thread_id {
                m.save_reply_draft(thread_id, all, SharedString::default(), cx); // sent: drop the draft
            }
        });
        self.reply = None;
        cx.emit(ThreadViewEvent::ReleaseFocus);
        cx.notify();
    }

    /// Hand the draft to the full composer (more recipients, subject, formatting).
    fn pop_out(&mut self, cx: &mut Context<Self>) {
        let Some(draft) = &self.reply else { return };
        if draft.submitting.is_some() { return; }
        let Some(Recipients { to, cc }) = self.recipients(draft.all, cx) else { return };
        let Some(thread) = self.mailbox.read(cx).selected_thread() else { return };
        let subject = if thread.subject.starts_with("Re:") { thread.subject.to_string() } else { format!("Re: {}", thread.subject) };
        let thread_id = thread.id.clone();
        let account = Some(thread.account.clone());
        let body = draft.editor.read(cx).text().clone();
        let all = draft.all;
        let existing = self.mailbox.read(cx).reply_draft(&thread_id).map(|draft| draft.id);
        let draft_id = self.mailbox.update(cx, |m, cx| {
            m.save_new_draft(existing, DraftKind::Reply { thread_id, all }, body.clone(), cx)
        });
        if draft_id.is_none() { return; }
        // Both editors address the same draft. Never delete before the recipient accepts it.
        cx.emit(ThreadViewEvent::PopOut(ComposeSeed { draft_id, account, from: None, to, cc, subject: subject.into(), body }));
        self.reply = None;
        cx.notify();
    }

    fn render_composer(&self, thread: &Thread, theme: &Theme, cx: &mut Context<Self>) -> AnyElement {
        let Some(draft) = &self.reply else {
            // collapsed: a quiet placeholder; clicking it means "reply all"
            let to = thread.senders().iter().filter(|n| **n != self_name(thread)).map(|n| first_name(n).to_string()).collect::<Vec<_>>().join(", ");
            return card(theme)
                .id("reply-placeholder")
                .flex()
                .flex_col()
                .gap(px(8.))
                .px(px(16.))
                .py(px(12.))
                .cursor_text()
                .child(div().text_size(px(SIZE_REGULAR)).font_weight(WEIGHT_NORMAL).text_color(theme.text_placeholder).child(format!("Reply to {to}…")))
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(6.))
                        .child(div().flex_1())
                        .child(IconButton::new("attach", IconName::MailAttach).size(24.))
                        .child(IconButton::new("send", IconName::MailSubmit).size(24.).style(ButtonStyle::Raised)),
                )
                .on_click(cx.listener(|this, _, window, cx| this.open_reply(true, window, cx)))
                .into_any_element();
        };

        if let Some(saved) = self.mailbox.read(cx).reply_draft(&draft.thread_id).filter(|saved| saved.sending || saved.wire.extra.contains_key(crate::durian::writes::PENDING_SEND)) {
            let pending = saved.sending;
            return card(theme).p(px(16.)).flex().flex_col().gap(px(12.))
                .child(if pending { "Queuing reply… Your draft is kept." } else { "Delivery is unconfirmed. Retry sends the original message, not a new copy." })
                .child(div().text_color(theme.text_body).child(draft.editor.read(cx).text().clone()))
                .when(!pending && saved.queued.is_none(), |s| s.child(PrimaryButton::new("reply-retry", "Retry original send").on_click(cx.listener(|this, _, _, cx| this.send_reply(cx)))))
                .into_any_element();
        }

        let all = draft.all;
        let recipients = self.recipients(all, cx).unwrap_or(Recipients { to: Vec::new(), cc: Vec::new() });
        let short = |names: &[SharedString]| names.iter().map(|n| first_name(n).to_string()).collect::<Vec<_>>();
        let mut line = format!("to {}", short(&recipients.to).join(", "));
        if !recipients.cc.is_empty() {
            let cc = short(&recipients.cc);
            line += &format!(" · cc {}{}", cc.iter().take(2).cloned().collect::<Vec<_>>().join(", "), if cc.len() > 2 { format!(" +{}", cc.len() - 2) } else { String::new() });
        }
        let can_send = !draft.editor.read(cx).is_empty();

        card(theme)
            .flex()
            .flex_col()
            .gap(px(10.))
            .px(px(16.))
            .py(px(12.))
            // the open composer reads as "active": accent ring instead of the hairline
            .border_color(theme.accent.opacity(0.45))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.))
                    .text_size(px(SIZE_MINI))
                    .child(Icon::new(IconName::MailReply).size(12.).color(theme.text_tertiary))
                    .child(
                        div()
                            .id("reply-mode")
                            .flex_none()
                            .font_weight(WEIGHT_MEDIUM)
                            .text_color(theme.text_secondary)
                            .hover(|s| s.underline())
                            .child(if all { "Reply all" } else { "Reply" })
                            .on_click(cx.listener(|this, _, _, cx| {
                                if let Some(all) = this.reply.as_ref().map(|d| !d.all) { this.ensure_reply(all, None, cx); }
                                cx.notify();
                            })),
                    )
                    .child(div().min_w_0().truncate().text_color(theme.text_tertiary).child(line))
                    .child(div().flex_1())
                    .child(
                        IconButton::new("reply-pop-out", IconName::PanelRight)
                            .size(24.)
                            .on_click(cx.listener(|this, _, _, cx| this.pop_out(cx))),
                    ),
            )
            .child(
                div()
                    .text_size(px(SIZE_REGULAR))
                    .line_height(px(24.))
                    .font_weight(WEIGHT_NORMAL)
                    .text_color(theme.text_body)
                    .child(draft.editor.clone()),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .child(IconButton::new("reply-attach", IconName::MailAttach).size(24.))
                    .child(div().flex_1())
                    .child(div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child("⌘↵ to send"))
                    .child(
                        div().when(!can_send, |s| s.opacity(0.5)).child(
                            PrimaryButton::new("reply-send", "Send").on_click(cx.listener(|this, _, _, cx| this.send_reply(cx))),
                        ),
                    ),
            )
            .into_any_element()
    }
}

fn self_name(thread: &Thread) -> SharedString {
    // the fixture's own address; good enough for a placeholder
    thread.messages().find(|m| m.from.email.starts_with("julian@")).map(|m| m.from.name.clone()).unwrap_or_default()
}

fn message_indices(thread: &Thread) -> Vec<usize> {
    thread.items.iter().enumerate().filter(|(_, i)| matches!(i, ThreadItem::Message(_))).map(|(ix, _)| ix).collect()
}

/// Latest message and anything unread start open; older read mail starts as a one-liner.
fn default_expanded(thread: &Thread, ix: usize) -> bool {
    let last_message = thread.items.iter().rposition(|i| matches!(i, ThreadItem::Message(_)));
    match &thread.items[ix] {
        ThreadItem::Message(m) => !m.read || Some(ix) == last_message,
        ThreadItem::Event(_) => true,
    }
}

/// Card with hairline border + soft shadow (message card / composer).
fn card(theme: &Theme) -> gpui::Div {
    div()
        .bg(theme.bg_card)
        .border_hairline()
        .border_color(theme.border_card)
        .rounded(theme.radius.md)
        .shadow(theme.panel_shadow())
}

/// Property chip: raised pill, 12px text, leading glyph.
fn property_chip(id: impl Into<gpui::ElementId>, theme: &Theme) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .flex()
        .items_center()
        .gap(px(6.))
        .h(px(26.))
        .px(px(10.))
        .rounded(theme.radius.pill)
        .bg(theme.bg_raised)
        .shadow(theme.raised_shadow(false))
        .text_size(px(SIZE_MINI))
        .font_weight(WEIGHT_NORMAL)
        .text_color(theme.text_tertiary)
        .hover(|s| s.bg(theme.bg_raised_hover))
}

/// History-entry row: 36px icon column, 12px tertiary text.
fn history_line(icon: AnyElement, text: AnyElement, theme: &Theme) -> gpui::Div {
    div()
        .flex()
        .items_center()
        .min_h(px(29.))
        .py(px(5.))
        .child(div().flex().flex_none().justify_center().w(px(36.)).child(icon))
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(4.))
                .text_size(px(SIZE_MINI))
                .font_weight(WEIGHT_NORMAL)
                .text_color(theme.text_tertiary)
                .child(text),
        )
}

fn render_event(event: &Event, theme: &Theme) -> AnyElement {
    let icon = match event.icon {
        EventIcon::Label => IconName::MailLabel,
        EventIcon::Clock => IconName::MailClock,
        EventIcon::Forward => IconName::MailForward,
    };
    let bold = |s: SharedString, color: Hsla| div().font_weight(WEIGHT_MEDIUM).text_color(color).child(s);
    history_line(
        Icon::new(icon).size(14.).color(theme.text_tertiary).into_any_element(),
        div()
            .flex()
            .gap(px(4.))
            .child(bold(event.actor.clone(), theme.text_tertiary))
            .child(event.text.clone())
            .child(bold(event.target.clone(), theme.text_tertiary))
            .child("·")
            .child(event.ago.clone())
            .into_any_element(),
        theme,
    )
    .into_any_element()
}

/// In place of the messages while the thread loads: a quiet line, or what went wrong.
fn render_pending(loading: Option<Loading>, theme: &Theme) -> AnyElement {
    let line = div().flex().items_center().gap(px(10.)).py(px(8.)).text_size(px(SIZE_SMALL)).text_color(theme.text_tertiary);
    match loading {
        Some(Loading::Failed(message)) => line
            .child(div().min_w_0().child(format!("Couldn't load this conversation: {message}")))
            .child(ui::SecondaryButton::new("retry-thread", "Try again").on_click(|_, window, cx| window.dispatch_action(Box::new(crate::Retry), cx)))
            .into_any_element(),
        // asked for, or about to be
        _ => line.child("Loading conversation…").into_any_element(),
    }
}

impl Render for ThreadView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        crate::trace::mark("thread render");
        let started = std::time::Instant::now();
        let element = self.render_thread(_window, cx);
        let bytes: usize = self.mailbox.read(cx).selected_thread().map_or(0, |t| t.messages().map(|m| m.body.len()).sum());
        crate::trace::slow(&format!("thread render ({bytes} body bytes)"), started);
        element
    }
}

impl ThreadView {
    /// Views for the expanded HTML messages that don't have one; views no longer shown go
    /// (their native views with them).
    #[cfg(target_os = "macos")]
    fn sync_webviews(&mut self, thread: &Thread, blocks: &[Block], window: &mut Window, cx: &mut Context<Self>) {
        if !crate::webview::available() {
            return;
        }
        let (dark, background) = (cx.theme().is_dark(), cx.theme().bg_card);
        let mut used: std::collections::HashSet<SharedString> = Default::default();
        for block in blocks {
            let Block::Card(ix) = block else { continue };
            let Some(ThreadItem::Message(Message { html: Some(html), .. })) = thread.items.get(*ix) else { continue };
            let key = webview_key(thread, *ix, dark);
            if !self.webviews.contains_key(&key) && !self.webviews_wanted.contains(&key) {
                let (html, key, this) = (html.clone(), key.clone(), cx.entity().downgrade());
                // for the right-click menu: what the message offers besides the page
                let ThreadItem::Message(m) = &thread.items[*ix] else { continue };
                let (body, sender, email) = (m.body.clone(), m.from.name.clone(), m.from.email.clone());
                window.on_next_frame(move |window, cx| {
                    let _ = this.update(cx, |this, cx| {
                        // still shown? (the thread may have changed meanwhile)
                        if this.webviews_wanted.contains(&key) && !this.webviews.contains_key(&key) {
                            let view = cx.new(|cx| crate::webview::MailWebView::new(html, dark, background, window, cx));
                            cx.subscribe_in(&view, window, move |this, _, event, window, cx| match event {
                                crate::webview::MailWebViewEvent::Hover(url) => {
                                    if this.hovered_link != *url {
                                        this.hovered_link = url.clone();
                                        cx.notify();
                                    }
                                }
                                crate::webview::MailWebViewEvent::ContextMenu { position, link, selection } => {
                                    let items = context_menus::html_body(selection.clone(), link.clone(), body.clone(), sender.clone(), email.clone(), cx);
                                    context_menus::open(*position, items, window, cx);
                                }
                            })
                            .detach();
                            this.webviews.insert(key, view);
                            cx.notify();
                        }
                    });
                });
            }
            used.insert(key);
        }
        self.webviews.retain(|key, _| used.contains(key));
        self.webviews_wanted = used;
    }

    fn render_thread(&mut self, window: &mut Window, cx: &mut Context<Self>) -> AnyElement {
        let mailbox = self.mailbox.read(cx);
        let Some(thread) = mailbox.selected_thread().cloned() else {
            // an empty list explains itself; with threads listed, say how to pick one
            if mailbox.visible().next().is_none() {
                return div().size_full().into_any_element();
            }
            return ui::EmptyState::new(IconName::MailEmail, "No thread selected")
                .hint(div().flex().items_center().gap(px(4.))
                    .children(crate::commands::keys_for(&crate::SelectNext, &[crate::keymaps::Context::List, crate::keymaps::Context::Thread], cx).into_iter().map(Kbd::new))
                    .children(crate::commands::keys_for(&crate::SelectPrev, &[crate::keymaps::Context::List, crate::keymaps::Context::Thread], cx).into_iter().map(Kbd::new))
                    .child("Select a thread from the list"))
                .into_any_element();
        };
        if !thread.partial && self.focused_message.is_some() {
            if let Some(ix) = thread.items.iter().position(|item| matches!(item, ThreadItem::Message(m) if Some(&m.id) == self.focused_message.as_ref())) {
                // Reveal on navigation, not on every repaint: clicking a card or
                // collapsing all must still work after keyboard navigation.
                if self.reveal_message {
                    self.show_all = true;
                    if self.base_expanded(&thread, ix, cx) { self.toggled.remove(&ix); }
                    else { self.toggled.insert(ix); }
                }
            } else {
                self.focused_message = None;
                self.reveal_message = false;
            }
        }
        // selectable blocks register in reading order: the subject first, then the messages
        self.selection.begin();
        let subject_text = ui::SelectableText::plain(&self.selection, thread.subject.clone(), cx.theme().font_family, WEIGHT_SEMIBOLD, cx.theme().text_primary);
        let mailbox = self.mailbox.read(cx);
        let tags = self.render_tags(&thread, mailbox, cx.theme());
        let blocks = self.blocks(&thread, cx);
        #[cfg(target_os = "macos")]
        self.sync_webviews(&thread, &blocks, window, cx);
        #[cfg(not(target_os = "macos"))]
        let _ = window;
        let header = self.render_header(&thread, cx);

        // clone once: the card renderers need `cx` mutably for their listeners
        let theme = cx.theme().clone();
        let timeline: Vec<AnyElement> = blocks
            .into_iter()
            .map(|block| match block {
                Block::Event(ix) => match &thread.items[ix] {
                    ThreadItem::Event(e) => render_event(e, &theme),
                    ThreadItem::Message(_) => unreachable!(),
                },
                Block::Card(ix) | Block::Collapsed(ix) => {
                    let ThreadItem::Message(m) = &thread.items[ix] else { unreachable!() };
                    let mine = self.mailbox.read(cx).is_me(&m.from);
                    let content = if matches!(block, Block::Card(_)) {
                        self.render_card(&thread, ix, m, mine, &theme, cx)
                    } else {
                        self.render_collapsed(ix, m, mine, &theme, cx)
                    };
                    let focused = self.focused_message.as_ref() == Some(&m.id);
                    div().id(SharedString::from(format!("message-focus:{}", m.id))).anchor_scroll(focused.then(|| self.message_anchor.clone()))
                        .rounded(theme.radius.md).border_l_2().border_color(if focused { theme.accent } else { gpui::transparent_black() })
                        .child(content).into_any_element()
                }
                Block::Fold { hidden } => self.render_fold(hidden, &theme, cx),
            })
            .collect();

        // only the list row so far: the conversation is on its way (or failed to come)
        let (timeline, composer) = if thread.partial {
            let loading = self.mailbox.read(cx).thread_loading(&thread.id).cloned();
            (vec![render_pending(loading, &theme)], None)
        } else if self.mailbox.read(cx).read_only() {
            // nothing can be sent yet: no reply box to type into
            (timeline, None)
        } else {
            (timeline, Some(self.render_composer(&thread, &theme, cx)))
        };
        let status = self.hovered_link.clone().map(|url| {
            div()
                .absolute()
                .left(px(8.))
                .bottom(px(8.))
                .max_w(px(520.))
                .px(px(8.))
                .py(px(3.))
                .rounded(theme.radius.sm)
                .bg(theme.bg_card)
                .shadow(theme.raised_shadow(false))
                .text_size(px(SIZE_MINI))
                .text_color(theme.text_secondary)
                .truncate()
                .child(url)
        });
        if self.reveal_message && !thread.partial {
            self.reveal_message = false;
            self.message_anchor.scroll_to(window, cx);
        }

        // size_full, not flex_1: as a cached view our parent is a plain box, not a flex row
        div()
            .relative()
            .flex()
            .flex_col()
            .size_full()
            .child(header)
            .child(
                div().id("thread-scroll").track_scroll(&self.scroll).flex_1().min_h_0().overflow_y_scroll().child(
                    div()
                        .w_full()
                        .max_w(px(CONTENT_MAX_WIDTH))
                        .mx_auto()
                        .px(px(24.))
                        .pt(px(28.))
                        .pb(px(48.))
                        .flex()
                        .flex_col()
                        .child(
                            div()
                                .text_size(px(SIZE_TITLE))
                                .line_height(px(32.))
                                .font_weight(WEIGHT_SEMIBOLD)
                                .text_color(theme.text_primary)
                                .child(subject_text),
                        )
                        .child(tags)
                        .child(div().h(px(1.)).my(px(12.)).border_b_hairline().border_color(theme.border_panel))
                        .child(div().flex().flex_col().gap(px(8.)).children(timeline))
                        .children(composer.map(|c| div().mt(px(16.)).child(c))),
                ),
            )
            // last child: absolutely positioned and painted above the scroll content
            .children(status)
            .into_any_element()
    }
}

/// What `html_doc` puts where it blocked a remote image.
const BLOCKED_MARK: &str = "class=\"durian-blocked\"";

/// The line above an HTML message: what was held back, if anything.
fn html_note(blocked: bool, clipped: bool) -> &'static str {
    match (blocked, clipped) {
        (false, false) => "HTML message",
        (true, false) => "HTML message · remote images blocked",
        (false, true) => "HTML message · very long, the rest is in the text version",
        (true, true) => "HTML message · remote images blocked · very long, the rest is in the text version",
    }
}
