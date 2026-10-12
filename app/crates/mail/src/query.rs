//! The query language shared with the CLI: what `mailcli list '<query>'` accepts, the search bar
//! accepts. A view is just a saved query, a tag click is `tag:name`.
//!
//! ```text
//! tag:review -tag:done from:mara to:me account:work is:unread is:starred has:attachment
//! newer:24h older:7d "exact phrase" free text
//! ```
//! Terms are AND-ed; `-` negates any term.

use gpui::SharedString;

use crate::model::{Mailbox, Thread, ThreadItem, first_name};

#[derive(Clone, Debug, PartialEq)]
pub enum Term {
    Tag(String),
    From(String),
    To(String),
    Subject(String),
    Account(String),
    Is(String),
    Has(String),
    /// age bounds in hours, against the latest message
    Newer(f32),
    Older(f32),
    Text(String),
}

#[derive(Clone, Debug, PartialEq)]
pub struct Clause {
    pub negate: bool,
    pub term: Term,
    /// The token still being typed (no trailing space): values match as prefixes, so the list
    /// narrows smoothly instead of flashing empty on `tag:o`.
    pub prefix: bool,
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct Query {
    pub clauses: Vec<Clause>,
}

/// "24h" / "7d" / "2w" / "30m" → hours.
fn duration_hours(s: &str) -> Option<f32> {
    let (num, unit) = s.split_at(s.find(|c: char| !c.is_ascii_digit() && c != '.')?);
    let n: f32 = num.parse().ok()?;
    Some(match unit {
        "m" | "min" => n / 60.,
        "h" => n,
        "d" => n * 24.,
        "w" => n * 24. * 7.,
        _ => return None,
    })
}

/// The fixture's "ago" strings ("now", "8m", "2h", "3d") → hours.
pub fn ago_hours(ago: &str) -> f32 {
    if ago == "now" { 0. } else { duration_hours(ago).unwrap_or(f32::MAX) }
}

/// Splits on whitespace, keeping "quoted phrases" (and key:"quoted values") together.
fn tokens(input: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut current = String::new();
    let mut quoted = false;
    for ch in input.chars() {
        match ch {
            '"' => quoted = !quoted,
            c if c.is_whitespace() && !quoted => {
                if !current.is_empty() {
                    out.push(std::mem::take(&mut current));
                }
            }
            c => current.push(c),
        }
    }
    if !current.is_empty() {
        out.push(current);
    }
    out
}

impl Query {
    pub fn parse(input: &str) -> Self {
        let mut query = Self::parse_terms(input);
        if !input.ends_with(char::is_whitespace)
            && let Some(last) = query.clauses.last_mut()
        {
            last.prefix = true;
        }
        query
    }

    fn parse_terms(input: &str) -> Self {
        let clauses = tokens(input)
            .into_iter()
            .filter_map(|token| {
                let (negate, token) = match token.strip_prefix('-') {
                    Some(rest) if !rest.is_empty() => (true, rest.to_string()),
                    _ => (false, token),
                };
                let term = match token.split_once(':') {
                    Some((key, value)) if !value.is_empty() => {
                        let value = value.to_lowercase();
                        match key.to_lowercase().as_str() {
                            "tag" | "label" => Term::Tag(value),
                            "from" => Term::From(value),
                            "to" => Term::To(value),
                            "subject" => Term::Subject(value),
                            "account" | "in" => Term::Account(value),
                            "is" => Term::Is(value),
                            "has" => Term::Has(value),
                            "newer" | "after" => Term::Newer(duration_hours(&value)?),
                            "older" | "before" => Term::Older(duration_hours(&value)?),
                            _ => Term::Text(token.to_lowercase()),
                        }
                    }
                    // an unfinished `tag:` while typing matches everything
                    Some((_, "")) => return None,
                    _ => Term::Text(token.to_lowercase()),
                };
                Some(Clause { negate, term, prefix: false })
            })
            .collect();
        Self { clauses }
    }

    pub fn is_empty(&self) -> bool {
        self.clauses.is_empty()
    }

    pub fn matches(&self, thread: &Thread, mailbox: &Mailbox) -> bool {
        self.matcher(mailbox).matches(thread)
    }

    /// For a pass over many threads: what matching needs beyond each thread is prepared once.
    pub fn matcher<'a>(&'a self, mailbox: &'a Mailbox) -> Matcher<'a> {
        // recipients are stored by name; `to:` also matches their addresses
        let emails = self.clauses.iter().any(|c| matches!(c.term, Term::To(_))).then(|| mailbox.contacts().into_iter().map(|c| (c.name, c.email)).collect());
        Matcher { query: self, mailbox, emails }
    }
}

pub struct Matcher<'a> {
    query: &'a Query,
    mailbox: &'a Mailbox,
    emails: Option<std::collections::HashMap<SharedString, SharedString>>,
}

impl Matcher<'_> {
    pub fn matches(&self, thread: &Thread) -> bool {
        self.query.clauses.iter().all(|c| unknown(&c.term, thread) || c.negate != term_matches(&c.term, c.prefix, thread, self.mailbox, self.emails.as_ref()))
    }
}

/// Case-insensitive `contains` for a lowercase needle, without lowercasing the haystack: it
/// runs over every body on every keystroke. An ASCII needle (the usual case) compares bytes.
fn contains_folded(haystack: &str, needle: &str) -> bool {
    if needle.is_ascii() {
        let n = needle.as_bytes();
        n.is_empty() || haystack.as_bytes().windows(n.len()).any(|w| w.eq_ignore_ascii_case(n))
    } else {
        haystack.to_lowercase().contains(needle)
    }
}

fn name_matches(name: &str, email: &str, needle: &str, mailbox: &Mailbox) -> bool {
    if needle == "me" {
        return name == mailbox.me.name.as_ref() || mailbox.accounts.iter().any(|a| a.email == email);
    }
    name.to_lowercase().contains(needle) || email.to_lowercase().contains(needle)
}

/// What a list row without its messages (`Thread::partial`) can't tell: its attachments and
/// recipients. Such a clause keeps the row – the server's answer already accounts for it.
fn unknown(term: &Term, thread: &Thread) -> bool {
    thread.partial && matches!(term, Term::Has(_) | Term::To(_))
}

fn term_matches(term: &Term, prefix: bool, thread: &Thread, mailbox: &Mailbox, emails: Option<&std::collections::HashMap<SharedString, SharedString>>) -> bool {
    let messages = || thread.messages();
    let eq = |value: &str, wanted: &str| if prefix { value.starts_with(wanted) } else { value == wanted };
    match term {
        Term::Tag(tag) => thread.tags.iter().any(|t| eq(&t.to_lowercase(), tag)),
        Term::From(who) => messages().any(|m| name_matches(&m.from.name, &m.from.email, who, mailbox)),
        Term::To(who) => messages().any(|m| {
            m.to.iter().chain(&m.cc).any(|n| {
                let email = emails.and_then(|e| e.get(n)).map_or("", |e| e.as_ref());
                name_matches(n, email, who, mailbox) || (who == "me" && *n == mailbox.me.name)
            })
        }),
        Term::Subject(needle) => contains_folded(&thread.subject, needle),
        Term::Account(id) => eq(&thread.account.to_lowercase(), id)
            || mailbox.account(&thread.account).is_some_and(|a| eq(&a.name.to_lowercase(), id)),
        // while typing, `is:st` means "any state starting with st"; no such state matches nothing
        Term::Is(state) => ["unread", "read", "starred"].iter().filter(|s| eq(s, state)).any(|s| match *s {
            "unread" => thread.unread(),
            "read" => !thread.unread(),
            _ => thread.starred,
        }),
        Term::Has(what) => match what.as_str() {
            "attachment" | "attachments" | "file" => messages().any(|m| !m.attachments.is_empty()),
            "html" => messages().any(|m| m.html.is_some()),
            _ => false,
        },
        Term::Newer(hours) => ago_hours(&thread.latest().ago) <= *hours,
        Term::Older(hours) => ago_hours(&thread.latest().ago) > *hours,
        Term::Text(needle) => {
            contains_folded(&thread.subject, needle)
                || thread.items.iter().any(|item| match item {
                    ThreadItem::Message(m) => contains_folded(&m.body, needle) || contains_folded(first_name(&m.from.name), needle),
                    ThreadItem::Event(_) => false,
                })
        }
    }
}

/// Completion for the token under the cursor (the last one): `tag:re` → tags starting with "re".
pub fn completions(input: &str, mailbox: &Mailbox) -> Vec<(SharedString, SharedString)> {
    let last = input.rsplit(char::is_whitespace).next().unwrap_or("");
    let (negate, last) = last.strip_prefix('-').map_or(("", last), |l| ("-", l));
    let Some((key, partial)) = last.split_once(':') else {
        // no key yet: offer the keys themselves
        return ["tag:", "from:", "to:", "subject:", "is:", "has:", "account:", "newer:", "older:"]
            .iter()
            .filter(|k| !last.is_empty() && k.starts_with(&last.to_lowercase()))
            .map(|k| (SharedString::from(format!("{negate}{k}")), SharedString::from("filter")))
            .collect();
    };
    let partial = partial.to_lowercase();
    let values: Vec<(String, String)> = match key {
        "tag" => mailbox
            .tags
            .iter()
            .map(|t| {
                let n = mailbox.threads.iter().filter(|th| th.tags.contains(&t.name)).count();
                (t.name.to_string(), format!("{n} thread{}", if n == 1 { "" } else { "s" }))
            })
            .collect(),
        "from" | "to" => std::iter::once(("me".to_string(), "you".to_string()))
            .chain(mailbox.contacts().into_iter().map(|c| (first_name(&c.name).to_lowercase(), c.email.to_string())))
            .collect(),
        "is" => ["unread", "read", "starred"].iter().map(|s| (s.to_string(), String::new())).collect(),
        "has" => ["attachment", "html"].iter().map(|s| (s.to_string(), String::new())).collect(),
        "account" => mailbox.accounts.iter().map(|a| (a.id.to_string(), a.email.to_string())).collect(),
        "newer" | "older" => ["1h", "24h", "7d", "30d"].iter().map(|s| (s.to_string(), String::new())).collect(),
        _ => Vec::new(),
    };
    let mut seen = std::collections::HashSet::new();
    values
        .into_iter()
        .filter(|(v, _)| v.starts_with(&partial) && seen.insert(v.clone()))
        .take(8)
        .map(|(v, detail)| (SharedString::from(format!("{negate}{key}:{v}")), SharedString::from(detail)))
        .collect()
}

/// Replace the last token of `input` with `completion` (and add a trailing space).
pub fn apply_completion(input: &str, completion: &str) -> String {
    let start = input.rfind(char::is_whitespace).map_or(0, |i| i + 1);
    format!("{}{completion} ", &input[..start])
}

#[cfg(test)]
mod folded {
    use super::contains_folded;

    #[test]
    fn case_insensitive_without_copies() {
        assert!(contains_folded("Rechnung für Oktober", "oktober"));
        assert!(contains_folded("Grüße aus KÖLN", "köln"), "non-ASCII needles fold too");
        assert!(!contains_folded("abc", "abcd"));
        assert!(contains_folded("ä😀x", "x"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_negation_quotes_and_durations() {
        let q = Query::parse(r#"tag:review -tag:done "exact phrase" newer:24h from:mara"#);
        assert_eq!(q.clauses.len(), 5);
        assert!(q.clauses[1].negate);
        assert_eq!(q.clauses[2].term, Term::Text("exact phrase".into()));
        assert_eq!(q.clauses[3].term, Term::Newer(24.));
        assert!(Query::parse("tag:").is_empty(), "an unfinished key matches everything");
        assert!(Query::parse("tag:o").clauses[0].prefix && !Query::parse("tag:o ").clauses[0].prefix);
    }
}
