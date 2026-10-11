use std::borrow::Cow;

use anyhow::Context as _;
use gpui::{App, AssetSource, SharedString};
use rust_embed::RustEmbed;

/// What the components need at runtime: the text faces (OFL) and the Lucide icons (ISC). Paths are
/// `fonts/…` and `icons/lucide/…`; the app chains this behind its own asset source.
#[derive(RustEmbed)]
#[folder = "assets"]
#[include = "fonts/**/*.ttf"]
#[include = "icons/lucide/*.svg"]
pub struct Assets;

impl AssetSource for Assets {
    fn load(&self, path: &str) -> anyhow::Result<Option<Cow<'static, [u8]>>> {
        Ok(Self::get(path).map(|f| f.data))
    }

    fn list(&self, path: &str) -> anyhow::Result<Vec<SharedString>> {
        Ok(Self::iter().filter(|p| p.starts_with(path)).map(SharedString::from).collect())
    }
}

impl Assets {
    pub(crate) fn load_fonts(cx: &App) -> anyhow::Result<()> {
        let fonts = Self::iter()
            .filter(|p| p.starts_with("fonts/"))
            .map(|p| Self::get(&p).map(|f| f.data).with_context(|| format!("missing {p}")))
            .collect::<anyhow::Result<Vec<_>>>()?;
        cx.text_system().add_fonts(fonts)
    }
}
