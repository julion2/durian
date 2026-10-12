//! Month view: whole weeks, each day a cell with its first events as compact blocks and a
//! "+N more" line. Clicking a day's number opens that day.

use chrono::Datelike;
use gpui::{AnyElement, Entity, IntoElement, MouseButton, SharedString, div, prelude::*, px};
use ui::StyledExt;
use ui::theme::{Theme, typography::*};

use crate::grid::event_block;
use crate::model::{Schedule, View};

const SHOWN: usize = 3;

pub fn render(schedule: &Entity<Schedule>, view: gpui::WeakEntity<crate::CalendarView>, theme: &Theme, cx: &gpui::App) -> AnyElement {
    let menu = |target: crate::MenuTarget| {
        let view = view.clone();
        move |event: &gpui::MouseDownEvent, _: &mut gpui::Window, cx: &mut gpui::App| {
            let _ = view.update(cx, |v, cx| v.request_menu(target.clone(), event.position, cx));
            cx.stop_propagation();
        }
    };
    let s = schedule.read(cx);
    let days = s.days();
    let month = s.anchor.month();
    let selected = s.selected.clone();
    let weekday_names = days.iter().take(7).map(|d| {
        div().flex_1().px(px(10.)).py(px(8.)).text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(d.format("%a").to_string())
    });

    let weeks = days.chunks(7).map(|week| {
        div().flex().flex_1().min_h_0().border_b_hairline().border_color(theme.border_panel).children(week.iter().map(|day| {
            let events: Vec<_> = s.events_on(*day).collect();
            let other_month = day.month() != month;
            let is_today = *day == s.today;
            let (open, date) = (schedule.clone(), *day);
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .gap(px(2.))
                .p(px(4.))
                .border_l_1()
                .border_color(theme.border_panel)
                .when(day.weekday().num_days_from_monday() >= 5, |c| c.bg(theme.bg_group.opacity(0.5)))
                .on_mouse_down(MouseButton::Right, menu(crate::MenuTarget::Day(*day)))
                .child(
                    div()
                        .id(SharedString::from(format!("month-day-{date}")))
                        .flex()
                        .items_center()
                        .justify_center()
                        .min_w(px(22.))
                        .h(px(22.))
                        .px(px(4.))
                        .self_start()
                        .rounded(theme.radius.pill)
                        .text_size(px(SIZE_MINI))
                        .font_weight(WEIGHT_MEDIUM)
                        .map(|n| match (is_today, other_month) {
                            (true, _) => n.bg(theme.accent).text_color(gpui::white()),
                            (false, true) => n.text_color(theme.text_quaternary).hover(|n| n.bg(theme.bg_nav_hover)),
                            (false, false) => n.text_color(theme.text_secondary).hover(|n| n.bg(theme.bg_nav_hover)),
                        })
                        .child(day.day().to_string())
                        .on_click(move |_, _, cx| {
                            open.update(cx, |s, cx| {
                                s.anchor = date;
                                s.set_view(View::Day, cx);
                            })
                        }),
                )
                .children(events.iter().take(SHOWN).map(|e| {
                    let color = s.calendar(&e.calendar).map(|c| c.color).unwrap_or(theme.accent);
                    let (schedule, id) = (schedule.clone(), e.id.clone());
                    div().h(px(20.)).flex_none().child(
                        event_block(e, color, selected.as_ref() == Some(&e.id), true, theme)
                            .on_mouse_down(MouseButton::Left, move |_, _, cx| {
                                schedule.update(cx, |s, cx| s.select(Some(id.clone()), cx));
                                cx.stop_propagation();
                            })
                            .on_mouse_down(MouseButton::Right, menu(crate::MenuTarget::Event(e.id.clone()))),
                    )
                }))
                .when(events.len() > SHOWN, |c| {
                    c.child(div().px(px(6.)).text_size(px(SIZE_MICRO)).text_color(theme.text_tertiary).child(format!("+{} more", events.len() - SHOWN)))
                })
        }))
    });

    div()
        .flex()
        .flex_col()
        .size_full()
        .child(div().flex().flex_none().border_b_hairline().border_color(theme.border_panel).children(weekday_names))
        .children(weeks)
        .into_any_element()
}
