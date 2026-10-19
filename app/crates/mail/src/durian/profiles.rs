//! Profiles from durian's config (`profiles.pkl`): named groups of accounts, each with its own
//! folders (name, icon, durian query). The account switcher shows profiles; a profile scopes
//! every search to its accounts, its folders are the sidebar's.
//!
//! durian doesn't serve profiles yet, so – like its macOS app – the client evaluates the file
//! with the `pkl` CLI against the bundled schema (`schema/Profiles.pkl`, a copy of durian's).
//! Evaluating takes 1–2 s, so the result is cached as JSON, keyed by the file and schema
//! contents: a launch with an unchanged file reads ~1 ms. Profiles are configuration, not
//! mail – fine on disk. Once durian has `GET /profiles`, that replaces all of this.

use std::path::{Path, PathBuf};
use std::process::Command;

use serde::Deserialize;

const SCHEMA: &str = include_str!("schema/Profiles.pkl");

pub use crate::model::{Folder, Profile, default_profile as default_index};

/// The search scope for a profile's accounts, as durian's macOS app builds it; None for all
/// accounts.
pub fn scope(profile: &Profile) -> Option<String> {
    if profile.accounts.is_empty() || profile.accounts.iter().any(|a| a == "*") {
        return None;
    }
    let paths: Vec<String> = profile.accounts.iter().map(|a| format!("path:{}/**", super::translate::quoted(a))).collect();
    Some(if paths.len() == 1 { paths[0].clone() } else { format!("({})", paths.join(" OR ")) })
}

#[derive(Deserialize)]
struct File {
    #[serde(default)]
    profiles: Vec<RawProfile>,
}

#[derive(Deserialize)]
struct RawProfile {
    name: String,
    #[serde(default)]
    accounts: Vec<String>,
    #[serde(default)]
    default: bool,
    #[serde(default)]
    color: Option<String>,
    #[serde(default)]
    folders: Vec<RawFolder>,
}

#[derive(Deserialize)]
struct RawFolder {
    name: String,
    #[serde(default)]
    icon: Option<String>,
    #[serde(default)]
    query: Option<String>,
}

/// `pkl eval -f json` output → profiles. Folders without a query aren't navigable: skipped.
pub fn parse(json: &str) -> Result<Vec<Profile>, String> {
    let file: File = serde_json::from_str(json).map_err(|e| format!("profiles: {e}"))?;
    Ok(file
        .profiles
        .into_iter()
        .map(|p| Profile {
            name: p.name.into(),
            accounts: p.accounts.into_iter().map(Into::into).collect(),
            default: p.default,
            color: p.color.as_deref().and_then(ui::theme::parse_css_color),
            folders: p
                .folders
                .into_iter()
                .filter_map(|f| Some(Folder { query: f.query.filter(|q| !q.trim().is_empty())?.into(), icon: f.icon.unwrap_or_default().into(), name: f.name.into() }))
                .collect(),
        })
        .collect())
}

/// Profiles as the server serves them (`GET /profiles`): no pkl, no schema here.
pub fn from_api(response: &super::api::ProfilesResponse) -> Vec<Profile> {
    response
        .profiles
        .iter()
        .map(|p| Profile {
            name: p.name.clone().into(),
            // scope() reads "*" as all accounts
            accounts: if p.all_accounts { vec!["*".into()] } else { p.accounts.iter().map(|a| a.name.clone().into()).collect() },
            default: p.default,
            color: p.color.as_deref().and_then(ui::theme::parse_css_color),
            folders: p
                .folders
                .iter()
                .filter(|f| !f.query.trim().is_empty())
                .map(|f| Folder { name: f.name.clone().into(), icon: f.icon.clone().into(), query: f.query.clone().into() })
                .collect(),
        })
        .collect()
}

/// durian's config folder (`$XDG_CONFIG_HOME/durian`, else `~/.config/durian`).
pub fn config_dir() -> Option<PathBuf> {
    let base = std::env::var_os("XDG_CONFIG_HOME").filter(|v| !v.is_empty()).map(PathBuf::from).or_else(|| std::env::var_os("HOME").map(|h| PathBuf::from(h).join(".config")))?;
    Some(base.join("durian"))
}

fn cache_key(file: &[u8]) -> String {
    use std::hash::{Hash, Hasher};
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    (file, SCHEMA).hash(&mut hasher);
    format!("{:016x}", hasher.finish())
}

/// The cached evaluation, if `profiles.pkl` is unchanged since it was made.
pub fn cached(config: &Path, cache: &Path) -> Option<Vec<Profile>> {
    let file = std::fs::read(config.join("profiles.pkl")).ok()?;
    let json = std::fs::read_to_string(cache.join(format!("profiles-{}.json", cache_key(&file)))).ok()?;
    parse(&json).ok()
}

/// Evaluate `profiles.pkl` with `pkl` (slow: 1–2 s) and cache the result. None: no file.
pub fn evaluate(config: &Path, cache: &Path) -> Result<Option<Vec<Profile>>, String> {
    let path = config.join("profiles.pkl");
    let Ok(file) = std::fs::read(&path) else { return Ok(None) };
    let schema_dir = cache.join("pkl-schema");
    std::fs::create_dir_all(&schema_dir).and_then(|_| std::fs::write(schema_dir.join("Profiles.pkl"), SCHEMA)).map_err(|e| format!("profiles: {e}"))?;
    let output = Command::new("pkl")
        .args(["eval", "-f", "json", "--module-path"])
        .arg(&schema_dir)
        .arg(&path)
        .output()
        .map_err(|e| format!("profiles: pkl not available ({e})"))?;
    if !output.status.success() {
        // pkl's message names the file and line, not mail content
        return Err(format!("profiles.pkl: {}", String::from_utf8_lossy(&output.stderr).lines().take(3).collect::<Vec<_>>().join(" ")));
    }
    let json = String::from_utf8_lossy(&output.stdout);
    let profiles = parse(&json)?;
    // stale evaluations of earlier versions of the file go
    for entry in std::fs::read_dir(cache).into_iter().flatten().flatten() {
        if entry.file_name().to_string_lossy().starts_with("profiles-") {
            let _ = std::fs::remove_file(entry.path());
        }
    }
    let _ = std::fs::write(cache.join(format!("profiles-{}.json", cache_key(&file))), json.as_bytes());
    Ok(Some(profiles))
}

/// Finite compatibility aliases for configured folder icons, grouped by bundled target.
/// These are semantic approximations, not SF Symbols rendering. Fill/decorated variants
/// must be listed explicitly: never infer meaning from a substring or strip suffixes.
/// Includes the old plain nicknames (`inbox`, `unread`, `send`, `draft`, `people`).
/// Extend this table for aliases; canonical SVG stems come only from `IconName`'s catalog.
pub const ICON_ALIASES: &[(&[&str], ui::IconName)] = &[
    (&["tray", "tray.fill", "tray.full", "tray.full.fill", "tray.and.arrow.down", "tray.and.arrow.down.fill", "inbox"], ui::IconName::MailInbox),
    (&["envelope.badge", "envelope.badge.fill", "unread"], ui::IconName::MailUnreadOnly),
    (&["paperplane", "paperplane.fill", "send"], ui::IconName::MailSend),
    (&["doc", "doc.fill", "doc.text", "doc.text.fill", "doc.plaintext", "doc.plaintext.fill", "doc.richtext", "doc.richtext.fill", "square.and.pencil", "pencil", "pencil.line", "draft"], ui::IconName::MailDraft),
    (&["trash", "trash.fill", "trash.circle", "trash.circle.fill", "xmark.bin", "xmark.bin.fill"], ui::IconName::MailTrash),
    (&["archivebox", "archivebox.fill", "checkmark", "checkmark.circle", "checkmark.circle.fill", "checkmark.square", "checkmark.square.fill", "checklist"], ui::IconName::MailDone),
    // The available list glyph is Lucide list-filter; tasks use circle-check above.
    (&["list.bullet", "list.bullet.circle", "list.bullet.circle.fill", "list.bullet.rectangle", "list.bullet.rectangle.fill", "list.number", "line.3.horizontal.decrease", "line.3.horizontal.decrease.circle", "line.3.horizontal.decrease.circle.fill"], ui::IconName::Filter),
    // Terminal is the available code glyph; don't mistake chevrons for mail navigation.
    (&["terminal", "terminal.fill", "chevron.left.forwardslash.chevron.right", "curlybraces", "curlybraces.square", "curlybraces.square.fill"], ui::IconName::MailTerminal),
    (&["star", "star.fill", "star.circle", "star.circle.fill", "flag", "flag.fill", "flag.circle", "flag.circle.fill"], ui::IconName::MailStarred),
    (&["clock", "clock.fill", "clock.circle", "clock.circle.fill"], ui::IconName::MailClock),
    (&["alarm", "alarm.fill", "zzz"], ui::IconName::MailSnooze),
    (&["person", "person.fill", "person.circle", "person.circle.fill", "person.badge.plus", "person.fill.badge.plus", "person.badge.minus", "person.fill.badge.minus", "person.2", "person.2.fill", "person.3", "person.3.fill", "person.crop.circle", "person.crop.circle.fill", "person.crop.circle.badge.plus", "person.crop.circle.fill.badge.plus", "people"], ui::IconName::MailUsers),
    (&["calendar", "calendar.circle", "calendar.circle.fill", "calendar.badge.clock", "calendar.badge.plus", "calendar.badge.minus", "calendar.badge.exclamationmark"], ui::IconName::Calendar),
    (&["envelope", "envelope.fill", "envelope.open", "envelope.open.fill", "envelope.circle", "envelope.circle.fill"], ui::IconName::MailEmail),
    (&["tag", "tag.fill", "tag.circle", "tag.circle.fill"], ui::IconName::MailLabel),
    (&["folder", "folder.fill", "folder.circle", "folder.circle.fill"], ui::IconName::Folder),
    (&["paperclip", "paperclip.circle", "paperclip.circle.fill"], ui::IconName::MailAttach),
];

/// Resolve a configured icon: canonical SVG stem first, then an exact compatibility alias.
/// Names are trimmed and ASCII case-insensitive. Unknown/empty names get a
/// neutral folder, not a guessed mail action. In particular, Legal's `building.columns`
/// and `scalemass` have no bundled equivalent and deliberately use this fallback.
pub fn ui_icon(symbol: &str) -> ui::IconName {
    let name = symbol.trim().to_ascii_lowercase();
    ui::IconName::from_name(&name)
        .or_else(|| ICON_ALIASES.iter().find_map(|(aliases, icon)| aliases.contains(&name.as_str()).then_some(*icon)))
        .unwrap_or(ui::IconName::Folder)
}

#[cfg(test)]
mod tests {
    use super::*;

    const JSON: &str = r##"{"profiles":[
        {"name":"All","accounts":["*"],"default":false,"folders":[{"name":"Inbox","icon":"tray","query":"tag:inbox AND NOT tag:sent"}]},
        {"name":"Work","accounts":["work","consulting"],"default":true,"color":"#3b82f6","folders":[
            {"name":"Inbox","icon":"tray","query":"tag:inbox"},{"name":"Section"},{"name":"Sent","icon":"paperplane","query":"tag:sent"}]},
        {"name":"Uni","accounts":["uni mail"],"folders":[]}]}"##;

    #[test]
    fn profiles_scope_their_accounts() {
        let profiles = parse(JSON).unwrap();
        assert_eq!(scope(&profiles[0]), None, "* is everything");
        assert_eq!(scope(&profiles[1]).as_deref(), Some("(path:work/** OR path:consulting/**)"));
        assert_eq!(scope(&profiles[2]).as_deref(), Some("path:\"uni mail\"/**"));
        assert_eq!(default_index(&profiles), Some(1));
        assert!(profiles[1].color.is_some());
    }

    #[test]
    fn folders_need_a_query() {
        let profiles = parse(JSON).unwrap();
        assert_eq!(profiles[1].folders.iter().map(|f| f.name.as_ref()).collect::<Vec<_>>(), ["Inbox", "Sent"]);
        assert_eq!(profiles[1].home().unwrap().query.as_ref(), "tag:inbox");
        assert_eq!(ui_icon("paperplane.fill"), ui::IconName::MailSend);
    }

    #[test]
    fn configured_icons_support_standard_symbols_and_variants() {
        use ui::IconName as I;
        for (name, expected) in [
            ("tray", I::MailInbox), ("tray.fill", I::MailInbox),
            ("envelope.badge", I::MailUnreadOnly), ("envelope.badge.fill", I::MailUnreadOnly),
            ("paperplane", I::MailSend), ("paperplane.fill", I::MailSend),
            ("doc.text", I::MailDraft), ("doc.text.fill", I::MailDraft),
            ("xmark.bin", I::MailTrash), ("xmark.bin.fill", I::MailTrash),
            ("trash", I::MailTrash), ("trash.fill", I::MailTrash),
            ("archivebox", I::MailDone), ("archivebox.fill", I::MailDone),
            ("checklist", I::MailDone), ("checkmark.circle.fill", I::MailDone),
            ("list.bullet", I::Filter), ("list.bullet.rectangle.fill", I::Filter),
            ("terminal.fill", I::MailTerminal), ("chevron.left.forwardslash.chevron.right", I::MailTerminal),
            ("person.badge.plus", I::MailUsers), ("person.fill.badge.plus", I::MailUsers),
            ("person.2.fill", I::MailUsers), ("person.crop.circle.fill", I::MailUsers),
            ("calendar", I::Calendar), ("calendar.circle.fill", I::Calendar), ("calendar.badge.clock", I::Calendar),
            ("envelope.fill", I::MailEmail), ("envelope.open.fill", I::MailEmail),
            ("star", I::MailStarred), ("star.fill", I::MailStarred),
            ("clock", I::MailClock), ("clock.fill", I::MailClock), ("alarm.fill", I::MailSnooze),
            ("paperclip", I::MailAttach), ("tag.fill", I::MailLabel),
        ] {
            assert_eq!(ui_icon(name), expected, "{name}");
        }
        assert_eq!(ui_icon("  PERSON.BADGE.PLUS  "), I::MailUsers);
    }

    #[test]
    fn canonical_icons_and_aliases_are_unambiguous() {
        for &icon in ui::IconName::ALL {
            assert_eq!(ui_icon(icon.file()), icon, "canonical {}", icon.file());
        }
        let mut seen = std::collections::HashSet::new();
        for &(aliases, expected) in ICON_ALIASES {
            for &alias in aliases {
                assert!(seen.insert(alias), "duplicate alias: {alias}");
                assert_eq!(alias, alias.trim().to_ascii_lowercase(), "normalized alias: {alias}");
                assert_eq!(ui_icon(alias), expected, "alias: {alias}");
            }
        }
        // Canonical names distinguish assets that SF compatibility deliberately collapses.
        assert_eq!(ui_icon("mail_star"), ui::IconName::MailStar);
        assert_eq!(ui_icon("mail_starred"), ui::IconName::MailStarred);
    }

    #[test]
    fn unknown_icons_do_not_guess_from_substrings_or_suffixes() {
        for name in [
            "", "   ", "unknown", "starfish", "restart", "doctor", "badge", "xmark",
            "person.badge.unsupported", "doc.unrecognized.fill", "star.fill.fill", "mail_inbox.fill",
            "building.columns", "building.columns.fill", "scalemass", "scalemass.fill",
            "mail_inbox.svg", "icons/lucide/mail_inbox.svg",
        ] {
            assert_eq!(ui_icon(name), ui::IconName::Folder, "{name}");
        }
    }

    #[test]
    fn api_profiles_scope_by_account_name() {
        let json = r##"{"ok":true,"profiles":[
            {"name":"All","all_accounts":true,"accounts":[{"name":"Work","email":"a@w"},{"name":"Home","email":"a@h"}],"folders":[{"name":"Inbox","icon":"tray","query":"tag:inbox"}]},
            {"name":"Work","default":true,"color":"#3b82f6","accounts":[{"name":"Work","email":"a@w"},{"name":"old"}],"folders":null}],
          "accounts":[{"name":"Work","email":"a@w","default":true},{"name":"Home","email":"a@h"}]}"##;
        let response: super::super::api::ProfilesResponse = serde_json::from_str(json).unwrap();
        let profiles = from_api(&response);
        assert_eq!(scope(&profiles[0]), None, "all accounts: no scope");
        assert_eq!(scope(&profiles[1]).as_deref(), Some("(path:Work/** OR path:old/**)"));
        assert_eq!(default_index(&profiles), Some(1));
        assert!(profiles[1].folders.is_empty() && profiles[1].color.is_some());
    }

    #[test]
    fn cache_follows_the_file() {
        let dir = std::env::temp_dir().join(format!("profiles-test-{}", std::process::id()));
        let (config, cache) = (dir.join("config"), dir.join("cache"));
        std::fs::create_dir_all(&config).unwrap();
        std::fs::create_dir_all(&cache).unwrap();
        std::fs::write(config.join("profiles.pkl"), "v1").unwrap();
        assert!(cached(&config, &cache).is_none());
        std::fs::write(cache.join(format!("profiles-{}.json", cache_key(b"v1"))), JSON).unwrap();
        assert_eq!(cached(&config, &cache).unwrap().len(), 3);
        std::fs::write(config.join("profiles.pkl"), "v2").unwrap();
        assert!(cached(&config, &cache).is_none(), "a changed file isn't served from the cache");
        let _ = std::fs::remove_dir_all(dir);
    }
}
