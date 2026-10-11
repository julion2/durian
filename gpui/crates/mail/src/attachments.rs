//! Attachment actions on real files: the native Quick Look panel (`qlmanage -p`), copying into
//! ~/Downloads and revealing in Finder. Where the files come from is the data source's business:
//! `Attachment::path` when it's on disk, else a [`Fetch`] the source installed brings it first.

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::rc::Rc;

use gpui::{App, Global};

use crate::model::Attachment;

/// What to do with an attachment once it's on disk.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Use {
    QuickLook,
    /// copy into ~/Downloads (and say where it went)
    Download,
}

/// Brings an attachment that isn't on disk yet (it's on a server), then uses it. Installed by
/// a live data source; without one such attachments do nothing.
#[derive(Clone)]
pub struct Fetch(pub Rc<dyn Fn(Attachment, Use, &mut App)>);

impl Global for Fetch {}

fn fetch(attachment: &Attachment, use_: Use, cx: &mut App) {
    if attachment.remote.is_some()
        && let Some(Fetch(fetch)) = cx.try_global::<Fetch>().cloned()
    {
        fetch(attachment.clone(), use_, cx);
    }
}

/// Space / click: the system Quick Look panel – PDFs, CSV, Markdown, images all for free.
pub fn quick_look(attachment: &Attachment, cx: &mut App) {
    match &attachment.path {
        Some(path) => quick_look_path(path),
        None => fetch(attachment, Use::QuickLook, cx),
    }
}

/// Quick Look any file – also .eml, which renders the HTML part like a mail client would.
pub fn quick_look_path(path: &Path) {
    let _ = Command::new("qlmanage").arg("-p").arg(path).stdout(Stdio::null()).stderr(Stdio::null()).spawn();
}

/// Copies into ~/Downloads and returns the new path; one that's still on the server is fetched
/// first, and its source reports where it went (None here).
pub fn download(attachment: &Attachment, cx: &mut App) -> Option<PathBuf> {
    match &attachment.path {
        Some(src) => copy_to_downloads(src, &attachment.name),
        None => {
            fetch(attachment, Use::Download, cx);
            None
        }
    }
}

/// A file name from a mail header, safe to create: no folders, no hidden or empty names, no
/// control characters.
pub fn safe_name(name: &str) -> String {
    let base = name.rsplit(['/', '\\']).next().unwrap_or(name);
    let clean: String = base.chars().filter(|c| !c.is_control() && *c != ':').collect();
    let clean = clean.trim().trim_start_matches('.').trim();
    if clean.is_empty() { "attachment".into() } else { clean.to_string() }
}

/// Into ~/Downloads without overwriting ("name (1).pdf" …); returns the new path.
pub fn copy_to_downloads(src: &Path, name: &str) -> Option<PathBuf> {
    let name = safe_name(name);
    let name = name.as_str();
    let downloads = std::env::var_os("HOME").map(PathBuf::from)?.join("Downloads");
    let (stem, ext) = match name.rsplit_once('.') {
        Some((stem, ext)) => (stem.to_string(), format!(".{ext}")),
        None => (name.to_string(), String::new()),
    };
    let target = (0..)
        .map(|n| downloads.join(if n == 0 { format!("{stem}{ext}") } else { format!("{stem} ({n}){ext}") }))
        .find(|p| !p.exists())?;
    std::fs::copy(src, &target).ok()?;
    Some(target)
}

pub fn reveal_in_finder(path: &Path) {
    let _ = Command::new("open").arg("-R").arg(path).spawn();
}

#[cfg(test)]
mod tests {
    use super::safe_name;

    #[test]
    fn header_names_cant_escape_or_hide() {
        assert_eq!(safe_name("../../.ssh/authorized_keys"), "authorized_keys");
        assert_eq!(safe_name("..\\evil.exe"), "evil.exe");
        assert_eq!(safe_name(".hidden"), "hidden");
        assert_eq!(safe_name("a\nb:c.pdf"), "abc.pdf");
        assert_eq!(safe_name("  "), "attachment");
        assert_eq!(safe_name("Rechnung 2026.pdf"), "Rechnung 2026.pdf");
    }
}
