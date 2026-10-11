//! Left navigation. Measurements from `design/raw/sidebar.txt`:
//! 244px wide, items 28px tall with 1px vertical margin, 12px horizontal inset, 8px radius.

use std::collections::HashSet;
use std::f32::consts::FRAC_PI_2;
use std::rc::Rc;

use gpui::{
    AnyElement, Context, ElementId, EventEmitter, Hsla, IntoElement, Render, SharedString, Window, div, img,
    prelude::*, px,
};

use crate::model::{Favorite, Fixture};
use ui::theme::{ActiveTheme, typography::*};
use crate::linear_icons::{self, LinearIcon};
use ui::{ButtonStyle, Icon, IconButton};

pub use ui::{SIDEBAR_WIDTH, TITLEBAR_HEIGHT};

#[derive(Clone, PartialEq, Eq, Hash, Debug)]
pub enum Section {
    Workspace,
    Favorites,
    FavoriteFolder(SharedString),
    Teams,
    Team(SharedString),
    Try,
}

#[derive(Clone, PartialEq, Eq, Debug)]
pub enum NavEvent {
    Navigate(SharedString),
}

pub struct Sidebar {
    fixture: Rc<Fixture>,
    active: SharedString,
    collapsed: HashSet<Section>,
}

impl EventEmitter<NavEvent> for Sidebar {}

/// What goes in front of a nav label.
enum Leading {
    Icon(LinearIcon),
    Named(SharedString, Option<Hsla>),
}

impl Sidebar {
    pub fn new(fixture: Rc<Fixture>) -> Self {
        Self { fixture, active: "my-issues".into(), collapsed: HashSet::default() }
    }

    fn toggle(&mut self, section: Section, cx: &mut Context<Self>) {
        if !self.collapsed.remove(&section) {
            self.collapsed.insert(section);
        }
        cx.notify();
    }

    fn is_open(&self, section: &Section) -> bool {
        !self.collapsed.contains(section)
    }

    fn nav_item(
        &self,
        key: impl Into<SharedString>,
        label: impl Into<SharedString>,
        leading: Leading,
        indent: f32,
        subtitle: Option<SharedString>,
        cx: &mut Context<Self>,
    ) -> gpui::Stateful<gpui::Div> {
        let key: SharedString = key.into();
        let active = self.active == key;
        let theme = cx.theme();
        let fg = if active { theme.text_nav_active } else { theme.text_muted };
        let (hover_bg, active_bg, faint) = (theme.bg_nav_hover, theme.bg_nav_active, theme.text_faint);
        let icon = match leading {
            Leading::Icon(name) => Icon::new(name).color(fg),
            Leading::Named(name, color) => Icon::new(linear_icons::named(&name)).color(color.unwrap_or(fg)),
        };

        div()
            .id(ElementId::Name(key.clone()))
            .flex()
            .items_center()
            .h(px(28.))
            .my(px(1.))
            .ml(px(indent))
            .pl(px(if indent > 0. { 6. } else { 8. }))
            .pr(px(9.))
            .gap(px(6.))
            .rounded(theme.radius.md)
            .text_size(px(SIZE_SMALL))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(fg)
            .when(active, |s| s.bg(active_bg))
            .when(!active, |s| s.hover(|s| s.bg(hover_bg)))
            .child(icon)
            .child(div().min_w_0().truncate().child(label.into()))
            .when_some(subtitle, |this, subtitle| {
                // document favorites show their parent after the title; it gives way first
                this.child(
                    div()
                        .min_w(px(33.))
                        .flex_shrink(3.)
                        .font_weight(WEIGHT_NORMAL)
                        .text_color(faint)
                        .truncate()
                        .child(subtitle),
                )
            })
            .on_click(cx.listener(move |this, _, _, cx| {
                this.active = key.clone();
                cx.emit(NavEvent::Navigate(key.clone()));
                cx.notify();
            }))
    }

    fn section_header(
        &self,
        section: Section,
        label: &'static str,
        trailing: Option<(LinearIcon, &'static str)>,
        cx: &mut Context<Self>,
    ) -> impl IntoElement + use<> {
        let open = self.is_open(&section);
        let theme = cx.theme();
        div()
            .group("section-header")
            .flex()
            .items_center()
            .justify_between()
            .h(px(28.))
            .mt(px(8.))
            .mb(px(1.))
            .pl(px(5.))
            .child(
                div()
                    .id(ElementId::Name(format!("section-{label}").into()))
                    .flex()
                    .flex_1()
                    .items_center()
                    .h_full()
                    .px(px(4.))
                    .rounded(theme.radius.md)
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_muted)
                    .child(label)
                    .child(
                        div().mt(px(1.)).ml(px(2.)).child(
                            Icon::new(LinearIcon::Disclosure)
                                .size(16.)
                                .color(theme.text_muted)
                                .rotate(if open { FRAC_PI_2 } else { 0. }),
                        ),
                    )
                    .on_click(cx.listener(move |this, _, _, cx| this.toggle(section.clone(), cx))),
            )
            .children(trailing.map(|(icon, id)| {
                div()
                    .mr(px(2.))
                    .invisible()
                    .group_hover("section-header", |s| s.visible())
                    .child(IconButton::new(id, icon).size(24.))
            }))
    }

    fn workspace_row(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let name = self.fixture.workspace.name.clone();
        div()
            .flex()
            .items_center()
            .h(px(44.))
            .flex_none()
            .px(px(12.))
            .child(
                div()
                    .id("workspace-menu")
                    .flex()
                    .items_center()
                    .h(px(28.))
                    .pl(px(5.))
                    .pr(px(9.))
                    .gap(px(6.))
                    .rounded(theme.radius.lg)
                    .hover(|s| s.bg(theme.bg_nav_hover))
                    .child(workspace_logo(&name))
                    .child(
                        div()
                            .text_size(px(SIZE_SMALL))
                            .font_weight(WEIGHT_WORKSPACE)
                            .text_color(theme.text_heading)
                            .child(name),
                    )
                    .child(Icon::new(LinearIcon::ChevronDownSmall).size(8.).color(theme.text_muted)),
            )
            .child(div().flex_1().min_w(px(8.)))
            .child(
                div()
                    .flex()
                    .gap(px(4.))
                    .child(IconButton::new("search", LinearIcon::Search))
                    .child(IconButton::new("compose", LinearIcon::Compose).style(ButtonStyle::Raised)),
            )
    }

    fn favorite(&self, fav: &Favorite, indent: f32, cx: &mut Context<Self>) -> AnyElement {
        match fav {
            Favorite::Document { title, subtitle, icon, color } => {
                let leading = Leading::Named(icon.clone(), Some(*color));
                self.nav_item(format!("doc-{title}"), title.clone(), leading, indent, Some(subtitle.clone()), cx)
                    .into_any_element()
            }
            Favorite::View { title, icon } => {
                let color = cx.theme().icon_view;
                self.nav_item(format!("view-{title}"), title.clone(), Leading::Named(icon.clone(), Some(color)), indent, None, cx)
                    .into_any_element()
            }
            Favorite::Folder { title, children } => {
                let section = Section::FavoriteFolder(title.clone());
                let open = self.is_open(&section);
                let theme = cx.theme();
                div()
                    .child(
                        div()
                            .id(ElementId::Name(format!("folder-{title}").into()))
                            .flex()
                            .items_center()
                            .h(px(28.))
                            .my(px(1.))
                            .pl(px(8.))
                            .gap(px(6.))
                            .rounded(theme.radius.md)
                            .text_size(px(SIZE_SMALL))
                            .font_weight(WEIGHT_MEDIUM)
                            .text_color(theme.text_muted)
                            .hover(|s| s.bg(theme.bg_nav_hover))
                            .child(Icon::new(LinearIcon::Folder).color(theme.text_muted))
                            .child(title.clone())
                            .child(
                                Icon::new(LinearIcon::Disclosure)
                                    .size(16.)
                                    .color(theme.text_muted)
                                    .rotate(if open { FRAC_PI_2 } else { 0. }),
                            )
                            .on_click(cx.listener(move |this, _, _, cx| this.toggle(section.clone(), cx))),
                    )
                    .when(open, |this| {
                        this.children(children.iter().map(|child| self.favorite(child, indent + 15., cx)))
                    })
                    .into_any_element()
            }
        }
    }

    fn team(&self, team_ix: usize, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let team = &self.fixture.teams[team_ix];
        let section = Section::Team(team.key.clone());
        let open = self.is_open(&section);
        let theme = cx.theme();
        let key = team.key.to_string().to_lowercase();
        div()
            .child(
                div()
                    .id(ElementId::Name(format!("team-{key}").into()))
                    .flex()
                    .items_center()
                    .h(px(28.))
                    .my(px(1.))
                    .pl(px(9.))
                    .rounded(theme.radius.md)
                    .text_size(px(SIZE_SMALL))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_muted)
                    .hover(|s| s.bg(theme.bg_nav_hover))
                    .child(Icon::new(linear_icons::named(&team.icon)).color(team.color))
                    .child(div().ml(px(7.)).child(team.name.clone()))
                    .child(
                        div().ml(px(6.)).child(
                            Icon::new(LinearIcon::Disclosure)
                                .size(16.)
                                .color(theme.text_faint)
                                .rotate(if open { FRAC_PI_2 } else { 0. }),
                        ),
                    )
                    .on_click(cx.listener(move |this, _, _, cx| this.toggle(section.clone(), cx))),
            )
            .when(open, |this| {
                this.child(self.nav_item(format!("{key}-home"), "Home", Leading::Icon(LinearIcon::Home), 19., None, cx))
                    .child(self.nav_item(format!("{key}-issues"), "Issues", Leading::Icon(LinearIcon::Issues), 19., None, cx))
                    .child(self.nav_item(format!("{key}-projects"), "Projects", Leading::Icon(LinearIcon::TeamProjects), 19., None, cx))
                    .child(self.nav_item(format!("{key}-views"), "Views", Leading::Icon(LinearIcon::TeamViews), 19., None, cx))
            })
    }
}

/// Window chrome row above the sidebar: room for the traffic lights, then sidebar toggle,
/// history and back/forward – positions measured from the desktop app.
pub fn titlebar() -> impl IntoElement {
    div()
        .flex()
        .items_center()
        .h(px(TITLEBAR_HEIGHT))
        .flex_none()
        .pl(px(89.))
        .window_control_area(gpui::WindowControlArea::Drag)
        .child(IconButton::new("toggle-sidebar", LinearIcon::SidebarToggle))
        .child(div().w(px(23.)))
        .child(IconButton::new("history", LinearIcon::History))
        .child(div().w(px(4.)))
        .child(IconButton::new("back", LinearIcon::ArrowLeft))
        .child(div().w(px(4.)))
        .child(IconButton::new("forward", LinearIcon::ArrowRight))
}

/// Fallback mark: Linear shows the uploaded workspace logo (a 20px rounded square). We don't have
/// access to that private upload, so we draw the habric wordmark plate instead.
pub fn workspace_logo(name: &SharedString) -> impl IntoElement + use<> {
    div()
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(px(20.))
        .rounded(px(8.))
        .bg(gpui::black())
        .text_color(gpui::white())
        .text_size(px(5.))
        .font_weight(WEIGHT_WORKSPACE)
        .child(name.clone())
}

impl Render for Sidebar {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let fixture = self.fixture.clone();
        let theme = cx.theme();
        let bg = theme.bg_app;

        let top = div()
            .pb(px(8.))
            .child(self.nav_item("inbox", "Inbox", Leading::Icon(LinearIcon::Inbox), 0., None, cx))
            .child(self.nav_item("my-issues", "My issues", Leading::Icon(LinearIcon::MyIssues), 0., None, cx))
            .child(self.nav_item("pulse", "Pulse", Leading::Icon(LinearIcon::Pulse), 0., None, cx));

        let workspace = div()
            .child(self.section_header(Section::Workspace, "Workspace", None, cx))
            .when(self.is_open(&Section::Workspace), |this| {
                this.pb(px(8.))
                    .child(self.nav_item("initiatives", "Initiatives", Leading::Icon(LinearIcon::Initiatives), 0., None, cx))
                    .child(self.nav_item("projects", "Projects", Leading::Icon(LinearIcon::Projects), 0., None, cx))
                    .child(self.nav_item("views", "Views", Leading::Icon(LinearIcon::Views), 0., None, cx))
                    .child(self.nav_item("customers", "Customers", Leading::Icon(LinearIcon::Customers), 0., None, cx))
                    .child(self.nav_item("more", "More", Leading::Icon(LinearIcon::More), 0., None, cx))
            });

        let favorites = div()
            .child(self.section_header(Section::Favorites, "Favorites", Some((LinearIcon::FolderAdd, "fav-folder")), cx))
            .when(self.is_open(&Section::Favorites), |this| {
                this.pb(px(8.)).children(fixture.favorites.iter().map(|f| self.favorite(f, 0., cx)))
            });

        let teams = div()
            .child(self.section_header(Section::Teams, "Your teams", Some((LinearIcon::Plus, "join-team")), cx))
            .when(self.is_open(&Section::Teams), |this| {
                this.pb(px(8.)).children((0..fixture.teams.len()).map(|ix| self.team(ix, cx)))
            });

        let try_section = div()
            .child(self.section_header(Section::Try, "Try", None, cx))
            .when(self.is_open(&Section::Try), |this| {
                this.child(self.nav_item("cycles", "Cycles", Leading::Icon(LinearIcon::Cycles), 0., None, cx))
            });

        div()
            .flex()
            .flex_col()
            .flex_none()
            .w(px(SIDEBAR_WIDTH))
            .h_full()
            .bg(bg)
            .child(titlebar())
            .child(self.workspace_row(cx))
            .child(
                div()
                    .id("sidebar-scroll")
                    .flex_1()
                    .min_h_0()
                    .overflow_y_scroll()
                    .px(px(12.))
                    .pt(px(8.))
                    .child(top)
                    .child(workspace)
                    .child(favorites)
                    .child(teams)
                    .child(try_section),
            )
            .child(self.footer(cx))
    }
}

impl Sidebar {
    fn footer(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let avatar = self.fixture.user.avatar.clone();
        div()
            .flex()
            .flex_none()
            .items_center()
            .gap(px(6.))
            .h(px(36.))
            .px(px(8.))
            .child(
                div()
                    .id("help")
                    .flex()
                    .items_center()
                    .justify_center()
                    .size(px(24.))
                    .rounded_full()
                    .bg(theme.bg_raised)
                    .shadow(theme.raised_shadow(false))
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_muted)
                    .child("?"),
            )
            .child(
                div()
                    .id("plan")
                    .flex()
                    .items_center()
                    .gap(px(6.))
                    .h(px(24.))
                    .pl(px(4.))
                    .pr(px(10.))
                    .rounded_full()
                    .bg(theme.bg_raised)
                    .shadow(theme.raised_shadow(false))
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_secondary)
                    .child(img(avatar).size(px(16.)).rounded_full())
                    .child("Free plan"),
            )
    }
}
