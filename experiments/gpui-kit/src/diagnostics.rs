//! Opt-in, debug-build-only input traces for synthetic demo runs.
//! No message text, query text, clipboard contents, or live-account data.

use gpui_kit::*;
use serde_json::{Value, json};
use std::sync::{
    OnceLock,
    atomic::{AtomicU64, Ordering},
};

fn enabled() -> bool {
    static ENABLED: OnceLock<bool> = OnceLock::new();
    *ENABLED.get_or_init(|| {
        cfg!(debug_assertions)
            && std::env::var("DURIAN_INPUT_TRACE").as_deref() == Ok("1")
            && !std::env::args().any(|arg| arg == "--live")
    })
}

pub fn record(stage: &str, details: impl FnOnce() -> Value) {
    static SEQUENCE: AtomicU64 = AtomicU64::new(0);
    if enabled() {
        eprintln!(
            "DURIAN_INPUT {}",
            json!({
                "seq": SEQUENCE.fetch_add(1, Ordering::Relaxed),
                "stage": stage,
                "details": details(),
            })
        );
    }
}

pub fn init(cx: &mut App) {
    if !enabled() {
        return;
    }
    for (stage, before) in [("gpui.received", true), ("gpui.resolved", false)] {
        let observer = move |event: &KeystrokeEvent, window: &mut Window, _: &mut App| {
            record(stage, || {
                json!({
                    "slash": event.keystroke.key == "/",
                    "key_length": event.keystroke.key.chars().count(),
                    "modifiers": format!("{:?}", event.keystroke.modifiers),
                    "action": event.action.as_deref().map(Action::name),
                    "input_context": event.context_stack.iter().any(|context| context.contains("Input")),
                    "mail_context": event.context_stack.iter().any(|context| context.contains("MailApp")),
                    "picker_context": event.context_stack.iter().any(|context| context.contains("MailPicker")),
                    "a11y_active": window.is_a11y_active(),
                })
            });
        };
        let subscription = if before {
            cx.intercept_keystrokes(observer)
        } else {
            cx.observe_keystrokes(observer)
        };
        // App-lifetime, passive observers; never consume an event or change focus.
        subscription.detach();
    }
    record("trace.enabled", || json!({"demo_only": true}));
}
