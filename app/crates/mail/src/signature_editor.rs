//! Toolbar-only signature picker. Inline reply retains the state, not the controls.

use std::{cell::Cell, rc::Rc};

use gpui::{Anchor, App, Bounds, Context, Entity, EventEmitter, FocusHandle, Focusable, IntoElement, KeyDownEvent,
    MouseButton, Pixels, Render, SharedString, Subscription, WeakEntity, Window, anchored, canvas, deferred, div, img, point, prelude::*, px};

use crate::durian::api::DraftContent;
use crate::html_render::{HtmlJob, HtmlRenderer, RenderState};
use crate::model::{Mailbox, MailboxEvent};
use crate::signatures::{Choice, Settings, Signature};
use ui::theme::{ActiveTheme, typography::*};
use ui::{AfterConfirm, Icon, IconName, Leading, Picker, PickerDelegate, PickerEvent, PickerItem, StyledExt, fuzzy_score};

pub struct Changed;

struct SignaturePicker {
    mailbox: Entity<Mailbox>,
    editor: WeakEntity<SignatureEditor>,
    account: String,
    current: Signature,
}

impl SignaturePicker {
    fn current(&self, cx: &App) -> Signature {
        let mut current = self.current.clone();
        current.resolve(&self.mailbox.read(cx).compose_settings);
        current
    }

    fn options(&self, cx: &App) -> Vec<(String, Signature)> {
        let settings = &self.mailbox.read(cx).compose_settings;
        let mut options = vec![("automatic".into(), Signature::new(&self.account, settings)),
            ("none".into(), Signature { choice: Choice::None, html: None })];
        if let Settings::Ready(settings) = settings {
            for name in settings.signatures.keys() {
                let mut signature = Signature { choice: Choice::None, html: None };
                signature.choose_named(name, &self.mailbox.read(cx).compose_settings);
                options.push((format!("named:{name}"), signature));
            }
        }
        // A recovered snapshot may differ from today's config. Preview and selecting
        // that saved row must mean the same thing; new definitions stay separate.
        let current = self.current(cx);
        if !options.iter().any(|(_, signature)| *signature == current) {
            options.insert(0, ("saved".into(), current));
        }
        options
    }

    fn current_id(&self, cx: &App) -> String {
        let current = self.current(cx);
        self.options(cx).into_iter().find(|(_, signature)| *signature == current).map(|(id, _)| id).unwrap_or_default()
    }
}

impl PickerDelegate for SignaturePicker {
    fn placeholder(&self) -> SharedString { "Find a signature…".into() }

    fn items(&self, query: &str, cx: &App) -> Vec<PickerItem> {
        let current = self.current_id(cx);
        let mut matches = Vec::new();
        for (id, signature) in self.options(cx) {
            let label = if id == "automatic" { "Account default".into() } else { signature.label() };
            let Some(score) = fuzzy_score(query, &label) else { continue };
            let mut item = PickerItem::new(id.clone(), label);
            item.leading = if id == current { Leading::Icon(IconName::StatusCheck.into()) } else { Leading::None };
            item.detail = match id.as_str() {
                "automatic" => Some(signature.label().into()),
                "saved" => Some("Saved version".into()),
                _ => None,
            };
            matches.push((score, item));
        }
        if !query.is_empty() { matches.sort_by(|a, b| b.0.cmp(&a.0)); }
        matches.into_iter().map(|(_, item)| item).collect()
    }

    fn selected(&mut self, item: Option<&PickerItem>, cx: &mut App) {
        let signature = item.and_then(|item| self.options(cx).into_iter().find(|(id, _)| id == item.id.as_ref()).map(|(_, signature)| signature));
        let editor = self.editor.clone();
        cx.defer(move |cx| { editor.update(cx, |editor, cx| editor.preview_candidate(signature, cx)).ok(); });
    }

    fn confirm(&mut self, item: &PickerItem, _: &str, _: &mut Window, cx: &mut App) -> AfterConfirm {
        if let Some((_, signature)) = self.options(cx).into_iter().find(|(id, _)| id == item.id.as_ref()) {
            self.editor.update(cx, |editor, cx| {
                if editor.frozen(cx) { return }
                editor.signature = signature;
                cx.emit(Changed);
                cx.notify();
            }).ok();
        }
        AfterConfirm::Close
    }
}

pub struct SignatureEditor {
    mailbox: Entity<Mailbox>,
    draft: String,
    account: String,
    signature: Signature,
    policy: crate::html_doc::Policy,
    focus: FocusHandle,
    previous_focus: Option<FocusHandle>,
    bounds: Rc<Cell<Bounds<Pixels>>>,
    menu: Option<Entity<Picker<SignaturePicker>>>,
    menu_subscription: Option<Subscription>,
    menu_width: f32,
    menu_height: f32,
    preview: Option<Entity<HtmlRenderer>>,
    preview_subscription: Option<Subscription>,
    preview_signature: Option<Signature>,
    preview_key: SharedString,
    preview_text: SharedString,
    _subscription: Subscription,
}

impl EventEmitter<Changed> for SignatureEditor {}

impl Focusable for SignatureEditor {
    fn focus_handle(&self, _: &App) -> FocusHandle { self.focus.clone() }
}

impl SignatureEditor {
    pub fn new(mailbox: Entity<Mailbox>, wire: &DraftContent, cx: &mut Context<Self>) -> Self {
        let signature = Signature::from_draft(wire);
        let policy = mailbox.read(cx).signature_policy;
        let subscription = cx.observe(&mailbox, |this, mailbox, cx| {
            let changed = !this.frozen(cx) && this.signature.resolve(&mailbox.read(cx).compose_settings);
            let policy = mailbox.read(cx).signature_policy;
            if changed || policy != this.policy {
                this.policy = policy;
                this.preview_candidate(if changed { Some(this.signature.clone()) } else { this.preview_signature.clone() }, cx);
            }
            if let Some(menu) = &this.menu { menu.update(cx, |menu, cx| menu.refresh(cx)); }
            if this.frozen(cx) { this.drop_menu(); }
            if changed { cx.emit(Changed); }
            cx.notify();
        });
        let mut this = Self { mailbox, draft: wire.id.clone(), account: wire.account_id.clone().unwrap_or_default(), signature, policy,
            focus: cx.focus_handle(), previous_focus: None, bounds: Rc::new(Cell::new(Bounds::default())),
            menu: None, menu_subscription: None, menu_width: 600., menu_height: 350.,
            preview: None, preview_subscription: None, preview_signature: None,
            preview_key: SharedString::default(), preview_text: SharedString::default(), _subscription: subscription };
        // The initial request may have finished while this same-session draft was
        // closed. Persist after the parent installs its Changed subscription.
        if !this.frozen(cx) && this.signature.resolve(&this.mailbox.read(cx).compose_settings) {
            let editor = cx.entity().downgrade();
            cx.defer(move |cx| { editor.update(cx, |this, cx| { if !this.frozen(cx) { cx.emit(Changed); } }).ok(); });
        }
        this
    }

    fn frozen(&self, cx: &App) -> bool {
        self.mailbox.read(cx).drafts.iter().any(|draft| draft.wire.id == self.draft &&
            (draft.sending || draft.deleting || draft.queued.is_some() || draft.wire.extra.contains_key(crate::durian::writes::PENDING_SEND)))
    }

    pub fn apply(&self, wire: &mut DraftContent) {
        self.signature.apply(wire);
    }

    pub fn change_account(&mut self, account: &str, cx: &mut Context<Self>) {
        if self.frozen(cx) { return }
        self.account = account.to_string();
        self.signature.change_account(account, &self.mailbox.read(cx).compose_settings);
        cx.notify();
    }

    fn close_menu(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(focus) = self.previous_focus.take() { focus.focus(window, cx); }
        self.drop_menu();
        cx.notify();
    }

    fn drop_menu(&mut self) {
        self.menu_subscription = None;
        self.menu = None;
        self.preview_subscription = None;
        self.preview = None;
        self.preview_signature = None;
    }

    fn open_menu(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.frozen(cx) || self.menu.is_some() { return }
        self.previous_focus = window.focused(cx);
        self.menu_width = (f32::from(window.viewport_size().width) - 32.).clamp(320., 600.);
        let list_width = self.menu_width * 0.44;
        let delegate = SignaturePicker { mailbox: self.mailbox.clone(), editor: cx.entity().downgrade(),
            account: self.account.clone(), current: self.signature.clone() };
        self.menu_height = (124. + delegate.options(cx).len() as f32 * 40.).clamp(250., 350.)
            .min(f32::from(window.viewport_size().height) - 32.);
        let selected = delegate.current_id(cx);
        let menu = cx.new(|cx| {
            let mut menu = Picker::new(delegate, list_width, window, cx).embedded(self.menu_height - 118.);
            menu.select_id(&selected);
            menu
        });
        self.menu_subscription = Some(cx.subscribe_in(&menu, window, |this, _, PickerEvent::Dismissed, window, cx| this.close_menu(window, cx)));
        self.menu = Some(menu);
        let renderer = cx.new(|cx| HtmlRenderer::signature_preview(self.menu_width - list_width - 26., cx));
        self.preview_subscription = Some(cx.observe(&renderer, |_, _, cx| cx.notify()));
        self.preview = Some(renderer);
        self.preview_candidate(Some(self.signature.clone()), cx);
        cx.notify();
    }

    fn preview_candidate(&mut self, signature: Option<Signature>, cx: &mut Context<Self>) {
        let Some(renderer) = &self.preview else { return };
        self.preview_signature = signature;
        self.preview_key = uuid::Uuid::new_v4().to_string().into();
        let jobs = match self.preview_signature.as_ref().and_then(|signature| signature.html.as_deref()) {
            Some(html) => match crate::html_doc::prepare(html, &Default::default(), self.policy) {
                Ok(html) => {
                    self.preview_text = crate::signatures::preview_text(&html).into();
                    vec![HtmlJob { key: self.preview_key.clone(), html: html.into() }]
                }
                Err(_) => { self.preview_text = "Signature too large to preview.".into(); Vec::new() }
            },
            None => { self.preview_text = if self.preview_signature.is_some() { "No signature" } else { "Choose a signature to preview" }.into(); Vec::new() }
        };
        renderer.update(cx, |renderer, cx| renderer.prioritize(jobs, 1, cx));
        cx.notify();
    }
}

impl Render for SignatureEditor {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let warning = match &self.mailbox.read(cx).compose_settings {
            Settings::Loading => Some("Loading signatures…"),
            Settings::Unavailable(_) => Some("Signatures unavailable. Your saved choice is kept."),
            Settings::Ready(_) => None,
        };
        let snapshot = self.preview.as_ref().and_then(|renderer| renderer.read(cx).state(&self.preview_key));
        let mut preview = div().flex().flex_col().w_full().min_w_0().flex_none()
            .rounded(theme.radius.sm).border_hairline().border_color(theme.border_card);
        match snapshot {
            Some(RenderState::Ready(snapshot)) => {
                for strip in &snapshot.strips { preview = preview.child(img(strip.path.clone()).w_full()); }
                if snapshot.clipped { preview = preview.child("Preview shortened; the complete signature is kept."); }
            }
            other => {
                preview = preview.p(px(10.)).gap(px(6.)).child(self.preview_text.clone());
                if let Some(status) = match other {
                    Some(RenderState::Pending) => Some("Loading preview…"),
                    Some(RenderState::Failed(_)) => Some("Text preview · HTML unavailable"),
                    _ => None,
                } { preview = preview.child(div().text_color(theme.text_tertiary).child(status)); }
            }
        }
        let bounds = self.bounds.clone();
        div().id("signature-picker").relative().flex().flex_none().items_center().gap(px(6.)).px(px(9.)).h(px(28.)).max_w(px(150.))
            .track_focus(&self.focus).key_context("SignatureButton")
            .text_size(px(SIZE_MINI)).text_color(theme.text_secondary).rounded(theme.radius.pill)
            .hover(|s| s.bg(theme.bg_raised_hover)).focus(|s| s.bg(theme.bg_option))
            .when(self.menu.is_none(), |s| s.tooltip(ui::tooltip("Choose signature")))
            .child(Icon::new(IconName::Compose).size(14.).color(theme.text_tertiary))
            .child(div().min_w_0().truncate().child(self.signature.label()))
            .child(Icon::new(IconName::ChevronDownSmall).size(12.).color(theme.text_tertiary))
            .child(canvas(move |bounds_value, _, _| bounds.set(bounds_value), |_, _, _, _| {}).absolute().size_full())
            .on_click(cx.listener(|this, _, window, cx| this.open_menu(window, cx)))
            .on_key_down(cx.listener(|this, event: &KeyDownEvent, window, cx| {
                if this.focus.is_focused(window) && !event.keystroke.modifiers.modified()
                    && matches!(event.keystroke.key.as_str(), "enter" | "space" | "down") {
                    this.open_menu(window, cx);
                    cx.stop_propagation();
                }
            }))
            .when_some(self.menu.clone(), |s, menu| {
                let position = self.bounds.get();
                s.child(deferred(div().id("signature-backdrop").absolute().inset_0().occlude()
                    .on_mouse_down(MouseButton::Left, cx.listener(|this, _, window, cx| this.close_menu(window, cx)))
                    .child(anchored().anchor(Anchor::BottomLeft)
                        .position(point(position.left() + position.size.width / 2. - px(self.menu_width / 2.), position.top() - px(8.)))
                        .snap_to_window_with_margin(px(8.))
                        .child(div().id("signature-popover").flex().flex_col().w(px(self.menu_width)).h(px(self.menu_height))
                            .bg(theme.bg_card).border_hairline().border_color(theme.border_card).rounded(theme.radius.xl).shadow(theme.overlay_shadow())
                            .on_mouse_down_out(cx.listener(|this, _, window, cx| { this.close_menu(window, cx); cx.stop_propagation(); }))
                            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                            .on_action(cx.listener(|_, _: &crate::compose::Send, _, cx| cx.stop_propagation()))
                            .on_action(cx.listener(|_, _: &crate::compose::NextField, _, cx| cx.stop_propagation()))
                            .on_action(cx.listener(|_, _: &crate::compose::PrevField, _, cx| cx.stop_propagation()))
                            .child(div().flex().flex_1().min_h_0().child(menu)
                                .child(div().flex().flex_col().flex_1().min_w_0().min_h_0().p(px(12.)).gap(px(10.)).border_l_1().border_color(theme.border_card)
                                    .child(div().flex_none().text_color(theme.text_tertiary).child("HTML preview"))
                                    .child(div().id("signature-preview-scroll").min_h_0().overflow_y_scroll().child(preview))))
                            .child(div().flex().flex_none().items_center().gap(px(8.)).h(px(52.)).px(px(12.)).border_t_1().border_color(theme.border_card)
                                .child(div().flex_1().min_w_0().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary)
                                    .child(warning.unwrap_or("↑↓ preview · Enter select · Esc cancel")))
                                .child(div().id("reload-signatures").flex_none().px(px(6.)).py(px(6.)).rounded(theme.radius.sm)
                                    .hover(|s| s.bg(theme.bg_raised_hover)).child(if warning.is_some() { "Retry" } else { "Reload" })
                                    .on_click(cx.listener(|this, _, _, cx| this.mailbox.update(cx, |_, cx| cx.emit(MailboxEvent::ReloadComposeSettings)))))))))
                    .with_priority(11))
            })
    }
}
