//! Centered placeholder for an empty pane: faint icon, title, optional detail line and a hint
//! row (usually keycaps saying how to get out of the empty state).

use gpui::{AnyElement, App, IntoElement, ParentElement, RenderOnce, SharedString, Styled, Window, div, px};

use crate::theme::{ActiveTheme, typography::*};
use crate::{Icon, IconSource};

#[derive(IntoElement)]
pub struct EmptyState {
    icon: IconSource,
    title: SharedString,
    detail: Option<AnyElement>,
    hint: Option<AnyElement>,
}

impl EmptyState {
    pub fn new(icon: impl Into<IconSource>, title: impl Into<SharedString>) -> Self {
        Self { icon: icon.into(), title: title.into(), detail: None, hint: None }
    }

    pub fn detail(mut self, detail: impl IntoElement) -> Self {
        self.detail = Some(detail.into_any_element());
        self
    }

    pub fn hint(mut self, hint: impl IntoElement) -> Self {
        self.hint = Some(hint.into_any_element());
        self
    }
}

impl RenderOnce for EmptyState {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .size_full()
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            // optical center: a little above the geometric one
            .pb(px(64.))
            .gap(px(6.))
            .child(div().mb(px(6.)).child(Icon::new(self.icon).size(28.).color(theme.text_quaternary)))
            .child(div().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_secondary).child(self.title))
            .children(self.detail.map(|d| div().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(d)))
            .children(self.hint.map(|h| {
                div().mt(px(6.)).flex().items_center().gap(px(4.)).text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child(h)
            }))
    }
}
