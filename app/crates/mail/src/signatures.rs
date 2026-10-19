//! Configured defaults are applied only to new drafts or an explicit account/default
//! change. The encrypted draft owns its snapshot; refreshing Pkl never rewrites it.

use gpui::SharedString;
use html5ever::tendril::TendrilSink as _;
use markup5ever_rcdom::{Handle, NodeData, RcDom};
use serde::{Deserialize, Serialize};

use crate::durian::api::{ComposeSettings, DraftContent};

const METADATA: &str = "gpuiSignature";

#[derive(Clone, Default)]
pub enum Settings {
    #[default]
    Loading,
    Ready(ComposeSettings),
    Unavailable(SharedString),
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "mode", rename_all = "snake_case")]
pub enum Choice {
    Automatic { account: String, name: Option<String>, pending: bool },
    Named { name: String },
    None,
    Saved,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Signature {
    pub choice: Choice,
    pub html: Option<String>,
}

impl Signature {
    pub fn new(account: &str, settings: &Settings) -> Self {
        let mut signature = Self { choice: Choice::Automatic { account: account.to_string(), name: None, pending: true }, html: None };
        signature.resolve(settings);
        signature
    }

    pub fn from_draft(draft: &DraftContent) -> Self {
        Self {
            // Foreign/older drafts, including an empty signature, are snapshots too.
            choice: draft.extra.get(METADATA).and_then(|value| serde_json::from_value(value.clone()).ok()).unwrap_or(Choice::Saved),
            html: draft.html_signature.clone(),
        }
    }

    pub fn apply(&self, draft: &mut DraftContent) {
        draft.html_signature = self.html.clone();
        draft.extra.insert(METADATA.into(), serde_json::to_value(&self.choice).unwrap());
    }

    pub fn retain_recovered_snapshot(draft: &mut DraftContent) {
        let mut signature = Self::from_draft(draft);
        if let Choice::Automatic { pending, .. } = &mut signature.choice {
            *pending = false;
            signature.apply(draft);
        }
    }

    /// Resolves an initial asynchronous lookup once, never a later Pkl edit.
    pub fn resolve(&mut self, settings: &Settings) -> bool {
        let Settings::Ready(settings) = settings else { return false };
        let Choice::Automatic { account, pending: true, .. } = &self.choice else { return false };
        let account = account.clone();
        let name = settings.account_defaults.get(&account.to_lowercase()).cloned();
        self.html = name.as_ref().and_then(|name| settings.signatures.get(name)).filter(|text| !text.trim().is_empty()).map(|text| as_html(text));
        self.choice = Choice::Automatic { account, name, pending: false };
        true
    }

    pub fn change_account(&mut self, account: &str, settings: &Settings) {
        if matches!(&self.choice, Choice::Automatic { account: previous, .. } if !previous.eq_ignore_ascii_case(account)) {
            *self = Self::new(account, settings);
        }
    }

    pub fn choose_named(&mut self, name: &str, settings: &Settings) -> bool {
        let Settings::Ready(settings) = settings else { return false };
        let Some(text) = settings.signatures.get(name) else { return false };
        self.choice = Choice::Named { name: name.to_string() };
        self.html = (!text.trim().is_empty()).then(|| as_html(text));
        true
    }

    pub fn label(&self) -> String {
        match &self.choice {
            Choice::Automatic { name: Some(name), .. } => name.clone(),
            Choice::Automatic { pending: true, .. } => "Signature".into(),
            Choice::Automatic { .. } => "None".into(),
            Choice::None => "None".into(),
            Choice::Named { name } => name.clone(),
            Choice::Saved => if self.html.is_some() { "Signature" } else { "None" }.into(),
        }
    }
}

/// Same content convention as Swift: recognizable HTML tags mean HTML; otherwise
/// escape text, including address brackets. No regex replacement of the message body.
fn as_html(text: &str) -> String {
    fn has_markup(node: &Handle, depth: usize) -> bool {
        if depth > 256 { return false }
        if let NodeData::Element { name, .. } = &node.data
            && matches!(name.local.as_ref(), "b" | "i" | "u" | "em" | "strong" | "br" | "div" | "span" | "p" | "a" | "img" | "table" | "tr" | "td" | "h1" | "h2" | "h3" | "h4" | "h5" | "h6" | "ul" | "ol" | "li" | "hr" | "font" | "style")
        { return true }
        node.children.borrow().iter().any(|child| has_markup(child, depth + 1))
    }
    let dom = html5ever::parse_document(RcDom::default(), Default::default()).one(text);
    if has_markup(&dom.document, 0) { text.to_string() }
    else { text.replace('&', "&amp;").replace('<', "&lt;").replace('>', "&gt;").replace("\r\n", "\n").replace('\n', "<br>") }
}

/// Fallback when the HTML helper is unavailable. Read text only from prepared markup,
/// not scripts/styles or image URLs, and bound recursion like the sanitizer does.
pub fn preview_text(prepared: &str) -> String {
    fn text(node: &Handle, out: &mut String, depth: usize) {
        if depth > 256 { return }
        match &node.data {
            NodeData::Text { contents } => out.push_str(&contents.borrow()),
            NodeData::Element { name, .. } => {
                if matches!(name.local.as_ref(), "head" | "style" | "script") { return }
                if matches!(name.local.as_ref(), "br" | "p" | "div" | "tr" | "li") && !out.ends_with('\n') { out.push('\n'); }
                for child in node.children.borrow().iter() { text(child, out, depth + 1); }
            }
            _ => for child in node.children.borrow().iter() { text(child, out, depth + 1); },
        }
    }
    let dom = html5ever::parse_document(RcDom::default(), Default::default()).one(prepared);
    let mut out = String::new();
    text(&dom.document, &mut out, 0);
    out.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn settings() -> Settings {
        Settings::Ready(ComposeSettings {
            signatures: [("Office".into(), "<b>Work</b>".into()), ("Personal".into(), "A & B\r\n<me@example.com>".into())].into(),
            account_defaults: [("work".into(), "Office".into()), ("home".into(), "Personal".into())].into(),
        })
    }

    #[test]
    fn defaults_follow_accounts_but_explicit_choices_and_aliases_do_not() {
        let settings = settings();
        let mut signature = Signature::new("Work", &settings);
        assert_eq!(signature.html.as_deref(), Some("<b>Work</b>"));
        let changed = Settings::Ready(ComposeSettings { signatures: [("Office".into(), "Changed".into())].into(), account_defaults: [("work".into(), "Office".into())].into() });
        signature.change_account("work", &changed); // another alias of the same account
        assert_eq!(signature.html.as_deref(), Some("<b>Work</b>"));
        signature.change_account("home", &settings);
        assert_eq!(signature.html.as_deref(), Some("A &amp; B<br>&lt;me@example.com&gt;"));
        assert!(signature.choose_named("Office", &settings));
        signature.change_account("work", &changed);
        assert_eq!(signature.html.as_deref(), Some("<b>Work</b>"));
        assert!(!signature.choose_named("Missing", &settings));
        signature = Signature { choice: Choice::None, html: None };
        signature.change_account("work", &settings);
        assert_eq!(signature.choice, Choice::None);
        assert_eq!(signature.html, None);
    }

    #[test]
    fn refresh_resolves_only_new_pending_defaults_not_recovered_snapshots() {
        let mut pending = Signature::new("work", &Settings::Loading);
        let mut wire = DraftContent::default();
        pending.apply(&mut wire);
        Signature::retain_recovered_snapshot(&mut wire);
        let mut recovered = Signature::from_draft(&wire);
        assert!(!recovered.resolve(&settings()));
        assert_eq!(recovered.html, None);
        assert!(pending.resolve(&settings()));
        pending.apply(&mut wire);
        let encoded = serde_json::to_string(&wire).unwrap();
        let mut wire = serde_json::from_str(&encoded).unwrap();
        Signature::retain_recovered_snapshot(&mut wire);
        let mut recovered = Signature::from_draft(&wire);
        assert!(!recovered.resolve(&Settings::Ready(Default::default())));
        assert_eq!(recovered.html.as_deref(), Some("<b>Work</b>"));
        let mut foreign = Signature::from_draft(&DraftContent { html_signature: Some("<i>Legacy</i>".into()), ..Default::default() });
        foreign.change_account("home", &settings());
        assert_eq!(foreign.html.as_deref(), Some("<i>Legacy</i>"));
    }

    #[test]
    fn signature_stays_between_body_and_quote_without_duplicates_or_identity_changes() {
        let mut draft = DraftContent { id: "stable".into(), from: "me@example.com".into(), to: vec!["you@example.com".into()], body: "Typed & kept".into(), quoted_content: Some("Original message".into()), ..Default::default() };
        let mut signature = Signature::new("home", &settings());
        signature.apply(&mut draft);
        signature.apply(&mut draft);
        let send = crate::durian::writes::outbox(&draft).unwrap();
        assert_eq!(send.body, "Typed &amp; kept<div style=\"margin-top:12px\">A &amp; B<br>&lt;me@example.com&gt;</div><br><br>Original message");
        assert_eq!(draft.body, "Typed & kept");
        assert!(send.is_html);
        assert!(signature.choose_named("Office", &settings()));
        signature.apply(&mut draft);
        let changed = crate::durian::writes::outbox(&draft).unwrap();
        assert_eq!(changed.idempotency_key, send.idempotency_key);
        assert_eq!(changed.body, "Typed &amp; kept<div style=\"margin-top:12px\"><b>Work</b></div><br><br>Original message");
        let html_body = ui::markdown::to_html(&draft.body);
        draft.html_body = Some(html_body.clone());
        let rich = crate::durian::writes::outbox(&draft).unwrap();
        assert_eq!(rich.body, format!("{html_body}<div style=\"margin-top:12px\"><b>Work</b></div><br><br>Original message"));
        for empty in [None, Some(String::new()), Some(" \n ".into())] {
            draft.html_signature = empty;
            draft.html_body = None;
            let plain = crate::durian::writes::outbox(&draft).unwrap();
            assert!(!plain.is_html, "an absent/empty signature adds neither HTML nor spacing");
            assert_eq!(plain.body, "Typed & kept\n\nOriginal message");
        }
        let mut unavailable = Signature::new("work", &Settings::Unavailable("Old server".into()));
        assert_eq!(unavailable.html, None);
        assert!(unavailable.resolve(&settings()));
        assert_eq!(unavailable.html.as_deref(), Some("<b>Work</b>"));
    }
}
