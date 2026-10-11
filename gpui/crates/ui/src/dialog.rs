//! Small modal confirmations ("Save to drafts?" / "Discard this draft?") and toasts:
//! 480px card, 15px/600 title, 15px/450 body, 32px
//! buttons – a secondary action on the left, Cancel + primary on the right. Enter = primary.

use gpui::{
    App, ClickEvent, ElementId, IntoElement, KeyBinding, MouseButton, RenderOnce, SharedString, Window, actions, div,
    prelude::*, px,
};

use crate::theme::{ActiveTheme, lch, typography::*};
use crate::{Icon, IconButton, IconName, StyledExt};

actions!(dialog, [ConfirmPrimary, DismissDialog]);

pub fn bind_dialog_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("enter", ConfirmPrimary, Some("Confirm")),
        KeyBinding::new("escape", DismissDialog, Some("Confirm")),
    ]);
}

type Handler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

fn dialog_button(id: impl Into<ElementId>, label: SharedString, primary: bool, cx: &App) -> gpui::Stateful<gpui::Div> {
    let theme = cx.theme();
    div()
        .id(id)
        .flex()
        .items_center()
        .justify_center()
        .h(px(32.))
        .px(px(12.))
        .rounded(theme.radius.pill)
        .text_size(px(SIZE_SMALL))
        .font_weight(WEIGHT_MEDIUM)
        .map(|s| {
            if primary {
                s.bg(theme.primary).text_color(theme.on_primary).hover(|s| s.bg(theme.primary_hover))
            } else {
                s.bg(theme.bg_card).text_color(theme.text_body).shadow(theme.raised_shadow(false)).hover(|s| s.bg(theme.bg_raised_hover))
            }
        })
        .child(label)
}

#[derive(IntoElement)]
pub struct ConfirmDialog {
    title: SharedString,
    body: SharedString,
    secondary: Option<(SharedString, Handler)>,
    primary: (SharedString, Handler),
    on_cancel: Handler,
    focus: Option<gpui::FocusHandle>,
}

impl ConfirmDialog {
    pub fn new(
        title: impl Into<SharedString>,
        body: impl Into<SharedString>,
        primary: impl Into<SharedString>,
        on_primary: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
        on_cancel: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        Self { title: title.into(), body: body.into(), secondary: None, primary: (primary.into(), Box::new(on_primary)), on_cancel: Box::new(on_cancel), focus: None }
    }

    /// The owner focuses this handle while the dialog is up, so Enter / Esc reach it.
    pub fn track_focus(mut self, handle: &gpui::FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    /// Left-aligned extra action (e.g. "Discard").
    pub fn secondary(mut self, label: impl Into<SharedString>, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.secondary = Some((label.into(), Box::new(f)));
        self
    }
}

impl RenderOnce for ConfirmDialog {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme().clone();
        let on_cancel = std::rc::Rc::new(self.on_cancel);
        let on_primary = std::rc::Rc::new(self.primary.1);
        let (cancel_a, cancel_b, primary_a) = (on_cancel.clone(), on_cancel.clone(), on_primary.clone());
        div()
            .id("confirm-backdrop")
            .absolute()
            .inset_0()
            .occlude()
            .bg(theme.backdrop)
            .flex()
            .justify_center()
            .items_start()
            .pt(px(220.))
            .on_mouse_down(MouseButton::Left, move |_, window, cx| cancel_a(&ClickEvent::default(), window, cx))
            .child(
                div()
                    .key_context("Confirm")
                    .when_some(self.focus, |s, focus| s.track_focus(&focus))
                    .on_action(move |_: &ConfirmPrimary, window, cx| primary_a(&ClickEvent::default(), window, cx))
                    .on_action(move |_: &DismissDialog, window, cx| cancel_b(&ClickEvent::default(), window, cx))
                    .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                    .w(px(480.))
                    .flex()
                    .flex_col()
                    .p(px(21.))
                    .bg(theme.bg_card)
                    .border_hairline()
                    .border_color(theme.border_card)
                    .rounded(theme.radius.xl)
                    .shadow(theme.overlay_shadow())
                    .child(div().mb(px(6.)).text_size(px(SIZE_REGULAR)).font_weight(WEIGHT_SEMIBOLD).text_color(theme.text_primary).child(self.title))
                    .child(div().text_size(px(SIZE_REGULAR)).font_weight(WEIGHT_NORMAL).text_color(theme.text_meta).child(self.body))
                    .child(
                        div()
                            .mt(px(20.))
                            .flex()
                            .items_center()
                            .gap(px(8.))
                            .children(self.secondary.map(|(label, f)| dialog_button("confirm-secondary", label, false, cx).on_click(f)))
                            .child(div().flex_1())
                            .child(dialog_button("confirm-cancel", "Cancel".into(), false, cx).on_click(move |e, w, cx| on_cancel(e, w, cx)))
                            .child(dialog_button("confirm-primary", self.primary.0, true, cx).on_click(move |e, w, cx| on_primary(e, w, cx))),
                    ),
            )
    }
}

/// Bottom-right notification with an optional action ("Draft saved · Open draft").
#[derive(IntoElement)]
pub struct Toast {
    message: SharedString,
    action: Option<(SharedString, Handler)>,
    on_close: Handler,
}

impl Toast {
    pub fn new(message: impl Into<SharedString>, on_close: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        Self { message: message.into(), action: None, on_close: Box::new(on_close) }
    }

    pub fn action(mut self, label: impl Into<SharedString>, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.action = Some((label.into(), Box::new(f)));
        self
    }
}

impl RenderOnce for Toast {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme().clone();
        div()
            .absolute()
            .right(px(16.))
            .bottom(px(16.))
            .w(px(360.))
            .flex()
            .items_center()
            .gap(px(10.))
            .h(px(46.))
            .pl(px(14.))
            .pr(px(8.))
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.lg)
            .shadow(theme.overlay_shadow())
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_center()
                    .size(px(16.))
                    .rounded_full()
                    .bg(lch(62., 58., 145.))
                    .child(Icon::new(IconName::StatusCheck).size(12.).color(gpui::white())),
            )
            .child(div().flex_1().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_primary).child(self.message))
            .children(self.action.map(|(label, f)| {
                div()
                    .id("toast-action")
                    .h(px(28.))
                    .px(px(10.))
                    .flex()
                    .items_center()
                    .rounded(theme.radius.pill)
                    .shadow(theme.raised_shadow(false))
                    .text_size(px(SIZE_SMALL))
                    .text_color(theme.text_body)
                    .hover(|s| s.bg(theme.bg_raised_hover))
                    .child(label)
                    .on_click(f)
            }))
            .child(IconButton::new("toast-close", IconName::CloseSmall).size(24.).on_click(self.on_close))
    }
}
