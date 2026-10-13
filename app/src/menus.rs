//! Native application chrome; configurable mail shortcuts still belong to keymaps.pkl.

use gpui::{App, KeyBinding, Menu, MenuItem, OsAction, PromptLevel, SystemMenuType, actions};

use crate::Quit;

actions!(durian, [About, Hide, HideOthers, ShowAll, Minimize, Zoom, CloseWindow]);

pub fn init(cx: &mut App) {
    cx.on_action(|_: &Hide, cx| cx.hide());
    cx.on_action(|_: &HideOthers, cx| cx.hide_other_apps());
    cx.on_action(|_: &ShowAll, cx| cx.unhide_other_apps());
    cx.on_action(|_: &Minimize, cx| with_window(cx, |window, _| window.minimize_window()));
    cx.on_action(|_: &Zoom, cx| with_window(cx, |window, _| window.zoom_window()));
    cx.on_action(|_: &CloseWindow, cx| with_window(cx, |window, _| window.remove_window()));
    cx.on_action(|_: &About, cx| with_window(cx, |window, cx| {
        let detail = format!(
            "Experimental Rust client\nVersion {} ({})\n\nUses your Durian configuration and Go backend.\nCalendar provider writes are not yet approved for real calendars.",
            option_env!("DURIAN_VERSION").unwrap_or("development"),
            option_env!("DURIAN_REVISION").unwrap_or("unversioned"),
        );
        let answer = window.prompt(PromptLevel::Info, "Durian Preview", Some(&detail), &["OK"], cx);
        cx.spawn(async move |_| { let _ = answer.await; }).detach();
    }));
}

fn with_window(cx: &mut App, f: impl FnOnce(&mut gpui::Window, &mut App) + 'static) {
    if let Some(handle) = cx.active_window() {
        // A global action may bubble from inside an ongoing window update.
        cx.defer(move |cx| { let _ = handle.update(cx, |_, window, cx| f(window, cx)); });
    }
}

/// Called again after keymap reload, because native menus cache their key equivalents.
pub fn bind_keys(cx: &mut App) {
    if !cfg!(target_os = "macos") { return; }
    cx.bind_keys([
        KeyBinding::new("cmd-h", Hide, None),
        KeyBinding::new("cmd-alt-h", HideOthers, None),
        KeyBinding::new("cmd-m", Minimize, None),
        KeyBinding::new("cmd-w", CloseWindow, None),
    ]);
    cx.set_menus([
        Menu::new("Durian Preview").items([
            MenuItem::action("About Durian Preview", About),
            MenuItem::separator(),
            MenuItem::os_submenu("Services", SystemMenuType::Services),
            MenuItem::separator(),
            MenuItem::action("Hide Durian Preview", Hide),
            MenuItem::action("Hide Others", HideOthers),
            MenuItem::action("Show All", ShowAll),
            MenuItem::separator(),
            MenuItem::action("Quit Durian Preview", Quit),
        ]),
        Menu::new("File").items([
            MenuItem::action("New Message", mail::app_menu::NewMessage),
            MenuItem::action("New Event", mail::app_menu::NewEvent),
            MenuItem::separator(),
            MenuItem::action("Close Window", CloseWindow),
        ]),
        Menu::new("Edit").items([
            MenuItem::os_action("Undo", ui::Undo, OsAction::Undo),
            MenuItem::os_action("Redo", ui::Redo, OsAction::Redo),
            MenuItem::separator(),
            MenuItem::os_action("Cut", ui::Cut, OsAction::Cut),
            MenuItem::os_action("Copy", ui::Copy, OsAction::Copy),
            MenuItem::os_action("Paste", ui::Paste, OsAction::Paste),
            MenuItem::os_action("Select All", ui::SelectAll, OsAction::SelectAll),
            MenuItem::separator(),
            MenuItem::action("Find in Mail", mail::app_menu::Find),
        ]),
        Menu::new("View").items([
            MenuItem::action("Mail", mail::app_menu::Mail),
            MenuItem::action("Calendar", mail::app_menu::Calendar),
            MenuItem::separator(),
            MenuItem::action("Refresh from Server", mail::app_menu::Refresh),
        ]),
        Menu::new("Window").items([
            MenuItem::action("Minimize", Minimize),
            MenuItem::action("Zoom", Zoom),
        ]),
        Menu::new("Help").items([
            MenuItem::action("Keyboard Shortcuts", mail::app_menu::Shortcuts),
            MenuItem::action("Reload Keymaps", mail::app_menu::ReloadKeymaps),
        ]),
    ]);
}
