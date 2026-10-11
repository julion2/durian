//! Design system for the mail client, on GPUI: the theme (light/dark tokens, type ramp) and the
//! primitives every view is built from – buttons, chips, nav items, pickers, menus, dialogs, text
//! fields and the live Markdown editor. Dimensions are logical pixels.
//!
//! Call [`init`] once at startup; it loads the bundled fonts and binds the keys the components
//! handle themselves. The app serves [`Assets`] (fonts + Lucide icons) next to its own.

mod assets;
mod avatar;
mod button;
mod chip;
mod context_menu;
mod dialog;
mod empty_state;
mod icon;
mod kbd;
pub mod link;
pub mod markdown;
pub mod plain;
mod nav;
mod picker;
mod selectable;
mod text_area;
mod text_input;
pub mod theme;

pub use assets::Assets;
pub use avatar::*;
pub use button::*;
pub use chip::*;
pub use context_menu::*;
pub use dialog::*;
pub use empty_state::*;
pub use icon::*;
pub use kbd::*;
pub use nav::*;
pub use picker::*;
pub use selectable::*;
pub use text_area::*;
pub use text_input::*;

use gpui::{AbsoluteLength, Animation, App, Styled, ease_out_quint, px};

/// Fonts + key bindings for the components. Call before opening a window.
pub fn init(cx: &mut App) -> anyhow::Result<()> {
    Assets::load_fonts(cx)?;
    bind_text_input_keys(cx);
    bind_text_area_keys(cx);
    bind_picker_keys(cx);
    bind_dialog_keys(cx);
    bind_context_menu_keys(cx);
    Ok(())
}

/// Entrance motion shared by overlays (menus, pickers, dialogs, sheets, toasts): short and
/// decelerating, so things land rather than slide. Exits are instant – a dismissed overlay
/// should get out of the way.
pub fn entrance(ms: u64) -> Animation {
    Animation::new(std::time::Duration::from_millis(ms)).with_easing(ease_out_quint())
}

/// 0.5px hairlines are used everywhere; GPUI's tailwind helpers stop at 1px.
pub trait StyledExt: Styled + Sized {
    fn border_hairline(mut self) -> Self {
        let w: AbsoluteLength = px(0.5).into();
        let b = &mut self.style().border_widths;
        (b.top, b.right, b.bottom, b.left) = (Some(w), Some(w), Some(w), Some(w));
        self
    }

    fn border_b_hairline(mut self) -> Self {
        self.style().border_widths.bottom = Some(px(0.5).into());
        self
    }

    fn border_t_hairline(mut self) -> Self {
        self.style().border_widths.top = Some(px(0.5).into());
        self
    }

    fn border_l_hairline(mut self) -> Self {
        self.style().border_widths.left = Some(px(0.5).into());
        self
    }
}

impl<T: Styled> StyledExt for T {}
