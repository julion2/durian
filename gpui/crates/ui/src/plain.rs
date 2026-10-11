//! Mail's text/plain as it was written: line breaks kept, no Markdown interpretation (a line
//! of `____` is a rule someone typed, `- ` a dash, `**` two asterisks), web and mail addresses
//! clickable, quoted lines (`>`) quieter. Paragraphs (blank-line separated) are selectable
//! blocks of one document.

use std::ops::Range;

use gpui::{AnyElement, FontWeight, IntoElement, ParentElement, SharedString, Styled, TextRun, div, px};

use crate::theme::Theme;

/// Links in `text`: `http(s)://…`, `www.…` and `mailto:` addresses, with trailing punctuation
/// (and the brackets of `<https://…>`) left out. Returns byte ranges and the target.
pub fn links(text: &str) -> Vec<(Range<usize>, SharedString)> {
    let mut out = Vec::new();
    let mut i = 0;
    while i < text.len() {
        let rest = &text[i..];
        // `get`, not slicing: the prefix length may end inside a multi-byte character
        let starts = ["https://", "http://", "mailto:", "www."].iter().find(|p| rest.len() > p.len() && rest.get(..p.len()).is_some_and(|head| head.eq_ignore_ascii_case(p)));
        // only at a word start: "xhttp://" is not a link
        let at_word_start = i == 0 || text[..i].chars().next_back().is_some_and(|c| c.is_whitespace() || "<([\"'".contains(c));
        if let (Some(prefix), true) = (starts, at_word_start) {
            let len = rest.find(|c: char| c.is_whitespace() || "<>\"".contains(c)).unwrap_or(rest.len());
            // closing punctuation belongs to the sentence, unless the URL opened it
            let mut end = len;
            while end > prefix.len() {
                let c = rest[..end].chars().next_back().unwrap_or(' ');
                let unbalanced_paren = c == ')' && rest[..end].matches('(').count() < rest[..end].matches(')').count();
                if ".,;:!?'\"".contains(c) || unbalanced_paren || c == ']' {
                    end -= c.len_utf8();
                } else {
                    break;
                }
            }
            if end > prefix.len() {
                let url = &rest[..end];
                let target = if prefix.eq_ignore_ascii_case("www.") { format!("https://{url}") } else { url.to_string() };
                out.push((i..i + end, target.into()));
                i += end;
                continue;
            }
        }
        i += rest.chars().next().map_or(1, char::len_utf8);
    }
    out
}

/// A body as paragraphs of lines (blank lines separate paragraphs; runs of them collapse).
fn paragraphs(body: &str) -> Vec<Vec<&str>> {
    let mut out: Vec<Vec<&str>> = Vec::new();
    let mut current: Vec<&str> = Vec::new();
    for line in body.lines() {
        if line.trim().is_empty() {
            if !current.is_empty() {
                out.push(std::mem::take(&mut current));
            }
        } else {
            current.push(line.trim_end());
        }
    }
    if !current.is_empty() {
        out.push(current);
    }
    out
}

fn run(len: usize, family: &str, color: gpui::Hsla, underline: bool) -> TextRun {
    TextRun {
        len,
        font: gpui::Font { family: family.to_string().into(), features: Default::default(), fallbacks: None, weight: FontWeight::NORMAL, style: gpui::FontStyle::Normal },
        color,
        background_color: None,
        underline: underline.then(|| gpui::UnderlineStyle { color: Some(color.opacity(0.4)), thickness: px(1.), wavy: false }),
        strikethrough: None,
    }
}

/// One paragraph's text, style runs and links.
fn paragraph(lines: &[&str], family: &str, theme: &Theme) -> (String, Vec<TextRun>, Vec<(Range<usize>, SharedString)>) {
    let (mut text, mut runs, mut all_links) = (String::new(), Vec::new(), Vec::new());
    for (n, line) in lines.iter().enumerate() {
        if n > 0 {
            text.push('\n');
            runs.push(run(1, family, theme.text_body, false));
        }
        let base = if line.trim_start().starts_with('>') { theme.text_tertiary } else { theme.text_body };
        let offset = text.len();
        let mut at = 0;
        for (range, url) in links(line) {
            if range.start > at {
                runs.push(run(range.start - at, family, base, false));
            }
            runs.push(run(range.len(), family, theme.accent, true));
            all_links.push((offset + range.start..offset + range.end, url));
            at = range.end;
        }
        if line.len() > at {
            runs.push(run(line.len() - at, family, base, false));
        }
        text.push_str(line);
    }
    (text, runs, all_links)
}

/// A text/plain body, selectable within `selection`.
pub fn render_selectable(body: &str, family: &str, theme: &Theme, selection: &crate::Selection) -> AnyElement {
    div()
        .flex()
        .flex_col()
        .gap(px(10.))
        .children(paragraphs(body).into_iter().map(|lines| {
            let (text, runs, links) = paragraph(&lines, family, theme);
            crate::SelectableText::new(selection, text, runs).links(links).into_any_element()
        }))
        .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn found(text: &str) -> Vec<(&str, String)> {
        links(text).into_iter().map(|(r, url)| (&text[r], url.to_string())).collect()
    }

    #[test]
    fn links_stop_before_sentence_punctuation() {
        assert_eq!(found("See https://x.org/a, then."), [("https://x.org/a", "https://x.org/a".into())]);
        assert_eq!(found("<https://x.org/b?c=d>"), [("https://x.org/b?c=d", "https://x.org/b?c=d".into())]);
        assert_eq!(found("(see https://en.wikipedia.org/wiki/Foo_(bar))"), [("https://en.wikipedia.org/wiki/Foo_(bar)", "https://en.wikipedia.org/wiki/Foo_(bar)".into())]);
        assert_eq!(found("www.durian.dev!"), [("www.durian.dev", "https://www.durian.dev".into())]);
        assert_eq!(found("write mailto:a@x.org"), [("mailto:a@x.org", "mailto:a@x.org".into())]);
        assert!(found("xhttps://nope.org and https://").is_empty());
    }

    #[test]
    fn any_text_is_safe_to_scan() {
        // multi-byte characters where a prefix comparison would cut them (a crash on real mail)
        assert!(found("Grüße\u{a0}aus Köln – ½ € 😀").is_empty());
        assert_eq!(found("Läuft:\u{a0}https://x.org/ä?q=ö 😀"), [("https://x.org/ä?q=ö", "https://x.org/ä?q=ö".into())]);
        for text in ["\u{a0}\u{a0}www", "ééééééé", "h\u{a0}ttps://", "mailto😀"] {
            let _ = links(text);
        }
    }

    #[test]
    fn paragraphs_keep_lines_and_collapse_blanks() {
        assert_eq!(paragraphs("Hi,\nline two\n\n\n\n____\n- not a list"), vec![vec!["Hi,", "line two"], vec!["____", "- not a list"]]);
    }
}

#[cfg(test)]
mod fuzz {
    /// Random text from pieces that trip byte-based code: multi-byte characters next to the
    /// ASCII the scanner looks for. Deterministic (fixed seed), so a failure reproduces.
    pub fn texts(count: usize) -> Vec<String> {
        const PIECES: &[&str] = &["h", "ttps://", "http", "www.", "mailto:", ":", "/", ".", ")", "(", " ", "\n", "\r\n", "\u{a0}", "ä", "€", "😀", "x", "[cid:", "]", ">", "<", "\"", "Re:", "AW: ", "-- ", "On ", " wrote:"];
        let mut seed: u64 = 0x2545_f491_4f6c_dd1d;
        (0..count)
            .map(|_| {
                seed ^= seed << 13;
                seed ^= seed >> 7;
                seed ^= seed << 17;
                let len = (seed % 40) as usize;
                (0..len)
                    .map(|_| {
                        seed ^= seed << 13;
                        seed ^= seed >> 7;
                        seed ^= seed << 17;
                        PIECES[(seed % PIECES.len() as u64) as usize]
                    })
                    .collect()
            })
            .collect()
    }

    #[test]
    fn links_never_panic_and_stay_on_char_boundaries() {
        for text in texts(20_000) {
            for (range, _) in super::links(&text) {
                assert!(text.is_char_boundary(range.start) && text.is_char_boundary(range.end), "{text:?}");
            }
            let _ = super::paragraphs(&text);
        }
    }
}
