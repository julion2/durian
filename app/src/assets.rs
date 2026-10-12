use std::borrow::Cow;

use anyhow::Context as _;
use gpui::{App, AssetSource, SharedString};
use rust_embed::RustEmbed;

/// The app's own assets (fixture avatars, images, pre-rendered mail HTML, the Linear
/// recreation's icons and its Inter) in front of the ui crate's (Geist, Lucide icons).
#[derive(RustEmbed)]
#[folder = "assets"]
#[include = "icons/*.svg"]
#[include = "fonts/**/*.ttf"]
#[include = "images/**/*"]
#[include = "avatars/**/*"]
#[include = "mail-html/**/*"]
pub struct Assets;

impl AssetSource for Assets {
    fn load(&self, path: &str) -> anyhow::Result<Option<Cow<'static, [u8]>>> {
        match Self::get(path) {
            Some(file) => Ok(Some(file.data)),
            None => ui::Assets.load(path),
        }
    }

    fn list(&self, path: &str) -> anyhow::Result<Vec<SharedString>> {
        let mut paths: Vec<SharedString> = Self::iter().filter(|p| p.starts_with(path)).map(SharedString::from).collect();
        paths.extend(ui::Assets.list(path)?);
        Ok(paths)
    }
}

impl Assets {
    /// Inter, for the Linear recreation (the mail UI uses the ui crate's Geist).
    pub fn load_fonts(cx: &App) -> anyhow::Result<()> {
        let fonts = Self::iter()
            .filter(|p| p.starts_with("fonts/"))
            .map(|p| Self::get(&p).map(|f| f.data).with_context(|| format!("missing {p}")))
            .collect::<anyhow::Result<Vec<_>>>()?;
        cx.text_system().add_fonts(fonts)
    }
}
