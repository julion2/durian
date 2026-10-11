//! New event: the mail composer's sibling – same card, header, title field and footer. "When"
//! takes the same natural language as ⌘K ("tomorrow 3pm for 1h") and shows what it understood;
//! guests come from the mail contacts.

use calendar::model::{Attendee, Event, Rsvp, Schedule};
use calendar::parse::{self, Parsed};
use chrono::{NaiveDateTime, Timelike};
use gpui::{App, Context, Entity, EventEmitter, FocusHandle, Focusable, IntoElement, MouseButton, Render, SharedString, Subscription, Window, div, prelude::*, px};
use ui::theme::{ActiveTheme, typography::*};
use ui::{Icon, IconButton, IconName, PrimaryButton, StyledExt, TextArea, TextAreaEvent, TextInput, TextInputEvent};

use crate::compose::{Close, NextField, PrevField, RecipientField, Send};
use crate::model::Mailbox;

pub enum EventComposeEvent {
    Saved(SharedString),
    Close,
}

/// Prefill: the slot that was dragged out, or the next full hour.
pub struct EventSeed {
    pub start: NaiveDateTime,
    pub end: NaiveDateTime,
    pub all_day: bool,
    /// "Create event from thread": the subject and the people on the thread
    pub title: Option<SharedString>,
    pub guests: Vec<SharedString>,
    /// Edit: the event to change (everything else is taken from it)
    pub editing: Option<Event>,
}

impl EventSeed {
    pub fn slot(start: NaiveDateTime, end: NaiveDateTime, all_day: bool) -> Self {
        Self { start, end, all_day, title: None, guests: Vec::new(), editing: None }
    }

    /// Edit an existing event: its time, title, guests (not you), place and notes.
    pub fn edit(event: Event, me: &[SharedString]) -> Self {
        let guests = event.attendees.iter().filter(|a| !me.iter().any(|m| m.eq_ignore_ascii_case(&a.email))).map(|a| a.name.clone()).collect();
        Self { start: event.start, end: event.end, all_day: event.all_day, title: Some(event.title.clone()), guests, editing: Some(event) }
    }
}

impl EventSeed {
    /// `c` in the calendar: the next full hour of the day in view, an hour long – within the
    /// working day (at night it's 9:00 today, in the evening 9:00 tomorrow).
    pub fn next_hour(day: chrono::NaiveDate) -> Self {
        let now = chrono::Local::now().naive_local();
        let nine = |d: chrono::NaiveDate| d.and_hms_opt(9, 0, 0).unwrap_or(now);
        let start = if day != now.date() {
            nine(day)
        } else {
            match now.hour() + 1 {
                h if h < 8 => nine(day),
                h if h > 20 => nine(day + chrono::Duration::days(1)),
                h => day.and_hms_opt(h, 0, 0).unwrap_or(now),
            }
        };
        Self::slot(start, start + chrono::Duration::hours(1), false)
    }
}

pub struct EventComposer {
    mailbox: Entity<Mailbox>,
    schedule: Entity<Schedule>,
    focus: FocusHandle,
    calendar: SharedString,
    title: Entity<TextInput>,
    when: Entity<TextInput>,
    /// what "When" currently reads as (None: can't tell)
    parsed: Option<Parsed>,
    guests: Entity<RecipientField>,
    location: Entity<TextInput>,
    notes: Entity<TextArea>,
    editing: Option<Event>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<EventComposeEvent> for EventComposer {}

impl Focusable for EventComposer {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

/// "Sat, Oct 10 12:30 – 13:30": readable, and the parser reads it back.
fn when_text(seed: &EventSeed) -> String {
    let day = |t: NaiveDateTime| t.format("%a, %b %-d").to_string();
    let last = (seed.end - chrono::Duration::minutes(1)).date();
    match (seed.all_day, last > seed.start.date()) {
        (true, false) => format!("{} all day", day(seed.start)),
        (true, true) => format!("{} – {} all day", day(seed.start), last.format("%a, %b %-d")),
        (false, false) => format!("{} {} – {}", day(seed.start), seed.start.format("%H:%M"), seed.end.format("%H:%M")),
        (false, true) => format!("{} {} – {} {}", day(seed.start), seed.start.format("%H:%M"), day(seed.end), seed.end.format("%H:%M")),
    }
}

impl EventComposer {
    pub fn new(mailbox: Entity<Mailbox>, schedule: Entity<Schedule>, seed: EventSeed, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let contacts = mailbox.read(cx).contacts();
        let calendar = match &seed.editing {
            Some(event) => event.calendar.clone(),
            None => schedule.read(cx).default_calendar(mailbox.read(cx).account_filter.as_deref()),
        };
        let title = cx.new(|cx| {
            let mut input = TextInput::new("Event title", cx);
            if let Some(t) = seed.title.clone() {
                input.set_text(t);
            }
            input
        });
        let when = cx.new(|cx| {
            let mut input = TextInput::new("When? e.g. tomorrow 3pm for 1h", cx);
            input.set_text(SharedString::from(when_text(&seed)));
            input
        });
        let parsed = Some(Parsed { title: String::new(), start: seed.start, end: seed.end, all_day: seed.all_day, people: Vec::new() });
        let guests = cx.new(|cx| RecipientField::new("Guests", false, seed.guests.clone(), contacts, cx).as_property(IconName::MailUsers, "Add guests…", cx));
        let place = seed.editing.as_ref().and_then(|e| e.location.clone().or(e.conference.clone()));
        let description = seed.editing.as_ref().and_then(|e| e.description.clone());
        let location = cx.new(|cx| {
            let mut input = TextInput::new("Add a place or call link", cx);
            if let Some(p) = place {
                input.set_text(p);
            }
            input
        });
        let notes = cx.new(|cx| {
            let mut area = TextArea::new("Add a description…", 4, cx).markdown(true);
            if let Some(d) = description {
                area.set_text(d, cx);
            }
            area
        });
        let subscriptions = vec![
            cx.subscribe(&when, |this, _, TextInputEvent::Changed(text), cx| {
                let names: Vec<SharedString> = this.mailbox.read(cx).contacts().into_iter().map(|c| c.name).collect();
                let names: Vec<&str> = names.iter().map(|n| n.as_ref()).collect();
                this.parsed = parse::parse(text, chrono::Local::now().naive_local(), &names);
                cx.notify();
            }),
            cx.subscribe_in(&notes, window, |this, _, event, window, cx| match event {
                TextAreaEvent::Submit => this.save(&Send, window, cx),
                TextAreaEvent::Cancel => this.close(&Close, window, cx),
                TextAreaEvent::Changed => {}
            }),
        ];
        title.read(cx).focus_handle(cx).focus(window, cx);
        Self { mailbox, schedule, focus: cx.focus_handle(), calendar, title, when, parsed, guests, location, notes, editing: seed.editing, _subscriptions: subscriptions }
    }

    fn save(&mut self, _: &Send, _window: &mut Window, cx: &mut Context<Self>) {
        let Some(when) = self.parsed.clone() else { return };
        let mailbox = self.mailbox.read(cx);
        let contacts = mailbox.contacts();
        let me = mailbox.me.clone();
        let guests = self.guests.read(cx).recipients();
        let mut attendees: Vec<Attendee> = Vec::new();
        if !guests.is_empty() {
            attendees.push(Attendee { name: me.name.clone(), email: me.email.clone(), rsvp: Rsvp::Accepted, organizer: true, avatar: me.avatar.as_ref().map(|a| a.path.clone()) });
            for guest in guests {
                // a guest who was already invited keeps their answer
                if let Some(known) = self.editing.as_ref().and_then(|e| e.attendees.iter().find(|a| a.name == guest && !a.organizer)) {
                    attendees.push(known.clone());
                    continue;
                }
                let contact = contacts.iter().find(|c| c.name == guest);
                attendees.push(Attendee {
                    name: guest.clone(),
                    email: contact.map(|c| c.email.clone()).unwrap_or(guest),
                    rsvp: Rsvp::NeedsAction,
                    organizer: false,
                    avatar: contact.and_then(|c| c.avatar.as_ref().map(|a| a.path.clone())),
                });
            }
        }
        let title = self.title.read(cx).text().trim().to_string();
        let location = self.location.read(cx).text().trim().to_string();
        let notes = self.notes.read(cx).text().trim().to_string();
        let editing = self.editing.clone();
        let event = Event {
            id: editing.as_ref().map(|e| e.id.clone()).unwrap_or_else(|| uuid::Uuid::new_v4().simple().to_string().into()),
            calendar: self.calendar.clone(),
            title: if title.is_empty() { "New event".into() } else { title.into() },
            start: when.start,
            end: when.end,
            all_day: when.all_day,
            // a link goes to the call field, anything else is a place
            conference: (location.starts_with("http") || location.contains("meet.")).then(|| location.clone().into()),
            location: (!location.is_empty() && !location.starts_with("http") && !location.contains("meet.")).then(|| location.into()),
            description: (!notes.is_empty()).then(|| notes.into()),
            attendees,
            rsvp: editing.as_ref().map_or(Rsvp::Accepted, |e| e.rsvp),
            recurrence: editing.as_ref().and_then(|e| e.recurrence.clone()),
            series: editing.as_ref().and_then(|e| e.series.clone()),
        };
        let id = event.id.clone();
        self.schedule.update(cx, |s, cx| if editing.is_some() { s.replace_event(event, cx) } else { s.add_event(event, cx) });
        cx.emit(EventComposeEvent::Saved(id));
    }

    fn close(&mut self, _: &Close, _window: &mut Window, cx: &mut Context<Self>) {
        cx.emit(EventComposeEvent::Close);
    }

    /// Tab order: title → when → guests → location → notes.
    fn move_focus(&mut self, delta: isize, window: &mut Window, cx: &mut Context<Self>) {
        let order: [FocusHandle; 5] = [
            self.title.read(cx).focus_handle(cx),
            self.when.read(cx).focus_handle(cx),
            self.guests.read(cx).input_focus(cx),
            self.location.read(cx).focus_handle(cx),
            self.notes.read(cx).focus_handle(cx),
        ];
        let current = order.iter().position(|f| f.is_focused(window)).unwrap_or(0) as isize;
        let next = (current + delta).rem_euclid(order.len() as isize) as usize;
        order[next].focus(window, cx);
    }

    /// All day on/off rewrites "When" – the text stays the single source of truth.
    fn toggle_all_day(&mut self, cx: &mut Context<Self>) {
        let Some(p) = self.parsed.clone() else { return };
        let text = if p.all_day {
            let start = p.start.date().and_hms_opt(9, 0, 0).unwrap_or(p.start);
            when_text(&EventSeed::slot(start, start + chrono::Duration::hours(1), false))
        } else {
            format!("{} all day", p.start.format("%a, %b %-d"))
        };
        self.when.update(cx, |input, cx| {
            input.set_text(SharedString::from(text.clone()));
            cx.notify();
        });
        let names: Vec<SharedString> = self.mailbox.read(cx).contacts().into_iter().map(|c| c.name).collect();
        let names: Vec<&str> = names.iter().map(|n| n.as_ref()).collect();
        self.parsed = parse::parse(&text, chrono::Local::now().naive_local(), &names);
        cx.notify();
    }

    /// Your other events in that slot (not declined, not all-day).
    fn conflicts(&self, cx: &App) -> Vec<Event> {
        let Some(p) = self.parsed.as_ref().filter(|p| !p.all_day) else { return Vec::new() };
        // every day the slot touches, not just the first
        self.schedule
            .read(cx)
            .events
            .iter()
            .filter(|e| !e.all_day && e.rsvp != Rsvp::Declined && e.start < p.end && e.end > p.start)
            .cloned()
            .collect()
    }
}

/// A property row: icon column, then the field – the details panel's layout, editable.
fn property(icon: IconName, field: impl IntoElement, theme: &ui::theme::Theme) -> gpui::Div {
    div()
        .flex()
        .items_center()
        .min_h(px(34.))
        .child(div().w(px(28.)).flex_none().child(Icon::new(icon).size(15.).color(theme.text_tertiary)))
        .child(div().flex_1().min_w_0().text_size(px(SIZE_SMALL)).text_color(theme.text_primary).child(field))
}

/// What "When" was understood as, as quiet chips: date · time · length.
fn chip(label: impl Into<SharedString>, theme: &ui::theme::Theme) -> gpui::Div {
    div()
        .flex()
        .items_center()
        .h(px(22.))
        .px(px(8.))
        .rounded(theme.radius.pill)
        .bg(theme.bg_raised)
        .shadow(theme.raised_shadow(false))
        .text_size(px(SIZE_MINI))
        .text_color(theme.text_secondary)
        .child(label.into())
}

fn length(p: &Parsed) -> String {
    let minutes = (p.end - p.start).num_minutes();
    match (minutes / 60, minutes % 60) {
        (0, m) => format!("{m}m"),
        (h, 0) => format!("{h}h"),
        (h, m) => format!("{h}h {m}m"),
    }
}

impl Render for EventComposer {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let calendar = self.schedule.read(cx).calendar(&self.calendar).cloned();
        let today = chrono::Local::now().date_naive();
        let red = ui::theme::lch(55., 70., 30.);
        let conflicts = self.conflicts(cx);
        let all_day = self.parsed.as_ref().is_some_and(|p| p.all_day);

        let understood = match &self.parsed {
            Some(p) => {
                let day = match (p.start.date() - today).num_days() {
                    0 => "Today".to_string(),
                    1 => "Tomorrow".to_string(),
                    _ => p.start.format("%a, %b %-d").to_string(),
                };
                let last = (p.end - chrono::Duration::minutes(1)).date();
                let days = (last - p.start.date()).num_days() + 1;
                let short = |t: NaiveDateTime| t.format("%a %H:%M").to_string();
                div()
                    .flex()
                    .flex_none()
                    .gap(px(6.))
                    .map(|d| match (p.all_day, days > 1) {
                        (false, false) => d.child(chip(day, &theme)).child(chip(format!("{} – {}", p.start.format("%H:%M"), p.end.format("%H:%M")), &theme)).child(chip(length(p), &theme)),
                        (false, true) => d.child(chip(short(p.start), &theme)).child(chip(format!("→ {}", short(p.end)), &theme)).child(chip(length(p), &theme)),
                        (true, false) => d.child(chip(day, &theme)).child(chip("All day", &theme)),
                        (true, true) => d.child(chip(format!("{} – {}", p.start.format("%b %-d"), last.format("%b %-d")), &theme)).child(chip(format!("{days} days"), &theme)),
                    })
            }
            None => div().flex_none().text_size(px(SIZE_MINI)).text_color(red).child("Can't read that – try \"fri 14-15\""),
        };
        let conflict_line = (!conflicts.is_empty()).then(|| {
            let names: Vec<String> = conflicts.iter().map(|e| format!("{} {}–{}", e.title, e.start.format("%H:%M"), e.end.format("%H:%M"))).collect();
            div().pl(px(28.)).pb(px(4.)).text_size(px(SIZE_MINI)).text_color(red).truncate().child(format!("Overlaps {}", names.join(", ")))
        });

        div()
            .key_context("Compose")
            .track_focus(&self.focus)
            .on_action(cx.listener(Self::save))
            .on_action(cx.listener(Self::close))
            .on_action(cx.listener(|this, _: &NextField, window, cx| this.move_focus(1, window, cx)))
            .on_action(cx.listener(|this, _: &PrevField, window, cx| this.move_focus(-1, window, cx)))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .w(px(640.))
            .flex()
            .flex_col()
            .bg(theme.bg_card)
            .border_hairline()
            .border_color(theme.border_card)
            .rounded(theme.radius.dialog)
            .shadow(theme.overlay_shadow())
            .text_color(theme.text_body)
            // header: calendar chip › New event, close – the mail composer's header
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.))
                    .h(px(52.))
                    .px(px(12.))
                    .child(
                        div()
                            .id("event-calendar")
                            .flex()
                            .items_center()
                            .gap(px(6.))
                            .h(px(24.))
                            .px(px(8.))
                            .rounded(theme.radius.pill)
                            .shadow(theme.raised_shadow(false))
                            .text_size(px(SIZE_MINI))
                            .text_color(theme.text_primary)
                            .hover(|s| s.bg(theme.bg_raised_hover))
                            .when_some(calendar, |s, c| s.child(div().size(px(8.)).rounded_full().bg(c.color)).child(c.name.clone()))
                            // click: the next calendar
                            .on_click(cx.listener(|this, _, _, cx| {
                                let calendars = &this.schedule.read(cx).calendars;
                                let next = calendars.iter().position(|c| c.id == this.calendar).map_or(0, |i| (i + 1) % calendars.len());
                                this.calendar = calendars[next].id.clone();
                                cx.notify();
                            })),
                    )
                    .child(div().text_size(px(SIZE_SMALL)).text_color(theme.text_meta).child("›"))
                    .child(div().text_size(px(SIZE_SMALL)).child(if self.editing.is_some() { "Edit event" } else { "New event" }))
                    .child(div().flex_1())
                    .child(IconButton::new("event-close", IconName::CloseSmall).on_click(cx.listener(|this, _, window, cx| this.close(&Close, window, cx)))),
            )
            .child(
                div()
                    .px(px(18.))
                    .flex()
                    .flex_col()
                    // title in the subject's style
                    .child(div().pt(px(2.)).pb(px(10.)).text_size(px(18.)).line_height(px(26.)).font_weight(WEIGHT_SEMIBOLD).text_color(theme.text_primary).child(self.title.clone()))
                    .child(property(
                        IconName::MailClock,
                        div().flex().items_center().gap(px(12.)).child(div().flex_1().min_w_0().child(self.when.clone())).child(understood),
                        &theme,
                    ))
                    .children(conflict_line)
                    .child(self.guests.clone())
                    .child(property(IconName::MapPin, self.location.clone(), &theme))
                    // description in the mail body's style
                    .child(
                        div()
                            .mt(px(10.))
                            .pt(px(12.))
                            .pb(px(12.))
                            .min_h(px(96.))
                            .border_t_hairline()
                            .border_color(theme.border_card)
                            .text_size(px(SIZE_REGULAR))
                            .line_height(px(22.5))
                            .child(self.notes.clone()),
                    ),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(8.))
                    .px(px(12.))
                    .pb(px(12.))
                    .child(
                        div()
                            .id("event-all-day")
                            .flex()
                            .items_center()
                            .gap(px(6.))
                            .h(px(28.))
                            .px(px(10.))
                            .rounded(theme.radius.pill)
                            .text_size(px(SIZE_MINI))
                            .font_weight(WEIGHT_MEDIUM)
                            .map(|b| {
                                if all_day {
                                    b.bg(theme.accent.opacity(0.16)).text_color(theme.accent)
                                } else {
                                    b.shadow(theme.raised_shadow(false)).text_color(theme.text_secondary).hover(|b| b.bg(theme.bg_raised_hover))
                                }
                            })
                            .child(Icon::new(IconName::Calendar).size(13.).color(if all_day { theme.accent } else { theme.text_tertiary }))
                            .child("All day")
                            .on_click(cx.listener(|this, _, _, cx| this.toggle_all_day(cx))),
                    )
                    .child(div().flex_1())
                    .child(div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child("⌘↵ to save"))
                    .child(div().when(self.parsed.is_none(), |s| s.opacity(0.5)).child(
                        PrimaryButton::new("event-save", "Save").on_click(cx.listener(|this, _, window, cx| this.save(&Send, window, cx))),
                    )),
            )
    }
}
