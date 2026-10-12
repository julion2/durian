//! Splits a text/plain body into what was written and what came along: the quoted history
//! ("On … wrote:", Outlook's header block, trailing `>` lines) and the signature (`-- `). The
//! thread view shows the first and folds the rest behind "…".
//!
//! Conservative: a body that is nothing but quote stays whole, and a reply written between
//! quoted lines (inline answers) is not cut.

/// (what was written, the quoted history / signature if any)
pub fn split(body: &str) -> (String, Option<String>) {
    let lines: Vec<&str> = body.lines().collect();
    let Some(cut) = cut(&lines) else { return (body.to_string(), None) };
    let written = lines[..cut].join("\n").trim_end().to_string();
    let rest = lines[cut..].join("\n").trim().to_string();
    if written.trim().is_empty() || rest.is_empty() { (body.to_string(), None) } else { (written, Some(rest)) }
}

/// The first line of the history, if there is one.
fn cut(lines: &[&str]) -> Option<usize> {
    let rule = |l: &str| l.trim().len() >= 5 && l.trim().chars().all(|c| c == '_' || c == '-');
    let ix = (0..lines.len()).find(|&ix| starts_history(lines, ix)).or_else(|| trailing_quote(lines))?;
    // Outlook's rule above its header block goes with it
    Some(if ix > 0 && rule(lines[ix - 1]) { ix - 1 } else { ix })
}

fn starts_history(lines: &[&str], ix: usize) -> bool {
    let line = lines[ix].trim();
    let next = lines.get(ix + 1).map_or("", |l| l.trim());
    // the signature delimiter (RFC 3676: "-- "), tolerating editors that eat the space
    if lines[ix] == "-- " || line == "--" {
        return true;
    }
    if matches!(line, "-----Original Message-----" | "-----Ursprüngliche Nachricht-----" | "-----Message d'origine-----") {
        return true;
    }
    // "On Tue, Oct 6, 2026 at 10:00, Mara <m@x> wrote:" – often wrapped onto two lines
    let attribution = |l: &str| {
        (l.starts_with("On ") && l.ends_with("wrote:"))
            || (l.starts_with("Am ") && l.contains("schrieb") && l.ends_with(':'))
            || (l.starts_with("Le ") && l.ends_with("a écrit :"))
    };
    if attribution(line) || (line.starts_with("On ") || line.starts_with("Am ")) && !line.ends_with(':') && attribution(&format!("{line} {next}")) {
        return true;
    }
    // Outlook: "From: …" with "Sent:" / "Date:" right below, after a rule or a blank line
    let header = |l: &str| ["From:", "Von:", "De :"].iter().any(|p| l.starts_with(p));
    let dated = |l: &str| ["Sent:", "Gesendet:", "Date:", "Datum:", "Envoyé :"].iter().any(|p| l.starts_with(p));
    let after_break = ix == 0 || lines[ix - 1].trim().is_empty() || lines[ix - 1].trim().chars().all(|c| c == '_' || c == '-');
    header(line) && after_break && lines[ix + 1..].iter().take(3).any(|l| dated(l.trim()))
}

/// A block of `>` lines that runs to the end (bottom-quoting), with nothing written inside it.
fn trailing_quote(lines: &[&str]) -> Option<usize> {
    let quoted = |l: &&str| l.trim_start().starts_with('>');
    let last = lines.iter().rposition(|l| !l.trim().is_empty())?;
    if !quoted(&lines[last]) {
        return None;
    }
    let mut start = last;
    while start > 0 && (quoted(&lines[start - 1]) || lines[start - 1].trim().is_empty()) {
        start -= 1;
    }
    Some(start)
}

#[cfg(test)]
mod tests {
    use super::split;

    #[test]
    fn attribution_lines_start_the_history() {
        let (written, quoted) = split("Sounds good.\n\nOn Tue, Oct 6, 2026 at 10:00, Mara <m@x.org> wrote:\n> the plan\n> more");
        assert_eq!(written, "Sounds good.");
        assert!(quoted.unwrap().starts_with("On Tue"));
        // wrapped onto two lines
        let (written, _) = split("Ja.\n\nAm Di., 6. Okt. 2026 um 10:00 Uhr schrieb Mara\n<m@x.org>:\n> Plan");
        assert_eq!(written, "Ja.");
        let (written, _) = split("ok\n\nOn Tue, Oct 6, 2026 at 10:00 Mara Lindqvist <m@x.org>\nwrote:\n> hi");
        assert_eq!(written, "ok");
    }

    #[test]
    fn outlook_headers_and_signatures() {
        let (written, quoted) = split("Thanks!\n\n________________________________\nFrom: Mara\nSent: Tuesday\nTo: me\n\nold text");
        assert_eq!(written, "Thanks!");
        assert!(quoted.unwrap().starts_with("____"));
        let (written, quoted) = split("Best\nAda\n-- \nAda Lovelace\nEngine Co.");
        assert_eq!((written.as_str(), quoted.as_deref()), ("Best\nAda", Some("-- \nAda Lovelace\nEngine Co.".trim())));
        // "From:" in running text is not a header
        assert_eq!(split("From: the team, with love.\nSee you").1, None);
    }

    #[test]
    fn bottom_quotes_fold_but_inline_answers_stay() {
        let (written, quoted) = split("Yes, Friday.\n\n> Can we meet?\n> Thursday or Friday?");
        assert_eq!(written, "Yes, Friday.");
        assert_eq!(quoted.unwrap(), "> Can we meet?\n> Thursday or Friday?");
        let inline = "> Thursday?\nNo.\n> Friday?\nYes.";
        assert_eq!(split(inline), (inline.to_string(), None));
    }

    #[test]
    fn nothing_but_quote_stays_whole() {
        let body = "> forwarded thought\n> nothing else";
        assert_eq!(split(body), (body.to_string(), None));
        assert_eq!(split("just text").1, None);
    }
}

#[cfg(test)]
mod fuzz {
    #[test]
    fn mail_text_functions_never_panic() {
        // same generator idea as ui::plain's: multi-byte characters around the ASCII markers
        const PIECES: &[&str] = &["On ", " wrote:", "Am ", "schrieb", ":", "\n", "\r\n", "> ", ">", "-- ", "--", "From:", "Sent:", "____", "\u{a0}", "ä", "😀", "x", "[cid:", "]", "Re:", "AW:", "Re[2]:", " "];
        let mut seed: u64 = 0x9e37_79b9_7f4a_7c15;
        let mut next = || {
            seed ^= seed << 13;
            seed ^= seed >> 7;
            seed ^= seed << 17;
            seed
        };
        for _ in 0..20_000 {
            let len = (next() % 30) as usize;
            let text: String = (0..len).map(|_| PIECES[(next() % PIECES.len() as u64) as usize]).collect();
            let _ = super::split(&text);
            let _ = crate::durian::display_subject(&text);
            let _ = crate::durian::split_addresses(&text);
            let _ = crate::durian::parse_address(&text);
        }
    }
}
