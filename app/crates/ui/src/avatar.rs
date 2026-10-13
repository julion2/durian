use gpui::{App, ImageSource, IntoElement, ObjectFit, RenderOnce, SharedString, Window, div, img, prelude::*, px};

use crate::theme::{ActiveTheme, lch, typography::*};

/// Round avatar: an image if we have one, otherwise initials on a hashed hue.
#[derive(IntoElement)]
pub struct Avatar {
    name: SharedString,
    image: Option<ImageSource>,
    logo: bool,
    size: f32,
}

impl Avatar {
    pub fn new(name: impl Into<SharedString>) -> Self {
        Self { name: name.into(), image: None, logo: false, size: 18. }
    }

    /// A photo filling the circle (an asset path, or an image in memory).
    pub fn image(mut self, path: impl Into<ImageSource>) -> Self {
        self.image = Some(path.into());
        self
    }

    /// A company mark filling the same circle, with a light backing for transparency.
    pub fn logo(mut self, path: impl Into<ImageSource>) -> Self {
        self.image = Some(path.into());
        self.logo = true;
        self
    }

    pub fn size(mut self, size: f32) -> Self {
        self.size = size;
        self
    }
}

pub fn initials(name: &str) -> String {
    name.split_whitespace().filter_map(|w| w.chars().next()).take(2).collect::<String>().to_uppercase()
}

fn hue_for(name: &str) -> f32 {
    // FNV-1a, stable across runs so a person keeps their color
    let hash = name.bytes().fold(0x811c_9dc5_u32, |h, b| (h ^ b as u32).wrapping_mul(0x0100_0193));
    (hash % 360) as f32
}

impl RenderOnce for Avatar {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let size = px(self.size);
        match self.image {
            Some(image) if self.logo => {
                let theme = cx.theme();
                return div()
                    .flex()
                    .flex_none()
                    .items_center()
                    .justify_center()
                    .size(size)
                    .rounded_full()
                    .bg(theme.bg_logo)
                    .shadow(theme.raised_shadow(false))
                    .child(img(image).size(size).object_fit(ObjectFit::Cover).rounded_full())
                    .into_any_element();
            }
            Some(image) => return img(image).size(size).flex_none().object_fit(ObjectFit::Cover).rounded_full().into_any_element(),
            None => {}
        }
        div()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(size)
            .rounded_full()
            .bg(lch(62., 38., hue_for(&self.name)))
            .text_color(gpui::white())
            .text_size(px((self.size * 0.42).max(7.)))
            .font_weight(WEIGHT_SEMIBOLD)
            .child(initials(&self.name))
            .into_any_element()
    }
}
