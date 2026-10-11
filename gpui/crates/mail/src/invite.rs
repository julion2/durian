//! The invitation card in a message: what, when, who – the day around it as a strip with the
//! clashes marked – and the answer, with the same Going / Maybe / No as the calendar.

use calendar::detail::{rsvp_buttons, rsvp_label, when};
use calendar::model::{Event, Rsvp, Schedule};
use chrono::{NaiveDateTime, Timelike};
use gpui::{AnyElement, App, Entity, IntoElement, SharedString, Window, div, prelude::*, px, relative};
use ui::theme::{Theme, lch, typography::*};
use ui::{Icon, IconName, StyledExt};

/// What an invitation mail says about its event (its iTIP method).
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Kind {
    /// an invitation or an update: to answer
    Request,
    /// the event is off
    Cancel,
    /// a guest's answer to your event
    Reply,
    /// anything else (published, countered …): shown, not answered here
    Other,
}

/// An invitation a message carries itself – durian's copy of its iCalendar part.
#[derive(Clone, Debug)]
pub struct Invitation {
    pub event: Event,
    pub kind: Kind,
    /// about one date of a series only
    pub occurrence: bool,
}

/// The card's event for a message: the calendar's own copy when it's loaded – it has the
/// calendar, and your latest answer – else the copy the mail carries. Guests the calendar
/// copy doesn't have yet (they come with its detail) are taken from the mail.
pub fn resolve(schedule: &Schedule, message: &crate::model::Message) -> Option<(Event, Kind, bool)> {
    if let Some(id) = &message.invite {
        return Some((schedule.event(id)?.clone(), Kind::Request, false));
    }
    let carried = message.invitation.as_ref()?;
    let event = match schedule.event(&carried.event.id) {
        Some(listed) => {
            let mut event = listed.clone();
            if event.attendees.is_empty() {
                event.attendees = carried.event.attendees.clone();
                event.description = event.description.or_else(|| carried.event.description.clone());
            }
            event
        }
        None => carried.event.clone(),
    };
    Some((event, carried.kind, carried.occurrence))
}

/// Events of that day that overlap the invitation (yours, not declined, not all-day).
pub fn conflicts<'a>(schedule: &'a Schedule, invite: &Event) -> Vec<&'a Event> {
    schedule
        .events_on(invite.start.date())
        .filter(|e| e.id != invite.id && !e.all_day && e.rsvp != Rsvp::Declined && e.start < invite.end && e.end > invite.start)
        .collect()
}

fn hour_of(t: NaiveDateTime) -> f32 {
    t.hour() as f32 + t.minute() as f32 / 60.
}

/// The hours around the invitation, with every event of that day in place.
fn day_strip(schedule: &Schedule, invite: &Event, clashing: &[&Event], theme: &Theme) -> gpui::Div {
    let from = (hour_of(invite.start) - 2.).floor().clamp(0., 20.);
    let to = (hour_of(invite.end) + 2.).ceil().clamp(from + 4., 24.);
    let span = to - from;
    let x = |t: NaiveDateTime| ((hour_of(t) - from) / span).clamp(0., 1.);
    let red = lch(55., 70., 30.);

    // only what falls inside the window – the rest would pile up as slivers at the edges
    let (window_start, window_end) = (from, to);
    // the invitation itself, also when only the mail has it
    let own = schedule.event(&invite.id).is_none().then_some(invite);
    let blocks = schedule
        .events_on(invite.start.date())
        .filter(move |e| !e.all_day && e.rsvp != Rsvp::Declined && hour_of(e.end) > window_start && hour_of(e.start) < window_end)
        .chain(own)
        .map(|e| {
        let is_invite = e.id == invite.id;
        let clash = clashing.iter().any(|c| c.id == e.id);
        let color = schedule.calendar(&e.calendar).map(|c| c.color).unwrap_or(theme.accent);
        let (left, right) = (x(e.start), x(e.end));
        div()
            .absolute()
            .top(px(if is_invite { 2. } else { 6. }))
            .bottom(px(if is_invite { 2. } else { 6. }))
            .left(relative(left))
            .w(relative((right - left).max(0.01)))
            .rounded(theme.radius.xs)
            .map(|b| {
                if is_invite {
                    b.border_1().border_color(theme.accent).bg(theme.accent.opacity(0.12))
                } else if clash {
                    b.bg(red.opacity(0.35))
                } else {
                    b.bg(color.opacity(0.3))
                }
            })
    });
    let ticks = ((from as u32 + 1)..(to as u32)).map(|h| {
        let at = (h as f32 - from) / span;
        div().absolute().left(relative(at)).top_0().bottom_0().w(px(0.5)).bg(theme.border_card).child(
            div().absolute().top(px(-14.)).left(px(-10.)).w(px(20.)).flex().justify_center().text_size(px(10.)).text_color(theme.text_quaternary).child(format!("{h}")),
        )
    });
    div().relative().h(px(28.)).mt(px(14.)).rounded(theme.radius.sm).bg(theme.bg_group).children(ticks).children(blocks)
}

pub fn render(
    schedule: &Entity<Schedule>,
    (event, kind, occurrence): (Event, Kind, bool),
    open: impl Fn(&mut Window, &mut App) + 'static,
    theme: &Theme,
    cx: &App,
) -> AnyElement {
    let s = schedule.read(cx);
    let calendar = s.calendar(&event.calendar).cloned();
    let cancelled = kind == Kind::Cancel;
    // the day's other events only when they're all here – else it would look free
    let known = s.knows(event.start.date()) && !cancelled;
    let clashing: Vec<Event> = if known { conflicts(s, &event).into_iter().cloned().collect() } else { Vec::new() };
    let clash_refs: Vec<&Event> = clashing.iter().collect();
    let strip = (known && !event.all_day).then(|| day_strip(s, &event, &clash_refs, theme));
    let guests = event.attendees.len();
    let (status, status_color) = match kind {
        Kind::Request => (rsvp_label(event.rsvp), if event.rsvp == Rsvp::NeedsAction { theme.accent } else { theme.text_tertiary }),
        Kind::Cancel => ("Cancelled", lch(55., 70., 30.)),
        Kind::Reply => ("Reply", theme.text_tertiary),
        Kind::Other => ("", theme.text_tertiary),
    };

    let tile = div()
        .flex()
        .flex_col()
        .items_center()
        .justify_center()
        .flex_none()
        .w(px(44.))
        .h(px(48.))
        .rounded(theme.radius.md)
        .bg(theme.bg_raised)
        .shadow(theme.raised_shadow(false))
        .child(div().text_size(px(10.)).font_weight(WEIGHT_SEMIBOLD).text_color(lch(55., 70., 30.)).child(event.start.format("%a").to_string().to_uppercase()))
        .child(div().text_size(px(18.)).line_height(px(22.)).font_weight(WEIGHT_SEMIBOLD).text_color(theme.text_primary).child(event.start.format("%-d").to_string()));

    let red = lch(55., 70., 30.);
    let clash_note = (!clashing.is_empty()).then(|| {
        let names: Vec<String> = clashing.iter().map(|e| format!("{} {}–{}", e.title, e.start.format("%H:%M"), e.end.format("%H:%M"))).collect();
        div().flex().items_center().gap(px(6.)).text_size(px(SIZE_MINI)).text_color(red).child("Conflicts with").child(div().min_w_0().truncate().child(names.join(", ")))
    });

    div()
        .flex()
        .flex_col()
        .gap(px(12.))
        .p(px(14.))
        .rounded(theme.radius.lg)
        .border_hairline()
        .border_color(theme.border_card)
        .bg(theme.bg_panel)
        .child(
            div()
                .flex()
                .gap(px(12.))
                .child(tile)
                .child(
                    div()
                        .flex_1()
                        .min_w_0()
                        .flex()
                        .flex_col()
                        .gap(px(3.))
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(8.))
                                .child(
                                    div()
                                        .flex_1()
                                        .min_w_0()
                                        .truncate()
                                        .text_size(px(SIZE_SMALL))
                                        .font_weight(WEIGHT_SEMIBOLD)
                                        .text_color(if cancelled { theme.text_tertiary } else { theme.text_primary })
                                        .when(cancelled, |t| t.line_through())
                                        .child(event.title.clone()),
                                )
                                .child(div().text_size(px(SIZE_MINI)).text_color(status_color).child(status)),
                        )
                        .child(div().text_size(px(SIZE_MINI)).text_color(theme.text_secondary).child(when(&event)))
                        .when(occurrence, |c| c.child(div().text_size(px(SIZE_MINI)).text_color(theme.text_tertiary).child("This date only – the rest of the series is unchanged")))
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(10.))
                                .text_size(px(SIZE_MINI))
                                .text_color(theme.text_tertiary)
                                .children(calendar.map(|c| div().flex().items_center().gap(px(5.)).child(div().size(px(7.)).rounded_full().bg(c.color)).child(c.name)))
                                .children(event.conference.clone().map(|link| {
                                    div().flex().items_center().gap(px(5.)).child(Icon::new(IconName::Video).size(12.).color(theme.text_tertiary)).child(div().text_color(theme.accent).child(link))
                                }))
                                .when(guests > 0, |r| {
                                    r.child(div().flex().items_center().gap(px(5.)).child(Icon::new(IconName::MailUsers).size(12.).color(theme.text_tertiary)).child(format!("{guests} guests")))
                                }),
                        ),
                ),
        )
        .children(strip)
        .children(clash_note)
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(12.))
                .when(kind == Kind::Request, |r| r.child(rsvp_buttons(schedule, &event, theme).w(px(300.))))
                .child(div().flex_1())
                .child(
                    div()
                        .id(SharedString::from(format!("open-event-{}", event.id)))
                        .flex()
                        .items_center()
                        .gap(px(4.))
                        .text_size(px(SIZE_MINI))
                        .text_color(theme.text_tertiary)
                        .hover(|l| l.text_color(theme.text_primary))
                        .child("Open in calendar")
                        .child(Icon::new(IconName::ArrowRight).size(12.).color(theme.text_tertiary))
                        .on_click(move |_, window, cx| open(window, cx)),
                ),
        )
        .into_any_element()
}
