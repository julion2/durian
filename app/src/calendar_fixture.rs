//! Demo data for the calendar: `data/calendar.json` with days relative to today (so the demo is
//! always current), weekday series expanded into occurrences, and attendee pictures taken from
//! the mail contacts. A real backend hands `CalendarData` over the same way.

use std::collections::HashMap;

use calendar::model::{Attendee, CalendarData, CalendarInfo, Event, Rsvp};
use chrono::{Datelike, Duration, NaiveDate, NaiveTime};
use gpui::SharedString;
use serde::Deserialize;

#[derive(Deserialize)]
struct Fixture {
    calendars: Vec<CalendarInfo>,
    events: Vec<RawEvent>,
}

#[derive(Deserialize)]
struct RawEvent {
    id: SharedString,
    calendar: SharedString,
    title: SharedString,
    /// days from today
    day: i64,
    start: Option<String>,
    end: Option<String>,
    #[serde(default)]
    all_day: bool,
    /// all-day length
    days: Option<i64>,
    /// "weekdays": one occurrence per weekday from `day` through `until_day`
    repeat: Option<String>,
    until_day: Option<i64>,
    recurrence: Option<SharedString>,
    location: Option<SharedString>,
    conference: Option<SharedString>,
    description: Option<SharedString>,
    #[serde(default)]
    attendees: Vec<Attendee>,
    rsvp: Option<Rsvp>,
}

fn clock(s: &Option<String>) -> NaiveTime {
    s.as_deref().and_then(|t| NaiveTime::parse_from_str(t, "%H:%M").ok()).unwrap_or(NaiveTime::MIN)
}

pub fn load(today: NaiveDate, avatars: &HashMap<String, SharedString>, me: Vec<SharedString>) -> anyhow::Result<CalendarData> {
    let fixture: Fixture = serde_json::from_slice(include_bytes!("../data/calendar.json"))?;
    let mut events = Vec::new();
    for raw in fixture.events {
        let days: Vec<i64> = match raw.repeat.as_deref() {
            Some("weekdays") => (raw.day..=raw.until_day.unwrap_or(raw.day))
                .filter(|d| (today + Duration::days(*d)).weekday().num_days_from_monday() < 5)
                .collect(),
            _ => vec![raw.day],
        };
        let attendees: Vec<Attendee> = raw
            .attendees
            .iter()
            .cloned()
            .map(|mut a| {
                a.avatar = avatars.get(a.email.as_ref()).cloned();
                a
            })
            .collect();
        for day in days {
            let date = today + Duration::days(day);
            let (start, end) = if raw.all_day {
                (date.and_time(NaiveTime::MIN), (date + Duration::days(raw.days.unwrap_or(1))).and_time(NaiveTime::MIN))
            } else {
                (date.and_time(clock(&raw.start)), date.and_time(clock(&raw.end)))
            };
            let id = if raw.repeat.is_some() { format!("{}-{}", raw.id, date).into() } else { raw.id.clone() };
            events.push(Event {
                id,
                calendar: raw.calendar.clone(),
                title: raw.title.clone(),
                start,
                end,
                all_day: raw.all_day,
                instants: if raw.all_day { None } else { calendar::model::EventInstants::local(start, end).ok() },
                time_zone: None,
                end_time_zone: None,
                floating: false,
                location: raw.location.clone(),
                conference: raw.conference.clone(),
                description: raw.description.clone(),
                attendees: attendees.clone(),
                rsvp: raw.rsvp.unwrap_or(Rsvp::Accepted),
                recurrence: raw.recurrence.clone(),
                series: raw.repeat.is_some().then(|| raw.id.clone()),
            });
        }
    }
    Ok(CalendarData { calendars: fixture.calendars, events, me })
}
