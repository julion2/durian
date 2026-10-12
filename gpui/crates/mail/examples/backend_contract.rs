//! Exercises the production client against the real, seeded Go server.
//! Run through //integration:gpui_test; no mailbox credentials or user data are used.

use mail::durian::{self, Client, Error, cache::{Stamp, ThreadCache}, profiles};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let server = std::env::var("DURIAN_TEST_URL")?;
    let token = std::env::var("DURIAN_TEST_TOKEN")?;
    let client = Client::new(&server, Some(token.clone()));
    assert!(matches!(Client::new(&server, Some("invalid".into())).version(), Err(Error::Unauthorized)));
    assert!(!client.version()?.version.is_empty());
    println!("PASS authenticated real Go server, wrong token rejected");

    assert!(matches!(Client::new(&server, Some("invalid".into())).contacts("alice"), Err(Error::Unauthorized)));
    let contacts = client.contacts("alice@")?;
    assert_eq!(contacts.len(), 1);
    assert_eq!(contacts[0].email, "alice@example.com");
    assert_eq!(contacts[0].name, "Alice Smith");
    assert_eq!(client.contacts("Bob J")?[0].email, "bob@example.com");
    assert!(client.contacts("not-in-this-address-book")?.is_empty());
    println!("PASS authenticated contact email/name prefix lookup and empty results");

    let response = client.profiles()?;
    let profiles = profiles::from_api(&response);
    let all = profiles.iter().find(|p| p.name == "All").expect("all-accounts profile");
    assert!(profiles::scope(all).is_none());
    let profile = profiles.iter().find(|p| p.default).expect("default profile");
    let scope = profiles::scope(profile).expect("scoped profile");
    let scoped = client.search(&format!("({scope}) (tag:inbox)"), 200, 50)?;
    assert_eq!(scoped.results.len(), 2);
    assert!(!client.settings()?.load_remote_images);
    println!("PASS server profiles, account-scoped query and remote-image defaults");

    let found = client.search("tag:inbox", 200, 50)?;
    assert_eq!(found.results.len(), 2);
    assert_eq!(client.count("tag:inbox")?, 2);
    assert_eq!(client.count("tag:unread")?, 1);
    assert!(client.search("subject:absent-contract-message", 25, 0)?.results.is_empty());
    let subject_query = |text| durian::translate::translate(&mail::query::Query::parse(text), &[]).server;
    let subjects = client.search(&subject_query("subject:Other "), 25, 0)?;
    assert_eq!(subjects.results.len(), 1);
    assert_eq!(subjects.results[0].subject, "Other Thread");
    assert!(client.search(&subject_query("subject:Different "), 25, 0)?.results.is_empty(), "body text must not satisfy subject:");
    let negated = client.search(&subject_query("-subject:Other "), 25, 0)?;
    assert_eq!(negated.results.len(), 1);
    assert_eq!(negated.results[0].subject, "Re: Hello World");
    assert!(found.threads.values().flat_map(|t| &t.messages).all(|m| m.html.is_none()), "enrichment is only a preview");
    let row = found.results.iter().find(|r| r.subject.contains("Hello World")).expect("seeded conversation");
    let content = client.thread(&row.thread_id)?;
    assert_eq!(content.messages.len(), 3);
    assert_eq!(content.messages[0].body, "Reply body", "wire order is newest first");
    assert!(content.messages[0].html.as_deref().unwrap().contains("Reply body"));
    let duplicates: Vec<_> = content.messages.iter().filter(|m| m.message_id.as_deref() == Some("msg1@test")).collect();
    assert_eq!(duplicates.len(), 2);
    assert_ne!(duplicates[0].id, duplicates[1].id);
    assert_ne!(duplicates[0].attachment_cache_id, duplicates[1].attachment_cache_id);
    assert!(duplicates.iter().all(|m| m.id.starts_with("local:")));
    assert!(client.thread("0000000000000000").is_err(), "HTTP 200 ok:false must not become an empty thread");
    assert!(matches!(client.attachment("local:999999999", 1), Err(Error::NotFound(_))));
    println!("PASS search, counts, full HTML and duplicate RFC Message-ID identity");

    let mapped = durian::thread(&content, chrono::Local::now().timestamp(), Default::default()).expect("mapped thread");
    assert!(!mapped.partial);
    assert_eq!(mapped.messages().last().unwrap().body.as_ref(), "Reply body", "UI order is oldest first");
    let mut cache = ThreadCache::new(2);
    cache.insert(Stamp::new(row.timestamp, &row.tags), mapped);
    assert!(cache.get(&row.thread_id, &Stamp::new(row.timestamp, &row.tags)).is_some());
    // An external client marks this thread read. The GPUI cache must update from the next
    // authoritative search, without enabling writes in the production client.
    let mut changed = ureq::post(format!("{server}/api/v1/threads/{}/tags", row.thread_id))
        .header("Authorization", format!("Bearer {token}"))
        .header("Content-Type", "application/json")
        .send(r#"{"tags":"-unread +flagged"}"#)?;
    let answer: serde_json::Value = serde_json::from_str(&changed.body_mut().read_to_string()?)?;
    assert_eq!(answer["ok"], true);
    let refreshed = client.search("tag:inbox", 200, 0)?;
    let updated = refreshed.results.iter().find(|r| r.thread_id == row.thread_id).unwrap();
    let cached = cache.get(&row.thread_id, &Stamp::new(updated.timestamp, &updated.tags)).unwrap();
    assert!(!cached.unread());
    assert!(cached.starred);
    assert!(cache.get(&row.thread_id, &Stamp::new(updated.timestamp + 1, &updated.tags)).is_none());
    assert_eq!(client.count("tag:unread")?, 0);
    println!("PASS external tag change reconciles cache; new message invalidates full content");

    // More than Go's shared 30-request burst: a large sidebar must not make
    // ordinary searches fail. Check actual values, not just HTTP success.
    std::thread::scope(|scope| {
        let mut workers = Vec::new();
        for worker in 0..4 {
            let client = &client;
            workers.push(scope.spawn(move || {
                for index in 0..20 {
                    let (query, expected) = if (index + worker) % 3 == 0 { ("tag:unread", 0) } else { ("tag:inbox", 2) };
                    assert_eq!(client.count(query).unwrap(), expected);
                }
            }));
        }
        for _ in 0..8 {
            let page = client.search("from:charlie", 7, 0).unwrap();
            assert_eq!(page.results.len(), 1);
            assert_eq!(page.results[0].subject, "Other Thread");
        }
        for worker in workers { worker.join().unwrap(); }
    });
    println!("PASS 80 sidebar counts plus 8 concurrent foreground searches against Go's unchanged rate limiter");

    let calendars = client.calendars("test")?;
    assert!(calendars.iter().any(|c| c.name == "Work"));
    let events = client.events("test", "2026-08-01", "2026-08-02")?;
    assert_eq!(events.len(), 1);
    assert_eq!(events[0].uid, "gpui-contract-event");
    let detail = client.event("test", "Work", "gpui-contract-event")?;
    assert_eq!(detail.subject, "GPUI contract meeting");
    assert!(client.events("test", "2026-08-02", "2026-08-03")?.is_empty());
    println!("PASS calendar window and full event detail through production client");

    // Another client removes the exact event while a UID-prefix neighbor remains.
    // None of the machine-client operations may read or mutate that neighbor.
    let (_, at) = durian::calendar_event(&"test".into(), &detail).unwrap();
    client.rsvp_event(&at, calendar::model::Rsvp::Accepted)?;
    assert_eq!(client.event("test", "Work", &at.uid)?.my_response.as_deref(), Some("accepted"));
    let neighbor = durian::calendar_write::EventWrite {
        account: "test".into(), calendar: "Work".into(), uid: format!("{}-neighbor", at.uid),
        subject: "Keep this invitation unchanged".into(), start: "2026-08-01T15:00:00Z".into(), end: "2026-08-01T16:00:00Z".into(),
        all_day: false, location: String::new(), description: String::new(), attendees: vec!["test@example.com".into()], replace_attendees: true,
    };
    client.put_event(&neighbor)?;
    let before = client.event("test", "Work", &neighbor.uid)?;
    // Use the legacy request to model a different client, not the implementation under test.
    ureq::delete(format!("{server}/api/v1/calendars/event"))
        .query("account", "test").query("calendar", "Work").query("ref", &at.uid)
        .header("Authorization", format!("Bearer {token}")).call()?;
    assert!(matches!(client.event("test", "Work", &at.uid), Err(Error::NotFound(_))));
    assert!(matches!(client.rsvp_event(&at, calendar::model::Rsvp::Declined), Err(Error::NotFound(_))));
    assert!(matches!(client.delete_event(&at), Err(Error::NotFound(_))));
    let after = client.event("test", "Work", &neighbor.uid)?;
    assert_eq!(after.uid, neighbor.uid);
    assert_eq!(after.subject, before.subject);
    assert_eq!(after.my_response, before.my_response, "missing UID must not change the neighbor's RSVP");
    let (_, neighbor_at) = durian::calendar_event(&"test".into(), &after).unwrap();
    client.delete_event(&neighbor_at)?;
    println!("PASS exact calendar GET/DELETE/RSVP after another client deletes the target; prefix neighbor unchanged");

    let mut write = durian::calendar_write::EventWrite {
        account: "local".into(), calendar: "Scratch".into(), uid: "stable-create-contract".into(),
        subject: "All-day saved locally".into(), start: "2026-08-03T00:00:00Z".into(), end: "2026-08-05T00:00:00Z".into(),
        all_day: true, location: "Room 2".into(), description: "Keep the full description".into(), attendees: Vec::new(), replace_attendees: true,
    };
    let created = client.put_event(&write)?;
    assert_eq!(created.uid, write.uid);
    assert!(created.all_day);
    write.subject = "Edited without a duplicate".into();
    client.put_event(&write)?;
    let saved = client.events("local", "2026-08-03", "2026-08-06")?;
    assert_eq!(saved.len(), 1, "stable UID retries upsert rather than duplicating");
    assert_eq!(saved[0].subject, write.subject);
    let detail = client.event("local", "Scratch", &write.uid)?;
    assert_eq!(detail.description.as_deref(), Some("Keep the full description"));
    let (event, at) = durian::calendar_event(&"local".into(), &detail).unwrap();
    assert_eq!(event.start.to_string(), "2026-08-03 00:00:00");
    assert_eq!(event.end.to_string(), "2026-08-05 00:00:00");
    assert!(!event.on(chrono::NaiveDate::from_ymd_opt(2026, 8, 5).unwrap()));
    client.sync_event(&at, "save")?; // local calendars do not contact a provider
    write.attendees.push("guest@example.com".into());
    assert!(client.put_event(&write).is_err(), "local calendars must not promise invitations");
    client.delete_event(&at)?;
    assert!(client.events("local", "2026-08-03", "2026-08-06")?.is_empty());
    assert!(client.delete_event(&at).is_err(), "a missing event is not an acknowledged delete");
    println!("PASS local calendar create/update/stable UID/delete, exclusive civil dates and rejected invitations");
    Ok(())
}
