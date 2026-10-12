//! Day and week view: a time grid with an all-day row on top. Events sit in their day column at
//! their time; events that overlap share the column side by side.

use std::cell::RefCell;
use std::collections::HashMap;
use std::rc::Rc;

use chrono::{Datelike, Duration, NaiveDate, NaiveDateTime, NaiveTime, Timelike};
use gpui::{
    AnyElement, Bounds, Div, Entity, Hsla, IntoElement, MouseButton, Pixels, ScrollHandle, SharedString, Stateful, WeakEntity, canvas, div,
    prelude::*, px, relative,
};
use ui::StyledExt;
use ui::theme::{Theme, typography::*};

use crate::model::{Event, Rsvp, Schedule};

pub const HOUR: f32 = 48.;
/// drag-to-create snaps to quarter hours
const SNAP: i64 = 15;

/// A slot being dragged out on one day, in minutes from midnight.
#[derive(Clone, Copy, Debug)]
pub struct Drag {
    /// where the mouse went down (day + minute of day)
    pub anchor_day: NaiveDate,
    pub anchor: i64,
    /// where it is now – may be another day
    pub day: NaiveDate,
    pub current: i64,
    /// dragged in the all-day row: whole days
    pub all_day: bool,
}

impl Drag {
    /// [start, end) – whichever way the mouse went: at least one snap long in the grid, whole
    /// days in the all-day row.
    pub fn range(&self) -> (NaiveDateTime, NaiveDateTime) {
        let at = |day: NaiveDate, minute: i64| day.and_time(NaiveTime::MIN) + Duration::minutes(minute);
        if self.all_day {
            let (first, last) = (self.anchor_day.min(self.day), self.anchor_day.max(self.day));
            return (at(first, 0), at(last + Duration::days(1), 0));
        }
        let (a, b) = (at(self.anchor_day, self.anchor), at(self.day, self.current));
        (a.min(b), a.max(b) + Duration::minutes(SNAP))
    }

    pub fn moved(&self) -> bool {
        self.anchor_day != self.day || self.anchor != self.current
    }
}

/// Where each day column was painted (window coordinates, scroll included), so a mouse
/// position can be turned into a time.
pub type ColumnBounds = Rc<RefCell<HashMap<NaiveDate, Bounds<Pixels>>>>;

/// The day column under x (clamped to the first / last column).
pub fn day_at(x: Pixels, columns: &HashMap<NaiveDate, Bounds<Pixels>>) -> Option<NaiveDate> {
    let mut sorted: Vec<(&NaiveDate, &Bounds<Pixels>)> = columns.iter().collect();
    sorted.sort_by_key(|(d, _)| **d);
    sorted
        .iter()
        .find(|(_, b)| x >= b.left() && x < b.right())
        .or_else(|| if x < sorted.first()?.1.left() { sorted.first() } else { sorted.last() })
        .map(|(d, _)| **d)
}

pub fn minute_at(y: Pixels, column: Bounds<Pixels>) -> i64 {
    let minutes = ((y - column.top()) / px(HOUR) * 60.) as i64;
    (minutes.div_euclid(SNAP) * SNAP).clamp(0, 24 * 60 - SNAP)
}

/// What the grid needs from its view for dragging out new events.
pub struct Interaction {
    pub view: WeakEntity<crate::CalendarView>,
    pub drag: Option<Drag>,
    pub columns: ColumnBounds,
}
const GUTTER: f32 = 56.;
const ALL_DAY_ROW: f32 = 24.;

/// Calendar color → the tinted block colors (fill, text, accent bar).
pub fn tint(color: Hsla, theme: &Theme) -> (Hsla, Hsla) {
    let fill = color.opacity(if theme.is_dark() { 0.26 } else { 0.16 });
    (fill, theme.text_primary)
}

pub fn time(t: chrono::NaiveDateTime) -> String {
    format!("{:02}:{:02}", t.hour(), t.minute())
}

/// Columns for overlapping timed events of one day: (event, column, columns in its cluster).
fn layout_day<'a>(events: &[&'a Event]) -> Vec<(&'a Event, usize, usize)> {
    let mut out = Vec::new();
    let mut cluster: Vec<(&Event, usize)> = Vec::new();
    let mut column_ends: Vec<chrono::NaiveDateTime> = Vec::new();
    let mut cluster_end = chrono::NaiveDateTime::MIN;
    let flush = |cluster: &mut Vec<(&'a Event, usize)>, columns: usize, out: &mut Vec<_>| {
        out.extend(cluster.drain(..).map(|(e, c)| (e, c, columns)));
    };
    for &event in events {
        if event.start >= cluster_end && !cluster.is_empty() {
            flush(&mut cluster, column_ends.len(), &mut out);
            column_ends.clear();
        }
        let column = match column_ends.iter().position(|end| *end <= event.start) {
            Some(c) => {
                column_ends[c] = event.end;
                c
            }
            None => {
                column_ends.push(event.end);
                column_ends.len() - 1
            }
        };
        cluster.push((event, column));
        cluster_end = cluster_end.max(event.end);
    }
    flush(&mut cluster, column_ends.len(), &mut out);
    out
}

/// Header row: weekday + date per column, today's date on an accent pill.
pub fn day_headers(days: &[NaiveDate], today: NaiveDate, theme: &Theme) -> Div {
    div().flex().flex_none().h(px(52.)).border_b_hairline().border_color(theme.border_panel).child(div().w(px(GUTTER)).flex_none()).children(
        days.iter().map(|day| {
            let is_today = *day == today;
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .items_center()
                .justify_center()
                .gap(px(6.))
                .child(div().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(day.format("%a").to_string()))
                .child(
                    div()
                        .flex()
                        .items_center()
                        .justify_center()
                        .min_w(px(26.))
                        .h(px(26.))
                        .px(px(5.))
                        .rounded(theme.radius.pill)
                        .text_size(px(SIZE_REGULAR))
                        .font_weight(WEIGHT_MEDIUM)
                        .map(|s| if is_today { s.bg(theme.accent).text_color(gpui::white()) } else { s.text_color(theme.text_secondary) })
                        .child(day.day().to_string()),
                )
        }),
    )
}

/// A block in the grid or a chip in the all-day row / month view.
pub fn event_block(event: &Event, color: Hsla, selected: bool, compact: bool, theme: &Theme) -> Stateful<Div> {
    let (fill, fg) = tint(color, theme);
    let pending = event.rsvp == Rsvp::NeedsAction;
    let declined = event.rsvp == Rsvp::Declined;
    div()
        .id(SharedString::from(format!("event-{}", event.id)))
        .size_full()
        .flex()
        .overflow_hidden()
        .rounded(theme.radius.sm)
        // invitations without an answer are outlined, like a placeholder until you reply
        .map(|s| match (selected, pending) {
            (true, _) => s.bg(color),
            (false, true) => s.bg(theme.bg_panel).border_1().border_color(color),
            (false, false) => s.bg(fill).hover(|s| s.bg(color.opacity(if theme.is_dark() { 0.36 } else { 0.24 }))),
        })
        .when(declined, |s| s.opacity(0.5))
        .child(div().w(px(3.)).h_full().flex_none().when(!selected, |s| s.bg(color)))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .px(px(6.))
                .py(px(if compact { 2. } else { 4. }))
                .flex()
                .map(|s| if compact { s.flex_row().items_center().gap(px(6.)) } else { s.flex_col() })
                .text_size(px(SIZE_MICRO))
                .line_height(px(15.))
                .text_color(if selected { gpui::white() } else { fg })
                .child(
                    div()
                        .min_w_0()
                        .flex_none()
                        .truncate()
                        .font_weight(WEIGHT_MEDIUM)
                        .when(declined, |s| s.line_through())
                        .child(event.title.clone()),
                )
                .when(!event.all_day, |s| {
                    s.child(
                        // one line, cut like the title: narrow overlapping blocks keep their title
                        div()
                            .flex_none()
                            .min_w_0()
                            .truncate()
                            .opacity(if selected { 0.85 } else { 0.7 })
                            .child(if compact { time(event.start) } else { format!("{} – {}", time(event.start), time(event.end)) }),
                    )
                }),
        )
}

pub fn render(schedule: &Entity<Schedule>, scroll: &ScrollHandle, now: chrono::NaiveDateTime, interaction: Interaction, theme: &Theme, cx: &gpui::App) -> AnyElement {
    let s = schedule.read(cx);
    let days = s.days();
    let selected = s.selected.clone();
    let color_of = |e: &Event| s.calendar(&e.calendar).map(|c| c.color).unwrap_or(theme.accent);

    let all_day: Vec<Vec<&Event>> = days.iter().map(|d| s.events_on(*d).filter(|e| e.all_day).collect()).collect();
    let all_day_rows = all_day.iter().map(Vec::len).max().unwrap_or(0);

    let menu = |target: crate::MenuTarget| {
        let view = interaction.view.clone();
        move |event: &gpui::MouseDownEvent, _: &mut gpui::Window, cx: &mut gpui::App| {
            let _ = view.update(cx, |v, cx| v.request_menu(target.clone(), event.position, cx));
            cx.stop_propagation();
        }
    };
    let click = |id: Option<SharedString>| {
        let schedule = schedule.clone();
        move |_: &gpui::MouseDownEvent, _: &mut gpui::Window, cx: &mut gpui::App| {
            schedule.update(cx, |s, cx| s.select(id.clone(), cx));
            cx.stop_propagation();
        }
    };

    // always there: it's where whole days are dragged out
    let _ = all_day_rows;
    let all_day_range = interaction.drag.filter(|d| d.all_day).map(|d| d.range());
    let all_day_row = Some({
        div()
            .flex()
            .flex_none()
            .min_h(px(ALL_DAY_ROW + 8.))
            .py(px(4.))
            .border_b_hairline()
            .border_color(theme.border_panel)
            .child(
                div()
                    .w(px(GUTTER))
                    .flex_none()
                    .pr(px(8.))
                    .flex()
                    .justify_end()
                    .text_size(px(SIZE_MICRO))
                    .text_color(theme.text_quaternary)
                    .child("all-day"),
            )
            .children(all_day.iter().zip(&days).map(|(events, day)| {
                let in_drag = all_day_range.is_some_and(|(a, b)| day.and_time(NaiveTime::MIN) >= a && day.and_time(NaiveTime::MIN) < b);
                div()
                    .flex_1()
                    .min_w_0()
                    .px(px(2.))
                    .flex()
                    .flex_col()
                    .gap(px(2.))
                    .on_mouse_down(MouseButton::Left, {
                        let (schedule, view, day) = (schedule.clone(), interaction.view.clone(), *day);
                        move |event: &gpui::MouseDownEvent, _, cx: &mut gpui::App| {
                            schedule.update(cx, |s, cx| s.select(None, cx));
                            let _ = view.update(cx, |v, cx| v.start_drag(day, event.position.y, true, cx));
                        }
                    })
                    .on_mouse_down(MouseButton::Right, menu(crate::MenuTarget::Day(*day)))
                    .children(events.iter().map(|e| {
                        div()
                            .h(px(ALL_DAY_ROW - 2.))
                            .child(
                                event_block(e, color_of(e), selected.as_ref() == Some(&e.id), true, theme)
                                    .on_mouse_down(MouseButton::Left, click(Some(e.id.clone())))
                                    .on_mouse_down(MouseButton::Right, menu(crate::MenuTarget::Event(e.id.clone()))),
                            )
                    }))
                    .when(in_drag, |c| c.child(div().h(px(ALL_DAY_ROW - 2.)).rounded(theme.radius.sm).bg(theme.accent.opacity(0.18)).border_1().border_color(theme.accent)))
            }))
    });

    let this_week = days.contains(&s.today);
    let columns = days.iter().map(|day| {
        let timed: Vec<&Event> = s.events_on(*day).filter(|e| !e.all_day).collect();
        let day_start = day.and_time(NaiveTime::MIN);
        let weekend = day.weekday().num_days_from_monday() >= 5;
        let is_today = *day == s.today;
        div()
            .flex_1()
            .min_w_0()
            .relative()
            .h_full()
            .border_l_1()
            .border_color(theme.border_panel)
            .when(weekend, |c| c.bg(theme.bg_group.opacity(0.5)))
            // empty space: clear the selection and start dragging out a new event
            .on_mouse_down(MouseButton::Left, {
                let (schedule, view, day) = (schedule.clone(), interaction.view.clone(), *day);
                move |event: &gpui::MouseDownEvent, _, cx: &mut gpui::App| {
                    schedule.update(cx, |s, cx| s.select(None, cx));
                    let _ = view.update(cx, |v, cx| v.start_drag(day, event.position.y, false, cx));
                }
            })
            .on_mouse_down(MouseButton::Right, {
                let (view, day) = (interaction.view.clone(), *day);
                move |event: &gpui::MouseDownEvent, _, cx: &mut gpui::App| {
                    let _ = view.update(cx, |v, cx| v.request_slot_menu(day, event.position, cx));
                }
            })
            .child({
                let (columns, day) = (interaction.columns.clone(), *day);
                canvas(move |bounds, _, _| {
                    columns.borrow_mut().insert(day, bounds);
                }, |_, _, _, _| {})
                .absolute()
                .size_full()
            })
            // the slot being dragged out
            .children(interaction.drag.filter(|d| !d.all_day).and_then(|d| {
                let (start, end) = d.range();
                let (day_start, day_end) = (day.and_time(NaiveTime::MIN), *day + Duration::days(1));
                let day_end = day_end.and_time(NaiveTime::MIN);
                if start >= day_end || end <= day_start {
                    return None;
                }
                // this day's piece of the slot; the label sits on the first piece
                let from = (start.max(day_start) - day_start).num_minutes();
                let to = (end.min(day_end) - day_start).num_minutes();
                let label = (start >= day_start).then(|| {
                    if end <= day_end { format!("{} – {}", time(start), time(end)) } else { format!("{} – {} {}", time(start), end.format("%a"), time(end)) }
                });
                Some((from, to, label))
            }).map(|(from, to, label)| {
                div()
                    .absolute()
                    .left(px(2.))
                    .right(px(2.))
                    .top(px(from as f32 / 60. * HOUR + 1.))
                    .h(px((to - from) as f32 / 60. * HOUR - 2.))
                    .rounded(theme.radius.sm)
                    .bg(theme.accent.opacity(0.18))
                    .border_1()
                    .border_color(theme.accent)
                    .px(px(6.))
                    .py(px(3.))
                    .text_size(px(SIZE_MICRO))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_primary)
                    .children(label)
            }))
            .children((0..24).map(|h| div().absolute().left_0().right_0().top(px(h as f32 * HOUR)).h(px(HOUR)).border_b_hairline().border_color(theme.border_panel)))
            .children(layout_day(&timed).into_iter().map(|(e, column, columns)| {
                // clip multi-day events to this day
                let from = (e.start.max(day_start) - day_start).num_minutes() as f32;
                let to = (e.end.min(day_start + Duration::days(1)) - day_start).num_minutes() as f32;
                let height = ((to - from) / 60. * HOUR).max(18.);
                let compact = height < 36.;
                div()
                    .absolute()
                    .top(px(from / 60. * HOUR + 1.))
                    .h(px(height - 2.))
                    .left(relative(column as f32 / columns as f32))
                    .w(relative(1. / columns as f32))
                    .px(px(2.))
                    .child(
                        event_block(e, color_of(e), selected.as_ref() == Some(&e.id), compact, theme)
                            .on_mouse_down(MouseButton::Left, click(Some(e.id.clone())))
                            .on_mouse_down(MouseButton::Right, menu(crate::MenuTarget::Event(e.id.clone()))),
                    )
            }))
            // the current time: a faint line through the whole week, a strong one with a dot in
            // today's column (the dot sits inside the column so no neighbour can clip it)
            .when(this_week, |c| {
                let y = (now.hour() * 60 + now.minute()) as f32 / 60. * HOUR;
                let red = ui::theme::lch(55., 70., 30.);
                if is_today {
                    c.child(div().absolute().left(px(4.)).right_0().top(px(y - 1.)).h(px(2.)).bg(red))
                        .child(div().absolute().left_0().top(px(y - 4.)).size(px(8.)).rounded_full().bg(red))
                } else {
                    c.child(div().absolute().left_0().right_0().top(px(y - 0.5)).h(px(1.)).bg(red.opacity(0.35)))
                }
            })
    });

    let gutter = div().w(px(GUTTER)).flex_none().relative().h_full().children((1..24).map(|h| {
        div()
            .absolute()
            .right(px(8.))
            .top(px(h as f32 * HOUR - 7.))
            .text_size(px(SIZE_MICRO))
            .text_color(theme.text_quaternary)
            .child(format!("{h:02}:00"))
    }));

    div()
        .flex()
        .flex_col()
        .size_full()
        // moves and the release anywhere over the grid (all-day row included) carry the drag
        .on_mouse_move({
            let view = interaction.view.clone();
            move |event: &gpui::MouseMoveEvent, _, cx: &mut gpui::App| {
                let _ = view.update(cx, |v, cx| v.update_drag(event.position, cx));
            }
        })
        .on_mouse_up(MouseButton::Left, {
            let view = interaction.view.clone();
            move |_, _, cx: &mut gpui::App| {
                let _ = view.update(cx, |v, cx| v.finish_drag(cx));
            }
        })
        .on_mouse_up_out(MouseButton::Left, {
            let view = interaction.view.clone();
            move |_, _, cx: &mut gpui::App| {
                let _ = view.update(cx, |v, cx| v.finish_drag(cx));
            }
        })
        .child(day_headers(&days, s.today, theme))
        .children(all_day_row)
        .child(
            div()
                .id("grid-scroll")
                .flex_1()
                .min_h_0()
                .overflow_y_scroll()
                .track_scroll(scroll)
                .child(div().flex().h(px(24. * HOUR)).child(gutter).children(columns)),
        )
        .into_any_element()
}
