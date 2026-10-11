//! Root view: window chrome + sidebar + the floating content panel.

use std::rc::Rc;

use gpui::{Context, Entity, IntoElement, Render, Window, div, prelude::*, px};

use crate::issue_list::IssueList;
use crate::model::Fixture;
use crate::sidebar::{Sidebar, TITLEBAR_HEIGHT};
use ui::theme::{ActiveTheme, typography::*};
use ui::StyledExt;

/// Height of the bottom strip under the panel (`--agent-toolbar-height`).
const BOTTOM_BAR: f32 = 28.;

pub struct Workspace {
    sidebar: Entity<Sidebar>,
    issue_list: Entity<IssueList>,
}

impl Workspace {
    pub fn new(fixture: Rc<Fixture>, cx: &mut Context<Self>) -> Self {
        let sidebar = cx.new(|_| Sidebar::new(fixture.clone()));
        let issue_list = cx.new(|cx| IssueList::new(fixture, cx));
        Self { sidebar, issue_list }
    }
}

impl Render for Workspace {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();
        div()
            .flex()
            .size_full()
            .bg(theme.bg_app)
            // the recreation keeps Linear's face; the mail UI uses the theme's (Geist)
            .font_family("Inter")
            .text_color(theme.text_primary)
            .text_size(px(SIZE_SMALL))
            .child(self.sidebar.clone())
            .child(
                div()
                    .flex()
                    .flex_col()
                    .flex_1()
                    .min_w_0()
                    .child(div().h(px(TITLEBAR_HEIGHT)).flex_none().window_control_area(gpui::WindowControlArea::Drag))
                    .child(
                        // <main>: 12px radius card, 0.5px border, soft shadow, `margin: 0 8px 8px 0`
                        div()
                            .flex_1()
                            .min_h_0()
                            .mr(px(8.))
                            .mb(px(8. + BOTTOM_BAR - 36.).max(px(8.)))
                            .rounded(theme.radius.xl)
                            .bg(theme.bg_panel)
                            .border_hairline()
                            .border_color(theme.border_panel)
                            .shadow(theme.panel_shadow())
                            .overflow_hidden()
                            .child(self.issue_list.clone()),
                    ),
            )
    }
}
