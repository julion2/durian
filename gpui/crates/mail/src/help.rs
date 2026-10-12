//! Keyboard shortcuts panel (`?`): a sheet on the right with a filter field and
//! grouped shortcuts from the effective keymap. Missing Swift actions are explicit.

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
    ("Search", "Accept completion", &["tab"]),
    ("Search", "Back to the list", &["enter"]),
    ("Search", "Clear search", &["escape"]),
];

const GROUP_ORDER: &[&str] = &["General", "List", "Calendar", "Thread", "Search", "Tag picker", "Composer", "Compose normal", "Unavailable"];

struct Row {
    group: &'static str,
    label: SharedString,
    keys: Vec<Vec<SharedString>>,
}

/// The shortcuts of the area the sheet was opened in (plus the ones that work everywhere).
fn rows(scope: Scope, cx: &App) -> Vec<Row> {
    let mut rows: Vec<Row> = COMMANDS
        .iter()
        .filter(|c| crate::commands::pkl_name((c.action)().as_ref(), c.scope).is_none())
        .map(|c| Row {
            group: "General",
            label: c.label.into(),
            // alternatives like "j / ↓" – but not the duplicate "?" / "shift-/" spelling
            keys: c.keys.iter().filter(|k| **k != "shift-/").map(|k| keycaps(k)).collect(),
        })
        .collect();
    let maps = cx.global::<crate::keymaps::Keymaps>();
    for b in &maps.bindings {
        use crate::keymaps::Context;
        if (scope == Scope::Calendar) != (b.entry.context == Context::Calendar) { continue }
        let available = crate::keymap_actions::available(&b.entry);
        let label = match (b.entry.context, b.entry.action.as_str()) {
            (Context::List, "first_email") => "First loaded thread".into(),
            (Context::List, "last_email") => "Last loaded thread".into(),
            (Context::List, "enter_visual_mode") => "Select range".into(),
            (Context::List, "enter_toggle_mode") => "Select individual threads".into(),
            (Context::List, "toggle_selection") => "Toggle mark and advance".into(),
            (Context::List, "exit_visual_mode") => "Clear selection".into(),
            (Context::Thread, "next_message") => "Next message".into(),
            (Context::Thread, "prev_message") => "Previous message".into(),
            (Context::Thread, "first_email") => "First message".into(),
            (Context::Thread, "last_email") => "Last message / end".into(),
            _ => COMMANDS.iter().find(|c| (c.scope == Scope::Any || c.scope == scope)
                && crate::commands::pkl_name((c.action)().as_ref(), c.scope) == Some(b.entry.action.as_str()))
                .map(|c| c.label.to_string()).unwrap_or_else(|| b.entry.action.replace('_', " ")),
        };
        let label = if available { label } else { format!("{}: {label} (not yet available)", b.entry.context.label()) };
        let group = if available { b.entry.context.label() } else { "Unavailable" };
        let keys = if maps.settings.keymaps_enabled { vec![keycaps(&b.strokes.join(" "))] } else { Vec::new() };
        if let Some(row) = rows.iter_mut().find(|r| r.group == group && r.label.as_ref() == label) { row.keys.extend(keys); }
        else { rows.push(Row { group, label: label.into(), keys }); }
    }
    // composer and search keys belong to the mail area
    rows.extend(EXTRA.iter().filter(|_| scope == Scope::Mail).map(|(group, label, keys)| Row {
        group,
        label: (*label).into(),
        keys: keys.iter().map(|k| keycaps(k)).collect(),
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
        let rows: Vec<Row> = rows(self.scope, cx).into_iter().filter(|r| fuzzy_score(&query, &r.label).is_some() || fuzzy_score(&query, r.group).is_some()).collect();

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
                            .min_h(px(30.))
                            .py(px(4.))
                            .gap(px(8.))
                            .text_size(px(SIZE_SMALL))
                            .text_color(theme.text_secondary)
                            .child(div().flex_1().min_w_0().truncate().child(row.label.clone()))
                            .child(div().flex().flex_wrap().justify_end().max_w(px(200.)).items_center().gap(px(6.)).children(row.keys.iter().enumerate().map(|(i, combo)| {
                                div()
                                    .flex()
                                    .flex_none()
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
