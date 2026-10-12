//! Swift-compatible keymaps.pkl: merge slots, then match strokes in the active context.
//! The CLI evaluates Pkl; this module never reads or writes a user's config files.

use std::collections::BTreeSet;
use std::time::{Duration, Instant};

use gpui::{App, DummyKeyboardMapper, KeyBinding, KeyBindingContextPredicate};
use serde::Deserialize;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, PartialOrd, Ord, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Context {
    #[default]
    List,
    Search,
    TagPicker,
    Thread,
    ComposeNormal,
    Calendar,
}

impl Context {
    pub fn predicate(self) -> &'static str {
        match self {
            Self::List => "Mail && !ThreadKeys && !Picker && !Compose && !TextInput && !ContextMenu && !ShortcutHelp",
            Self::Thread => "Mail && ThreadKeys && !Picker && !Compose && !TextInput && !ContextMenu && !ShortcutHelp",
            Self::Calendar => "Calendar && !Picker && !Compose && !TextInput && !ContextMenu && !ShortcutHelp",
            Self::Search => "(SearchBar > TextInput) || ((Picker && !TagKeys) > TextInput)",
            Self::TagPicker => "TagKeys > Picker > TextInput",
            Self::ComposeNormal => "ComposeNormal",
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::List => "List",
            Self::Search => "Search",
            Self::TagPicker => "Tag picker",
            Self::Thread => "Thread",
            Self::ComposeNormal => "Compose normal",
            Self::Calendar => "Calendar",
        }
    }
}

#[derive(Clone, Debug, Deserialize)]
pub struct Entry {
    pub action: String,
    pub key: String,
    #[serde(default)]
    pub modifiers: Vec<String>,
    #[serde(default = "enabled")]
    pub enabled: bool,
    #[serde(default)]
    pub supports_count: bool,
    #[serde(default)]
    pub context: Context,
    #[serde(default)]
    pub tags: Option<String>,
}

fn enabled() -> bool {
    true
}
fn timeout() -> f64 {
    1.0
}

#[derive(Clone, Debug, Deserialize)]
pub struct Settings {
    #[serde(default = "enabled")]
    pub keymaps_enabled: bool,
    #[serde(default = "timeout")]
    pub sequence_timeout: f64,
}

impl Default for Settings {
    fn default() -> Self {
        Self { keymaps_enabled: true, sequence_timeout: timeout() }
    }
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct File {
    #[serde(default)]
    pub keymaps: Vec<Entry>,
    #[serde(default)]
    pub global_settings: Settings,
}

#[derive(Clone, Debug)]
pub struct Binding {
    pub entry: Entry,
    pub strokes: Vec<String>,
}

#[derive(Clone)]
pub struct Keymaps {
    pub bindings: Vec<Binding>,
    pub settings: Settings,
    disabled: Vec<Stroke>,
}

impl gpui::Global for Keymaps {}

impl Default for Keymaps {
    fn default() -> Self {
        Self::from_file(None).expect("built-in keymaps are valid")
    }
}

impl Keymaps {
    pub fn from_file(file: Option<File>) -> Result<Self, String> {
        let file = file.unwrap_or_default();
        if file.global_settings.sequence_timeout <= 0. || Duration::try_from_secs_f64(file.global_settings.sequence_timeout).is_err() {
            return Err("sequence_timeout must be a finite positive duration".into());
        }
        // Match Swift's key + sorted modifiers + context identity, not action identity.
        // Last entry wins, including a disabled entry after an enabled duplicate.
        let mut entries = defaults();
        for mut entry in file.keymaps {
            entry.modifiers.sort();
            entries.retain(|old| {
                !(old.key == entry.key && old.context == entry.context && {
                    let mut modifiers = old.modifiers.clone();
                    modifiers.sort();
                    modifiers == entry.modifiers
                })
            });
            entries.push(entry);
        }
        let mut bindings: Vec<Binding> = Vec::new();
        let mut disabled = Vec::new();
        for entry in entries {
            let strokes = strokes(&entry)?;
            if !entry.enabled {
                if text_safe(&entry) && strokes.len() == 1 {
                    disabled.push(Stroke { key: strokes[0].clone(), context: entry.context });
                }
                continue;
            }
            bindings.retain(|old| !(old.entry.context == entry.context && old.strokes == strokes));
            bindings.push(Binding { entry, strokes });
        }
        Ok(Self { bindings, settings: file.global_settings, disabled })
    }

    pub fn for_context(&self, context: Context) -> impl Iterator<Item = &Binding> {
        self.bindings.iter().filter(move |b| self.settings.keymaps_enabled && b.entry.context == context && text_safe(&b.entry))
    }

    pub fn bind(&self, cx: &mut App) {
        if !self.settings.keymaps_enabled {
            return;
        }
        let mut keys: BTreeSet<(Context, String)> =
            self.bindings.iter().filter(|b| text_safe(&b.entry)).flat_map(|b| b.strokes.iter().cloned().map(|s| (b.entry.context, s))).collect();
        // Also consume explicitly disabled native popup keys (e.g. Enter), so
        // a deeper TextInput binding cannot resurrect them. Basic editing stays intact.
        keys.extend(self.disabled.iter().map(|s| (s.context, s.key.clone())));
        for context in [Context::List, Context::Thread, Context::Calendar] {
            for digit in 0..=9 {
                keys.insert((context, digit.to_string()));
            }
        }
        let bindings = keys.into_iter().map(|(context, key)| {
            let predicate = KeyBindingContextPredicate::parse(context.predicate()).expect("keymap context");
            KeyBinding::load(&key, Box::new(Stroke { key: key.clone(), context }), Some(predicate.into()), false, None, &DummyKeyboardMapper)
                .expect("validated keymap stroke")
        });
        cx.bind_keys(bindings);
    }
}

#[derive(Clone, PartialEq, gpui::Action)]
#[action(no_json, no_register)]
pub struct Stroke {
    pub key: String,
    pub context: Context,
}

pub fn text_safe(entry: &Entry) -> bool {
    !matches!(entry.context, Context::Search | Context::TagPicker)
        || !entry.modifiers.is_empty()
        || ["Escape", "Enter", "Tab", "Down", "Up", "Left", "Right"].contains(&entry.key.as_str())
}

fn strokes(entry: &Entry) -> Result<Vec<String>, String> {
    let named = match entry.key.as_str() {
        "Escape" => Some("escape"),
        "Enter" => Some("enter"),
        "Tab" => Some("tab"),
        "Down" => Some("down"),
        "Up" => Some("up"),
        "Left" => Some("left"),
        "Right" => Some("right"),
        " " => Some("space"),
        _ => None,
    };
    if entry.key.is_empty() || entry.key.chars().count() > 64 {
        return Err("keys must contain 1–64 characters".into());
    }
    if entry.modifiers.iter().any(|m| !["cmd", "ctrl", "alt", "shift"].contains(&m.as_str())) {
        return Err("unknown modifier (use cmd, ctrl, alt or shift)".into());
    }
    if !entry.modifiers.is_empty() && named.is_none() && entry.key.chars().count() > 1 {
        return Err("modified bindings must name a single key".into());
    }
    let keys: Vec<String> = named.map(|k| vec![k.to_string()]).unwrap_or_else(|| entry.key.chars().map(|c| c.to_string()).collect());
    keys.into_iter()
        .map(|key| {
            let mut modifiers = entry.modifiers.clone();
            if key.len() == 1 && key.as_bytes()[0].is_ascii_uppercase() && modifiers.is_empty() {
                modifiers.push("shift".into());
            }
            modifiers.sort();
            modifiers.dedup();
            let key = if key == " " { "space".into() } else { key.to_ascii_lowercase() };
            let stroke = if modifiers.is_empty() { key } else { format!("{}-{key}", modifiers.join("-")) };
            KeyBinding::load(&stroke, Box::new(Stroke { key: stroke.clone(), context: entry.context }), None, false, None, &DummyKeyboardMapper)
                .map_err(|_| format!("invalid key in {} context", entry.context.label()))?;
            Ok(stroke)
        })
        .collect()
}

#[derive(Default)]
pub struct Engine {
    context: Option<Context>,
    keys: Vec<String>,
    count: Option<usize>,
    last: Option<Instant>,
}

pub struct Command {
    pub entry: Entry,
    pub count: usize,
}

impl Engine {
    pub fn clear(&mut self) {
        *self = Self::default();
    }

    pub fn display(&self) -> String {
        format!("{}{}", self.count.map(|n| n.to_string()).unwrap_or_default(), self.keys.join(" "))
    }

    pub fn feed(&mut self, stroke: &Stroke, maps: &Keymaps, now: Instant) -> Option<Command> {
        if self.context != Some(stroke.context) || self.last.is_some_and(|at| now.duration_since(at) >= Duration::from_secs_f64(maps.settings.sequence_timeout))
        {
            self.clear();
        }
        if !maps.settings.keymaps_enabled {
            self.clear();
            return None;
        }
        self.context = Some(stroke.context);
        self.last = Some(now);
        if self.keys.is_empty() && stroke.key.len() == 1 && stroke.key.as_bytes()[0].is_ascii_digit() {
            self.count = Some((self.count.unwrap_or(0) * 10 + stroke.key.parse::<usize>().unwrap()).min(999));
            return None;
        }
        self.keys.push(stroke.key.clone());
        let bindings: Vec<_> = maps.for_context(stroke.context).collect();
        // Exact wins over partial, just like Swift. The sequence flag isn't required.
        if let Some(binding) = bindings.iter().find(|b| b.strokes == self.keys) {
            let supports_count = bindings.iter().any(|b| b.entry.action == binding.entry.action && b.entry.supports_count);
            let command = Command { entry: binding.entry.clone(), count: if supports_count { self.count.unwrap_or(1) } else { 1 } };
            self.clear();
            return Some(command);
        }
        if !bindings.iter().any(|b| b.strokes.starts_with(&self.keys)) {
            self.clear();
        }
        None
    }
}

/// Defaults mirror KeymapsManager.swift. Actions not implemented in this client stay
/// visible in help and report an explicit explanation rather than doing something else.
fn defaults() -> Vec<Entry> {
    let mut entries = Vec::new();
    let mut add = |context, action: &str, key: &str, modifiers: &[&str], supports_count| {
        entries.push(Entry {
            action: action.into(),
            key: key.into(),
            modifiers: modifiers.iter().map(|s| s.to_string()).collect(),
            enabled: true,
            supports_count,
            context,
            tags: None,
        });
    };
    use Context::*;
    for (action, key, count) in [
        ("next_email", "j", true),
        ("prev_email", "k", true),
        ("next_email", "Down", false),
        ("prev_email", "Up", false),
        ("last_email", "G", false),
        ("first_email", "gg", false),
        ("archive", "a", false),
        ("compose", "c", false),
        ("reply", "r", false),
        ("reply_all", "R", false),
        ("forward", "f", false),
        ("toggle_read", "u", false),
        ("toggle_star", "s", false),
        ("delete", "dd", true),
        ("go_inbox", "gi", false),
        ("go_sent", "gs", false),
        ("go_drafts", "gd", false),
        ("go_archive", "ga", false),
        ("next_folder", "J", false),
        ("prev_folder", "K", false),
        ("folder_picker", "gf", false),
        ("search", "/", false),
        ("tag_picker", "t", false),
        ("close_detail", "q", false),
        ("enter_visual_mode", "v", false),
        ("enter_toggle_mode", "V", false),
        ("toggle_selection", " ", false),
        ("exit_visual_mode", "Escape", false),
        ("enter_thread", "l", false),
        ("go_calendar", "gc", false),
    ] {
        add(List, action, key, &[], count);
    }
    for n in 1..=9 {
        add(List, "go_folder", &format!("g{n}"), &[], false);
    }
    add(List, "search", "/", &["cmd"], false);
    // Keep the client's existing explicit refresh in every non-editing area.
    for context in [List, Thread, Calendar] {
        add(context, "reload_inbox", "r", &["cmd"], false);
        #[cfg(not(target_os = "macos"))]
        add(context, "reload_inbox", "r", &["ctrl"], false);
    }
    for context in [List, Thread] {
        add(context, "page_down", "d", &["ctrl"], true);
        add(context, "page_up", "u", &["ctrl"], true);
    }
    for context in [Search, TagPicker] {
        for (key, action) in [("j", "select_next"), ("k", "select_prev"), ("n", "select_next"), ("p", "select_prev")] {
            add(context, action, key, &["ctrl"], false);
        }
    }
    add(ComposeNormal, "exit_insert", "jk", &[], false);
    for (action, key, count) in [
        ("scroll_down", "j", true),
        ("scroll_up", "k", true),
        ("next_message", "n", true),
        ("prev_message", "N", true),
        ("first_email", "gg", false),
        ("last_email", "G", false),
        ("close_detail", "h", false),
        ("close_detail", "Escape", false),
        ("reply", "r", false),
    ] {
        add(Thread, action, key, &[], count);
    }
    for (action, key, count) in [
        ("next_email", "j", true),
        ("prev_email", "k", true),
        ("first_email", "gg", false),
        ("last_email", "G", false),
        ("calendar_prev_day", "h", true),
        ("calendar_next_day", "l", true),
        ("calendar_prev_period", "H", false),
        ("calendar_next_period", "L", false),
        ("calendar_prev_period", "[", false),
        ("calendar_next_period", "]", false),
        ("calendar_command", ":", false),
        ("calendar_detail", "v", false),
        ("calendar_today", "t", false),
        ("calendar_edit", "i", false),
        ("calendar_new", "n", false),
        ("calendar_new", "o", false),
        ("calendar_new", "O", false),
        ("calendar_delete", "dd", false),
        ("calendar_toggle_sidebar", "s", false),
        ("calendar_toggle_declined", "D", false),
        ("close_detail", "q", false),
        ("close_detail", "Escape", false),
    ] {
        add(Calendar, action, key, &[], count);
    }
    // GPUI-only commands use the same override mechanism. Swift's slots win;
    // e.g. Space is selection, never the old attachment-preview shortcut.
    let swift_actions: BTreeSet<_> = entries.iter().map(|e| e.action.clone()).collect();
    for command in crate::commands::COMMANDS {
        let Some(action) = crate::commands::pkl_name((command.action)().as_ref(), command.scope) else { continue };
        if swift_actions.contains(action) {
            continue;
        }
        let contexts: &[Context] = match command.scope {
            crate::commands::Scope::Mail => &[List, Thread],
            crate::commands::Scope::Calendar => &[Calendar],
            crate::commands::Scope::Any => &[List, Thread, Calendar],
        };
        for &context in contexts {
            for key in command.keys {
                let mut parts: Vec<_> = key.split('-').collect();
                let key = match parts.pop().unwrap() {
                    "enter" => "Enter",
                    "escape" => "Escape",
                    "space" => " ",
                    other => other,
                };
                let entry = Entry {
                    action: action.into(),
                    key: key.into(),
                    modifiers: parts.iter().map(|s| s.to_string()).collect(),
                    context,
                    enabled: true,
                    supports_count: false,
                    tags: None,
                };
                let candidate = strokes(&entry).expect("native command shortcut");
                // An extra single-key binding must not swallow a Swift sequence
                // (the old calendar Day-view `d` prevented `dd` from deleting).
                if !entries.iter().any(|e| e.context == context && strokes(e).is_ok_and(|s| s.starts_with(&candidate) || candidate.starts_with(&s))) {
                    entries.push(entry);
                }
            }
        }
    }
    entries
}

#[cfg(test)]
mod tests {
    use super::*;

    fn maps(json: &str) -> Keymaps {
        Keymaps::from_file(Some(serde_json::from_str(json).unwrap())).unwrap()
    }
    fn feed(engine: &mut Engine, maps: &Keymaps, context: Context, key: &str, at: Instant) -> Option<(String, usize)> {
        engine.feed(&Stroke { context, key: key.into() }, maps, at).map(|c| (c.entry.action, c.count))
    }

    #[test]
    fn swift_defaults_have_distinct_actions_and_contexts() {
        let maps = Keymaps::default();
        let now = Instant::now();
        for (context, keys, action, count) in [
            (Context::List, vec!["a"], "archive", 1),
            (Context::List, vec!["shift-r"], "reply_all", 1),
            (Context::List, vec!["f"], "forward", 1),
            (Context::List, vec!["t"], "tag_picker", 1),
            (Context::List, vec!["l"], "enter_thread", 1),
            (Context::List, vec!["g", "g"], "first_email", 1),
            (Context::List, vec!["3", "down"], "next_email", 3),
            (Context::Thread, vec!["3", "j"], "scroll_down", 3),
            (Context::Calendar, vec!["shift-l"], "calendar_next_period", 1),
            (Context::Calendar, vec!["d", "d"], "calendar_delete", 1),
            (Context::Search, vec!["ctrl-j"], "select_next", 1),
            (Context::TagPicker, vec!["ctrl-p"], "select_prev", 1),
        ] {
            let mut engine = Engine::default();
            let actions: Vec<_> = keys.into_iter().filter_map(|key| feed(&mut engine, &maps, context, key, now)).collect();
            assert_eq!(actions, vec![(action.into(), count)]);
        }
    }

    #[test]
    fn user_slots_disable_override_and_restore_without_changing_other_contexts() {
        let maps = maps(
            r#"{"keymaps":[
            {"action":"next_email","key":"j","enabled":false},
            {"action":"prev_email","key":"x"},
            {"action":"next_email","key":"x","supports_count":true},
            {"action":"archive","key":"y","modifiers":["ctrl","alt"]},
            {"action":"forward","key":"y","modifiers":["alt","ctrl"]}
        ]}"#,
        );
        let mut engine = Engine::default();
        let now = Instant::now();
        assert_eq!(feed(&mut engine, &maps, Context::List, "j", now), None);
        assert_eq!(feed(&mut engine, &maps, Context::Thread, "j", now), Some(("scroll_down".into(), 1)));
        assert_eq!(feed(&mut engine, &maps, Context::List, "4", now), None);
        assert_eq!(feed(&mut engine, &maps, Context::List, "x", now), Some(("next_email".into(), 4)));
        assert_eq!(feed(&mut engine, &maps, Context::List, "alt-ctrl-y", now), Some(("forward".into(), 1)));
        assert_eq!(feed(&mut engine, &Keymaps::default(), Context::List, "j", now), Some(("next_email".into(), 1)));
    }

    #[test]
    fn count_timeout_mismatch_and_context_switch_do_not_leak() {
        let maps = maps(r#"{"global_settings":{"sequence_timeout":0.35}}"#);
        let mut engine = Engine::default();
        let now = Instant::now();
        feed(&mut engine, &maps, Context::List, "g", now);
        assert_eq!(feed(&mut engine, &maps, Context::List, "g", now + Duration::from_millis(349)), Some(("first_email".into(), 1)));
        feed(&mut engine, &maps, Context::List, "g", now);
        assert_eq!(feed(&mut engine, &maps, Context::List, "g", now + Duration::from_millis(350)), None);
        assert_eq!(feed(&mut engine, &maps, Context::Calendar, "g", now + Duration::from_millis(351)), None);
        feed(&mut engine, &maps, Context::Calendar, "z", now + Duration::from_millis(352));
        assert!(engine.display().is_empty());
        feed(&mut engine, &maps, Context::List, "6", now + Duration::from_secs(1));
        assert_eq!(feed(&mut engine, &maps, Context::List, "j", now + Duration::from_millis(1350)), Some(("next_email".into(), 1)));
        feed(&mut engine, &maps, Context::List, "7", now + Duration::from_secs(2));
        assert_eq!(feed(&mut engine, &maps, Context::List, "a", now + Duration::from_millis(2001)), Some(("archive".into(), 1)));
    }

    #[test]
    fn named_keys_do_not_create_character_prefixes_and_exact_wins() {
        let maps = maps(r#"{"keymaps":[{"action":"archive","key":"g"}]}"#);
        let mut engine = Engine::default();
        let now = Instant::now();
        assert_eq!(feed(&mut engine, &maps, Context::List, "g", now), Some(("archive".into(), 1)));
        feed(&mut engine, &maps, Context::List, "shift-e", now);
        assert!(engine.display().is_empty());
        assert_eq!(feed(&mut engine, &maps, Context::List, "escape", now), Some(("exit_visual_mode".into(), 1)));
    }

    #[test]
    fn disabled_map_and_text_contexts_never_turn_typing_into_commands() {
        let maps = maps(r#"{"keymaps":[{"action":"delete","key":"dd","context":"search"}]}"#);
        let mut engine = Engine::default();
        let now = Instant::now();
        assert_eq!(feed(&mut engine, &maps, Context::Search, "d", now), None);
        assert!(engine.display().is_empty());
        assert_eq!(feed(&mut engine, &maps, Context::Search, "d", now), None);
        let mut disabled = maps;
        disabled.settings.keymaps_enabled = false;
        assert_eq!(feed(&mut engine, &disabled, Context::List, "a", now), None);
        assert_eq!(feed(&mut engine, &disabled, Context::List, "5", now), None);
        assert!(engine.display().is_empty());
    }
}
