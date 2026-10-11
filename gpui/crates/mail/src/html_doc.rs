//! Mail HTML as the client shows it: a new document built from an allowlist of the sender's
//! markup, never the markup itself. Renderer-independent – the snapshot helper renders it
//! today, a live WebView later. Policy, not layout: WebKit still owns CSS.
//!
//! - elements and attributes from an allowlist; scripts, frames, forms, media, SVG/MathML,
//!   `<base>`, `<meta>`, `<link>` and event handlers never make it in
//! - a Content-Security-Policy ahead of everything the sender wrote (no script, no network
//!   except what `Policy` allows)
//! - images: `cid:` resolves only to this message's own parts (as handed in); remote images
//!   follow durian's `load_remote_images` setting – blocked ones become a quiet placeholder
//!   where the mail gave them a size, tracking pixels vanish
//! - links: only web and mail links keep their target (`ui::link`)
//! - a base stylesheet (system font, images that fit, wrapping) under the mail's own CSS
//!
//! Based on durian's GPUI experiment (PR #427, `document.rs`).

use std::collections::HashMap;

use html5ever::tendril::TendrilSink as _;
use markup5ever_rcdom::{Handle, NodeData, RcDom};

/// Largest HTML part shown as HTML (real mail tops out around 0.5 MB); bigger mail shows its
/// text part.
const MAX_HTML: usize = 2 << 20;
/// Nesting beyond this is cut (table soup is deep, but not this deep).
const MAX_DEPTH: usize = 256;

#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Policy {
    /// durian's `load_remote_images` (config.pkl [settings]); off by default
    pub remote_images: bool,
}

#[derive(Debug, PartialEq)]
pub enum Refused {
    TooLarge,
}

const DROP: &[&str] = &[
    "script", "noscript", "iframe", "frame", "frameset", "object", "embed", "applet", "form", "input", "button", "select", "option", "textarea",
    "video", "audio", "source", "track", "canvas", "link", "meta", "base", "template", "title", "dialog", "portal", "slot",
];

const KEEP: &[&str] = &[
    "body", "div", "span", "p", "br", "hr", "wbr", "a", "img", "picture", "table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "col",
    "colgroup", "ul", "ol", "li", "dl", "dt", "dd", "blockquote", "pre", "code", "b", "strong", "em", "i", "u", "s", "strike", "del", "ins",
    "small", "big", "sub", "sup", "mark", "abbr", "cite", "q", "h1", "h2", "h3", "h4", "h5", "h6", "section", "article", "header", "footer",
    "main", "nav", "aside", "center", "font", "figure", "figcaption", "details", "summary", "address", "time",
];

const ATTRIBUTES: &[&str] = &[
    "style", "class", "id", "title", "alt", "width", "height", "align", "valign", "colspan", "rowspan", "cellpadding", "cellspacing", "border",
    "bgcolor", "color", "face", "size", "dir", "lang", "start", "type", "open", "nowrap", "background-color", "role", "summary", "abbr", "scope",
];

const VOID: &[&str] = &["img", "br", "hr", "col", "wbr"];

const BASE_CSS: &str = "\
:root{color-scheme:light}\
html,body{margin:0;padding:0}\
body{padding:16px 20px;font:14px/1.5 -apple-system,BlinkMacSystemFont,'Helvetica Neue',sans-serif;color:#1f1f22;background:#fff;\
overflow-wrap:break-word;-webkit-text-size-adjust:100%}\
img{max-width:100%;height:auto}\
pre{white-space:pre-wrap}\
blockquote{margin:0 0 0 .6em;padding-left:.8em;border-left:2px solid #d8d8dc;color:#55555b}\
.durian-blocked{display:inline-block;box-sizing:border-box;max-width:100%;background:#f1f1f3;border-radius:4px;color:#8b8b92;\
font:11px/1.3 -apple-system,sans-serif;overflow:hidden;text-align:center;vertical-align:middle}";

fn csp(policy: Policy) -> String {
    let images = if policy.remote_images { "data: https:" } else { "data:" };
    format!(
        "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src {images}; font-src 'none'; connect-src 'none'; \
         frame-src 'none'; object-src 'none'; media-src 'none'; form-action 'none'; base-uri 'none'"
    )
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace('"', "&quot;")
}

/// The document to render. `images`: this message's embedded parts by content id (without
/// angle brackets) → a URL the client produced (`data:` today).
pub fn prepare(html: &str, images: &HashMap<String, String>, policy: Policy) -> Result<String, Refused> {
    if html.len() > MAX_HTML {
        return Err(Refused::TooLarge);
    }
    let dom = html5ever::parse_document(RcDom::default(), Default::default()).one(html);
    let mut body = String::with_capacity(html.len());
    walk(&dom.document, &mut body, images, policy, 0);
    // the policy precedes everything the sender wrote: it can't be loosened by the mail
    Ok(format!(
        "<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"{}\">\
         <meta name=\"referrer\" content=\"no-referrer\"><style>{BASE_CSS}</style></head>{body}</html>",
        csp(policy)
    ))
}

fn attr<'a>(attrs: &'a [html5ever::Attribute], name: &str) -> Option<&'a str> {
    attrs.iter().find(|a| a.name.ns.as_ref().is_empty() && a.name.local.as_ref() == name).map(|a| a.value.as_ref())
}

/// A remote image that isn't loaded: a box of its declared size with its alt text, or – with
/// no size – just the alt text. 1×1 (and other tiny) images are tracking pixels: nothing.
fn blocked_image(attrs: &[html5ever::Attribute]) -> String {
    let dimension = |name: &str| attr(attrs, name).and_then(|v| v.trim().trim_end_matches("px").parse::<u32>().ok());
    let alt = attr(attrs, "alt").map(str::trim).unwrap_or("");
    match (dimension("width"), dimension("height")) {
        (Some(w), Some(h)) if w * h <= 16 => String::new(),
        (Some(w), Some(h)) => format!("<span class=\"durian-blocked\" style=\"width:{w}px;height:{h}px;line-height:{h}px\">{}</span>", escape(alt)),
        _ if !alt.is_empty() => format!("<span class=\"durian-blocked\" style=\"padding:2px 6px\">{}</span>", escape(alt)),
        _ => String::new(),
    }
}

fn walk(node: &Handle, out: &mut String, images: &HashMap<String, String>, policy: Policy, depth: usize) {
    if depth > MAX_DEPTH {
        return;
    }
    let children = |out: &mut String| {
        for child in node.children.borrow().iter() {
            walk(child, out, images, policy, depth + 1);
        }
    };
    match &node.data {
        NodeData::Text { contents } => out.push_str(&escape(&contents.borrow())),
        NodeData::Element { name, attrs, .. } => {
            // foreign content (SVG, MathML) never gets promoted into HTML
            if name.ns.as_ref() != "http://www.w3.org/1999/xhtml" {
                return;
            }
            let tag = name.local.as_ref();
            if DROP.contains(&tag) {
                return;
            }
            let attrs = attrs.borrow();
            if tag == "style" {
                // the raw-text parser already ended it at any closing tag; CSS stays verbatim
                out.push_str("<style>");
                for child in node.children.borrow().iter() {
                    if let NodeData::Text { contents } = &child.data {
                        out.push_str(&contents.borrow().replace("</", "<\\/"));
                    }
                }
                out.push_str("</style>");
                return;
            }
            if tag == "img" {
                let src = attr(&attrs, "src").map(str::trim).unwrap_or("");
                let resolved = match src.get(..4).map(str::to_ascii_lowercase).as_deref() {
                    Some("cid:") => images.get(src[4..].trim_matches(['<', '>'])).cloned(),
                    _ if policy.remote_images && src.to_ascii_lowercase().starts_with("https://") => Some(src.to_string()),
                    _ => None,
                };
                match resolved {
                    Some(url) => {
                        out.push_str(&format!("<img src=\"{}\"", escape(&url)));
                        push_attributes(out, &attrs, tag);
                        out.push('>');
                    }
                    None if src.to_ascii_lowercase().starts_with("http") => out.push_str(&blocked_image(&attrs)),
                    // cid: without a part, data:, file:, … – nothing
                    None => {}
                }
                return;
            }
            let keep = KEEP.contains(&tag);
            if keep {
                out.push('<');
                out.push_str(tag);
                push_attributes(out, &attrs, tag);
                out.push('>');
            }
            children(out);
            if keep && !VOID.contains(&tag) {
                out.push_str(&format!("</{tag}>"));
            }
        }
        _ => children(out),
    }
}

fn push_attributes(out: &mut String, attrs: &[html5ever::Attribute], tag: &str) {
    for a in attrs.iter().filter(|a| a.name.ns.as_ref().is_empty()) {
        let (key, value) = (a.name.local.as_ref(), a.value.as_ref());
        let keep = match key {
            "href" if tag == "a" => value.starts_with('#') || ui::link::allowed(value),
            "src" => false,
            _ => ATTRIBUTES.contains(&key),
        };
        if keep {
            out.push_str(&format!(" {key}=\"{}\"", escape(value)));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn doc(html: &str) -> String {
        prepare(html, &HashMap::new(), Policy::default()).unwrap()
    }

    #[test]
    fn keeps_mail_css_and_geometry_but_no_active_content() {
        let out = doc(r##"<head><title>T</title><base href="https://evil.invalid"><meta http-equiv="refresh" content="0;url=https://evil.invalid"><style>.mail { display:grid; gap:13px }</style><link rel="dns-prefetch" href="//evil.invalid"></head><body bgcolor="#fafafa" onload="steal()"><table><tr><td colspan="2" style="padding:17px">Hello &amp; bye</td></tr></table><script>steal()</script><svg><script>bad()</script></svg><iframe srcdoc="bad"></iframe><form action="https://evil.invalid"><input></form><img srcset="https://evil.invalid/x 2x" src="https://evil.invalid/tracking" width="1" height="1"><a href="javascript:bad()">Bad</a><a href="https://example.com">Good</a><!--[if mso]><v:rect/><![endif]--></body>"##);
        for wanted in ["display:grid; gap:13px", "colspan=\"2\"", "padding:17px", "Hello &amp; bye", "href=\"https://example.com\"", "bgcolor=\"#fafafa\""] {
            assert!(out.contains(wanted), "missing {wanted}");
        }
        for forbidden in ["<script", "<svg", "<iframe", "<form", "<input", "onload", "srcset", "javascript:", "evil.invalid", "<base", "<link", "<title", "v:rect", "refresh"] {
            assert!(!out.contains(forbidden), "retained {forbidden}");
        }
        // the policy comes before anything the sender wrote
        assert!(out.find("Content-Security-Policy").unwrap() < out.find("display:grid").unwrap());
        assert!(out.contains("img-src data:;"), "no remote images by default");
    }

    #[test]
    fn images_resolve_only_from_this_message() {
        let images = HashMap::from([("logo@x".to_string(), "data:image/png;base64,AAAA".to_string())]);
        let out = prepare(r#"<img src="cid:logo@x" alt="Logo"><img src="cid:missing"><img src="file:///etc/passwd"><img src="data:image/svg+xml,bad">"#, &images, Policy::default()).unwrap();
        assert_eq!(out.matches("<img").count(), 1);
        assert!(out.contains(r#"<img src="data:image/png;base64,AAAA" alt="Logo">"#));
        assert!(!out.contains("file:") && !out.contains("svg+xml"));
    }

    #[test]
    fn remote_images_follow_the_setting() {
        let html = r#"<img src="https://cdn.x.org/hero.png" width="600" height="200" alt="Autumn sale"><img src="https://t.x.org/p.gif" width="1" height="1"><img src="http://x.org/a.png" alt="Logo">"#;
        let blocked = doc(html);
        assert!(!blocked.contains("cdn.x.org") && !blocked.contains("t.x.org"));
        assert!(blocked.contains(r#"style="width:600px;height:200px;line-height:200px">Autumn sale</span>"#), "{blocked}");
        assert_eq!(blocked.matches("durian-blocked\"").count(), 2, "the pixel leaves nothing behind");
        // the thread view tells "remote images blocked" by this mark, so it must be there only then
        assert!(blocked.contains(r#"class="durian-blocked""#));
        assert!(!doc("<p>no images</p>").contains(r#"class="durian-blocked""#));
        let allowed = prepare(html, &HashMap::new(), Policy { remote_images: true }).unwrap();
        assert!(allowed.contains(r#"<img src="https://cdn.x.org/hero.png""#));
        assert!(!allowed.contains("http://x.org"), "plain http stays blocked");
        assert!(allowed.contains("img-src data: https:;"));
    }

    #[test]
    fn style_text_cant_close_its_element() {
        let out = doc("<style>p{color:red}</style><p>x</p>");
        assert!(out.contains("<style>p{color:red}</style>"));
        assert_eq!(prepare(&"x".repeat(MAX_HTML + 1), &HashMap::new(), Policy::default()), Err(Refused::TooLarge));
    }
}
