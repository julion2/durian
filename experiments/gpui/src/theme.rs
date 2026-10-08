//! Minimal semantic color palette, roughly matching the macOS/Qt Durian look.

use gpui::{Hsla, rgb};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Mode {
    Light,
    Dark,
}

#[derive(Clone, Copy, Debug)]
pub struct Theme {
    pub mode: Mode,
    pub window_bg: Hsla,
    pub sidebar_bg: Hsla,
    pub list_bg: Hsla,
    pub detail_bg: Hsla,
    pub card_bg: Hsla,
    pub border: Hsla,
    pub text: Hsla,
    pub text_muted: Hsla,
    pub text_faint: Hsla,
    pub accent: Hsla,
    pub accent_fg: Hsla,
    pub selection: Hsla,
    pub hover: Hsla,
    pub unread_dot: Hsla,
    pub tag_bg: Hsla,
    pub tag_fg: Hsla,
}

impl Theme {
    pub fn light() -> Self {
        Self {
            mode: Mode::Light,
            window_bg: rgb(0xfafafa).into(),
            sidebar_bg: rgb(0xf3f3f5).into(),
            list_bg: rgb(0xffffff).into(),
            detail_bg: rgb(0xfafafa).into(),
            card_bg: rgb(0xffffff).into(),
            border: rgb(0xe3e3e6).into(),
            text: rgb(0x1c1c1e).into(),
            text_muted: rgb(0x6e6e73).into(),
            text_faint: rgb(0xa1a1a6).into(),
            accent: rgb(0x0a84ff).into(),
            accent_fg: rgb(0xffffff).into(),
            selection: rgb(0xe5f0ff).into(),
            hover: rgb(0xf2f2f4).into(),
            unread_dot: rgb(0x0a84ff).into(),
            tag_bg: rgb(0xeceef1).into(),
            tag_fg: rgb(0x4a4a4f).into(),
        }
    }

    pub fn dark() -> Self {
        Self {
            mode: Mode::Dark,
            window_bg: rgb(0x1e1e20).into(),
            sidebar_bg: rgb(0x232326).into(),
            list_bg: rgb(0x1b1b1d).into(),
            detail_bg: rgb(0x1e1e20).into(),
            card_bg: rgb(0x27272a).into(),
            border: rgb(0x333336).into(),
            text: rgb(0xf2f2f2).into(),
            text_muted: rgb(0xa0a0a6).into(),
            text_faint: rgb(0x6b6b70).into(),
            accent: rgb(0x409cff).into(),
            accent_fg: rgb(0xffffff).into(),
            selection: rgb(0x2b3a52).into(),
            hover: rgb(0x2a2a2d).into(),
            unread_dot: rgb(0x409cff).into(),
            tag_bg: rgb(0x343438).into(),
            tag_fg: rgb(0xc8c8cc).into(),
        }
    }

    pub fn toggled(&self) -> Self {
        match self.mode {
            Mode::Light => Self::dark(),
            Mode::Dark => Self::light(),
        }
    }
}

/// Deterministic avatar color from a name, similar to `AvatarHelper.js`.
pub fn avatar_color(name: &str) -> Hsla {
    const PALETTE: [u32; 8] = [
        0x5b8def, 0xe0656d, 0x4cb782, 0xf0a04b, 0x9b6bd6, 0x2ca9bc, 0xd6699b, 0x7c8a9c,
    ];
    let hash = name.bytes().fold(0u32, |h, b| h.wrapping_mul(31).wrapping_add(b as u32));
    rgb(PALETTE[(hash % PALETTE.len() as u32) as usize]).into()
}
