//! durian as a data source: the HTTP API's read side mapped onto the UI's model.
//!
//! Read-only for now – [`client::Client`] only has GET endpoints. Writes (tags, send, drafts,
//! calendar) come later as intents. Set `MAIL_BACKEND=http://127.0.0.1:9723` (and
//! `MAIL_TOKEN` unless the server runs with `--no-auth`) to use it.
//!
//! - [`client`]: blocking HTTP, typed errors
//! - [`api`]: the wire types
//! - [`translate`]: the UI's query language → durian's
//! - [`live`]: the runtime – searches, paging, thread fetches, all off the UI thread
//! - [`cache`]: full threads already downloaded
//! - this module: wire types → the UI's model

pub mod api;
pub mod cache;
pub mod client;
pub mod live;
pub mod profiles;
pub mod translate;

use calendar::model::{Attendee, Event as CalEvent, Rsvp};
use chrono::{DateTime, Local, NaiveDate, NaiveDateTime, TimeZone};
use gpui::{Hsla, SharedString};

use crate::model::{Attachment, Contact, Message, RemotePart, Thread, ThreadItem};
pub use client::{Client, Error};
pub use live::Live;

/// Tags durian uses for state or sets automatically – shown as read/star/folders (or not at
/// all), not as labels in the sidebar. `list`, `newsletter`, `notification` stay labels.
pub const SYSTEM_TAGS: &[&str] = &["inbox", "unread", "flagged", "replied", "archive", "trash", "sent", "draft", "spam", "cal", "signed", "to-me", "cc-only", "ephemeral"];

/// A subject for display: without the reply/forward prefixes mail clients stack up
/// ("Re: AW: WG: …", in the languages durian's users write), "(no subject)" if nothing's left.
pub fn display_subject(subject: &str) -> SharedString {
    const PREFIXES: &[&str] = &["re", "aw", "antw", "wg", "fw", "fwd", "sv", "vs", "tr", "rv", "réf"];
    let mut rest = subject.trim();
    loop {
        let Some((head, tail)) = rest.split_once(':') else { break };
        // "Re[2]:" counts too
        let word = head.trim().split('[').next().unwrap_or("").to_lowercase();
        if !PREFIXES.contains(&word.as_str()) {
            break;
        }
        rest = tail.trim_start();
    }
    if rest.is_empty() { "(no subject)".into() } else { rest.to_string().into() }
}

/// A stable, pleasant color per name (tags, accounts, calendars without one).
pub fn hashed_color(name: &str) -> Hsla {
    let hue = name.bytes().fold(2166136261u32, |h, b| (h ^ b as u32).wrapping_mul(16777619)) % 360;
    ui::theme::lch(62., 48., hue as f32)
}

/// "Mara Lindqvist <mara@habric.com>" / "mara@habric.com" / "\"Doe, Jane\" <j@x>"
pub fn parse_address(raw: &str) -> Contact {
    let raw = raw.trim();
    let (name, email) = match (raw.rfind('<'), raw.rfind('>')) {
        (Some(a), Some(b)) if a < b => (raw[..a].trim().trim_matches('"').trim(), raw[a + 1..b].trim()),
        _ => ("", raw),
    };
    let name = if name.is_empty() { email.split('@').next().unwrap_or(email) } else { name };
    Contact { name: name.to_string().into(), email: email.to_string().into(), avatar: None }
}

/// Comma-separated address list, commas inside quotes or <> kept.
pub fn split_addresses(raw: &str) -> Vec<Contact> {
    let (mut out, mut current, mut quoted, mut angle) = (Vec::new(), String::new(), false, false);
    for c in raw.chars() {
        match c {
            '"' => quoted = !quoted,
            '<' => angle = true,
            '>' => angle = false,
            ',' if !quoted && !angle => {
                if !current.trim().is_empty() {
                    out.push(parse_address(&current));
                }
                current.clear();
                continue;
            }
            _ => {}
        }
        current.push(c);
    }
    if !current.trim().is_empty() {
        out.push(parse_address(&current));
    }
    out
}

/// "now" / "8m" / "2h" / "3d" / "5w" – the UI's age format (`query::ago_hours` reads it back).
fn ago(timestamp: i64, now: i64) -> SharedString {
    let minutes = (now - timestamp).max(0) / 60;
    match minutes {
        0 => "now".into(),
        m if m < 60 => format!("{m}m").into(),
        m if m < 60 * 24 => format!("{}h", m / 60).into(),
        m if m < 60 * 24 * 7 => format!("{}d", m / (60 * 24)).into(),
        m => format!("{}w", m / (60 * 24 * 7)).into(),
    }
}

fn at(timestamp: i64) -> SharedString {
    Local.timestamp_opt(timestamp, 0).single().map_or_else(SharedString::default, |t| t.format("%a, %b %-d · %H:%M").to_string().into())
}

fn size(bytes: i64) -> SharedString {
    match bytes {
        b if b >= 1 << 20 => format!("{:.1} MB", b as f64 / (1 << 20) as f64).into(),
        b if b >= 1 << 10 => format!("{} KB", b >> 10).into(),
        b => format!("{b} B").into(),
    }
}

/// Largest image inlined into the HTML; a bigger one is listed as an attachment instead.
const INLINE_MAX: i64 = 5 << 20;

/// The content id a part is referenced by in HTML (`cid:…`), without the angle brackets.
fn content_id(a: &api::AttachmentInfo) -> Option<&str> {
    a.content_id.as_deref().map(|id| id.trim().trim_matches(['<', '>'])).filter(|id| !id.is_empty())
}

fn referenced(a: &api::AttachmentInfo, html: &str) -> bool {
    content_id(a).is_some_and(|id| html.contains(&format!("cid:{id}")))
}

/// Is this part an attachment as the user means it? Decided from the message alone, so the
/// list doesn't change while images load:
/// - a signed mail's signature: no
/// - referenced by the HTML (`cid:`): shown in place – listed only if too big to inline
/// - an inline image with a content id that nothing references: it belonged to the quoted
///   original or signature the server stripped (Outlook re-attaches those): no
/// - anything else (real attachments, inline parts without an id): yes
///
/// Without HTML (search previews), an inline image with a content id counts as embedded.
fn listed(a: &api::AttachmentInfo, html: Option<&str>) -> bool {
    let kind = a.content_type.to_ascii_lowercase();
    if matches!(kind.as_str(), "application/pkcs7-signature" | "application/x-pkcs7-signature" | "application/pgp-signature") {
        return false;
    }
    if content_id(a).is_none() {
        return true;
    }
    let inline_image = a.disposition.as_deref() == Some("inline") && kind.starts_with("image/");
    match html {
        Some(html) if referenced(a, html) => a.size > INLINE_MAX,
        _ => !inline_image,
    }
}

/// The parts a message's HTML shows in place, to fetch and inline (each small enough).
pub fn embedded_parts(m: &api::ThreadMessage) -> Vec<&api::AttachmentInfo> {
    let Some(html) = m.html.as_deref() else { return Vec::new() };
    m.attachments.iter().filter(|a| referenced(a, html) && a.size <= INLINE_MAX).collect()
}

/// How mail HTML is shown: durian's `load_remote_images` (config.pkl [settings], served at
/// `/settings`); a server that doesn't serve it gets the default, off.
pub fn html_policy(settings: &api::Settings) -> crate::html_doc::Policy {
    crate::html_doc::Policy { remote_images: settings.load_remote_images }
}

/// The message's HTML as the client shows it (`html_doc::prepare`), with fetched images in
/// place of their `cid:` references – as `data:` URIs, the renderer has no network. Parts
/// missing from `images` show nothing. None: no HTML, or too large to show as HTML.
pub fn prepared_html(m: &api::ThreadMessage, images: &[(&api::AttachmentInfo, Vec<u8>)], policy: crate::html_doc::Policy) -> Option<String> {
    use base64::Engine;
    let html = m.html.as_deref().filter(|h| !h.trim().is_empty())?;
    let sources = images
        .iter()
        .filter_map(|(part, bytes)| {
            let kind = if part.content_type.starts_with("image/") { part.content_type.as_str() } else { "application/octet-stream" };
            Some((content_id(part)?.to_string(), format!("data:{kind};base64,{}", base64::engine::general_purpose::STANDARD.encode(bytes))))
        })
        .collect();
    crate::html_doc::prepare(html, &sources, policy).ok()
}

/// A text/plain body as shown: Unix line breaks, and without Outlook's `[cid:image001.png@…]`
/// placeholders for images the text part can't show.
fn text_body(body: &str) -> String {
    let body = body.replace("\r\n", "\n").replace('\r', "\n");
    let mut out = String::with_capacity(body.len());
    let mut rest = body.as_str();
    while let Some(start) = rest.find("[cid:") {
        let Some(len) = rest[start..].find(']') else { break };
        out.push_str(&rest[..start]);
        rest = &rest[start + len + 1..];
    }
    out.push_str(rest);
    out
}

/// One message. A message whose HTML embeds images gets its HTML once they're fetched
/// (`inline_images`); until then its text shows, as for any HTML still rendering.
fn message(m: &api::ThreadMessage, now: i64, policy: crate::html_doc::Policy) -> Message {
    let names = |list: &str| split_addresses(list).into_iter().map(|c| c.name).collect();
    let html = m.html.as_deref().filter(|h| !h.trim().is_empty());
    // what was written, apart from the history that came along (durian strips most of it
    // already; this catches what's left, e.g. Outlook's "From:/Sent:" blocks)
    let (body, quoted) = crate::quote::split(&text_body(&m.body));
    Message {
        id: m.id.clone().into(),
        from: parse_address(&m.from),
        to: names(&m.to),
        cc: names(&m.cc),
        bcc: names(&m.bcc),
        at: at(m.timestamp),
        ago: ago(m.timestamp, now),
        read: !m.tags.iter().any(|t| t == "unread"),
        body: body.into(),
        plain: true,
        quoted: quoted.map(Into::into),
        attachments: m
            .attachments
            .iter()
            .filter(|a| listed(a, html))
            .map(|a| Attachment {
                name: if a.filename.is_empty() { "Unnamed attachment".into() } else { a.filename.clone().into() },
                size: size(a.size),
                path: None,
                remote: Some(RemotePart { message: m.id.clone().into(), part: a.part_id }),
            })
            .collect(),
        html_source: None,
        invite: None,
        invitation: m.invitation.as_ref().and_then(|i| invitation(&m.account.clone().unwrap_or_default().into(), i)),
        html: if embedded_parts(m).is_empty() { prepared_html(m, &[], policy).map(Into::into) } else { None },
        eml: None,
    }
}

/// A full thread, oldest message first (the UI's timeline order). None if nothing in it is
/// shown (only reactions), which the UI can't list.
pub fn thread(content: &api::ThreadContent, now: i64, policy: crate::html_doc::Policy) -> Option<Thread> {
    let mut messages: Vec<&api::ThreadMessage> = content.messages.iter().filter(|m| !m.is_reaction).collect();
    if messages.is_empty() {
        return None;
    }
    messages.sort_by_key(|m| m.timestamp);
    let mut tags: Vec<SharedString> = Vec::new();
    for t in messages.iter().flat_map(|m| &m.tags) {
        if !tags.iter().any(|x| x == t) {
            tags.push(t.clone().into());
        }
    }
    Some(Thread {
        id: content.thread_id.clone().into(),
        subject: display_subject(&content.subject),
        starred: tags.iter().any(|t| t == "flagged"),
        account: messages.iter().find_map(|m| m.account.clone()).unwrap_or_default().into(),
        tags,
        items: messages.into_iter().map(|m| ThreadItem::Message(message(m, now, policy))).collect(),
        partial: false,
    })
}

/// The thread-level state a search row carries, applied to a full thread we already have:
/// tags, star, and – when nothing is unread any more – every message read.
pub fn retag(thread: &mut Thread, tags: &[String]) {
    thread.tags = tags.iter().map(|t| t.clone().into()).collect();
    thread.starred = tags.iter().any(|t| t == "flagged");
    if !tags.iter().any(|t| t == "unread") {
        for item in &mut thread.items {
            if let ThreadItem::Message(m) = item {
                m.read = true;
            }
        }
    }
}

/// A search row without content: one stand-in message, filled in when the thread is opened.
pub fn summary(r: &api::SearchResult, now: i64) -> Thread {
    let tags: Vec<SharedString> = r.tags.split(',').map(str::trim).filter(|t| !t.is_empty()).map(|t| t.to_string().into()).collect();
    let stand_in = Message {
        id: SharedString::default(),
        plain: true,
        from: parse_address(&r.from),
        to: Vec::new(),
        cc: Vec::new(),
        bcc: Vec::new(),
        at: at(r.timestamp),
        ago: ago(r.timestamp, now),
        read: !tags.iter().any(|t| t == "unread"),
        body: SharedString::default(),
        quoted: None,
        attachments: Vec::new(),
        html_source: None,
        invite: None,
        invitation: None,
        html: None,
        eml: None,
    };
    Thread {
        id: r.thread_id.clone().into(),
        subject: display_subject(&r.subject),
        starred: tags.iter().any(|t| t == "flagged"),
        account: SharedString::default(),
        tags,
        items: vec![ThreadItem::Message(stand_in)],
        partial: true,
    }
}

pub fn parse_time(s: &str) -> Option<NaiveDateTime> {
    DateTime::parse_from_rfc3339(s)
        .map(|t| t.with_timezone(&Local).naive_local())
        .ok()
        .or_else(|| NaiveDate::parse_from_str(s.get(..10)?, "%Y-%m-%d").ok().map(|d| d.and_hms_opt(0, 0, 0).unwrap_or_default()))
}

/// An answer as durian spells it (`my_response`, an attendee's `response`).
fn answer(response: &str) -> Rsvp {
    match response.to_ascii_lowercase().as_str() {
        "accepted" | "organizer" => Rsvp::Accepted,
        "declined" => Rsvp::Declined,
        "tentative" | "tentativelyaccepted" => Rsvp::Tentative,
        // none, notResponded, needsAction …
        _ => Rsvp::NeedsAction,
    }
}

/// Where an event lives on the server: what `/calendars/event` needs to find it again.
#[derive(Clone, Debug, PartialEq)]
pub struct EventRef {
    pub account: SharedString,
    pub calendar: String,
    pub uid: String,
}

/// A calendar's id in the UI: account and name (names repeat across accounts).
pub fn calendar_id(account: &str, name: &str) -> SharedString {
    format!("{account}/{name}").into()
}

/// One occurrence from `/calendars/events` (summary fields only). Guests and organizer come
/// with the detail; until then, "none" is taken as nothing to answer – most events without
/// an answer are your own.
pub fn calendar_event(account: &SharedString, e: &api::CalendarEvent) -> Option<(CalEvent, EventRef)> {
    let (start, end) = (parse_time(&e.start)?, parse_time(&e.end)?);
    let rsvp = match e.my_response.as_deref() {
        None | Some("none") | Some("") => Rsvp::Accepted,
        Some(r) => answer(r),
    };
    let event = CalEvent {
        // occurrences of a series share the uid
        id: format!("{}@{}", e.uid, e.start).into(),
        calendar: calendar_id(account, &e.calendar),
        title: e.subject.clone().into(),
        start,
        end: end.max(start),
        all_day: e.all_day,
        location: e.location.clone().filter(|l| !l.is_empty()).map(Into::into),
        conference: e.online_meeting_url.clone().filter(|u| !u.is_empty()).map(Into::into),
        description: None,
        attendees: Vec::new(),
        rsvp,
        recurrence: e.recurring.then(|| "Repeats".into()),
        series: e.recurring.then(|| e.uid.clone().into()),
    };
    Some((event, EventRef { account: account.clone(), calendar: e.calendar.clone(), uid: e.uid.clone() }))
}

/// The invitation a message carries, as its card shows it. The event is translated like the
/// calendar's own (same id), so a loaded copy of it is found; the answer is durian's for the
/// account the mail reached, where "none" means not answered yet – unlike a calendar entry
/// without one, which is your own.
pub fn invitation(account: &SharedString, i: &api::Invitation) -> Option<crate::invite::Invitation> {
    use crate::invite::Kind;
    let (mut event, _) = calendar_event(account, &i.event)?;
    let rsvp = answer(i.event.my_response.as_deref().unwrap_or(""));
    (event.description, event.attendees, event.rsvp) = event_detail(&i.event, &[], rsvp);
    let kind = match i.method.as_deref() {
        Some("REQUEST") => Kind::Request,
        Some("CANCEL") => Kind::Cancel,
        Some("REPLY") => Kind::Reply,
        _ => Kind::Other,
    };
    Some(crate::invite::Invitation { event, kind, occurrence: i.occurrence })
}

/// The detail's guests (organizer first) and your answer: an organizer you are means it's
/// yours; otherwise your attendee entry tells; if you're not listed, the summary's answer
/// (`fallback`) stands.
pub fn event_detail(e: &api::CalendarEvent, me: &[SharedString], fallback: Rsvp) -> (Option<SharedString>, Vec<Attendee>, Rsvp) {
    let mine = |email: &str| me.iter().any(|m| m.eq_ignore_ascii_case(email));
    let mut attendees: Vec<Attendee> = Vec::new();
    if let Some(o) = e.organizer.as_ref().filter(|o| !o.email.is_empty()) {
        let name = if o.name.is_empty() { o.email.clone() } else { o.name.clone() };
        attendees.push(Attendee { name: name.into(), email: o.email.clone().into(), rsvp: Rsvp::Accepted, organizer: true, avatar: None });
    }
    for a in &e.attendees {
        if attendees.iter().any(|x| x.email.eq_ignore_ascii_case(&a.email)) {
            continue;
        }
        let name = if a.name.is_empty() { a.email.clone() } else { a.name.clone() };
        attendees.push(Attendee { name: name.into(), email: a.email.clone().into(), rsvp: answer(a.response.as_deref().unwrap_or("")), organizer: false, avatar: None });
    }
    let rsvp = match attendees.iter().find(|a| mine(&a.email)) {
        Some(a) if a.organizer => Rsvp::Accepted,
        Some(a) => a.rsvp,
        None => fallback,
    };
    let description = e.description.clone().filter(|d| !d.trim().is_empty()).map(Into::into);
    (description, attendees, rsvp)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A message of `/threads` carrying an invitation, as durian serves it.
    fn invitation_message(method: &str, my_response: &str) -> api::ThreadMessage {
        let json = r#"{"id":"local:7","account":"work","from":"Mara <mara@x.dev>","timestamp":1,"body":"Planning",
            "invitation":{"method":"METHOD","occurrence":true,"event":{"calendar":"","uid":"plan@x.dev","subject":"Planning",
              "start":"2026-10-20T08:00:00Z","end":"2026-10-20T09:00:00Z","all_day":false,"my_response":"RESPONSE",
              "organizer":{"name":"Mara","email":"mara@x.dev"},
              "attendees":[{"name":"Mara","email":"mara@x.dev","response":"accepted"},{"name":"Me","email":"me@x.dev","response":"none"},{"email":"jo@x.dev","response":"tentative"}]}}}"#;
        serde_json::from_str(&json.replace("METHOD", method).replace("RESPONSE", my_response)).unwrap()
    }

    #[test]
    fn invitations_carry_their_event() {
        let m = invitation_message("REQUEST", "none");
        let carried = m.invitation.as_ref().unwrap();
        let inv = invitation(&"work".into(), carried).unwrap();
        // the same id as the calendar's copy of that occurrence, so a loaded one is found
        let (listed, _) = calendar_event(&"work".into(), &carried.event).unwrap();
        assert_eq!(inv.event.id, listed.id);
        // no answer in an invitation is an open question; in the calendar it's your own event
        assert_eq!(inv.event.rsvp, Rsvp::NeedsAction);
        assert_eq!(listed.rsvp, Rsvp::Accepted);
        assert_eq!(inv.kind, crate::invite::Kind::Request);
        assert!(inv.occurrence);
        let guests: Vec<(&str, bool)> = inv.event.attendees.iter().map(|a| (a.email.as_ref(), a.organizer)).collect();
        assert_eq!(guests, [("mara@x.dev", true), ("me@x.dev", false), ("jo@x.dev", false)]);

        // and the thread's message has it
        let content = api::ThreadContent { thread_id: "t".into(), subject: "Planning".into(), messages: vec![m] };
        let thread = thread(&content, 0, Default::default()).unwrap();
        assert!(thread.messages().next().unwrap().invitation.is_some());
    }

    /// The card shows the calendar's copy of the event when it's loaded (its calendar, your
    /// latest answer), with the mail's guests until the calendar has its own; else the mail's.
    #[test]
    fn invitation_card_prefers_the_calendar_copy() {
        use calendar::model::{CalendarData, Schedule};
        let m = invitation_message("REQUEST", "none");
        let content = api::ThreadContent { thread_id: "t".into(), subject: "Planning".into(), messages: vec![m.clone()] };
        let thread = thread(&content, 0, Default::default()).unwrap();
        let message = thread.messages().next().unwrap();
        let today = chrono::NaiveDate::from_ymd_opt(2026, 10, 10).unwrap();
        let schedule = |events| Schedule::new(CalendarData { calendars: Vec::new(), events, me: Vec::new() }, today);

        // not loaded: the mail's copy
        let (event, kind, occurrence) = crate::invite::resolve(&schedule(Vec::new()), message).unwrap();
        assert_eq!((event.rsvp, event.attendees.len(), kind, occurrence), (Rsvp::NeedsAction, 3, crate::invite::Kind::Request, true));

        // loaded, answered since, guests not fetched yet: the calendar's, with the mail's guests
        let (mut listed, _) = calendar_event(&"work".into(), &m.invitation.as_ref().unwrap().event).unwrap();
        listed.calendar = "work/Work".into();
        listed.rsvp = Rsvp::Tentative;
        let (event, ..) = crate::invite::resolve(&schedule(vec![listed.clone()]), message).unwrap();
        assert_eq!((event.calendar.as_ref(), event.rsvp, event.attendees.len()), ("work/Work", Rsvp::Tentative, 3));

        // the calendar's own guests win once it has them
        listed.attendees = vec![Attendee { name: "Mara".into(), email: "mara@x.dev".into(), rsvp: Rsvp::Accepted, organizer: true, avatar: None }];
        let (event, ..) = crate::invite::resolve(&schedule(vec![listed]), message).unwrap();
        assert_eq!(event.attendees.len(), 1);
    }

    #[test]
    fn invitation_methods_and_answers() {
        use crate::invite::Kind;
        for (method, kind) in [("REQUEST", Kind::Request), ("CANCEL", Kind::Cancel), ("REPLY", Kind::Reply), ("PUBLISH", Kind::Other), ("", Kind::Other)] {
            let m = invitation_message(method, "none");
            assert_eq!(invitation(&"work".into(), m.invitation.as_ref().unwrap()).unwrap().kind, kind, "{method}");
        }
        for (response, rsvp) in [("accepted", Rsvp::Accepted), ("declined", Rsvp::Declined), ("tentative", Rsvp::Tentative), ("organizer", Rsvp::Accepted), ("", Rsvp::NeedsAction)] {
            let m = invitation_message("REQUEST", response);
            assert_eq!(invitation(&"work".into(), m.invitation.as_ref().unwrap()).unwrap().event.rsvp, rsvp, "{response}");
        }
    }

    #[test]
    fn html_follows_the_remote_images_setting() {
        let m: api::ThreadMessage = serde_json::from_str(r#"{"id":"local:1","timestamp":1,"body":"","html":"<p>hi<img src=\"https://cdn.x.dev/a.png\"></p>"}"#).unwrap();
        let off = prepared_html(&m, &[], html_policy(&api::Settings::default())).unwrap();
        let on = prepared_html(&m, &[], html_policy(&api::Settings { load_remote_images: true })).unwrap();
        assert!(!off.contains("https://cdn.x.dev/a.png"), "{off}");
        assert!(on.contains("https://cdn.x.dev/a.png"), "{on}");
    }

    #[test]
    fn addresses_parse_with_names_quotes_and_commas() {
        let list = split_addresses(r#"Mara Lindqvist <mara@habric.com>, "Doe, Jane" <jane@x.org>, bob@y.dev"#);
        let got: Vec<(String, String)> = list.iter().map(|c| (c.name.to_string(), c.email.to_string())).collect();
        assert_eq!(got, [("Mara Lindqvist".into(), "mara@habric.com".into()), ("Doe, Jane".into(), "jane@x.org".into()), ("bob".into(), "bob@y.dev".into())]);
    }

    #[test]
    fn ages_match_the_ui_format() {
        let now = 1_000_000;
        assert_eq!(ago(now - 30, now).as_ref(), "now");
        assert_eq!(ago(now - 8 * 60, now).as_ref(), "8m");
        assert_eq!(ago(now - 3 * 3600, now).as_ref(), "3h");
        assert_eq!(ago(now - 2 * 86400, now).as_ref(), "2d");
        assert_eq!(ago(now - 15 * 86400, now).as_ref(), "2w");
    }

    #[test]
    fn decodes_a_search_response_and_maps_threads() {
        let json = r#"{"ok":true,"results":[
            {"thread_id":"a1","subject":"Hi","from":"Mara <mara@x>","date":"15:04","timestamp":100,"tags":"inbox,unread"},
            {"thread_id":"b2","subject":"Old","from":"bob@y","timestamp":50,"tags":"inbox,flagged"}],
          "threads":{"a1":{"thread_id":"a1","subject":"Hi","messages":[
            {"id":"local:2","from":"Mara <mara@x>","to":"me@x","timestamp":100,"body":"later","tags":["inbox","unread"],"account":"me@x"},
            {"id":"local:1","from":"me@x","to":"Mara <mara@x>","timestamp":90,"body":"first","tags":["sent"],"attachments":[{"part_id":2,"filename":"a.pdf","size":2048}]}]}}}"#;
        let search: api::SearchResponse = serde_json::from_str(json).unwrap();
        let full = thread(&search.threads["a1"], 200, Default::default()).unwrap();
        assert!(!full.partial && full.account == "me@x");
        assert_eq!(full.messages().map(|m| m.body.to_string()).collect::<Vec<_>>(), ["first", "later"], "oldest first");
        assert!(full.unread());
        assert_eq!(full.messages().next().unwrap().attachments[0].size.as_ref(), "2 KB");
        let row = summary(&search.results[1], 200);
        assert!(row.partial && row.starred && !row.unread());
    }

    fn event(json: &str) -> api::CalendarEvent {
        serde_json::from_str(json).unwrap()
    }

    #[test]
    fn embedded_images_and_signatures_are_not_attachments() {
        let part = |json: &str| -> api::AttachmentInfo { serde_json::from_str(json).unwrap() };
        let logo = part(r#"{"part_id":2,"content_type":"image/png","disposition":"inline","content_id":"<logo@x>","size":10}"#);
        assert!(!listed(&logo, Some(r#"<img src="cid:logo@x">"#)), "shown in place");
        assert!(!listed(&logo, Some("<p>reply, quote stripped</p>")), "the stripped original's image (Outlook re-attaches it)");
        assert!(!listed(&logo, None), "a preview without HTML");
        let huge = part(r#"{"part_id":3,"content_type":"image/png","disposition":"inline","content_id":"<huge@x>","size":99999999}"#);
        assert!(listed(&huge, Some(r#"<img src="cid:huge@x">"#)), "too big to inline: reachable as an attachment");
        // Outlook gives real attachments a content id too
        let docx = part(r#"{"part_id":4,"content_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","disposition":"attachment","content_id":"<d@x>","size":10}"#);
        assert!(listed(&docx, Some("<p>see attached</p>")));
        // Apple Mail: inline parts without an id are meant to be seen
        assert!(listed(&part(r#"{"part_id":5,"content_type":"application/pdf","disposition":"inline"}"#), Some("<p></p>")));
        assert!(!listed(&part(r#"{"part_id":6,"content_type":"application/pkcs7-signature"}"#), None));
    }

    #[test]
    fn subjects_lose_reply_prefixes() {
        assert_eq!(display_subject("AW: WG: Re[2]: eQMS Freigabe").as_ref(), "eQMS Freigabe");
        assert_eq!(display_subject("Fwd: re: Hello").as_ref(), "Hello");
        assert_eq!(display_subject("Agenda: Q4").as_ref(), "Agenda: Q4", "a colon in the subject itself stays");
        assert_eq!(display_subject("  ").as_ref(), "(no subject)");
        assert_eq!(display_subject("Re:").as_ref(), "(no subject)");
    }

    #[test]
    fn text_bodies_lose_crlf_and_cid_placeholders() {
        assert_eq!(text_body("Hi,\r\n[cid:image001.png@01DA]\r\nBest"), "Hi,\n\nBest");
        assert_eq!(text_body("a [cid:broken"), "a [cid:broken", "an unclosed bracket stays");
    }

    #[test]
    fn eqms_like_reply_lists_only_the_real_attachment() {
        // a reply whose quoted original (with ten inline images) was stripped server-side
        let parts: String = (0..10).map(|i| format!(r#"{{"part_id":{i},"content_type":"image/png","disposition":"inline","content_id":"<img{i}@x>","size":100}},"#)).collect();
        let json = format!(r#"{{"id":"local:2","html":"<div>ok</div><div></div>","attachments":[{parts}{{"part_id":10,"content_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","disposition":"attachment","content_id":"<doc@x>","size":5000,"filename":"a.docx"}}]}}"#);
        let m: api::ThreadMessage = serde_json::from_str(&json).unwrap();
        let mapped = message(&m, 0, Default::default());
        assert_eq!(mapped.attachments.len(), 1);
        assert!(embedded_parts(&m).is_empty() && mapped.html.is_some(), "nothing to fetch: HTML right away");
    }

    #[test]
    fn embedded_images_become_data_uris() {
        let m: api::ThreadMessage = serde_json::from_str(r#"{"id":"local:7","html":"<img src=\"cid:logo@x\"><img src=\"cid:gone@x\">","attachments":[
            {"part_id":2,"content_type":"image/png","disposition":"inline","content_id":"<logo@x>","size":10},
            {"part_id":3,"content_type":"image/png","disposition":"inline","content_id":"<gone@x>","size":10}]}"#).unwrap();
        assert!(message(&m, 0, Default::default()).html.is_none(), "HTML waits for its images");
        let parts = embedded_parts(&m);
        assert_eq!(parts.len(), 2);
        // one fetched, one failed: the failed one stays a broken reference
        let html = prepared_html(&m, &[(parts[0], b"png".to_vec())], Default::default()).unwrap();
        assert!(html.contains(r#"<img src="data:image/png;base64,cG5n">"#), "{html}");
        assert_eq!(html.matches("<img").count(), 1, "the failed one shows nothing");
    }

    #[test]
    fn calendar_summaries_map_to_occurrences() {
        let e = event(r#"{"calendar":"Work","uid":"abc@google.com","subject":"Standup","start":"2026-10-12T09:00:00+02:00","end":"2026-10-12T09:15:00+02:00","recurring":true,"my_response":"none"}"#);
        let (ev, at) = calendar_event(&"work".into(), &e).unwrap();
        assert_eq!(ev.calendar.as_ref(), "work/Work");
        assert_eq!(ev.series.as_deref(), Some("abc@google.com"));
        assert_eq!(ev.rsvp, Rsvp::Accepted, "no answer in a summary: likely your own");
        assert_eq!(at, EventRef { account: "work".into(), calendar: "Work".into(), uid: "abc@google.com".into() });
        assert!(calendar_event(&"work".into(), &event(r#"{"calendar":"W","uid":"x","start":"garbage","end":"x"}"#)).is_none());
    }

    #[test]
    fn the_detail_decides_whose_event_it_is() {
        let me: Vec<SharedString> = vec!["ada@work.dev".into()];
        let invited = event(r#"{"calendar":"W","uid":"u","start":"2026-10-12","end":"2026-10-13",
            "organizer":{"name":"Mara","email":"mara@x.org"},
            "attendees":[{"name":"Mara","email":"mara@x.org","response":"accepted"},{"name":"","email":"Ada@work.dev","response":"notResponded"}]}"#);
        let (_, guests, rsvp) = event_detail(&invited, &me, Rsvp::Accepted);
        assert_eq!(guests.len(), 2, "the organizer isn't listed twice");
        assert!(guests[0].organizer && guests[1].name == "Ada@work.dev");
        assert_eq!(rsvp, Rsvp::NeedsAction);

        let own = event(r#"{"calendar":"W","uid":"u","start":"2026-10-12","end":"2026-10-13","organizer":{"email":"ada@work.dev"},"attendees":[{"email":"bob@y","response":"tentativelyAccepted"}]}"#);
        let (_, guests, rsvp) = event_detail(&own, &me, Rsvp::NeedsAction);
        assert_eq!((rsvp, guests[1].rsvp), (Rsvp::Accepted, Rsvp::Tentative));

        let unlisted = event(r#"{"calendar":"W","uid":"u","start":"2026-10-12","end":"2026-10-13","description":"  "}"#);
        let (description, _, rsvp) = event_detail(&unlisted, &me, Rsvp::Declined);
        assert_eq!((description, rsvp), (None, Rsvp::Declined));
    }
}

/// Cost of mapping and previewing real threads: `DURIAN_PROBE=/dir/of/thread/json cargo test
/// -p mail --release probe -- --ignored --nocapture` (files: `/threads/{id}` answers).
#[cfg(test)]
mod probe {
    use std::time::Instant;

    #[test]
    #[ignore]
    fn probe() {
        let Some(dir) = std::env::var_os("DURIAN_PROBE") else { return };
        let (mut map, mut preview, mut plain, mut n, mut bytes) = (0f64, 0f64, 0f64, 0, 0);
        let mut worst = (0f64, 0usize);
        for entry in std::fs::read_dir(dir).unwrap() {
            let text = std::fs::read_to_string(entry.unwrap().path()).unwrap();
            let Ok(response) = serde_json::from_str::<super::api::ThreadResponse>(&text) else { println!("decode failed"); continue };
            let Some(content) = response.thread else { continue };
            let t = Instant::now();
            let Some(thread) = super::thread(&content, 0, Default::default()) else { continue };
            map += t.elapsed().as_secs_f64();
            for m in thread.messages() {
                n += 1;
                bytes += m.body.len();
                let t = Instant::now();
                let _ = m.preview();
                let took = t.elapsed().as_secs_f64();
                preview += took;
                if took > worst.0 {
                    worst = (took, m.body.len());
                }
                let t = Instant::now();
                let _ = ui::markdown::plain_text(&m.body);
                plain += t.elapsed().as_secs_f64();
            }
        }
        println!("{n} messages, {bytes} body bytes: map {:.1}ms, preview {:.1}ms (worst {:.1}ms for {} bytes), plain_text {:.1}ms", map * 1e3, preview * 1e3, worst.0 * 1e3, worst.1, plain * 1e3);
    }
}
