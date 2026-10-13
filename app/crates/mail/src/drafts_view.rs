//! Drafts page: sections of 376×140 cards with a
//! 13/500 title, 12px age, a hover "discard" button and a clipped preview. Reply drafts show the
//! thread they belong to in a bordered context box ("Replying to …").

use gpui::{Context, Entity, EventEmitter, FocusHandle, IntoElement, Render, SharedString, Window, div, prelude::*, px};

use crate::model::{Draft, DraftKind, Mailbox, first_name};
use ui::markdown;
use ui::theme::{ActiveTheme, Theme, typography::*};
use ui::{ConfirmDialog, Icon, IconButton, IconName, MenuItem, StyledExt};

pub enum DraftsViewEvent {
    Open(usize),
}

pub struct DraftsView {
    mailbox: Entity<Mailbox>,
    confirm_discard: Option<usize>,
    confirm_focus: FocusHandle,
}

impl EventEmitter<DraftsViewEvent> for DraftsView {}

impl DraftsView {
    pub fn new(mailbox: Entity<Mailbox>, cx: &mut Context<Self>) -> Self {
        cx.observe(&mailbox, |_, _, cx| cx.notify()).detach();
        Self { mailbox, confirm_discard: None, confirm_focus: cx.focus_handle() }
    }

    fn card(&self, draft: &Draft, theme: &Theme, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let mailbox = self.mailbox.read(cx);
        let id = draft.id;
        let (icon, title, context, preview): (IconName, SharedString, Option<SharedString>, SharedString) = match &draft.kind {
            DraftKind::New { to, subject, .. } => {
                let to = to.iter().map(|n| first_name(n).to_string()).collect::<Vec<_>>().join(", ");
                let title = if subject.trim().is_empty() { "(no subject)".into() } else { subject.clone() };
                (IconName::Compose, title, (!to.is_empty()).then(|| format!("To {to}").into()), draft.body.clone())
            }
            DraftKind::Reply { thread_id, all } => {
                let thread = mailbox.threads.iter().find(|t| t.id == *thread_id);
                let subject = thread.map(|t| t.subject.clone()).unwrap_or_else(|| "Deleted thread".into());
                let sender = thread.map(|t| first_name(&t.latest().from.name).to_string()).unwrap_or_default();
                let context = format!("{} {sender}", if *all { "Replying all to" } else { "Replying to" });
                (IconName::MailReply, subject, Some(context.into()), draft.body.clone())
            }
        };
        let is_reply = matches!(draft.kind, DraftKind::Reply { .. });

        div()
            .id(("draft", id))
            .group("draft-card")
            .relative()
            .w(px(376.))
            .h(px(140.))
            .flex()
            .flex_col()
            .gap(px(12.))
            .pt(px(12.))
            .px(px(16.))
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.md)
            .shadow(theme.panel_shadow())
            .overflow_hidden()
            .cursor_pointer()
            .hover(|s| s.border_color(theme.border_chip))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .h(px(28.))
                    .child(Icon::new(icon).color(theme.text_tertiary))
                    .child(div().min_w_0().truncate().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_primary).child(title))
                    .child(div().flex_none().text_size(px(SIZE_MINI)).text_color(theme.text_meta).child(draft.age()))
                    .child(div().flex_1())
                    .child(
                        div().invisible().group_hover("draft-card", |s| s.visible()).child(
                            IconButton::new(("discard-draft", id), IconName::MailTrash)
                                .tooltip("Discard draft")
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    cx.stop_propagation();
                                    this.confirm_discard = Some(id);
                                    this.confirm_focus.focus(window, cx);
                                    cx.notify();
                                })),
                        ),
                    ),
            )
            .map(|s| {
                if is_reply {
                    // bordered context box that runs off the bottom edge
                    s.child(
                        div()
                            .flex()
                            .flex_col()
                            .flex_1()
                            .mb(px(-16.))
                            .border_hairline()
                            .border_color(theme.border_card)
                            .rounded_t(theme.radius.md)
                            .child(
                                div()
                                    .flex()
                                    .items_center()
                                    .gap(px(8.))
                                    .px(px(12.))
                                    .py(px(8.))
                                    .border_b_hairline()
                                    .border_color(theme.border_card)
                                    .text_size(px(SIZE_SMALL))
                                    .text_color(theme.text_meta)
                                    .child(Icon::new(IconName::MailReply).color(theme.text_meta))
                                    .child(context.unwrap_or_default()),
                            )
                            .child(div().px(px(12.)).py(px(10.)).text_size(px(SIZE_SMALL)).text_color(theme.text_body).child(markdown::render(format!("draft-{id}"), &preview, theme.font_family, theme))),
                    )
                } else {
                    s.children(context.map(|c| div().mt(px(-8.)).text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(c)))
                        .child(div().text_size(px(SIZE_SMALL)).line_height(px(19.)).text_color(theme.text_meta).child(markdown::render(format!("draft-{id}"), &preview, theme.font_family, theme)))
                }
            })
            // the preview runs off the card: fade it out instead of cutting a line in half
            .child(
                div().absolute().left_0().right_0().bottom_0().h(px(36.)).bg(gpui::linear_gradient(
                    180.,
                    gpui::linear_color_stop(theme.bg_card.opacity(0.), 0.),
                    gpui::linear_color_stop(theme.bg_card, 0.85),
                )),
            )
            .on_click(cx.listener(move |_, _, _, cx| cx.emit(DraftsViewEvent::Open(id))))
            .on_mouse_down(gpui::MouseButton::Right, {
                let this = cx.entity();
                move |event, window, cx| {
                    let (open, discard) = (this.clone(), this.clone());
                    let items = vec![
                        MenuItem::handler("Open draft", IconName::Compose, move |_, cx| open.update(cx, |_, cx| cx.emit(DraftsViewEvent::Open(id)))),
                        MenuItem::Separator,
                        MenuItem::handler("Discard draft…", IconName::MailTrash, move |window, cx| {
                            discard.update(cx, |this, cx| {
                                this.confirm_discard = Some(id);
                                this.confirm_focus.focus(window, cx);
                                cx.notify();
                            })
                        }),
                    ];
                    crate::context_menus::open(event.position, items, window, cx);
                }
            })
    }
}

impl Render for DraftsView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let drafts = self.mailbox.read(cx).drafts.clone();
        let (mut new, mut replies) = (Vec::new(), Vec::new());
        for draft in drafts.iter().rev() {
            let card = self.card(draft, &theme, cx).into_any_element();
            match draft.kind {
                DraftKind::New { .. } => new.push(card),
                DraftKind::Reply { .. } => replies.push(card),
            }
        }
        let section = |title: &'static str, cards: Vec<gpui::AnyElement>| {
            (!cards.is_empty()).then(|| {
                div()
                    .flex()
                    .flex_col()
                    .gap(px(12.))
                    .mb(px(28.))
                    .child(div().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_tertiary).child(title))
                    .child(div().flex().flex_wrap().gap(px(12.)).children(cards))
            })
        };
        let empty = drafts.is_empty();

        div()
            .relative()
            .size_full()
            .flex()
            .flex_col()
            .child(
                div()
                    .flex()
                    .flex_none()
                    .items_center()
                    .h(px(44.))
                    .px(px(18.))
                    .border_b_hairline()
                    .border_color(theme.border_panel)
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_secondary)
                    .child("Drafts"),
            )
            .child(
                div()
                    .id("drafts-scroll")
                    .flex_1()
                    .min_h_0()
                    .overflow_y_scroll()
                    .px(px(18.))
                    .pt(px(24.))
                    .children(section("New messages", new))
                    .children(section("Replies", replies))
                    .when(empty, |s| {
                        s.child(div().pt(px(80.)).flex().justify_center().text_size(px(SIZE_SMALL)).text_color(theme.text_tertiary).child("No drafts"))
                    }),
            )
            .when_some(self.confirm_discard, |s, id| {
                let this = cx.entity();
                let (discard, cancel) = (this.clone(), this);
                s.child(
                    ConfirmDialog::new(
                        "Discard this draft?",
                        "Your draft will be deleted.",
                        "Discard",
                        move |_, _, cx| {
                            discard.update(cx, |this, cx| {
                                this.confirm_discard = None;
                                this.mailbox.update(cx, |m, cx| m.discard_draft(id, cx));
                            })
                        },
                        move |_, _, cx| {
                            cancel.update(cx, |this, cx| {
                                this.confirm_discard = None;
                                cx.notify();
                            })
                        },
                    )
                    .track_focus(&self.confirm_focus),
                )
            })
    }
}
