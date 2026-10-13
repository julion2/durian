//! Sidebar building blocks: nav items and collapsible section headers.

use std::f32::consts::FRAC_PI_2;

use gpui::{
    AnyElement, App, ClickEvent, ElementId, IntoElement, RenderOnce, SharedString, Window, div, prelude::*, px,
};

use crate::theme::{ActiveTheme, typography::*};
use crate::{Icon, IconButton, IconName, IconSource};

pub const SIDEBAR_WIDTH: f32 = 244.;
pub const TITLEBAR_HEIGHT: f32 = 42.;

type ClickHandler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// 28px navigation row: leading glyph, label, optional trailing count.
#[derive(IntoElement)]
pub struct NavItem {
    id: ElementId,
    label: SharedString,
    leading: Option<AnyElement>,
    icon: Option<IconSource>,
    trailing: Option<SharedString>,
    active: bool,
    indent: f32,
    on_click: Option<ClickHandler>,
}

impl NavItem {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            leading: None,
            icon: None,
            trailing: None,
            active: false,
            indent: 0.,
            on_click: None,
        }
    }

    /// Icon tinted with the row's text color.
    pub fn icon(mut self, icon: impl Into<IconSource>) -> Self {
        self.icon = Some(icon.into());
        self
    }

    /// Any custom leading element (colored icon, tag dot, …).
    pub fn leading(mut self, el: impl IntoElement) -> Self {
        self.leading = Some(el.into_any_element());
        self
    }

    pub fn trailing(mut self, text: impl Into<SharedString>) -> Self {
        self.trailing = Some(text.into());
        self
    }

    pub fn active(mut self, active: bool) -> Self {
        self.active = active;
        self
    }

    pub fn indent(mut self, indent: f32) -> Self {
        self.indent = indent;
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for NavItem {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        // the active item recedes while the window is in the background
        let active_bg = if window.is_window_active() { theme.bg_nav_active } else { theme.bg_nav_active.opacity(0.5) };
        let enabled = self.on_click.is_some();
        let fg = if !enabled { theme.text_quaternary } else if self.active { theme.text_nav_active } else { theme.text_muted };
        let leading = self.leading.or_else(|| self.icon.map(|i| Icon::new(i).color(fg).into_any_element()));
        div()
            .id(self.id)
            .flex()
            .items_center()
            .h(px(28.))
            .my(px(1.))
            .ml(px(self.indent))
            .pl(px(if self.indent > 0. { 6. } else { 8. }))
            .pr(px(9.))
            .gap(px(6.))
            .rounded(theme.radius.md)
            .text_size(px(SIZE_SMALL))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(fg)
            .when(enabled, |s| s.cursor_pointer().map(|s| if self.active { s.bg(active_bg) } else { s.hover(|s| s.bg(theme.bg_nav_hover)) }))
            .children(leading.map(|el| div().flex().flex_none().items_center().justify_center().size(px(14.)).child(el)))
            .child(div().flex_1().min_w_0().truncate().child(self.label))
            // counts: one step smaller, tabular digits so 1/2/3 share a right edge, quaternary ink
            .children(self.trailing.map(|t| {
                div()
                    .flex_none()
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_NORMAL)
                    .font_features(gpui::FontFeatures(std::sync::Arc::new(vec![("tnum".into(), 1)])))
                    .text_color(theme.text_quaternary)
                    .child(t)
            }))
            .when_some(self.on_click, |s, f| s.on_click(f))
    }
}

/// "Workspace ▾" style collapsible section header with an optional hover-revealed action.
#[derive(IntoElement)]
pub struct SectionHeader {
    id: ElementId,
    label: SharedString,
    open: bool,
    action: Option<IconButton>,
    on_toggle: Option<ClickHandler>,
}

impl SectionHeader {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, open: bool) -> Self {
        Self { id: id.into(), label: label.into(), open, action: None, on_toggle: None }
    }

    pub fn action(mut self, button: IconButton) -> Self {
        self.action = Some(button);
        self
    }

    pub fn on_toggle(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_toggle = Some(Box::new(f));
        self
    }
}

impl RenderOnce for SectionHeader {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = cx.theme();
        let group: SharedString = format!("section-{:?}", self.id).into();
        div()
            .group(group.clone())
            .flex()
            .items_center()
            .justify_between()
            .h(px(28.))
            .mt(px(8.))
            .mb(px(1.))
            .pl(px(5.))
            .child(
                div()
                    .id(self.id)
                    .flex()
                    .flex_1()
                    .items_center()
                    .h_full()
                    .px(px(4.))
                    .rounded(theme.radius.md)
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_muted)
                    .hover(|s| s.bg(theme.bg_nav_hover))
                    .child(self.label)
                    .child(
                        div().mt(px(1.)).ml(px(2.)).child(
                            Icon::new(IconName::Disclosure)
                                .size(16.)
                                .color(theme.text_muted)
                                .rotate(if self.open { FRAC_PI_2 } else { 0. }),
                        ),
                    )
                    .when_some(self.on_toggle, |s, f| s.on_click(f)),
            )
            .children(self.action.map(|button| {
                div().mr(px(2.)).invisible().group_hover(group, |s| s.visible()).child(button.size(24.))
            }))
    }
}
