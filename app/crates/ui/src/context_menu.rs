//! Right-click menus: 12px radius surface lighter than
//! cards, 32px rows with an inset 8px-radius highlight, 12px separators, keycaps on the right,
//! and type-to-filter (typing narrows the items). ↑/↓/↵/Esc work while it's open.

use std::rc::Rc;

use gpui::{
    Action, AnimationExt, App, Context, EventEmitter, FocusHandle, Focusable, IntoElement, KeyBinding, KeyDownEvent, MouseButton,
    Pixels, Point, Render, SharedString, Window, actions, anchored, deferred, div, prelude::*, px,
};

use crate::theme::{ActiveTheme, typography::*};
use crate::{Icon, IconSource, Kbd, StyledExt, fuzzy_score};

actions!(context_menu, [MenuNext, MenuPrev, MenuConfirm, MenuCancel]);

pub fn bind_context_menu_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("down", MenuNext, Some("ContextMenu")),
        KeyBinding::new("up", MenuPrev, Some("ContextMenu")),
        KeyBinding::new("enter", MenuConfirm, Some("ContextMenu")),
        KeyBinding::new("escape", MenuCancel, Some("ContextMenu")),
    ]);
}

type Handler = Rc<dyn Fn(&mut Window, &mut App)>;

/// How a row reads: plain, the current choice of a set (checkmark), or one that destroys
/// something (red).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Tone {
    #[default]
    Normal,
    Checked,
    Destructive,
}

pub enum MenuItem {
    /// Runs a GPUI action (dispatched after the menu closed, so it reaches the focused view).
    Action { label: SharedString, icon: Option<IconSource>, keys: Vec<SharedString>, action: Box<dyn Action>, tone: Tone },
    /// Runs a closure (clipboard, open link, …).
    Handler { label: SharedString, icon: Option<IconSource>, handler: Handler, tone: Tone },
    Separator,
}

impl MenuItem {
    pub fn action(label: impl Into<SharedString>, icon: impl Into<IconSource>, keys: Vec<SharedString>, action: Box<dyn Action>) -> Self {
        Self::Action { label: label.into(), icon: Some(icon.into()), keys, action, tone: Tone::Normal }
    }

    pub fn handler(label: impl Into<SharedString>, icon: impl Into<IconSource>, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        Self::Handler { label: label.into(), icon: Some(icon.into()), handler: Rc::new(f), tone: Tone::Normal }
    }

    /// One option of a set (Going / Maybe / Not going): no icon, a checkmark on the current one.
    pub fn choice(label: impl Into<SharedString>, checked: bool, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        Self::Handler { label: label.into(), icon: None, handler: Rc::new(f), tone: if checked { Tone::Checked } else { Tone::Normal } }
    }

    pub fn tone(mut self, new: Tone) -> Self {
        if let Self::Action { tone, .. } | Self::Handler { tone, .. } = &mut self {
            *tone = new;
        }
        self
    }

    /// The current choice of a set gets a checkmark instead of its icon.
    pub fn checked(self, on: bool) -> Self {
        if on { self.tone(Tone::Checked) } else { self }
    }

    pub fn destructive(self) -> Self {
        self.tone(Tone::Destructive)
    }

    fn label(&self) -> Option<&SharedString> {
        match self {
            MenuItem::Action { label, .. } | MenuItem::Handler { label, .. } => Some(label),
            MenuItem::Separator => None,
        }
    }
}

pub enum ContextMenuEvent {
    Dismissed,
}

pub struct ContextMenu {
    items: Vec<MenuItem>,
    position: Point<Pixels>,
    selected: Option<usize>,
    filter: String,
    focus: FocusHandle,
}

impl EventEmitter<ContextMenuEvent> for ContextMenu {}

impl Focusable for ContextMenu {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl ContextMenu {
    pub fn new(position: Point<Pixels>, items: Vec<MenuItem>, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let focus = cx.focus_handle();
        focus.focus(window, cx);
        Self { items, position, selected: None, filter: String::new(), focus }
    }

    /// Indices of rows to show: all items, or the label matches while filtering (no separators).
    fn visible(&self) -> Vec<usize> {
        if self.filter.is_empty() {
            return (0..self.items.len()).collect();
        }
        let mut scored: Vec<(i32, usize)> = self
            .items
            .iter()
            .enumerate()
            .filter_map(|(ix, item)| Some((fuzzy_score(&self.filter, item.label()?)?, ix)))
            .collect();
        scored.sort_by(|a, b| b.0.cmp(&a.0));
        scored.into_iter().map(|(_, ix)| ix).collect()
    }

    fn step(&mut self, delta: isize, cx: &mut Context<Self>) {
        let rows: Vec<usize> = self.visible().into_iter().filter(|&ix| self.items[ix].label().is_some()).collect();
        if rows.is_empty() {
            return;
        }
        let pos = self.selected.and_then(|s| rows.iter().position(|&r| r == s));
        let next = match pos {
            Some(p) => (p as isize + delta).rem_euclid(rows.len() as isize) as usize,
            None if delta > 0 => 0,
            None => rows.len() - 1,
        };
        self.selected = Some(rows[next]);
        cx.notify();
    }

    fn run(&mut self, ix: usize, window: &mut Window, cx: &mut Context<Self>) {
        cx.emit(ContextMenuEvent::Dismissed);
        match &self.items[ix] {
            MenuItem::Action { action, .. } => {
                let action = action.boxed_clone();
                window.defer(cx, move |window, cx| window.dispatch_action(action, cx));
            }
            MenuItem::Handler { handler, .. } => {
                let handler = handler.clone();
                window.defer(cx, move |window, cx| handler(window, cx));
            }
            MenuItem::Separator => {}
        }
    }
}

impl Render for ContextMenu {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let visible = self.visible();
        let rows = visible.iter().map(|&ix| match &self.items[ix] {
            MenuItem::Separator => div().py(px(6.)).child(div().h(px(1.)).border_b_hairline().border_color(theme.separator_menu)).into_any_element(),
            item => {
                let (label, icon, keys, tone) = match item {
                    MenuItem::Action { label, icon, keys, tone, .. } => (label.clone(), icon.clone(), keys.clone(), *tone),
                    MenuItem::Handler { label, icon, tone, .. } => (label.clone(), icon.clone(), Vec::new(), *tone),
                    MenuItem::Separator => unreachable!(),
                };
                let red = crate::theme::lch(55., 70., 30.);
                let (icon, icon_color) = match tone {
                    Tone::Checked => (Some(IconSource::from(crate::IconName::StatusCheck)), theme.text_primary),
                    Tone::Destructive => (icon, red),
                    Tone::Normal => (icon, theme.text_tertiary),
                };
                let active = self.selected == Some(ix);
                div()
                    .id(("menu-item", ix))
                    .relative()
                    .flex()
                    .items_center()
                    .gap(px(10.))
                    .h(px(32.))
                    .pl(px(14.))
                    .pr(px(14.))
                    // highlight pill inset 6px
                    .when(active, |s| s.child(div().absolute().left(px(6.)).right(px(6.)).top_0().bottom_0().rounded(theme.radius.md).bg(theme.bg_option)))
                    // checked rows without an icon still line up with the rest
                    .child(div().relative().size(px(16.)).flex_none().children(icon.map(|i| Icon::new(i).size(16.).color(icon_color))))
                    .child(div().relative().flex_1().whitespace_nowrap().when(tone == Tone::Destructive, |l| l.text_color(red)).child(label))
                    .when(!keys.is_empty(), |s| s.child(div().relative().flex().gap(px(3.)).children(keys.into_iter().map(Kbd::new))))
                    .on_mouse_move(cx.listener(move |this, _, _, cx| {
                        if this.selected != Some(ix) {
                            this.selected = Some(ix);
                            cx.notify();
                        }
                    }))
                    .on_mouse_down(MouseButton::Left, cx.listener(move |this, _, window, cx| {
                        cx.stop_propagation();
                        this.run(ix, window, cx);
                    }))
                    .into_any_element()
            }
        });

        let menu = div()
            .key_context("ContextMenu")
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &MenuNext, _, cx| this.step(1, cx)))
            .on_action(cx.listener(|this, _: &MenuPrev, _, cx| this.step(-1, cx)))
            .on_action(cx.listener(|this, _: &MenuConfirm, window, cx| {
                if let Some(ix) = this.selected {
                    this.run(ix, window, cx);
                }
            }))
            .on_action(cx.listener(|_, _: &MenuCancel, _, cx| cx.emit(ContextMenuEvent::Dismissed)))
            // type to filter
            .on_key_down(cx.listener(|this, event: &KeyDownEvent, _, cx| {
                let ks = &event.keystroke;
                if ks.modifiers.modified() {
                    return;
                }
                if ks.key == "backspace" {
                    this.filter.pop();
                } else if let Some(ch) = ks.key_char.as_ref().filter(|c| c.chars().all(|c| !c.is_control())) {
                    this.filter.push_str(ch);
                } else {
                    return;
                }
                this.selected = this.visible().into_iter().find(|&ix| this.items[ix].label().is_some());
                cx.stop_propagation();
                cx.notify();
            }))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_mouse_down(MouseButton::Right, |_, _, cx| cx.stop_propagation())
            .min_w(px(200.))
            .max_w(px(360.))
            .py(px(6.))
            .bg(theme.bg_menu)
            .border_hairline()
            .border_color(theme.border_menu)
            .rounded(theme.radius.xl)
            .shadow(theme.menu_shadow())
            .text_size(px(SIZE_SMALL))
            .font_weight(WEIGHT_NORMAL)
            .text_color(theme.text_secondary)
            .when(!self.filter.is_empty(), |s| {
                s.child(div().px(px(14.)).pb(px(4.)).text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child(format!("Filter: {}", self.filter)))
            })
            .children(rows);

        // full-window catcher closes on any click outside; the menu snaps inside the window
        deferred(
            div()
                .id("context-menu-backdrop")
                .absolute()
                .inset_0()
                .occlude()
                .on_mouse_down(MouseButton::Left, cx.listener(|_, _, _, cx| cx.emit(ContextMenuEvent::Dismissed)))
                .on_mouse_down(MouseButton::Right, cx.listener(|_, _, _, cx| cx.emit(ContextMenuEvent::Dismissed)))
                .child(
                    anchored()
                        .position(self.position)
                        .snap_to_window_with_margin(px(8.))
                        .child(menu.with_animation("context-menu-in", crate::entrance(110), |menu, t| menu.opacity(t))),
                ),
        )
        .with_priority(10)
    }
}
