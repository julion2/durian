use gpui::{
    AnyView, App, ClickEvent, Context, ElementId, IntoElement, Render, RenderOnce, SharedString, Window, div, prelude::*, px,
};

use crate::theme::{ActiveTheme, typography::*};
use crate::{Icon, IconSource, StyledExt};

type ClickHandler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// GPUI owns hover delay, dismissal and placement; this view only supplies our styling.
pub fn tooltip(text: impl Into<SharedString>) -> impl Fn(&mut Window, &mut App) -> AnyView + 'static {
    let text = text.into();
    move |_, cx| cx.new(|_| Tooltip(text.clone())).into()
}

struct Tooltip(SharedString);

impl Render for Tooltip {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        div().pl(px(8.)).pt(px(8.)).child(
            div().px(px(8.)).py(px(5.)).max_w(px(320.))
                .rounded(theme.radius.sm).bg(theme.bg_card)
                .border_hairline().border_color(theme.border_card).shadow(theme.overlay_shadow())
                .text_size(px(SIZE_MINI)).text_color(theme.text_primary).child(self.0.clone()),
        )
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum ButtonStyle {
    /// No chrome until hovered (titlebar, sidebar header).
    Ghost,
    /// White pill with hairline ring + soft shadow (toolbar, compose).
    Raised,
}

/// Circular icon-only button, 28px by default.
#[derive(IntoElement)]
pub struct IconButton {
    id: ElementId,
    icon: IconSource,
    size: f32,
    style: ButtonStyle,
    on_click: Option<ClickHandler>,
    tooltip: Option<SharedString>,
}

impl IconButton {
    pub fn new(id: impl Into<ElementId>, icon: impl Into<IconSource>) -> Self {
        Self { id: id.into(), icon: icon.into(), size: 28., style: ButtonStyle::Ghost, on_click: None, tooltip: None }
    }

    pub fn size(mut self, size: f32) -> Self {
        self.size = size;
        self
    }

    pub fn style(mut self, style: ButtonStyle) -> Self {
        self.style = style;
        self
    }

    pub fn tooltip(mut self, text: impl Into<SharedString>) -> Self {
        self.tooltip = Some(text.into());
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for IconButton {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        let raised = self.style == ButtonStyle::Raised;
        let enabled = self.on_click.is_some();
        div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(px(self.size))
            .rounded(theme.radius.pill)
            .when(raised, |s| s.bg(theme.bg_raised).shadow(theme.raised_shadow(false)))
            .when(enabled, |s| s.cursor_pointer().hover(|s| {
                let theme = cx.theme();
                if raised { s.bg(theme.bg_raised_hover).shadow(theme.raised_shadow(true)) } else { s.bg(theme.bg_nav_hover) }
            }))
            .when_some(self.tooltip, |s, text| s.tooltip(tooltip(text)))
            .child(Icon::new(self.icon).size(14.).color(if !enabled { theme.text_quaternary } else if raised { theme.text_tertiary } else { theme.text_muted }))
            .on_click(move |event, window, cx| {
                // A button must never collapse its containing message or activate a row,
                // including when it is unavailable and only offers an explanation.
                cx.stop_propagation();
                if let Some(f) = &self.on_click { f(event, window, cx); }
            })
    }
}

/// Pill-shaped view tab ("Assigned", "Created", …) in the content header.
#[derive(IntoElement)]
pub struct TabPill {
    id: ElementId,
    label: SharedString,
    active: bool,
    on_click: Option<ClickHandler>,
}

impl TabPill {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, active: bool) -> Self {
        Self { id: id.into(), label: label.into(), active, on_click: None }
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for TabPill {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .h(px(28.))
            .min_w(px(28.))
            .px(px(10.))
            .rounded(theme.radius.pill)
            .text_size(px(SIZE_MINI))
            .font_weight(WEIGHT_MEDIUM)
            .whitespace_nowrap()
            .map(|s| {
                if self.active {
                    s.bg(theme.bg_tab_active).text_color(theme.text_secondary)
                } else {
                    s.bg(theme.bg_raised)
                        .text_color(theme.text_tertiary)
                        .shadow(theme.raised_shadow(false))
                        .hover(|s| {
                            let theme = cx.theme();
                            s.bg(theme.bg_raised_hover).text_color(theme.text_secondary).shadow(theme.raised_shadow(true))
                        })
                }
            })
            .child(self.label)
            .when_some(self.on_click, |s, f| s.on_click(f))
    }
}

/// Filled primary action button ("Send"): 28px, 12px/500, pill.
#[derive(IntoElement)]
pub struct PrimaryButton {
    id: ElementId,
    label: SharedString,
    on_click: Option<ClickHandler>,
    disabled: bool,
}

impl PrimaryButton {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self { id: id.into(), label: label.into(), on_click: None, disabled: false }
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for PrimaryButton {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        let enabled = !self.disabled && self.on_click.is_some();
        div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .h(px(28.))
            .px(px(12.))
            .rounded(theme.radius.pill)
            .bg(theme.primary)
            .when(enabled, |s| s.cursor_pointer().hover(|s| s.bg(theme.primary_hover)))
            .when(!enabled, |s| s.opacity(0.5))
            .text_size(px(SIZE_MINI))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(theme.on_primary)
            .child(self.label)
            .on_click(move |event, window, cx| {
                cx.stop_propagation();
                if enabled { if let Some(f) = &self.on_click { f(event, window, cx); } }
            })
    }
}

/// Raised secondary action ("Try again"): the primary button's shape, quieter.
#[derive(IntoElement)]
pub struct SecondaryButton {
    id: ElementId,
    label: SharedString,
    on_click: Option<ClickHandler>,
}

impl SecondaryButton {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self { id: id.into(), label: label.into(), on_click: None }
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for SecondaryButton {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .h(px(28.))
            .px(px(12.))
            .rounded(theme.radius.pill)
            .bg(theme.bg_raised)
            .shadow(theme.raised_shadow(false))
            .hover(|s| {
                let theme = cx.theme();
                s.bg(theme.bg_raised_hover).text_color(theme.text_primary).shadow(theme.raised_shadow(true))
            })
            .text_size(px(SIZE_MINI))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(theme.text_secondary)
            .child(self.label)
            .when_some(self.on_click, |s, f| s.on_click(f))
    }
}
