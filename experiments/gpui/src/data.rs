//! Mail data model plus a tiny HTTP client for the Durian CLI API.
//!
//! The GUI must never touch the SQLite store directly, so everything comes
//! through `durian serve` on `localhost:9723` (see `openapi.yaml`). When the
//! server is not reachable we fall back to seed data so the UI can still be
//! explored standalone.
//! API calls are blocking and only support the unauthenticated local
//! `durian serve --no-auth` experiment; this module does not accept or log
//! credentials.
//!
//! Shared by both spikes (`gpui-kit` includes it via `#[path]`); each uses a
//! different subset, hence the blanket dead-code allowance.
#![allow(dead_code)]

use serde::Deserialize;

#[derive(Clone, Debug)]
pub struct Folder {
    pub name: &'static str,
    pub query: &'static str,
    pub icon: &'static str,
}

/// Folder list mirrors the default sidebar in the Qt spike / macOS app.
pub const FOLDERS: &[Folder] = &[
    Folder {
        name: "Inbox",
        query: "tag:inbox",
        icon: "icons/inbox.svg",
    },
    Folder {
        name: "Starred",
        query: "tag:flagged",
        icon: "icons/star.svg",
    },
    Folder {
        name: "Sent",
        query: "tag:sent",
        icon: "icons/send.svg",
    },
    Folder {
        name: "Drafts",
        query: "tag:draft",
        icon: "icons/file.svg",
    },
    Folder {
        name: "Archive",
        query: "tag:archive",
        icon: "icons/archive.svg",
    },
    Folder {
        name: "Trash",
        query: "tag:deleted",
        icon: "icons/trash.svg",
    },
];

#[derive(Clone, Debug, Default)]
pub struct ThreadPreview {
    pub thread_id: String,
    pub subject: String,
    pub sender: String,
    pub preview: String,
    pub date: String,
    pub tags: Vec<String>,
    pub unread: bool,
    pub message_count: usize,
}

#[derive(Clone, Debug, Default)]
pub struct Message {
    pub id: String,
    pub from: String,
    pub to: String,
    pub date: String,
    pub body: String,
    pub html: String,
    pub attachments: Vec<Attachment>,
}

#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct Attachment {
    pub part_id: u32,
    pub filename: String,
    pub content_type: String,
    pub size: u64,
    pub disposition: String,
    pub content_id: String,
}

#[derive(Clone, Debug, Deserialize)]
pub struct Contact {
    #[serde(default)]
    pub name: String,
    pub email: String,
}

#[derive(Clone, Debug, Default)]
pub struct Thread {
    pub subject: String,
    /// Newest first, matching the Durian API.
    pub messages: Vec<Message>,
}

// MARK: - API types (subset of openapi.yaml)

#[derive(Deserialize)]
struct SearchResponse {
    ok: bool,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    results: Vec<MailSearchResult>,
    #[serde(default)]
    threads: std::collections::HashMap<String, ThreadContent>,
}

#[derive(Deserialize)]
struct MailSearchResult {
    thread_id: String,
    #[serde(default)]
    subject: String,
    #[serde(default)]
    from: String,
    #[serde(default)]
    date: String,
    #[serde(default)]
    tags: String,
}

#[derive(Deserialize, Clone)]
struct ThreadContent {
    #[serde(default)]
    subject: String,
    #[serde(default)]
    messages: Vec<ThreadMessage>,
}

#[derive(Deserialize, Clone)]
struct ThreadMessage {
    #[serde(default)]
    id: String,
    #[serde(default)]
    from: String,
    #[serde(default)]
    to: String,
    #[serde(default)]
    date: String,
    #[serde(default)]
    body: String,
    #[serde(default)]
    html: String,
    #[serde(default)]
    attachments: Vec<Attachment>,
    #[serde(default)]
    tags: Vec<String>,
}

#[derive(Deserialize)]
struct TagsResponse {
    ok: bool,
    #[serde(default)]
    error: Option<String>,
    #[serde(default)]
    tags: Vec<String>,
}

pub const API_BASE: &str = "http://localhost:9723";
const ATTACHMENT_MAX_BYTES: u64 = 20 * 1024 * 1024;
const LOCAL_API_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(30);

/// Runs a search against `durian serve --no-auth`. Blocking; call from the
/// background executor.
pub fn search(query: &str, limit: usize) -> Result<Vec<(ThreadPreview, Thread)>, String> {
    let url = format!("{API_BASE}/api/v1/search");
    let response: SearchResponse = local_api_agent()
        .get(&url)
        .query("query", query)
        .query("limit", limit.to_string())
        // enrich = max number of threads that get full message content
        .query("enrich", limit.to_string())
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("invalid JSON: {e}"))?;

    decode_response(response)
}

/// Searches the contact index exposed by `durian serve --no-auth`.
///
/// The endpoint intentionally returns a raw JSON array rather than the usual
/// Durian response envelope. Blocking; call from the background executor.
pub fn contacts(query: &str, limit: usize) -> Result<Vec<Contact>, String> {
    let url = format!("{API_BASE}/api/v1/contacts/search");
    local_api_agent()
        .get(&url)
        .query("query", query)
        .query("limit", limit.to_string())
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("invalid JSON: {e}"))
}

/// Lists tags from the standard Durian response envelope. Blocking; call from
/// the background executor.
pub fn tags() -> Result<Vec<String>, String> {
    let url = format!("{API_BASE}/api/v1/tags");
    let response: TagsResponse = local_api_agent()
        .get(&url)
        .call()
        .map_err(|e| format!("request failed: {e}"))?
        .body_mut()
        .read_json()
        .map_err(|e| format!("invalid JSON: {e}"))?;

    decode_tags_response(response)
}

/// Downloads one attachment from `durian serve --no-auth`.
///
/// `message_id` is opaque and encoded as exactly one path segment. Downloads
/// have a 30-second end-to-end timeout and reject bodies larger than 20 MiB,
/// including chunked responses without a `Content-Length` header. Blocking;
/// call from the background executor.
pub fn attachment(message_id: &str, part_id: u32) -> Result<Vec<u8>, String> {
    use std::io::Read;

    let message_id = percent_encode_path_segment(message_id);
    let url = format!("{API_BASE}/api/v1/messages/{message_id}/attachments/{part_id}");
    // Read one extra byte so an exactly-20-MiB body succeeds while an
    // unknown-length oversized body is detected without unbounded allocation.
    // The outer `take` also bounds transparently decompressed response data.
    let mut response = local_api_agent()
        .get(&url)
        .call()
        .map_err(|e| format!("request failed: {e}"))?;
    let mut reader = response
        .body_mut()
        .with_config()
        .reader()
        .take(ATTACHMENT_MAX_BYTES + 1);
    let mut bytes = Vec::new();
    reader
        .read_to_end(&mut bytes)
        .map_err(|e| format!("attachment read failed: {e}"))?;
    if bytes.len() as u64 > ATTACHMENT_MAX_BYTES {
        return Err("attachment exceeds 20 MiB limit".into());
    }
    Ok(bytes)
}

fn local_api_agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .timeout_global(Some(LOCAL_API_TIMEOUT))
        .build()
        .into()
}

fn percent_encode_path_segment(value: &str) -> String {
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    let mut encoded = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            encoded.push(char::from(byte));
        } else {
            encoded.push('%');
            encoded.push(char::from(HEX[(byte >> 4) as usize]));
            encoded.push(char::from(HEX[(byte & 0x0f) as usize]));
        }
    }
    encoded
}

fn decode_tags_response(response: TagsResponse) -> Result<Vec<String>, String> {
    if !response.ok {
        return Err(response
            .error
            .unwrap_or_else(|| "API returned error".into()));
    }
    Ok(response.tags)
}

fn decode_response(response: SearchResponse) -> Result<Vec<(ThreadPreview, Thread)>, String> {
    if !response.ok {
        return Err(response
            .error
            .unwrap_or_else(|| "API returned error".into()));
    }

    Ok(response
        .results
        .into_iter()
        .map(|r| {
            let content = response.threads.get(&r.thread_id).cloned();
            let messages: Vec<Message> = content
                .as_ref()
                .map(|c| {
                    c.messages
                        .iter()
                        .map(|m| Message {
                            id: m.id.clone(),
                            from: m.from.clone(),
                            to: m.to.clone(),
                            date: m.date.clone(),
                            body: m.body.clone(),
                            html: m.html.clone(),
                            attachments: m.attachments.clone(),
                        })
                        .collect()
                })
                .unwrap_or_default();
            let unread = content
                .as_ref()
                .map(|c| {
                    c.messages
                        .iter()
                        .any(|m| m.tags.iter().any(|t| t == "unread"))
                })
                .unwrap_or(false);
            let preview = messages
                .first()
                .map(|m| first_line(&m.body))
                .unwrap_or_default();
            let tags: Vec<String> = r
                .tags
                .split(',')
                .map(|t| t.trim().to_string())
                .filter(|t| !t.is_empty())
                .collect();
            let thread = Thread {
                subject: content
                    .map(|c| c.subject)
                    .unwrap_or_else(|| r.subject.clone()),
                messages,
            };
            let preview = ThreadPreview {
                thread_id: r.thread_id,
                subject: r.subject,
                sender: display_name(&r.from),
                preview,
                date: r.date,
                tags,
                unread,
                message_count: thread.messages.len().max(1),
            };
            (preview, thread)
        })
        .collect())
}

fn first_line(body: &str) -> String {
    body.lines()
        .map(str::trim)
        .find(|l| !l.is_empty() && !l.starts_with('>'))
        .unwrap_or("")
        .chars()
        .take(120)
        .collect()
}

/// "Jane Doe <jane@example.com>" → "Jane Doe"; "jane@example.com" → "jane".
pub fn display_name(from: &str) -> String {
    let from = from.trim();
    if let Some(idx) = from.find('<') {
        let name = from[..idx].trim().trim_matches('"');
        if !name.is_empty() {
            return name.to_string();
        }
    }
    from.split('@')
        .next()
        .unwrap_or(from)
        .trim_matches('"')
        .to_string()
}

/// "Jane Doe <jane@example.com>" → "jane@example.com"; "jane@example.com" → unchanged.
pub fn email_address(from: &str) -> String {
    let from = from.trim();
    match (from.find('<'), from.rfind('>')) {
        (Some(start), Some(end)) if start < end => from[start + 1..end].trim().to_string(),
        _ => from.to_string(),
    }
}

pub fn initials(name: &str) -> String {
    let mut parts = name.split_whitespace().filter_map(|p| p.chars().next());
    let first = parts.next().unwrap_or('?');
    match parts.last() {
        Some(last) => format!("{first}{last}").to_uppercase(),
        None => first.to_uppercase().to_string(),
    }
}

// MARK: - Seed data (used when durian serve is not running)

pub fn seed() -> Vec<(ThreadPreview, Thread)> {
    let rows: &[(&str, &str, &str, &str, &[&str], bool, usize)] = &[
        (
            "Welcome to Durian",
            "Julian <julian@js-lab.org>",
            "This is a GPUI spike: sidebar, virtualized thread list and a detail view — all rendered on the GPU.",
            "09:12",
            &["inbox"],
            true,
            1,
        ),
        (
            "Weekly report — KW 38",
            "team@company.com",
            "Highlights from the week, action items, and open questions for Monday.",
            "Yesterday",
            &["inbox", "work"],
            true,
            4,
        ),
        (
            "Design review: navigation & list",
            "Mara Fischer <mara@company.com>",
            "The updated sketches are ready. A quieter sidebar, denser mail list, and more room for the conversation.",
            "Yesterday",
            &["inbox", "design", "flagged"],
            false,
            3,
        ),
        (
            "Re: Bazel 9 migration",
            "Tobias Renz <tobias@company.com>",
            "rules_swift is fine now, but the Qt genrules still need the moc wrapper.",
            "Wed",
            &["inbox", "work"],
            false,
            12,
        ),
        (
            "Your invoice #48211 is ready",
            "billing@hetzner.com",
            "The invoice for September is available in your account.",
            "Wed",
            &["inbox", "finance"],
            false,
            1,
        ),
        (
            "Konzertkarten: Bestätigung",
            "tickets@eventim.de",
            "Vielen Dank für Ihre Bestellung. Ihre Tickets finden Sie im Anhang.",
            "Tue",
            &["inbox", "attachment"],
            false,
            1,
        ),
        (
            "PR #382: ship licenses and binary SBOMs",
            "GitHub <notifications@github.com>",
            "julion2 merged pull request #382 into main.",
            "Tue",
            &["inbox", "github"],
            false,
            3,
        ),
        (
            "Lunch on Thursday?",
            "Nina Albrecht <nina@example.org>",
            "There's a new ramen place near the office. 12:30?",
            "Mon",
            &["inbox"],
            false,
            2,
        ),
        (
            "Security advisory: GHSA-2026-1177",
            "security@rustsec.org",
            "A memory safety issue was reported in an older version of a transitive dependency.",
            "Mon",
            &["inbox", "security"],
            false,
            1,
        ),
        (
            "Pkl 0.30 released",
            "pkl-announce@apple.com",
            "New features: improved module resolution, faster evaluation, and better error messages.",
            "Sep 12",
            &["inbox", "dev"],
            false,
            1,
        ),
        (
            "Re: Re: JMAP sync edge cases",
            "Sofia Lind <sofia@fastmail.com>",
            "Confirmed — the server drops the Bcc header on delivery, so we should only restore it for local drafts.",
            "Sep 11",
            &["inbox", "work"],
            false,
            9,
        ),
        (
            "Zed 0.230 changelog",
            "hi@zed.dev",
            "GPUI now ships as gpui + gpui_platform; the Linux backend is split into gpui_linux and gpui_wgpu.",
            "Sep 10",
            &["inbox", "dev"],
            false,
            1,
        ),
    ];

    rows.iter()
        .enumerate()
        .map(|(ix, (subject, from, preview, date, tags, unread, count))| {
            let sender = display_name(from);
            let messages = (0..*count)
                .rev()
                .map(|m| Message {
                    from: if m % 2 == 0 { from.to_string() } else { "Julian <julian@js-lab.org>".into() },
                    to: if m % 2 == 0 { "julian@js-lab.org".into() } else { from.to_string() },
                    date: if ix == 2 { format!("Yesterday, {}:24", 9 + m) } else { format!("{date} · msg {}", m + 1) },
                    body: if ix == 2 {
                        [
                            "Can we review the mail layout tomorrow?\n\nI'd like to look at the sidebar, list density, and how we show longer conversations. A few sketches are ready to discuss.\n\nMara",
                            "Let's keep the mail itself at the centre.\n\nThe list should be easy to scan without turning every email into a large card. And I'd like to see who's talking in the conversation, not just a wall of text.\n\nJulian",
                            "The updated sketches are ready.\n\nA quieter sidebar, denser mail list, and more room for the conversation. Each message has its own sender and date; earlier replies can stay folded until you need them.\n\nI've kept the keyboard flow intact. Let's walk through it tomorrow at 10:30.\n\nThanks,\nMara",
                        ][m].to_string()
                    } else if m + 1 == *count {
                        format!("{preview}\n\nThis is seed data rendered by the GPUI experiment. Start `durian serve --no-auth` to see real mail.\n\n— {sender}")
                    } else {
                        format!("Earlier message {} in this thread.\n\n> quoted context from before", m + 1)
                    },
                    ..Default::default()
                })
                .collect();
            (
                ThreadPreview {
                    thread_id: format!("seed-{ix}"),
                    subject: subject.to_string(),
                    sender,
                    preview: preview.to_string(),
                    date: date.to_string(),
                    tags: tags.iter().map(|t| t.to_string()).collect(),
                    unread: *unread,
                    message_count: *count,
                },
                Thread { subject: subject.to_string(), messages },
            )
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn enriched_search_preserves_html_attachments_and_message_order() {
        let response = serde_json::from_str(r#"{
            "ok": true,
            "results": [{"thread_id":"a", "from":"Mara <mara@example.org>", "subject":"Review", "tags":"inbox, flagged"}],
            "threads": {"a": {"subject":"Review", "messages":[
                {"id":"local:42/part", "from":"Mara <mara@example.org>", "date":"Fri, 18 Sep 2026 11:24:00 +0200", "body":"\n> quotation\nNewest answer", "html":"<p>Newest answer</p>", "attachments":[
                    {"part_id":3, "filename":"review.pdf", "content_type":"application/pdf", "size":4096, "disposition":"attachment", "content_id":"document-3"},
                    {"part_id":4, "filename":"defaults.txt"}
                ], "tags":["inbox"]},
                {"from":"Julian <julian@example.org>", "date":"Thu, 17 Sep 2026 09:00:00 +0200", "body":"Older question", "tags":["unread"]}
            ]}}
        }"#).unwrap();
        let rows = decode_response(response).unwrap();
        let (preview, thread) = &rows[0];
        assert_eq!(preview.preview, "Newest answer");
        assert_eq!(preview.message_count, 2);
        assert!(preview.unread);
        assert_eq!(preview.tags, ["inbox", "flagged"]);
        assert_eq!(thread.messages[0].from, "Mara <mara@example.org>");
        assert_eq!(thread.messages[0].id, "local:42/part");
        assert_eq!(thread.messages[0].html, "<p>Newest answer</p>");
        assert_eq!(thread.messages[0].attachments.len(), 2);
        assert_eq!(thread.messages[0].attachments[0].part_id, 3);
        assert_eq!(thread.messages[0].attachments[0].size, 4096);
        assert_eq!(thread.messages[0].attachments[0].content_id, "document-3");
        assert_eq!(thread.messages[0].attachments[1].content_type, "");
        assert_eq!(thread.messages[0].attachments[1].size, 0);
        assert_eq!(thread.messages[1].body, "Older question");
        assert!(thread.messages[1].id.is_empty());
        assert!(thread.messages[1].attachments.is_empty());
    }

    #[test]
    fn opaque_message_id_is_encoded_as_one_path_segment() {
        assert_eq!(
            percent_encode_path_segment("local/id%雪"),
            "local%2Fid%25%E9%9B%AA"
        );
        assert_eq!(
            percent_encode_path_segment("AZaz09-._~:@"),
            "AZaz09-._~%3A%40"
        );
    }

    #[test]
    fn response_envelopes_return_api_errors() {
        let search_response =
            serde_json::from_str(r#"{"ok":false,"error":"search unavailable"}"#).unwrap();
        assert_eq!(
            decode_response(search_response).unwrap_err(),
            "search unavailable"
        );

        let tags_response =
            serde_json::from_str(r#"{"ok":false,"error":"tags unavailable"}"#).unwrap();
        assert_eq!(
            decode_tags_response(tags_response).unwrap_err(),
            "tags unavailable"
        );

        let missing_error = serde_json::from_str(r#"{"ok":false}"#).unwrap();
        assert_eq!(
            decode_tags_response(missing_error).unwrap_err(),
            "API returned error"
        );
    }

    #[test]
    fn demo_conversation_matches_the_api_order() {
        let rows = seed();
        let (_, thread) = rows.iter().find(|(p, _)| p.thread_id == "seed-2").unwrap();
        assert_eq!(thread.messages.len(), 3);
        assert_eq!(thread.messages[0].date, "Yesterday, 11:24");
        assert!(thread.messages[0].body.starts_with("The updated sketches"));
        assert!(thread.messages[1].from.starts_with("Julian"));
        assert!(thread.messages[2].body.starts_with("Can we review"));
    }
}
