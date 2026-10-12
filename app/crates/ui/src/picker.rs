//! Command-menu surface as a reusable picker: 12px-radius
//! card with an overlay shadow, optional context chip, 15px query input, 13px option rows with a
//! leading glyph and keycap hints; the active row gets an `lch(95)` pill inset by 2px.
//!
//! What it lists and what Enter does is up to a [`PickerDelegate`] – the command menu and the tag
//! picker are two delegates over the same element, like Zed's `Picker<D>`.

use gpui::{
    App, Context, Entity, EventEmitter, Focusable, Hsla, IntoElement, KeyBinding, MouseButton, Render, ScrollHandle,
    SharedString, Subscription, Window, actions, div, prelude::*, px,
};

use crate::theme::{ActiveTheme, typography::*};
use crate::{Icon, IconName, IconSource, Kbd, StyledExt, TextInput, TextInputEvent};

actions!(picker, [SelectNextItem, SelectPrevItem, ConfirmItem, Dismiss]);

const CONTEXT: &str = "Picker";

pub fn bind_picker_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("down", SelectNextItem, Some(CONTEXT)),
        KeyBinding::new("up", SelectPrevItem, Some(CONTEXT)),
        KeyBinding::new("enter", ConfirmItem, Some(CONTEXT)),
        KeyBinding::new("escape", Dismiss, Some(CONTEXT)),
    ]);
}

#[derive(Clone)]
pub enum Leading {
    None,
    Icon(IconSource),
    Dot(Hsla),
}

#[derive(Clone)]
pub struct PickerItem {
    pub id: SharedString,
    pub label: SharedString,
    /// Dimmed text after the label (e.g. the sender of a thread).
    pub detail: Option<SharedString>,
    pub leading: Leading,
    /// Keycaps shown on the right, e.g. `["⌘", "K"]`.
    pub keys: Vec<SharedString>,
    /// `Some` renders a checkbox (multi-select pickers).
    pub checked: Option<bool>,
    pub section: Option<SharedString>,
}

impl PickerItem {
    pub fn new(id: impl Into<SharedString>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            detail: None,
            leading: Leading::None,
            keys: Vec::new(),
            checked: None,
            section: None,
        }
    }
}

pub enum AfterConfirm {
    Close,
    KeepOpen,
}

pub trait PickerDelegate: 'static {
    fn placeholder(&self) -> SharedString;
    /// Chip above the input naming what the picker acts on ("Thread ⋅ subject").
    fn context(&self, _cx: &App) -> Option<(SharedString, SharedString)> {
        None
    }
    /// Items for `query`, already filtered and ordered.
    fn items(&self, query: &str, cx: &App) -> Vec<PickerItem>;
    fn confirm(&mut self, item: &PickerItem, query: &str, window: &mut Window, cx: &mut App) -> AfterConfirm;
}

pub enum PickerEvent {
    Dismissed,
}

pub struct Picker<D: PickerDelegate> {
    delegate: D,
    input: Entity<TextInput>,
    items: Vec<PickerItem>,
    selected: usize,
    scroll: ScrollHandle,
    width: f32,
    _input_subscription: Subscription,
}

impl<D: PickerDelegate> EventEmitter<PickerEvent> for Picker<D> {}

impl<D: PickerDelegate> Picker<D> {
    pub fn new(delegate: D, width: f32, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextInput::new(delegate.placeholder(), cx));
        let subscription = cx.subscribe(&input, |this, _, event, cx| match event {
            TextInputEvent::Changed(_) => this.refilter(cx),
        });
        input.read(cx).focus_handle(cx).focus(window, cx);
        let mut this = Self {
            delegate,
            input,
            items: Vec::new(),
            selected: 0,
            scroll: ScrollHandle::new(),
            width,
            _input_subscription: subscription,
        };
        this.refilter(cx);
        this
    }

    fn refilter(&mut self, cx: &mut Context<Self>) {
        let query = self.input.read(cx).text().clone();
        self.items = self.delegate.items(&query, cx);
        self.selected = 0;
        self.scroll.scroll_to_item(0);
        cx.notify();
    }

    fn select(&mut self, ix: usize, cx: &mut Context<Self>) {
        if ix < self.items.len() {
            self.selected = ix;
            // children of the list include section headers, so map item → child index
            self.scroll.scroll_to_item(self.child_index(ix));
            cx.notify();
        }
    }

    fn child_index(&self, item_ix: usize) -> usize {
        let mut child = 0;
        let mut section: Option<&SharedString> = None;
        for (ix, item) in self.items.iter().enumerate() {
            if item.section.is_some() && item.section.as_ref() != section {
                section = item.section.as_ref();
                child += 1;
            }
            if ix == item_ix {
                return child;
            }
            child += 1;
        }
        child
    }

    fn select_next(&mut self, _: &SelectNextItem, _: &mut Window, cx: &mut Context<Self>) {
        if !self.items.is_empty() {
            self.select((self.selected + 1) % self.items.len(), cx);
        }
    }

    fn select_prev(&mut self, _: &SelectPrevItem, _: &mut Window, cx: &mut Context<Self>) {
        if !self.items.is_empty() {
            self.select((self.selected + self.items.len() - 1) % self.items.len(), cx);
        }
    }

    fn confirm(&mut self, _: &ConfirmItem, window: &mut Window, cx: &mut Context<Self>) {
        let Some(item) = self.items.get(self.selected).cloned() else { return };
        let query = self.input.read(cx).text().to_string();
        match self.delegate.confirm(&item, &query, window, cx) {
            AfterConfirm::Close => cx.emit(PickerEvent::Dismissed),
            AfterConfirm::KeepOpen => {
                // re-read items (checkmarks changed); keep the cursor on the same row
                let selected = self.selected;
                self.items = self.delegate.items(&query, cx);
                self.selected = selected.min(self.items.len().saturating_sub(1));
                cx.notify();
            }
        }
    }

    fn dismiss(&mut self, _: &Dismiss, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(PickerEvent::Dismissed);
    }

    /// Confirm via a fresh query reset – used when a delegate created something from the query.
    pub fn clear_query(&mut self, cx: &mut Context<Self>) {
        self.input.update(cx, |input, cx| input.clear(cx));
    }
}

/// Subsequence match with bonuses for word starts and adjacency. `None` = no match.
pub fn fuzzy_score(query: &str, text: &str) -> Option<i32> {
    if query.is_empty() {
        return Some(0);
    }
    let text_lower: Vec<char> = text.to_lowercase().chars().collect();
    let mut score = 0;
    let mut last: Option<usize> = None;
    let mut pos = 0;
    for qc in query.to_lowercase().chars().filter(|c| !c.is_whitespace()) {
        let found = (pos..text_lower.len()).find(|&i| text_lower[i] == qc)?;
        let word_start = found == 0 || !text_lower[found - 1].is_alphanumeric();
        score += 1 + if word_start { 8 } else { 0 } + if last == Some(found.wrapping_sub(1)) { 5 } else { 0 };
        score -= (found as i32 - pos as i32).min(10) / 3;
        last = Some(found);
        pos = found + 1;
    }
    Some(score)
}

impl<D: PickerDelegate> Render for Picker<D> {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let selected_bg = theme.bg_option;
        let context = self.delegate.context(cx);

        let mut rows = Vec::new();
        let mut section: Option<SharedString> = None;
        for (ix, item) in self.items.iter().enumerate() {
            if item.section.is_some() && item.section != section {
                section = item.section.clone();
                rows.push(
                    div()
                        .h(px(30.))
                        .px(px(12.))
                        .pt(px(10.))
                        .text_size(px(SIZE_MINI))
                        .font_weight(WEIGHT_MEDIUM)
                        .text_color(theme.text_tertiary)
                        .child(section.clone().unwrap_or_default())
                        .into_any_element(),
                );
            }
            let active = ix == self.selected;
            let leading = match &item.leading {
                Leading::Icon(icon) => Some(Icon::new(icon.clone()).size(16.).color(if active { theme.text_primary } else { theme.text_meta }).into_any_element()),
                Leading::Dot(color) => Some(div().size(px(9.)).rounded_full().bg(*color).into_any_element()),
                Leading::None => None,
            };
            rows.push(
                div()
                    .id(("picker-item", ix))
                    .relative()
                    .flex()
                    .items_center()
                    .gap(px(12.))
                    .h(px(40.))
                    .px(px(12.))
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_NORMAL)
                    .text_color(theme.text_primary)
                    // the pill is inset 2px top/bottom
                    .when(active, |s| {
                        s.child(div().absolute().left_0().right_0().top(px(2.)).bottom(px(2.)).rounded(theme.radius.md).bg(selected_bg))
                    })
                    .when_some(item.checked, |s, checked| {
                        s.child(
                            div()
                                .relative()
                                .flex()
                                .flex_none()
                                .items_center()
                                .justify_center()
                                .size(px(14.))
                                .rounded(theme.radius.xs)
                                .border_1()
                                .border_color(if checked { theme.accent } else { theme.text_quaternary })
                                .when(checked, |s| s.bg(theme.accent))
                                .when(checked, |s| s.child(Icon::new(IconName::StatusCheck).size(12.).color(gpui::white()))),
                        )
                    })
                    .children(leading.map(|el| div().relative().flex().flex_none().items_center().justify_center().w(px(16.)).child(el)))
                    .child(
                        div()
                            .relative()
                            .flex()
                            .flex_1()
                            .min_w_0()
                            .items_baseline()
                            .gap(px(8.))
                            .child(div().flex_none().max_w(px(self.width * 0.7)).truncate().child(item.label.clone()))
                            .children(item.detail.clone().map(|d| div().min_w_0().truncate().text_color(theme.text_tertiary).child(d))),
                    )
                    .when(!item.keys.is_empty(), |s| {
                        s.child(div().relative().flex().flex_none().gap(px(4.)).children(item.keys.iter().cloned().map(Kbd::new)))
                    })
                    .on_mouse_move(cx.listener(move |this, _, _, cx| {
                        if this.selected != ix {
                            this.selected = ix;
                            cx.notify();
                        }
                    }))
                    .on_mouse_down(MouseButton::Left, cx.listener(move |this, _, window, cx| {
                        this.selected = ix;
                        this.confirm(&ConfirmItem, window, cx);
                    }))
                    .into_any_element(),
            );
        }
        let empty = rows.is_empty();

        div()
            .key_context(CONTEXT)
            .on_action(cx.listener(Self::select_next))
            .on_action(cx.listener(Self::select_prev))
            .on_action(cx.listener(Self::confirm))
            .on_action(cx.listener(Self::dismiss))
            // keep clicks inside from reaching the dismissing backdrop
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .w(px(self.width))
            .flex()
            .flex_col()
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.xl)
            .shadow(theme.overlay_shadow())
            .text_color(theme.text_body)
            .when_some(context, |s, (prefix, title)| {
                s.child(
                    div().flex().items_end().h(px(34.)).px(px(14.)).child(
                        div()
                            .flex()
                            .gap(px(4.))
                            .max_w_full()
                            .px(px(6.))
                            .py(px(2.))
                            .rounded(theme.radius.lg)
                            .bg(selected_bg)
                            .text_size(px(SIZE_MINI))
                            .truncate()
                            .child(div().flex_none().font_weight(WEIGHT_MEDIUM).opacity(0.5).child(format!("{prefix} ⋅")))
                            .child(div().min_w_0().truncate().child(title)),
                    ),
                )
            })
            .child(
                div()
                    .px(px(6.))
                    .pt(px(6.))
                    .child(div().px(px(12.)).py(px(11.)).text_size(px(SIZE_REGULAR)).line_height(px(18.)).child(self.input.clone())),
            )
            .child(div().h(px(1.)).border_b_hairline().border_color(theme.border_card))
            .child(
                div()
                    .id("picker-list")
                    .track_scroll(&self.scroll)
                    .max_h(px(370.))
                    .overflow_y_scroll()
                    .px(px(6.))
                    .py(px(6.))
                    .children(rows)
                    .when(empty, |s| {
                        s.child(
                            div()
                                .h(px(40.))
                                .px(px(12.))
                                .flex()
                                .items_center()
                                .text_size(px(SIZE_SMALL))
                                .text_color(theme.text_tertiary)
                                .child("No results"),
                        )
                    }),
            )
    }
}
