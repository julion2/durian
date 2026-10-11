//! Full composer: a 750px card
//! with 21px radius over a dimmed backdrop, breadcrumb header, big title field (here: subject),
//! body editor, and a footer with a filled primary action.

use gpui::{
    App, Context, Entity, EventEmitter, FocusHandle, Focusable, IntoElement, KeyBinding, KeyDownEvent, MouseButton,
    Render, SharedString, Subscription, Window, actions, deferred, div, prelude::*, px,
};

use crate::contact_avatar;
use crate::model::{Contact, DraftKind, Mailbox};
use ui::theme::{ActiveTheme, typography::*};
use ui::{ConfirmDialog, IconButton, IconName, PrimaryButton, StyledExt, TextArea, TextAreaEvent, TextInput, TextInputEvent, fuzzy_score};

actions!(compose, [Send, Close, CommitRecipient, NextSuggestion, PrevSuggestion, NextField, PrevField]);

pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("cmd-enter", Send, Some("Compose")),
        KeyBinding::new("escape", Close, Some("Compose")),
        KeyBinding::new("tab", NextField, Some("Compose")),
        KeyBinding::new("shift-tab", PrevField, Some("Compose")),
        KeyBinding::new("enter", CommitRecipient, Some("RecipientField")),
        KeyBinding::new("tab", CommitRecipient, Some("RecipientField")),
        KeyBinding::new("down", NextSuggestion, Some("RecipientField")),
        KeyBinding::new("up", PrevSuggestion, Some("RecipientField")),
    ]);
}

/// What the composer starts with (new mail, pop-out reply, forward).
#[derive(Clone, Default)]
pub struct ComposeSeed {
    /// Reopening a saved draft keeps updating (and finally deletes) that draft.
    pub draft_id: Option<usize>,
    /// Sending account id; None = the mailbox's default for the current filter.
    pub account: Option<SharedString>,
    /// Sending address within that account (primary or alias); None = primary.
    pub from: Option<SharedString>,
    pub to: Vec<SharedString>,
    pub cc: Vec<SharedString>,
    pub subject: SharedString,
    pub body: SharedString,
}

#[derive(Clone, Copy)]
enum Format {
    Wrap(&'static str),
    Prefix(&'static str),
}

pub enum ComposeEvent {
    Close,
    DraftSaved(usize),
}

// ---------------------------------------------------------------------------------------------
// Recipient field: chips + query input + contact suggestions

pub enum RecipientFieldEvent {
    /// × or Backspace on an empty Cc/Bcc row
    Close,
}

impl EventEmitter<RecipientFieldEvent> for RecipientField {}

pub struct RecipientField {
    label: &'static str,
    /// label column: 36px next to To/Cc, wider where the field sits among longer labels
    label_width: f32,
    /// an icon instead of the label, no divider: a property row (event composer)
    icon: Option<IconName>,
    /// Cc / Bcc can be closed again; To can't.
    closable: bool,
    recipients: Vec<SharedString>,
    input: Entity<TextInput>,
    contacts: Vec<Contact>,
    suggestions: Vec<Contact>,
    selected: usize,
    _subscription: Subscription,
}

impl RecipientField {
    pub(crate) fn new(label: &'static str, closable: bool, recipients: Vec<SharedString>, contacts: Vec<Contact>, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextInput::new("", cx));
        let subscription = cx.subscribe(&input, |this, _, event, cx| match event {
            TextInputEvent::Changed(query) => this.update_suggestions(query, cx),
        });
        Self { label, label_width: 36., icon: None, closable, recipients, input, contacts, suggestions: Vec::new(), selected: 0, _subscription: subscription }
    }

    pub(crate) fn as_property(mut self, icon: IconName, placeholder: &'static str, cx: &mut Context<Self>) -> Self {
        self.icon = Some(icon);
        self.label_width = 28.;
        self.input.update(cx, |input, _| input.set_placeholder(placeholder));
        self
    }

    fn update_suggestions(&mut self, query: &str, cx: &mut Context<Self>) {
        let query = query.trim();
        self.suggestions = if query.is_empty() {
            Vec::new()
        } else {
            let mut scored: Vec<(i32, Contact)> = self
                .contacts
                .iter()
                .filter(|c| !self.recipients.contains(&c.name))
                .filter_map(|c| {
                    let score = fuzzy_score(query, &c.name).max(fuzzy_score(query, &c.email))?;
                    Some((score, c.clone()))
                })
                .collect();
            scored.sort_by(|a, b| b.0.cmp(&a.0));
            scored.into_iter().take(5).map(|(_, c)| c).collect()
        };
        self.selected = 0;
        cx.notify();
    }

    /// Enter / Tab / comma: take the highlighted suggestion, or the typed address as-is.
    fn commit(&mut self, cx: &mut Context<Self>) -> bool {
        let typed = self.input.read(cx).text().trim().trim_end_matches(',').to_string();
        let name: Option<SharedString> = match self.suggestions.get(self.selected) {
            Some(contact) => Some(contact.name.clone()),
            None if typed.contains('@') => Some(typed.into()),
            None => None,
        };
        let Some(name) = name else { return false };
        if !self.recipients.contains(&name) {
            self.recipients.push(name);
        }
        self.input.update(cx, |input, cx| input.clear(cx));
        self.suggestions.clear();
        cx.notify();
        true
    }

    pub(crate) fn input_focus(&self, cx: &App) -> FocusHandle {
        self.input.read(cx).focus_handle(cx)
    }

    pub fn recipients(&self) -> Vec<SharedString> {
        self.recipients.clone()
    }

    fn clear(&mut self, cx: &mut Context<Self>) {
        self.recipients.clear();
        self.suggestions.clear();
        self.input.update(cx, |input, cx| input.clear(cx));
        cx.notify();
    }
}

impl Render for RecipientField {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let chips = self.recipients.iter().enumerate().map(|(ix, name)| {
            let contact = self.contacts.iter().find(|c| c.name == *name).cloned().unwrap_or(Contact {
                name: name.clone(),
                email: name.clone(),
                avatar: None,
            });
            let group: SharedString = format!("recipient-{}-{ix}", self.label).into();
            div()
                .id(("recipient", ix))
                .group(group.clone())
                .flex()
                .flex_none()
                .items_center()
                .gap(px(6.))
                .h(px(24.))
                .pl(px(3.))
                .pr(px(4.))
                .rounded(theme.radius.pill)
                .bg(theme.bg_raised)
                .shadow(theme.raised_shadow(false))
                .text_size(px(SIZE_MINI))
                .font_weight(WEIGHT_NORMAL)
                .text_color(theme.text_secondary)
                .child(contact_avatar(&contact).size(18.))
                .child(name.clone())
                // × on hover removes; keeps the chip's width so nothing jumps
                .child(
                    div()
                        .id(SharedString::from(format!("{group}-remove")))
                        .flex()
                        .items_center()
                        .justify_center()
                        .size(px(16.))
                        .rounded(theme.radius.pill)
                        .invisible()
                        .group_hover(group, |s| s.visible())
                        .hover(|s| s.bg(theme.bg_nav_hover))
                        .child(ui::Icon::new(IconName::CloseSmall).size(12.).color(theme.text_tertiary))
                        .on_click(cx.listener(move |this, _, _, cx| {
                            cx.stop_propagation();
                            if ix < this.recipients.len() {
                                this.recipients.remove(ix);
                            }
                            cx.notify();
                        })),
                )
        });

        // deferred: painted after the rest of the dialog, so subject/body never draw over it
        let suggestions = (!self.suggestions.is_empty()).then(|| {
            deferred(div()
                .absolute()
                .top(px(38.))
                .left(px(48.))
                .w(px(320.))
                .p(px(4.))
                .bg(theme.bg_card)
                .border_hairline()
                .border_color(theme.border_card)
                .rounded(theme.radius.lg)
                .shadow(theme.overlay_shadow())
                .children(self.suggestions.iter().enumerate().map(|(ix, contact)| {
                    let active = ix == self.selected;
                    div()
                        .id(("suggestion", ix))
                        .flex()
                        .items_center()
                        .gap(px(8.))
                        .h(px(36.))
                        .px(px(8.))
                        .rounded(theme.radius.sm)
                        .when(active, |s| s.bg(theme.bg_option))
                        .child(contact_avatar(contact).size(20.))
                        .child(div().text_size(px(SIZE_SMALL)).text_color(theme.text_primary).child(contact.name.clone()))
                        .child(div().min_w_0().truncate().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(contact.email.clone()))
                        .on_mouse_down(MouseButton::Left, cx.listener(move |this, _, _, cx| {
                            this.selected = ix;
                            this.commit(cx);
                            cx.stop_propagation();
                        }))
                })))
            .with_priority(1)
        });

        div()
            .key_context("RecipientField")
            .on_action(cx.listener(|this, _: &CommitRecipient, _, cx| {
                // nothing to commit: let Tab/Enter fall through to the dialog
                if !this.commit(cx) {
                    cx.propagate();
                }
            }))
            .on_action(cx.listener(|this, _: &NextSuggestion, _, cx| {
                if !this.suggestions.is_empty() {
                    this.selected = (this.selected + 1) % this.suggestions.len();
                    cx.notify();
                }
            }))
            .on_action(cx.listener(|this, _: &PrevSuggestion, _, cx| {
                if !this.suggestions.is_empty() {
                    this.selected = (this.selected + this.suggestions.len() - 1) % this.suggestions.len();
                    cx.notify();
                }
            }))
            // comma commits (plain text input, so a key listener sees it)
            .capture_key_down(cx.listener(|this, event: &KeyDownEvent, _, cx| {
                match event.keystroke.key.as_str() {
                    "," => {
                        if this.commit(cx) {
                            cx.stop_propagation();
                        }
                    }
                    _ => {}
                }
            }))
            // Backspace on an empty query removes the last chip (or closes an empty Cc/Bcc row).
            // It has to be caught as the *action*, on the way down: key bindings run before key
            // listeners, and the text field's own Backspace would swallow it.
            .capture_action(cx.listener(|this, _: &ui::Backspace, _, cx| {
                if !this.input.read(cx).text().is_empty() {
                    return;
                }
                if this.recipients.pop().is_some() {
                    cx.stop_propagation();
                    cx.notify();
                } else if this.closable {
                    cx.stop_propagation();
                    cx.emit(RecipientFieldEvent::Close);
                }
            }))
            .relative()
            .flex()
            .items_center()
            .flex_wrap()
            .gap(px(6.))
            .min_h(px(if self.icon.is_some() { 34. } else { 40. }))
            .py(px(if self.icon.is_some() { 3. } else { 6. }))
            .when(self.icon.is_none(), |s| s.border_b_hairline().border_color(theme.border_card))
            .child(div().w(px(self.label_width)).flex_none().text_size(px(SIZE_SMALL)).text_color(theme.text_tertiary).map(|l| match self.icon {
                Some(icon) => l.child(ui::Icon::new(icon).size(15.).color(theme.text_tertiary)),
                None => l.child(self.label),
            }))
            .children(chips)
            .child(div().flex_1().min_w(px(120.)).text_size(px(SIZE_SMALL)).text_color(theme.text_primary).child(self.input.clone()))
            .when(self.closable, |s| {
                s.group("recipient-row").child(
                    div()
                        .invisible()
                        .group_hover("recipient-row", |s| s.visible())
                        .child(IconButton::new(SharedString::from(format!("close-{}", self.label)), IconName::CloseSmall).size(22.).on_click(cx.listener(|_, _, _, cx| cx.emit(RecipientFieldEvent::Close)))),
                )
            })
            .children(suggestions)
    }
}

// ---------------------------------------------------------------------------------------------
// Dialog

pub struct ComposeDialog {
    mailbox: Entity<Mailbox>,
    focus_handle: FocusHandle,
    to: Entity<RecipientField>,
    cc: Entity<RecipientField>,
    bcc: Entity<RecipientField>,
    show_cc: bool,
    show_bcc: bool,
    subject: Entity<TextInput>,
    body: Entity<TextArea>,
    draft_id: Option<usize>,
    account: SharedString,
    /// sending address (the account's primary or an alias)
    from: SharedString,
    /// "Save to drafts?" is up
    confirm_close: bool,
    confirm_focus: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<ComposeEvent> for ComposeDialog {}

impl Focusable for ComposeDialog {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl ComposeDialog {
    pub fn new(mailbox: Entity<Mailbox>, seed: ComposeSeed, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let contacts = mailbox.read(cx).contacts();
        let account = seed.account.clone().or_else(|| mailbox.read(cx).sending_account(None).map(|a| a.id.clone())).unwrap_or_default();
        let from = seed.from.clone().or_else(|| mailbox.read(cx).account(&account).map(|a| a.email.clone())).unwrap_or_default();
        let show_cc = !seed.cc.is_empty();
        let to = cx.new(|cx| RecipientField::new("To", false, seed.to.clone(), contacts.clone(), cx));
        let cc = cx.new(|cx| RecipientField::new("Cc", true, seed.cc.clone(), contacts.clone(), cx));
        let bcc = cx.new(|cx| RecipientField::new("Bcc", true, Vec::new(), contacts, cx));
        let subject = cx.new(|cx| {
            let mut input = TextInput::new("Subject", cx);
            input.set_text(seed.subject.clone());
            input
        });
        let body = cx.new(|cx| {
            let mut area = TextArea::new("Write your message…", 6, cx).markdown(true);
            area.set_text(seed.body.clone(), cx);
            area
        });
        let subscriptions = vec![
            cx.subscribe_in(&body, window, |this, _, event, window, cx| match event {
                TextAreaEvent::Submit => this.send(&Send, window, cx),
                TextAreaEvent::Cancel => this.close(&Close, window, cx),
                TextAreaEvent::Changed => cx.notify(),
            }),
            // closing a Cc/Bcc row drops its recipients and moves on to the subject
            cx.subscribe_in(&cc, window, |this, field, RecipientFieldEvent::Close, window, cx| {
                field.update(cx, |f, cx| f.clear(cx));
                this.show_cc = false;
                this.subject.read(cx).focus_handle(cx).focus(window, cx);
                cx.notify();
            }),
            cx.subscribe_in(&bcc, window, |this, field, RecipientFieldEvent::Close, window, cx| {
                field.update(cx, |f, cx| f.clear(cx));
                this.show_bcc = false;
                this.subject.read(cx).focus_handle(cx).focus(window, cx);
                cx.notify();
            }),
        ];

        // start where the user has to type next
        if seed.to.is_empty() {
            to.read(cx).input_focus(cx).focus(window, cx);
        } else if seed.subject.is_empty() {
            subject.read(cx).focus_handle(cx).focus(window, cx);
        } else {
            body.read(cx).focus_handle(cx).focus(window, cx);
        }

        Self {
            mailbox,
            focus_handle: cx.focus_handle(),
            to,
            cc,
            bcc,
            show_cc,
            show_bcc: false,
            subject,
            body,
            draft_id: seed.draft_id,
            account,
            from,
            confirm_close: false,
            confirm_focus: cx.focus_handle(),
            _subscriptions: subscriptions,
        }
    }

    fn send(&mut self, _: &Send, _: &mut Window, cx: &mut Context<Self>) {
        // pending text in the To field counts as a recipient
        self.to.update(cx, |field, cx| field.commit(cx));
        let to = self.to.read(cx).recipients();
        let body = self.body.read(cx).text().trim().to_string();
        if to.is_empty() || body.is_empty() {
            return;
        }
        let cc = self.cc.read(cx).recipients();
        let bcc = self.bcc.read(cx).recipients();
        let subject = self.subject.read(cx).text().clone();
        let subject = if subject.trim().is_empty() { "(no subject)".into() } else { subject };
        self.mailbox.update(cx, |m, cx| m.send_new(self.account.clone(), self.from.clone(), subject, to, cc, bcc, body.into(), cx));
        if let Some(id) = self.draft_id {
            self.mailbox.update(cx, |m, cx| m.discard_draft(id, cx));
        }
        cx.emit(ComposeEvent::Close);
    }

    fn has_content(&self, cx: &App) -> bool {
        !self.to.read(cx).recipients.is_empty()
            || !self.cc.read(cx).recipients.is_empty()
            || !self.bcc.read(cx).recipients.is_empty()
            || !self.subject.read(cx).text().trim().is_empty()
            || !self.body.read(cx).is_empty()
    }

    fn save_draft(&mut self, cx: &mut Context<Self>) {
        let kind = DraftKind::New {
            account: self.account.clone(),
            from: self.from.clone(),
            to: self.to.read(cx).recipients(),
            cc: self.cc.read(cx).recipients(),
            bcc: self.bcc.read(cx).recipients(),
            subject: self.subject.read(cx).text().clone(),
        };
        let body = self.body.read(cx).text().clone();
        if let Some(id) = self.mailbox.update(cx, |m, cx| m.save_new_draft(self.draft_id, kind, body, cx)) {
            cx.emit(ComposeEvent::DraftSaved(id));
        }
        cx.emit(ComposeEvent::Close);
    }

    fn discard(&mut self, cx: &mut Context<Self>) {
        if let Some(id) = self.draft_id {
            self.mailbox.update(cx, |m, cx| m.discard_draft(id, cx));
        }
        cx.emit(ComposeEvent::Close);
    }

    fn format_button(&self, id: &'static str, label: &'static str, format: Format, theme: &ui::theme::Theme, cx: &mut Context<Self>) -> gpui::Stateful<gpui::Div> {
        div()
            .id(id)
            .flex()
            .items_center()
            .justify_center()
            .size(px(28.))
            .rounded(theme.radius.pill)
            .text_size(px(SIZE_SMALL))
            .text_color(theme.text_tertiary)
            .hover(|s| s.bg(theme.bg_nav_hover).text_color(theme.text_secondary))
            .child(label)
            .on_click(cx.listener(move |this, _, window, cx| {
                this.body.update(cx, |body, cx| match format {
                    Format::Wrap(marker) => body.toggle_wrap(marker, cx),
                    Format::Prefix(prefix) => body.toggle_line_prefix(prefix, cx),
                });
                this.body.read(cx).focus_handle(cx).focus(window, cx);
            }))
    }

    /// Tab order: To → Cc (when shown) → Subject → Body.
    fn fields(&self, cx: &App) -> Vec<FocusHandle> {
        let mut fields = vec![self.to.read(cx).input_focus(cx)];
        if self.show_cc {
            fields.push(self.cc.read(cx).input_focus(cx));
        }
        if self.show_bcc {
            fields.push(self.bcc.read(cx).input_focus(cx));
        }
        fields.push(self.subject.read(cx).focus_handle(cx));
        fields.push(self.body.read(cx).focus_handle(cx));
        fields
    }

    fn move_focus(&mut self, delta: isize, window: &mut Window, cx: &mut Context<Self>) {
        let fields = self.fields(cx);
        let current = fields.iter().position(|f| f.is_focused(window)).unwrap_or(0) as isize;
        let next = (current + delta).rem_euclid(fields.len() as isize) as usize;
        fields[next].focus(window, cx);
    }

    fn next_field(&mut self, _: &NextField, window: &mut Window, cx: &mut Context<Self>) {
        self.move_focus(1, window, cx);
    }

    fn prev_field(&mut self, _: &PrevField, window: &mut Window, cx: &mut Context<Self>) {
        self.move_focus(-1, window, cx);
    }

    /// Esc / ×: an empty composer just closes; with content it asks "Save to drafts?".
    fn close(&mut self, _: &Close, window: &mut Window, cx: &mut Context<Self>) {
        if !self.has_content(cx) {
            cx.emit(ComposeEvent::Close);
            return;
        }
        self.confirm_close = true;
        self.confirm_focus.focus(window, cx);
        cx.notify();
    }
}

impl Render for ComposeDialog {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let mailbox = self.mailbox.read(cx);
        let account = mailbox.account(&self.account).cloned();
        let sender_count = mailbox.sender_options().len();
        let from = self.from.clone();
        let can_send = !self.to.read(cx).recipients.is_empty() || !self.to.read(cx).input.read(cx).text().is_empty();

        div()
            .key_context("Compose")
            .track_focus(&self.focus_handle)
            .on_action(cx.listener(Self::send))
            .on_action(cx.listener(Self::close))
            .on_action(cx.listener(Self::next_field))
            .on_action(cx.listener(Self::prev_field))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .relative()
            .w(px(750.))
            .flex()
            .flex_col()
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.dialog)
            .shadow(theme.overlay_shadow())
            .text_color(theme.text_body)
            // header: account chip › New message, close
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.))
                    .h(px(52.))
                    .pl(px(12.))
                    .pr(px(12.))
                    // From: the sending account; click to switch
                    .child(
                        div()
                            .id("compose-from")
                            .flex()
                            .items_center()
                            .gap(px(6.))
                            .h(px(24.))
                            .pl(px(8.))
                            .pr(px(8.))
                            .rounded(theme.radius.pill)
                            .shadow(theme.raised_shadow(false))
                            .text_size(px(SIZE_MINI))
                            .text_color(theme.text_primary)
                            .hover(|s| s.bg(theme.bg_raised_hover))
                            .when_some(account.clone(), |s, a| {
                                s.child(div().size(px(8.)).rounded_full().bg(a.color))
                                    .child(a.name.clone())
                                    .child(div().text_color(theme.text_tertiary).child(from.clone()))
                            })
                            // cycles through the active profile's addresses (all accounts in All accounts)
                            .when(sender_count > 1, |s| {
                                s.on_click(cx.listener(|this, _, _, cx| {
                                    let options = this.mailbox.read(cx).sender_options();
                                    let next = options.iter().position(|(_, e)| *e == this.from).map_or(0, |i| (i + 1) % options.len());
                                    (this.account, this.from) = options[next].clone();
                                    cx.notify();
                                }))
                            }),
                    )
                    .child(div().text_size(px(SIZE_SMALL)).text_color(theme.text_meta).child("›"))
                    .child(div().text_size(px(SIZE_SMALL)).child("New message"))
                    .child(div().flex_1())
                    .when(self.has_content(cx), |s| {
                        s.child(
                            div()
                                .id("save-as-draft")
                                .h(px(28.))
                                .px(px(10.))
                                .flex()
                                .items_center()
                                .rounded(theme.radius.pill)
                                .shadow(theme.raised_shadow(false))
                                .text_size(px(SIZE_SMALL))
                                .text_color(theme.text_body)
                                .hover(|s| s.bg(theme.bg_raised_hover))
                                .child("Save as draft")
                                .on_click(cx.listener(|this, _, _, cx| this.save_draft(cx))),
                        )
                    })
                    .child(IconButton::new("compose-close", IconName::CloseSmall).on_click(cx.listener(|this, _, window, cx| this.close(&Close, window, cx)))),
            )
            .child(
                div()
                    .px(px(18.))
                    .flex()
                    .flex_col()
                    .child(
                        div()
                            .relative()
                            .child(self.to.clone())
                            // "Cc  Bcc" reveal links, like most mail clients
                            .child(
                                div()
                                    .absolute()
                                    .right_0()
                                    .top(px(12.))
                                    .flex()
                                    .gap(px(10.))
                                    .text_size(px(SIZE_MINI))
                                    .text_color(theme.text_tertiary)
                                    .when(!self.show_cc, |s| {
                                        s.child(
                                            div()
                                                .id("show-cc")
                                                .hover(|s| s.text_color(theme.text_secondary))
                                                .child("Cc")
                                                .on_click(cx.listener(|this, _, window, cx| {
                                                    this.show_cc = true;
                                                    this.cc.read(cx).input_focus(cx).focus(window, cx);
                                                    cx.notify();
                                                })),
                                        )
                                    })
                                    .when(!self.show_bcc, |s| {
                                        s.child(
                                            div()
                                                .id("show-bcc")
                                                .hover(|s| s.text_color(theme.text_secondary))
                                                .child("Bcc")
                                                .on_click(cx.listener(|this, _, window, cx| {
                                                    this.show_bcc = true;
                                                    this.bcc.read(cx).input_focus(cx).focus(window, cx);
                                                    cx.notify();
                                                })),
                                        )
                                    }),
                            ),
                    )
                    .when(self.show_cc, |s| s.child(self.cc.clone()))
                    .when(self.show_bcc, |s| s.child(self.bcc.clone()))
                    // subject in title style: 18px / 600
                    .child(
                        div()
                            .pt(px(14.))
                            .pb(px(4.))
                            .text_size(px(18.))
                            .line_height(px(26.))
                            .font_weight(WEIGHT_SEMIBOLD)
                            .text_color(theme.text_primary)
                            .child(self.subject.clone()),
                    )
                    .child(
                        div()
                            .pt(px(6.))
                            .pb(px(12.))
                            .max_h(px(420.))
                            .text_size(px(SIZE_REGULAR))
                            .line_height(px(22.5))
                            .font_weight(WEIGHT_NORMAL)
                            .child(self.body.clone()),
                    ),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .px(px(12.))
                    .pb(px(12.))
                    .child(IconButton::new("compose-attach", IconName::MailAttach).style(ui::ButtonStyle::Raised))
                    .child(div().w(px(4.)))
                    // formatting: Markdown under the hood, styled live in the body
                    .child(self.format_button("fmt-bold", "B", Format::Wrap("**"), &theme, cx).font_weight(WEIGHT_SEMIBOLD))
                    .child(self.format_button("fmt-italic", "I", Format::Wrap("_"), &theme, cx).italic())
                    .child(self.format_button("fmt-code", "</>", Format::Wrap("`"), &theme, cx).font_family(theme.font_mono).text_size(px(11.)))
                    .child(self.format_button("fmt-list", "•", Format::Prefix("- "), &theme, cx))
                    .child(self.format_button("fmt-ordered", "1.", Format::Prefix("1. "), &theme, cx))
                    .child(self.format_button("fmt-quote", "❝", Format::Prefix("> "), &theme, cx))
                    .child(div().flex_1())
                    .child(div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child("⌘↵ to send"))
                    .child(div().when(!can_send, |s| s.opacity(0.5)).child(
                        PrimaryButton::new("compose-send", "Send").on_click(cx.listener(|this, _, window, cx| this.send(&Send, window, cx))),
                    )),
            )
            .when(self.confirm_close, |s| {
                let this = cx.entity();
                let (save, discard, cancel) = (this.clone(), this.clone(), this);
                s.child(
                    deferred(
                        ConfirmDialog::new(
                            "Save to drafts?",
                            "You can finish this message later from your drafts.",
                            "Save",
                            move |_, _, cx| save.update(cx, |this, cx| this.save_draft(cx)),
                            move |_, window, cx| {
                                cancel.update(cx, |this, cx| {
                                    this.confirm_close = false;
                                    this.body.read(cx).focus_handle(cx).focus(window, cx);
                                    cx.notify();
                                })
                            },
                        )
                        .secondary("Discard", move |_, _, cx| discard.update(cx, |this, cx| this.discard(cx)))
                        .track_focus(&self.confirm_focus),
                    )
                    .with_priority(2),
                )
            })
    }
}
