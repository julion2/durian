//! Demo data for the mail UI: `data/mail.json`, with the newsletters' HTML read from the app's
//! assets and the demo attachments written into the cache, so Quick Look and Finder get real
//! files. A real backend hands `MailData` over the same way.

use std::path::{Path, PathBuf};

use rust_embed::RustEmbed;

use mail::model::{MailData, ThreadItem};

#[derive(RustEmbed)]
#[folder = "data/attachments"]
struct DemoFiles;

pub fn load() -> anyhow::Result<MailData> {
    let mut data: MailData = serde_json::from_slice(include_bytes!("../data/mail.json"))?;
    let attachments = mail::cache_dir("attachments");
    for thread in &mut data.threads {
        // Older fixture data has state booleans; expose the same tags as live mail.
        for (tag, present) in [("unread", thread.unread()), ("flagged", thread.starred)] {
            if present && !thread.tags.iter().any(|t| t == tag) { thread.tags.push(tag.into()); }
        }
        for item in &mut thread.items {
            let ThreadItem::Message(m) = item else { continue };
            if let Some(source) = &m.html_source {
                let file = crate::assets::Assets::get(source).ok_or_else(|| anyhow::anyhow!("missing {source}"))?;
                m.html = Some(String::from_utf8_lossy(&file.data).into_owned().into());
            }
            for a in &mut m.attachments {
                a.path = materialize(&attachments, &a.name);
            }
        }
    }
    Ok(data)
}

/// email → picture path of everyone the mail data knows, so the calendar shows the same faces.
pub fn avatars(data: &MailData) -> std::collections::HashMap<String, gpui::SharedString> {
    let mut out = std::collections::HashMap::new();
    let mut add = |c: &mail::model::Contact| {
        if let Some(a) = &c.avatar {
            out.insert(c.email.to_string(), a.path.clone());
        }
    };
    add(&data.me);
    for thread in &data.threads {
        for item in &thread.items {
            if let ThreadItem::Message(m) = item {
                add(&m.from);
            }
        }
    }
    out
}

/// The demo file on disk, written from the embedded copy on first use.
fn materialize(dir: &Path, name: &str) -> Option<PathBuf> {
    let path = dir.join(name);
    if !path.exists() {
        let file = DemoFiles::get(name)?;
        std::fs::create_dir_all(dir).ok()?;
        std::fs::write(&path, file.data).ok()?;
    }
    Some(path)
}

/// The demo data drives these: query semantics and sender options against known threads.
#[cfg(test)]
mod tests {
    use mail::model::Mailbox;
    use mail::query::{Query, apply_completion, completions};

    fn mailbox() -> Mailbox {
        Mailbox::new(super::load().unwrap())
    }

    fn ids(query: &str) -> Vec<String> {
        let mailbox = mailbox();
        let q = Query::parse(query);
        mailbox.threads.iter().filter(|t| q.matches(t, &mailbox)).map(|t| t.id.to_string()).collect()
    }

    #[test]
    fn demo_participants_count_name_only_recipients_once() {
        let mailbox = mailbox();
        let thread = mailbox.threads.iter().find(|t| t.id == "rfc-tags").unwrap();
        // Mara, Tobias and Julian alternate sender/recipient roles over five messages.
        assert_eq!(thread.people_count(), 3);
    }

    #[test]
    fn matches_fixture_threads() {
        assert_eq!(ids("tag:incident"), ["incident", "invite-postmortem"]);
        assert!(ids("tag:oss -tag:alerts").contains(&"gpui-pr".to_string()));
        assert!(!ids("tag:oss -tag:alerts").contains(&"ci".to_string()));
        assert!(ids("from:mara").contains(&"rfc-tags".to_string()));
        assert!(ids("has:attachment account:work").iter().all(|id| ["incident", "rfc-tags", "stripe", "invoice"].contains(&id.as_str())));
        assert!(ids("\"rollback\"").contains(&"deploy".to_string()));
        assert!(ids("newer:3h").iter().all(|id| id != "lunch"));
        assert!(ids("tag:o").contains(&"gpui-pr".to_string()), "typing a tag narrows by prefix");
        assert!(ids("tag:o ").is_empty(), "a finished token is exact");
        assert_eq!(ids("tag:flagged "), ["rfc-tags"]);
        assert!(ids("tag:unread ").contains(&"rfc-tags".to_string()));
        assert!(!ids("-tag:unread ").contains(&"rfc-tags".to_string()));
    }

    #[test]
    fn completes_the_last_token() {
        let mailbox = mailbox();
        let tags: Vec<_> = completions("from:me tag:in", &mailbox).into_iter().map(|(c, _)| c.to_string()).collect();
        assert!(tags.contains(&"tag:infra".to_string()) && tags.contains(&"tag:incident".to_string()));
        assert_eq!(apply_completion("from:me tag:in", "tag:infra"), "from:me tag:infra ");
        assert!(completions("ta", &mailbox).iter().any(|(c, _)| c == "tag:"));
        assert!(completions("tag:un", &mailbox).iter().any(|(c, _)| c == "tag:unread"));
        assert!(completions("is:", &mailbox).is_empty());
    }

    #[test]
    fn sender_options_follow_the_profile() {
        let mut mailbox = mailbox();
        let all: Vec<_> = mailbox.sender_options().into_iter().map(|(_, e)| e).collect();
        assert_eq!(all.len(), 5, "3 primaries + 2 aliases in All accounts: {all:?}");

        mailbox.account_filter = Some("work".into());
        let work: Vec<_> = mailbox.sender_options().into_iter().map(|(_, e)| e.to_string()).collect();
        assert_eq!(work, ["julian@habric.com", "team@habric.com"]);

        mailbox.account_filter = Some("oss".into());
        assert_eq!(mailbox.sender_options().len(), 1, "single address: the chip isn't clickable");
    }
}
