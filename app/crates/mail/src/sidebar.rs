//! Mail navigation, tag-based: system views on
//! top, then tags (no folders), then saved queries that mirror the CLI.

use std::collections::HashSet;

use gpui::{AnyElement, Context, Entity, IntoElement, Render, SharedString, Window, deferred, div, prelude::*, px};

use crate::contact_avatar;
use crate::model::{Account, Mailbox, account_inbox_query, folder_unread_query};
use ui::theme::{ActiveTheme, typography::*};
use ui::{ButtonStyle, Icon, IconButton, IconName, Kbd, NavItem, SectionHeader, StyledExt, color_dot};

#[derive(Clone, Copy, PartialEq, Eq, Hash)]
enum Section {
    Tags,
    Views,
}

pub enum SidebarEvent {
    Navigate(SharedString),
    ToggleSidebar,
}

/// Window chrome for mail: room for the traffic lights and the sidebar toggle. No history or
/// back/forward – mail navigation is j/k, g-shortcuts and ⌘K, there's no deep stack to go back in.
pub fn mail_titlebar(open: bool, on_toggle: impl Fn(&gpui::ClickEvent, &mut Window, &mut gpui::App) + 'static) -> impl IntoElement {
    div()
        .flex()
        .flex_none()
        .items_center()
        .h(px(ui::TITLEBAR_HEIGHT))
        .pl(px(89.))
        .pr(px(8.))
        .window_control_area(gpui::WindowControlArea::Drag)
        .child(IconButton::new(if open { "sidebar-collapse" } else { "sidebar-expand" }, IconName::SidebarToggle)
            .tooltip(if open { "Hide sidebar" } else { "Show sidebar" }).on_click(on_toggle))
}

impl gpui::EventEmitter<SidebarEvent> for MailSidebar {}

pub struct MailSidebar {
    mailbox: Entity<Mailbox>,
    menu_open: bool,
    active: SharedString,
    collapsed: HashSet<Section>,
}

impl MailSidebar {
    pub fn new(mailbox: Entity<Mailbox>, cx: &mut Context<Self>) -> Self {
        cx.observe(&mailbox, |_, _, cx| cx.notify()).detach();
        Self { mailbox, menu_open: false, active: "inbox".into(), collapsed: HashSet::default() }
    }

    fn item(&self, key: &'static str, label: &'static str, icon: IconName, count: Option<SharedString>, cx: &mut Context<Self>) -> NavItem {
        NavItem::new(key, label)
            .icon(icon)
            .active(self.active == key)
            .when_some(count, |item, c| item.trailing(c))
            .on_click(cx.listener(move |this, _, _, cx| {
                this.active = key.into();
                cx.emit(SidebarEvent::Navigate(key.into()));
                cx.notify();
            }))
    }

    fn pick(&mut self, account: Option<SharedString>, cx: &mut Context<Self>) {
        self.menu_open = false;
        self.mailbox.update(cx, |m, cx| m.set_account_filter(account, cx));
        cx.notify();
    }

    pub fn set_active(&mut self, key: impl Into<SharedString>, cx: &mut Context<Self>) {
        self.active = key.into();
        cx.notify();
    }

    fn toggle(&mut self, section: Section, cx: &mut Context<Self>) {
        if !self.collapsed.remove(&section) {
            self.collapsed.insert(section);
        }
        cx.notify();
    }
}

impl Render for MailSidebar {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let mailbox = self.mailbox.read(cx);
        let live = mailbox.remote.is_some();
        let (unread, per_tag) = mailbox.unread_counts();
        let drafts = mailbox.drafts.len();
        let tags: Vec<_> = mailbox.tags.iter().map(|t| {
            let count = if live {
                mailbox.count_for(&mailbox.sidebar_count_query(&format!("tag:{}", crate::durian::translate::quoted(&t.name))))
            } else {
                Some(per_tag.get(&t.name).copied().unwrap_or(0) as u64)
            };
            (t.name.clone(), t.color, count)
        }).collect();
        let views = mailbox.views.clone();
        let me = mailbox.me.clone();
        let accounts: Vec<(Account, Option<u64>)> = mailbox.accounts.iter().map(|a| {
            let count = if live { mailbox.count_for(&account_inbox_query(Some(&a.id))) } else { Some(mailbox.account_unread(&a.id) as u64) };
            (a.clone(), count)
        }).collect();
        let all_unread = if live { mailbox.count_for(&account_inbox_query(None)) } else { Some(mailbox.threads.iter().filter(|t| t.unread()).count() as u64) };
        let inbox_unread = if live { mailbox.count_for(&mailbox.sidebar_count_query("tag:inbox")) } else { Some(unread as u64) };
        let active = mailbox.account_filter.as_ref().and_then(|id| mailbox.account(id)).cloned();
        // durian's profiles, when the data source has them, replace accounts in the switcher
        // and their folders replace the fixed system views
        let profiles = mailbox.profiles.clone();
        let profile = mailbox.active_profile().cloned();
        let profile_ix = mailbox.profile;
        let open_folder = mailbox.folder;
        // the sidebar starts on "inbox": with profiles, that's the open folder
        let on_folder = |ix: usize, active: &SharedString| *active == format!("folder:{ix}") || (active.as_ref() == "inbox" && open_folder == Some(ix));
        let folder_counts: Vec<_> = profile.as_ref().map(|p| p.folders.iter().enumerate().map(|(ix, folder)| {
            if live {
                count_label(mailbox.count_for(&folder_unread_query(p, folder)))
            } else {
                count_label(Some(if on_folder(ix, &self.active) { unread as u64 } else { 0 }))
            }
        }).collect()).unwrap_or_default();
        let (switcher_label, switcher_color): (SharedString, Option<gpui::Hsla>) = match &profile {
            Some(p) => (p.name.clone(), p.color),
            None => (active.as_ref().map_or(SharedString::from("All accounts"), |a| a.name.clone()), active.as_ref().map(|a| a.color)),
        };
        let theme = cx.theme().clone();
        let (bg, heading, muted) = (theme.bg_app, theme.text_heading, theme.text_muted);
        let tags_open = !self.collapsed.contains(&Section::Tags);
        let views_open = !self.collapsed.contains(&Section::Views);

        let account = div()
            .flex()
            .items_center()
            .h(px(44.))
            .flex_none()
            .px(px(12.))
            .child(
                // account switcher: all accounts or one
                // shrinks (the name truncates) so the buttons on the right always fit
                div()
                    .id("account")
                    .flex()
                    .items_center()
                    .min_w_0()
                    .h(px(28.))
                    .pl(px(4.))
                    .pr(px(9.))
                    .gap(px(7.))
                    .rounded(theme.radius.lg)
                    .hover(|s| s.bg(theme.bg_nav_hover))
                    .when(self.menu_open, |s| s.bg(theme.bg_nav_hover))
                    .child(
                        div()
                            .relative()
                            .flex_none()
                            .child(contact_avatar(&me).size(20.))
                            .when_some(switcher_color, |s, color| {
                                s.child(div().absolute().right(px(-2.)).bottom(px(-2.)).size(px(9.)).rounded_full().bg(color).border(px(2.)).border_color(bg))
                            }),
                    )
                    .child(
                        div()
                            .min_w_0()
                            .truncate()
                            .text_size(px(SIZE_SMALL))
                            .font_weight(WEIGHT_WORKSPACE)
                            .text_color(heading)
                            .child(switcher_label),
                    )
                    .child(div().flex_none().child(Icon::new(IconName::ChevronDownSmall).size(8.).color(muted)))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.menu_open = !this.menu_open;
                        cx.notify();
                    })),
            )
            .child(div().flex_1().min_w(px(4.)))
            .child(
                div()
                    .flex()
                    .flex_none()
                    .gap(px(4.))
                    // same actions as the shortcuts (⌘K, c), so menus, keys and buttons stay in sync
                    .child(
                        IconButton::new("search", IconName::Search)
                            .tooltip("Command menu")
                            .on_click(|_, window, cx| window.dispatch_action(Box::new(crate::OpenCommandMenu), cx)),
                    )
                    .child(
                        IconButton::new("compose", IconName::Compose)
                            .style(ButtonStyle::Raised)
                            .tooltip(if self.active == "calendar" { "New event" } else { "New message" })
                            .on_click(|_, window, cx| window.dispatch_action(Box::new(crate::ComposeHere), cx)),
                    ),
            );

        let folders = profile.as_ref().filter(|p| !p.folders.is_empty()).map(|p| {
            div()
                .pb(px(8.))
                .children(p.folders.iter().enumerate().map(|(ix, folder)| {
                    let key: SharedString = format!("folder:{ix}").into();
                    let active = on_folder(ix, &self.active);
                    NavItem::new(SharedString::from(format!("folder-{ix}")), folder.name.clone())
                        .icon(crate::durian::profiles::ui_icon(&folder.icon))
                        .active(active)
                        .when_some(folder_counts[ix].clone(), |item, count| item.trailing(count))
                        .on_click(cx.listener(move |this, _, _, cx| {
                            this.active = key.clone();
                            cx.emit(SidebarEvent::Navigate(key.clone()));
                            cx.notify();
                        }))
                }))
                // the app's calendar, apart from the profile's folders (which may have a mail
                // folder of that name: invitations)
                .child(div().h(px(1.)).mx(px(8.)).my(px(6.)).bg(theme.border_panel))
                .child(self.item("calendar", "Calendar", IconName::Calendar, None, cx))
        });
        let system = div()
            .pb(px(8.))
            .child(self.item("inbox", "Inbox", IconName::MailInbox, count_label(inbox_unread), cx))
            .child(self.item("starred", "Starred", IconName::MailStarred, None, cx))
            .child(self.item("snoozed", "Snoozed", IconName::MailClock, None, cx))
            .child(self.item("sent", "Sent", IconName::MailSend, None, cx))
            // Drafts only shows up while there are drafts
            .when(drafts > 0, |s| s.child(self.item("drafts", "Drafts", IconName::MailDraft, Some(drafts.to_string().into()), cx)))
            .child(self.item("all", "All mail", IconName::MailEmail, None, cx))
            .child(self.item("calendar", "Calendar", IconName::Calendar, None, cx));

        let tag_section = div()
            .child(
                SectionHeader::new("tags", "Tags", tags_open)
                    .action(IconButton::new("add-tag", IconName::Plus).tooltip("Create tags by adding them to a thread"))
                    .on_toggle(cx.listener(|this, _, _, cx| this.toggle(Section::Tags, cx))),
            )
            .when(tags_open, |this| {
                this.pb(px(8.)).children(tags.into_iter().map(|(name, color, count)| {
                    let key: SharedString = format!("tag:{name}").into();
                    let active = self.active == key;
                    let menu_name = name.clone();
                    div()
                        .on_mouse_down(gpui::MouseButton::Right, move |event, window, cx| {
                            crate::context_menus::open(event.position, crate::context_menus::tag(menu_name.clone()), window, cx);
                        })
                        .child(
                            NavItem::new(SharedString::from(format!("tag-{name}")), name)
                                .leading(color_dot(color))
                                .when_some(count_label(count), |item, count| item.trailing(count))
                                .active(active)
                                .on_click(cx.listener(move |this, _, _, cx| {
                                    this.active = key.clone();
                                    cx.emit(SidebarEvent::Navigate(key.clone()));
                                    cx.notify();
                                })),
                        )
                }))
            });

        let view_section = div()
            .child(
                SectionHeader::new("views", "Views", views_open)
                    .action(IconButton::new("add-view", IconName::Plus).tooltip("Use Save view for this session; configure persistent folders in profiles.pkl"))
                    .on_toggle(cx.listener(|this, _, _, cx| this.toggle(Section::Views, cx))),
            )
            .when(views_open, |this| {
                this.children(views.into_iter().map(|view| {
                    let key: SharedString = format!("view:{}", view.name).into();
                    let active = self.active == key;
                    NavItem::new(SharedString::from(format!("view-{}", view.name)), view.name.clone())
                        .icon(IconName::Views)
                        .active(active)
                        .on_click(cx.listener(move |this, _, _, cx| {
                            this.active = key.clone();
                            cx.emit(SidebarEvent::Navigate(key.clone()));
                            cx.notify();
                        }))
                }))
            });

        div()
            .flex()
            .flex_col()
            .flex_none()
            // the workspace decides the width (resizable)
            .w_full()
            .h_full()
            .bg(bg)
            .child(mail_titlebar(true, cx.listener(|_, _, _, cx| cx.emit(SidebarEvent::ToggleSidebar))))
            .when(self.menu_open, |s| {
                let account_keys = |n: usize| -> Vec<SharedString> {
                    let name = if n == 0 { "show_all_accounts".to_string() } else { format!("show_account_{n}") };
                    let Some(action) = crate::commands::action_for(&name) else { return Vec::new() };
                    crate::commands::keys_for(action.as_ref(), &[crate::keymaps::Context::List, crate::keymaps::Context::Thread, crate::keymaps::Context::Calendar], cx)
                };
                let row = |id: SharedString, leading: AnyElement, label: SharedString, detail: Option<SharedString>, count: Option<SharedString>, keys: Vec<SharedString>, selected: bool| {
                    div()
                        .id(id)
                        .flex()
                        .items_center()
                        .gap(px(8.))
                        .h(px(32.))
                        .px(px(8.))
                        .rounded(theme.radius.sm)
                        .when(selected, |s| s.bg(theme.bg_nav_active))
                        .hover(|s| s.bg(theme.bg_nav_hover))
                        .child(div().flex().flex_none().items_center().justify_center().w(px(16.)).child(leading))
                        .child(div().flex_none().text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_primary).child(label))
                        .child(div().flex_1().min_w_0().truncate().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).children(detail))
                        .children(count.map(|count| div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child(count)))
                        .children(keys.into_iter().map(Kbd::new))
                };
                let current = active.as_ref().map(|a| a.id.clone());
                let menu = div()
                    .w(px(300.))
                    .p(px(4.))
                    .bg(theme.bg_card)
                    .border_hairline()
                    .border_color(theme.border_card)
                    .rounded(theme.radius.lg)
                    .shadow(theme.overlay_shadow());
                let menu = if profiles.is_empty() {
                    let mut menu = menu
                        .child(
                            row("acct-all".into(), Icon::new(IconName::MailInbox).color(theme.text_tertiary).into_any_element(), "All accounts".into(), None, count_label(all_unread), account_keys(0), current.is_none())
                                .on_click(cx.listener(|this, _, _, cx| this.pick(None, cx))),
                        )
                        .child(div().h(px(1.)).my(px(4.)).bg(theme.border_card));
                    for (n, (account, unread)) in accounts.iter().enumerate() {
                        let id = account.id.clone();
                        let selected = current.as_ref() == Some(&account.id);
                        let key = account_keys(n + 1);
                        menu = menu.child(
                            row(format!("acct-{}", account.id).into(), div().size(px(8.)).rounded_full().bg(account.color).into_any_element(), account.name.clone(), Some(account.email.clone()), count_label(*unread), key, selected)
                                .on_click(cx.listener(move |this, _, _, cx| this.pick(Some(id.clone()), cx))),
                        );
                    }
                    menu.child(div().h(px(1.)).my(px(4.)).bg(theme.border_card))
                        .child(div().px(px(8.)).py(px(6.)).text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child("Manage accounts in config.pkl"))
                } else {
                    // profiles as configured in durian (profiles.pkl): name, accounts, ⌘1…
                    menu.children(profiles.iter().enumerate().map(|(n, p)| {
                        let accounts: SharedString = if p.accounts.iter().any(|a| a == "*") { "All accounts".into() } else { p.accounts.iter().map(|a| a.as_ref()).collect::<Vec<_>>().join(", ").into() };
                        let leading = match p.color {
                            Some(color) => div().size(px(8.)).rounded_full().bg(color).into_any_element(),
                            None => Icon::new(IconName::MailInbox).color(theme.text_tertiary).into_any_element(),
                        };
                        let key = account_keys(n + 1);
                        row(format!("profile-{n}").into(), leading, p.name.clone(), Some(accounts), None, key, profile_ix == Some(n))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.menu_open = false;
                                this.active = "folder:0".into();
                                cx.emit(SidebarEvent::Navigate(format!("profile:{n}").into()));
                                cx.notify();
                            }))
                    }))
                };
                s.child(
                    deferred(
                        div()
                            .id("account-menu-backdrop")
                            .absolute()
                            .inset_0()
                            .occlude()
                            .on_mouse_down(gpui::MouseButton::Left, cx.listener(|this, _, _, cx| {
                                this.menu_open = false;
                                cx.notify();
                            }))
                            .child(div().absolute().left(px(12.)).top(px(84.))
                                .on_mouse_down_out(cx.listener(|this, _, _, cx| {
                                    this.menu_open = false;
                                    cx.notify();
                                }))
                                .on_mouse_down(gpui::MouseButton::Left, |_, _, cx| cx.stop_propagation()).child(menu)),
                    )
                    .with_priority(3),
                )
            })
            .child(account)
            .child(
                div()
                    .id("mail-sidebar-scroll")
                    .flex_1()
                    .min_h_0()
                    .overflow_y_scroll()
                    .px(px(12.))
                    .pt(px(8.))
                    .map(|s| match folders {
                        Some(folders) => s.child(folders),
                        None => s.child(system),
                    })
                    .child(tag_section)
                    .child(view_section),
            )
    }
}

/// Zero gets no badge; an unknown live count is visibly distinct from zero.
fn count_label(count: Option<u64>) -> Option<SharedString> {
    match count {
        None => Some("—".into()),
        Some(0) => None,
        Some(n) => Some(n.to_string().into()),
    }
}

#[cfg(test)]
mod count_tests {
    #[test]
    fn unknown_zero_and_large_counts_are_distinct_badges() {
        assert_eq!(super::count_label(None).as_deref(), Some("—"));
        assert_eq!(super::count_label(Some(0)), None);
        assert_eq!(super::count_label(Some(501)).as_deref(), Some("501"));
    }
}
