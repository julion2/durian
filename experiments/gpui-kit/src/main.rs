//! Native reconstruction of Durian's Swift mail UI, not a new design direction.
//! Sample mail by default; --live opts into read-only access to durian serve.

mod accounts;
#[cfg(not(target_os = "macos"))]
mod browser;
#[cfg(target_os = "macos")]
#[path = "browser_mac.rs"]
mod browser;
mod compose;
mod content;
#[path = "../../gpui/src/data.rs"]
mod data;
// Backend state is verified independently before enabling composer writes.
#[allow(dead_code)]
mod delivery;
mod demo;
mod picker;

use std::collections::BTreeSet;

use gpui_kit::assets::IconName;
use gpui_kit::base::SelectableText;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::menu::{ContextMenuExt as _, DropdownMenu as _};
use gpui_kit::component::notification::Notification;
use gpui_kit::component::scroll::ScrollableElement as _;
use gpui_kit::component::{
    ActiveTheme, Icon, Root, Sizable as _, Theme, ThemeMode, TitleBar, WindowExt, h_flex,
    h_resizable, resizable_panel, v_flex,
};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

use data::{FOLDERS, Thread, ThreadPreview};

actions!(
    durian,
    [
        Next,
        Previous,
        First,
        Last,
        NextMessage,
        PreviousMessage,
        FocusThread,
        FocusList,
        NextFolder,
        PreviousFolder,
        Search,
        EditTags,
        Attachments,
        ToggleBody,
        BrowserPreview,
        FocusHtml,
        Back,
        Reload,
        ToggleTheme,
        Compose,
        Reply,
        Archive,
        ToggleRead,
        Pin,
        Delete,
        Quit
    ]
);

// Colors mirror Color.Detail in macos/durian/Utilities/ColorExtensions.swift.
// This is the theme boundary; rendering below only consumes semantic roles.
fn apply_mail_theme(cx: &mut App) {
    let theme = Theme::global_mut(cx);
    theme.font_family = if cfg!(target_os = "macos") {
        ".SystemUIFont"
    } else {
        "Inter"
    }
    .into();
    theme.font_size = px(16.);
    let dark = theme.is_dark();
    theme.background = rgb(if dark { 0x1e1e20 } else { 0xffffff }).into();
    theme.foreground = rgb(if dark { 0xf5f5f5 } else { 0x0a0a0a }).into();
    theme.muted_foreground = rgb(if dark { 0x9ca3af } else { 0x6a7282 }).into();
    theme.sidebar = rgb(if dark { 0x252527 } else { 0xf5f5f7 }).into();
    theme.sidebar_foreground = theme.foreground;
    theme.group_box = rgb(if dark { 0x2a2a2c } else { 0xffffff }).into();
    theme.border = rgb(if dark { 0x3a3a3c } else { 0xe5e7eb }).into();
    theme.sidebar_border = theme.border;
    theme.primary = rgb(0x3675d9).into();
    theme.primary_foreground = rgb(0xffffff).into();
    theme.sidebar_accent = theme.primary;
    theme.sidebar_accent_foreground = theme.primary_foreground;
    theme.ring = theme.primary;
    theme.list_hover = theme.foreground.opacity(0.05);
    theme.tokens.sidebar_accent = theme.sidebar_accent.into();
    Theme::sync_base(cx);
}

fn avatar(name: &str, large: bool, cx: &App) -> impl IntoElement {
    let theme = cx.theme();
    let colors = [theme.blue, theme.red, theme.green, theme.cyan];
    let hash = name
        .chars()
        .fold(0usize, |h, c| h.wrapping_mul(31).wrapping_add(c as usize));
    div()
        .flex()
        .items_center()
        .justify_center()
        .flex_shrink_0()
        .size(rems(if large { 2.5 } else { 2. }))
        .rounded_full()
        .bg(colors[hash % colors.len()])
        .text_color(theme.primary_foreground)
        .text_sm()
        .font_weight(FontWeight::SEMIBOLD)
        .child(data::initials(name))
}

fn command(id: &'static str, icon: IconName, label: &'static str, action: impl Action) -> Button {
    Button::new(id)
        .ghost()
        .small()
        .icon(icon)
        .tooltip(label)
        .on_click(move |_, window, cx| window.dispatch_action(action.boxed_clone(), cx))
}

fn unsupported(label: &str, window: &mut Window, cx: &mut App) {
    window.push_notification(
        Notification::info(format!(
            "{label} is unavailable in this read-only prototype."
        )),
        cx,
    );
}

fn has_tag(mail: &ThreadPreview, tag: &str) -> bool {
    mail.tags.iter().any(|t| t == tag)
}

fn visible_tags(mail: &ThreadPreview, folder: &str) -> Vec<String> {
    mail.tags
        .iter()
        .filter(|t| ![folder, "flagged", "unread", "attachment"].contains(&t.as_str()))
        .cloned()
        .collect()
}

fn group_title(preview: &ThreadPreview, thread: &Thread) -> String {
    if has_tag(preview, "flagged") {
        return "Pinned".into();
    }
    thread
        .messages
        .first()
        .and_then(|m| chrono::DateTime::parse_from_rfc2822(&m.date).ok())
        .map(|d| d.format("%B %Y").to_string())
        .unwrap_or_else(|| "Messages".into())
}

/// Domain indices stay separate from visible positions, so filtering cannot
/// silently retarget a reply to a different thread.
fn filtered_rows(
    mail: &[(ThreadPreview, Thread)],
    folder: &str,
    query: &str,
    label: Option<&str>,
) -> Vec<usize> {
    let query = query.trim().to_lowercase();
    let mut rows: Vec<_> = mail
        .iter()
        .enumerate()
        .filter(|(_, (p, _))| {
            has_tag(p, folder)
                && label.is_none_or(|l| has_tag(p, l))
                && (query.is_empty()
                    || [&p.subject, &p.sender, &p.preview]
                        .iter()
                        .any(|s| s.to_lowercase().contains(&query)))
        })
        .map(|(ix, _)| ix)
        .collect();
    rows.sort_by_key(|&ix| !has_tag(&mail[ix].0, "flagged"));
    rows
}

struct MailApp {
    focus: FocusHandle,
    search_ids: Option<Vec<String>>,
    query: String,
    folder: usize,
    label: Option<String>,
    mail: Vec<(ThreadPreview, Thread)>,
    rows: Vec<usize>,
    selected: Option<usize>,
    mail_list: ListState,
    message_list: ListState,
    thread_focused: bool,
    message: usize,
    details: BTreeSet<usize>,
    live: bool,
    loading: bool,
    load_generation: usize,
    error: Option<String>,
    contents: Vec<Entity<content::MailContent>>,
    content_subscriptions: Vec<Subscription>,
    popup_subscription: Option<Subscription>,
    // Keep the emitter alive until its queued selection event has been delivered,
    // even when accepting a result closes the dialog in the same update.
    popup: Option<Entity<picker::MailPicker>>,
}

impl MailApp {
    fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            focus: cx.focus_handle(),
            search_ids: None,
            query: String::new(),
            folder: 0,
            label: None,
            mail: demo::mail(),
            rows: Vec::new(),
            selected: None,
            mail_list: ListState::new(0, ListAlignment::Top, px(200.)),
            message_list: ListState::new(0, ListAlignment::Top, px(400.)),
            thread_focused: false,
            message: 0,
            details: BTreeSet::new(),
            live: std::env::args().any(|arg| arg == "--live"),
            loading: false,
            load_generation: 0,
            error: None,
            contents: Vec::new(),
            content_subscriptions: Vec::new(),
            popup_subscription: None,
            popup: None,
        };
        if this.live {
            this.mail.clear();
            this.reload(window, cx);
        } else {
            this.refilter(cx);
            if let Some(row) = this
                .rows
                .iter()
                .position(|&ix| this.mail[ix].0.thread_id == "design-review")
            {
                this.select(row, cx);
            }
        }
        this
    }

    fn refilter(&mut self, cx: &mut Context<Self>) {
        let keep = self.selected.and_then(|r| self.rows.get(r).copied());
        self.rows = if let Some(ids) = &self.search_ids {
            ids.iter()
                .filter_map(|id| self.mail.iter().position(|(p, _)| &p.thread_id == id))
                .collect()
        } else {
            filtered_rows(
                &self.mail,
                FOLDERS[self.folder].query.trim_start_matches("tag:"),
                "",
                self.label.as_deref(),
            )
        };
        self.mail_list.reset(self.rows.len());
        self.selected = None;
        if !self.rows.is_empty() {
            self.select(
                keep.and_then(|ix| self.rows.iter().position(|&r| r == ix))
                    .unwrap_or(0),
                cx,
            );
        } else {
            self.message_list.reset(0);
            self.contents.clear();
            self.content_subscriptions.clear();
        }
        cx.notify();
    }

    fn current(&self) -> Option<&(ThreadPreview, Thread)> {
        self.selected
            .and_then(|r| self.rows.get(r))
            .and_then(|&ix| self.mail.get(ix))
    }

    fn select(&mut self, row: usize, cx: &mut Context<Self>) {
        if self.rows.is_empty() {
            return;
        }
        self.selected = Some(row.min(self.rows.len() - 1));
        self.message = 0;
        self.details.clear();
        self.thread_focused = false;
        self.contents.clear();
        self.content_subscriptions.clear();
        let messages = self
            .current()
            .map(|(_, t)| t.messages.clone())
            .unwrap_or_default();
        for (index, message) in messages.into_iter().enumerate() {
            let view = cx.new(|cx| content::MailContent::new(message, self.live, cx));
            self.content_subscriptions.push(cx.subscribe(
                &view,
                move |this, _, _: &content::ContentChanged, cx| {
                    this.message_list.remeasure_items(index + 1..index + 2);
                    cx.notify();
                },
            ));
            self.contents.push(view);
        }
        self.mail_list.scroll_to_reveal_item(self.selected.unwrap());
        self.message_list
            .reset(self.current().map_or(0, |(_, t)| t.messages.len() + 1));
        self.message_list.scroll_to(ListOffset::default());
        cx.notify();
    }

    fn navigate(&mut self, delta: isize, cx: &mut Context<Self>) {
        if self.thread_focused {
            self.message_list.scroll_by(px(delta as f32 * 80.));
            cx.notify();
        } else {
            let next = (self.selected.unwrap_or(0) as isize + delta).max(0) as usize;
            self.select(next, cx);
        }
    }

    fn focus_message(&mut self, delta: isize, window: &mut Window, cx: &mut Context<Self>) {
        let count = self.current().map_or(0, |(_, t)| t.messages.len());
        if count == 0 {
            return;
        }
        self.message = (self.message as isize + delta).clamp(0, count as isize - 1) as usize;
        self.thread_focused = true;
        self.message_list.scroll_to(ListOffset {
            item_ix: if self.message == 0 {
                0
            } else {
                self.message + 1
            },
            offset_in_item: px(0.),
        });
        window.focus(&self.focus, cx);
        cx.notify();
    }

    fn set_folder(&mut self, ix: usize, window: &mut Window, cx: &mut Context<Self>) {
        self.folder = ix;
        self.label = None;
        self.query.clear();
        self.search_ids = None;
        self.selected = None;
        window.focus(&self.focus, cx);
        if self.live {
            self.mail.clear();
            self.refilter(cx);
            self.reload(window, cx);
        } else {
            self.refilter(cx);
        }
    }

    fn reload(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.live {
            self.refilter(cx);
            return;
        }
        self.loading = true;
        self.error = None;
        self.load_generation += 1;
        let generation = self.load_generation;
        let query = if self.search_ids.is_some() {
            self.query.clone()
        } else {
            FOLDERS[self.folder].query.to_string()
        };
        let task = cx
            .background_executor()
            .spawn(async move { data::search(&query, 200) });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update(cx, |this, cx| {
                if this.load_generation != generation {
                    return;
                }
                this.loading = false;
                match result {
                    Ok(mail) => {
                        if this.search_ids.is_some() {
                            this.search_ids =
                                Some(mail.iter().map(|(p, _)| p.thread_id.clone()).collect());
                        }
                        this.mail = mail;
                        this.selected = None;
                        this.refilter(cx);
                    }
                    Err(_) => {
                        this.mail.clear();
                        this.refilter(cx);
                        this.error =
                            Some("Couldn’t load mail. Check durian serve and retry.".into());
                    }
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn reply(&self, window: &mut Window, cx: &mut App) {
        if let Some((_, thread)) = self.current() {
            if let Some(message) = thread.messages.get(self.message) {
                compose::open(Some((thread.subject.clone(), message.clone())), window, cx);
            }
        }
    }

    fn focus_search(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let popup =
            cx.new(|cx| picker::MailPicker::search(self.mail.clone(), self.live, window, cx));
        self.popup_subscription = Some(cx.subscribe_in(&popup, window, |this, _, event, _, cx| {
            if let picker::PickerEvent::Open {
                query,
                mail,
                selected,
            } = event
            {
                this.query = query.clone();
                this.label = None;
                this.load_generation += 1;
                this.loading = false;
                this.error = None;
                this.search_ids = Some(mail.iter().map(|(p, _)| p.thread_id.clone()).collect());
                for item in mail {
                    if let Some(existing) = this
                        .mail
                        .iter_mut()
                        .find(|(p, _)| p.thread_id == item.0.thread_id)
                    {
                        *existing = item.clone();
                    } else {
                        this.mail.push(item.clone());
                    }
                }
                this.refilter(cx);
                this.select(*selected, cx);
            }
        }));
        self.show_picker(popup, "Search mail", window, cx);
    }

    fn edit_tags(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some((preview, _)) = self.current() else {
            return;
        };
        let id = preview.thread_id.clone();
        let tags = preview.tags.clone();
        let all = self.mail.iter().flat_map(|(p, _)| p.tags.clone()).collect();
        let popup = cx.new(|cx| picker::MailPicker::tags(all, tags, self.live, window, cx));
        self.popup_subscription = Some(cx.subscribe(&popup, move |this, _, event, cx| {
            if let picker::PickerEvent::Tags(tags) = event {
                if let Some((p, _)) = this.mail.iter_mut().find(|(p, _)| p.thread_id == id) {
                    p.tags = tags.clone();
                    p.unread = has_tag(p, "unread");
                }
                this.refilter(cx);
            }
        }));
        self.show_picker(popup, "Tags", window, cx);
    }

    fn show_picker(
        &mut self,
        popup: Entity<picker::MailPicker>,
        title: &'static str,
        window: &mut Window,
        cx: &mut App,
    ) {
        self.popup = Some(popup.clone());
        let content = popup.clone();
        window.open_dialog(cx, move |dialog, window, _| {
            dialog
                .title(title)
                .width(window.rem_size() * 39.)
                .child(content.clone())
        });
        window.defer(cx, move |window, cx| {
            popup.update(cx, |this, cx| this.focus(window, cx))
        });
    }

    fn open_attachments(&mut self, window: &mut Window, cx: &mut App) {
        let Some((_, thread)) = self.current() else {
            return;
        };
        let Some(message) = thread.messages.get(self.message) else {
            return;
        };
        if message.attachments.is_empty() {
            window.push_notification(Notification::info("This message has no attachments."), cx);
            return;
        }
        let popup =
            cx.new(|cx| picker::MailPicker::attachments(message.clone(), self.live, window, cx));
        self.show_picker(popup, "Attachments", window, cx);
    }

    fn back(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if window.has_active_dialog(cx) {
            window.close_dialog(cx);
            return;
        }
        self.thread_focused = false;
        if self.search_ids.take().is_some() {
            self.query.clear();
            self.refilter(cx);
            if self.live {
                self.reload(window, cx);
            }
        } else if self.label.take().is_some() {
            self.refilter(cx);
        }
        window.focus(&self.focus, cx);
        cx.notify();
    }

    fn sidebar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        let labels: BTreeSet<_> = self
            .mail
            .iter()
            .flat_map(|(p, _)| visible_tags(p, "inbox"))
            .filter(|t| !["sent", "draft", "archive", "deleted"].contains(&t.as_str()))
            .collect();
        v_flex()
            .size_full()
            .bg(theme.sidebar)
            .child(
                div()
                    .h(rems(3.5))
                    .flex_shrink_0()
                    .px_4()
                    .flex()
                    .items_center()
                    .font_weight(FontWeight::SEMIBOLD)
                    .text_sm()
                    .child("All mail"),
            )
            .child(
                v_flex()
                    .id("folder-scroll")
                    .flex_1()
                    .min_h_0()
                    .px_2()
                    .gap_1()
                    .overflow_y_scrollbar()
                    .child(
                        div()
                            .px_2()
                            .pb_1()
                            .text_xs()
                            .text_color(theme.muted_foreground)
                            .child("Tags"),
                    )
                    .children(FOLDERS.iter().enumerate().map(|(ix, folder)| {
                        let tag = folder.query.trim_start_matches("tag:");
                        let count = self
                            .mail
                            .iter()
                            .filter(|(p, _)| has_tag(p, tag) && p.unread)
                            .count();
                        let icon = match folder.name {
                            "Inbox" => IconName::Inbox,
                            "Starred" => IconName::Pin,
                            "Sent" => IconName::Send,
                            "Drafts" => IconName::FileText,
                            "Archive" => IconName::Archive,
                            _ => IconName::Trash,
                        };
                        let active =
                            ix == self.folder && self.label.is_none() && self.search_ids.is_none();
                        let name = if folder.name == "Starred" {
                            "Pinned"
                        } else {
                            folder.name
                        };
                        Button::new(("folder", ix))
                            .ghost()
                            .small()
                            .w_full()
                            .accessibility_label(name)
                            .child(
                                h_flex()
                                    .w_full()
                                    .gap_2()
                                    .child(Icon::new(icon).small())
                                    .child(div().flex_1().child(name))
                                    .when(count > 0, |row| {
                                        row.child(div().text_xs().child(count.to_string()))
                                    }),
                            )
                            .when(active, |b| {
                                b.bg(theme.primary).text_color(theme.primary_foreground)
                            })
                            .on_click(cx.listener(move |this, _, window, cx| {
                                this.set_folder(ix, window, cx)
                            }))
                    }))
                    .child(
                        div()
                            .px_2()
                            .pt_6()
                            .pb_1()
                            .text_xs()
                            .text_color(theme.muted_foreground)
                            .child("Labels"),
                    )
                    .children(labels.into_iter().enumerate().map(|(ix, label)| {
                        let active = self.label.as_deref() == Some(&label);
                        Button::new(("label", ix))
                            .ghost()
                            .small()
                            .w_full()
                            .accessibility_label(label.clone())
                            .child(
                                h_flex()
                                    .w_full()
                                    .gap_2()
                                    .child(Icon::new(IconName::Tag).small())
                                    .child(label.clone()),
                            )
                            .when(active, |b| {
                                b.bg(theme.primary).text_color(theme.primary_foreground)
                            })
                            .on_click(cx.listener(move |this, _, window, cx| {
                                this.search_ids = None;
                                this.label = if active { None } else { Some(label.clone()) };
                                this.refilter(cx);
                                window.focus(&this.focus, cx);
                            }))
                    })),
            )
            .child(
                div()
                    .p_4()
                    .text_xs()
                    .text_color(theme.muted_foreground)
                    .child(if self.live {
                        "Read-only · durian serve"
                    } else {
                        "Sample mail · read-only"
                    }),
            )
    }

    fn mail_row(&self, row: usize, cx: &mut Context<Self>) -> AnyElement {
        let (preview, thread) = &self.mail[self.rows[row]];
        let theme = cx.theme();
        let selected = self.selected == Some(row);
        let secondary = if selected {
            theme.primary_foreground.opacity(0.8)
        } else {
            theme.muted_foreground
        };
        let foreground = if selected {
            theme.primary_foreground
        } else {
            theme.foreground
        };
        let group = group_title(preview, thread);
        let show_header = row == 0
            || group_title(
                &self.mail[self.rows[row - 1]].0,
                &self.mail[self.rows[row - 1]].1,
            ) != group;
        let sender = thread
            .messages
            .iter()
            .find(|m| data::email_address(&m.from) != demo::OWN_EMAIL)
            .map(|m| data::display_name(&m.from))
            .unwrap_or_else(|| preview.sender.clone());
        let tags = visible_tags(
            preview,
            FOLDERS[self.folder].query.trim_start_matches("tag:"),
        );
        v_flex()
            .w_full()
            .when(show_header, |el| {
                el.child(
                    div()
                        .px_3()
                        .pt_3()
                        .pb_1()
                        .text_xs()
                        .font_weight(FontWeight::MEDIUM)
                        .text_color(if group == "Pinned" {
                            theme.yellow
                        } else {
                            theme.muted_foreground
                        })
                        .child(group),
                )
            })
            .child(
                h_flex()
                    .id(("mail", row))
                    .mx_2()
                    .px_3()
                    .py_2()
                    .gap(rems(0.625))
                    .items_start()
                    .rounded_md()
                    .text_color(foreground)
                    .bg(if selected {
                        theme.primary
                    } else {
                        theme.background
                    })
                    .when(!selected, |el| el.hover(|s| s.bg(theme.list_hover)))
                    .on_click(cx.listener(move |this, _, window, cx| {
                        this.select(row, cx);
                        window.focus(&this.focus, cx);
                    }))
                    .on_mouse_down(
                        MouseButton::Right,
                        cx.listener(move |this, _, window, cx| {
                            this.select(row, cx);
                            window.focus(&this.focus, cx);
                        }),
                    )
                    .context_menu(|menu, _, _| {
                        menu.menu("Reply", Box::new(Reply))
                            .separator()
                            .menu("Archive (prototype)", Box::new(Archive))
                    })
                    .child(avatar(&sender, false, cx))
                    .child(
                        v_flex()
                            .flex_1()
                            .min_w_0()
                            .gap(rems(0.1875))
                            .text_size(rems(0.8125))
                            .line_height(rems(1.0625))
                            .child(
                                h_flex()
                                    .gap_1()
                                    .w_full()
                                    .when(preview.unread, |el| {
                                        el.child(div().size_2().rounded_full().bg(if selected {
                                            foreground
                                        } else {
                                            theme.blue
                                        }))
                                    })
                                    .when(has_tag(preview, "flagged"), |el| {
                                        el.child(
                                            Icon::new(IconName::Pin)
                                                .xsmall()
                                                .text_color(theme.yellow),
                                        )
                                    })
                                    .child(
                                        div()
                                            .flex_1()
                                            .min_w_0()
                                            .truncate()
                                            .font_weight(if preview.unread {
                                                FontWeight::BOLD
                                            } else {
                                                FontWeight::MEDIUM
                                            })
                                            .child(preview.sender.clone()),
                                    )
                                    .child(
                                        div()
                                            .text_xs()
                                            .text_color(secondary)
                                            .child(preview.date.clone()),
                                    ),
                            )
                            .child(
                                h_flex()
                                    .gap_1()
                                    .w_full()
                                    .child(
                                        div()
                                            .flex_1()
                                            .min_w_0()
                                            .truncate()
                                            .font_weight(if preview.unread {
                                                FontWeight::SEMIBOLD
                                            } else {
                                                FontWeight::NORMAL
                                            })
                                            .child(preview.subject.clone()),
                                    )
                                    .when(has_tag(preview, "attachment"), |el| {
                                        el.child(
                                            Icon::new(IconName::Paperclip)
                                                .xsmall()
                                                .text_color(secondary),
                                        )
                                    }),
                            )
                            .child(
                                div()
                                    .w_full()
                                    .line_clamp(2)
                                    .text_color(secondary)
                                    .child(preview.preview.clone()),
                            )
                            .when(!tags.is_empty(), |el| {
                                el.child(h_flex().gap_1().pt_1().children(
                                    tags.into_iter().take(3).map(|tag| {
                                        div()
                                            .px(rems(0.375))
                                            .py(rems(0.0625))
                                            .rounded_full()
                                            .bg(foreground.opacity(0.08))
                                            .text_color(secondary)
                                            .text_xs()
                                            .child(tag)
                                    }),
                                ))
                            }),
                    ),
            )
            .into_any_element()
    }

    fn mail_pane(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let view = cx.entity();
        v_flex()
            .size_full()
            .bg(cx.theme().background)
            .child(
                v_flex()
                    .h(rems(3.5))
                    .flex_shrink_0()
                    .px_5()
                    .justify_center()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(FontWeight::BOLD)
                            .child("Durian"),
                    )
                    .child(
                        div()
                            .text_xs()
                            .text_color(cx.theme().muted_foreground)
                            .child(if self.search_ids.is_some() {
                                "Search results"
                            } else {
                                FOLDERS[self.folder].name
                            }),
                    ),
            )
            .when(self.search_ids.is_some(), |el| {
                el.child(
                    div()
                        .px_3()
                        .pb_2()
                        .text_xs()
                        .text_color(cx.theme().muted_foreground)
                        .child(format!("“{}” · Esc to return", self.query)),
                )
            })
            .when_some(self.label.clone(), |el, label| {
                el.child(
                    div()
                        .px_3()
                        .pb_2()
                        .text_xs()
                        .text_color(cx.theme().muted_foreground)
                        .child(format!("Label: {label} · Esc to clear")),
                )
            })
            .when(self.loading, |el| {
                el.child(div().p_4().text_sm().child("Loading mail…"))
            })
            .when_some(self.error.clone(), |el, error| {
                el.child(div().p_4().text_sm().child(error))
            })
            .when(
                self.rows.is_empty() && !self.loading && self.error.is_none(),
                |el| {
                    el.child(
                        div()
                            .p_6()
                            .text_sm()
                            .text_color(cx.theme().muted_foreground)
                            .child("No mail matches"),
                    )
                },
            )
            .child(
                list(self.mail_list.clone(), move |ix, _, cx| {
                    view.update(cx, |this, cx| this.mail_row(ix, cx))
                })
                .flex_1()
                .min_h_0(),
            )
    }

    fn thread_item(&self, ix: usize, cx: &mut Context<Self>) -> AnyElement {
        let Some((preview, thread)) = self.current() else {
            return div().into_any_element();
        };
        let theme = cx.theme();
        if ix == 0 {
            return v_flex()
                .px_8()
                .pt_8()
                .pb_6()
                .gap_2()
                .child(
                    div()
                        .text_xl()
                        .font_weight(FontWeight::BOLD)
                        .child(thread.subject.clone()),
                )
                .child(
                    h_flex()
                        .gap_2()
                        .children(
                            visible_tags(
                                preview,
                                FOLDERS[self.folder].query.trim_start_matches("tag:"),
                            )
                            .into_iter()
                            .map(|tag| {
                                div()
                                    .rounded_full()
                                    .px_2()
                                    .py_1()
                                    .text_xs()
                                    .text_color(theme.muted_foreground)
                                    .bg(theme.foreground.opacity(0.06))
                                    .child(tag)
                            }),
                        )
                        .child(
                            Button::new("add-tag")
                                .ghost()
                                .xsmall()
                                .icon(IconName::Plus)
                                .tooltip("Edit tags (t)")
                                .on_click(cx.listener(|this, _, w, cx| this.edit_tags(w, cx))),
                        ),
                )
                .into_any_element();
        }
        let m = ix - 1;
        let Some(message) = thread.messages.get(m) else {
            return div().into_any_element();
        };
        let own = data::email_address(&message.from) == demo::OWN_EMAIL;
        let expanded = self.details.contains(&m);
        let focused = self.thread_focused && self.message == m;
        let from = data::display_name(&message.from);
        let date = chrono::DateTime::parse_from_rfc2822(&message.date)
            .map(|d| d.format("%b %-d, %H:%M").to_string())
            .unwrap_or_else(|_| message.date.clone());
        let reply = (thread.subject.clone(), message.clone());
        div()
            .pl(if own { rems(3.5) } else { rems(2.) })
            .pr_8()
            .pb_4()
            .child(
                v_flex()
                    .id(("message", m))
                    .p_6()
                    .pb_4()
                    .gap_4()
                    .w_full()
                    .rounded_lg()
                    .bg(theme.group_box)
                    .shadow_sm()
                    .border_l_2()
                    .border_color(if focused {
                        theme.primary
                    } else {
                        theme.group_box
                    })
                    .on_mouse_down(
                        MouseButton::Left,
                        cx.listener(move |this, _, window, cx| {
                            this.message = m;
                            this.thread_focused = true;
                            window.focus(&this.focus, cx);
                            cx.notify();
                        }),
                    )
                    .child(
                        h_flex()
                            .items_start()
                            .gap_3()
                            .w_full()
                            .child(avatar(&from, true, cx))
                            .child(
                                v_flex()
                                    .flex_1()
                                    .min_w_0()
                                    .items_start()
                                    .gap_1()
                                    .child(
                                        div()
                                            .text_base()
                                            .font_weight(FontWeight::SEMIBOLD)
                                            .child(from),
                                    )
                                    .child(
                                        Button::new(("recipients", m))
                                            .ghost()
                                            .xsmall()
                                            .label(format!(
                                                "To: {}",
                                                data::display_name(&message.to)
                                            ))
                                            .icon(if expanded {
                                                IconName::ChevronDown
                                            } else {
                                                IconName::ChevronRight
                                            })
                                            .tooltip("Message details")
                                            .on_click(cx.listener(move |this, _, _, cx| {
                                                if !this.details.remove(&m) {
                                                    this.details.insert(m);
                                                }
                                                this.message_list.remeasure_items(m + 1..m + 2);
                                                cx.notify();
                                            })),
                                    ),
                            )
                            .child(
                                div()
                                    .text_sm()
                                    .text_color(theme.muted_foreground)
                                    .child(date),
                            ),
                    )
                    .when(expanded, |el| {
                        el.child(
                            v_flex()
                                .gap_1()
                                .text_xs()
                                .text_color(theme.muted_foreground)
                                .child(SelectableText::new(
                                    ("from", m),
                                    format!("From: {}", message.from),
                                ))
                                .child(SelectableText::new(
                                    ("to", m),
                                    format!("To: {}", message.to),
                                )),
                        )
                    })
                    .child(self.contents[m].clone())
                    .when(m == 0, |el| {
                        el.child(
                            h_flex().justify_end().child(
                                Button::new(("reply-message", m))
                                    .ghost()
                                    .small()
                                    .icon(IconName::Reply)
                                    .tooltip("Reply to this message")
                                    .on_click(move |_, window, cx| {
                                        compose::open(Some(reply.clone()), window, cx)
                                    }),
                            ),
                        )
                    }),
            )
            .into_any_element()
    }

    fn thread_pane(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let view = cx.entity();
        v_flex()
            .size_full()
            .bg(cx.theme().background)
            .child(
                h_flex()
                    .h(rems(3.5))
                    .px_6()
                    .gap_1()
                    .flex_shrink_0()
                    .child(command(
                        "compose",
                        IconName::SquarePen,
                        "New message (c)",
                        Compose,
                    ))
                    .child(command("reply", IconName::Reply, "Reply (r)", Reply))
                    .child(command(
                        "attachments",
                        IconName::Paperclip,
                        "Attachments (o)",
                        Attachments,
                    ))
                    .child(
                        Button::new("reply-all")
                            .ghost()
                            .small()
                            .icon(IconName::ReplyAll)
                            .tooltip("Reply all (not connected)")
                            .on_click(|_, w, cx| unsupported("Reply all", w, cx)),
                    )
                    .child(
                        Button::new("forward")
                            .ghost()
                            .small()
                            .icon(IconName::Forward)
                            .tooltip("Forward (not connected)")
                            .on_click(|_, w, cx| unsupported("Forward", w, cx)),
                    )
                    .child(command(
                        "delete",
                        IconName::Trash,
                        "Delete (not connected)",
                        Delete,
                    ))
                    .child(command("pin", IconName::Pin, "Pin (not connected)", Pin))
                    .child(command(
                        "read",
                        IconName::MailOpen,
                        "Mark read (not connected)",
                        ToggleRead,
                    ))
                    .child(div().flex_1())
                    .child(command("search", IconName::Search, "Search (/)", Search))
                    .child(command(
                        "reload",
                        IconName::RefreshCw,
                        "Reload (Ctrl+R)",
                        Reload,
                    )),
            )
            .when(self.current().is_none(), |el| {
                el.child(
                    div()
                        .p_8()
                        .text_lg()
                        .text_color(cx.theme().muted_foreground)
                        .child("Select an email"),
                )
            })
            .child(
                list(self.message_list.clone(), move |ix, _, cx| {
                    view.update(cx, |this, cx| this.thread_item(ix, cx))
                })
                .flex_1()
                .min_h_0(),
            )
    }
}

impl Render for MailApp {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        #[cfg(target_os = "macos")]
        {
            let overlay_active = window.has_active_dialog(cx)
                || window.has_active_sheet(cx)
                || !window.notifications(cx).is_empty()
                || gpui_kit::base::GlobalState::is_in_deferred_context(cx);
            browser::set_native_overlay_active(window, overlay_active);
        }
        let notifications = Root::render_notification_layer(window, cx);
        let dialogs = Root::render_dialog_layer(window, cx);
        let theme = cx.theme();
        let rem = window.rem_size();
        v_flex()
            .relative()
            .size_full()
            .bg(theme.background)
            .text_color(theme.foreground)
            .key_context("MailApp")
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &Next, _, cx| this.navigate(1, cx)))
            .on_action(cx.listener(|this, _: &Previous, _, cx| this.navigate(-1, cx)))
            .on_action(cx.listener(|this, _: &First, _, cx| {
                if this.thread_focused {
                    this.message_list.scroll_to(ListOffset::default());
                    cx.notify();
                } else {
                    this.select(0, cx);
                }
            }))
            .on_action(cx.listener(|this, _: &Last, _, cx| {
                if this.thread_focused {
                    this.message_list.scroll_to_end();
                    cx.notify();
                } else {
                    this.select(this.rows.len().saturating_sub(1), cx);
                }
            }))
            .on_action(cx.listener(|this, _: &NextMessage, w, cx| this.focus_message(1, w, cx)))
            .on_action(
                cx.listener(|this, _: &PreviousMessage, w, cx| this.focus_message(-1, w, cx)),
            )
            .on_action(cx.listener(|this, _: &FocusThread, w, cx| this.focus_message(0, w, cx)))
            .on_action(cx.listener(|this, _: &FocusList, w, cx| {
                this.thread_focused = false;
                w.focus(&this.focus, cx);
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &NextFolder, w, cx| {
                this.set_folder((this.folder + 1) % FOLDERS.len(), w, cx)
            }))
            .on_action(cx.listener(|this, _: &PreviousFolder, w, cx| {
                this.set_folder((this.folder + FOLDERS.len() - 1) % FOLDERS.len(), w, cx)
            }))
            .on_action(cx.listener(|this, _: &Search, w, cx| this.focus_search(w, cx)))
            .on_action(cx.listener(|this, _: &EditTags, w, cx| this.edit_tags(w, cx)))
            .on_action(cx.listener(|this, _: &Attachments, w, cx| this.open_attachments(w, cx)))
            .on_action(cx.listener(|this, _: &ToggleBody, w, cx| {
                if let Some(content) = this.contents.get(this.message) {
                    content.update(cx, |content, cx| content.toggle_format(cx));
                }
                w.focus(&this.focus, cx);
            }))
            .on_action(cx.listener(|this, _: &BrowserPreview, w, cx| {
                if let Some(content) = this.contents.get(this.message) {
                    content.update(cx, |content, cx| content.open_browser(w, cx));
                }
            }))
            .on_action(cx.listener(|this, _: &FocusHtml, w, cx| {
                this.thread_focused = true;
                if let Some(content) = this.contents.get(this.message) {
                    content.update(cx, |content, cx| content.focus_browser(w, cx));
                }
                cx.notify();
            }))
            .on_action(cx.listener(|this, _: &Back, w, cx| this.back(w, cx)))
            .on_action(cx.listener(|this, _: &Reload, w, cx| this.reload(w, cx)))
            .on_action(cx.listener(|this, _: &Reply, w, cx| this.reply(w, cx)))
            .on_action(|_: &Compose, w, cx| compose::open(None, w, cx))
            .on_action(|_: &Archive, w, cx| unsupported("Archive", w, cx))
            .on_action(|_: &ToggleRead, w, cx| unsupported("Mark read", w, cx))
            .on_action(|_: &Pin, w, cx| unsupported("Pin", w, cx))
            .on_action(|_: &Delete, w, cx| unsupported("Delete", w, cx))
            .on_action(cx.listener(|this, _: &ToggleTheme, w, cx| {
                let mode = if cx.theme().is_dark() {
                    ThemeMode::Light
                } else {
                    ThemeMode::Dark
                };
                Theme::change(mode, Some(w), cx);
                apply_mail_theme(cx);
                w.focus(&this.focus, cx);
            }))
            .child(
                TitleBar::new()
                    .child(
                        div()
                            .text_xs()
                            .text_color(theme.muted_foreground)
                            .child("Durian · macOS reference"),
                    )
                    .child(
                        Button::new("view-options")
                            .ghost()
                            .xsmall()
                            .icon(IconName::Ellipsis)
                            .tooltip("View options")
                            .dropdown_menu(|menu, _, _| {
                                menu.menu("Toggle light / dark", Box::new(ToggleTheme))
                            }),
                    ),
            )
            .child(
                div().flex_1().min_h_0().child(
                    h_resizable("mail-panes")
                        .child(
                            resizable_panel()
                                .size(rem * 11.5)
                                .size_range(rem * 10. ..rem * 16.)
                                .child(self.sidebar(cx)),
                        )
                        .child(
                            resizable_panel()
                                .size(rem * 24.)
                                .size_range(rem * 19. ..rem * 34.)
                                .child(self.mail_pane(cx)),
                        )
                        .child(
                            resizable_panel()
                                .size_range(rem * 28. ..px(f32::MAX))
                                .child(self.thread_pane(cx)),
                        ),
                ),
            )
            .children(notifications)
            .children(dialogs)
    }
}

fn main() {
    env_logger::init();
    gpui_kit::application()
        .with_assets(gpui_kit::assets::AllAssets)
        .run(|cx| {
            gpui_kit::init(cx);
            Theme::change(ThemeMode::Light, None, cx);
            apply_mail_theme(cx);
            compose::init(cx);
            content::init(cx);
            picker::init(cx);
            browser::init(cx);
            const NAV: Option<&str> = Some("MailApp && !Input");
            cx.bind_keys([
                KeyBinding::new("j", Next, NAV),
                KeyBinding::new("k", Previous, NAV),
                KeyBinding::new("down", Next, NAV),
                KeyBinding::new("up", Previous, NAV),
                KeyBinding::new("g g", First, NAV),
                KeyBinding::new("shift-g", Last, NAV),
                KeyBinding::new("enter", FocusThread, NAV),
                KeyBinding::new("l", FocusThread, NAV),
                KeyBinding::new("h", FocusList, NAV),
                KeyBinding::new("n", NextMessage, NAV),
                KeyBinding::new("shift-n", PreviousMessage, NAV),
                KeyBinding::new("shift-j", NextFolder, NAV),
                KeyBinding::new("shift-k", PreviousFolder, NAV),
                KeyBinding::new("/", Search, NAV),
                KeyBinding::new("t", EditTags, NAV),
                KeyBinding::new("o", Attachments, NAV),
                KeyBinding::new("v", ToggleBody, NAV),
                KeyBinding::new("shift-v", BrowserPreview, NAV),
                KeyBinding::new("alt-v", FocusHtml, NAV),
                KeyBinding::new("r", Reply, NAV),
                KeyBinding::new("c", Compose, NAV),
                KeyBinding::new("a", Archive, NAV),
                KeyBinding::new("u", ToggleRead, NAV),
                KeyBinding::new("s", Pin, NAV),
                KeyBinding::new("escape", Back, Some("MailApp")),
                KeyBinding::new("ctrl-r", Reload, Some("MailApp")),
                KeyBinding::new("ctrl-n", Compose, Some("MailApp")),
                KeyBinding::new("cmd-n", Compose, Some("MailApp")),
                KeyBinding::new("ctrl-shift-t", ToggleTheme, Some("MailApp")),
                // No global quit: it would bypass a compose window's dirty-close guard.
                KeyBinding::new("ctrl-q", Quit, Some("MailApp")),
            ]);
            cx.on_action(|_: &Quit, cx| {
                if cx.windows().len() == 1 {
                    cx.quit();
                }
            });
            let bounds = Bounds::centered(None, size(px(1360.), px(850.)), cx);
            cx.open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    window_decorations: Some(WindowDecorations::Client),
                    window_background: WindowBackgroundAppearance::Opaque,
                    window_min_size: Some(size(px(960.), px(600.))),
                    ..TitleBar::window_options()
                },
                |window, cx| {
                    window.set_window_title("Durian — macOS reference");
                    let view = cx.new(|cx| MailApp::new(window, cx));
                    window.focus(&view.read(cx).focus.clone(), cx);
                    cx.new(|cx| Root::new(view, window, cx))
                },
            )
            .expect("open Durian window");
            cx.activate(true);
        });
}

#[cfg(test)]
mod tests {
    use super::{demo, filtered_rows, group_title, visible_tags};

    #[test]
    fn filters_intersect_and_keep_domain_indices() {
        let mail = demo::mail();
        let rows = filtered_rows(&mail, "inbox", "RIVERA", Some("design"));
        assert_eq!(rows.len(), 1);
        assert_eq!(mail[rows[0]].0.thread_id, "design-review");
        assert!(filtered_rows(&mail, "sent", "RIVERA", Some("design")).is_empty());
        assert!(filtered_rows(&mail, "inbox", "RIVERA", Some("finance")).is_empty());
    }

    #[test]
    fn groups_and_tags_follow_mac_mail_semantics() {
        let mail = demo::mail();
        assert_eq!(group_title(&mail[0].0, &mail[0].1), "Pinned");
        assert_eq!(group_title(&mail[2].0, &mail[2].1), "March 2026");
        assert_eq!(visible_tags(&mail[0].0, "inbox"), ["events"]);
        assert_eq!(visible_tags(&mail[0].0, "flagged"), ["inbox", "events"]);
    }
}
