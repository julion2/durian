//! Icons of the local Linear recreation: the set extracted from Linear (`assets/icons/`). They're
//! for this reference build only – the mail UI and the `ui` crate draw Lucide.

use gpui::SharedString;
use ui::IconSource;

macro_rules! linear_icons {
    ($($variant:ident => $file:literal,)*) => {
        #[derive(Clone, Copy, Debug, PartialEq, Eq)]
        pub enum LinearIcon { $($variant,)* }

        impl From<LinearIcon> for IconSource {
            fn from(icon: LinearIcon) -> Self {
                IconSource::Path(SharedString::new_static(match icon { $(LinearIcon::$variant => concat!("icons/", $file, ".svg"),)* }))
            }
        }
    };
}

linear_icons! {
    SidebarToggle => "sidebar_toggle",
    History => "history",
    ArrowLeft => "arrow_left",
    ArrowRight => "arrow_right",
    ChevronDownSmall => "chevron_down_small",
    Search => "search",
    Compose => "compose",
    Inbox => "inbox",
    MyIssues => "my_issues",
    Pulse => "pulse",
    Disclosure => "disclosure",
    Initiatives => "initiatives",
    Projects => "projects",
    Views => "views",
    Customers => "customers",
    More => "more",
    FolderAdd => "folder_add",
    CloseSmall => "close_small",
    Folder => "folder",
    Plus => "plus",
    Home => "home",
    Issues => "issues",
    TeamProjects => "team_projects",
    TeamViews => "team_views",
    Cycles => "cycles",
    Filter => "filter",
    Display => "display",
    PanelRight => "panel_right",
    GroupCaret => "group_caret",
    PriorityUrgent => "priority_urgent",
    PriorityHigh => "priority_high",
    PriorityMedium => "priority_medium",
    PriorityLow => "priority_low",
    PriorityNone => "priority_none",
    Blocked => "blocked",
    Blocking => "blocking",
    DueOverdue => "due_overdue",
    Calendar => "calendar",
    GroupBacklog => "group_backlog",
    GroupCompleted => "group_completed",
    GroupCanceled => "group_canceled",
    StatusCheck => "status_check",
    StatusCross => "status_cross",
    MailInbox => "mail_inbox",
    MailSend => "mail_send",
    MailEmail => "mail_email",
    MailDraft => "mail_draft",
    MailTrash => "mail_trash",
    MailArchive => "mail_archive",
    MailClock => "mail_clock",
    MailPin => "mail_pin",
    MailLabel => "mail_label",
    MailTerminal => "mail_terminal",
    MailUsers => "mail_users",
    MailStarred => "mail_starred",
    MailStar => "mail_star",
    MailSnooze => "mail_snooze",
    MailDone => "mail_done",
    MailBell => "mail_bell",
    MailUnreadOnly => "mail_unread_only",
    MailTagAdd => "mail_tag_add",
    MailAttach => "mail_attach",
    MailSubmit => "mail_submit",
    MailReaction => "mail_reaction",
    MailOptions => "mail_options",
    MailExpand => "mail_expand",
    MailReply => "mail_reply",
    MailForward => "mail_forward",
    MailDownload => "mail_download",
}

/// Icons named in the fixture data, e.g. per-project glyphs: `project_hiring` → `icons/project_hiring.svg`.
pub fn named(name: &str) -> IconSource {
    IconSource::Path(format!("icons/{name}.svg").into())
}
