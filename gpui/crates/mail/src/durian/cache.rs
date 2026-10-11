//! Full threads already downloaded, so moving back through the list or re-running a search
//! doesn't fetch them again. Bounded (least recently used goes first) and keyed by what the
//! search row says about the thread, so a thread that changed on the server isn't served stale.

use std::collections::HashMap;

use gpui::SharedString;

use crate::model::Thread;

/// What a search row tells about a thread's state: its newest message and its tags.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Stamp {
    pub latest: i64,
    /// sorted, so "inbox,unread" and "unread,inbox" are the same state
    pub tags: Vec<String>,
}

impl Stamp {
    pub fn new(latest: i64, tags: &str) -> Self {
        let mut tags: Vec<String> = tags.split(',').map(str::trim).filter(|t| !t.is_empty()).map(String::from).collect();
        tags.sort();
        tags.dedup();
        Self { latest, tags }
    }
}

/// How a cached thread relates to a fresh row for it.
#[derive(Debug, PartialEq, Eq)]
pub enum Fit {
    /// nothing changed
    Same,
    /// same messages, other tags: patch the tags in
    Retag,
    /// new messages (or a change only a refetch explains)
    Stale,
}

/// Compare what we cached with what the server says now.
pub fn fit(cached: &Stamp, row: &Stamp) -> Fit {
    // any other newest message – a new one, or the newest deleted – means other messages
    if cached.latest != row.latest {
        return Fit::Stale;
    }
    if cached.tags == row.tags {
        return Fit::Same;
    }
    // unread appeared: on which message? Every other change is thread-level (losing unread
    // included: then every message is read)
    let has = |s: &Stamp| s.tags.iter().any(|t| t == "unread");
    if has(row) && !has(cached) { Fit::Stale } else { Fit::Retag }
}

struct Entry {
    stamp: Stamp,
    thread: Thread,
    used: u64,
}

pub struct ThreadCache {
    entries: HashMap<SharedString, Entry>,
    capacity: usize,
    clock: u64,
}

impl ThreadCache {
    pub fn new(capacity: usize) -> Self {
        Self { entries: HashMap::new(), capacity, clock: 0 }
    }

    pub fn insert(&mut self, stamp: Stamp, thread: Thread) {
        self.clock += 1;
        self.entries.insert(thread.id.clone(), Entry { stamp, thread, used: self.clock });
        if self.entries.len() > self.capacity {
            // O(n) over a few hundred entries, once per insert past the bound: cheaper than a list
            if let Some(oldest) = self.entries.iter().min_by_key(|(_, e)| e.used).map(|(id, _)| id.clone()) {
                self.entries.remove(&oldest);
            }
        }
    }

    /// The full thread for a row, if what we have still describes it (tags patched in when
    /// only those changed). A stale entry is dropped.
    pub fn get(&mut self, id: &str, row: &Stamp) -> Option<Thread> {
        let fit = fit(&self.entries.get(id)?.stamp, row);
        if fit == Fit::Stale {
            self.entries.remove(id);
            return None;
        }
        self.clock += 1;
        let entry = self.entries.get_mut(id)?;
        entry.used = self.clock;
        if fit == Fit::Retag {
            super::retag(&mut entry.thread, &row.tags);
            entry.stamp = row.clone();
        }
        Some(entry.thread.clone())
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn stamp(latest: i64, tags: &str) -> Stamp {
        Stamp::new(latest, tags)
    }

    #[test]
    fn stamps_ignore_tag_order() {
        assert_eq!(stamp(5, "unread, inbox"), stamp(5, "inbox,unread,"));
    }

    #[test]
    fn a_new_message_makes_the_thread_stale() {
        assert_eq!(fit(&stamp(5, "inbox"), &stamp(5, "inbox")), Fit::Same);
        assert_eq!(fit(&stamp(5, "inbox"), &stamp(9, "inbox,unread")), Fit::Stale);
    }

    #[test]
    fn tag_changes_patch_unless_they_need_the_messages() {
        // archived or flagged elsewhere: the messages are what we have
        assert_eq!(fit(&stamp(5, "inbox,unread"), &stamp(5, "unread")), Fit::Retag);
        assert_eq!(fit(&stamp(5, "inbox"), &stamp(5, "inbox,flagged")), Fit::Retag);
        // read elsewhere: every message is read now, that's patchable too
        assert_eq!(fit(&stamp(5, "inbox,unread"), &stamp(5, "inbox")), Fit::Retag);
        // marked unread elsewhere: which message? only the server knows
        assert_eq!(fit(&stamp(5, "inbox"), &stamp(5, "inbox,unread")), Fit::Stale);
    }
}
