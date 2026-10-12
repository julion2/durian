//! The one way a link from mail content leaves the app. Mail is hostile input: a link may
//! point at `file:///…`, at another app's URL scheme (`x-apple.systempreferences:`, `tel:`,
//! custom handlers) or at nothing useful (`javascript:`, `about:blank#…`). Only web pages and
//! new mail open; everything else is refused.

use gpui::App;

/// Schemes a click in mail may open.
const ALLOWED: &[&str] = &["https", "http", "mailto"];

/// Whether `url` may be opened from mail content.
pub fn allowed(url: &str) -> bool {
    let url = url.trim();
    let Some((scheme, rest)) = url.split_once(':') else { return false };
    let scheme = scheme.to_ascii_lowercase();
    // a scheme is letters, digits, + - . ; anything else before ':' isn't one
    let valid = scheme.chars().next().is_some_and(|c| c.is_ascii_alphabetic()) && scheme.chars().all(|c| c.is_ascii_alphanumeric() || "+-.".contains(c));
    let has_target = match scheme.as_str() {
        "http" | "https" => rest.starts_with("//") && rest.len() > 2,
        _ => !rest.is_empty(),
    };
    valid && ALLOWED.contains(&scheme.as_str()) && has_target && !url.chars().any(char::is_control)
}

/// Open a link from mail content, if it's one that may be opened. Returns whether it was.
pub fn open(url: &str, cx: &mut App) -> bool {
    let ok = allowed(url);
    if ok {
        cx.open_url(url.trim());
    }
    ok
}

#[cfg(test)]
mod tests {
    use super::allowed;

    #[test]
    fn only_web_and_mail_links_open() {
        for ok in ["https://durian.dev/x?a=1#f", "http://example.org", "mailto:ada@x.org", "HTTPS://X.ORG"] {
            assert!(allowed(ok), "{ok}");
        }
        for refused in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "x-apple.systempreferences:com.apple.preference.security",
            "tel:+1555",
            "about:blank#top",
            "cid:logo@x",
            "data:text/html,hi",
            "https:",
            "https://",
            "//evil.org/protocol-relative",
            "#fragment",
            "https://x.org/\nfile:///x",
        ] {
            assert!(!allowed(refused), "{refused:?}");
        }
    }
}
