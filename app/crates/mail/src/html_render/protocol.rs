//! The mailrender wire contract, independent of GPUI and the platform rendering engine.
//! One JSON object per line; the helper announces readiness before accepting jobs.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

// The existing helper's strip and page limits. Other engines implementing this protocol
// must return the same bounded artifact layout, not arbitrary paths or GPU dimensions.
const MAX_HEIGHT: f32 = 16000.;
const STRIP_HEIGHT: f32 = 4000.;
const MAX_STRIPS: usize = 4;
pub(super) const MAX_LINKS: usize = 16384;
/// Replies/cache metadata contain geometry and paths, not HTML or image bytes.
pub(super) const MAX_REPLY_BYTES: usize = 8 << 20;

#[derive(Serialize)]
pub(super) struct Request<'a> {
    pub id: &'a str,
    /// A document prepared by html_doc, never raw sender markup.
    pub html: &'a str,
    pub width: f32,
    pub out: &'a Path,
}

/// One horizontal slice of a rendered page (GPU textures have a maximum height).
#[derive(Clone, Serialize, Deserialize)]
pub struct Strip {
    pub path: PathBuf,
    pub y: f32,
    pub height: f32,
}

#[derive(Deserialize)]
pub(super) struct Link {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
    pub href: String,
}

#[derive(Deserialize)]
pub(super) struct Reply {
    pub id: Option<String>,
    pub ok: Option<bool>,
    pub ready: Option<bool>,
    pub height: Option<f32>,
    pub width: Option<f32>,
    #[serde(default)]
    pub clipped: bool,
    #[serde(default)]
    pub strips: Vec<Strip>,
    #[serde(default)]
    pub links: Vec<Link>,
    pub ms: Option<f64>,
    pub error: Option<String>,
}

/// Validate both helper replies and cached metadata against the requested output. The
/// engine writes indexed JPEGs beside `out`; it cannot name other files for the UI to open.
pub(super) fn valid_snapshot(width: f32, expected_width: f32, height: f32, strips: &[Strip], out: &Path) -> bool {
    if !width.is_finite()
        || width != expected_width
        || !height.is_finite()
        || !(1.0..=MAX_HEIGHT).contains(&height)
        || strips.is_empty()
        || strips.len() > MAX_STRIPS
    {
        return false;
    }
    let mut bottom = 0.;
    for (index, strip) in strips.iter().enumerate() {
        if strip.path != out.with_extension(format!("{index}.jpg"))
            || !strip.y.is_finite()
            || !strip.height.is_finite()
            || (strip.y - bottom).abs() > 0.5
            || !(1.0..=STRIP_HEIGHT).contains(&strip.height)
        {
            return false;
        }
        bottom = strip.y + strip.height;
    }
    (bottom - height).abs() <= 0.5
}

pub(super) fn valid_link(x: f32, y: f32, w: f32, h: f32) -> bool {
    [x, y, w, h].into_iter().all(|v| v.is_finite() && v.abs() <= 1_000_000.) && w >= 0. && h >= 0.
}
