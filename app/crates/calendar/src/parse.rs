//! Natural-language events for ⌘K and the composer's "When" field, English and German:
//!
//! ```text
//! Lunch with Mara tomorrow 12:30          Lunch mit Mara morgen 12:30
//! Review fri 14-15                        Zahnarzt 12.10. um 8:30 für 45min
//! Call Tobias 3pm for 30m                 Offsite next friday all day
//! ```
//!
//! Recognised pieces (day, time, range, duration, people) are taken out; what remains is the
//! title. Without any date or time in the text there is no event – plain ⌘K searches stay
//! searches.

use chrono::{Datelike, Duration, NaiveDate, NaiveDateTime, NaiveTime, Weekday};

#[derive(Debug, Clone, PartialEq)]
pub struct Parsed {
    pub title: String,
    pub start: NaiveDateTime,
    pub end: NaiveDateTime,
    pub all_day: bool,
    /// names as they matched the known people (`with Mara` → "Mara Lindqvist")
    pub people: Vec<String>,
}

const MONTHS: [(&str, u32); 24] = [
    ("jan", 1), ("feb", 2), ("mar", 3), ("apr", 4), ("may", 5), ("jun", 6), ("jul", 7), ("aug", 8), ("sep", 9), ("oct", 10), ("nov", 11), ("dec", 12),
    ("jän", 1), ("mär", 3), ("mai", 5), ("okt", 10), ("dez", 12), ("januar", 1), ("februar", 2), ("märz", 3), ("juni", 6), ("juli", 7), ("oktober", 10), ("dezember", 12),
];

fn weekday(word: &str) -> Option<Weekday> {
    let w = word.trim_end_matches('.');
    Some(match w {
        "mon" | "monday" | "mo" | "montag" => Weekday::Mon,
        "tue" | "tues" | "tuesday" | "di" | "dienstag" => Weekday::Tue,
        "wed" | "wednesday" | "mi" | "mittwoch" => Weekday::Wed,
        "thu" | "thur" | "thurs" | "thursday" | "do" | "donnerstag" => Weekday::Thu,
        "fri" | "friday" | "fr" | "freitag" => Weekday::Fri,
        "sat" | "saturday" | "sa" | "samstag" => Weekday::Sat,
        "sun" | "sunday" | "so" | "sonntag" => Weekday::Sun,
        _ => return None,
    })
}

fn month(word: &str) -> Option<u32> {
    let w = word.trim_end_matches('.');
    MONTHS.iter().find(|(name, _)| w == *name || (w.len() >= 3 && name.len() == 3 && w.starts_with(name) && w.len() <= 9)).map(|(_, m)| *m)
}

/// "12:30", "9", "15h", "3pm", "3:15pm" → time of day.
fn clock(token: &str) -> Option<NaiveTime> {
    let t = token.trim_end_matches(['h', '.']);
    let (t, pm) = if let Some(t) = t.strip_suffix("pm") {
        (t, Some(true))
    } else if let Some(t) = t.strip_suffix("am") {
        (t, Some(false))
    } else {
        (t, None)
    };
    let (h, m) = match t.split_once(':') {
        Some((h, m)) => (h.parse::<u32>().ok()?, m.parse::<u32>().ok()?),
        None => (t.parse::<u32>().ok()?, 0),
    };
    let h = match pm {
        Some(true) if h < 12 => h + 12,
        Some(false) if h == 12 => 0,
        _ => h,
    };
    NaiveTime::from_hms_opt(h, m, 0)
}

/// "30m", "45min", "1h", "1.5h", "90" (minutes) → duration.
fn duration(token: &str) -> Option<Duration> {
    let units = [("minutes", 1.), ("minuten", 1.), ("mins", 1.), ("min", 1.), ("m", 1.), ("hours", 60.), ("stunden", 60.), ("std", 60.), ("h", 60.)];
    let (number, factor) = units.iter().find_map(|(u, f)| token.strip_suffix(u).map(|n| (n, *f))).unwrap_or((token, 1.));
    let n: f64 = number.replace(',', ".").parse().ok()?;
    (n > 0.).then(|| Duration::minutes((n * factor).round() as i64))
}

/// "12.10." / "12.10.2026" / "12/10" (day first) → date in the coming year.
fn numeric_date(token: &str, today: NaiveDate) -> Option<NaiveDate> {
    let parts: Vec<&str> = token.trim_end_matches('.').split(['.', '/']).collect();
    if parts.len() < 2 || parts.len() > 3 || !token.contains(['.', '/']) {
        return None;
    }
    let day: u32 = parts[0].parse().ok()?;
    let month: u32 = parts[1].parse().ok()?;
    let year = match parts.get(2) {
        Some(y) => y.parse::<i32>().ok().map(|y| if y < 100 { 2000 + y } else { y })?,
        None => today.year(),
    };
    let date = NaiveDate::from_ymd_opt(year, month, day)?;
    Some(if parts.len() == 2 && date < today { NaiveDate::from_ymd_opt(year + 1, month, day)? } else { date })
}

fn next_weekday(today: NaiveDate, target: Weekday, skip_week: bool) -> NaiveDate {
    let ahead = (target.num_days_from_monday() as i64 - today.weekday().num_days_from_monday() as i64).rem_euclid(7);
    today + Duration::days(if skip_week && ahead == 0 { 7 } else { ahead })
}

pub fn parse(input: &str, now: NaiveDateTime, people: &[&str]) -> Option<Parsed> {
    let today = now.date();
    let tokens: Vec<String> = input.split_whitespace().map(str::to_string).collect();
    let lower: Vec<String> = tokens.iter().map(|t| t.to_lowercase().trim_end_matches(',').to_string()).collect();
    let mut used = vec![false; tokens.len()];
    let mut date: Option<NaiveDate> = None;
    let mut start: Option<NaiveTime> = None;
    let mut end: Option<NaiveTime> = None;
    let mut length: Option<Duration> = None;
    let mut all_day = false;
    let mut found_people = Vec::new();

    let mut end_date: Option<NaiveDate> = None;
    let mut connector: Option<usize> = None;
    // The first day found is the start; a day after a connector (or after an end day) is the end.
    macro_rules! set_date {
        ($d:expr) => {{
            let d = $d;
            match connector.take() {
                Some(k) if date.is_some() => {
                    used[k] = true;
                    end_date = Some(d);
                }
                _ if end_date.is_some() => end_date = Some(d),
                _ => date = Some(d),
            }
        }};
    }

    let mut i = 0;
    while i < tokens.len() {
        let w = lower[i].as_str();
        let next = lower.get(i + 1).map(String::as_str);
        let take = |n: usize, used: &mut Vec<bool>| (i..(i + n).min(tokens.len())).for_each(|k| used[k] = true);

        // "– Sat, Oct 10 …": a connector before a second day makes it the end day
        if matches!(w, "-" | "–" | "to" | "bis" | "until") && date.is_some() {
            let starts_date = next.is_some_and(|n| {
                weekday(n).is_some() || month(n).is_some() || numeric_date(n, today).is_some() || matches!(n, "today" | "heute" | "tomorrow" | "morgen")
            });
            if starts_date {
                connector = Some(i);
                i += 1;
                continue;
            }
        }

        // day words
        let relative = match w {
            "today" | "heute" => Some(0),
            "tomorrow" | "morgen" => Some(1),
            "übermorgen" => Some(2),
            _ => None,
        };
        if let Some(days) = relative {
            set_date!(today + Duration::days(days));
            take(1, &mut used);
            i += 1;
            continue;
        }
        if w == "day" && next == Some("after") && lower.get(i + 2).map(String::as_str) == Some("tomorrow") {
            set_date!(today + Duration::days(2));
            take(3, &mut used);
            i += 3;
            continue;
        }
        // "next friday" / "nächsten freitag"
        if matches!(w, "next" | "nächsten" | "nächster" | "naechsten") {
            if let Some(day) = next.and_then(weekday) {
                set_date!(next_weekday(today, day, true));
                take(2, &mut used);
                i += 2;
                continue;
            }
        }
        if let Some(day) = weekday(w) {
            // two-letter German abbreviations collide with words ("do", "so"): only as days
            // when a time or date follows or they stand alone at the end
            let short = w.len() == 2;
            let followed_by_time = next.is_some_and(|n| clock(n).is_some() || matches!(n, "um" | "at"));
            if !short || followed_by_time || i + 1 == tokens.len() {
                set_date!(next_weekday(today, day, false));
                take(1, &mut used);
                i += 1;
                continue;
            }
        }
        if let Some(d) = numeric_date(w, today) {
            set_date!(d);
            take(1, &mut used);
            i += 1;
            continue;
        }
        // "oct 12" / "12 oct" / "12. oktober"
        if let (Some(m), Some(day)) = (month(w), next.and_then(|n| n.trim_end_matches('.').parse::<u32>().ok())) {
            if let Some(d) = NaiveDate::from_ymd_opt(today.year(), m, day) {
                set_date!(if d < today { d.with_year(today.year() + 1).unwrap_or(d) } else { d });
                take(2, &mut used);
                i += 2;
                continue;
            }
        }
        if let (Ok(day), Some(m)) = (w.trim_end_matches('.').parse::<u32>(), next.and_then(month)) {
            if let Some(d) = NaiveDate::from_ymd_opt(today.year(), m, day) {
                set_date!(if d < today { d.with_year(today.year() + 1).unwrap_or(d) } else { d });
                take(2, &mut used);
                i += 2;
                continue;
            }
        }
        if (w == "all" && next == Some("day")) || matches!(w, "ganztägig" | "ganztags") {
            all_day = true;
            let n = if w == "all" { 2 } else { 1 };
            take(n, &mut used);
            i += n;
            continue;
        }
        // durations: "for 30m", "für 1h"
        if matches!(w, "for" | "für") {
            if let Some(d) = next.and_then(duration) {
                length = Some(d);
                take(2, &mut used);
                i += 2;
                continue;
            }
        }
        // people: "with Mara and Tobias" / "mit Mara, Tobias"
        if matches!(w, "with" | "mit") {
            let mut k = i + 1;
            let mut names = Vec::new();
            while k < tokens.len() {
                let word = lower[k].as_str();
                if matches!(word, "and" | "und" | "&") {
                    k += 1;
                    continue;
                }
                let Some(full) = people.iter().find(|p| p.to_lowercase().split_whitespace().next() == Some(word) || p.to_lowercase() == word) else { break };
                names.push(full.to_string());
                k += 1;
            }
            if !names.is_empty() {
                found_people.extend(names);
                (i..k).for_each(|j| used[j] = true);
                i = k;
                continue;
            }
        }
        // times: "12:30", "um 12", "at 3pm", "15-16", "15:00–16:30", "14 bis 15", "12 uhr"
        let (skip_marker, candidate) = if matches!(w, "um" | "at" | "@") { (1, next) } else { (0, Some(w)) };
        if let Some(c) = candidate {
            let range = c.split_once(['-', '–']).and_then(|(a, b)| Some((clock(a)?, clock(b)?)));
            let marked = skip_marker == 1;
            // a bare number only counts as a time after "um"/"at", with ":"/"h"/am/pm/"uhr", or as
            // the start of a range that ends in a clear time ("14 bis 15:30")
            let explicit = c.contains(':') || c.ends_with('h') || c.ends_with("am") || c.ends_with("pm");
            let uhr = lower.get(i + skip_marker + 1).map(String::as_str) == Some("uhr");
            let range_start = matches!(lower.get(i + skip_marker + 1).map(String::as_str), Some("bis" | "to" | "-" | "–" | "until"))
                && lower.get(i + skip_marker + 2).is_some_and(|e| e.contains(':') || e.ends_with("pm") || e.ends_with("am"));
            if let Some((a, b)) = range {
                start = Some(a);
                end = Some(b);
                take(skip_marker + 1, &mut used);
                i += skip_marker + 1;
                continue;
            }
            if let Some(t) = clock(c).filter(|_| marked || explicit || uhr || range_start) {
                // after a second day ("… – Sat 11:00") a time ends the event
                if end_date.is_some() && start.is_some() {
                    end = Some(t);
                } else {
                    start = Some(t);
                }
                let mut n = skip_marker + 1 + usize::from(uhr);
                // "14 bis 15" / "2pm to 3pm" / "14 - 15"
                let after = lower.get(i + n).map(String::as_str);
                if matches!(after, Some("bis" | "to" | "-" | "–" | "until")) {
                    if let Some(e) = lower.get(i + n + 1).and_then(|e| clock(e)) {
                        end = Some(e);
                        n += 2;
                    }
                }
                take(n, &mut used);
                i += n;
                continue;
            }
        }
        i += 1;
    }

    if date.is_none() && start.is_none() && !all_day {
        return None;
    }
    // a time without a day: today if it's still ahead, else tomorrow
    let day = date.unwrap_or_else(|| match start {
        Some(t) if t <= now.time() => today + Duration::days(1),
        _ => today,
    });
    let end_day = end_date.filter(|d| *d > day);
    let (start_at, end_at, all_day) = match start {
        Some(s) if !all_day => {
            let start_at = day.and_time(s);
            let end_at = match (end_day, end, length) {
                // across days: "Fri 14:00 – Sat 11:00"
                (Some(d), e, _) => d.and_time(e.unwrap_or(s)),
                (None, Some(e), _) if e > s => day.and_time(e),
                (None, _, Some(d)) => start_at + d,
                _ => start_at + Duration::hours(1),
            };
            (start_at, end_at, false)
        }
        // only days: an all-day event, through the end day if there is one
        _ => (day.and_time(NaiveTime::MIN), (end_day.unwrap_or(day) + Duration::days(1)).and_time(NaiveTime::MIN), true),
    };

    let words: Vec<&str> = tokens.iter().zip(&used).filter(|(_, u)| !**u).map(|(t, _)| t.as_str()).collect();
    // connectors left dangling at the end ("Lunch on", "Termin am")
    let mut title_words = words;
    while title_words.last().is_some_and(|w| matches!(w.to_lowercase().as_str(), "on" | "am" | "at" | "um" | "with" | "mit" | "for" | "für" | "-")) {
        title_words.pop();
    }
    let mut title = title_words.join(" ");
    if let Some(first) = title.get(..1) {
        title = first.to_uppercase() + &title[1..];
    }
    Some(Parsed { title, start: start_at, end: end_at, all_day, people: found_people })
}

/// One line for previews: "Tomorrow · 12:30 – 13:30 · with Mara".
pub fn describe(p: &Parsed, today: NaiveDate) -> String {
    let day = match (p.start.date() - today).num_days() {
        0 => "Today".to_string(),
        1 => "Tomorrow".to_string(),
        2..=6 => p.start.format("%A").to_string(),
        _ => p.start.format("%a, %b %-d").to_string(),
    };
    let last = (p.end - Duration::minutes(1)).date();
    let when = match (p.all_day, last > p.start.date()) {
        (true, false) => format!("{day} · all day"),
        (true, true) => format!("{day} – {} · all day", last.format("%a, %b %-d")),
        (false, false) => format!("{day} · {} – {}", p.start.format("%H:%M"), p.end.format("%H:%M")),
        (false, true) => format!("{day} {} – {} {}", p.start.format("%H:%M"), p.end.format("%a"), p.end.format("%H:%M")),
    };
    let people: Vec<&str> = p.people.iter().filter_map(|n| n.split_whitespace().next()).collect();
    if people.is_empty() { when } else { format!("{when} · with {}", people.join(", ")) }
}

#[cfg(test)]
mod tests {
    use super::*;

    const PEOPLE: &[&str] = &["Mara Lindqvist", "Tobias Brandt", "Jonas Weber"];

    /// Friday, Oct 9 2026, 10:00
    fn now() -> NaiveDateTime {
        NaiveDate::from_ymd_opt(2026, 10, 9).unwrap().and_hms_opt(10, 0, 0).unwrap()
    }

    fn at(day: u32, h: u32, m: u32) -> NaiveDateTime {
        NaiveDate::from_ymd_opt(2026, 10, day).unwrap().and_hms_opt(h, m, 0).unwrap()
    }

    #[test]
    fn english_and_german_say_the_same() {
        for input in ["Lunch with Mara tomorrow 12:30", "Lunch mit Mara morgen 12:30", "lunch tomorrow at 12:30 with mara"] {
            let p = parse(input, now(), PEOPLE).unwrap();
            assert_eq!(p.title, "Lunch", "{input}");
            assert_eq!((p.start, p.end), (at(10, 12, 30), at(10, 13, 30)), "{input}");
            assert_eq!(p.people, ["Mara Lindqvist"], "{input}");
        }
    }

    #[test]
    fn ranges_durations_weekdays() {
        let p = parse("Review mon 14-15", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.start, p.end), ("Review", at(12, 14, 0), at(12, 15, 0)));

        let p = parse("Zahnarzt 14.10. um 8:30 für 45min", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.start, p.end), ("Zahnarzt", at(14, 8, 30), at(14, 9, 15)));

        let p = parse("Call Tobias 3pm for 30m", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.start, p.end), ("Call Tobias", at(9, 15, 0), at(9, 15, 30)));

        for input in ["Sync 14 bis 15:30", "Sync um 14 bis 15:30", "sync 2pm to 3:30pm"] {
            let p = parse(input, now(), PEOPLE).unwrap();
            assert_eq!((p.title.as_str(), p.start, p.end), ("Sync", at(9, 14, 0), at(9, 15, 30)), "{input}");
        }
        assert!(parse("Room 14 on floor 3", now(), PEOPLE).is_none(), "bare numbers alone are not times");
    }

    #[test]
    fn days_without_times_are_all_day() {
        let p = parse("Offsite next friday all day", now(), PEOPLE).unwrap();
        assert!(p.all_day);
        assert_eq!(p.start.date(), NaiveDate::from_ymd_opt(2026, 10, 16).unwrap(), "next friday skips today");
        let p = parse("Release oct 20", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.all_day, p.start.day()), ("Release", true, 20));
    }

    #[test]
    fn ranges_across_days() {
        let p = parse("Workshop Fri, Oct 9 14:00 – Sat, Oct 10 11:00", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.start, p.end, p.all_day), ("Workshop", at(9, 14, 0), at(10, 11, 0), false));

        let p = parse("Offsite oct 12 - oct 14", now(), PEOPLE).unwrap();
        assert_eq!((p.title.as_str(), p.start, p.end, p.all_day), ("Offsite", at(12, 0, 0), at(15, 0, 0), true));

        let p = parse("Fri, Oct 9 – Mon, Oct 12 all day", now(), PEOPLE).unwrap();
        assert_eq!((p.start, p.end, p.all_day), (at(9, 0, 0), at(13, 0, 0), true));

        assert_eq!(parse("Q&A - planning tomorrow", now(), PEOPLE).unwrap().title, "Q&A - planning", "a dash that isn't a range stays");
    }

    #[test]
    fn past_times_roll_to_tomorrow_and_plain_text_is_no_event() {
        let p = parse("Coffee 9:00", now(), PEOPLE).unwrap();
        assert_eq!(p.start, at(10, 9, 0), "9:00 already passed today");
        assert!(parse("postmortem review", now(), PEOPLE).is_none());
        assert!(parse("do the thing", now(), PEOPLE).is_none(), "'do' is a word here, not Thursday");
    }
}
