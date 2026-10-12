use gpui::{
    AnyElement, App, ElementId, IntoElement, ParentElement, RenderOnce, SharedString, Window, div, prelude::*, px,
};

use crate::theme::{ActiveTheme, typography::*};
use crate::StyledExt;

/// The rounded "pill" used for project, label, due date and relation properties in list rows.
#[derive(IntoElement)]
pub struct Chip {
    id: ElementId,
    leading: Option<AnyElement>,
    label: SharedString,
    max_width: f32,
}

impl Chip {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self { id: id.into(), leading: None, label: label.into(), max_width: 224. }
    }

    pub fn leading(mut self, el: impl IntoElement) -> Self {
        self.leading = Some(el.into_any_element());
        self
    }

    pub fn max_width(mut self, w: f32) -> Self {
        self.max_width = w;
        self
    }
}

impl RenderOnce for Chip {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .id(self.id)
            .flex()
            .items_center()
            .flex_shrink_0()
            .h(px(24.))
            .max_w(px(self.max_width))
            .px(px(8.))
            .rounded(theme.radius.pill)
            .bg(theme.bg_panel)
            .border_hairline()
            .border_color(theme.border_chip)
            .overflow_hidden()
            .hover(|s| s.bg(cx.theme().bg_chip_hover))
            .children(self.leading.map(|el| div().flex().flex_none().items_center().justify_center().size(px(14.)).child(el)))
            .child(
                div()
                    .ml(px(6.))
                    .mr(px(1.))
                    .min_w_0()
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_NORMAL)
                    .text_color(theme.text_tertiary)
                    .whitespace_nowrap()
                    .text_ellipsis()
                    .overflow_hidden()
                    .child(self.label),
            )
    }
}

/// 9px colored dot used as the leading element of label chips.
pub fn color_dot(color: gpui::Hsla) -> impl IntoElement {
    div().size(px(9.)).rounded_full().bg(color)
}

/// Small status pill for a message's delivery state, e.g. the planned send-later UI:
/// `[clock] Scheduled · Thu 09:00 · Edit`. Not shown anywhere yet – kept for that feature.
/// (It was first used as "Sent as HTML + plain text · View", opening the outgoing .eml in Quick
/// Look; see `mail::attachments::quick_look_path` and `Message::eml`.)
#[allow(dead_code)]
#[derive(IntoElement)]
pub struct StatusChip {
    id: ElementId,
    icon: crate::IconSource,
    label: SharedString,
    action: Option<SharedString>,
    on_click: Option<Box<dyn Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static>>,
}

#[allow(dead_code)]
impl StatusChip {
    pub fn new(id: impl Into<ElementId>, icon: impl Into<crate::IconSource>, label: impl Into<SharedString>) -> Self {
        Self { id: id.into(), icon: icon.into(), label: label.into(), action: None, on_click: None }
    }

    /// Trailing action word ("Edit", "View") and what clicking the chip does.
    pub fn action(mut self, label: impl Into<SharedString>, f: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.action = Some(label.into());
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for StatusChip {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .id(self.id)
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
            .child(crate::Icon::new(self.icon).size(12.).color(theme.text_tertiary))
            .child(self.label)
            .children(self.action.map(|a| div().text_color(theme.text_secondary).child(a)))
            .when_some(self.on_click, |s, f| s.on_click(f))
    }
}
