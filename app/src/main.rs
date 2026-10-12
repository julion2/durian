mod assets;
mod backend;
mod calendar_fixture;
mod mail_fixture;

use std::sync::mpsc;

use gpui::{
    App, Bounds, Context, Entity, Render, SharedString, TitlebarOptions, Window, WindowBounds,
    WindowOptions, div, point, prelude::*, px, size,
};
use ui::theme::ActiveTheme;

use crate::assets::Assets;

gpui::actions!(durian, [Quit]);

fn bind_keys(cx: &mut App) {
    // Rebuild rather than appending unbind tombstones. Deleting an override must
    // reveal the default again without accumulating old bindings across reloads.
    cx.clear_key_bindings();
    ui::bind_keys(cx);
    mail::bind_keys(cx);
    cx.bind_keys([
        gpui::KeyBinding::new("cmd-q", Quit, None),
        gpui::KeyBinding::new("ctrl-q", Quit, None),
    ]);
}

enum Startup {
    Ready(mail::durian::live::Preconnect),
    Reconnecting(usize),
    Failed(String),
}

/// Only the startup status lives here. Once ready, render the existing mail workspace.
/// A slow CLI or a failed handshake must not block the window's event loop or load fixtures.
struct MailEntry {
    workspace: Option<Entity<mail::MailWorkspace>>,
    status: SharedString,
    owner: backend::Backend,
}

impl MailEntry {
    fn new(mut start: mpsc::Receiver<Startup>, owner: backend::Backend, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let mut entry = Self { workspace: None, status: "Starting Durian…".into(), owner };
        match start.try_recv() {
            Ok(result) => entry.connected(result, window, cx),
            Err(mpsc::TryRecvError::Empty) => {}
            Err(mpsc::TryRecvError::Disconnected) => entry.status = "Durian startup failed.".into(),
        }
        // Keep receiving: an owned replacement has new credentials, not new edit state.
        // External mode closes this channel after its one initial connection.
        cx.spawn_in(window, async move |entry, cx| {
            loop {
                let (receiver, result) = cx.background_executor().spawn(async move {
                    let result = start.recv();
                    (start, result)
                }).await;
                start = receiver;
                let Ok(result) = result else { break };
                if entry.update_in(cx, |entry, window, cx| entry.connected(result, window, cx)).is_err() { break; }
            }
        }).detach();
        entry
    }

    fn connected(&mut self, result: Startup, window: &mut Window, cx: &mut Context<Self>) {
        if self.owner.stopped() { return; }
        match result {
            Startup::Ready(connect) => {
                if let Some(workspace) = &self.workspace {
                    workspace.update(cx, |workspace, cx| workspace.reconnect(connect, window, cx));
                } else {
                    self.workspace = Some(cx.new(|cx| mail::MailWorkspace::live(connect, window, cx)));
                }
            }
            Startup::Reconnecting(attempt) => {
                self.status = format!("Durian server exited. Reconnecting… (attempt {attempt}/3)").into();
                if let Some(workspace) = &self.workspace {
                    workspace.update(cx, |workspace, cx| workspace.backend_unavailable(self.status.clone(), cx));
                }
            }
            Startup::Failed(error) => {
                eprintln!("[BACKEND] {error}");
                self.status = error.into();
                if let Some(workspace) = &self.workspace {
                    workspace.update(cx, |workspace, cx| workspace.backend_unavailable(self.status.clone(), cx));
                }
            }
        }
        cx.notify();
    }
}

impl Render for MailEntry {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if let Some(workspace) = &self.workspace {
            return workspace.clone().into_any_element();
        }
        let theme = cx.theme();
        div().size_full().flex().items_center().justify_center().p(px(32.))
            .bg(theme.bg_app).font_family(theme.font_family).text_color(theme.text_primary)
            .child(self.status.clone()).into_any_element()
    }
}

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
    zlog::init();
    zlog::init_output_stderr();
    mail::trace::started();
    let demo = match std::env::args().skip(1).collect::<Vec<_>>().as_slice() {
        [] => false,
        [arg] if arg == "--demo" => true,
        // Keep the prototype's explicit mail invocation working; it now means live mail.
        [arg] if arg == "mail" => false,
        [arg] if arg == "--help" || arg == "-h" => {
            println!("Usage: durian-gpui [--demo]\n\nDefault: experimental live Durian mail with writes.\n--demo: bundled mail and calendar fixtures.\n\nMAIL_BACKEND / MAIL_TOKEN: connect to an external server.\nDURIAN_BIN: owned durian executable (otherwise sibling or PATH).");
            return;
        }
        _ => {
            eprintln!("Usage: durian-gpui [--demo]");
            std::process::exit(2);
        }
    };
    // Set the platform cache root before preconnect evaluates/caches profiles.
    mail::set_cache_root(mail::default_cache_root());
    let owner = backend::Backend::default();
    let start = if demo {
        None
    } else {
        let (tx, rx) = mpsc::channel();
        let source = backend::Source::from_env();
        let backend = owner.clone();
        // Start before GPUI: READY and Live::preconnect overlap platform/window bring-up.
        // Neither the CLI handshake nor any HTTP call blocks the UI thread.
        std::thread::spawn(move || {
            let source = source.map_err(|error| error.to_string()).and_then(|source| {
                mail::init_session()
                    .map_err(|_| "Could not create private Durian session storage. Check the temporary directory permissions.".to_string())
                    .map(|_| source)
            });
            let source = match source {
                Ok(source) => source,
                Err(error) => {
                    let _ = tx.send(Startup::Failed(error));
                    mail::clear_session();
                    return;
                }
            };
            let mut connected = false;
            backend.run(source, |event| {
                let update = match event {
                    backend::Event::Connected(connection) => {
                        connected = true;
                        Startup::Ready(mail::durian::Live::preconnect(mail::durian::Client::new(&connection.server, connection.token)))
                    }
                    backend::Event::Reconnecting { attempt } => Startup::Reconnecting(attempt),
                    backend::Event::Failed(error) => {
                        // Session uses OnceLock: never delete it while retrying or while
                        // an old workspace may still be draining background work.
                        if !connected { mail::clear_session(); }
                        Startup::Failed(error.to_string())
                    }
                };
                tx.send(update).is_ok()
            });
            if backend.stopped() {
                mail::clear_session();
            }
        });
        Some(rx)
    };
    if mail::uses_snapshots() {
        mail::html_render::start_helper_early();
    }
    #[cfg(target_os = "macos")]
    disable_font_smoothing();
    let quit_owner = owner.clone();
    let window_owner = owner.clone();
    gpui_platform::application().with_assets(Assets).run(move |cx: &mut App| {
        ui::init(cx).expect("failed to load bundled fonts");
        // MAIL_APPEARANCE=light|dark pins the appearance (screenshots of both modes)
        match std::env::var("MAIL_APPEARANCE").as_deref() {
            Ok("light") => cx.set_global(ui::theme::Appearance::Light),
            Ok("dark") => cx.set_global(ui::theme::Appearance::Dark),
            _ => {}
        }
        ui::theme::apply_appearance(cx);
        cx.set_global(mail::keymaps::Keymaps::default());
        bind_keys(cx);
        cx.observe_global::<mail::keymaps::Keymaps>(bind_keys).detach();
        cx.on_action(|_: &Quit, cx| cx.quit());
        cx.on_window_closed(|cx, _| {
            if cx.windows().is_empty() { cx.quit(); }
        }).detach();
        cx.on_app_quit(move |cx| {
            let owner = quit_owner.clone();
            cx.background_executor().spawn(async move {
                owner.stop();
                mail::html_render::stop_helper_early();
                mail::clear_session();
            })
        }).detach();

        if let Some(start) = start {
            open_window("Durian", cx, move |window, cx| MailEntry::new(start, window_owner, window, cx));
        } else {
            let data = mail_fixture::load().expect("failed to load the demo mail data");
            let avatars = mail_fixture::avatars(&data);
            // your addresses: every account's primary and aliases
            let me = data.accounts.iter().flat_map(|a| std::iter::once(a.email.clone()).chain(a.aliases.iter().cloned())).collect();
            let calendar = calendar_fixture::load(chrono::Local::now().date_naive(), &avatars, me).expect("failed to load the demo calendar");
            open_window("Durian — Demo", cx, move |window, cx| mail::MailWorkspace::new(data, calendar, window, cx));
        }
        // MAIL_BACKGROUND: a second instance that leaves the focus where it is.
        if std::env::var_os("MAIL_BACKGROUND").is_none() {
            cx.activate(true);
        }
    });
    // Also covers a platform event loop returning without an app-quit notification.
    owner.stop();
    mail::html_render::stop_helper_early();
    mail::clear_session();
}
