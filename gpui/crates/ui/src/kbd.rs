use gpui::{App, IntoElement, RenderOnce, SharedString, Window, div, prelude::*, px};

use crate::theme::{ActiveTheme, typography::*};
use crate::StyledExt;

/// Keycap hint (command menu, context menus): 11px glyph, 0.5px border, 3px radius.
#[derive(IntoElement)]
pub struct Kbd {
    key: SharedString,
}

impl Kbd {
    pub fn new(key: impl Into<SharedString>) -> Self {
        Self { key: key.into() }
    }
}

impl RenderOnce for Kbd {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .min_w(px(18.))
            .h(px(18.))
            .px(px(4.))
            .rounded(theme.radius.xs)
            .border_hairline()
            .border_color(theme.border_card)
            .bg(theme.bg_card)
            .text_size(px(SIZE_MICRO))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(theme.text_meta)
            .child(self.key)
    }
}
