//! Design tokens. The palette is defined in CIE LCH (perceptual lightness makes light/dark pairs
//! and hover steps easy to reason about) and converted to sRGB once at startup.

use gpui::{App, BoxShadow, Global, Hsla, Pixels, Rgba, point, px};

/// CSS `lch(L% C H / A)` → sRGB. CSS Lab/LCH use the D50 white point, so we go
/// LCH → Lab → XYZ(D50) → Bradford → XYZ(D65) → linear sRGB → gamma-encoded sRGB.
/// Hue of every neutral (backgrounds, text, borders): a warm stone. Lightness steps and chroma
/// are per token; changing this one value re-tints the whole greyscale.
pub const NEUTRAL_HUE: f32 = 70.;

pub fn lch(l: f32, c: f32, h: f32) -> Hsla {
    lcha(l, c, h, 1.0)
}

pub fn lcha(l: f32, c: f32, h: f32, alpha: f32) -> Hsla {
    let (l, c, h) = (l as f64, c as f64, (h as f64).to_radians());
    let (a, b) = (c * h.cos(), c * h.sin());

    let fy = (l + 16.0) / 116.0;
    let fx = fy + a / 500.0;
    let fz = fy - b / 200.0;
    const EPS: f64 = 216.0 / 24389.0;
    const KAPPA: f64 = 24389.0 / 27.0;
    let inv = |f: f64| if f.powi(3) > EPS { f.powi(3) } else { (116.0 * f - 16.0) / KAPPA };
    let yr = if l > KAPPA * EPS { fy.powi(3) } else { l / KAPPA };
    let (x, y, z) = (inv(fx) * 0.96422, yr, inv(fz) * 0.82521);

    // Bradford-adapted D50 → D65
    let x65 = 0.955_576_6 * x - 0.023_039_3 * y + 0.063_163_6 * z;
    let y65 = -0.028_289_5 * x + 1.009_941_6 * y + 0.021_007_7 * z;
    let z65 = 0.012_298_2 * x - 0.020_483 * y + 1.329_909_8 * z;

    let lin = [
        3.240_969_9 * x65 - 1.537_383_2 * y65 - 0.498_610_8 * z65,
        -0.969_243_6 * x65 + 1.875_967_5 * y65 + 0.041_555_1 * z65,
        0.055_630_1 * x65 - 0.203_977 * y65 + 1.056_971_5 * z65,
    ];
    let enc = |v: f64| {
        let v = v.clamp(0.0, 1.0);
        (if v <= 0.003_130_8 { 12.92 * v } else { 1.055 * v.powf(1.0 / 2.4) - 0.055 }) as f32
    };
    Rgba { r: enc(lin[0]), g: enc(lin[1]), b: enc(lin[2]), a: alpha }.into()
}

/// Parses the color strings that come straight out of the extraction (`lch(…)`, `rgb(…)`, `#hex`).
pub fn parse_css_color(s: &str) -> Option<Hsla> {
    let s = s.trim();
    let args = |prefix: &str| -> Option<Vec<f32>> {
        let inner = s.strip_prefix(prefix)?.strip_suffix(')')?;
        Some(
            inner
                .split([' ', ',', '/'])
                .filter(|p| !p.is_empty())
                .filter_map(|p| p.trim_end_matches('%').parse().ok())
                .collect(),
        )
    };
    if let Some(v) = args("lch(") {
        return Some(lcha(v[0], v[1], v[2], v.get(3).copied().unwrap_or(1.0)));
    }
    if let Some(v) = args("rgb(") {
        let a = v.get(3).copied().unwrap_or(1.0);
        return Some(Rgba { r: v[0] / 255.0, g: v[1] / 255.0, b: v[2] / 255.0, a }.into());
    }
    let hex = s.strip_prefix('#')?;
    let n = u32::from_str_radix(hex, 16).ok()?;
    Some(gpui::rgb(n).into())
}

#[derive(Clone)]
pub struct Theme {
    pub font_family: &'static str,
    /// code spans, fences, query syntax
    pub font_mono: &'static str,
    pub radius: Radii,
    pub dark: bool,

    // surfaces
    pub bg_app: Hsla,         // --bg-sidebar-color: window + sidebar
    pub bg_panel: Hsla,       // --color-bg-primary: the rounded content card
    pub bg_raised: Hsla,      // pill buttons / inactive tabs
    pub bg_raised_hover: Hsla,
    pub bg_group: Hsla,       // group header strip + row hover
    pub bg_chip_hover: Hsla,
    pub bg_tab_active: Hsla,
    pub bg_nav_hover: Hsla,
    pub bg_nav_active: Hsla,
    pub bg_selected: Hsla,    // keyboard-selected list row (inbox)
    pub bg_card: Hsla,        // comment cards, composer, command menu

    // text
    pub text_primary: Hsla,   // issue titles
    pub text_nav_active: Hsla,
    pub text_secondary: Hsla, // page title, toolbar icons
    pub text_heading: Hsla,   // group titles, workspace name
    pub text_muted: Hsla,     // nav items, counts, section headers
    pub text_tertiary: Hsla,  // identifiers, chip labels, dates
    pub text_faint: Hsla,     // trailing nav affordances
    pub text_quaternary: Hsla, // --color-text-quaternary: counts, de-emphasized meta
    pub icon_caret: Hsla,     // group collapse caret
    pub icon_view: Hsla,      // favorite view icons
    pub text_body: Hsla,      // long-form text in cards (15px)
    pub text_meta: Hsla,      // card meta line, kbd glyphs
    pub text_placeholder: Hsla,
    pub accent: Hsla,         // unread dot, focus

    // lines
    pub border_panel: Hsla,
    pub border_chip: Hsla,
    pub border_card: Hsla,
    pub ring: Hsla,
    pub ring_hover: Hsla,

    // surfaces that used to be hard-coded
    pub bg_option: Hsla,      // active row in pickers / suggestion lists
    pub bg_code: Hsla,
    pub text_code: Hsla,
    pub backdrop: Hsla,       // dims the app behind modal dialogs
    pub bg_logo: Hsla,        // favicon discs stay light: logos are drawn for light backgrounds
    pub primary: Hsla,        // filled primary button
    pub primary_hover: Hsla,
    pub on_primary: Hsla,
    /// drop shadow strength for raised controls / panels / overlays
    pub shadow: Hsla,
    // context menus
    pub bg_menu: Hsla,
    pub border_menu: Hsla,
    pub separator_menu: Hsla,
}

impl Theme {
    pub fn light() -> Self {
        // warm neutrals: a touch more chroma than a pure grey so the stone hue shows; pure
        // white surfaces (cards, menus) stay white
        let n = |l: f32, c: f32| if l >= 99.9 { lch(l, 0., NEUTRAL_HUE) } else { lch(l, c + 0.8, NEUTRAL_HUE) };
        Self {
            // static faces: GPUI doesn't apply the weight axis of variable fonts (measured: every
            // weight of a variable face rendered as 400)
            font_family: "Geist",
            font_mono: "Geist Mono",
            radius: Radii::DEFAULT,
            dark: false,

            bg_app: n(94.44, 0.5),
            bg_panel: n(97.94, 0.5),
            bg_raised: n(99.997, 0.5),
            bg_raised_hover: n(94.854, 0.5),
            bg_group: n(94.44, 0.5),
            bg_chip_hover: n(92.44, 0.5),
            bg_tab_active: n(93.483, 0.5),
            bg_nav_hover: n(91.64, 0.5),
            bg_nav_active: n(89.54, 0.5),
            bg_selected: n(92.94, 0.5),
            bg_card: n(100., 0.),

            text_primary: n(9.794, 0.),
            text_nav_active: n(9.444, 0.),
            text_secondary: n(19.588, 1.25),
            text_heading: n(18.888, 1.25),
            text_muted: n(37.776, 1.25),
            text_tertiary: n(39.176, 1.25),
            text_faint: n(62.33, 1.25),
            text_quaternary: n(64.64, 1.25),
            icon_caret: n(80.94, 0.),
            icon_view: lch(67.969, 3.577, 260.65),
            text_body: n(20., 1.),
            text_meta: n(40., 1.),
            text_placeholder: n(66., 1.),
            accent: lch(48., 59.31, 288.43),

            border_panel: n(89.84, 0.),
            border_chip: n(84.44, 0.),
            border_card: n(91.9, 0.),
            ring: lcha(0., 0., 0., 0.09),
            ring_hover: lcha(0., 0., 0., 0.15),

            bg_option: n(95., 0.),
            bg_code: n(95., 0.),
            text_code: lch(30., 20., 20.),
            backdrop: lcha(0., 0., 0., 0.16),
            bg_logo: n(100., 0.),
            primary: lch(53., 52.26, 286.91),
            primary_hover: lch(49., 52.26, 286.91),
            on_primary: lch(100., 5., 286.91),
            shadow: lcha(0., 0., 0., 0.04),
            bg_menu: n(100., 0.),
            border_menu: n(88.49, 0.),
            separator_menu: n(91.9, 0.),
        }
    }

    /// Dark theme: one warm neutral hue throughout, lightness steps by role.
    pub fn dark() -> Self {
        let n = |l: f32, c: f32| lch(l, c, NEUTRAL_HUE);
        Self {
            font_family: "Geist",
            font_mono: "Geist Mono",
            radius: Radii::DEFAULT,
            dark: true,

            bg_app: n(8.986, 2.213),        // --bg-sidebar-color
            bg_panel: n(12.236, 2.213),     // --color-bg-primary
            bg_raised: n(17.379, 2.534),    // raised button (measured)
            bg_raised_hover: n(20.5, 3.0),
            bg_group: n(16.486, 2.713),     // --color-bg-quaternary
            bg_chip_hover: n(16.486, 2.713),
            bg_tab_active: n(19.5, 3.0),
            bg_nav_hover: n(17.5, 3.0),
            bg_nav_active: n(21.486, 3.213), // active nav item (measured)
            bg_selected: n(18.5, 3.0),
            bg_card: n(16.6, 3.213),        // comment/draft card (measured #28292c)

            text_primary: n(100., 0.),      // --color-text-primary
            text_nav_active: n(100., 0.),
            text_secondary: n(91.224, 2.106), // --color-text-secondary
            text_heading: n(91.224, 2.106),
            text_muted: n(66., 2.106),
            text_tertiary: n(64.894, 2.106), // --color-text-tertiary
            text_faint: n(46., 2.106),
            text_quaternary: n(42.076, 2.106), // --color-text-quaternary
            icon_caret: n(32., 2.106),
            icon_view: lch(55., 3.577, 260.65),
            text_body: n(91.224, 2.106),
            text_meta: n(64.894, 2.106),
            text_placeholder: n(42.076, 2.106),
            accent: lch(62., 52., 288.43),

            border_panel: n(20.636, 3.413),  // <main> border (measured)
            border_chip: n(24.236, 3.413),   // --color-border-tertiary
            border_card: n(21.836, 3.413),   // --color-border-secondary
            ring: lcha(100., 0., 0., 0.158),      // light ring on dark (measured)
            ring_hover: lcha(100., 0., 0., 0.22),

            bg_option: n(20.5, 3.213),
            bg_code: n(23., 3.4),
            text_code: lch(80., 22., 30.),
            backdrop: lcha(0., 0., 0., 0.45),
            bg_logo: lch(100., 0., 282.),
            primary: lch(53., 52.26, 286.91),
            primary_hover: lch(58., 52.26, 286.91),
            on_primary: lch(100., 5., 286.91),
            shadow: lcha(0., 0., 0., 0.3),
            bg_menu: n(20.236, 2.713),       // measured
            border_menu: n(34.636, 3.913),
            separator_menu: n(28.636, 3.913),
        }
    }

    pub fn for_appearance(appearance: gpui::WindowAppearance) -> Self {
        match appearance {
            gpui::WindowAppearance::Dark | gpui::WindowAppearance::VibrantDark => Self::dark(),
            _ => Self::light(),
        }
    }

    /// "Raised" control: a 0.5px hairline ring plus soft drop shadows. In dark mode the
    /// ring is light and the drop shadow heavier (`lch(0 0 0 / .3) 0 .5px 1px 1px`, measured).
    pub fn raised_shadow(&self, hover: bool) -> Vec<BoxShadow> {
        let shadow = |color: Hsla, y: f32, blur: f32, spread: f32| BoxShadow {
            color,
            offset: point(px(0.), px(y)),
            blur_radius: px(blur),
            spread_radius: px(spread),
            inset: false,
        };
        let ring = shadow(if hover { self.ring_hover } else { self.ring }, 0., 0., 0.5);
        if self.dark {
            vec![ring, shadow(self.shadow, 0.5, 1., 1.)]
        } else {
            vec![ring, shadow(lcha(0., 0., 0., 0.02), 3., 6., -2.), shadow(lcha(0., 0., 0., 0.04), 1., 1., 0.)]
        }
    }

    /// Command menu / popovers.
    pub fn overlay_shadow(&self) -> Vec<BoxShadow> {
        let s = |a: f32, y: f32, blur: f32| BoxShadow {
            color: lcha(0., 0., 0., a),
            offset: point(px(0.), px(y)),
            blur_radius: px(blur),
            spread_radius: px(0.),
            inset: false,
        };
        if self.dark {
            vec![s(0.35, 9., 48.), s(0.4, 6., 24.), s(0.3, 1., 1.)]
        } else {
            vec![s(0.08, 9., 48.), s(0.1, 6., 24.), s(0.04, 1., 1.)]
        }
    }

    /// Context menus: three soft layers (dark: 12.5% each, measured).
    pub fn menu_shadow(&self) -> Vec<BoxShadow> {
        let a = if self.dark { 0.125 } else { 0.06 };
        [(3., 8.), (2., 5.), (1., 1.)]
            .into_iter()
            .into_iter()
            .map(|(y, blur)| BoxShadow {
                color: lcha(0., 0., 0., a),
                offset: point(px(0.), px(y)),
                blur_radius: px(blur),
                spread_radius: px(0.),
                inset: false,
            })
            .collect()
    }

    pub fn panel_shadow(&self) -> Vec<BoxShadow> {
        self.raised_shadow(false).into_iter().skip(1).collect()
    }

    /// Thin wrapper so call sites read `theme.is_dark()`.
    pub fn is_dark(&self) -> bool {
        self.dark
    }
}

impl Global for Theme {}

/// Whether the theme follows macOS or is pinned by the user.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Appearance {
    System,
    Light,
    Dark,
}

impl Global for Appearance {}

/// Corner radii by role. Components take these instead of pixel values, so the shape language
/// lives in one place.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Radii {
    /// keycaps, inline code, tiny highlights
    pub xs: Pixels,
    /// menu and picker rows, code blocks
    pub sm: Pixels,
    /// list rows, nav items, inputs, cards
    pub md: Pixels,
    /// dropdowns, small menus, toasts
    pub lg: Pixels,
    /// floating surfaces: panels, context menu, command menu, sheets
    pub xl: Pixels,
    /// the composer dialog
    pub dialog: Pixels,
    /// chips, pill buttons, round icon buttons (avatars and dots stay round regardless)
    pub pill: Pixels,
}

impl Radii {
    pub const DEFAULT: Radii = Radii { xs: px(4.), sm: px(7.), md: px(8.), lg: px(10.), xl: px(12.), dialog: px(21.), pill: px(9999.) };
}

/// Install the theme for the current preference + system appearance and repaint every window
/// (cached views would otherwise keep their old colors).
pub fn apply_appearance(cx: &mut App) {
    let preference = cx.try_global::<Appearance>().copied().unwrap_or(Appearance::System);
    let theme = match preference {
        Appearance::Light => Theme::light(),
        Appearance::Dark => Theme::dark(),
        Appearance::System => Theme::for_appearance(cx.window_appearance()),
    };
    cx.set_global(theme);
    cx.refresh_windows();
}

pub trait ActiveTheme {
    fn theme(&self) -> &Theme;
}

impl ActiveTheme for App {
    fn theme(&self) -> &Theme {
        self.global::<Theme>()
    }
}

/// Type ramp (px).
pub mod typography {
    use gpui::FontWeight;
    pub const SIZE_MINI: f32 = 12.;
    pub const SIZE_MICRO: f32 = 11.;
    pub const SIZE_SMALL: f32 = 13.;
    pub const SIZE_REGULAR: f32 = 15.;
    pub const SIZE_TITLE: f32 = 24.;
    pub const WEIGHT_NORMAL: FontWeight = FontWeight(450.);
    pub const WEIGHT_MEDIUM: FontWeight = FontWeight(500.);
    pub const WEIGHT_WORKSPACE: FontWeight = FontWeight(550.);
    pub const WEIGHT_SEMIBOLD: FontWeight = FontWeight(600.);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex(c: Hsla) -> String {
        let c = c.to_rgb();
        format!("#{:02x}{:02x}{:02x}", (c.r * 255.).round() as u8, (c.g * 255.).round() as u8, (c.b * 255.).round() as u8)
    }

    #[test]
    fn lch_matches_reference_hex() {
        // light panel / sidebar backgrounds, checked against a CSS lch() implementation
        assert_eq!(hex(lch(97.94, 0.5, 282.)), "#f9f9fa");
        assert_eq!(hex(lch(94.44, 0.5, 282.)), "#efeff0");
    }
}
