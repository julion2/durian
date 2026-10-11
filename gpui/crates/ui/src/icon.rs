use gpui::{
    App, Hsla, IntoElement, Pixels, Radians, RenderOnce, SharedString, Styled, Transformation, Window, prelude::*, px,
    svg,
};

macro_rules! icons {
    ($($variant:ident => $file:literal,)*) => {
        /// The UI's icon set: Lucide (ISC), one file per name under `assets/icons/lucide/`,
        /// generated from `tools/icons/map.tsv` by `tools/icons/build.py`.
        #[derive(Clone, Copy, Debug, PartialEq, Eq)]
        pub enum IconName { $($variant,)* }

        impl IconName {
            pub const ALL: &[IconName] = &[$(IconName::$variant,)*];

            pub fn file(self) -> &'static str {
                match self { $(IconName::$variant => $file,)* }
            }

            pub fn path(self) -> SharedString {
                SharedString::new_static(match self { $(IconName::$variant => concat!("icons/lucide/", $file, ".svg"),)* })
            }
        }
    };
}

icons! {
    SidebarToggle => "sidebar_toggle",
    ArrowLeft => "arrow_left",
    ArrowRight => "arrow_right",
    ChevronDownSmall => "chevron_down_small",
    Search => "search",
    Compose => "compose",
    Disclosure => "disclosure",
    Views => "views",
    More => "more",
    CloseSmall => "close_small",
    Folder => "folder",
    Plus => "plus",
    Filter => "filter",
    Display => "display",
    PanelRight => "panel_right",
    GroupCaret => "group_caret",
    StatusCheck => "status_check",
    MailInbox => "mail_inbox",
    MailSend => "mail_send",
    MailEmail => "mail_email",
    MailDraft => "mail_draft",
    MailTrash => "mail_trash",
    MailClock => "mail_clock",
    MailLabel => "mail_label",
    MailTerminal => "mail_terminal",
    MailUsers => "mail_users",
    MailStarred => "mail_starred",
    MailStar => "mail_star",
    MailSnooze => "mail_snooze",
    MailDone => "mail_done",
    MailUnreadOnly => "mail_unread_only",
    MailTagAdd => "mail_tag_add",
    MailAttach => "mail_attach",
    MailSubmit => "mail_submit",
    MailOptions => "mail_options",
    MailExpand => "mail_expand",
    MailReply => "mail_reply",
    MailForward => "mail_forward",
    MailDownload => "mail_download",
    Calendar => "calendar",
    MapPin => "map_pin",
    Video => "video",
}

/// What an [`Icon`] draws: one of the UI's own icons, or any SVG the app serves – e.g. glyphs
/// named in data (per-project icons) or an app-specific set.
#[derive(Clone, Debug, PartialEq)]
pub enum IconSource {
    Named(IconName),
    Path(SharedString),
}

impl From<IconName> for IconSource {
    fn from(name: IconName) -> Self {
        IconSource::Named(name)
    }
}

impl IconSource {
    fn path(&self) -> SharedString {
        match self {
            IconSource::Named(name) => name.path(),
            IconSource::Path(path) => path.clone(),
        }
    }
}

#[derive(IntoElement)]
pub struct Icon {
    path: SharedString,
    size: Pixels,
    color: Option<Hsla>,
    rotation: Option<Radians>,
}

impl Icon {
    pub fn new(source: impl Into<IconSource>) -> Self {
        Self::path(source.into().path())
    }


    pub fn path(path: impl Into<SharedString>) -> Self {
        Self { path: path.into(), size: px(14.), color: None, rotation: None }
    }

    pub fn size(mut self, size: f32) -> Self {
        self.size = px(size);
        self
    }

    pub fn color(mut self, color: Hsla) -> Self {
        self.color = Some(color);
        self
    }

    /// Used for disclosure carets (collapsed = -90°).
    pub fn rotate(mut self, radians: f32) -> Self {
        self.rotation = Some(Radians(radians));
        self
    }
}

impl RenderOnce for Icon {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        svg()
            .path(self.path)
            .size(self.size)
            .flex_none()
            .when_some(self.color, |this, color| this.text_color(color))
            .when_some(self.rotation, |this, r| this.with_transformation(Transformation::rotate(r)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every name must have its SVG – a missing one would render as nothing.
    #[test]
    fn every_icon_has_a_file() {
        let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        let missing: Vec<_> = IconName::ALL.iter().filter(|i| !root.join("assets").join(i.path().as_ref()).exists()).collect();
        assert!(missing.is_empty(), "no SVG for {missing:?} – run tools/icons/build.py");
    }
}
