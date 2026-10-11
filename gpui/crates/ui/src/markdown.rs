//! Just enough Markdown for mail: **bold**, *italic* / _italic_, `code`, [links](url), bare URLs,
//! fenced code, > quotes, - / 1. lists and # headings.
//!
//! Two consumers: message bodies render it (markers hidden), and the composer styles it live
//! (markers kept but faded), so what you type is what gets sent.

use std::ops::Range;
use std::sync::Arc;

use gpui::{
    AnyElement, ElementId, Font, FontFeatures, FontStyle, FontWeight, Hsla, InteractiveText, IntoElement, SharedString,
    StyledText, TextRun, UnderlineStyle, div, prelude::*, px,
};

use crate::theme::{Theme, typography::*};


#[derive(Clone, Copy, Default, PartialEq)]
struct Style {
    bold: bool,
    italic: bool,
    code: bool,
    link: bool,
    marker: bool,
    /// inside a ``` fence (whole line in mono)
    fence: bool,
    /// heading text
    heading: bool,
    /// list bullet / number / quote bar shown in place of the source prefix
    bullet: bool,
    quote: bool,
}

/// A closed inline construct (`**bold**`, `` `code` ``, `[text](url)`) and its syntax markers.
#[derive(Clone)]
struct Construct {
    full: Range<usize>,
    markers: Vec<Range<usize>>,
}

/// Per-byte inline styles for one line, plus the link targets.
fn inline(line: &str) -> (Vec<Style>, Vec<(Range<usize>, SharedString)>) {
    let (styles, links, _) = inline_constructs(line);
    (styles, links)
}

fn inline_constructs(line: &str) -> (Vec<Style>, Vec<(Range<usize>, SharedString)>, Vec<Construct>) {
    let bytes = line.as_bytes();
    let mut styles = vec![Style::default(); line.len()];
    let mut links = Vec::new();
    let mut constructs = Vec::new();
    let (mut bold, mut italic) = (false, false);
    let (mut bold_open, mut italic_open) = (0, 0);
    let mut i = 0;
    let closes = |from: usize, pat: &str| line[from..].find(pat).map(|p| p + from);
    while i < line.len() {
        let rest = &line[i..];
        let prev_alnum = i > 0 && (bytes[i - 1] as char).is_alphanumeric();
        if rest.starts_with('`') {
            if let Some(end) = closes(i + 1, "`") {
                styles[i].marker = true;
                styles[end].marker = true;
                constructs.push(Construct { full: i..end + 1, markers: vec![i..i + 1, end..end + 1] });
                for s in &mut styles[i + 1..end] {
                    s.code = true;
                }
                i = end + 1;
                continue;
            }
        }
        if rest.starts_with("**") && (bold || closes(i + 2, "**").is_some()) {
            styles[i].marker = true;
            styles[i + 1].marker = true;
            if bold {
                constructs.push(Construct { full: bold_open..i + 2, markers: vec![bold_open..bold_open + 2, i..i + 2] });
            } else {
                bold_open = i;
            }
            bold = !bold;
            i += 2;
            continue;
        }
        if (rest.starts_with('*') || (rest.starts_with('_') && (italic || !prev_alnum)))
            && (italic || closes(i + 1, &rest[..1]).is_some_and(|e| e > i + 1))
        {
            styles[i].marker = true;
            if italic {
                constructs.push(Construct { full: italic_open..i + 1, markers: vec![italic_open..italic_open + 1, i..i + 1] });
            } else {
                italic_open = i;
            }
            italic = !italic;
            i += 1;
            continue;
        }
        if rest.starts_with('[')
            && let Some(mid) = closes(i, "](")
            && let Some(end) = closes(mid, ")")
        {
            let url: SharedString = line[mid + 2..end].to_string().into();
            styles[i].marker = true;
            for s in &mut styles[mid..=end] {
                s.marker = true;
            }
            for s in &mut styles[i + 1..mid] {
                s.link = true;
            }
            links.push((i + 1..mid, url));
            constructs.push(Construct { full: i..end + 1, markers: vec![i..i + 1, mid..end + 1] });
            i = end + 1;
            continue;
        }
        if rest.starts_with("http://") || rest.starts_with("https://") {
            let end = rest.find(char::is_whitespace).map_or(line.len(), |e| i + e);
            for s in &mut styles[i..end] {
                s.link = true;
            }
            links.push((i..end, line[i..end].to_string().into()));
            i = end;
            continue;
        }
        let len = rest.chars().next().map_or(1, char::len_utf8);
        for s in &mut styles[i..i + len] {
            s.bold = bold;
            s.italic = italic;
        }
        i += len;
    }
    (styles, links, constructs)
}

struct Palette {
    text: Hsla,
    faint: Hsla,
    link: Hsla,
    code_bg: Hsla,
    code_fg: Hsla,
    mono: &'static str,
}

fn palette(theme: &Theme) -> Palette {
    Palette { text: theme.text_body, faint: theme.text_quaternary, link: theme.accent, code_bg: theme.bg_code, code_fg: theme.text_code, mono: theme.font_mono }
}

fn run(len: usize, family: &str, weight: FontWeight, italic: bool, color: Hsla, bg: Option<Hsla>, underline: bool) -> TextRun {
    TextRun {
        len,
        font: Font {
            family: SharedString::from(family.to_string()),
            features: FontFeatures(Arc::new(Vec::new())),
            fallbacks: None,
            weight,
            style: if italic { FontStyle::Italic } else { FontStyle::Normal },
        },
        color,
        background_color: bg,
        underline: underline.then(|| UnderlineStyle { color: Some(color.opacity(0.4)), thickness: px(1.), wavy: false }),
        strikethrough: None,
    }
}

fn style_run(style: Style, len: usize, family: &str, base_weight: FontWeight, p: &Palette) -> TextRun {
    if style.fence {
        return run(len, p.mono, WEIGHT_NORMAL, false, if style.marker { p.faint } else { p.code_fg }, Some(p.code_bg), false);
    }
    if style.marker {
        return run(len, family, base_weight, false, p.faint, None, false);
    }
    if style.bullet {
        return run(len, family, WEIGHT_SEMIBOLD, false, p.faint, None, false);
    }
    let weight = if style.bold || style.heading { WEIGHT_SEMIBOLD } else { base_weight };
    if style.quote {
        return run(len, family, weight, true, p.faint.opacity(1.), None, false);
    }
    if style.code {
        return run(len, p.mono, weight, false, p.code_fg, Some(p.code_bg), false);
    }
    run(len, family, weight, style.italic, if style.link { p.link } else { p.text }, None, style.link)
}

/// Merge per-byte styles into runs.
fn runs_from(styles: &[Style], family: &str, weight: FontWeight, p: &Palette) -> Vec<TextRun> {
    let mut runs: Vec<TextRun> = Vec::new();
    let mut start = 0;
    for i in 1..=styles.len() {
        if i == styles.len() || styles[i] != styles[start] {
            runs.push(style_run(styles[start], i - start, family, weight, p));
            start = i;
        }
    }
    runs
}

/// What the composer shows for its Markdown source: the display text, its runs and offset maps.
pub struct LiveText {
    pub display: SharedString,
    pub runs: Vec<TextRun>,
    /// source byte offset → display byte offset (len = source.len() + 1)
    pub to_display: Vec<usize>,
    /// display byte offset → source byte offset (len = display.len() + 1)
    pub to_source: Vec<usize>,
}

/// Live rendering for the composer: closed inline markers disappear
/// unless the cursor is inside that construct, "- " shows as "• ", fences render as code.
/// Editing still happens on the Markdown source; the maps translate cursor and selection.
pub fn live(text: &str, cursor: Option<usize>, family: &str, theme: &Theme) -> LiveText {
    let p = palette(theme);
    // per-source-byte styles and the source ranges to replace in the display
    let mut styles = vec![Style::default(); text.len()];
    let mut replace: Vec<(Range<usize>, &'static str)> = Vec::new();
    let mut in_fence = false;
    let mut offset = 0;
    for line in text.split('\n') {
        let range = offset..offset + line.len();
        offset += line.len() + 1;
        let fence = line.trim_start().starts_with("```");
        if fence || in_fence {
            in_fence ^= fence;
            for s in &mut styles[range] {
                s.fence = true;
                s.marker = fence;
            }
            continue;
        }
        let (prefix, _) = block_prefix(line);
        let base = range.start;
        if prefix > 0 {
            let cursor_in_prefix = cursor.is_some_and(|c| c >= base && c < base + prefix);
            if line.starts_with("- ") || line.starts_with("* ") {
                if !cursor_in_prefix {
                    replace.push((base..base + 2, "• "));
                }
                styles[base].bullet = true;
                styles[base + 1].bullet = true;
            } else {
                for s in &mut styles[base..base + prefix] {
                    s.marker = true;
                }
                if line.starts_with('#') && !cursor_in_prefix {
                    replace.push((base..base + prefix, ""));
                }
            }
        }
        let body = &line[prefix..];
        let (inline_styles, _, constructs) = inline_constructs(body);
        let heading = line.starts_with('#') && prefix > 0;
        let quote = line.starts_with("> ");
        for (i, mut s) in inline_styles.into_iter().enumerate() {
            s.heading = heading;
            s.quote = quote && !s.marker;
            styles[base + prefix + i] = s;
        }
        for c in constructs {
            let full = base + prefix + c.full.start..base + prefix + c.full.end;
            // reveal the syntax while the cursor is inside; hide it once you've moved past
            if cursor.is_some_and(|c| c >= full.start && c < full.end) {
                continue;
            }
            replace.extend(c.markers.into_iter().map(|m| (base + prefix + m.start..base + prefix + m.end, "")));
        }
    }
    replace.sort_by_key(|(r, _)| r.start);

    let mut display = String::new();
    let mut display_styles: Vec<Style> = Vec::new();
    let mut to_display = vec![0; text.len() + 1];
    let mut to_source = Vec::new();
    let mut next = replace.iter().peekable();
    let mut i = 0;
    while i < text.len() {
        if let Some((range, with)) = next.next_if(|(r, _)| r.start == i) {
            let bullet = Style { bullet: true, ..Style::default() };
            for (j, _) in with.char_indices().flat_map(|(k, ch)| std::iter::repeat_n((k, ch), ch.len_utf8())) {
                let _ = j;
                to_source.push(range.start);
                display_styles.push(bullet);
            }
            for src in range.clone() {
                to_display[src] = display.len();
            }
            display.push_str(with);
            i = range.end;
            continue;
        }
        let ch = text[i..].chars().next().unwrap_or(' ');
        for k in 0..ch.len_utf8() {
            to_display[i + k] = display.len() + k;
            to_source.push(i + k);
            display_styles.push(styles[i + k]);
        }
        display.push(ch);
        i += ch.len_utf8();
    }
    to_display[text.len()] = display.len();
    to_source.push(text.len());

    // newlines must keep a plain run so paragraphs split cleanly
    let runs = if display.is_empty() { Vec::new() } else { runs_from(&display_styles, family, WEIGHT_NORMAL, &p) };
    LiveText { display: display.into(), runs, to_display, to_source }
}

/// Plain-text preview (list rows, collapsed messages): markers gone, bullets as "•", no fences.
pub fn plain_text(text: &str) -> String {
    let mut out = Vec::new();
    for block in blocks(text) {
        match block {
            Block::Paragraph(lines) => out.extend(lines.iter().map(|l| display_line_text(l))),
            Block::Heading(line) => out.push(display_line_text(line)),
            Block::Quote(lines) => out.extend(lines.iter().map(|l| display_line_text(l))),
            Block::List { ordered, items } => out.extend(
                items.iter().enumerate().map(|(i, item)| format!("{} {}", if ordered { format!("{}.", i + 1) } else { "•".into() }, display_line_text(item))),
            ),
            Block::Code(lines) => out.extend(lines.iter().map(|l| l.to_string())),
        }
    }
    out.join(" ")
}

fn display_line_text(line: &str) -> String {
    let (styles, _) = inline(line);
    line.char_indices().filter(|(i, _)| !styles[*i].marker).map(|(_, c)| c).collect()
}

/// Length of a block marker ("# ", "> ", "- ", "12. ") and the weight of the line's text.
fn block_prefix(line: &str) -> (usize, FontWeight) {
    if let Some(level) = line.find(|c| c != '#').filter(|&n| n > 0 && n <= 3 && line[n..].starts_with(' ')) {
        return (level + 1, WEIGHT_SEMIBOLD);
    }
    for p in ["> ", "- ", "* "] {
        if line.starts_with(p) {
            return (p.len(), WEIGHT_NORMAL);
        }
    }
    let digits = line.find(|c: char| !c.is_ascii_digit()).unwrap_or(0);
    if digits > 0 && line[digits..].starts_with(". ") {
        return (digits + 2, WEIGHT_NORMAL);
    }
    (0, WEIGHT_NORMAL)
}

/// One displayed line with markers removed: text, runs and clickable link ranges.
fn display_line(line: &str, family: &str, weight: FontWeight, p: &Palette) -> (String, Vec<TextRun>, Vec<(Range<usize>, SharedString)>) {
    let (styles, links) = inline(line);
    let mut text = String::new();
    let mut kept = Vec::new();
    let mut map = vec![0; line.len() + 1];
    for (i, ch) in line.char_indices() {
        map[i] = text.len();
        if !styles[i].marker {
            text.push(ch);
            kept.extend(std::iter::repeat_n(styles[i], ch.len_utf8()));
        }
    }
    map[line.len()] = text.len();
    let links = links.into_iter().map(|(r, url)| (map[r.start]..map[r.end], url)).collect();
    let runs = if text.is_empty() { Vec::new() } else { runs_from(&kept, family, weight, p) };
    (text, runs, links)
}

enum Block<'a> {
    Paragraph(Vec<&'a str>),
    Heading(&'a str),
    Code(Vec<&'a str>),
    Quote(Vec<&'a str>),
    List { ordered: bool, items: Vec<&'a str> },
}

fn blocks(text: &str) -> Vec<Block<'_>> {
    let mut out: Vec<Block> = Vec::new();
    let mut lines = text.split('\n').peekable();
    while let Some(line) = lines.next() {
        if line.trim().is_empty() {
            continue;
        }
        if line.trim_start().starts_with("```") {
            let mut code = Vec::new();
            for l in lines.by_ref() {
                if l.trim_start().starts_with("```") {
                    break;
                }
                code.push(l);
            }
            out.push(Block::Code(code));
            continue;
        }
        let (prefix, _) = block_prefix(line);
        if line.starts_with('#') && prefix > 0 {
            out.push(Block::Heading(&line[prefix..]));
        } else if line.starts_with("> ") {
            let mut quote = vec![&line[2..]];
            while let Some(l) = lines.next_if(|l| l.starts_with("> ")) {
                quote.push(&l[2..]);
            }
            out.push(Block::Quote(quote));
        } else if prefix > 0 {
            let ordered = line.as_bytes()[0].is_ascii_digit();
            let mut items = vec![&line[prefix..]];
            while let Some(l) = lines.next_if(|l| block_prefix(l).0 > 0 && !l.starts_with('#') && !l.starts_with("> ")) {
                items.push(&l[block_prefix(l).0..]);
            }
            out.push(Block::List { ordered, items });
        } else {
            let mut para = vec![line];
            while let Some(l) = lines.next_if(|l| !l.trim().is_empty() && block_prefix(l).0 == 0 && !l.trim_start().starts_with("```")) {
                para.push(l);
            }
            out.push(Block::Paragraph(para));
        }
    }
    out
}

fn text_block(id: ElementId, lines: &[&str], family: &str, weight: FontWeight, p: &Palette, selection: Option<&crate::Selection>) -> AnyElement {
    let (mut text, mut runs, mut links) = (String::new(), Vec::new(), Vec::new());
    for (ix, line) in lines.iter().enumerate() {
        if ix > 0 {
            text.push('\n');
            runs.push(run(1, family, weight, false, p.text, None, false));
        }
        let (t, r, l) = display_line(line, family, weight, p);
        let offset = text.len();
        links.extend(l.into_iter().map(|(range, url)| (range.start + offset..range.end + offset, url)));
        text.push_str(&t);
        runs.extend(r);
    }
    if let Some(selection) = selection {
        return crate::SelectableText::new(selection, text, runs).links(links).into_any_element();
    }
    let styled = StyledText::new(text).with_runs(runs);
    let (ranges, urls): (Vec<_>, Vec<_>) = links.into_iter().unzip();
    InteractiveText::new(id, styled).on_click(ranges, move |ix, _, cx| {
        crate::link::open(&urls[ix], cx);
    }).into_any_element()
}

/// Message body: Markdown rendered with markers hidden.
pub fn render(id: impl Into<SharedString>, body: &str, family: &str, theme: &Theme) -> AnyElement {
    render_with(id, body, family, theme, None)
}

/// The same, with every block's text selectable within `selection` (one document, reading
/// order: paragraphs, list items, quotes, code).
pub fn render_selectable(id: impl Into<SharedString>, body: &str, family: &str, theme: &Theme, selection: &crate::Selection) -> AnyElement {
    render_with(id, body, family, theme, Some(selection))
}

fn render_with(id: impl Into<SharedString>, body: &str, family: &str, theme: &Theme, selection: Option<&crate::Selection>) -> AnyElement {
    let id: SharedString = id.into();
    let p = palette(theme);
    let key = |n: usize| ElementId::Name(format!("{id}-{n}").into());
    div()
        .flex()
        .flex_col()
        .gap(px(10.))
        .children(blocks(body).into_iter().enumerate().map(|(n, block)| match block {
            Block::Paragraph(lines) => text_block(key(n), &lines, family, WEIGHT_NORMAL, &p, selection),
            Block::Heading(line) => div().text_size(px(17.)).child(text_block(key(n), &[line], family, WEIGHT_SEMIBOLD, &p, selection)).into_any_element(),
            Block::Code(lines) => div()
                .px(px(12.))
                .py(px(10.))
                .rounded(theme.radius.sm)
                .bg(p.code_bg)
                .font_family(p.mono)
                .text_size(px(SIZE_SMALL))
                .line_height(px(20.))
                .text_color(p.code_fg)
                .map(|d| match selection {
                    Some(sel) => {
                        let code = lines.join("\n");
                        let len = code.len();
                        d.child(crate::SelectableText::new(sel, code, vec![run(len, p.mono, WEIGHT_NORMAL, false, p.code_fg, None, false)]))
                    }
                    None => d.child(lines.join("\n")),
                })
                .into_any_element(),
            Block::Quote(lines) => div()
                .pl(px(12.))
                .border_l_2()
                .border_color(theme.border_card)
                .text_color(theme.text_meta)
                .child(text_block(key(n), &lines, family, WEIGHT_NORMAL, &Palette { text: theme.text_meta, ..palette(theme) }, selection))
                .into_any_element(),
            Block::List { ordered, items } => div()
                .flex()
                .flex_col()
                .gap(px(4.))
                .children(items.into_iter().enumerate().map(|(i, item)| {
                    div()
                        .flex()
                        .gap(px(8.))
                        .child(
                            div()
                                .flex_none()
                                .w(px(18.))
                                .text_color(theme.text_tertiary)
                                .child(if ordered { format!("{}.", i + 1) } else { "•".into() }),
                        )
                        .child(div().flex_1().min_w_0().child(text_block(ElementId::Name(format!("{id}-{n}-{i}").into()), &[item], family, WEIGHT_NORMAL, &p, selection)))
                }))
                .into_any_element(),
        }))
        .into_any_element()
}

// ---------------------------------------------------------------------------------------------
// HTML for the text/html part of outgoing mail. Same parser as the editor and the reader, so the
// recipient sees exactly what was shown while writing. Styles are inline: Gmail and Outlook drop
// <style> blocks.

const HTML_BODY: &str = "font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:14px;line-height:1.55;color:#1d1d24";
const HTML_P: &str = "margin:0 0 12px";
const HTML_CODE: &str = "font-family:Menlo,Consolas,monospace;font-size:12.5px;background:#f2f2f5;border-radius:4px;padding:1px 4px";
const HTML_PRE: &str = "font-family:Menlo,Consolas,monospace;font-size:12.5px;line-height:1.5;background:#f6f6f8;border-radius:6px;padding:10px 12px;margin:0 0 12px;white-space:pre-wrap";
const HTML_QUOTE: &str = "margin:0 0 12px;padding-left:12px;border-left:3px solid #e2e2e8;color:#5b5b66";
const HTML_LIST: &str = "margin:0 0 12px;padding-left:22px";
const HTML_LINK: &str = "color:#5e6ad2";

fn escape(text: &str) -> String {
    text.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace('"', "&quot;")
}

fn inline_html(line: &str) -> String {
    let (styles, links) = inline(line);
    let mut out = String::new();
    let mut open = Style::default();
    let mut open_link: Option<usize> = None;
    let close = |out: &mut String, s: Style, link: Option<usize>| {
        if link.is_some() {
            out.push_str("</a>");
        }
        if s.code {
            out.push_str("</code>");
        }
        if s.italic {
            out.push_str("</em>");
        }
        if s.bold {
            out.push_str("</strong>");
        }
    };
    for (i, ch) in line.char_indices() {
        let s = styles[i];
        if s.marker {
            continue;
        }
        let link = links.iter().position(|(r, _)| r.contains(&i));
        let state = Style { bold: s.bold, italic: s.italic, code: s.code, ..Style::default() };
        if state != open || link != open_link {
            close(&mut out, open, open_link);
            if state.bold {
                out.push_str("<strong>");
            }
            if state.italic {
                out.push_str("<em>");
            }
            if state.code {
                out.push_str(&format!("<code style=\"{HTML_CODE}\">"));
            }
            if let Some(ix) = link {
                out.push_str(&format!("<a href=\"{}\" style=\"{HTML_LINK}\">", escape(&links[ix].1)));
            }
            (open, open_link) = (state, link);
        }
        out.push_str(&escape(&ch.to_string()));
    }
    close(&mut out, open, open_link);
    out
}

/// Full HTML document for the text/html alternative.
pub fn to_html(text: &str) -> String {
    let mut body = String::new();
    for block in blocks(text) {
        match block {
            Block::Paragraph(lines) => {
                body.push_str(&format!("<p style=\"{HTML_P}\">{}</p>\n", lines.iter().map(|l| inline_html(l)).collect::<Vec<_>>().join("<br>\n")))
            }
            Block::Heading(line) => body.push_str(&format!("<h3 style=\"margin:0 0 10px;font-size:17px\">{}</h3>\n", inline_html(line))),
            Block::Code(lines) => body.push_str(&format!("<pre style=\"{HTML_PRE}\">{}</pre>\n", escape(&lines.join("\n")))),
            Block::Quote(lines) => body.push_str(&format!(
                "<blockquote style=\"{HTML_QUOTE}\">{}</blockquote>\n",
                lines.iter().map(|l| inline_html(l)).collect::<Vec<_>>().join("<br>\n")
            )),
            Block::List { ordered, items } => {
                let tag = if ordered { "ol" } else { "ul" };
                body.push_str(&format!("<{tag} style=\"{HTML_LIST}\">"));
                for item in items {
                    body.push_str(&format!("<li>{}</li>", inline_html(item)));
                }
                body.push_str(&format!("</{tag}>\n"));
            }
        }
    }
    format!("<!doctype html>\n<html><head><meta charset=\"utf-8\"></head><body style=\"{HTML_BODY}\">\n{body}</body></html>\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kinds(line: &str) -> String {
        let (styles, _) = inline(line);
        line.char_indices()
            .map(|(i, _)| match styles[i] {
                s if s.marker => '~',
                s if s.code => 'c',
                s if s.bold && s.italic => 'B',
                s if s.bold => 'b',
                s if s.italic => 'i',
                s if s.link => 'l',
                _ => '.',
            })
            .collect()
    }

    #[test]
    fn inline_markers() {
        assert_eq!(kinds("a **b** c"), "..~~b~~..");
        assert_eq!(kinds("x `y` z"), "..~c~..");
        assert_eq!(kinds("an _it_ snake_case"), "...~ii~...........");
        assert_eq!(kinds("[go](http://x)"), "~ll~~~~~~~~~~~");
        assert_eq!(kinds("2 * 3 = 6"), ".........");
    }

    #[test]
    fn html_part() {
        let html = to_html("Hi **team**, see `cfg` and [doc](https://x.dev?a=1&b=2)\n\n- one\n- two\n\n```\na < b\n```");
        assert!(html.contains("<strong>team</strong>"));
        assert!(html.contains(">cfg</code>"));
        assert!(html.contains("href=\"https://x.dev?a=1&amp;b=2\""));
        assert!(html.contains("<li>one</li><li>two</li>"));
        assert!(html.contains("a &lt; b"));
        assert!(!html.contains("**"));
    }
}

#[cfg(test)]
mod live_tests {
    use super::*;

    #[test]
    fn live_hides_closed_markers_unless_cursor_inside() {
        let theme = crate::theme::Theme::light();
        let text = "a **bold** b";
        let away = live(text, Some(0), "Geist", &theme);
        assert_eq!(away.display.as_ref(), "a bold b");
        let inside = live(text, Some(4), "Geist", &theme);
        assert_eq!(inside.display.as_ref(), text);
        // runs always cover the display text, maps round-trip at construct edges
        for lt in [&away, &inside] {
            assert_eq!(lt.runs.iter().map(|r| r.len).sum::<usize>(), lt.display.len());
            assert_eq!(lt.to_source.len(), lt.display.len() + 1);
        }
        assert_eq!(away.to_display[text.len()], away.display.len());
        assert_eq!(away.to_source[away.to_display[4]], 4);
    }

    #[test]
    fn live_lists_and_plain_preview() {
        let theme = crate::theme::Theme::light();
        let lt = live("- one\n- two", None, "Geist", &theme);
        assert_eq!(lt.display.as_ref(), "• one\n• two");
        assert_eq!(plain_text("Hi **there**\n\n- one\n- `two`"), "Hi there • one • two");
    }
}
