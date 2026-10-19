//! Bounded write scheduling. A key is one thread or draft: operations for it
//! finish in order, even when another thread/draft can progress independently.

use std::collections::{HashSet, VecDeque};

use super::{api, client::{Client, Result}};

pub(super) const IN_FLIGHT: usize = 4;
const PENDING_LIMIT: usize = 128;
const PENDING_BYTES: usize = 8 << 20;

#[derive(Clone)]
pub enum Intent {
    Tags { thread: String, ops: String },
    Save { draft: api::DraftContent },
    Delete { id: String, local_id: usize },
    Send { draft: api::DraftContent, local_id: usize, reply_all: Option<bool> },
    Undo { id: i64, draft: api::DraftContent, local_id: usize },
}

impl Intent {
    fn key(&self) -> String {
        match self {
            Self::Tags { thread, .. } => format!("thread:{thread}"),
            Self::Save { draft } | Self::Send { draft, .. } | Self::Undo { draft, .. } => format!("draft:{}", draft.id),
            Self::Delete { id, .. } => format!("draft:{id}"),
        }
    }

    fn bytes(&self) -> usize {
        match self {
            Self::Tags { thread, ops } => thread.len() + ops.len(),
            Self::Delete { id, .. } => id.len(),
            Self::Save { draft } | Self::Send { draft, .. } | Self::Undo { draft, .. } => serde_json::to_vec(draft).map_or(PENDING_BYTES + 1, |b| b.len()),
        }
    }
}

#[derive(Default)]
pub(super) struct Queue {
    pending: VecDeque<Intent>,
    running: HashSet<String>,
}

impl Queue {
    pub fn push(&mut self, intent: Intent) -> Result<()> {
        let bytes: usize = self.pending.iter().map(Intent::bytes).sum();
        // Autosave replaces only another unsent save at the tail for this draft;
        // never cross a send/delete barrier.
        if matches!(intent, Intent::Save { .. }) {
            let key = intent.key();
            if let Some(previous) = self.pending.iter_mut().rev().find(|i| i.key() == key) {
                if matches!(previous, Intent::Save { .. }) {
                    if bytes - previous.bytes() + intent.bytes() > PENDING_BYTES { return Err(queue_full()) }
                    *previous = intent;
                    return Ok(());
                }
            }
        }
        if self.pending.len() >= PENDING_LIMIT || bytes + intent.bytes() > PENDING_BYTES { return Err(queue_full()) }
        self.pending.push_back(intent);
        Ok(())
    }

    pub fn next(&mut self) -> Option<(String, Intent)> {
        if self.running.len() >= IN_FLIGHT { return None }
        let ix = self.pending.iter().position(|i| !self.running.contains(&i.key()))?;
        let intent = self.pending.remove(ix)?;
        let key = intent.key();
        self.running.insert(key.clone());
        Some((key, intent))
    }

    pub fn finished(&mut self, key: &str) { self.running.remove(key); }
    pub fn pending_thread(&self, id: &str) -> bool {
        let key = format!("thread:{id}");
        self.running.contains(&key) || self.pending.iter().any(|i| i.key() == key)
    }
}

fn queue_full() -> super::client::Error { super::client::Error::Decode("Write queue is full (128 intents / 8 MiB). Content retained; retry when pending writes finish.".into()) }

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Delivery {
    Queued,
    InFlight,
    Failed(String),
    ReconciliationRequired,
    DeliveredWithWarning(String),
    Sent,
    Unknown,
}

impl Delivery {
    pub fn from_update(update: &api::OutboxUpdate) -> Option<Self> {
        Some(match update.status.as_str() {
            "queued" => Self::Queued, "sent" => Self::Sent,
            "failed" => Self::Failed(update.error.clone()),
            "reconciliation_required" => Self::ReconciliationRequired,
            "delivered_with_warning" => Self::DeliveredWithWarning(update.error.clone()),
            _ => return None,
        })
    }

    pub fn reconcile(previous: Option<&Self>, entry: Option<&api::OutboxEntry>) -> Self {
        // A terminal observed SSE result is evidence; absence from GET alone
        // could mean cancellation, missed delivery, or external deletion.
        if matches!(previous, Some(Self::Sent | Self::DeliveredWithWarning(_))) { return previous.unwrap().clone() }
        match entry {
            Some(e) if e.delivery_confirmed => Self::DeliveredWithWarning(e.last_error.clone()),
            Some(e) if e.in_flight && (matches!(previous, Some(Self::ReconciliationRequired)) || e.last_error == "Delivery status is unknown. Verify the provider outcome before reconciling.") => Self::ReconciliationRequired,
            Some(e) if e.in_flight => Self::InFlight,
            Some(e) if !e.last_error.is_empty() => {
                if matches!(previous, Some(Self::ReconciliationRequired)) { Self::ReconciliationRequired } else { Self::Failed(e.last_error.clone()) }
            }
            Some(_) => Self::Queued,
            None => Self::Unknown,
        }
    }

    pub fn message(&self) -> String {
        match self {
            Self::Queued => "Queued for delivery — not yet sent".into(),
            Self::InFlight => "Outbox item is claimed; delivery may be in progress or need reconciliation. Cancellation may be too late".into(),
            Self::Failed(error) => format!("Delivery failed: {error}. The outbox retains the message."),
            Self::ReconciliationRequired => "Delivery status unknown. Verify the provider's outbox Message-ID before retrying.".into(),
            Self::DeliveredWithWarning(error) => format!("Delivered, but Sent filing needs attention: {error}"),
            Self::Sent => "Delivery confirmed".into(),
            Self::Unknown => "Delivery status unknown; a missing outbox row is not proof of delivery".into(),
        }
    }
}

pub enum Outcome {
    Tags,
    Saved,
    Deleted,
    Sent(api::Enqueued),
    Undone { persisted: bool },
}

/// Stable key and frozen payload are saved before enqueue. After an unknown
/// response/crash, retry sends this same logical message, never edited content.
pub const PENDING_SEND: &str = "gpuiPendingSend";
pub const QUEUED_SEND: &str = "gpuiQueuedSend";
pub const SEND_KEY: &str = "gpuiSendKey";
pub const REPLY_TARGET: &str = "gpuiReplyTarget";
pub const OWN_ADDRESSES: &str = "gpuiOwnAddresses";

pub fn draft_fits(draft: &api::DraftContent) -> bool {
    serde_json::to_vec(&serde_json::json!({"draft_json": draft})).is_ok_and(|bytes| bytes.len() <= 1 << 20)
}

pub fn execute(client: &Client, intent: &mut Intent) -> Result<Outcome> {
    match intent {
        Intent::Tags { thread, ops } => { client.tag_thread(thread, ops)?; Ok(Outcome::Tags) }
        Intent::Save { draft } => { client.save_draft(draft)?; Ok(Outcome::Saved) }
        Intent::Delete { id, .. } => { client.delete_draft(id)?; Ok(Outcome::Deleted) }
        Intent::Undo { id, draft, .. } => {
            client.cancel_send(*id)?; // 404/409/unknown outcome never restores or rotates
            draft.extra.remove(PENDING_SEND); draft.extra.remove(QUEUED_SEND);
            draft.extra.insert(SEND_KEY.into(), uuid::Uuid::new_v4().to_string().into());
            let persisted = client.save_draft(draft).is_ok();
            Ok(Outcome::Undone { persisted })
        }
        Intent::Send { draft, reply_all, .. } => {
            let payload = match draft.extra.get(PENDING_SEND) {
                Some(frozen) => serde_json::from_value(frozen.clone()).map_err(|e| super::client::Error::Decode(e.to_string()))?,
                None => {
                    if draft.reply_thread_id.is_some() && reply_all.is_some() && !draft.extra.contains_key(REPLY_TARGET) {
                        return Err(super::client::Error::Decode("Reply has no captured full-thread target; reopen it before sending".into()));
                    }
                    let payload = outbox(draft)?;
                    draft.extra.insert(PENDING_SEND.into(), serde_json::to_value(&payload).unwrap());
                    payload
                }
            };
            if !draft_fits(draft) {
                draft.extra.remove(PENDING_SEND);
                return Err(super::client::Error::Decode("Draft including send recovery exceeds 1 MiB; nothing was queued".into()))
            }
            client.save_draft(draft)?;
            let queued = client.enqueue(&payload)?;
            // Best-effort receipt: the frozen key still makes recovery safe if
            // this write fails. The enqueue itself already succeeded.
            draft.extra.insert(QUEUED_SEND.into(), serde_json::json!({"id": queued.id, "send_after": queued.send_after}));
            let _ = client.save_draft(draft);
            Ok(Outcome::Sent(queued))
        }
    }
}

fn addresses(list: &[String]) -> Result<Vec<String>> {
    list.iter().map(|s| {
        let email = super::parse_address(s).email.to_string();
        let valid = email.split_once('@').is_some_and(|(local, domain)| !local.is_empty() && !domain.is_empty())
            && !email.chars().any(|c| c.is_whitespace() || matches!(c, '<' | '>' | ',' | ';'));
        if valid { Ok(email) } else { Err(super::client::Error::Decode("Recipient needs a real email address".into())) }
    }).collect()
}

pub fn outbox(draft: &api::DraftContent) -> Result<api::OutboxDraft> {
    use base64::Engine;
    let from = addresses(std::slice::from_ref(&draft.from))?.remove(0);
    let to = addresses(&draft.to)?;
    if to.is_empty() { return Err(super::client::Error::Decode("Add a To recipient".into())) }
    let cc = addresses(&draft.cc)?;
    let bcc = addresses(&draft.bcc)?;
    let attachments = draft.attachments.iter().map(|a| {
        base64::engine::general_purpose::STANDARD.decode(&a.data).map_err(|_| super::client::Error::Decode("Attachment data is invalid".into()))?;
        Ok(api::OutboxAttachment { filename: a.filename.clone(), mime_type: a.mime_type.clone(), data_base64: a.data.clone() })
    }).collect::<Result<Vec<_>>>()?;
    // Rich/quoted shared Swift drafts must not silently lose their content.
    let mut body = draft.html_body.clone().filter(|b| !b.is_empty()).unwrap_or_else(|| draft.body.clone());
    let mut is_html = draft.is_html || draft.html_body.as_ref().is_some_and(|b| !b.is_empty());
    let signature = draft.html_signature.as_deref().filter(|s| !s.trim().is_empty());
    if signature.is_some() || draft.quoted_is_html {
        if !is_html { body = escape_html(&body).replace('\n', "<br>"); }
        is_html = true;
    }
    if let Some(signature) = signature {
        // Block margins collapse with the Markdown paragraph's existing 12px gap;
        // line breaks added on top of that made the signature look two paragraphs away.
        body.push_str(&format!("<div style=\"margin-top:12px\">{signature}</div>"));
    }
    if let Some(quote) = &draft.quoted_content {
        body.push_str(if is_html { "<br><br>" } else { "\n\n" });
        body.push_str(&if is_html && !draft.quoted_is_html { escape_html(quote).replace('\n', "<br>") } else { quote.clone() });
    }
    let identity = draft.extra.get(SEND_KEY).and_then(|key| key.as_str()).unwrap_or(&draft.id);
    Ok(api::OutboxDraft { idempotency_key: format!("gpui-compose:{identity}"), from, to, cc, bcc,
        subject: draft.subject.clone(), body, is_html,
        in_reply_to: draft.in_reply_to.clone().unwrap_or_default(), references: draft.references.clone().unwrap_or_default(),
        attachments, delay_seconds: 10 })
}

fn escape_html(s: &str) -> String { s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;") }

/// Resolve reply participants and headers from the full server thread, not
/// display names or an enriched preview (which omits reply headers).
pub fn prepare_reply(draft: &mut api::DraftContent, thread: &api::ThreadContent, all: bool) -> Result<()> {
    let captured = draft.extra.get(REPLY_TARGET).map(|v| serde_json::from_value::<api::ThreadMessage>(v["message"].clone())).transpose()
        .map_err(|_| super::client::Error::Decode("Captured reply target is invalid".into()))?;
    let latest = captured.as_ref().or_else(|| thread.messages.iter().filter(|m| !m.is_reaction).max_by_key(|m| m.timestamp))
        .ok_or_else(|| super::client::Error::Decode("Reply thread has no message".into()))?;
    let mut seen = HashSet::new();
    seen.insert(super::parse_address(&draft.from).email.to_lowercase());
    if let Some(own) = draft.extra.get(OWN_ADDRESSES).and_then(|v| v.as_array()) {
        seen.extend(own.iter().filter_map(|v| v.as_str()).map(str::to_lowercase));
    }
    let mine = seen.contains(&super::parse_address(&latest.from).email.to_lowercase());
    let mut collect = |raw: &str| -> Vec<String> {
        super::split_addresses(raw).into_iter().filter_map(|c| {
            seen.insert(c.email.to_lowercase()).then(|| c.email.to_string())
        }).collect()
    };
    draft.to = collect(if mine { &latest.to } else { &latest.from });
    if all && !mine { draft.to.extend(collect(&latest.to)); }
    draft.cc = if all { collect(&latest.cc) } else { Vec::new() };
    draft.bcc.clear();
    if captured.is_none() {
        draft.subject = if thread.subject.to_lowercase().starts_with("re:") { thread.subject.clone() } else { format!("Re: {}", thread.subject) };
        draft.extra.insert(REPLY_TARGET.into(), serde_json::json!({"message": {
            "id": latest.id, "account": latest.account, "from": latest.from, "to": latest.to, "cc": latest.cc,
            "message_id": latest.message_id, "references": latest.references, "timestamp": latest.timestamp
        }}));
    }
    draft.in_reply_to = latest.message_id.clone();
    let mut refs = latest.references.clone().unwrap_or_default();
    if let Some(id) = &latest.message_id {
        if !refs.split_whitespace().any(|r| r == id) {
            if !refs.is_empty() { refs.push(' '); }
            refs.push_str(id);
        }
    }
    draft.references = Some(refs);
    Ok(())
}

/// Forward the message the user opened, including real attachment bytes. A new
/// arrival cannot silently replace that target while the background GET runs.
pub(super) fn forward(client: &Client, thread_id: &str, target: &str, account: String, from: String) -> Result<api::DraftContent> {
    use base64::Engine;
    let content = client.thread(thread_id)?;
    let message = content.messages.iter().find(|m| m.id == target)
        .ok_or_else(|| super::client::Error::Decode("The original forward message is no longer available".into()))?;
    let now = chrono::Utc::now().to_rfc3339();
    let mut draft = api::DraftContent { id: uuid::Uuid::new_v4().to_string(), account_id: Some(account), from,
        subject: if content.subject.to_lowercase().starts_with("fwd:") { content.subject.clone() } else { format!("Fwd: {}", content.subject) },
        body: format!("\n\n---------- Forwarded message ----------\nFrom: {}\nDate: {}\nSubject: {}\n\n{}", message.from,
            chrono::DateTime::from_timestamp(message.timestamp, 0).map(|date| date.to_rfc3339()).unwrap_or_default(), content.subject, message.body),
        created_at: now.clone(), modified_at: now, ..Default::default() };
    for attachment in &message.attachments {
        if attachment.size > 1 << 20 { return Err(super::client::Error::Decode("Forward attachment exceeds the 1 MiB recovery draft limit".into())) }
        let data = client.draft_attachment(&message.id, attachment.part_id)?;
        draft.attachments.push(api::DraftAttachment { id: uuid::Uuid::new_v4().to_string(), filename: attachment.filename.clone(),
            mime_type: attachment.content_type.clone(), data: base64::engine::general_purpose::STANDARD.encode(data) });
        if !draft_fits(&draft) { return Err(super::client::Error::Decode("Forward exceeds the 1 MiB recovery draft limit; no partial forward was saved".into())) }
    }
    if !draft_fits(&draft) { return Err(super::client::Error::Decode("Forward exceeds the 1 MiB recovery draft limit".into())) }
    Ok(draft)
}

/// Tag state overlay applies to old replies/search pages until writes drain.
pub fn apply_tags(thread: &mut crate::model::Thread, ops: &str) {
    for op in ops.split_whitespace() {
        let (sign, tag) = op.split_at(1);
        if sign == "+" {
            if matches!(tag, "inbox" | "archive" | "trash") {
                thread.tags.retain(|t| !matches!(t.as_ref(), "inbox" | "archive" | "trash"));
            }
            if !thread.tags.iter().any(|t| t == tag) { thread.tags.push(tag.to_string().into()); }
        } else { thread.tags.retain(|t| t != tag); }
        if tag == "flagged" { thread.starred = sign == "+"; }
        if tag == "unread" {
            for item in &mut thread.items {
                if let crate::model::ThreadItem::Message(m) = item { m.read = sign != "+"; }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{io::{BufRead, BufReader, Read, Write}, net::TcpListener, sync::{Arc, Mutex}};

    fn draft() -> api::DraftContent {
        api::DraftContent { id: "draft-identity".into(), from: "Me <me@test.invalid>".into(),
            to: vec!["Alex <alex.one@test.invalid>".into()], cc: vec!["alex.two@test.invalid".into()],
            bcc: vec!["blind@test.invalid".into()], body: "Unsent asymmetric body".into(), subject: "Subject".into(),
            in_reply_to: Some("<rfc-parent@test.invalid>".into()), references: Some("<root@test.invalid> <rfc-parent@test.invalid>".into()),
            attachments: vec![api::DraftAttachment { id: "attachment".into(), filename: "empty.txt".into(), mime_type: "text/plain".into(), data: String::new() }],
            ..Default::default() }
    }

    /// Actual blocking transport, including an accepted enqueue whose response
    /// disappears. A retry must carry exactly the same frozen logical request.
    #[test]
    fn lost_enqueue_response_keeps_key_payload_and_recovery() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let server = format!("http://{}", listener.local_addr().unwrap());
        let received = Arc::new(Mutex::new(Vec::new()));
        let capture = received.clone();
        let thread = std::thread::spawn(move || {
            for ix in 0..5 {
                let (mut stream, _) = listener.accept().unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut headers = String::new();
                let mut length = 0;
                loop {
                    let mut line = String::new(); reader.read_line(&mut line).unwrap();
                    if line == "\r\n" { break }
                    if let Some(value) = line.to_lowercase().strip_prefix("content-length:") { length = value.trim().parse().unwrap(); }
                    headers.push_str(&line);
                }
                assert!(headers.to_lowercase().contains("authorization: bearer fixture-token"));
                let mut bytes = vec![0; length]; reader.read_exact(&mut bytes).unwrap();
                let body: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
                capture.lock().unwrap().push((headers, body));
                if ix == 1 { continue } // server accepted the send but client lost its answer
                let body = if ix == 3 { r#"{"ok":true,"id":71,"send_after":4102444800}"# } else { r#"{"ok":true}"# };
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
            }
        });
        let client = Client::new(&server, Some("fixture-token".into()));
        let mut intent = Intent::Send { draft: draft(), local_id: 1, reply_all: None };
        assert!(matches!(execute(&client, &mut intent), Err(super::super::client::Error::Unreachable(_))));
        if let Intent::Send { draft, .. } = &mut intent {
            assert!(draft.extra.contains_key(PENDING_SEND));
            draft.body = "Edited after unknown outcome must not change enqueue".into();
        }
        let Outcome::Sent(item) = execute(&client, &mut intent).unwrap() else { panic!("not queued") };
        assert_eq!(item.id, 71);
        thread.join().unwrap();
        let requests = received.lock().unwrap();
        assert!(requests[0].0.starts_with("PUT /api/v1/local-drafts/draft-identity "));
        assert_eq!(requests[0].1["draft_json"][PENDING_SEND], requests[1].1);
        assert_eq!(requests[1].1, requests[3].1);
        assert_eq!(requests[3].1["body"], "Unsent asymmetric body");
        assert_eq!(requests[3].1["bcc"][0], "blind@test.invalid");
        assert_eq!(requests[3].1["in_reply_to"], "<rfc-parent@test.invalid>");
        assert_eq!(requests[3].1["attachments"][0]["data_base64"], "");
    }

    #[test]
    fn replies_use_real_addresses_and_rfc_headers_not_local_row_ids() {
        let content: api::ThreadContent = serde_json::from_str(r#"{"thread_id":"global","subject":"Topic","messages":[
            {"id":"local:92","message_id":"<parent@test.invalid>","references":"<root@test.invalid>","timestamp":8,"from":"Alex <alex.two@test.invalid>","to":"Me <me@test.invalid>, Alex <alex.one@test.invalid>","cc":"other@test.invalid, ME@test.invalid"},
            {"id":"local:17","message_id":"<old@test.invalid>","timestamp":3,"from":"old@test.invalid"}]}"#).unwrap();
        let mut draft = draft();
        prepare_reply(&mut draft, &content, true).unwrap();
        let payload = outbox(&draft).unwrap();
        assert_eq!(payload.to, ["alex.two@test.invalid", "alex.one@test.invalid"]);
        assert_eq!(payload.cc, ["other@test.invalid"]);
        assert!(payload.bcc.is_empty());
        assert_eq!(payload.in_reply_to, "<parent@test.invalid>");
        assert_eq!(payload.references, "<root@test.invalid> <parent@test.invalid>");
        assert!(!payload.references.contains("local:"));
        prepare_reply(&mut draft, &content, false).unwrap();
        assert_eq!(outbox(&draft).unwrap().to, ["alex.two@test.invalid"]);
        assert!(draft.cc.is_empty());
        draft.account_id = Some("original-account".into());
        draft.extra.insert(OWN_ADDRESSES.into(), serde_json::json!(["me@test.invalid", "alias@test.invalid"]));
        let arrived: api::ThreadContent = serde_json::from_str(r#"{"thread_id":"global","subject":"Changed","messages":[
            {"id":"local:100","account":"other-account","message_id":"<new@test.invalid>","timestamp":999,"from":"Alex <new-alex@test.invalid>","to":"alias@test.invalid"}]}"#).unwrap();
        // Simulate recovery/autosave, then toggle mode after a newer message.
        let mut recovered: api::DraftContent = serde_json::from_value(serde_json::to_value(draft).unwrap()).unwrap();
        prepare_reply(&mut recovered, &arrived, true).unwrap();
        assert_eq!(recovered.account_id.as_deref(), Some("original-account"));
        assert_eq!(recovered.from, "Me <me@test.invalid>");
        assert_eq!(recovered.subject, "Re: Topic");
        assert_eq!(recovered.to, ["alex.two@test.invalid", "alex.one@test.invalid"]);
        assert_eq!(recovered.cc, ["other@test.invalid"]);
        assert_eq!(recovered.in_reply_to.as_deref(), Some("<parent@test.invalid>"));
        assert_eq!(recovered.references.as_deref(), Some("<root@test.invalid> <parent@test.invalid>"));
    }

    #[test]
    fn serialized_writes_coalesce_saves_without_crossing_delete_or_send() {
        let mut queue = Queue::default();
        queue.push(Intent::Save { draft: draft() }).unwrap();
        let (key, mut first) = queue.next().unwrap();
        let mut newer = draft(); newer.body = "Newer".into();
        queue.push(Intent::Save { draft: newer.clone() }).unwrap();
        newer.body = "Newest".into(); queue.push(Intent::Save { draft: newer }).unwrap();
        queue.push(Intent::Delete { id: draft().id, local_id: 1 }).unwrap();
        queue.push(Intent::Send { draft: draft(), local_id: 1, reply_all: None }).unwrap();
        assert!(queue.next().is_none(), "one logical draft cannot overlap");
        queue.finished(&key);
        let (key, last_save) = queue.next().unwrap();
        assert!(matches!(last_save, Intent::Save { ref draft } if draft.body == "Newest"));
        queue.finished(&key);
        let (key, deletion) = queue.next().unwrap(); assert!(matches!(deletion, Intent::Delete { .. }));
        queue.finished(&key);
        assert!(matches!(queue.next().unwrap().1, Intent::Send { .. }));
        // Reject oversized recovery before any transport can be attempted.
        if let Intent::Save { draft } = &mut first { draft.body = "x".repeat(1 << 20); }
        let unreachable = Client::new("http://127.0.0.1:1", None);
        assert!(matches!(execute(&unreachable, &mut first), Err(super::super::client::Error::Decode(_))));
    }

    #[test]
    fn bounded_queue_rejects_without_discarding_accepted_intents_and_caps_running_keys() {
        let mut queue = Queue::default();
        for i in 0..128 { queue.push(Intent::Tags { thread: format!("global-{i}"), ops: "+flagged".into() }).unwrap(); }
        assert!(queue.push(Intent::Delete { id: "draft-preserved".into(), local_id: 9 }).is_err());
        let mut running = Vec::new();
        for _ in 0..4 { running.push(queue.next().unwrap().0); }
        assert!(queue.next().is_none(), "a fifth transport must wait");
        for key in running { queue.finished(&key); }
        let mut drained = 4;
        while let Some((key, _)) = queue.next() { queue.finished(&key); drained += 1; }
        assert_eq!(drained, 128, "rejection cannot drop other queued actions");
        for i in 0..7 {
            let mut d = draft(); d.id = format!("large-{i}"); d.body = "x".repeat(1 << 20);
            queue.push(Intent::Save { draft: d }).unwrap();
        }
        let mut d = draft(); d.id = "large-8".into(); d.body = "x".repeat(1 << 20);
        assert!(queue.push(Intent::Save { draft: d }).is_err(), "byte bound applies before 128 requests");
    }

    fn server(replies: Vec<(u16, Vec<u8>)>) -> (Client, std::thread::JoinHandle<Vec<(String, serde_json::Value)>>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client = Client::new(&format!("http://{}", listener.local_addr().unwrap()), Some("fixture-token".into()));
        let handle = std::thread::spawn(move || replies.into_iter().map(|(status, body)| {
            let (mut stream, _) = listener.accept().unwrap();
            let mut reader = BufReader::new(stream.try_clone().unwrap());
            let mut headers = String::new(); let mut length = 0;
            loop {
                let mut line = String::new(); reader.read_line(&mut line).unwrap();
                if line == "\r\n" { break }
                if let Some(value) = line.to_lowercase().strip_prefix("content-length:") { length = value.trim().parse().unwrap(); }
                headers.push_str(&line);
            }
            assert!(headers.to_lowercase().contains("authorization: bearer fixture-token"));
            let mut bytes = vec![0; length]; reader.read_exact(&mut bytes).unwrap();
            write!(stream, "HTTP/1.1 {status} X\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", body.len()).unwrap();
            stream.write_all(&body).unwrap();
            (headers, serde_json::from_slice(&bytes).unwrap_or(serde_json::Value::Null))
        }).collect());
        (client, handle)
    }

    #[test]
    fn undo_requires_confirmed_cancellation_before_rotating_identity() {
        for status in [404, 409, 500] {
            let (client, handle) = server(vec![(status, br#"{"error":"cancellation not confirmed"}"#.to_vec())]);
            let mut d = draft(); d.extra.insert(PENDING_SEND.into(), serde_json::to_value(outbox(&d).unwrap()).unwrap());
            d.extra.insert(QUEUED_SEND.into(), serde_json::json!({"id": 72, "send_after": 1}));
            let original = d.clone();
            let mut intent = Intent::Undo { id: 72, draft: d, local_id: 5 };
            assert!(execute(&client, &mut intent).is_err());
            let Intent::Undo { draft, .. } = intent else { panic!() };
            assert_eq!(draft, original, "{status} must not restore or generate another send key");
            assert!(handle.join().unwrap()[0].0.starts_with("DELETE /api/v1/outbox/72 "));
        }
        let (client, handle) = server(vec![(200, br#"{"ok":true}"#.to_vec()), (500, br#"{"error":"draft disk unavailable"}"#.to_vec())]);
        let mut d = draft(); let original_key = outbox(&d).unwrap().idempotency_key;
        d.extra.insert(PENDING_SEND.into(), serde_json::to_value(outbox(&d).unwrap()).unwrap());
        let mut intent = Intent::Undo { id: 72, draft: d, local_id: 5 };
        assert!(matches!(execute(&client, &mut intent).unwrap(), Outcome::Undone { persisted: false }));
        let Intent::Undo { draft, .. } = intent else { panic!() };
        assert_eq!(draft.id, "draft-identity");
        assert!(!draft.extra.contains_key(PENDING_SEND));
        assert_ne!(outbox(&draft).unwrap().idempotency_key, original_key);
        assert_eq!(handle.join().unwrap().len(), 2);
    }

    #[test]
    fn forward_transport_keeps_selected_message_and_real_binary_and_empty_attachments() {
        let content = br#"{"ok":true,"thread":{"thread_id":"global","subject":"Original","messages":[
            {"id":"local:2","timestamp":2,"from":"Alex <two@test.invalid>","body":"NEW WRONG BODY"},
            {"id":"local:1","timestamp":1,"from":"Alex <one@test.invalid>","body":"ORIGINAL BODY","attachments":[
                {"part_id":3,"filename":"empty.txt","content_type":"text/plain","size":0},
                {"part_id":9,"filename":"binary.bin","content_type":"application/octet-stream","size":3}]}]}}"#;
        let (client, handle) = server(vec![(200, content.to_vec()), (200, Vec::new()), (200, vec![0, 255, 4])]);
        let draft = forward(&client, "global", "local:1", "home".into(), "alias@test.invalid".into()).unwrap();
        assert!(draft.body.contains("ORIGINAL BODY") && !draft.body.contains("NEW WRONG BODY"));
        assert_eq!(draft.from, "alias@test.invalid");
        assert_eq!(draft.account_id.as_deref(), Some("home"));
        assert!(draft.in_reply_to.is_none() && draft.references.is_none());
        assert_eq!(draft.attachments[0].data, "");
        assert_eq!(draft.attachments[1].data, "AP8E");
        let requests = handle.join().unwrap();
        assert!(requests[1].0.starts_with("GET /api/v1/messages/local%3A1/attachments/3 "));
        assert!(requests[2].0.starts_with("GET /api/v1/messages/local%3A1/attachments/9 "));
    }

    #[test]
    fn durable_outbox_and_sse_results_do_not_invent_success_after_absence() {
        let rows = br#"[{"id":1,"message_id":"<a>","in_flight":false,"delivery_confirmed":false,"last_error":"Rejected"},
            {"id":2,"message_id":"<b>","in_flight":true,"delivery_confirmed":false,"last_error":"Delivery status is unknown. Verify the provider outcome before reconciling."},
            {"id":3,"message_id":"<c>","in_flight":true,"delivery_confirmed":true,"last_error":"Sent filing failed"}]"#;
        let (client, handle) = server(vec![(200, rows.to_vec()), (200, b"[]".to_vec())]);
        let entries = client.outbox().unwrap();
        assert_eq!(Delivery::reconcile(None, Some(&entries[0])), Delivery::Failed("Rejected".into()));
        assert_eq!(Delivery::reconcile(None, Some(&entries[1])), Delivery::ReconciliationRequired);
        let confirmed = Delivery::reconcile(None, Some(&entries[2]));
        assert_eq!(confirmed, Delivery::DeliveredWithWarning("Sent filing failed".into()));
        assert!(client.outbox().unwrap().is_empty());
        assert_eq!(Delivery::reconcile(Some(&Delivery::Queued), None), Delivery::Unknown);
        assert_eq!(Delivery::reconcile(Some(&confirmed), None), confirmed);
        assert_eq!(Delivery::reconcile(Some(&Delivery::Sent), None), Delivery::Sent);
        handle.join().unwrap();
    }

    /// Run only against integration's disposable encrypted Go server. No provider
    /// traffic: the frozen payload has a one-hour delay and is cancelled below.
    #[test]
    #[ignore]
    fn real_go_mail_write_contract() {
        let server = std::env::var("DURIAN_TEST_URL").unwrap();
        let client = Client::new(&server, Some(std::env::var("DURIAN_TEST_TOKEN").unwrap()));
        let bad = Client::new(&server, Some("invalid".into()));
        assert!(matches!(bad.save_draft(&draft()), Err(super::super::client::Error::Unauthorized)));
        let mut recovery = draft(); recovery.id = uuid::Uuid::new_v4().to_string();
        client.save_draft(&recovery).unwrap();
        let stored = client.drafts().unwrap().into_iter().find(|d| d.id == recovery.id).unwrap();
        assert_eq!(serde_json::from_value::<api::DraftContent>(stored.draft_json).unwrap(), recovery);
        let mut payload = outbox(&recovery).unwrap(); payload.delay_seconds = 3600;
        recovery.extra.insert(PENDING_SEND.into(), serde_json::to_value(&payload).unwrap());
        let mut intent = Intent::Send { draft: recovery.clone(), local_id: 1, reply_all: None };
        let Outcome::Sent(first) = execute(&client, &mut intent).unwrap() else { panic!() };
        let Outcome::Sent(second) = execute(&client, &mut intent).unwrap() else { panic!() };
        assert_eq!(first.id, second.id);
        assert_eq!(first.send_after, second.send_after);
        assert!(first.send_after > chrono::Utc::now().timestamp() + 3500);
        client.cancel_send(first.id).unwrap();
        assert!(matches!(client.cancel_send(first.id), Err(super::super::client::Error::NotFound(_))));
        client.delete_draft(&recovery.id).unwrap();
        assert!(!client.drafts().unwrap().iter().any(|d| d.id == recovery.id));
        let found = client.search("tag:inbox", 200, 0).unwrap();
        let id = &found.results[0].thread_id;
        client.tag_thread(id, "+gpui-contract-label +flagged -unread").unwrap();
        let content = client.thread(id).unwrap();
        assert!(content.messages.iter().all(|m| m.tags.iter().any(|t| t == "gpui-contract-label") && !m.tags.iter().any(|t| t == "unread")));
        client.tag_thread(id, "-gpui-contract-label -flagged").unwrap();
    }
}
