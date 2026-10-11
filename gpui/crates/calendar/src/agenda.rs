//! Agenda: the next two weeks as a list, grouped by day – rows and group headers in the style of
//! the mail list layout, so both lists read the same.

use chrono::Duration;
use gpui::{AnyElement, Entity, IntoElement, MouseButton, SharedString, div, prelude::*, px};
use ui::theme::{Theme, typography::*};
use ui::{EmptyState, IconName};

use crate::detail::rsvp_label;
use crate::grid::time;
use crate::model::{Rsvp, Schedule};

pub fn render(schedule: &Entity<Schedule>, view: gpui::WeakEntity<crate::CalendarView>, theme: &Theme, cx: &gpui::App) -> AnyElement {
    let s = schedule.read(cx);
    let selected = s.selected.clone();
    let groups: Vec<_> = s.days().into_iter().map(|day| (day, s.events_on(day).collect::<Vec<_>>())).filter(|(_, events)| !events.is_empty()).collect();
    if groups.is_empty() {
        return EmptyState::new(IconName::Calendar, "Nothing scheduled").detail("The next two weeks are free").into_any_element();
    }

    let label = |day: chrono::NaiveDate| {
        let date = day.format("%a, %b %-d").to_string();
        match (day - s.today).num_days() {
            0 => format!("Today · {date}"),
            1 => format!("Tomorrow · {date}"),
            _ => date,
        }
    };

    div()
        .id("agenda")
        .size_full()
        .overflow_y_scroll()
        .py(px(4.))
        .children(groups.into_iter().map(|(day, events)| {
            div()
                .flex()
                .flex_col()
                .child(
                    div()
                        .flex()
                        .items_center()
                        .h(px(36.))
                        .mx(px(8.))
                        .mt(px(4.))
                        .px(px(12.))
                        .gap(px(8.))
                        .rounded(theme.radius.md)
                        .bg(theme.bg_group)
                        .text_size(px(SIZE_SMALL))
                        .child(div().font_weight(WEIGHT_MEDIUM).text_color(theme.text_heading).child(label(day)))
                        .child(div().text_color(theme.text_muted).child(events.len().to_string())),
                )
                .children(events.into_iter().map(|e| {
                    let color = s.calendar(&e.calendar).map(|c| c.color).unwrap_or(theme.accent);
                    let is_selected = selected.as_ref() == Some(&e.id);
                    let span = if e.all_day {
                        "all day".to_string()
                    } else if e.start.date() < day {
                        format!("until {}", time(e.end))
                    } else {
                        format!("{} – {}", time(e.start), time(e.end.min(day.and_hms_opt(23, 59, 0).unwrap_or(e.end) + Duration::minutes(1))))
                    };
                    let (schedule, id) = (schedule.clone(), e.id.clone());
                    div()
                        .id(SharedString::from(format!("agenda-{}-{day}", e.id)))
                        .flex()
                        .items_center()
                        .h(px(44.))
                        .mx(px(8.))
                        .px(px(12.))
                        .gap(px(12.))
                        .rounded(theme.radius.md)
                        .text_size(px(SIZE_SMALL))
                        .map(|r| if is_selected { r.bg(theme.bg_selected) } else { r.hover(|r| r.bg(theme.bg_group)) })
                        .when(e.rsvp == Rsvp::Declined, |r| r.opacity(0.5))
                        .child(div().w(px(3.)).h(px(20.)).rounded(theme.radius.xs).bg(color))
                        .child(div().w(px(110.)).flex_none().text_color(theme.text_tertiary).child(span))
                        .child(div().flex_1().min_w_0().truncate().font_weight(WEIGHT_MEDIUM).text_color(theme.text_primary).child(e.title.clone()))
                        .children(e.location.clone().map(|l| div().max_w(px(220.)).truncate().text_color(theme.text_tertiary).child(l)))
                        .when(e.rsvp == Rsvp::NeedsAction, |r| r.child(div().text_size(px(SIZE_MINI)).text_color(theme.accent).child(rsvp_label(e.rsvp))))
                        .on_mouse_down(MouseButton::Left, move |_, _, cx| schedule.update(cx, |s, cx| s.select(Some(id.clone()), cx)))
                        .on_mouse_down(MouseButton::Right, {
                            let (view, id) = (view.clone(), e.id.clone());
                            move |event: &gpui::MouseDownEvent, _, cx: &mut gpui::App| {
                                let _ = view.update(cx, |v, cx| v.request_menu(crate::MenuTarget::Event(id.clone()), event.position, cx));
                            }
                        })
                }))
        }))
        .into_any_element()
}
