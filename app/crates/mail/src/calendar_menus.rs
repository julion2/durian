//! Right-click in the calendar: what's offered depends on what's under the cursor.
//!
//! - your own event: edit, duplicate, email the guests, copy; delete (cancel, if there are
//!   guests – they'll hear about it; this one or the whole series, if it repeats)
//! - an invitation: answer (the current answer checked), find the invitation mail, email the
//!   organizer, copy; remove from your calendar
//! - empty time: a new event at that quarter hour, a new all-day event, open the day
//! - an empty day (all-day row, month cell): a new all-day event, open the day

use calendar::MenuTarget;
use calendar::detail::when;
use calendar::model::{Event, Rsvp, Schedule, View};
use chrono::{Duration, NaiveDate, NaiveTime};
use gpui::{App, ClipboardItem, Entity, SharedString, WeakEntity};
use ui::{IconName, MenuItem};

use crate::commands::keys_for;
use crate::event_compose::EventSeed;
use crate::*;

fn cmd(label: &str, icon: IconName, action: Box<dyn gpui::Action>, cx: &App) -> MenuItem {
    let keys = keys_for(action.as_ref(), &[keymaps::Context::Calendar], cx);
    MenuItem::action(label.to_string(), icon, keys, action)
}

/// "Postmortem review\nFri, Oct 9 · 14:00 – 15:00 (1h)\nRoom Kepler\nmeet.habric.com/…"
fn details(event: &Event) -> String {
    [Some(event.title.to_string()), Some(when(event)), event.location.as_ref().map(|l| l.to_string()), event.conference.as_ref().map(|c| c.to_string())]
        .into_iter()
        .flatten()
        .collect::<Vec<_>>()
        .join("\n")
}

pub fn build(target: &MenuTarget, schedule: &Entity<Schedule>, workspace: WeakEntity<MailWorkspace>, cx: &App) -> Vec<MenuItem> {
    match target {
        MenuTarget::Event(id) => schedule.read(cx).event(id).cloned().map(|e| event(e, schedule, workspace, cx)).unwrap_or_default(),
        MenuTarget::Slot(at) => {
            let at = *at;
            let label = format!("New event at {}", at.format("%H:%M"));
            let w = workspace.clone();
            let mut items = vec![MenuItem::handler(label, IconName::Calendar, move |window, cx| {
                let _ = w.update(cx, |this, cx| this.open_event_compose(EventSeed::slot(at, at + Duration::hours(1), false), window, cx));
            })];
            items.extend(day(at.date(), schedule, workspace));
            items
        }
        MenuTarget::Day(date) => day(*date, schedule, workspace),
    }
}

/// New all-day event · Open <day>
fn day(date: NaiveDate, schedule: &Entity<Schedule>, workspace: WeakEntity<MailWorkspace>) -> Vec<MenuItem> {
    let midnight = date.and_time(NaiveTime::MIN);
    let schedule = schedule.clone();
    vec![
        MenuItem::handler("New all-day event", IconName::Calendar, move |window, cx| {
            let _ = workspace.update(cx, |this, cx| this.open_event_compose(EventSeed::slot(midnight, midnight + Duration::days(1), true), window, cx));
        }),
        MenuItem::Separator,
        MenuItem::handler(format!("Open {}", date.format("%A, %b %-d")), IconName::ArrowRight, move |_, cx| {
            schedule.update(cx, |s, cx| {
                s.anchor = date;
                s.set_view(View::Day, cx);
            })
        }),
    ]
}

/// What a right-click on an event offers, decided without any UI – so it can be tested.
#[derive(Debug, PartialEq)]
enum Entry {
    Edit,
    Duplicate,
    EmailGuests,
    Copy,
    Separator,
    /// "Delete" / "Cancel event" / "Delete this event" / "Cancel this event"
    Delete(&'static str),
    DeleteSeries,
    Answer(&'static str, Rsvp),
    ShowInvitation,
    EmailOrganizer(String),
    Remove,
}

fn plan(e: &Event, schedule: &Schedule, has_invitation: bool) -> Vec<Entry> {
    let others = e.attendees.iter().filter(|a| !schedule.me.iter().any(|m| m.eq_ignore_ascii_case(&a.email))).count();
    if schedule.is_own(e) {
        let mut entries = vec![Entry::Edit, Entry::Duplicate];
        if others > 0 {
            entries.push(Entry::EmailGuests);
        }
        entries.extend([Entry::Copy, Entry::Separator]);
        // with guests it's a cancellation – they'll hear about it
        match (e.series.is_some(), others > 0) {
            (true, _) if schedule.loaded.is_some() => entries.push(Entry::DeleteSeries),
            (true, guests) => entries.extend([Entry::Delete(if guests { "Cancel this event" } else { "Delete this event" }), Entry::DeleteSeries]),
            (false, guests) => entries.push(Entry::Delete(if guests { "Cancel event" } else { "Delete" })),
        }
        return entries;
    }
    // an invitation: answer first – it's what you came for
    let mut entries = vec![Entry::Answer("Going", Rsvp::Accepted), Entry::Answer("Maybe", Rsvp::Tentative), Entry::Answer("Not going", Rsvp::Declined), Entry::Separator];
    if has_invitation {
        entries.push(Entry::ShowInvitation);
    }
    if let Some(organizer) = e.attendees.iter().find(|a| a.organizer) {
        entries.push(Entry::EmailOrganizer(organizer.name.split_whitespace().next().unwrap_or("organizer").to_string()));
    }
    entries.extend([Entry::Copy, Entry::Separator]);
    if e.series.is_some() && schedule.loaded.is_some() {
        entries.push(Entry::DeleteSeries);
    } else {
        entries.push(Entry::Remove);
    }
    entries
}

fn event(e: Event, schedule: &Entity<Schedule>, workspace: WeakEntity<MailWorkspace>, cx: &App) -> Vec<MenuItem> {
    let cmd = |label: &str, icon, action: Box<dyn gpui::Action>| cmd(label, icon, action, cx);
    let has_invitation = workspace.upgrade().is_some_and(|ws| ws.read(cx).invitation_thread(&e.id, cx).is_some());
    let me = schedule.read(cx).me.clone();
    let others: Vec<SharedString> = e.attendees.iter().filter(|a| !me.iter().any(|m| m.eq_ignore_ascii_case(&a.email))).map(|a| a.email.clone()).collect();
    plan(&e, schedule.read(cx), has_invitation)
        .into_iter()
        .map(|entry| match entry {
            Entry::Edit => cmd("Edit…", IconName::Compose, Box::new(calendar::EditEvent)),
            Entry::Duplicate => cmd("Duplicate", IconName::Plus, Box::new(calendar::DuplicateEvent)),
            Entry::EmailGuests => {
                let (w, to, subject) = (workspace.clone(), others.clone(), e.title.clone());
                MenuItem::handler("Email guests…", IconName::MailUsers, move |window, cx| {
                    let _ = w.update(cx, |this, cx| this.email_people(to.clone(), subject.clone(), window, cx));
                })
            }
            Entry::Copy => {
                let text = details(&e);
                MenuItem::handler("Copy details", IconName::MailEmail, move |_, cx| cx.write_to_clipboard(ClipboardItem::new_string(text.clone())))
            }
            Entry::Separator => MenuItem::Separator,
            Entry::Delete(label) => cmd(label, IconName::MailTrash, Box::new(calendar::DeleteEvent)).destructive(),
            Entry::DeleteSeries => {
                let (w, ids) = (workspace.clone(), schedule.read(cx).series_ids(&e.id));
                MenuItem::handler("Delete entire series…", IconName::MailTrash, move |window, cx| {
                    let _ = w.update(cx, |this, cx| this.request_delete(ids.clone(), window, cx));
                })
                .destructive()
            }
            Entry::Answer(label, rsvp) => {
                let (schedule, id) = (schedule.clone(), e.id.clone());
                MenuItem::choice(label, e.rsvp == rsvp, move |_, cx| schedule.update(cx, |s, cx| s.set_rsvp(&id, rsvp, cx)))
            }
            Entry::ShowInvitation => {
                let (w, id) = (workspace.clone(), e.id.clone());
                MenuItem::handler("Show invitation", IconName::MailInbox, move |window, cx| {
                    let _ = w.update(cx, |this, cx| this.show_invitation(&id, window, cx));
                })
            }
            Entry::EmailOrganizer(first) => {
                let organizer = e.attendees.iter().find(|a| a.organizer).map(|a| a.email.clone()).unwrap_or_default();
                let (w, subject) = (workspace.clone(), e.title.clone());
                MenuItem::handler(format!("Email {first}…"), IconName::MailReply, move |window, cx| {
                    let _ = w.update(cx, |this, cx| this.email_people(vec![organizer.clone()], subject.clone(), window, cx));
                })
            }
            Entry::Remove => cmd("Remove from calendar", IconName::MailTrash, Box::new(calendar::DeleteEvent)).destructive(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use calendar::model::{Attendee, CalendarData};
    use chrono::NaiveDate;

    fn person(name: &str, email: &str, organizer: bool) -> Attendee {
        Attendee { name: name.into(), email: email.into(), rsvp: Rsvp::Accepted, organizer, avatar: None }
    }

    fn event(attendees: Vec<Attendee>, series: bool) -> Event {
        let start = NaiveDate::from_ymd_opt(2026, 10, 9).unwrap().and_hms_opt(14, 0, 0).unwrap();
        Event {
            id: "e".into(),
            calendar: "c".into(),
            title: "Review".into(),
            start,
            end: start + Duration::hours(1),
            all_day: false,
            instants: None,
            time_zone: None,
            end_time_zone: None,
            floating: false,
            location: None,
            conference: None,
            description: None,
            attendees,
            rsvp: Rsvp::NeedsAction,
            is_organizer: false,
            recurrence: None,
            occurrence_source: None,
            series: series.then(|| "s".into()),
        }
    }

    fn schedule() -> Schedule {
        let me = vec!["julian@habric.com".into(), "julian@home.example".into()];
        Schedule::new(CalendarData { calendars: Vec::new(), events: Vec::new(), me }, NaiveDate::from_ymd_opt(2026, 10, 9).unwrap())
    }

    #[test]
    fn own_events_are_edited_and_deleted_or_cancelled() {
        let s = schedule();
        let alone = plan(&event(Vec::new(), false), &s, false);
        assert_eq!(alone, [Entry::Edit, Entry::Duplicate, Entry::Copy, Entry::Separator, Entry::Delete("Delete")]);

        let with_guests = event(vec![person("Julian Schenker", "julian@habric.com", true), person("Mara Lindqvist", "mara@habric.com", false)], false);
        let entries = plan(&with_guests, &s, false);
        assert!(entries.contains(&Entry::EmailGuests) && entries.contains(&Entry::Delete("Cancel event")));

        let series = plan(&event(Vec::new(), true), &s, false);
        assert!(series.ends_with(&[Entry::Delete("Delete this event"), Entry::DeleteSeries]));

        let mut secondary = event(vec![person("Secondary", "secondary@group.calendar.google.com", true)], false);
        secondary.is_organizer = true;
        assert!(plan(&secondary, &s, false).contains(&Entry::Edit), "explicit organizer authority allows edits without an account-email match");
    }

    #[test]
    fn invitations_are_answered_not_edited() {
        let s = schedule();
        let mut invite = event(vec![person("Tobias Brandt", "tobias@habric.com", true), person("Julian Schenker", "julian@habric.com", false)], true);
        invite.rsvp = Rsvp::Accepted;
        let entries = plan(&invite, &s, true);
        assert_eq!(&entries[..3], &[Entry::Answer("Going", Rsvp::Accepted), Entry::Answer("Maybe", Rsvp::Tentative), Entry::Answer("Not going", Rsvp::Declined)]);
        assert!(entries.contains(&Entry::ShowInvitation) && entries.contains(&Entry::EmailOrganizer("Tobias".into())));
        assert!(!entries.contains(&Entry::Edit) && entries.last() == Some(&Entry::Remove), "no edit, no series delete – just leave it");
        // organizer matching ignores case and covers aliases
        let own_alias = event(vec![person("Julian", "JULIAN@home.example", true)], false);
        assert!(plan(&own_alias, &s, false).contains(&Entry::Edit));
    }
}
