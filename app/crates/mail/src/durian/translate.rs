//! The UI's query language → durian's (`store/search.go`): the server narrows, the client
//! refines. Whatever durian can express goes to the server; what it can't – or only more
//! coarsely – stays as a local refinement over the results. The server part is always a
//! superset of the exact answer, so refining loses nothing.
//!
//! Differences that matter:
//! - negation is `NOT x`, not `-x`
//! - dates are whole days and `m` means *months* (`date:3m..`), so `newer:30m` (minutes) or
//!   `newer:5h` become "since N days" on the server and are cut exactly here
//! - unread / starred are tags (`tag:unread`, `tag:flagged`); there is no `is:`
//! - accounts are folder paths there (`path:work/`)
//! - "has:html" isn't a field; a token still being typed (`tag:o`) can't go out either – the
//!   server matches whole words (its text index is blind: HMAC tokens), so prefixes match here

use gpui::SharedString;

use crate::query::{Clause, Query, Term};

#[derive(Debug, Default, PartialEq)]
pub struct Translated {
    /// for `/search?query=` (empty: no constraint beyond the view's base query)
    pub server: String,
    /// clauses the client applies to the results
    pub refine: Query,
}

/// Quote a value if it has anything but word characters (durian splits on spaces).
pub fn quoted(v: &str) -> String {
    value(v)
}

fn value(v: &str) -> String {
    if v.chars().all(|c| c.is_alphanumeric() || "@._-+".contains(c)) { v.to_string() } else { format!("\"{}\"", v.replace('"', "")) }
}

fn any_of(field: &str, values: &[String]) -> String {
    match values {
        [one] => format!("{field}:{}", value(one)),
        many => format!("({})", many.iter().map(|v| format!("{field}:{}", value(v))).collect::<Vec<_>>().join(" OR ")),
    }
}

/// `me`: the user's addresses (`from:me` / `to:me`).
pub fn translate(query: &Query, me: &[SharedString]) -> Translated {
    let me: Vec<String> = me.iter().filter(|a| !a.is_empty()).map(|a| a.to_string()).collect();
    let mut server: Vec<String> = Vec::new();
    let mut refine: Vec<Clause> = Vec::new();
    let who = |w: &str| if w == "me" && !me.is_empty() { me.clone() } else { vec![w.to_string()] };

    for clause in &query.clauses {
        // still being typed: prefix semantics exist only here
        if clause.prefix {
            refine.push(clause.clone());
            continue;
        }
        let part: Option<String> = match &clause.term {
            Term::Tag(t) => Some(format!("tag:{}", value(t))),
            Term::From(w) => Some(any_of("from", &who(w))),
            Term::Subject(s) => Some(format!("subject:{}", value(s))),
            // the UI's to: covers cc as well
            Term::To(w) => {
                let w = who(w);
                Some(format!("({} OR {})", any_of("to", &w), any_of("cc", &w)))
            }
            Term::Is(state) => match state.as_str() {
                "unread" => Some("tag:unread".into()),
                "read" => Some("NOT tag:unread".into()),
                "starred" => Some("tag:flagged".into()),
                _ => None,
            },
            Term::Has(what) if matches!(what.as_str(), "attachment" | "attachments" | "file") => Some("has:attachment".into()),
            Term::Account(a) => Some(format!("path:{}/", value(a))),
            Term::Has(_) => None,
            // whole days, rounded so the server's window contains the exact one
            Term::Newer(hours) => {
                refine.push(clause.clone());
                let days = (hours / 24.).ceil().max(1.) as u32;
                (!clause.negate).then(|| format!("date:{days}d.."))
            }
            Term::Older(hours) => {
                refine.push(clause.clone());
                let days = (hours / 24.).floor() as u32;
                (!clause.negate && days >= 1).then(|| format!("date:..{days}d"))
            }
            Term::Text(t) => Some(value(t)),
        };
        match part {
            // negated time windows are refined only (NOT of a superset would drop matches)
            Some(p) if clause.negate => server.push(format!("NOT {p}")),
            Some(p) => server.push(p),
            None => {
                if !matches!(clause.term, Term::Newer(_) | Term::Older(_)) {
                    refine.push(clause.clone());
                }
            }
        }
    }
    Translated { server: server.join(" "), refine: Query { clauses: refine } }
}

/// Only one account's mail (the account switcher).
pub fn account_scope(account: &str) -> String {
    format!("path:{}/", value(account))
}

/// The view's base query and the user's search, combined for `/search`.
pub fn combine(base: &str, search: &str) -> String {
    match (base.trim(), search.trim()) {
        ("", s) => s.to_string(),
        (b, "") => b.to_string(),
        (b, s) => format!("({b}) ({s})"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn t(q: &str) -> Translated {
        translate(&Query::parse(&format!("{q} ")), &["me@work.dev".into(), "me@home.dev".into()])
    }

    #[test]
    fn tags_states_and_negation() {
        assert_eq!(t("tag:review -tag:done").server, "tag:review NOT tag:done");
        assert_eq!(t("is:unread is:starred").server, "tag:unread tag:flagged");
        assert_eq!(t("is:read").server, "NOT tag:unread");
        assert_eq!(t("has:attachment").server, "has:attachment");
        assert_eq!(t("subject:Other").server, "subject:other");
        assert_eq!(t("-subject:\"Other Thread\"").server, "NOT subject:\"other thread\"");
        assert!(t("subject:Other").refine.is_empty());
    }

    #[test]
    fn me_expands_to_every_address() {
        assert_eq!(t("from:me").server, "(from:me@work.dev OR from:me@home.dev)");
        assert_eq!(t("to:mara").server, "(to:mara OR cc:mara)");
    }

    #[test]
    fn time_is_whole_days_on_the_server_exact_here() {
        // 5 hours: "since 1 day" there, cut to 5h here
        let x = t("newer:5h");
        assert_eq!(x.server, "date:1d..");
        assert_eq!(x.refine.clauses.len(), 1);
        // 30 minutes is not 30 months
        assert_eq!(t("newer:30m").server, "date:1d..");
        assert_eq!(t("older:3d").server, "date:..3d");
        assert_eq!(t("older:5h").server, "", "less than a day: no server bound, refined here");
        assert_eq!(t("-newer:2d").server, "", "a negated window can't be a server superset");
    }

    #[test]
    fn what_durian_lacks_is_refined_locally() {
        let x = t("account:work has:html phrase");
        assert_eq!(x.server, "path:work/ phrase");
        assert_eq!(x.refine.clauses.len(), 1);
        assert_eq!(t("\"exact phrase\"").server, "\"exact phrase\"");
        // the token being typed (no trailing space) stays local
        let typing = translate(&Query::parse("tag:inbox tag:o"), &[]);
        assert_eq!(typing.server, "tag:inbox");
        assert_eq!(typing.refine.clauses.len(), 1);
    }

    #[test]
    fn base_and_search_combine() {
        assert_eq!(combine("tag:inbox", ""), "tag:inbox");
        assert_eq!(combine("tag:inbox", "from:mara"), "(tag:inbox) (from:mara)");
        assert_eq!(combine("", "x"), "x");
    }
}
