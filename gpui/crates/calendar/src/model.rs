//! Calendar data as the data source hands it over ([`CalendarData`]), and the [`Schedule`] entity
//! the views share: which period is shown, in which view, and which event is selected.

use chrono::{Datelike, Duration, NaiveDate, NaiveDateTime, NaiveTime};
use gpui::{Context, EventEmitter, Hsla, SharedString};
use serde::{Deserialize, Deserializer};
use ui::theme::parse_css_color;

fn color<'de, D: Deserializer<'de>>(d: D) -> Result<Hsla, D::Error> {
    let s = String::deserialize(d)?;
    parse_css_color(&s).ok_or_else(|| serde::de::Error::custom(format!("bad color {s}")))
}

/// One calendar of an account ("Work", "Personal", "OSS meetups").
#[derive(Deserialize, Clone, Debug)]
pub struct CalendarInfo {
    pub id: SharedString,
    pub name: SharedString,
    /// mail account id the calendar belongs to
    pub account: SharedString,
    #[serde(deserialize_with = "color")]
    pub color: Hsla,
}

#[derive(Deserialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Rsvp {
    Accepted,
    Tentative,
    Declined,
    /// invited, not answered yet
    NeedsAction,
}

#[derive(Deserialize, Clone, Debug)]
pub struct Attendee {
    pub name: SharedString,
    pub email: SharedString,
    pub rsvp: Rsvp,
    #[serde(default)]
    pub organizer: bool,
    /// picture path served by the app's assets (the same pictures the mail UI shows)
    #[serde(default)]
    pub avatar: Option<SharedString>,
}

#[derive(Clone, Debug)]
pub struct Event {
    pub id: SharedString,
    pub calendar: SharedString,
    pub title: SharedString,
    pub start: NaiveDateTime,
    /// exclusive; all-day events end at midnight after their last day
    pub end: NaiveDateTime,
    pub all_day: bool,
    pub location: Option<SharedString>,
    pub conference: Option<SharedString>,
    pub description: Option<SharedString>,
    pub attendees: Vec<Attendee>,
    /// the user's own answer
    pub rsvp: Rsvp,
    /// "Every weekday", "Weekly on Tuesday" – display only, occurrences are expanded by the source
    pub recurrence: Option<SharedString>,
    /// occurrences of one series share this id
    pub series: Option<SharedString>,
}

impl Event {
    pub fn duration(&self) -> Duration {
        self.end - self.start
    }

    /// Does the event touch this day? (timed events only count the days they actually cover)
    pub fn on(&self, day: NaiveDate) -> bool {
        let start = day.and_time(NaiveTime::MIN);
        self.start < start + Duration::days(1) && self.end > start
    }
}

pub struct CalendarData {
    pub calendars: Vec<CalendarInfo>,
    pub events: Vec<Event>,
    /// the user's own addresses: decides whether an event is yours or an invitation
    pub me: Vec<SharedString>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum View {
    Day,
    Week,
    Month,
    Agenda,
}

impl View {
    pub fn label(self) -> &'static str {
        match self {
            View::Day => "Day",
            View::Week => "Week",
            View::Month => "Month",
            View::Agenda => "Agenda",
        }
    }
}

/// User intentions are separate from authoritative server answers. A remote
/// schedule never treats a local edit as a successful persistence operation.
#[derive(Clone, Debug)]
pub enum WriteIntent {
    Save { event: Event, new: bool },
    Delete(Vec<Event>),
    Rsvp { event: Event, response: Rsvp },
}

pub enum ScheduleEvent {
    SelectionChanged,
    /// A change was asked for, but the calendar is only read from a server (nothing changed).
    ReadOnly,
    WriteRequested(WriteIntent),
    WriteFinished { id: SharedString, result: Result<SharedString, SharedString> },
    EditRequested(SharedString),
    EditReady(Event),
}

/// The calendar state every view reads: data, the visible period and the selection.
pub struct Schedule {
    pub me: Vec<SharedString>,
    pub calendars: Vec<CalendarInfo>,
    pub events: Vec<Event>,
    pub view: View,
    /// the day the visible period is built around (its week, its month …)
    pub anchor: NaiveDate,
    pub selected: Option<SharedString>,
    pub today: NaiveDate,
    /// A host can disable writes until its persistence controller is ready.
    pub read_only: bool,
    /// The periods whose events are here [from, to), when they come in windows from a
    /// server; None: all of them are (fixtures).
    pub loaded: Option<Vec<(NaiveDate, NaiveDate)>>,
}

impl EventEmitter<ScheduleEvent> for Schedule {}

impl Schedule {
    pub fn new(data: CalendarData, today: NaiveDate) -> Self {
        let mut events = data.events;
        events.sort_by_key(|e| (e.start, std::cmp::Reverse(e.end)));
        Self { me: data.me, calendars: data.calendars, events, view: View::Week, anchor: today, selected: None, today, read_only: false, loaded: None }
    }

    /// Whether every event of that day is here – only then can a day look free.
    pub fn knows(&self, day: NaiveDate) -> bool {
        self.loaded.as_ref().is_none_or(|windows| windows.iter().any(|&(from, to)| from <= day && day < to))
    }

    pub fn calendar(&self, id: &str) -> Option<&CalendarInfo> {
        self.calendars.iter().find(|c| c.id == id)
    }

    pub fn event(&self, id: &str) -> Option<&Event> {
        self.events.iter().find(|e| e.id == id)
    }

    pub fn selected_event(&self) -> Option<&Event> {
        self.selected.as_ref().and_then(|id| self.event(id))
    }

    /// The days the current view shows, first to last.
    pub fn days(&self) -> Vec<NaiveDate> {
        match self.view {
            View::Day => vec![self.anchor],
            View::Week => {
                let monday = self.anchor - Duration::days(self.anchor.weekday().num_days_from_monday() as i64);
                (0..7).map(|i| monday + Duration::days(i)).collect()
            }
            View::Month => {
                // whole weeks covering the month, Monday first
                let first = self.anchor.with_day(1).unwrap_or(self.anchor);
                let start = first - Duration::days(first.weekday().num_days_from_monday() as i64);
                let next_month = if first.month() == 12 {
                    NaiveDate::from_ymd_opt(first.year() + 1, 1, 1)
                } else {
                    NaiveDate::from_ymd_opt(first.year(), first.month() + 1, 1)
                }
                .unwrap_or(first);
                let weeks = ((next_month - start).num_days() + 6) / 7;
                (0..weeks * 7).map(|i| start + Duration::days(i)).collect()
            }
            View::Agenda => (0..14).map(|i| self.anchor + Duration::days(i)).collect(),
        }
    }

    pub fn events_on(&self, day: NaiveDate) -> impl Iterator<Item = &Event> {
        self.events.iter().filter(move |e| e.on(day))
    }

    pub fn set_view(&mut self, view: View, cx: &mut Context<Self>) {
        self.view = view;
        cx.notify();
    }

    /// h / l: one period back or forward (a day, a week, a month, two weeks of agenda).
    pub fn shift(&mut self, direction: i64, cx: &mut Context<Self>) {
        self.anchor = match self.view {
            View::Day => self.anchor + Duration::days(direction),
            View::Week => self.anchor + Duration::days(7 * direction),
            View::Agenda => self.anchor + Duration::days(14 * direction),
            View::Month => {
                let months = self.anchor.year() * 12 + self.anchor.month0() as i32 + direction as i32;
                let (year, month) = (months.div_euclid(12), months.rem_euclid(12) as u32 + 1);
                NaiveDate::from_ymd_opt(year, month, 1).unwrap_or(self.anchor)
            }
        };
        cx.notify();
    }

    pub fn go_to_today(&mut self, cx: &mut Context<Self>) {
        self.anchor = self.today;
        cx.notify();
    }

    pub fn select(&mut self, id: Option<SharedString>, cx: &mut Context<Self>) {
        if self.selected != id {
            self.selected = id;
            cx.emit(ScheduleEvent::SelectionChanged);
            cx.notify();
        }
    }

    /// The calendar new events land in: the first one of the account in view, else the first.
    pub fn default_calendar(&self, account: Option<&str>) -> SharedString {
        account
            .and_then(|a| self.calendars.iter().find(|c| c.account == a))
            .or(self.calendars.first())
            .map(|c| c.id.clone())
            .unwrap_or_default()
    }

    /// Guard for every change: true (and an explanation for the user) when it's refused.
    fn refuse(&self, cx: &mut Context<Self>) -> bool {
        if self.read_only {
            cx.emit(ScheduleEvent::ReadOnly);
        }
        self.read_only
    }

    /// Insert a new event (kept in time order), select it and bring its day into view. Local
    /// and optimistic, like every change here until intents exist.
    pub fn add_event(&mut self, event: Event, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        if self.loaded.is_some() {
            cx.emit(ScheduleEvent::WriteRequested(WriteIntent::Save { event, new: true }));
            return;
        }
        self.apply_event(event, cx);
    }

    /// A confirmed write or demo edit, never a new write intention.
    pub fn apply_event(&mut self, event: Event, cx: &mut Context<Self>) {
        self.events.retain(|e| e.id != event.id);
        let at = self.events.partition_point(|e| (e.start, std::cmp::Reverse(e.end)) <= (event.start, std::cmp::Reverse(event.end)));
        self.anchor = event.start.date();
        let id = event.id.clone();
        self.events.insert(at, event);
        self.select(Some(id), cx);
        cx.notify();
    }

    /// A data source's answer for the days [from, to): the calendars, and every event
    /// overlapping those days – replacing what was listed there, keeping what lies outside.
    /// Not a user's change, so allowed when read-only. The selection stays if its event does.
    pub fn load_window(&mut self, calendars: Vec<CalendarInfo>, from: NaiveDate, to: NaiveDate, events: Vec<Event>, cx: &mut Context<Self>) {
        let (start, end) = (from.and_time(NaiveTime::MIN), to.and_time(NaiveTime::MIN));
        self.calendars = calendars;
        self.events.retain(|e| !(e.start < end && e.end > start));
        self.events.extend(events);
        self.events.sort_by_key(|e| (e.start, std::cmp::Reverse(e.end)));
        self.loaded.get_or_insert_with(Vec::new).push((from, to));
        if self.selected.as_ref().is_some_and(|id| self.event(id).is_none()) {
            self.select(None, cx);
        }
        cx.notify();
    }

    /// What only an event's detail carries (guests, organizer, description, your answer), for
    /// every listed occurrence of it. From the data source, so allowed when read-only.
    pub fn fill_detail(&mut self, ids: &[SharedString], description: Option<SharedString>, attendees: Vec<Attendee>, rsvp: Rsvp, cx: &mut Context<Self>) {
        for event in self.events.iter_mut().filter(|e| ids.contains(&e.id)) {
            event.description = description.clone();
            event.attendees = attendees.clone();
            event.rsvp = rsvp;
        }
        cx.notify();
    }

    /// Yours to edit and delete: nobody else organizes it.
    pub fn is_own(&self, event: &Event) -> bool {
        event.attendees.iter().find(|a| a.organizer).is_none_or(|o| self.me.iter().any(|m| m.eq_ignore_ascii_case(&o.email)))
    }

    /// Take events out (delete, cancel, remove); returns them for undo.
    pub fn remove_events(&mut self, ids: &[SharedString], cx: &mut Context<Self>) -> Vec<Event> {
        if self.refuse(cx) {
            return Vec::new();
        }
        if self.loaded.is_some() {
            let events = self.events.iter().filter(|e| ids.contains(&e.id)).cloned().collect();
            cx.emit(ScheduleEvent::WriteRequested(WriteIntent::Delete(events)));
            // No local Undo: deletion may already notify guests on the server.
            return Vec::new();
        }
        let (removed, kept): (Vec<Event>, Vec<Event>) = std::mem::take(&mut self.events).into_iter().partition(|e| ids.contains(&e.id));
        self.events = kept;
        if self.selected.as_ref().is_some_and(|s| ids.contains(s)) {
            self.select(None, cx);
        }
        cx.notify();
        removed
    }

    /// Undo of `remove_events`.
    pub fn restore_events(&mut self, events: Vec<Event>, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        if self.loaded.is_some() {
            for event in events {
                cx.emit(ScheduleEvent::WriteRequested(WriteIntent::Save { event, new: false }));
            }
            return;
        }
        self.events.extend(events);
        self.events.sort_by_key(|e| (e.start, std::cmp::Reverse(e.end)));
        cx.notify();
    }

    /// Ids of the whole series an event belongs to (just itself if it's single).
    pub fn series_ids(&self, id: &str) -> Vec<SharedString> {
        match self.event(id).and_then(|e| e.series.clone()) {
            Some(series) => self.events.iter().filter(|e| e.series.as_ref() == Some(&series)).map(|e| e.id.clone()).collect(),
            None => vec![id.to_string().into()],
        }
    }

    /// A copy right after the original (same length), selected. Guests are not re-invited.
    pub fn duplicate(&mut self, id: &str, new_id: SharedString, cx: &mut Context<Self>) {
        let Some(mut copy) = self.event(id).cloned() else { return };
        let length = copy.end - copy.start;
        copy.id = new_id;
        copy.start = copy.end;
        copy.end = copy.start + length;
        copy.series = None;
        copy.recurrence = None;
        copy.rsvp = Rsvp::Accepted;
        copy.attendees.clear();
        self.add_event(copy, cx);
    }

    /// Edit in place (keeps the id, so selections and invitations stay attached).
    pub fn replace_event(&mut self, event: Event, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        if self.loaded.is_some() {
            cx.emit(ScheduleEvent::WriteRequested(WriteIntent::Save { event, new: false }));
            return;
        }
        self.apply_event(event, cx);
    }

    /// The user's answer to an invitation. Local and optimistic; the data source hears about it
    /// once intents exist.
    pub fn set_rsvp(&mut self, id: &str, rsvp: Rsvp, cx: &mut Context<Self>) {
        if self.refuse(cx) {
            return;
        }
        if self.loaded.is_some() {
            if let Some(event) = self.event(id).cloned() {
                cx.emit(ScheduleEvent::WriteRequested(WriteIntent::Rsvp { event, response: rsvp }));
            }
            return;
        }
        if let Some(event) = self.events.iter_mut().find(|e| e.id == id) {
            event.rsvp = rsvp;
            cx.notify();
        }
    }

    /// j / k: the next or previous event in the visible period, in time order.
    pub fn select_relative(&mut self, delta: isize, cx: &mut Context<Self>) {
        let days = self.days();
        let visible: Vec<&Event> = self.events.iter().filter(|e| days.iter().any(|d| e.on(*d))).collect();
        if visible.is_empty() {
            return;
        }
        let current = self.selected.as_ref().and_then(|id| visible.iter().position(|e| e.id == *id));
        let next = match current {
            Some(ix) => (ix as isize + delta).clamp(0, visible.len() as isize - 1) as usize,
            // The first step selects what's on now/next; remaining counted steps
            // move from there, just like repeated single-key navigation.
            None => {
                let now = chrono::Local::now().naive_local();
                let start = visible.iter().position(|e| e.end > now && !e.all_day).unwrap_or(0);
                (start as isize + delta - delta.signum()).clamp(0, visible.len() as isize - 1) as usize
            }
        };
        let id = visible[next].id.clone();
        self.select(Some(id), cx);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_day_is_known_only_inside_a_loaded_window() {
        let day = |d| NaiveDate::from_ymd_opt(2026, 10, d).unwrap();
        let mut schedule = Schedule::new(CalendarData { calendars: Vec::new(), events: Vec::new(), me: Vec::new() }, day(10));
        // fixtures: everything is here
        assert!(schedule.knows(day(1)));
        // served in windows: nothing before the first
        schedule.loaded = Some(Vec::new());
        assert!(!schedule.knows(day(10)));
        schedule.loaded = Some(vec![(day(5), day(12)), (day(20), day(22))]);
        assert!(schedule.knows(day(5)) && schedule.knows(day(11)) && schedule.knows(day(21)));
        assert!(!schedule.knows(day(12)) && !schedule.knows(day(4)) && !schedule.knows(day(15)));
    }
}
