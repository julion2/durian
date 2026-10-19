//! Wire types of durian's HTTP API (`openapi.yaml`, `/api/v1`), as the server sends them.
//! Everything optional on the wire is `#[serde(default)]`, so older servers (fields added
//! later, like `account` on messages) still decode.

use std::collections::HashMap;

use serde::{Deserialize, Deserializer, Serialize};

/// Go encodes an empty slice or map as `null`: read that as empty, like a missing field.
fn nullable<'de, D: Deserializer<'de>, T: Default + Deserialize<'de>>(d: D) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// The envelope most responses share: `{ok, error, …payload}`.
#[derive(Deserialize, Debug, Default)]
pub struct Envelope {
    #[serde(default)]
    pub ok: Option<bool>,
    #[serde(default)]
    pub error: Option<String>,
}

#[derive(Deserialize, Debug)]
pub struct Version {
    pub version: String,
    #[serde(default, deserialize_with = "nullable")]
    pub commit: String,
}

/// `GET /contacts/search`: ordered by usage on the server, not thread proximity.
#[derive(Deserialize, Debug)]
pub struct Contact {
    pub email: String,
    #[serde(default)]
    pub name: String,
}

/// `GET /search`: one row per thread; `threads` holds full content for the first `enrich`.
#[derive(Deserialize, Debug)]
pub struct SearchResponse {
    #[serde(flatten)]
    pub envelope: Envelope,
    #[serde(default, deserialize_with = "nullable")]
    pub results: Vec<SearchResult>,
    #[serde(default, deserialize_with = "nullable")]
    pub threads: HashMap<String, ThreadContent>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct SearchResult {
    pub thread_id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub subject: String,
    /// "Name <email>" of the latest sender
    #[serde(default, deserialize_with = "nullable")]
    pub from: String,
    /// display string ("15:04", "Yesterday 15:04", …) – we format from `timestamp` instead
    #[serde(default, deserialize_with = "nullable")]
    pub date: String,
    #[serde(default)]
    pub timestamp: i64,
    /// comma-separated (unlike the array on messages)
    #[serde(default, deserialize_with = "nullable")]
    pub tags: String,
}

#[derive(Deserialize, Debug)]
pub struct SearchCount {
    pub count: u64,
}

#[derive(Deserialize, Debug)]
pub struct ThreadResponse {
    #[serde(flatten)]
    pub envelope: Envelope,
    pub thread: Option<ThreadContent>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ThreadContent {
    pub thread_id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub subject: String,
    #[serde(default, deserialize_with = "nullable")]
    pub messages: Vec<ThreadMessage>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ThreadMessage {
    /// opaque ("local:<rowid>"); percent-encode as one path segment, never parse
    pub id: String,
    #[serde(default)]
    pub attachment_cache_id: Option<String>,
    /// RFC 5322 Message-ID – metadata only, not unique
    #[serde(default)]
    pub message_id: Option<String>,
    #[serde(default)]
    pub in_reply_to: Option<String>,
    #[serde(default)]
    pub references: Option<String>,
    #[serde(default)]
    pub account: Option<String>,
    #[serde(default)]
    pub subject: Option<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub from: String,
    /// comma-separated address lists
    #[serde(default, deserialize_with = "nullable")]
    pub to: String,
    #[serde(default, deserialize_with = "nullable")]
    pub cc: String,
    #[serde(default, deserialize_with = "nullable")]
    pub bcc: String,
    #[serde(default)]
    pub timestamp: i64,
    #[serde(default, deserialize_with = "nullable")]
    pub body: String,
    #[serde(default)]
    pub html: Option<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub attachments: Vec<AttachmentInfo>,
    #[serde(default, deserialize_with = "nullable")]
    pub tags: Vec<String>,
    #[serde(default)]
    pub is_reaction: bool,
    /// the calendar invitation the message carries (`/threads` only, durian ≥ #440)
    #[serde(default)]
    pub invitation: Option<Invitation>,
}

/// A message's iCalendar part as durian reads it.
#[derive(Deserialize, Debug, Clone)]
pub struct Invitation {
    /// iTIP method, upper case: REQUEST, CANCEL, REPLY, …; absent when the part names none
    #[serde(default)]
    pub method: Option<String>,
    /// as `/calendars/event` serves it; `calendar` is empty (it's the mail's copy) and
    /// `my_response` is read for the account the mail reached
    pub event: CalendarEvent,
    /// about one date of a series only – answering the uid answers every date
    #[serde(default)]
    pub occurrence: bool,
}

/// `/settings`: config.pkl's [settings], as far as durian serves them.
#[derive(Deserialize, Debug, Default)]
pub struct SettingsResponse {
    #[serde(default)]
    pub settings: Settings,
}

#[derive(Deserialize, Debug, Default, Clone, PartialEq)]
pub struct Settings {
    /// whether HTML mail may load remote images (tracking pixels included)
    #[serde(default)]
    pub load_remote_images: bool,
}

/// Safe composition configuration, never the complete account/transport config.
#[derive(Deserialize, Debug, Default, Clone, PartialEq)]
pub struct ComposeSettings {
    pub signatures: std::collections::BTreeMap<String, String>,
    /// Lowercased configured account names, as used by /profiles and drafts.
    pub account_defaults: std::collections::BTreeMap<String, String>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct AttachmentInfo {
    pub part_id: i64,
    /// "attachment" or "inline" (embedded in the HTML, or meant to be shown in place)
    #[serde(default)]
    pub disposition: Option<String>,
    /// what the HTML references it by (`cid:…`)
    #[serde(default)]
    pub content_id: Option<String>,
    #[serde(default, deserialize_with = "nullable")]
    pub filename: String,
    #[serde(default, deserialize_with = "nullable")]
    pub content_type: String,
    #[serde(default)]
    pub size: i64,
}

#[derive(Deserialize, Debug)]
pub struct TagsResponse {
    #[serde(default, deserialize_with = "nullable")]
    pub tags: Vec<String>,
}

/// `GET /profiles`: profiles.pkl with its account references resolved.
#[derive(Deserialize, Debug, Clone)]
pub struct ProfilesResponse {
    #[serde(default, deserialize_with = "nullable")]
    pub profiles: Vec<ApiProfile>,
    /// every configured account
    #[serde(default, deserialize_with = "nullable")]
    pub accounts: Vec<ApiAccount>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ApiProfile {
    pub name: String,
    #[serde(default)]
    pub default: bool,
    #[serde(default)]
    pub color: Option<String>,
    /// written as accounts = { "*" }
    #[serde(default)]
    pub all_accounts: bool,
    #[serde(default, deserialize_with = "nullable")]
    pub accounts: Vec<ApiAccount>,
    #[serde(default, deserialize_with = "nullable")]
    pub folders: Vec<ApiFolder>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ApiAccount {
    /// scope searches with path:<name>/**
    pub name: String,
    #[serde(default, deserialize_with = "nullable")]
    pub display_name: String,
    /// empty for a reference that matches no configured account
    #[serde(default, deserialize_with = "nullable")]
    pub email: String,
    #[serde(default, deserialize_with = "nullable")]
    pub alias: String,
    #[serde(default)]
    pub default: bool,
}

#[derive(Deserialize, Debug, Clone)]
pub struct ApiFolder {
    pub name: String,
    #[serde(default, deserialize_with = "nullable")]
    pub icon: String,
    pub query: String,
}

#[derive(Deserialize, Debug)]
pub struct CalendarsResponse {
    #[serde(default, deserialize_with = "nullable")]
    pub calendars: Vec<Calendar>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct Calendar {
    pub name: String,
    /// "#RRGGBB", absent when the provider has none
    #[serde(default)]
    pub color: Option<String>,
}

#[derive(Deserialize, Debug)]
pub struct CalendarEventResponse {
    pub event: CalendarEvent,
}

#[derive(Deserialize, Debug)]
pub struct CalendarEventsResponse {
    #[serde(default, deserialize_with = "nullable")]
    pub events: Vec<CalendarEvent>,
}

/// One event or one occurrence of a series (summary fields only from `/calendars/events`).
#[derive(Deserialize, Debug, Clone)]
pub struct CalendarEvent {
    pub calendar: String,
    /// iCalUID – shared by every occurrence of a series
    pub uid: String,
    #[serde(default, deserialize_with = "nullable")]
    pub subject: String,
    pub start: String,
    pub end: String,
    #[serde(default)]
    pub all_day: bool,
    #[serde(default)]
    pub location: Option<String>,
    /// accepted | declined | tentative | organizer | none
    #[serde(default)]
    pub my_response: Option<String>,
    #[serde(default)]
    pub online_meeting_url: Option<String>,
    #[serde(default)]
    pub recurring: bool,
    /// detail endpoint only
    #[serde(default)]
    pub organizer: Option<CalendarPerson>,
    #[serde(default, deserialize_with = "nullable")]
    pub attendees: Vec<CalendarAttendee>,
    #[serde(default)]
    pub description: Option<String>,
}

#[derive(Deserialize, Debug, Clone)]
pub struct CalendarPerson {
    #[serde(default, deserialize_with = "nullable")]
    pub name: String,
    pub email: String,
}

#[derive(Deserialize, Debug, Clone)]
pub struct CalendarAttendee {
    #[serde(default, deserialize_with = "nullable")]
    pub name: String,
    pub email: String,
    /// accepted | declined | tentativelyAccepted | none | notResponded …
    #[serde(default)]
    pub response: Option<String>,
}

/// Opaque local-draft JSON uses Swift's EmailDraft/EmailAttachment field names.
/// The server encrypts it at rest; GPUI never writes a plaintext snapshot.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct DraftContent {
    pub id: String,
    #[serde(default)] pub from: String,
    #[serde(default)] pub to: Vec<String>,
    #[serde(default)] pub cc: Vec<String>,
    #[serde(default)] pub bcc: Vec<String>,
    #[serde(default)] pub subject: String,
    #[serde(default)] pub body: String,
    #[serde(default, rename = "isHTML")] pub is_html: bool,
    #[serde(default)] pub in_reply_to: Option<String>,
    #[serde(default)] pub references: Option<String>,
    #[serde(default)] pub account_id: Option<String>,
    #[serde(default)] pub reply_thread_id: Option<String>,
    #[serde(default)] pub created_at: String,
    #[serde(default)] pub modified_at: String,
    #[serde(default)] pub attachments: Vec<DraftAttachment>,
    // Preserve Swift rich/quoted content when reopening a shared draft.
    #[serde(default)] pub html_body: Option<String>,
    #[serde(default)] pub html_signature: Option<String>,
    #[serde(default)] pub quoted_content: Option<String>,
    #[serde(default, rename = "quotedIsHTML")] pub quoted_is_html: bool,
    #[serde(flatten)] pub extra: HashMap<String, serde_json::Value>,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct DraftAttachment {
    pub id: String,
    pub filename: String,
    pub mime_type: String,
    /// Swift Data JSON encoding is base64.
    pub data: String,
}

#[derive(Debug, Deserialize)]
pub struct LocalDraft {
    pub id: String,
    pub draft_json: serde_json::Value,
    pub created_at: i64,
    pub modified_at: i64,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub struct OutboxAttachment {
    pub filename: String,
    pub mime_type: String,
    pub data_base64: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub struct OutboxDraft {
    pub idempotency_key: String,
    pub from: String,
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub bcc: Vec<String>,
    pub subject: String,
    pub body: String,
    pub is_html: bool,
    pub in_reply_to: String,
    pub references: String,
    pub attachments: Vec<OutboxAttachment>,
    pub delay_seconds: u32,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Enqueued {
    pub id: i64,
    pub send_after: i64,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct OutboxUpdate {
    pub item_id: i64,
    pub status: String,
    #[serde(default)] pub error: String,
}

#[derive(Clone, Debug, Deserialize)]
pub struct OutboxEntry {
    pub id: i64,
    pub message_id: String,
    #[serde(default)] pub last_error: String,
    pub in_flight: bool,
    pub delivery_confirmed: bool,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn null_collections_read_as_empty() {
        let empty: SearchResponse = serde_json::from_str(r#"{"ok":true,"results":null,"threads":null}"#).unwrap();
        assert!(empty.results.is_empty() && empty.threads.is_empty());
        let m: ThreadMessage = serde_json::from_str(r#"{"id":"local:1","from":null,"tags":null,"attachments":null}"#).unwrap();
        assert!(m.tags.is_empty() && m.attachments.is_empty() && m.from.is_empty());
    }
}
