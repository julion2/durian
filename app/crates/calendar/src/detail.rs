//! The selected event, in the panel right of the grid: same building blocks as the thread view –
//! title, property chips, people with avatars, a Markdown description.

use gpui::{AnyElement, Entity, IntoElement, SharedString, div, prelude::*, px};
use ui::theme::{Theme, typography::*};
use ui::{Avatar, Icon, IconName, StyledExt, markdown};

use crate::grid::time;
use crate::model::{Event, Rsvp, Schedule};

pub const WIDTH: f32 = 340.;

fn chip(theme: &Theme) -> gpui::Div {
    div()
        .flex()
        .items_center()
        .gap(px(6.))
        .h(px(26.))
        .px(px(10.))
        .rounded(theme.radius.pill)
        .bg(theme.bg_raised)
        .shadow(theme.raised_shadow(false))
        .text_size(px(SIZE_MINI))
        .text_color(theme.text_secondary)
}

pub fn when(event: &Event) -> String {
    let day = event.start.format("%a, %b %-d");
    if event.all_day {
        let days = event.duration().num_days();
        return if days > 1 { format!("{day} – {} · all day", (event.end - chrono::Duration::days(1)).format("%a, %b %-d")) } else { format!("{day} · all day") };
    }
    let minutes = event.duration().num_minutes();
    let length = if minutes % 60 == 0 { format!("{}h", minutes / 60) } else if minutes > 60 { format!("{}h {}m", minutes / 60, minutes % 60) } else { format!("{minutes}m") };
    format!("{day} · {} – {} ({length})", time(event.start), time(event.end))
}

pub fn rsvp_label(rsvp: Rsvp) -> &'static str {
    match rsvp {
        Rsvp::Accepted => "Going",
        Rsvp::Tentative => "Maybe",
        Rsvp::Declined => "Not going",
        Rsvp::NeedsAction => "Invited",
    }
}

fn rsvp_mark(rsvp: Rsvp, theme: &Theme) -> AnyElement {
    let (glyph, color) = match rsvp {
        Rsvp::Accepted => ("✓", ui::theme::lch(60., 50., 145.)),
        Rsvp::Tentative => ("?", theme.text_tertiary),
        Rsvp::Declined => ("✕", ui::theme::lch(55., 60., 30.)),
        Rsvp::NeedsAction => ("·", theme.text_quaternary),
    };
    div().w(px(14.)).text_size(px(SIZE_MINI)).text_color(color).child(glyph).into_any_element()
}

/// Going / Maybe / No – one row everywhere an invitation can be answered (details panel, the
/// invitation card in mail). Local for now; the backend hears about it once intents exist.
pub fn rsvp_buttons(schedule: &Entity<Schedule>, event: &Event, theme: &Theme) -> gpui::Div {
    div().flex().gap(px(6.)).children([Rsvp::Accepted, Rsvp::Tentative, Rsvp::Declined].into_iter().map(|answer| {
        let active = event.rsvp == answer;
        let (schedule, id) = (schedule.clone(), event.id.clone());
        div()
            .id(SharedString::from(format!("rsvp-{}-{answer:?}", event.id)))
            .flex_1()
            .flex()
            .items_center()
            .justify_center()
            .h(px(28.))
            .rounded(theme.radius.pill)
            .text_size(px(SIZE_MINI))
            .font_weight(WEIGHT_MEDIUM)
            .map(|b| {
                if active {
                    b.bg(theme.primary).text_color(theme.on_primary)
                } else {
                    b.bg(theme.bg_raised).shadow(theme.raised_shadow(false)).text_color(theme.text_secondary).hover(|b| b.bg(theme.bg_raised_hover))
                }
            })
            .child(match answer {
                Rsvp::Accepted => "Going",
                Rsvp::Tentative => "Maybe",
                _ => "No",
            })
            .on_click(move |_, _, cx| schedule.update(cx, |s, cx| s.set_rsvp(&id, answer, cx)))
    }))
}

/// "3 going · 1 maybe · 2 waiting" – the guests' answers, without you.
fn answer_summary(schedule: &Schedule, event: &Event) -> Option<String> {
    let guests: Vec<&crate::model::Attendee> = event.attendees.iter().filter(|a| !schedule.me.iter().any(|m| m.eq_ignore_ascii_case(&a.email))).collect();
    if guests.is_empty() {
        return None;
    }
    let count = |r: Rsvp| guests.iter().filter(|a| a.rsvp == r).count();
    let parts: Vec<String> = [(Rsvp::Accepted, "going"), (Rsvp::Tentative, "maybe"), (Rsvp::Declined, "declined"), (Rsvp::NeedsAction, "waiting")]
        .into_iter()
        .filter_map(|(r, label)| (count(r) > 0).then(|| format!("{} {label}", count(r))))
        .collect();
    Some(parts.join(" · "))
}

/// Your own event: who's coming, and Edit / Cancel (Delete when there are no guests) – the
/// same actions as E and ⌫.
fn organizer_bar(schedule: &Schedule, event: &Event, theme: &Theme) -> gpui::Div {
    let summary = answer_summary(schedule, event);
    let button = |id: &'static str, label: &'static str, destructive: bool, action: fn() -> Box<dyn gpui::Action>| {
        div()
            .id(id)
            .flex()
            .items_center()
            .justify_center()
            .h(px(28.))
            .px(px(12.))
            .rounded(theme.radius.pill)
            .bg(theme.bg_raised)
            .shadow(theme.raised_shadow(false))
            .text_size(px(SIZE_MINI))
            .font_weight(WEIGHT_MEDIUM)
            .text_color(if destructive { ui::theme::lch(55., 70., 30.) } else { theme.text_secondary })
            .hover(|b| b.bg(theme.bg_raised_hover))
            .child(label)
            .on_click(move |_, window, cx| window.dispatch_action(action(), cx))
    };
    div()
        .flex()
        .items_center()
        .gap(px(8.))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                // without guests there's nobody to organize – just the buttons
                .when(summary.is_some(), |d| d.child(div().text_size(px(SIZE_MINI)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_secondary).child("You're organizing")))
                .children(summary.map(|t| div().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child(t))),
        )
        .child(button("organizer-edit", "Edit", false, || Box::new(crate::EditEvent)))
        .child(button("organizer-cancel", if event.attendees.len() > 1 { "Cancel event" } else { "Delete" }, true, || Box::new(crate::DeleteEvent)))
}

pub fn render(schedule: &Entity<Schedule>, event: &Event, selection: &ui::Selection, theme: &Theme, cx: &gpui::App) -> AnyElement {
    // selectable in reading order: title, place, description
    selection.begin();
    let title = ui::SelectableText::plain(selection, event.title.clone(), theme.font_family, WEIGHT_SEMIBOLD, theme.text_primary);
    let place = event.location.clone().map(|l| ui::SelectableText::plain(selection, l, theme.font_family, WEIGHT_NORMAL, theme.text_secondary));
    let s = schedule.read(cx);
    let calendar = s.calendar(&event.calendar);
    let color = calendar.map(|c| c.color).unwrap_or(theme.accent);
    // `invited`: there are guests at all (the people list shows); `own`: you organize it –
    // then there's nothing to answer, only to edit or cancel
    let invited = !event.attendees.is_empty();
    let own = s.is_own(event);

    let answers = if own { Some(organizer_bar(s, event, theme)) } else { invited.then(|| rsvp_buttons(schedule, event, theme)) };

    div()
        .id("event-detail")
        .w(px(WIDTH))
        .flex_none()
        .h_full()
        .overflow_y_scroll()
        .border_l_hairline()
        .border_color(theme.border_panel)
        .p(px(20.))
        .flex()
        .flex_col()
        .gap(px(14.))
        .child(div().text_size(px(18.)).child(title))
        .child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(6.))
                .children(calendar.map(|c| chip(theme).child(div().size(px(8.)).rounded_full().bg(color)).child(c.name.clone())))
                .children(event.recurrence.clone().map(|r| chip(theme).child(r)))
                .when(event.occurrence_source.as_deref() == Some("local-estimate"), |d| d.child(
                    chip(theme).id("estimated-occurrence")
                        .child("Estimated times")
                        .tooltip(ui::tooltip("Locally expanded series. Sync this calendar to use the provider's times; daylight-saving transitions may differ.")),
                ))
                .when(invited && !own, |d| d.child(chip(theme).child(rsvp_label(event.rsvp))))
                .when(own && invited, |d| d.child(chip(theme).child("Organizer"))),
        )
        .child(
            div()
                .flex()
                .flex_col()
                .gap(px(8.))
                .text_size(px(SIZE_SMALL))
                .text_color(theme.text_secondary)
                .child(div().flex().items_center().gap(px(8.)).child(Icon::new(IconName::MailClock).size(14.).color(theme.text_tertiary)).child(when(event)))
                .children(place.map(|l| div().flex().items_center().gap(px(8.)).child(Icon::new(IconName::MapPin).size(14.).color(theme.text_tertiary)).child(l)))
                .children(event.conference.clone().map(|link| {
                    div().flex().items_center().gap(px(8.)).child(Icon::new(IconName::Video).size(14.).color(theme.text_tertiary)).child(div().text_color(theme.accent).child(link))
                })),
        )
        .children(answers)
        .when(invited, |d| {
            d.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(2.))
                    .child(div().mb(px(4.)).text_size(px(SIZE_MINI)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_tertiary).child(format!("{} people", event.attendees.len())))
                    .children(event.attendees.iter().map(|a| {
                        let avatar = match &a.avatar {
                            Some(path) => Avatar::new(a.name.clone()).image(path.clone()),
                            None => Avatar::new(a.name.clone()),
                        };
                        div()
                            .flex()
                            .items_center()
                            .gap(px(8.))
                            .h(px(28.))
                            .text_size(px(SIZE_SMALL))
                            .child(avatar.size(20.))
                            .child(div().flex_1().min_w_0().truncate().text_color(theme.text_secondary).child(a.name.clone()))
                            .when(a.organizer, |r| r.child(div().text_size(px(SIZE_MICRO)).text_color(theme.text_quaternary).child("organizer")))
                            .child(rsvp_mark(a.rsvp, theme))
                    })),
            )
        })
        .children(event.description.clone().map(|text| {
            div()
                .pt(px(12.))
                .border_t_hairline()
                .border_color(theme.border_panel)
                .text_size(px(SIZE_SMALL))
                .text_color(theme.text_body)
                .child(markdown::render_selectable(format!("event-{}", event.id), &text, theme.font_family, theme, selection))
        }))
        .into_any_element()
}
