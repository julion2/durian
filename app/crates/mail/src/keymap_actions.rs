//! Workspace adapter for the shared keymap vocabulary. All writes use Mailbox intents.

use std::time::{Duration, Instant};

use gpui::{App, Context, SharedString, WeakEntity, Window};
use ui::{AfterConfirm, PickerDelegate, PickerItem};

use crate::keymaps::{self, Keymaps};
use crate::{MailWorkspace, ToastAction, commands, thread_list};

pub(super) fn available(entry: &keymaps::Entry) -> bool {
    if entry.context == keymaps::Context::ComposeNormal || !keymaps::text_safe(entry) {
        return false;
    }
    commands::action_for(&entry.action).is_some()
        || matches!(
            entry.action.as_str(),
            "enter_thread"
                | "enter_visual_mode"
                | "enter_toggle_mode"
                | "toggle_selection"
                | "exit_visual_mode"
                | "next_message"
                | "prev_message"
                | "first_email"
                | "last_email"
                | "page_down"
                | "page_up"
                | "scroll_down"
                | "scroll_up"
                | "delete"
                | "tag_op"
                | "go_sent"
                | "go_archive"
                | "go_folder"
                | "next_folder"
                | "prev_folder"
                | "folder_picker"
                | "select_next"
                | "select_prev"
                | "confirm_selection"
                | "close_popup"
                | "calendar_toggle_sidebar"
        )
}

impl MailWorkspace {
    pub(super) fn clear_key_sequence(&mut self) {
        self.key_engine.clear();
        self.key_timer = None;
        self.key_focus = None;
    }

    pub(super) fn reload_keymaps(&mut self, report: bool, cx: &mut Context<Self>) {
        let Some(client) = self.live.as_ref().map(|l| l.read(cx).client()) else { return };
        self.key_load = Some(cx.spawn(async move |this, cx| {
            let result = cx.background_executor().spawn(async move { client.keymaps() }).await;
            let _ = this.update(cx, |this, cx| {
                this.keymaps_ready = true;
                this.clear_key_sequence();
                let result = result.map_err(|e| e.to_string()).and_then(Keymaps::from_file);
                match result {
                    Ok(maps) => {
                        let unavailable = maps.bindings.iter().filter(|b| !available(&b.entry)).count();
                        cx.set_global(maps);
                        crate::trace::note(&format!("keymaps loaded; {unavailable} unavailable bindings listed in help"));
                        if report {
                            this.show_toast("Keymaps reloaded".into(), ToastAction::None, cx);
                        }
                    }
                    // Keep the last working map during a bad edit or server outage.
                    // On first launch that map is the built-in Swift-compatible default.
                    Err(error) => this.show_toast(format!("Keymaps not loaded; keeping current bindings: {error}").into(), ToastAction::None, cx),
                }
                cx.notify();
            });
        }));
    }

    pub(super) fn key_stroke(&mut self, stroke: &keymaps::Stroke, window: &mut Window, cx: &mut Context<Self>) {
        if !self.keymaps_ready {
            return;
        }
        let focus = window.focused(cx);
        if self.key_focus != focus {
            self.clear_key_sequence();
        }
        self.key_focus = focus;
        let maps = cx.global::<Keymaps>();
        let command = self.key_engine.feed(stroke, maps, Instant::now());
        let timeout = Duration::from_secs_f64(maps.settings.sequence_timeout);
        self.key_timer = None;
        if let Some(command) = command {
            self.run_key_command(command, window, cx);
        } else if !self.key_engine.display().is_empty() {
            self.key_timer = Some(cx.spawn(async move |this, cx| {
                cx.background_executor().timer(timeout).await;
                let _ = this.update(cx, |this, cx| {
                    this.clear_key_sequence();
                    cx.notify();
                });
            }));
        }
        cx.notify();
    }

    fn run_key_command(&mut self, command: keymaps::Command, window: &mut Window, cx: &mut Context<Self>) {
        let (entry, count) = (command.entry, command.count);
        let action = entry.action.as_str();
        let direction: isize = if matches!(action, "prev_email" | "prev_message" | "page_up" | "scroll_up" | "prev_folder") { -1 } else { 1 };
        match action {
            "next_email" | "prev_email" => {
                if entry.context == keymaps::Context::Calendar {
                    self.schedule.update(cx, |s, cx| s.select_relative(direction * count as isize, cx));
                } else {
                    self.direction = direction;
                    self.mailbox.update(cx, |m, cx| m.select_relative(direction * count as isize, cx));
                }
            }
            "first_email" | "last_email" => {
                let last = action == "last_email";
                match entry.context {
                    keymaps::Context::Thread => self.thread.update(cx, |t, cx| t.message_edge(last, cx)),
                    keymaps::Context::Calendar => self.schedule.update(cx, |s, cx| {
                        let days = s.days();
                        let mut events = s.events.iter().filter(|e| days.iter().any(|d| e.on(*d)));
                        let id = if last { events.next_back() } else { events.next() }.map(|e| e.id.clone());
                        s.select(id, cx);
                    }),
                    _ => self.mailbox.update(cx, |m, cx| {
                        let visible: Vec<_> = m.visible().collect();
                        if let Some(&ix) = if last { visible.last() } else { visible.first() } {
                            m.select(ix, cx);
                        }
                    }),
                }
            }
            "scroll_down" | "scroll_up" | "page_down" | "page_up" => {
                if entry.context == keymaps::Context::Thread {
                    self.thread.update(cx, |t, cx| t.scroll_by(direction * count as isize, action.starts_with("page_"), cx));
                } else {
                    let page = self.list.read(cx).page_size(window);
                    self.mailbox.update(cx, |m, cx| m.select_relative(direction * count as isize * page as isize, cx));
                }
            }
            "enter_thread" => self.set_focus_mode(true, cx),
            "enter_visual_mode" | "enter_toggle_mode" => self.mailbox.update(cx, |m, cx| m.begin_selection(action == "enter_toggle_mode", cx)),
            "toggle_selection" => self.mailbox.update(cx, |m, cx| m.toggle_selection(cx)),
            "exit_visual_mode" => {
                if self.mailbox.read(cx).selection_mode().is_some() {
                    self.mailbox.update(cx, |m, cx| m.clear_selection(cx));
                } else {
                    self.set_focus_mode(false, cx);
                }
            }
            "next_message" | "prev_message" => self.thread.update(cx, |t, cx| t.navigate_message(direction * count as isize, cx)),
            "tag_op" | "delete" => {
                let ops = if action == "delete" { "+trash -inbox -archive" } else { entry.tags.as_deref().unwrap_or("") };
                self.mailbox.update(cx, |m, cx| m.tag_selected(ops, if action == "delete" { count } else { 1 }, cx));
            }
            "go_inbox" | "go_sent" | "go_archive" => {
                let tag = match action {
                    "go_sent" => "sent",
                    "go_archive" => "archive",
                    _ => "inbox",
                };
                let folder = self.mailbox.read(cx).active_profile().and_then(|p| p.folders.iter().position(|f| f.query.as_ref() == format!("tag:{tag}")));
                let key = folder.map(|ix| format!("folder:{ix}")).unwrap_or_else(|| format!("tag:{tag}"));
                self.navigate(key.into(), window, cx);
            }
            "go_folder" | "next_folder" | "prev_folder" => {
                let m = self.mailbox.read(cx);
                let folders: Vec<_> = m
                    .active_profile()
                    .map(|p| p.folders.iter().enumerate().filter(|(_, f)| !f.query.is_empty()).map(|(ix, _)| ix).collect())
                    .unwrap_or_default();
                let current = m.folder.and_then(|ix| folders.iter().position(|i| *i == ix)).unwrap_or(0);
                let target = if action == "go_folder" {
                    entry.key.chars().last().and_then(|c| c.to_digit(10)).and_then(|n| n.checked_sub(1)).map(|n| n as usize)
                } else if folders.is_empty() {
                    None
                } else {
                    Some((current as isize + direction).rem_euclid(folders.len() as isize) as usize)
                };
                if let Some(ix) = target.and_then(|i| folders.get(i)) {
                    self.navigate(format!("folder:{ix}").into(), window, cx);
                }
            }
            "folder_picker" => {
                let folders = self.mailbox.read(cx).active_profile().map(|p| p.folders.clone()).unwrap_or_default();
                let delegate = Folders { folders, workspace: cx.entity().downgrade() };
                self.open_picker(delegate, 440., window, cx);
            }
            "select_next" | "select_prev" | "confirm_selection" | "close_popup" => {
                let picker = self.picker.is_some();
                let action: Box<dyn gpui::Action> = match (action, picker) {
                    ("select_next", true) => Box::new(ui::SelectNextItem),
                    ("select_prev", true) => Box::new(ui::SelectPrevItem),
                    ("confirm_selection", true) => Box::new(ui::ConfirmItem),
                    ("close_popup", true) => Box::new(ui::Dismiss),
                    ("select_next", false) => Box::new(thread_list::SearchNext),
                    ("select_prev", false) => Box::new(thread_list::SearchPrev),
                    ("confirm_selection", false) => Box::new(thread_list::SearchSubmit),
                    _ => Box::new(thread_list::SearchCancel),
                };
                window.dispatch_action(action, cx);
            }
            "calendar_toggle_sidebar" => window.dispatch_action(Box::new(crate::ToggleSidebar), cx),
            _ => match commands::action_for(action) {
                Some(action) => window.dispatch_action(action, cx),
                None => self.show_toast(format!("Action '{action}' is not available in GPUI yet. See Keyboard shortcuts.").into(), ToastAction::None, cx),
            },
        }
    }
}

struct Folders {
    folders: Vec<crate::model::Folder>,
    workspace: WeakEntity<MailWorkspace>,
}

impl PickerDelegate for Folders {
    fn placeholder(&self) -> SharedString {
        "Go to folder…".into()
    }

    fn items(&self, query: &str, _: &App) -> Vec<PickerItem> {
        self.folders
            .iter()
            .enumerate()
            .filter(|(_, f)| !f.query.is_empty() && ui::fuzzy_score(query, &f.name).is_some())
            .map(|(ix, f)| PickerItem::new(ix.to_string(), f.name.clone()))
            .collect()
    }

    fn confirm(&mut self, item: &PickerItem, _: &str, window: &mut Window, cx: &mut App) -> AfterConfirm {
        let key: SharedString = format!("folder:{}", item.id).into();
        let workspace = self.workspace.clone();
        window.defer(cx, move |window, cx| {
            let _ = workspace.update(cx, |w, cx| w.navigate(key, window, cx));
        });
        AfterConfirm::Close
    }
}
