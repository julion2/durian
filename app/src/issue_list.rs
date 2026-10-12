//! "My issues" content panel: header (title + view tabs + toolbar) and the grouped, virtualized
//! issue list. Row geometry from `design/raw/issueRows.txt`:
//!
//! ```text
//! | 16 | ☐ 18 | 8 | prio 16 | 8 | id 61 | 8 | status 16 | 8 | title … chips avatar | 8 | created 60 | 8 | 18 |
//! ```

use std::collections::HashSet;
use std::f32::consts::FRAC_PI_2;
use std::rc::Rc;
use std::sync::Arc;

use gpui::{
    AnyElement, Context, Entity, FontFeatures, Hsla, IntoElement, ListAlignment, ListState, Render,
    SharedString, Window, div, img, list, prelude::*, px,
};

use crate::model::{DueUrgency, Fixture, GroupKind, Issue, Priority, group_issues};
use ui::theme::{ActiveTheme, Theme, lch, typography::*};
use crate::status_icon::{ProgressPie, StatusIcon};
use crate::linear_icons::{self, LinearIcon};
use ui::{ButtonStyle, Chip, Icon, IconButton, StyledExt, TabPill, color_dot};

const ROW_HEIGHT: f32 = 44.;
const GROUP_HEIGHT: f32 = 36.;
const IDENTIFIER_WIDTH: f32 = 61.;
const CREATED_WIDTH: f32 = 60.;

const TABS: [&str; 4] = ["Assigned", "Created", "Subscribed", "Activity"];

/// One line in the virtualized list.
#[derive(Clone, Copy)]
enum Row {
    Group { kind: GroupKind, count: usize },
    Issue(usize),
}

pub struct IssueList {
    fixture: Rc<Fixture>,
    rows: Vec<Row>,
    collapsed: HashSet<GroupKind>,
    list_state: ListState,
    active_tab: usize,
}

impl IssueList {
    pub fn new(fixture: Rc<Fixture>, _cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            fixture,
            rows: Vec::new(),
            collapsed: HashSet::default(),
            list_state: ListState::new(0, ListAlignment::Top, px(400.)),
            active_tab: 0,
        };
        this.rebuild_rows();
        this
    }

    fn rebuild_rows(&mut self) {
        self.rows.clear();
        for group in group_issues(&self.fixture.issues) {
            self.rows.push(Row::Group { kind: group.kind, count: group.issues.len() });
            if !self.collapsed.contains(&group.kind) {
                self.rows.extend(group.issues.into_iter().map(Row::Issue));
            }
        }
        self.list_state.reset(self.rows.len());
    }

    fn toggle_group(&mut self, kind: GroupKind, cx: &mut Context<Self>) {
        if !self.collapsed.remove(&kind) {
            self.collapsed.insert(kind);
        }
        self.rebuild_rows();
        cx.notify();
    }

    fn render_header(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme();
        let tabs = TABS.iter().enumerate().map(|(ix, label)| {
            TabPill::new(("tab", ix), *label, ix == self.active_tab).on_click(cx.listener(move |this, _, _, cx| {
                this.active_tab = ix;
                cx.notify();
            }))
        });

        div()
            .flex()
            .flex_col()
            .flex_none()
            .child(
                div()
                    .flex()
                    .items_center()
                    .h(px(44.))
                    .px(px(8.))
                    .border_b_hairline()
                    .border_color(theme.border_panel)
                    .child(
                        div()
                            .pl(px(10.))
                            .pr(px(4.))
                            .text_size(px(SIZE_SMALL))
                            .font_weight(WEIGHT_MEDIUM)
                            .text_color(theme.text_secondary)
                            .child("My issues"),
                    ),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .h(px(44.))
                    .px(px(8.))
                    .gap(px(6.))
                    .child(div().flex().flex_1().items_center().gap(px(8.)).children(tabs))
                    .child(
                        div()
                            .flex()
                            .gap(px(6.))
                            .child(IconButton::new("filter", LinearIcon::Filter).style(ButtonStyle::Raised))
                            .child(IconButton::new("display", LinearIcon::Display).style(ButtonStyle::Raised))
                            .child(IconButton::new("details", LinearIcon::PanelRight).style(ButtonStyle::Raised)),
                    ),
            )
    }

    fn render_row(&self, ix: usize, cx: &mut Context<Self>) -> AnyElement {
        match self.rows[ix] {
            Row::Group { kind, count } => self.render_group(kind, count, cx),
            Row::Issue(issue_ix) => render_issue(&self.fixture, &self.fixture.issues[issue_ix], cx.theme()),
        }
    }

    fn render_group(&self, kind: GroupKind, count: usize, cx: &mut Context<Self>) -> AnyElement {
        let theme = cx.theme();
        let collapsed = self.collapsed.contains(&kind);
        let icon = group_icon(kind, theme);

        div()
            .w_full()
            .pb(px(2.))
            .child(
                div()
                    .id(("group", kind as usize))
                    .flex()
                    .items_center()
                    .h(px(GROUP_HEIGHT))
                    .mx(px(8.))
                    .pl(px(4.))
                    .pr(px(8.))
                    .rounded(theme.radius.md)
                    .bg(theme.bg_group)
                    .child(
                        div().flex().items_center().justify_center().w(px(26.)).h(px(28.)).child(
                            div()
                                .flex()
                                .items_center()
                                .justify_center()
                                .size(px(20.))
                                .rounded_full()
                                .child(
                                    Icon::new(LinearIcon::GroupCaret)
                                        .size(16.)
                                        .color(theme.icon_caret)
                                        .rotate(if collapsed { 0. } else { FRAC_PI_2 }),
                                ),
                        ),
                    )
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .ml(px(4.))
                            .gap(px(8.))
                            .children(icon)
                            .child(
                                div()
                                    .ml(px(2.))
                                    .text_size(px(SIZE_SMALL))
                                    .font_weight(WEIGHT_MEDIUM)
                                    .text_color(theme.text_heading)
                                    .child(kind.title()),
                            )
                            .child(
                                div()
                                    .text_size(px(SIZE_SMALL))
                                    .font_weight(WEIGHT_NORMAL)
                                    .text_color(theme.text_muted)
                                    .child(count.to_string()),
                            ),
                    )
                    .child(div().flex_1())
                    .when(kind.can_create(), |this| {
                        this.child(IconButton::new(("group-add", kind as usize), LinearIcon::Plus).size(24.))
                    })
                    .on_click(cx.listener(move |this, _, _, cx| this.toggle_group(kind, cx))),
            )
            .into_any_element()
    }
}

fn group_icon(kind: GroupKind, theme: &Theme) -> Option<Icon> {
    let (name, color) = match kind {
        GroupKind::Blocking => (LinearIcon::Blocking, lch(50., 80., 29.)),
        GroupKind::Backlog => (LinearIcon::GroupBacklog, theme.text_muted),
        GroupKind::Completed => (LinearIcon::GroupCompleted, theme.text_muted),
        GroupKind::Canceled => (LinearIcon::GroupCanceled, theme.text_muted),
        GroupKind::Urgent | GroupKind::OtherActive => return None,
    };
    Some(Icon::new(name).size(16.).color(color))
}

fn priority_icon(priority: Priority, theme: &Theme) -> Icon {
    let name = match priority {
        Priority::Urgent => LinearIcon::PriorityUrgent,
        Priority::High => LinearIcon::PriorityHigh,
        Priority::Medium => LinearIcon::PriorityMedium,
        Priority::Low => LinearIcon::PriorityLow,
        Priority::None => LinearIcon::PriorityNone,
    };
    let color = if priority == Priority::Urgent { lch(66., 80., 48.) } else { theme.text_tertiary };
    Icon::new(name).size(16.).color(color)
}

fn due_icon(urgency: DueUrgency, theme: &Theme) -> Icon {
    let (name, color): (LinearIcon, Hsla) = match urgency {
        DueUrgency::Overdue => (LinearIcon::DueOverdue, lch(58., 73., 29.)),
        DueUrgency::Today => (LinearIcon::Calendar, lch(58., 73., 29.)),
        DueUrgency::Soon => (LinearIcon::Calendar, lch(66., 80., 48.)),
        DueUrgency::Normal => (LinearIcon::Calendar, theme.text_tertiary),
    };
    Icon::new(name).color(color)
}

/// Property chips in Linear's order: relations, sub-issues, labels, project, due date.
fn issue_chips(fixture: &Fixture, issue: &Issue, theme: &Theme) -> Vec<AnyElement> {
    let id = &issue.identifier;
    let mut chips = Vec::new();
    let relation = lch(50., 80., 29.);
    if issue.blocked_by > 0 {
        chips.push(
            Chip::new(SharedString::from(format!("{id}-blocked")), issue.blocked_by.to_string())
                .leading(Icon::new(LinearIcon::Blocked).color(relation))
                .into_any_element(),
        );
    }
    if issue.blocking > 0 {
        chips.push(
            Chip::new(SharedString::from(format!("{id}-blocking")), issue.blocking.to_string())
                .leading(Icon::new(LinearIcon::Blocking).color(relation))
                .into_any_element(),
        );
    }
    if let Some(sub) = issue.sub_issues {
        chips.push(
            Chip::new(SharedString::from(format!("{id}-sub")), format!("{}/{}", sub.done, sub.total))
                .leading(ProgressPie::new(sub.done as f32 / sub.total.max(1) as f32, lch(48., 59.31, 288.43)))
                .into_any_element(),
        );
    }
    for label in &issue.labels {
        let color = fixture.label(label).map_or(theme.text_tertiary, |l| l.color);
        chips.push(
            Chip::new(SharedString::from(format!("{id}-label-{label}")), label.clone())
                .leading(color_dot(color))
                .into_any_element(),
        );
    }
    if let Some(project) = issue.project.as_ref().and_then(|p| fixture.project(p)) {
        chips.push(
            Chip::new(SharedString::from(format!("{id}-project")), project.name.clone())
                .max_width(290.)
                .leading(Icon::new(linear_icons::named(&project.icon)).color(project.color))
                .into_any_element(),
        );
    }
    if let Some(due) = &issue.due {
        chips.push(
            Chip::new(SharedString::from(format!("{id}-due")), due.label.clone())
                .leading(due_icon(due.urgency, theme))
                .into_any_element(),
        );
    }
    chips
}

fn render_issue(fixture: &Fixture, issue: &Issue, theme: &Theme) -> AnyElement {
    let tnum = FontFeatures(Arc::new(vec![("tnum".into(), 1), ("calt".into(), 1)]));
    let hover_bg = theme.bg_group;

    div()
        .id(issue.identifier.clone())
        .group("row")
        .relative()
        .w_full()
        .flex()
        .items_center()
        .h(px(ROW_HEIGHT))
        .pl(px(16.))
        .gap(px(8.))
        // hover highlight is inset 8px like Linear's `::before { inset: 0 8px }`
        .child(div().absolute().top_0().bottom_0().left(px(8.)).right(px(8.)).rounded(theme.radius.md).group_hover("row", |s| s.bg(hover_bg)))
        .child(div().w(px(18.)).flex_none()) // selection checkbox column, revealed on hover in Linear
        .child(div().flex().flex_none().w(px(16.)).child(priority_icon(issue.priority, theme)))
        .child(
            div()
                .w(px(IDENTIFIER_WIDTH))
                .flex_none()
                .text_size(px(SIZE_SMALL))
                .font_weight(WEIGHT_NORMAL)
                .text_color(theme.text_tertiary)
                .font_features(tnum)
                .whitespace_nowrap()
                .child(issue.identifier.clone()),
        )
        .child(div().flex().flex_none().w(px(16.)).px(px(1.)).child(StatusIcon::new(&issue.status)))
        .child(
            div()
                .flex()
                .flex_1()
                .min_w_0()
                .items_center()
                .gap(px(8.))
                .mr(px(-5.))
                .overflow_hidden()
                .child(
                    div()
                        .min_w_0()
                        .text_size(px(SIZE_SMALL))
                        .font_weight(WEIGHT_MEDIUM)
                        .text_color(theme.text_primary)
                        .truncate()
                        .child(issue.title.clone()),
                )
                .child(
                    div()
                        .flex()
                        .flex_grow(1.) // basis stays `auto` (= chip widths); flex_1 would zero it
                        .min_w(px(90.))
                        // Linear truncates the title before it hides any property chip
                        .flex_shrink_0()
                        .justify_end()
                        .items_center()
                        .gap(px(3.))
                        .overflow_hidden()
                        .children(issue_chips(fixture, issue, theme)),
                )
                .child(
                    div()
                        .flex_none()
                        .mr(px(3.))
                        .child(img(fixture.user.avatar.clone()).size(px(18.)).rounded_full()),
                ),
        )
        .child(
            div()
                .flex()
                .flex_none()
                .justify_end()
                .w(px(CREATED_WIDTH))
                .text_size(px(SIZE_MINI))
                .font_weight(WEIGHT_NORMAL)
                .text_color(theme.text_tertiary)
                .whitespace_nowrap()
                .child(issue.created.clone()),
        )
        .child(div().w(px(18.)).flex_none())
        .into_any_element()
}

impl Render for IssueList {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let this: Entity<Self> = cx.entity();
        div()
            .flex()
            .flex_col()
            .size_full()
            .child(self.render_header(cx))
            .child(
                list(self.list_state.clone(), move |ix, _window, cx| {
                    this.update(cx, |this, cx| this.render_row(ix, cx))
                })
                .flex_1()
                .min_h_0(),
            )
    }
}
