//! Mail policy, not an HTML layout engine. WebKit owns CSS and rendering.
use base64::Engine as _;
use html5ever::tendril::TendrilSink as _;
use markup5ever_rcdom::{Handle, NodeData, RcDom};
use serde::Deserialize;
use std::collections::HashMap;

pub const CSP: &str = "default-src 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src durianmail:; font-src 'none'; connect-src 'none'; frame-src 'none'; object-src 'none'; media-src 'none'; form-action 'none'; base-uri 'none'";

#[derive(Deserialize)]
pub struct Mail {
    pub html: String,
    pub dark: bool,
    /// PNGs already decoded/bounded by the GPUI attachment pipeline. Keys are
    /// same-message Content-IDs; no filesystem or network URLs cross this IPC.
    #[serde(default)]
    pub images: HashMap<String, String>,
}

pub struct Document {
    pub html: String,
    pub images: HashMap<String, Vec<u8>>,
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

pub fn prepare(mail: &Mail) -> Result<Document, String> {
    if mail.html.len() > 1024 * 1024 {
        return Err("HTML exceeds the 1 MiB preview limit".into());
    }
    if mail.images.len() > 8 {
        return Err("Too many inline images".into());
    }
    let mut images = HashMap::new();
    let mut sources = HashMap::new();
    for (id, encoded) in &mail.images {
        let png = base64::engine::general_purpose::STANDARD
            .decode(encoded)
            .map_err(|_| "Invalid inline image")?;
        if png.len() > 20 * 1024 * 1024 || !png.starts_with(b"\x89PNG\r\n\x1a\n") {
            return Err("Inline image is not a bounded PNG".into());
        }
        let url = format!("durianmail://image/{}", images.len());
        sources.insert(id.as_str(), url.clone());
        images.insert(url, png);
    }
    let dom =
        html5ever::parse_document(RcDom::default(), Default::default()).one(mail.html.as_str());
    let mut body = String::new();
    walk(&dom.document, &mut body, &sources, 0);
    // This policy precedes ALL sender-controlled content. The sender cannot
    // replace it with a more permissive policy. No document script is allowed;
    // only Wry's separately injected host script implements theme/shortcuts.
    let html = format!(
        "<!doctype html><html><head><meta http-equiv=\"Content-Security-Policy\" content=\"{CSP}\"><meta name=\"referrer\" content=\"no-referrer\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><style>body{{margin:1.5rem;font:15px/1.6 -apple-system,Inter,sans-serif;color:#1e1e20;background:#fff}}img{{max-width:100%;height:auto}}:focus-visible{{outline:2px solid #3675d9;outline-offset:3px}}</style></head>{body}</html>"
    );
    Ok(Document { html, images })
}

fn walk(node: &Handle, out: &mut String, sources: &HashMap<&str, String>, depth: usize) {
    if depth > 100 {
        return;
    }
    match &node.data {
        NodeData::Text { contents } => out.push_str(&escape(&contents.borrow())),
        NodeData::Element { name, attrs, .. } => {
            // Drop active content and foreign namespaces completely. Never
            // promote script contents, SVG or MathML into the HTML namespace.
            if name.ns.as_ref() != "http://www.w3.org/1999/xhtml" {
                return;
            }
            let tag = name.local.as_ref();
            if [
                "script", "noscript", "iframe", "frame", "frameset", "object", "embed", "form",
                "input", "button", "select", "textarea", "video", "audio", "source", "track",
                "link", "meta", "base", "template",
            ]
            .contains(&tag)
            {
                return;
            }
            if tag == "style" {
                out.push_str("<style>");
                for child in node.children.borrow().iter() {
                    if let NodeData::Text { contents } = &child.data {
                        // HTML's raw-text parser has already ended this node
                        // at any closing style tag; preserve CSS verbatim.
                        out.push_str(&contents.borrow());
                    }
                }
                out.push_str("</style>");
                return;
            }
            let keep = [
                "body",
                "div",
                "span",
                "p",
                "br",
                "hr",
                "wbr",
                "a",
                "img",
                "picture",
                "table",
                "thead",
                "tbody",
                "tfoot",
                "tr",
                "th",
                "td",
                "caption",
                "col",
                "colgroup",
                "ul",
                "ol",
                "li",
                "dl",
                "dt",
                "dd",
                "blockquote",
                "pre",
                "code",
                "b",
                "strong",
                "em",
                "i",
                "u",
                "s",
                "del",
                "small",
                "sub",
                "sup",
                "h1",
                "h2",
                "h3",
                "h4",
                "h5",
                "h6",
                "section",
                "article",
                "header",
                "footer",
                "main",
                "center",
                "font",
                "figure",
                "figcaption",
                "details",
                "summary",
            ]
            .contains(&tag);
            if keep {
                out.push('<');
                out.push_str(tag);
                for attr in attrs.borrow().iter() {
                    if !attr.name.ns.as_ref().is_empty() {
                        continue;
                    }
                    let key = attr.name.local.as_ref();
                    let value = attr.value.as_ref();
                    if key == "src" && tag == "img" {
                        if let Some(url) = value
                            .strip_prefix("cid:")
                            .and_then(|id| sources.get(id.trim_matches(['<', '>'])))
                        {
                            out.push_str(&format!(" src=\"{}\"", escape(url)));
                        }
                    } else if key == "href" && tag == "a" {
                        if value.starts_with('#') || external_link(value) {
                            out.push_str(&format!(" href=\"{}\"", escape(value)));
                        }
                    } else if [
                        "style",
                        "class",
                        "id",
                        "title",
                        "alt",
                        "width",
                        "height",
                        "align",
                        "valign",
                        "colspan",
                        "rowspan",
                        "cellpadding",
                        "cellspacing",
                        "border",
                        "bgcolor",
                        "color",
                        "face",
                        "size",
                        "dir",
                        "lang",
                        "start",
                        "type",
                        "open",
                    ]
                    .contains(&key)
                    {
                        out.push_str(&format!(" {key}=\"{}\"", escape(value)));
                    }
                }
                out.push('>');
            }
            for child in node.children.borrow().iter() {
                walk(child, out, sources, depth + 1);
            }
            if keep && !["img", "br", "hr", "col", "wbr"].contains(&tag) {
                out.push_str(&format!("</{tag}>"));
            }
        }
        _ => {
            for child in node.children.borrow().iter() {
                walk(child, out, sources, depth + 1);
            }
        }
    }
}

pub fn external_link(url: &str) -> bool {
    !url.chars().any(char::is_control)
        && ["https://", "http://", "mailto:"]
            .iter()
            .any(|scheme| url.starts_with(scheme))
}

/// Reuse the actual Swift client's transform, rather than inventing a second
/// dark-mail algorithm. The Swift multiline string only escapes backslashes.
pub fn dark_script() -> String {
    include_str!("../../../macos/durian/Utilities/DarkModeTransform.swift")
        .split("\"\"\"")
        .nth(1)
        .expect("Swift dark-mode JavaScript literal")
        .replace("\\\\", "\\")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn preserves_email_css_and_geometry_but_not_active_content() {
        let document = prepare(&Mail {
            html: r##"<head><base href="https://evil.invalid"><meta http-equiv="refresh" content="0;url=https://evil.invalid"><style>.mail { display:grid; gap:13px }</style><link rel="dns-prefetch" href="//evil.invalid"></head><body bgcolor="#fafafa" onload="steal()"><table><tr><td colspan="2" style="padding:17px">Hello &amp; bye</td></tr></table><script>steal()</script><svg><script>bad()</script></svg><iframe srcdoc="bad"></iframe><form action="https://evil.invalid"><input></form><img srcset="https://evil.invalid/x 2x" src="https://evil.invalid/tracking"><a href="javascript:bad()">Bad</a><a href="https://example.com">Good</a></body>"##.into(),
            dark: false,
            images: HashMap::new(),
        }).unwrap();
        for wanted in [
            "display:grid; gap:13px",
            "colspan=\"2\"",
            "padding:17px",
            "Hello &amp; bye",
            "https://example.com",
            CSP,
        ] {
            assert!(document.html.contains(wanted), "missing {wanted}");
        }
        for forbidden in [
            "<script",
            "<svg",
            "<iframe",
            "<form",
            "<input",
            "onload",
            "srcset",
            "javascript:",
            "evil.invalid",
            "<base",
            "<link",
        ] {
            assert!(!document.html.contains(forbidden), "retained {forbidden}");
        }
        assert!(document.html.find(CSP).unwrap() < document.html.find("display:grid").unwrap());
    }

    #[test]
    fn cid_can_only_resolve_from_this_messages_map() {
        let png = include_bytes!("../../gpui-kit/fixtures/review.png");
        let document = prepare(&Mail {
            html: "<img src='cid:good'><img src='cid:missing'><img src='file:///etc/passwd'><img src='data:image/svg+xml,bad'>".into(),
            dark: true,
            images: HashMap::from([("good".into(), base64::engine::general_purpose::STANDARD.encode(png))]),
        }).unwrap();
        assert_eq!(document.images.len(), 1);
        assert_eq!(document.images["durianmail://image/0"], png);
        assert_eq!(document.html.matches("src=").count(), 1);
        assert!(!document.html.contains("file:"));
        assert!(!document.html.contains("data:"));
    }

    #[test]
    fn dark_transform_keeps_real_javascript_regex() {
        let script = dark_script();
        assert!(script.contains(r"/rgba?\((\d+),\s*(\d+),\s*(\d+)/"));
        assert!(script.contains("MIN_CONTRAST = 4.5"));
        assert!(!script.contains(r"\\d"));
    }
}
