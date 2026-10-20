//! Calendar write intentions: commit to the Go vdir, then sync only that event.
//! Network failures never turn into local-only success or an automatic retry.

use std::collections::{HashMap, HashSet, VecDeque};

use calendar::model::{CalendarInfo, Event, EventInstants, Rsvp, Schedule, ScheduleEvent, WriteIntent};
use chrono::NaiveDateTime;
use gpui::{Context, Entity, EventEmitter, SharedString, Subscription, Task};
use serde::Serialize;
use serde_json::json;

use super::{Client, EventRef, Live, api, calendar_event, event_detail};
use super::client::{Error, Result};

#[derive(Serialize)]
pub struct EventWrite {
    pub account: String,
    pub calendar: String,
    /// A stable client-generated UID also makes an uncertain create retry an upsert.
    pub uid: String,
    pub subject: String,
    pub start: String,
    pub end: String,
    pub all_day: bool,
    /// Explicit metadata also permits transitions out of floating/zoned semantics.
    /// Empty end zone inherits the start zone; empty start zone means UTC.
    pub time_zone: String,
    pub end_time_zone: String,
    pub floating: bool,
    pub location: String,
    pub description: String,
    pub attendees: Vec<String>,
    pub replace_attendees: bool,
}

impl Client {
    pub fn put_event(&self, write: &EventWrite) -> Result<api::CalendarEvent> {
        let wire = self.request_json::<api::CalendarEventResponse>("PUT", "/calendars/event", &[], write)?.event;
        validate_event(&write.account, &write.calendar, &write.uid, wire)
    }

    pub fn delete_event(&self, at: &EventRef) -> Result<()> {
        self.request_json::<serde_json::Value>("DELETE", "/calendars/event",
            &[("account", &at.account), ("calendar", &at.calendar), ("uid", &at.uid)], &json!({}))?;
        Ok(())
    }

    pub fn rsvp_event(&self, at: &EventRef, response: Rsvp) -> Result<api::CalendarEvent> {
        let response = match response {
            Rsvp::Accepted => "accept",
            Rsvp::Tentative => "tentative",
            Rsvp::Declined => "decline",
            Rsvp::NeedsAction => return Err(invalid("Choose accept, tentative or decline.")),
        };
        let wire = self.request_json::<api::CalendarEventResponse>("POST", "/calendars/rsvp", &[],
            &json!({"account": at.account.as_ref(), "calendar": at.calendar, "uid": at.uid, "response": response}))?.event;
        validate_event(&at.account, &at.calendar, &at.uid, wire)
    }

    pub fn sync_event(&self, at: &EventRef, operation: &str) -> Result<()> {
        if at.account == "local" { return Ok(()) }
        self.request_json::<serde_json::Value>("POST", "/calendars/sync/event", &[],
            &json!({"account": at.account.as_ref(), "calendar": at.calendar, "uid": at.uid, "operation": operation}))?;
        Ok(())
    }
}

fn invalid(message: &str) -> Error {
    Error::Decode(message.to_string())
}

fn validate_event(account: &str, calendar: &str, uid: &str, wire: api::CalendarEvent) -> Result<api::CalendarEvent> {
    if wire.uid != uid || wire.calendar != calendar || calendar_event(&account.to_string().into(), &wire).is_none() {
        return Err(invalid("The server returned a different or invalid event; the change may have been saved. Refresh before retrying."));
    }
    Ok(wire)
}

fn timestamp(time: NaiveDateTime, all_day: bool, floating: bool) -> Result<String> {
    if all_day {
        return Ok(time.date().and_hms_opt(0, 0, 0).unwrap().and_utc().to_rfc3339());
    }
    if floating {
        return Ok(time.and_utc().to_rfc3339());
    }
    // Never silently move a time in a daylight-saving gap or choose one side of
    // an ambiguous clock hour. The user can choose an unambiguous time instead.
    EventInstants::resolve_local(time).map(|t| t.to_rfc3339()).map_err(invalid)
}

fn reference(event: &Event) -> Result<EventRef> {
    EventRef::from_occurrence(&event.id).ok_or_else(|| invalid("This event has no server identity. Refresh the calendar."))
}

pub fn event_write(event: &Event, new: bool, calendars: &[CalendarInfo]) -> Result<EventWrite> {
    let target = calendars.iter().find(|c| c.id == event.calendar)
        .ok_or_else(|| invalid("Choose an available calendar before saving."))?;
    let uid = if new { event.id.to_string() } else {
        let at = reference(event)?;
        if at.account != target.account || at.calendar != target.name.as_ref() {
            return Err(invalid("Moving an existing event between calendars is not supported."));
        }
        at.uid
    };
    if event.duration() <= chrono::Duration::zero() {
        return Err(invalid("The event must end after it starts."));
    }
    let attendees = event.attendees.iter().filter(|a| !a.organizer).map(|a| a.email.to_string()).collect::<Vec<_>>();
    if target.account == "local" && !attendees.is_empty() {
        return Err(invalid("Local calendars cannot send invitations. Choose an account calendar or remove the guests."));
    }
    let instants = event.instants.filter(|_| !event.all_day && !event.floating);
    Ok(EventWrite {
        account: target.account.to_string(), calendar: target.name.to_string(), uid,
        subject: event.title.to_string(),
        start: match instants { Some(t) => t.start.to_rfc3339(), None => timestamp(event.start, event.all_day, event.floating)? },
        end: match instants { Some(t) => t.end.to_rfc3339(), None => timestamp(event.end, event.all_day, event.floating)? },
        time_zone: if event.all_day { String::new() } else { event.time_zone.as_ref().map(ToString::to_string).unwrap_or_default() },
        end_time_zone: if event.all_day { String::new() } else { event.end_time_zone.as_ref().map(ToString::to_string).unwrap_or_default() },
        floating: !event.all_day && event.floating,
        all_day: event.all_day, location: event.location.as_ref().or(event.conference.as_ref()).map(ToString::to_string).unwrap_or_default(),
        description: event.description.as_ref().map(ToString::to_string).unwrap_or_default(), attendees, replace_attendees: true,
    })
}

pub enum WriteEvent {
    Notice(SharedString),
}

#[derive(Clone)]
struct SyncRequest {
    at: EventRef,
    operation: &'static str,
}

#[derive(Clone)]
enum Recovery {
    Sync(SyncRequest),
    /// No local ACK: establish exact absence before offering the scoped sync.
    Delete(EventRef),
}

impl Recovery {
    fn at(&self) -> &EventRef {
        match self { Self::Sync(request) => &request.at, Self::Delete(at) => at }
    }
}

enum WorkKind {
    Change { intent: WriteIntent, calendars: Vec<CalendarInfo> },
    Recover(Recovery),
}

struct Work {
    kind: WorkKind,
    keys: Vec<EventRef>,
}

#[derive(Default)]
struct Outcome {
    event: Option<(SharedString, api::CalendarEvent)>,
    removed: Vec<EventRef>,
    resolved: Vec<EventRef>,
    recovery: Vec<Recovery>,
    notice: Option<SharedString>,
    error: Option<Error>,
}

/// One active request sequence and a bounded queue. Two changes to the same
/// server event cannot overlap, even when they name different occurrences.
pub struct CalendarWrites {
    live: Entity<Live>,
    schedule: Entity<Schedule>,
    queue: VecDeque<Work>,
    pending: HashSet<EventRef>,
    recovery: HashMap<EventRef, Recovery>,
    task: Option<Task<()>>,
    edit_task: Option<Task<()>>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<WriteEvent> for CalendarWrites {}

impl CalendarWrites {
    pub fn new(live: Entity<Live>, schedule: Entity<Schedule>, cx: &mut Context<Self>) -> Self {
        let subscription = cx.subscribe(&schedule, |this, _, event, cx| match event {
            ScheduleEvent::WriteRequested(intent) => this.enqueue(intent.clone(), cx),
            ScheduleEvent::EditRequested(id) => this.edit(id.clone(), cx),
            _ => {}
        });
        let connected = cx.observe(&live, |this, _, cx| this.start(cx));
        Self { live, schedule, queue: VecDeque::new(), pending: HashSet::new(), recovery: HashMap::new(), task: None, edit_task: None, _subscriptions: vec![subscription, connected] }
    }

    pub fn recovery_count(&self) -> usize { self.recovery.len() }

    /// Opening any editor supersedes a pending detail lookup. Dropping its GPUI
    /// result task prevents a late response from opening another editor.
    pub fn cancel_edit(&mut self) { self.edit_task = None; }

    /// Explicit recovery reconciles uncertain deletions before scoped sync.
    /// It never replays PUT/DELETE/RSVP or treats a lost ACK as a rejected write.
    pub fn recover(&mut self, cx: &mut Context<Self>) {
        if !self.live.read(cx).backend_available() { return; }
        for request in self.recovery.values() {
            if self.queue.len() >= 32 { break; }
            if self.pending.insert(request.at().clone()) {
                self.queue.push_back(Work { kind: WorkKind::Recover(request.clone()), keys: vec![request.at().clone()] });
            }
        }
        self.start(cx);
    }

    fn report(&self, intent: &WriteIntent, result: std::result::Result<SharedString, SharedString>, cx: &mut Context<Self>) {
        let id = match intent {
            WriteIntent::Save { event, .. } | WriteIntent::Rsvp { event, .. } => event.id.clone(),
            WriteIntent::Delete(events) => events.first().map(|e| e.id.clone()).unwrap_or_default(),
        };
        if let Err(error) = &result { cx.emit(WriteEvent::Notice(error.clone())); }
        self.schedule.update(cx, |_, cx| cx.emit(ScheduleEvent::WriteFinished { id, result }));
    }

    fn enqueue(&mut self, intent: WriteIntent, cx: &mut Context<Self>) {
        if !self.live.read(cx).backend_available() {
            self.report(&intent, Err("The server is reconnecting. Your event was not submitted; try saving after it reconnects.".into()), cx);
            return;
        }
        let calendars = self.schedule.read(cx).calendars.clone();
        let keys = match keys(&intent, &calendars) {
            Ok(keys) if !keys.is_empty() => keys,
            Ok(_) => return,
            Err(error) => { self.report(&intent, Err(error.to_string().into()), cx); return; }
        };
        if self.queue.len() >= 32 || keys.iter().any(|key| self.pending.contains(key) || self.recovery.contains_key(key)) {
            self.report(&intent, Err("A calendar change is still pending. Wait for it, or use Recover calendar changes before editing this event again.".into()), cx);
            return;
        }
        self.pending.extend(keys.iter().cloned());
        self.queue.push_back(Work { kind: WorkKind::Change { intent, calendars }, keys });
        self.start(cx);
    }

    fn start(&mut self, cx: &mut Context<Self>) {
        if self.task.is_some() || !self.live.read(cx).backend_available() { return; }
        let Some(work) = self.queue.pop_front() else { return };
        let client = self.live.read(cx).client();
        let executor = cx.background_executor().clone();
        self.task = Some(cx.spawn(async move |this, cx| {
            let (work, outcome) = executor.spawn(async move {
                let outcome = execute(&client, &work);
                (work, outcome)
            }).await;
            this.update(cx, |this, cx| {
                this.task = None;
                for key in &work.keys { this.pending.remove(key); }
                for at in outcome.resolved { this.recovery.remove(&at); }
                if !outcome.recovery.is_empty() {
                    cx.emit(WriteEvent::Notice("Calendar changes are unconfirmed. Use Recover calendar changes before assuming deletion or guest notification succeeded.".into()));
                }
                for request in outcome.recovery { this.recovery.insert(request.at().clone(), request); }
                if let Some(notice) = outcome.notice { cx.emit(WriteEvent::Notice(notice)); }
                let mut saved = SharedString::default();
                let me = this.schedule.read(cx).me.clone();
                this.schedule.update(cx, |schedule, cx| {
                    schedule.events.retain(|event| !reference(event).is_ok_and(|at| outcome.removed.contains(&at)));
                    if let Some((account, wire)) = &outcome.event {
                        if let Some((mut event, _)) = calendar_event(account, wire) {
                            (event.description, event.attendees, event.rsvp, event.is_organizer) = event_detail(wire, &me, event.rsvp);
                            saved = event.id.clone();
                            schedule.apply_event(event, cx);
                        }
                    }
                    if schedule.selected.as_ref().is_some_and(|id| schedule.event(id).is_none()) {
                        schedule.select(None, cx);
                    }
                    cx.notify();
                });
                match &work.kind {
                    WorkKind::Change { intent, .. } => {
                        let result = match outcome.error {
                            Some(error) => Err(format!("Calendar request failed: {error}. Earlier changes may already be saved. Use Recover calendar changes if shown; your editor was kept.").into()),
                            None => Ok(saved),
                        };
                        this.report(intent, result, cx);
                    }
                    WorkKind::Recover(_) => if let Some(error) = outcome.error {
                        cx.emit(WriteEvent::Notice(format!("Calendar recovery failed: {error}. The pending change was kept.").into()));
                    },
                }
                // Refresh after both success and uncertain transport failure. Never
                // replay an intent automatically: a lost response may hide a commit.
                this.live.update(cx, |live, cx| live.refresh_calendar(cx));
                cx.notify();
                this.start(cx);
            }).ok();
        }));
    }

    fn edit(&mut self, id: SharedString, cx: &mut Context<Self>) {
        let Some(event) = self.schedule.read(cx).event(&id).cloned() else { return };
        let Ok(at) = reference(&event) else { return };
        if !self.live.read(cx).backend_available() { return; }
        let client = self.live.read(cx).client();
        let executor = cx.background_executor().clone();
        self.edit_task = Some(cx.spawn(async move |this, cx| {
            let result = executor.spawn(async move {
                let wire = client.event(&at.account, &at.calendar, &at.uid)?;
                validate_event(&at.account, &at.calendar, &at.uid, wire).map(|wire| (at, wire))
            }).await;
            this.update(cx, |this, cx| {
                this.edit_task = None;
                if this.schedule.read(cx).selected.as_ref() != Some(&id) { return; }
                match result {
                    Ok((at, wire)) => {
                        let Some((mut master, _)) = calendar_event(&at.account, &wire) else { return };
                        let schedule = this.schedule.read(cx);
                        (master.description, master.attendees, master.rsvp, master.is_organizer) = event_detail(&wire, &schedule.me, master.rsvp);
                        if !schedule.is_own(&master) {
                            cx.emit(WriteEvent::Notice("Only the organizer can edit this meeting.".into()));
                            return;
                        }
                        // The detail endpoint resolves the stored master, not a visible
                        // occurrence. Editing its times cannot shift a series by months.
                        this.schedule.update(cx, |_, cx| cx.emit(ScheduleEvent::EditReady(master)));
                    }
                    Err(_) => cx.emit(WriteEvent::Notice("Could not load complete event details. Try editing again after refresh.".into())),
                }
            }).ok();
        }));
    }
}

fn keys(intent: &WriteIntent, calendars: &[CalendarInfo]) -> Result<Vec<EventRef>> {
    let keys = match intent {
        WriteIntent::Save { event, new } => {
            let write = event_write(event, *new, calendars)?;
            vec![EventRef { account: write.account.into(), calendar: write.calendar, uid: write.uid }]
        }
        WriteIntent::Rsvp { event, .. } => vec![reference(event)?],
        WriteIntent::Delete(events) => events.iter().map(reference).collect::<Result<Vec<_>>>()?,
    };
    let mut unique = HashSet::new();
    Ok(keys.into_iter().filter(|key| unique.insert(key.clone())).collect())
}

fn execute(client: &Client, work: &Work) -> Outcome {
    let mut outcome = Outcome::default();
    let result = (|| -> Result<()> {
        match &work.kind {
            WorkKind::Recover(Recovery::Sync(request)) => sync(client, request.clone(), &mut outcome),
            WorkKind::Recover(Recovery::Delete(at)) => {
                // A 404 can also mean the account/calendar disappeared. Verify the
                // scope first, and let Go's delete-only sync reject conflicting plans.
                if !client.calendars(&at.account)?.iter().any(|calendar| calendar.name == at.calendar) {
                    return Err(invalid("The original calendar is unavailable. Restore access before recovering this deletion."));
                }
                match client.event(&at.account, &at.calendar, &at.uid) {
                    Ok(event) => {
                        validate_event(&at.account, &at.calendar, &at.uid, event)?;
                        outcome.resolved.push(at.clone());
                        outcome.notice = Some("The event still exists. No deletion was repeated; review the refreshed event before deleting again.".into());
                    }
                    Err(Error::NotFound(_)) => {
                        outcome.removed.push(at.clone());
                        sync(client, SyncRequest { at: at.clone(), operation: "delete" }, &mut outcome);
                    }
                    Err(error) => return Err(error),
                }
            }
            WorkKind::Change { intent, calendars } => match intent {
                WriteIntent::Save { event, new } => {
                    let wire = client.put_event(&event_write(event, *new, calendars)?)?;
                    let at = &work.keys[0];
                    outcome.event = Some((at.account.clone(), wire));
                    sync(client, SyncRequest { at: at.clone(), operation: "save" }, &mut outcome);
                }
                WriteIntent::Rsvp { response, .. } => {
                    let at = &work.keys[0];
                    let wire = client.rsvp_event(at, *response)?;
                    outcome.event = Some((at.account.clone(), wire));
                    sync(client, SyncRequest { at: at.clone(), operation: "rsvp" }, &mut outcome);
                }
                WriteIntent::Delete(_) => {
                    for at in &work.keys {
                        if let Err(error) = client.delete_event(at) {
                            // Even a 5xx/invalid response may follow the rename. A
                            // definite 4xx rejection does not manufacture a retry.
                            if matches!(&error, Error::Unreachable(_) | Error::Decode(_))
                                || matches!(&error, Error::Server { status, .. } if *status >= 500 || *status < 300) {
                                outcome.recovery.push(Recovery::Delete(at.clone()));
                            }
                            return Err(error);
                        }
                        outcome.removed.push(at.clone());
                        sync(client, SyncRequest { at: at.clone(), operation: "delete" }, &mut outcome);
                    }
                }
            }
        }
        Ok(())
    })();
    // Preserve earlier acknowledged deletions and pending syncs if a later
    // request fails; a multi-event command is not an atomic server transaction.
    outcome.error = result.err();
    outcome
}

fn sync(client: &Client, request: SyncRequest, outcome: &mut Outcome) {
    match client.sync_event(&request.at, request.operation) {
        Ok(()) => outcome.resolved.push(request.at),
        Err(_) => outcome.recovery.push(Recovery::Sync(request)),
    }
}
