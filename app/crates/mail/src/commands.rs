//! Command metadata. Configurable shortcuts come from the effective keymaps, not this table.

use gpui::{Action, App, DummyKeyboardMapper, Entity, KeyBinding, KeyBindingContextPredicate, SharedString, Window};

use crate::model::Mailbox;
use crate::*;
use ui::{AfterConfirm, IconName, Leading, PickerDelegate, PickerItem, fuzzy_score};

/// Where a command applies: everywhere, or only in one area of the app. Bindings, ⌘K and the
/// shortcut sheet all follow it, so mail and calendar share keys without stepping on each other.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Scope {
    Any,
    Mail,
    Calendar,
}

pub struct Command {
    pub scope: Scope,
    pub label: &'static str,
    pub icon: IconName,
    /// GPUI keystroke syntax; the first one is displayed.
    pub keys: &'static [&'static str],
    pub action: fn() -> Box<dyn Action>,
    /// Listed in ⌘K (motions like j/k are bound but not worth a row).
    pub in_menu: bool,
    /// Section in the keyboard shortcuts panel (?).
    pub group: &'static str,
}

pub const COMMANDS: &[Command] = &[
    Command { scope: Scope::Mail, label: "Search", icon: IconName::Search, keys: &["/"], action: || Box::new(FocusSearch), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Any, label: "Refresh from server", icon: IconName::MailInbox, keys: &["cmd-r", "ctrl-r"], action: || Box::new(Retry), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Reply all", icon: IconName::MailReply, keys: &["a"], action: || Box::new(ReplyAll), in_menu: true, group: "Writing" },
    Command { scope: Scope::Mail, label: "Reply", icon: IconName::MailReply, keys: &["r"], action: || Box::new(Reply), in_menu: true, group: "Writing" },
    Command { scope: Scope::Mail, label: "Forward…", icon: IconName::MailForward, keys: &[], action: || Box::new(Forward), in_menu: true, group: "Writing" },
    Command { scope: Scope::Mail, label: "New message", icon: IconName::Compose, keys: &["c"], action: || Box::new(NewMessage), in_menu: true, group: "Writing" },
    Command { scope: Scope::Mail, label: "Create event from thread", icon: IconName::Calendar, keys: &[], action: || Box::new(EventFromThread), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Mark done", icon: IconName::MailDone, keys: &["e"], action: || Box::new(Archive), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Preview attachment", icon: IconName::MailAttach, keys: &["space"], action: || Box::new(PreviewAttachment), in_menu: true, group: "Thread" },
    Command { scope: Scope::Any, label: "Show all accounts", icon: IconName::MailInbox, keys: &["cmd-0"], action: || Box::new(ShowAllAccounts), in_menu: true, group: "Accounts" },
    Command { scope: Scope::Any, label: "Show account 1", icon: IconName::MailInbox, keys: &["cmd-1"], action: || Box::new(ShowAccount1), in_menu: false, group: "Accounts" },
    Command { scope: Scope::Any, label: "Show account 2", icon: IconName::MailInbox, keys: &["cmd-2"], action: || Box::new(ShowAccount2), in_menu: false, group: "Accounts" },
    Command { scope: Scope::Any, label: "Show account 3", icon: IconName::MailInbox, keys: &["cmd-3"], action: || Box::new(ShowAccount3), in_menu: false, group: "Accounts" },
    Command { scope: Scope::Any, label: "Toggle dark mode", icon: IconName::Display, keys: &[], action: || Box::new(ToggleDarkMode), in_menu: true, group: "General" },
    Command { scope: Scope::Any, label: "Use system appearance", icon: IconName::Display, keys: &[], action: || Box::new(UseSystemAppearance), in_menu: true, group: "General" },
    Command { scope: Scope::Any, label: "Toggle sidebar", icon: IconName::SidebarToggle, keys: &["cmd-\\"], action: || Box::new(ToggleSidebar), in_menu: true, group: "General" },
    Command { scope: Scope::Any, label: "Go to drafts", icon: IconName::MailDraft, keys: &["g d"], action: || Box::new(GoToDrafts), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Any, label: "Go to inbox", icon: IconName::MailInbox, keys: &["g i"], action: || Box::new(GoToInbox), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Add or remove tags…", icon: IconName::MailLabel, keys: &["l"], action: || Box::new(OpenTagPicker), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Mark as read / unread", icon: IconName::MailEmail, keys: &["u"], action: || Box::new(ToggleRead), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Star / unstar", icon: IconName::MailStar, keys: &["s"], action: || Box::new(ToggleStar), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Expand / collapse all messages", icon: IconName::MailExpand, keys: &[";"], action: || Box::new(ToggleExpandAll), in_menu: true, group: "Thread" },
    Command { scope: Scope::Mail, label: "Toggle list layout", icon: IconName::Display, keys: &[], action: || Box::new(ToggleListLayout), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Toggle focus mode", icon: IconName::PanelRight, keys: &["enter"], action: || Box::new(ToggleFocusMode), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Next thread", icon: IconName::ArrowRight, keys: &["j", "down"], action: || Box::new(SelectNext), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Previous thread", icon: IconName::ArrowLeft, keys: &["k", "up"], action: || Box::new(SelectPrev), in_menu: true, group: "Navigation" },
    Command { scope: Scope::Mail, label: "Back to split view", icon: IconName::SidebarToggle, keys: &["escape"], action: || Box::new(ExitFocusMode), in_menu: false, group: "Navigation" },
    Command { scope: Scope::Any, label: "Go to calendar", icon: IconName::Calendar, keys: &["g c"], action: || Box::new(GoToCalendar), in_menu: true, group: "Navigation" },
    // calendar: same motions as mail (j/k/esc route to the calendar), plus periods and views
    Command { scope: Scope::Calendar, label: "Next event", icon: IconName::ArrowRight, keys: &["j", "down"], action: || Box::new(SelectNext), in_menu: false, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Previous event", icon: IconName::ArrowLeft, keys: &["k", "up"], action: || Box::new(SelectPrev), in_menu: false, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Clear selection", icon: IconName::CloseSmall, keys: &["escape"], action: || Box::new(ExitFocusMode), in_menu: false, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Edit event", icon: IconName::Compose, keys: &["e"], action: || Box::new(calendar::EditEvent), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Delete event", icon: IconName::MailTrash, keys: &["backspace", "delete"], action: || Box::new(calendar::DeleteEvent), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Duplicate event", icon: IconName::Plus, keys: &["cmd-d"], action: || Box::new(calendar::DuplicateEvent), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "New event", icon: IconName::Calendar, keys: &["c"], action: || Box::new(NewEvent), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Next period", icon: IconName::ArrowRight, keys: &["l", "right"], action: || Box::new(calendar::NextPeriod), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Previous period", icon: IconName::ArrowLeft, keys: &["h", "left"], action: || Box::new(calendar::PrevPeriod), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Go to today", icon: IconName::Calendar, keys: &["t"], action: || Box::new(calendar::GoToToday), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Day view", icon: IconName::Calendar, keys: &["d"], action: || Box::new(calendar::ShowDay), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Week view", icon: IconName::Calendar, keys: &["w"], action: || Box::new(calendar::ShowWeek), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Month view", icon: IconName::Calendar, keys: &["m"], action: || Box::new(calendar::ShowMonth), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Calendar, label: "Agenda", icon: IconName::Calendar, keys: &["a"], action: || Box::new(calendar::ShowAgenda), in_menu: true, group: "Calendar" },
    Command { scope: Scope::Any, label: "Copy selected text", icon: IconName::MailEmail, keys: &["cmd-c"], action: || Box::new(CopySelection), in_menu: false, group: "General" },
    Command { scope: Scope::Any, label: "Select all text", icon: IconName::MailEmail, keys: &["cmd-a"], action: || Box::new(SelectAllText), in_menu: false, group: "General" },
    Command { scope: Scope::Any, label: "Keyboard shortcuts", icon: IconName::Display, keys: &["?"], action: || Box::new(ToggleShortcutHelp), in_menu: true, group: "General" },
    Command { scope: Scope::Any, label: "Command menu", icon: IconName::Search, keys: &["cmd-k"], action: || Box::new(OpenCommandMenu), in_menu: false, group: "General" },
    Command { scope: Scope::Any, label: "Reload keymaps", icon: IconName::Display, keys: &["cmd-alt-r", "ctrl-alt-r"], action: || Box::new(ReloadKeymaps), in_menu: true, group: "General" },
];

/// Shortcuts are suspended while a picker or any text field owns the keyboard. The workspace
/// root carries "Workspace" (plus "Mail" in the mail area); the calendar view adds "Calendar".
const IDLE: &str = "!Picker && !Compose && !TextInput && !ContextMenu && !SearchBar && !ShortcutHelp";

pub fn context(scope: Scope) -> String {
    let area = match scope {
        Scope::Any => "Workspace",
        Scope::Mail => "Mail",
        Scope::Calendar => "Calendar",
    };
    format!("{area} && {IDLE}")
}

pub fn bind_keys(cx: &mut App) {
    let predicates: Vec<(Scope, std::rc::Rc<KeyBindingContextPredicate>)> = [Scope::Any, Scope::Mail, Scope::Calendar]
        .into_iter()
        .map(|scope| (scope, KeyBindingContextPredicate::parse(&context(scope)).unwrap().into()))
        .collect();
    let bindings = COMMANDS
        .iter()
        .filter(|c| pkl_name((c.action)().as_ref(), c.scope).is_none())
        .flat_map(|c| c.keys.iter().map(move |k| (k, c)))
        .map(|(k, c)| {
            let predicate = predicates.iter().find(|(s, _)| *s == c.scope).map(|(_, p)| p.clone());
            KeyBinding::load(k, (c.action)(), predicate, false, None, &DummyKeyboardMapper).expect("valid keystroke in COMMANDS")
        })
        .collect::<Vec<_>>();
    cx.bind_keys(bindings);
    #[cfg(not(target_os = "macos"))]
    cx.bind_keys([
        KeyBinding::new("ctrl-c", CopySelection, Some(&context(Scope::Any))),
        KeyBinding::new("ctrl-a", SelectAllText, Some(&context(Scope::Any))),
    ]);
}

/// Keycaps from the effective config, including removed/rebound defaults.
/// A surface shared between areas shows only a shortcut that works in all of them.
pub fn keys_for(action: &dyn Action, contexts: &[crate::keymaps::Context], cx: &App) -> Vec<SharedString> {
    COMMANDS
        .iter()
        .find(|c| (c.action)().name() == action.name())
        .and_then(|c| {
            let (&first, rest) = contexts.split_first()?;
            shortcuts(c, first, cx).into_iter().find(|key| rest.iter().all(|context| shortcuts(c, *context, cx).contains(key)))
        })
        .map(|k| keycaps(&k))
        .unwrap_or_default()
}

pub fn shortcuts(command: &Command, context: crate::keymaps::Context, cx: &App) -> Vec<String> {
    let Some(name) = pkl_name((command.action)().as_ref(), command.scope) else {
        return command.keys.iter().map(|k| k.to_string()).collect();
    };
    let maps = cx.global::<crate::keymaps::Keymaps>();
    maps.for_context(context).filter(|b| b.entry.action == name).map(|b| b.strokes.join(" ")).collect()
}

/// Native application commands (reload, clipboard) remain usable with keymaps disabled.
pub fn pkl_name(action: &dyn Action, _scope: Scope) -> Option<&'static str> {
    Some(match action.name().rsplit("::").next()? {
        "SelectNext" => "next_email", "SelectPrev" => "prev_email", "Archive" => "archive",
        "ToggleStar" => "toggle_star", "ToggleRead" => "toggle_read", "Reply" => "reply", "ReplyAll" => "reply_all",
        "Forward" => "forward", "NewMessage" => "compose", "FocusSearch" => "search", "Retry" => "reload_inbox",
        "GoToInbox" => "go_inbox", "GoToDrafts" => "go_drafts", "GoToCalendar" => "go_calendar",
        "OpenTagPicker" => "tag_picker", "OpenThread" => "enter_thread", "ExitFocusMode" => "close_detail",
        "ToggleFocusMode" => "toggle_focus_mode", "ToggleExpandAll" => "toggle_expand_all",
        "ToggleListLayout" => "toggle_list_layout", "ToggleSidebar" => "toggle_sidebar",
        "ToggleShortcutHelp" => "shortcut_help", "OpenCommandMenu" => "command_menu",
        "PreviewAttachment" => "preview_attachment", "EventFromThread" => "event_from_thread",
        "ShowAllAccounts" => "show_all_accounts", "ShowAccount1" => "show_account_1",
        "ShowAccount2" => "show_account_2", "ShowAccount3" => "show_account_3",
        "ToggleDarkMode" => "toggle_dark_mode", "UseSystemAppearance" => "system_appearance",
        "EditEvent" => "calendar_edit", "DeleteEvent" => "calendar_delete", "NewEvent" => "calendar_new",
        "NextPeriod" => "calendar_next_period", "PrevPeriod" => "calendar_prev_period", "GoToToday" => "calendar_today",
        "ShowDay" => "calendar_view_day", "ShowWeek" => "calendar_view_week", "ShowMonth" => "calendar_view_month",
        "ShowAgenda" => "calendar_view_agenda", "DuplicateEvent" => "calendar_duplicate",
        _ => return None,
    })
}

pub fn action_for(name: &str) -> Option<Box<dyn Action>> {
    COMMANDS.iter().find(|c| pkl_name((c.action)().as_ref(), c.scope) == Some(name)).map(|c| (c.action)())
}

/// "cmd-k" → ["⌘", "K"], "e" → ["E"], "enter" → ["↵"]
pub fn keycaps(keystroke: &str) -> Vec<SharedString> {
    // "g d" is a two-stroke binding: one keycap per stroke
    keystroke
        .split_whitespace()
        .flat_map(|stroke| stroke.split('-'))
        .map(|part| {
            match part {
                "cmd" => "⌘",
                "shift" => "⇧",
                "alt" => "⌥",
                "ctrl" => "⌃",
                "enter" => "↵",
                "escape" => "esc",
                "up" => "↑",
                "left" => "←",
                "right" => "→",
                "space" => "␣",
                "tab" => "⇥",
                "backspace" => "⌫",
                "delete" => "⌦",
                "down" => "↓",
                other => return other.to_uppercase().into(),
            }
            .into()
        })
        .collect()
}

/// ⌘K: the commands of the current area, plus "go to thread" / "go to event" once you type.
pub struct CommandMenu {
    pub mailbox: Entity<Mailbox>,
    pub schedule: Entity<calendar::model::Schedule>,
    pub scope: Scope,
    pub key_context: crate::keymaps::Context,
    /// called with the new event's id after "Create event"
    pub on_event_created: std::rc::Rc<dyn Fn(SharedString, &mut Window, &mut App)>,
}

impl CommandMenu {
    /// The query read as an event ("Lunch with Mara tomorrow 12:30"), if it is one.
    fn parsed(&self, query: &str, cx: &App) -> Option<calendar::parse::Parsed> {
        let contacts = self.mailbox.read(cx).contacts();
        let names: Vec<&str> = contacts.iter().map(|c| c.name.as_ref()).collect();
        calendar::parse::parse(query, chrono::Local::now().naive_local(), &names)
    }
}

/// A parsed event becomes a real one: the user organizes it, the named people are invited.
pub fn event_from(parsed: &calendar::parse::Parsed, calendar: SharedString, mailbox: &Mailbox) -> calendar::model::Event {
    use calendar::model::{Attendee, Event, Rsvp};
    let contacts = mailbox.contacts();
    let me = &mailbox.me;
    let mut attendees = Vec::new();
    if !parsed.people.is_empty() {
        attendees.push(Attendee { name: me.name.clone(), email: me.email.clone(), rsvp: Rsvp::Accepted, organizer: true, avatar: me.avatar.as_ref().map(|a| a.path.clone()) });
        for name in &parsed.people {
            if let Some(c) = contacts.iter().find(|c| c.name.as_ref() == name.as_str()) {
                attendees.push(Attendee { name: c.name.clone(), email: c.email.clone(), rsvp: Rsvp::NeedsAction, organizer: false, avatar: c.avatar.as_ref().map(|a| a.path.clone()) });
            }
        }
    }
    let title = if parsed.title.is_empty() { "New event".to_string() } else { parsed.title.clone() };
    Event {
        id: uuid::Uuid::new_v4().simple().to_string().into(),
        calendar,
        title: title.into(),
        start: parsed.start,
        end: parsed.end,
        all_day: parsed.all_day,
        location: None,
        conference: None,
        description: None,
        attendees,
        rsvp: Rsvp::Accepted,
        recurrence: None,
        series: None,
    }
}

impl PickerDelegate for CommandMenu {
    fn placeholder(&self) -> SharedString {
        "Type a command or search…".into()
    }

    fn context(&self, cx: &App) -> Option<(SharedString, SharedString)> {
        if self.scope == Scope::Calendar {
            let event = self.schedule.read(cx).selected_event()?;
            return Some(("Event".into(), event.title.clone()));
        }
        let thread = self.mailbox.read(cx).selected_thread()?;
        Some(("Thread".into(), thread.subject.clone()))
    }

    fn items(&self, query: &str, cx: &App) -> Vec<PickerItem> {
        let mut commands: Vec<(i32, PickerItem)> = COMMANDS
            .iter()
            .enumerate()
            .filter(|(_, c)| c.in_menu && (c.scope == Scope::Any || c.scope == self.scope))
            .filter_map(|(ix, c)| {
                let score = fuzzy_score(query, c.label)?;
                let mut item = PickerItem::new(format!("cmd:{ix}"), c.label);
                item.leading = Leading::Icon(c.icon.into());
                item.keys = shortcuts(c, self.key_context, cx).first().map(|k| keycaps(k)).unwrap_or_default();
                item.section = (!query.is_empty()).then(|| "Commands".into());
                Some((score, item))
            })
            .collect();
        commands.sort_by(|a, b| b.0.cmp(&a.0));
        let mut items: Vec<PickerItem> = Vec::new();
        // typed like an event: offer to create it, first
        if let Some(p) = (!query.is_empty()).then(|| self.parsed(query, cx)).flatten() {
            let title = if p.title.is_empty() { "New event".to_string() } else { p.title.clone() };
            let mut item = PickerItem::new("create-event", format!("Create event: {title}"));
            item.detail = Some(calendar::parse::describe(&p, chrono::Local::now().date_naive()).into());
            item.leading = Leading::Icon(IconName::Calendar.into());
            item.section = Some("Create".into());
            items.push(item);
        }
        items.extend(commands.into_iter().map(|(_, i)| i));

        if !query.is_empty() && self.scope == Scope::Calendar {
            let schedule = self.schedule.read(cx);
            let mut events: Vec<(i32, PickerItem)> = schedule
                .events
                .iter()
                .filter_map(|e| {
                    let score = fuzzy_score(query, &e.title)?;
                    let mut item = PickerItem::new(format!("event:{}", e.id), e.title.clone());
                    item.detail = Some(e.start.format("%a, %b %-d · %H:%M").to_string().into());
                    item.leading = Leading::Icon(IconName::Calendar.into());
                    item.section = Some("Events".into());
                    Some((score, item))
                })
                .collect();
            events.sort_by(|a, b| b.0.cmp(&a.0));
            items.extend(events.into_iter().take(8).map(|(_, i)| i));
        } else if !query.is_empty() {
            let mailbox = self.mailbox.read(cx);
            let mut threads: Vec<(i32, PickerItem)> = mailbox
                .threads
                .iter()
                .enumerate()
                .filter_map(|(ix, t)| {
                    let score = fuzzy_score(query, &t.subject)?;
                    let mut item = PickerItem::new(format!("thread:{ix}"), t.subject.clone());
                    item.detail = Some(t.latest().from.name.clone());
                    item.leading = Leading::Icon(IconName::MailEmail.into());
                    item.section = Some("Threads".into());
                    Some((score, item))
                })
                .collect();
            threads.sort_by(|a, b| b.0.cmp(&a.0));
            items.extend(threads.into_iter().take(8).map(|(_, i)| i));
        }
        items
    }

    fn confirm(&mut self, item: &PickerItem, query: &str, window: &mut Window, cx: &mut App) -> AfterConfirm {
        if item.id.as_ref() == "create-event" {
            if let Some(parsed) = self.parsed(query, cx) {
                let mailbox = self.mailbox.read(cx);
                let calendar = self.schedule.read(cx).default_calendar(mailbox.account_filter.as_deref());
                let event = event_from(&parsed, calendar, mailbox);
                let id = event.id.clone();
                self.schedule.update(cx, |s, cx| s.add_event(event, cx));
                let on_created = self.on_event_created.clone();
                window.defer(cx, move |window, cx| on_created(id, window, cx));
            }
            return AfterConfirm::Close;
        }
        if let Some(ix) = item.id.strip_prefix("cmd:").and_then(|i| i.parse::<usize>().ok()) {
            let action = (COMMANDS[ix].action)();
            // run after the menu closed and focus is back on the workspace
            window.defer(cx, move |window, cx| window.dispatch_action(action, cx));
        } else if let Some(ix) = item.id.strip_prefix("thread:").and_then(|i| i.parse::<usize>().ok()) {
            self.mailbox.update(cx, |m, cx| m.select(ix, cx));
        } else if let Some(id) = item.id.strip_prefix("event:") {
            // jump to the event's day and select it
            let id: SharedString = id.to_string().into();
            self.schedule.update(cx, |s, cx| {
                if let Some(day) = s.event(&id).map(|e| e.start.date()) {
                    s.anchor = day;
                }
                s.select(Some(id), cx);
            });
        }
        AfterConfirm::Close
    }
}

/// Toggle tags on the open thread; typing a new name offers to create it.
pub struct TagPicker {
    pub mailbox: Entity<Mailbox>,
}

impl PickerDelegate for TagPicker {
    fn placeholder(&self) -> SharedString {
        "Add tags…".into()
    }

    fn context(&self, cx: &App) -> Option<(SharedString, SharedString)> {
        let mailbox = self.mailbox.read(cx);
        if mailbox.selection_mode().is_some() {
            return Some(("Tags".into(), format!("{} selected threads", mailbox.selected_ids(1).len()).into()));
        }
        let thread = mailbox.selected_thread()?;
        Some(("Tags".into(), thread.subject.clone()))
    }

    fn items(&self, query: &str, cx: &App) -> Vec<PickerItem> {
        let mailbox = self.mailbox.read(cx);
        let ids = mailbox.selected_ids(1);
        let selected: Vec<_> = mailbox.threads.iter().filter(|t| ids.contains(&t.id)).collect();
        let mut tags: Vec<(i32, PickerItem)> = mailbox
            .tags
            .iter()
            .filter_map(|tag| {
                let score = fuzzy_score(query, &tag.name)?;
                let mut item = PickerItem::new(format!("tag:{}", tag.name), tag.name.clone());
                item.leading = Leading::Dot(tag.color);
                item.checked = Some(!selected.is_empty() && selected.iter().all(|t| t.tags.contains(&tag.name)));
                Some((score, item))
            })
            .collect();
        if !query.is_empty() {
            tags.sort_by(|a, b| b.0.cmp(&a.0));
        }
        let mut items: Vec<PickerItem> = tags.into_iter().map(|(_, i)| i).collect();

        let name = query.trim();
        if !name.is_empty() && !mailbox.tags.iter().any(|t| t.name.eq_ignore_ascii_case(name)) {
            let mut create = PickerItem::new(format!("new:{name}"), format!("Create tag “{name}”"));
            create.leading = Leading::Icon(IconName::Plus.into());
            items.push(create);
        }
        items
    }

    fn confirm(&mut self, item: &PickerItem, _query: &str, _window: &mut Window, cx: &mut App) -> AfterConfirm {
        if let Some(name) = item.id.strip_prefix("tag:") {
            let name: SharedString = name.to_string().into();
            self.mailbox.update(cx, |m, cx| m.toggle_tag_selected(&name, cx));
        } else if let Some(name) = item.id.strip_prefix("new:") {
            let name: SharedString = name.to_string().into();
            self.mailbox.update(cx, |m, cx| {
                m.create_tag(name.clone(), cx);
                m.toggle_tag_selected(&name, cx);
            });
        }
        // the tag picker stays open for multi-select; Esc closes
        AfterConfirm::KeepOpen
    }
}
