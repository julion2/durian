//! Durable composer state. HTTP only; never opens the mail database or SMTP.
//!
//! A send intent is saved before contacting the outbox. From that point its
//! content and idempotency key are immutable, including after restart. Loading
//! drafts never retries sends; retry is an explicit user action. These records
//! have their own schema/ID namespace and do not overwrite Swift/IMAP drafts.

use serde::{Deserialize, Serialize};
use std::time::Duration;

const SCHEMA: &str = "durian-gpui-draft-v1";
const PREFIX: &str = "gpui-";
const MAX_DRAFT_BYTES: usize = 1 << 20;

/// Plain-text wire content, shared by the saved draft and outbox request.
/// Recipient entries are mailbox strings, not comma-separated lists. The form
/// owns parsing/validation; partial recipient entries may be saved as drafts.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Content {
    pub from: String,
    pub to: Vec<String>,
    pub cc: Vec<String>,
    pub bcc: Vec<String>,
    pub subject: String,
    pub body: String,
    pub in_reply_to: String,
    pub references: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Receipt {
    pub id: i64,
    pub send_after: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "state", rename_all = "snake_case")]
enum Delivery {
    Editing,
    Pending,
    Queued { receipt: Receipt },
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Draft {
    schema: String,
    id: String,
    content: Content,
    delivery: Delivery,
}

impl Draft {
    pub fn new(content: Content) -> Self {
        Self {
            schema: SCHEMA.into(),
            id: format!("{PREFIX}{}", uuid::Uuid::new_v4()),
            content,
            delivery: Delivery::Editing,
        }
    }

    pub fn id(&self) -> &str {
        &self.id
    }

    pub fn content(&self) -> &Content {
        &self.content
    }

    pub fn is_editable(&self) -> bool {
        self.delivery == Delivery::Editing
    }

    pub fn replace_content(&mut self, content: Content) -> Result<(), &'static str> {
        if !self.is_editable() {
            return Err("This send has started. Retry its status before editing another message.");
        }
        self.content = content;
        Ok(())
    }

    pub fn receipt(&self) -> Option<Receipt> {
        match self.delivery {
            Delivery::Queued { receipt } => Some(receipt),
            _ => None,
        }
    }

    fn validate_record(&self, id: &str) -> Result<(), &'static str> {
        let valid_id = id
            .strip_prefix(PREFIX)
            .is_some_and(|suffix| uuid::Uuid::parse_str(suffix).is_ok());
        if !valid_id
            || self.id != id
            || self.schema != SCHEMA
            || self.receipt().is_some_and(|receipt| receipt.id <= 0)
        {
            return Err("Unsupported or inconsistent GPUI draft. It has not been changed.");
        }
        Ok(())
    }

    fn save_body(&self) -> Result<Vec<u8>, &'static str> {
        self.validate_record(self.id())?;
        let body = serde_json::to_vec(&serde_json::json!({ "draft_json": self }))
            .map_err(|_| "Couldn’t encode the draft.")?;
        if body.len() > MAX_DRAFT_BYTES {
            return Err("Draft exceeds the server’s 1 MiB limit.");
        }
        Ok(body)
    }
}

/// A receipt confirms enqueue, never provider delivery. Even if saving the
/// receipt fails, the earlier Pending record retains the same retry key.
#[derive(Debug)]
pub struct Enqueued {
    pub receipt: Receipt,
    pub receipt_saved: bool,
}

#[derive(Debug, PartialEq, Eq)]
pub enum SendError {
    Invalid,
    SaveFailed,
    /// The request may have committed. Do not unlock content or mint a key.
    OutcomeUnknown,
}

pub struct Api {
    base: String,
    agent: ureq::Agent,
}

impl Api {
    /// Only construct on explicit live-mode opt-in. Like the existing prototype
    /// reads, this requires a local `durian serve --no-auth`.
    pub fn local() -> Self {
        Self::new(crate::data::API_BASE)
    }

    fn new(base: &str) -> Self {
        Self {
            base: base.into(),
            agent: ureq::Agent::config_builder()
                .timeout_global(Some(Duration::from_secs(10)))
                // Never replay a write or disclose a draft across a redirect.
                .max_redirects(0)
                .build()
                .into(),
        }
    }

    pub fn save(&self, draft: &Draft) -> Result<(), &'static str> {
        let body = draft.save_body()?;
        let value: serde_json::Value = self
            .agent
            .put(format!("{}/api/v1/local-drafts/{}", self.base, draft.id()))
            .header("Content-Type", "application/json")
            .send(body)
            .map_err(|_| "Couldn’t save the draft. Your changes remain in this window.")?
            .body_mut()
            .read_json()
            .map_err(|_| "The server did not confirm saving the draft.")?;
        if value.get("ok") != Some(&serde_json::Value::Bool(true)) {
            return Err("The server did not confirm saving the draft.");
        }
        Ok(())
    }

    /// Other clients' opaque JSON and unknown future schemas remain untouched.
    /// A malformed record bearing our exact schema is an error, not an empty
    /// successful list that could conceal an unconfirmed send.
    pub fn list(&self) -> Result<Vec<Draft>, &'static str> {
        #[derive(Deserialize)]
        struct Record {
            id: String,
            draft_json: serde_json::Value,
        }
        let records: Vec<Record> = self
            .agent
            .get(format!("{}/api/v1/local-drafts", self.base))
            .call()
            .map_err(|_| "Couldn’t load saved drafts. Retry when the server is available.")?
            .body_mut()
            .read_json()
            .map_err(|_| "The server returned an invalid draft list.")?;
        let mut drafts = Vec::new();
        for record in records {
            if !record.id.starts_with(PREFIX)
                || record.draft_json.get("schema").and_then(|v| v.as_str()) != Some(SCHEMA)
            {
                continue;
            }
            let draft: Draft = serde_json::from_value(record.draft_json)
                .map_err(|_| "A saved GPUI draft is invalid. It has not been changed.")?;
            draft.validate_record(&record.id)?;
            drafts.push(draft);
        }
        Ok(drafts)
    }

    /// Blocking. The UI must run this on the background executor, serialize
    /// operations per draft, and freeze its fields until the result is known.
    pub fn enqueue(&self, draft: &mut Draft) -> Result<Enqueued, SendError> {
        if let Some(receipt) = draft.receipt() {
            return Ok(Enqueued {
                receipt,
                receipt_saved: self.save(draft).is_ok(),
            });
        }
        let content = &draft.content;
        if content.from.trim().is_empty()
            || content.to.is_empty()
            || content.to.iter().any(|to| to.trim().is_empty())
        {
            return Err(SendError::Invalid);
        }
        let prior = std::mem::replace(&mut draft.delivery, Delivery::Pending);
        if draft.save_body().is_err() {
            // No HTTP request has started, so a local validation failure must
            // not strand an editable draft (e.g. just above the size limit).
            draft.delivery = prior;
            return Err(SendError::Invalid);
        }
        // A failed PUT is itself ambiguous. Keep the record frozen: a crash
        // may recover this Pending intent, and both sessions must agree on it.
        self.save(draft).map_err(|_| SendError::SaveFailed)?;
        let payload = serde_json::json!({
            "idempotency_key": draft.id(),
            "from": draft.content.from,
            "to": draft.content.to,
            "cc": draft.content.cc,
            "bcc": draft.content.bcc,
            "subject": draft.content.subject,
            "body": draft.content.body,
            "is_html": false,
            "in_reply_to": draft.content.in_reply_to,
            "references": draft.content.references,
            "attachments": [],
            "delay_seconds": 0,
        });
        #[derive(Deserialize)]
        struct Response {
            ok: bool,
            #[serde(flatten)]
            receipt: Receipt,
        }
        let response: Response = self
            .agent
            .post(format!("{}/api/v1/outbox/send", self.base))
            .send_json(&payload)
            .map_err(|_| SendError::OutcomeUnknown)?
            .body_mut()
            .read_json()
            .map_err(|_| SendError::OutcomeUnknown)?;
        if !response.ok || response.receipt.id <= 0 {
            return Err(SendError::OutcomeUnknown);
        }
        draft.delivery = Delivery::Queued {
            receipt: response.receipt,
        };
        Ok(Enqueued {
            receipt: response.receipt,
            receipt_saved: self.save(draft).is_ok(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::{Value, json};
    use std::io::{BufRead, BufReader, Read, Write};
    use std::net::TcpListener;
    use std::sync::{Arc, Mutex};
    use std::thread;
    use std::time::Instant;

    type Requests = Arc<Mutex<Vec<(String, Value)>>>;

    // A disposable HTTP sink, never SMTP. No fixed port or real account data.
    // None deliberately loses the response after accepting a complete request.
    fn server(responses: Vec<Option<(u16, Value)>>) -> (Api, Requests, thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let api = Api::new(&format!("http://{}", listener.local_addr().unwrap()));
        let requests: Requests = Arc::default();
        let captured = requests.clone();
        let task = thread::spawn(move || {
            for response in responses {
                let deadline = Instant::now() + Duration::from_secs(5);
                let (mut stream, _) = loop {
                    match listener.accept() {
                        Ok(connection) => break connection,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < deadline, "expected HTTP request missing");
                            thread::sleep(Duration::from_millis(5));
                        }
                        Err(error) => panic!("accept: {error}"),
                    }
                };
                // Accepted sockets may inherit the listener's nonblocking mode
                // on BSD/macOS. This HTTP reader needs blocking I/O + timeouts.
                stream.set_nonblocking(false).unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                stream
                    .set_write_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                reader.read_line(&mut line).unwrap();
                let request_line = line.trim().to_string();
                let mut length = 0;
                let mut chunked = false;
                loop {
                    line.clear();
                    reader.read_line(&mut line).unwrap();
                    if line == "\r\n" {
                        break;
                    }
                    let (name, value) = line.split_once(':').unwrap();
                    if name.eq_ignore_ascii_case("content-length") {
                        length = value.trim().parse::<usize>().unwrap();
                    }
                    if name.eq_ignore_ascii_case("transfer-encoding") {
                        chunked = value.trim() == "chunked";
                    }
                }
                let mut body = Vec::new();
                if chunked {
                    loop {
                        line.clear();
                        reader.read_line(&mut line).unwrap();
                        let size = usize::from_str_radix(line.trim(), 16).unwrap();
                        if size == 0 {
                            break;
                        }
                        let offset = body.len();
                        body.resize(offset + size, 0);
                        reader.read_exact(&mut body[offset..]).unwrap();
                        let mut crlf = [0; 2];
                        reader.read_exact(&mut crlf).unwrap();
                        assert_eq!(&crlf, b"\r\n");
                    }
                } else {
                    body.resize(length, 0);
                    reader.read_exact(&mut body).unwrap();
                }
                captured.lock().unwrap().push((
                    request_line,
                    if body.is_empty() {
                        Value::Null
                    } else {
                        serde_json::from_slice(&body).unwrap()
                    },
                ));
                if let Some((status, body)) = response {
                    let body = body.to_string();
                    write!(stream, "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).unwrap();
                }
            }
        });
        (api, requests, task)
    }

    fn ok() -> Option<(u16, Value)> {
        Some((200, json!({"ok": true})))
    }
    fn receipt() -> Option<(u16, Value)> {
        Some((200, json!({"ok": true, "id": 73, "send_after": 1800000017})))
    }
    fn draft() -> Draft {
        Draft::new(Content {
            from: "sender@example.test".into(),
            to: vec!["\"Dœ, Jane\" <jane@example.test>".into()],
            cc: vec!["review@example.test".into()],
            bcc: vec!["private@example.test".into()],
            subject: "Re: Überblick".into(),
            body: "Grüße 👋\n\n> earlier message".into(),
            in_reply_to: "<reply@example.test>".into(),
            references: "<root@example.test> <reply@example.test>".into(),
        })
    }

    #[test]
    fn saves_pending_before_post_and_records_enqueue_not_delivery() {
        let (api, requests, task) = server(vec![ok(), receipt(), ok()]);
        let mut draft = draft();
        let outcome = api.enqueue(&mut draft).unwrap();
        task.join().unwrap();
        let requests = requests.lock().unwrap();
        assert_eq!(requests.len(), 3);
        assert_eq!(
            requests[0].0,
            format!("PUT /api/v1/local-drafts/{} HTTP/1.1", draft.id())
        );
        assert_eq!(
            requests[0].1["draft_json"]["delivery"],
            json!({"state": "pending"})
        );
        assert_eq!(requests[1].0, "POST /api/v1/outbox/send HTTP/1.1");
        assert_eq!(
            requests[1].1,
            json!({
                "idempotency_key": draft.id(), "from": "sender@example.test",
                "to": ["\"Dœ, Jane\" <jane@example.test>"], "cc": ["review@example.test"],
                "bcc": ["private@example.test"], "subject": "Re: Überblick",
                "body": "Grüße 👋\n\n> earlier message", "is_html": false,
                "in_reply_to": "<reply@example.test>",
                "references": "<root@example.test> <reply@example.test>",
                "attachments": [], "delay_seconds": 0,
            })
        );
        assert_eq!(
            requests[2].1["draft_json"]["delivery"],
            json!({
                "state": "queued", "receipt": {"id": 73, "send_after": 1800000017}
            })
        );
        assert_eq!(
            outcome.receipt,
            Receipt {
                id: 73,
                send_after: 1800000017
            }
        );
        assert!(outcome.receipt_saved);
        assert!(!draft.is_editable());
    }

    #[test]
    fn lost_enqueue_response_restores_frozen_intent_and_retries_exact_payload() {
        let (api, requests, task) = server(vec![ok(), None]);
        let mut draft = draft();
        assert_eq!(
            api.enqueue(&mut draft).unwrap_err(),
            SendError::OutcomeUnknown
        );
        task.join().unwrap();
        let requests = requests.lock().unwrap();
        assert_eq!(requests.len(), 2, "no implicit POST retry");
        let saved = requests[0].1["draft_json"].clone();
        let first_post = requests[1].1.clone();
        let list = json!([{ "id": draft.id(), "draft_json": saved }]);
        // New client + server connection simulates the process restarting.
        let (api, retries, task) = server(vec![Some((200, list)), ok(), receipt(), ok()]);
        let mut restored = api.list().unwrap().pop().unwrap();
        assert_eq!(restored.id(), draft.id());
        assert!(!restored.is_editable());
        let mut edited = restored.content().clone();
        edited.to = vec!["different@example.test".into()];
        assert!(restored.replace_content(edited).is_err());
        assert_eq!(api.enqueue(&mut restored).unwrap().receipt.id, 73);
        task.join().unwrap();
        assert_eq!(retries.lock().unwrap()[2].1, first_post);
    }

    #[test]
    fn failed_save_never_posts_and_retains_content() {
        for response in [
            Some((500, json!({}))),
            Some((200, json!({"ok": false}))),
            None,
        ] {
            let (api, requests, task) = server(vec![response]);
            let mut draft = draft();
            let content = draft.content().clone();
            assert_eq!(api.enqueue(&mut draft).unwrap_err(), SendError::SaveFailed);
            task.join().unwrap();
            assert_eq!(requests.lock().unwrap().len(), 1);
            assert_eq!(draft.content(), &content);
            assert!(!draft.is_editable());
        }
    }

    #[test]
    fn receipt_save_failure_is_still_queued_and_same_session_never_posts_again() {
        let (api, requests, task) = server(vec![ok(), receipt(), None, ok()]);
        let mut draft = draft();
        let result = api.enqueue(&mut draft).unwrap();
        assert_eq!(result.receipt.id, 73);
        assert!(!result.receipt_saved);
        assert_eq!(draft.receipt(), Some(result.receipt));
        assert!(api.enqueue(&mut draft).unwrap().receipt_saved);
        task.join().unwrap();
        assert_eq!(
            requests
                .lock()
                .unwrap()
                .iter()
                .filter(|(r, _)| r.starts_with("POST"))
                .count(),
            1
        );
    }

    #[test]
    fn partial_drafts_round_trip_without_touching_other_clients() {
        let mut draft = Draft::new(Content::default());
        draft
            .replace_content(Content {
                to: vec!["unfinished recipient".into()],
                body: "文面".into(),
                ..Default::default()
            })
            .unwrap();
        let list = json!([
            {"id": "swift-draft", "draft_json": {"body": "leave untouched"}},
            {"id": "gpui-future", "draft_json": {"schema": "durian-gpui-draft-v2"}},
            {"id": draft.id(), "draft_json": draft}
        ]);
        let (api, requests, task) = server(vec![ok(), Some((200, list))]);
        api.save(&draft).unwrap();
        assert_eq!(api.list().unwrap(), vec![draft]);
        task.join().unwrap();
        assert_eq!(
            requests.lock().unwrap().len(),
            2,
            "listing must not send or repair drafts"
        );
    }

    #[test]
    fn corrupt_owned_record_is_not_silently_hidden_or_reassigned() {
        let draft = draft();
        let wrong_id = Draft::new(Content::default()).id().to_string();
        for data in [
            json!({"schema": SCHEMA}),
            serde_json::to_value(draft).unwrap(),
        ] {
            let (api, _, task) = server(vec![Some((
                200,
                json!([{"id": wrong_id, "draft_json": data}]),
            ))]);
            assert!(api.list().is_err());
            task.join().unwrap();
        }
    }

    #[test]
    fn invalid_envelope_stays_editable_without_network_access() {
        let (api, requests, task) = server(vec![]);
        let mut draft = Draft::new(Content::default());
        assert_eq!(api.enqueue(&mut draft).unwrap_err(), SendError::Invalid);
        assert!(draft.is_editable());
        task.join().unwrap();
        assert!(requests.lock().unwrap().is_empty());
    }

    #[test]
    fn size_limit_counts_utf8_json_and_does_not_freeze_oversized_drafts() {
        let mut draft = Draft::new(Content {
            from: "a@example.test".into(),
            to: vec!["b@example.test".into()],
            ..Default::default()
        });
        // Independently specified wire envelope; every generated v4 ID has
        // the same byte length. Do not derive the boundary from save_body.
        let envelope_bytes = br#"{"draft_json":{"schema":"durian-gpui-draft-v1","id":"gpui-00000000-0000-0000-0000-000000000000","content":{"from":"a@example.test","to":["b@example.test"],"cc":[],"bcc":[],"subject":"","body":"","in_reply_to":"","references":""},"delivery":{"state":"editing"}}}"#.len();
        draft.content.body = "x".repeat(1_048_576 - envelope_bytes);
        assert_eq!(draft.save_body().unwrap().len(), 1_048_576);
        // A character count or body-only bound would miss this boundary.
        draft.content.body.push('é');
        let (api, requests, task) = server(vec![]);
        assert!(api.save(&draft).is_err());
        assert_eq!(api.enqueue(&mut draft).unwrap_err(), SendError::Invalid);
        assert!(draft.is_editable());
        task.join().unwrap();
        assert!(requests.lock().unwrap().is_empty());
    }
}
