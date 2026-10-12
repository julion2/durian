//! What each right-click offers. Commands reuse the action table (so keycaps match the real
//! bindings); the rest are small handlers (clipboard, open, reveal).

use gpui::{App, ClipboardItem, Pixels, Point, SharedString, Window};

use crate::commands::keys_for;
use crate::model::{Attachment, Thread};
use crate::*;
use ui::{IconName, MenuItem};

/// Ask the workspace to show a context menu at `position`.
pub fn open(position: Point<Pixels>, items: Vec<MenuItem>, window: &mut Window, cx: &mut App) {
    cx.set_global(PendingMenu(Some((position, items))));
    window.dispatch_action(Box::new(ShowContextMenu), cx);
}

fn cmd(label: &str, icon: IconName, action: Box<dyn gpui::Action>, cx: &App) -> MenuItem {
    // Message menus are shared by split-list and full-thread views.
    let keys = keys_for(action.as_ref(), &[keymaps::Context::List, keymaps::Context::Thread], cx);
    MenuItem::action(label.to_string(), icon, keys, action)
}

fn copy(label: &str, icon: IconName, text: impl Into<String>) -> MenuItem {
    let text: String = text.into();
    MenuItem::handler(label.to_string(), icon, move |_, cx| cx.write_to_clipboard(ClipboardItem::new_string(text.clone())))
}

/// Thread row in the list (right-click also selects it, so commands act on it).
pub fn thread(thread: &Thread, cx: &App) -> Vec<MenuItem> {
    let cmd = |label: &str, icon, action: Box<dyn gpui::Action>| {
        let keys = keys_for(action.as_ref(), &[keymaps::Context::List], cx);
        MenuItem::action(label.to_string(), icon, keys, action)
    };
    vec![
        cmd("Reply", IconName::MailReply, Box::new(Reply)),
        cmd("Reply all", IconName::MailReply, Box::new(ReplyAll)),
        cmd("Forward…", IconName::MailForward, Box::new(Forward)),
        MenuItem::Separator,
        if thread.unread() {
            cmd("Mark as read", IconName::MailEmail, Box::new(ToggleRead))
        } else {
            cmd("Mark as unread", IconName::MailEmail, Box::new(ToggleRead))
        },
        cmd(if thread.starred { "Unstar" } else { "Star" }, IconName::MailStar, Box::new(ToggleStar)),
        cmd("Add or remove tags…", IconName::MailLabel, Box::new(OpenTagPicker)),
        cmd("Create event…", IconName::Calendar, Box::new(EventFromThread)),
        MenuItem::Separator,
        copy("Copy subject", IconName::MailEmail, thread.subject.to_string()),
        MenuItem::Separator,
        cmd("Mark done", IconName::MailDone, Box::new(Archive)),
    ]
}

/// A message card in the thread.
pub fn message(body: SharedString, sender: SharedString, email: SharedString, cx: &App) -> Vec<MenuItem> {
    let cmd = |label: &str, icon, action: Box<dyn gpui::Action>| cmd(label, icon, action, cx);
    vec![
        cmd("Reply", IconName::MailReply, Box::new(Reply)),
        cmd("Reply all", IconName::MailReply, Box::new(ReplyAll)),
        cmd("Forward…", IconName::MailForward, Box::new(Forward)),
        MenuItem::Separator,
        copy("Copy text", IconName::MailEmail, body.to_string()),
        copy(&format!("Copy {}'s address", crate::model::first_name(&sender)), IconName::MailUsers, email.to_string()),
    ]
}

pub fn attachment(attachment: Attachment) -> Vec<MenuItem> {
    let name = attachment.name.clone();
    let (preview, download, reveal) = (attachment.clone(), attachment.clone(), attachment);
    let on_disk = reveal.path.is_some();
    let mut items = vec![
        MenuItem::handler("Preview", IconName::MailAttach, move |_, cx| attachments::quick_look(&preview, cx)),
        MenuItem::handler("Download", IconName::MailDownload, move |_, cx| {
            if let Some(path) = attachments::download(&download, cx) {
                attachments::reveal_in_finder(&path);
            }
        }),
    ];
    // a file still on the server has no place in Finder yet (Download reveals it)
    if on_disk {
        items.push(MenuItem::handler("Show in Finder", IconName::Folder, move |_, _| {
            if let Some(path) = &reveal.path {
                attachments::reveal_in_finder(path);
            }
        }));
    }
    items.extend([MenuItem::Separator, copy("Copy file name", IconName::MailAttach, name.to_string())]);
    items
}

/// Right-click in an HTML message: the selection, the link under the pointer (if it may be
/// opened), then what the message itself offers.
pub fn html_body(selection: String, link: Option<SharedString>, body: SharedString, sender: SharedString, email: SharedString, cx: &App) -> Vec<MenuItem> {
    let mut items = Vec::new();
    if !selection.trim().is_empty() {
        items.push(copy("Copy", IconName::MailEmail, selection));
        items.push(MenuItem::Separator);
    }
    if let Some(url) = link.filter(|u| ui::link::allowed(u)) {
        items.extend(self::link(url));
        items.push(MenuItem::Separator);
    }
    items.extend(message(body, sender, email, cx));
    items
}

pub fn link(url: SharedString) -> Vec<MenuItem> {
    let open = url.clone();
    vec![
        MenuItem::handler("Open link", IconName::ArrowRight, move |_, cx| {
            ui::link::open(&open, cx);
        }),
        copy("Copy link address", IconName::MailEmail, url.to_string()),
    ]
}

/// Tags double as queries – hand the query (and the CLI line) to the developer.
pub fn tag(name: SharedString) -> Vec<MenuItem> {
    vec![
        copy("Copy query", IconName::MailLabel, format!("tag:{name}")),
        copy("Copy mailcli command", IconName::MailTerminal, format!("mailcli list 'tag:{name}'")),
    ]
}
