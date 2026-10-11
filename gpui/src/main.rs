mod assets;
mod issue_list;
mod calendar_fixture;
mod mail_fixture;
mod model;
mod sidebar;
mod linear_icons;
mod status_icon;
mod workspace;

use std::rc::Rc;

use gpui::{
    App, Bounds, Context, Render, TitlebarOptions, Window, WindowBounds, WindowOptions, point, prelude::*, px, size,
};

use crate::assets::Assets;
use crate::model::Fixture;
use crate::workspace::Workspace;

fn open_window<V: Render>(title: &'static str, cx: &mut App, build: impl FnOnce(&mut Window, &mut Context<V>) -> V + 'static) {
    // same logical size as the reference captures (1420×816 @2x)
    let bounds = Bounds::centered(None, size(px(1420.), px(816.)), cx);
    cx.open_window(
        WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            titlebar: Some(TitlebarOptions {
                title: Some(title.into()),
                appears_transparent: true,
                traffic_light_position: Some(point(px(16.), px(15.))),
            }),
            window_min_size: Some(size(px(800.), px(500.))),
            ..Default::default()
        },
        |window, cx| cx.new(|cx| build(window, cx)),
    )
    .expect("failed to open window");
}

/// GPUI mimics CoreGraphics font smoothing: light glyphs get dilated by their luminance, so text
/// on dark backgrounds renders a weight heavier than intended (what the web calls
/// `-webkit-font-smoothing: antialiased` is the look we want). GPUI skips the dilation when the app's own
/// `AppleFontSmoothing` preference is 0 – read once, lazily, so set it before the first glyph.
#[cfg(target_os = "macos")]
fn disable_font_smoothing() {
    use objc2_foundation::{NSString, NSUserDefaults};
    NSUserDefaults::standardUserDefaults().setInteger_forKey(0, &NSString::from_str("AppleFontSmoothing"));
}

fn main() {
    mail::trace::started();
    let mail = std::env::args().nth(1).as_deref() == Some("mail");
    if mail {
        // first: the connect below caches into it
        mail::set_cache_root(std::env::var_os("HOME").map(std::path::PathBuf::from).unwrap_or_else(std::env::temp_dir).join("Library/Caches/mail-prototype"));
    }
    // MAIL_BACKEND=http://127.0.0.1:9723 reads a durian server (read-only); MAIL_TOKEN is its
    // bearer token, unless it runs with --no-auth. Connecting and the HTML helper start here,
    // before GPUI and the window: those take 150–450 ms the network can use.
    let live = mail
        .then(|| std::env::var("MAIL_BACKEND").ok().filter(|s| !s.trim().is_empty()))
        .flatten()
        .map(|server| {
            let token = std::env::var("MAIL_TOKEN").ok().filter(|t| !t.trim().is_empty());
            mail::durian::Live::preconnect(mail::durian::Client::new(&server, token))
        });
    if mail && mail::uses_snapshots() {
        mail::html_render::start_helper_early();
    }
    #[cfg(target_os = "macos")]
    disable_font_smoothing();
    gpui_platform::application().with_assets(Assets).run(move |cx: &mut App| {
        ui::init(cx).expect("failed to load bundled fonts");
        // MAIL_APPEARANCE=light|dark pins the appearance (screenshots of both modes)
        match std::env::var("MAIL_APPEARANCE").as_deref() {
            Ok("light") => cx.set_global(ui::theme::Appearance::Light),
            Ok("dark") => cx.set_global(ui::theme::Appearance::Dark),
            _ => {}
        }
        ui::theme::apply_appearance(cx);
        mail::bind_keys(cx);

        if mail {
            if let Some(connect) = live {
                open_window("Mail", cx, move |window, cx| mail::MailWorkspace::live(connect, window, cx));
                // MAIL_BACKGROUND: a second instance that leaves the focus where it is
                if std::env::var_os("MAIL_BACKGROUND").is_none() {
                    cx.activate(true);
                }
                return;
            }
            let data = mail_fixture::load().expect("failed to load the demo mail data");
            let avatars = mail_fixture::avatars(&data);
            // your addresses: every account's primary and aliases
            let me = data.accounts.iter().flat_map(|a| std::iter::once(a.email.clone()).chain(a.aliases.iter().cloned())).collect();
            let calendar = calendar_fixture::load(chrono::Local::now().date_naive(), &avatars, me).expect("failed to load the demo calendar");
            open_window("Mail", cx, move |window, cx| mail::MailWorkspace::new(data, calendar, window, cx));
        } else {
            Assets::load_fonts(cx).expect("failed to load Inter");
            let fixture = Rc::new(Fixture::load().expect("failed to load data/my_issues.json"));
            open_window("Linear", cx, move |_, cx| Workspace::new(fixture, cx));
        }
        cx.activate(true);
    });
}
