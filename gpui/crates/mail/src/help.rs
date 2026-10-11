//! Keyboard shortcuts panel (`?`): a sheet on the right with a filter field and
//! grouped shortcuts. Everything bindable comes from `COMMANDS`, so it can't drift from the real
//! key bindings; composer-only keys (not actions of the mail view) are listed alongside.

use gpui::{
    App, Context, Entity, EventEmitter, FocusHandle, Focusable, IntoElement, KeyBinding, MouseButton, Render, SharedString,
    Subscription, Window, actions, div, prelude::*, px,
};

use crate::commands::{COMMANDS, Scope, keycaps};
use ui::theme::{ActiveTheme, typography::*};
use ui::{IconButton, IconName, Kbd, StyledExt, TextInput, TextInputEvent, fuzzy_score};

actions!(shortcut_help, [CloseShortcutHelp]);

pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([KeyBinding::new("escape", CloseShortcutHelp, Some("ShortcutHelp"))]);
}

pub enum ShortcutHelpEvent {
    Close,
}

/// Keys that live in text fields / dialogs rather than the mail view.
const EXTRA: &[(&str, &str, &[&str])] = &[
    ("Composer", "Send", &["cmd-enter"]),
    ("Composer", "Bold / italic / code", &["cmd-b", "cmd-i", "cmd-e"]),
    ("Composer", "Next / previous field", &["tab", "shift-tab"]),
    ("Composer", "Close (asks to save a draft)", &["escape"]),
    ("Navigation", "Move by N threads", &["5", "j"]),
    ("Search", "Accept completion", &["tab"]),
    ("Search", "Back to the list", &["enter"]),
    ("Search", "Clear search", &["escape"]),
];

const GROUP_ORDER: &[&str] = &["General", "Navigation", "Calendar", "Thread", "Writing", "Composer", "Search", "Accounts"];

struct Row {
    group: &'static str,
    label: SharedString,
    keys: Vec<Vec<SharedString>>,
}

/// The shortcuts of the area the sheet was opened in (plus the ones that work everywhere).
fn rows(scope: Scope) -> Vec<Row> {
    let mut rows: Vec<Row> = COMMANDS
        .iter()
        .filter(|c| !c.keys.is_empty() && (c.scope == Scope::Any || c.scope == scope))
        .map(|c| Row {
            group: c.group,
            label: c.label.into(),
            // alternatives like "j / ↓" – but not the duplicate "?" / "shift-/" spelling
            keys: c.keys.iter().filter(|k| **k != "shift-/").map(|k| keycaps(k)).collect(),
        })
        .collect();
    // composer and search keys belong to the mail area
    rows.extend(EXTRA.iter().filter(|_| scope == Scope::Mail).map(|(group, label, keys)| Row {
        group,
        label: (*label).into(),
        keys: if *label == "Move by N threads" {
            vec![vec!["5".into(), "J".into()]]
        } else {
            keys.iter().map(|k| keycaps(k)).collect()
        },
    }));
    rows
}

pub struct ShortcutHelp {
    scope: Scope,
    filter: Entity<TextInput>,
    focus: FocusHandle,
    _subscription: Subscription,
}

impl EventEmitter<ShortcutHelpEvent> for ShortcutHelp {}

impl Focusable for ShortcutHelp {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl ShortcutHelp {
    pub fn new(scope: Scope, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let filter = cx.new(|cx| TextInput::new("Search shortcuts", cx));
        let subscription = cx.subscribe(&filter, |_, _, TextInputEvent::Changed(_), cx| cx.notify());
        filter.read(cx).focus_handle(cx).focus(window, cx);
        Self { scope, filter, focus: cx.focus_handle(), _subscription: subscription }
    }
}

impl Render for ShortcutHelp {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let query = self.filter.read(cx).text().to_string();
        let rows: Vec<Row> = rows(self.scope).into_iter().filter(|r| fuzzy_score(&query, &r.label).is_some() || fuzzy_score(&query, r.group).is_some()).collect();

        let sections = GROUP_ORDER.iter().filter_map(|group| {
            let items: Vec<&Row> = rows.iter().filter(|r| r.group == *group).collect();
            (!items.is_empty()).then(|| {
                div()
                    .flex()
                    .flex_col()
                    .mb(px(18.))
                    .child(div().mb(px(6.)).text_size(px(SIZE_MINI)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_tertiary).child(*group))
                    .children(items.into_iter().map(|row| {
                        div()
                            .flex()
                            .items_center()
                            .h(px(30.))
                            .text_size(px(SIZE_SMALL))
                            .text_color(theme.text_secondary)
                            .child(div().flex_1().min_w_0().truncate().child(row.label.clone()))
                            .child(div().flex().items_center().gap(px(6.)).children(row.keys.iter().enumerate().map(|(i, combo)| {
                                div()
                                    .flex()
                                    .items_center()
                                    .gap(px(3.))
                                    .when(i > 0, |s| s.child(div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child("or")))
                                    .children(combo.iter().cloned().map(Kbd::new))
                            })))
                    }))
            })
        });

        div()
            .key_context("ShortcutHelp")
            .track_focus(&self.focus)
            .on_action(cx.listener(|_, _: &CloseShortcutHelp, _, cx| cx.emit(ShortcutHelpEvent::Close)))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .absolute()
            .top(px(50.))
            .right(px(16.))
            .bottom(px(16.))
            .w(px(380.))
            .flex()
            .flex_col()
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.xl)
            .shadow(theme.overlay_shadow())
            .child(
                div()
                    .flex()
                    .items_center()
                    .h(px(48.))
                    .pl(px(18.))
                    .pr(px(8.))
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_primary)
                    .child(div().flex_1().child("Keyboard shortcuts"))
                    .child(IconButton::new("close-help", IconName::CloseSmall).on_click(cx.listener(|_, _, _, cx| cx.emit(ShortcutHelpEvent::Close)))),
            )
            .child(
                div().px(px(12.)).pb(px(10.)).child(
                    div()
                        .flex()
                        .items_center()
                        .h(px(30.))
                        .px(px(10.))
                        .rounded(theme.radius.md)
                        .bg(theme.bg_raised)
                        .shadow(theme.raised_shadow(false))
                        .text_size(px(SIZE_SMALL))
                        .child(self.filter.clone()),
                ),
            )
            .child(div().id("help-scroll").flex_1().min_h_0().overflow_y_scroll().px(px(18.)).pt(px(8.)).children(sections))
    }
}
