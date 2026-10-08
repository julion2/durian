//! Windowless Chromium renderer. GPUI owns all windows, clipping and scrolling.
#[path = "../../mail-webview/src/document.rs"]
mod document;
#[cfg(target_os = "macos")]
mod mac;
mod protocol;

use base64::Engine as _;
use cef::{args::Args, *};
use protocol::{Command as Input, Event as Output};
use std::{
    io::{BufRead, Read, Write},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
        mpsc,
    },
    time::{Duration, Instant},
};

#[derive(Clone)]
struct Surface {
    viewport: Arc<Mutex<(u32, u32, f32)>>,
    scroll_y: Arc<Mutex<f64>>,
    requested_generation: Arc<AtomicU64>,
    ready_generation: Arc<AtomicU64>,
    closed: Arc<AtomicBool>,
}

fn emit(event: Output, pixels: &[u8]) {
    let json = serde_json::to_vec(&event).expect("serializable IPC");
    let mut stdout = std::io::stdout().lock();
    if stdout
        .write_all(&(json.len() as u32).to_le_bytes())
        .and_then(|_| stdout.write_all(&json))
        .and_then(|_| stdout.write_all(pixels))
        .and_then(|_| stdout.flush())
        .is_err()
    {
        // The parent owns our lifetime. Do not leave a browser running after
        // its window/process has gone away.
        std::process::exit(0);
    }
}

wrap_render_handler! {
    struct Renderer { surface: Surface, }
    impl RenderHandler {
        fn view_rect(&self, _browser: Option<&mut Browser>, rect: Option<&mut Rect>) {
            let (width, height, _) = *self.surface.viewport.lock().unwrap();
            if let Some(rect) = rect { rect.width = width as i32; rect.height = height as i32; }
        }
        fn screen_info(&self, _browser: Option<&mut Browser>, info: Option<&mut ScreenInfo>) -> i32 {
            if let Some(info) = info {
                info.device_scale_factor = self.surface.viewport.lock().unwrap().2;
                return 1;
            }
            0
        }
        fn on_scroll_offset_changed(&self, _browser: Option<&mut Browser>, _x: f64, y: f64) {
            // CEF reports device pixels; GPUI layout and scrollTo use CSS px.
            *self.surface.scroll_y.lock().unwrap() = y / f64::from(self.surface.viewport.lock().unwrap().2);
        }
        fn on_paint(&self, _browser: Option<&mut Browser>, kind: PaintElementType,
            _dirty: Option<&[Rect]>, buffer: *const u8, width: i32, height: i32) {
            if kind != PaintElementType::VIEW || buffer.is_null() || width <= 0 || height <= 0 { return; }
            // Resize/scale requests precede the renderer's layout commit. Do
            // not label a prior layout's pixels with the requested geometry.
            if self.surface.ready_generation.load(Ordering::SeqCst)
                != self.surface.requested_generation.load(Ordering::SeqCst) { return; }
            let Some(len) = protocol::frame_len(width as u32, height as u32) else { return; };
            // CEF owns this exact BGRA buffer for the duration of on_paint.
            let pixels = unsafe { std::slice::from_raw_parts(buffer, len) };
            let scale = self.surface.viewport.lock().unwrap().2;
            let y = *self.surface.scroll_y.lock().unwrap();
            emit(Output::Frame { width: width as u32, height: height as u32, scale, y }, pixels);
        }
        fn on_text_selection_changed(&self, _browser: Option<&mut Browser>, text: Option<&CefString>, _range: Option<&Range>) {
            emit(Output::Selection { text: text.map(ToString::to_string).unwrap_or_default() }, &[]);
        }
    }
}

wrap_display_handler! {
    struct Display { surface: Surface, }
    impl DisplayHandler {
        fn on_console_message(&self, browser: Option<&mut Browser>, _level: LogSeverity,
            message: Option<&CefString>, source: Option<&CefString>, _line: i32) -> i32 {
            if source.map(ToString::to_string).as_deref() == Some("https://durian-host.invalid/") {
                let message = message.map(ToString::to_string).unwrap_or_default();
                if let Some(value) = message.strip_prefix("durian-height:").and_then(|s| s.parse::<f64>().ok()) {
                    if value.is_finite() && (1.0..=1_000_000.0).contains(&value) {
                        emit(Output::Height { height: value }, &[]);
                    }
                } else if let Some(url) = message.strip_prefix("durian-link:")
                    && document::external_link(url) { emit(Output::Link { url: url.into() }, &[]); }
                else if let Some(generation) = message.strip_prefix("durian-viewport:").and_then(|s| s.parse::<u64>().ok())
                    && generation == self.surface.requested_generation.load(Ordering::SeqCst) {
                    self.surface.ready_generation.store(generation, Ordering::SeqCst);
                    if let Some(host) = browser.and_then(|browser| browser.host()) {
                        host.invalidate(PaintElementType::VIEW);
                    }
                }
            }
            // Never log sender-controlled content or URLs.
            1
        }
    }
}

fn script(frame: &Frame, source: &str) {
    frame.execute_java_script(
        Some(&source.into()),
        Some(&"https://durian-host.invalid/".into()),
        1,
    );
}

wrap_load_handler! {
    struct Loaded { dark: bool, }
    impl LoadHandler {
        fn on_load_end(&self, _browser: Option<&mut Browser>, frame: Option<&mut Frame>, _status: i32) {
            let Some(frame) = frame.filter(|frame| frame.is_main() == 1) else { return; };
            let theme = if self.dark { format!(r#"{}
                document.documentElement.style.setProperty('background','#2a2a2c','important');
                document.body.style.setProperty('background-color','#2a2a2c','important');
                document.body.style.setProperty('color','#e5e5e5','important');
                document.documentElement.style.colorScheme='dark';"#, document::dark_script()) } else { String::new() };
            script(frame, &format!(r#"(() => {{
                {theme}
                document.documentElement.style.overflow='hidden';
                document.body.style.overflow='hidden';
                let previous = 0;
                function measure() {{
                    const height = Math.ceil(document.body.getBoundingClientRect().height +
                        parseFloat(getComputedStyle(document.body).marginTop || 0) +
                        parseFloat(getComputedStyle(document.body).marginBottom || 0));
                    if (height !== previous) {{ previous=height; console.log('durian-height:'+height); }}
                }}
                new ResizeObserver(measure).observe(document.body);
                document.addEventListener('load', measure, true); measure();
                document.addEventListener('click', event => {{
                    const link = event.target.closest('a');
                    if (link) {{ event.preventDefault(); const url=link.getAttribute('href');
                        if (url && !url.startsWith('#')) console.log('durian-link:'+url); }}
                }}, true);
            }})()"#));
        }
    }
}

// CSP is backed by an all-resource network veto, not only navigation policy.
wrap_resource_request_handler! {
    struct Resources {}
    impl ResourceRequestHandler {
        fn on_before_resource_load(&self, _browser: Option<&mut Browser>, _frame: Option<&mut Frame>,
            request: Option<&mut Request>, _callback: Option<&mut Callback>) -> ReturnValue {
            let url = request.map(|request| CefString::from(&request.url()).to_string()).unwrap_or_default();
            if url.starts_with("data:") { ReturnValue::CONTINUE } else { ReturnValue::CANCEL }
        }
    }
}
wrap_request_handler! {
    struct Requests {}
    impl RequestHandler {
        fn on_before_browse(&self, _browser: Option<&mut Browser>, _frame: Option<&mut Frame>,
            request: Option<&mut Request>, _gesture: i32, _redirect: i32) -> i32 {
            let url = request.map(|request| CefString::from(&request.url()).to_string()).unwrap_or_default();
            (!url.starts_with("data:text/html;charset=utf-8;base64,") && url != "about:blank") as i32
        }
        fn resource_request_handler(&self, _browser: Option<&mut Browser>, _frame: Option<&mut Frame>,
            _request: Option<&mut Request>, _navigation: i32, _download: i32,
            _initiator: Option<&CefString>, _disable: Option<&mut i32>) -> Option<ResourceRequestHandler> {
            Some(Resources::new())
        }
    }
}
wrap_life_span_handler! {
    struct Lifetime { closed: Arc<AtomicBool>, }
    impl LifeSpanHandler {
        fn on_before_close(&self, _browser: Option<&mut Browser>) { self.closed.store(true, Ordering::SeqCst); }
    }
}
wrap_client! {
    struct ClientImpl { surface: Surface, dark: bool, }
    impl Client {
        fn render_handler(&self) -> Option<RenderHandler> { Some(Renderer::new(self.surface.clone())) }
        fn display_handler(&self) -> Option<DisplayHandler> { Some(Display::new(self.surface.clone())) }
        fn load_handler(&self) -> Option<LoadHandler> { Some(Loaded::new(self.dark)) }
        fn request_handler(&self) -> Option<RequestHandler> { Some(Requests::new()) }
        fn life_span_handler(&self) -> Option<LifeSpanHandler> { Some(Lifetime::new(self.surface.closed.clone())) }
    }
}
wrap_app! {
    struct AppImpl {}
    impl App {
        fn on_before_command_line_processing(&self, _process: Option<&CefString>, command: Option<&mut CommandLine>) {
            if let Some(command) = command {
                #[cfg(target_os = "linux")]
                command.append_switch_with_value(Some(&"ozone-platform".into()), Some(&"headless".into()));
                for switch in ["disable-gpu", "disable-background-networking", "disable-component-update", "disable-sync", "no-first-run", "no-default-browser-check"] {
                    command.append_switch(Some(&switch.into()));
                }
            }
        }
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    #[cfg(target_os = "macos")]
    let _loader = {
        let loader = library_loader::LibraryLoader::new(&std::env::current_exe()?, false);
        if !loader.load() {
            return Err("Couldn’t load CEF framework".into());
        }
        mac::setup_application();
        loader
    };
    let _ = api_hash(sys::CEF_API_VERSION_LAST, 0);
    let args = Args::new();
    let mut app = AppImpl::new();
    let result = execute_process(
        Some(args.as_main_args()),
        Some(&mut app),
        std::ptr::null_mut(),
    );
    if result >= 0 {
        std::process::exit(result);
    }
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        let mut reader = std::io::stdin().lock();
        loop {
            // Parent-owned pipe. Bound commands before deserializing HTML/images.
            let mut line = String::new();
            match reader
                .by_ref()
                .take(32 * 1024 * 1024 + 1)
                .read_line(&mut line)
            {
                Ok(0) | Err(_) => break,
                Ok(_) if line.len() > 32 * 1024 * 1024 => break,
                Ok(_) => match serde_json::from_str::<Input>(&line) {
                    Ok(command) => {
                        if tx.send(command).is_err() {
                            return;
                        }
                    }
                    Err(_) => break,
                },
            }
        }
        let _ = tx.send(Input::Close);
    });
    let Input::Open { html, dark, images } = rx.recv()? else {
        return Err("Expected message".into());
    };
    let mail = document::Mail { html, dark, images };
    let mut prepared = document::prepare(&mail)?;
    // The same sanitizer/CSP as WebKit. Only normalized PNGs supplied by the
    // parent become data images. There is no file or HTTP server to embed.
    prepared.html = prepared
        .html
        .replace("img-src durianmail:", "img-src data:");
    for (url, bytes) in prepared.images {
        prepared.html = prepared.html.replace(
            &url,
            &format!(
                "data:image/png;base64,{}",
                base64::engine::general_purpose::STANDARD.encode(bytes)
            ),
        );
    }
    let url = format!(
        "data:text/html;charset=utf-8;base64,{}",
        base64::engine::general_purpose::STANDARD.encode(prepared.html)
    );
    let cache = tempfile::tempdir()?;
    let settings = Settings {
        root_cache_path: cache.path().to_string_lossy().as_ref().into(),
        windowless_rendering_enabled: 1,
        external_message_pump: 1,
        log_severity: LogSeverity::DISABLE,
        ..Default::default()
    };
    if initialize(
        Some(args.as_main_args()),
        Some(&settings),
        Some(&mut app),
        std::ptr::null_mut(),
    ) != 1
    {
        return Err("CEF initialization failed".into());
    }
    let surface = Surface {
        viewport: Arc::new(Mutex::new((640, 600, 1.0))),
        scroll_y: Arc::new(Mutex::new(0.0)),
        requested_generation: Arc::new(AtomicU64::new(0)),
        ready_generation: Arc::new(AtomicU64::new(0)),
        closed: Arc::new(AtomicBool::new(false)),
    };
    let mut client = ClientImpl::new(surface.clone(), mail.dark);
    let browser = browser_host_create_browser_sync(
        Some(&WindowInfo {
            windowless_rendering_enabled: 1,
            ..Default::default()
        }),
        Some(&mut client),
        Some(&url.as_str().into()),
        Some(&BrowserSettings {
            windowless_frame_rate: 30,
            ..Default::default()
        }),
        None,
        None,
    )
    .ok_or("Couldn’t create browser")?;
    let host = browser.host().ok_or("No browser host")?;
    host.set_focus(1);
    let mut closing = None;
    loop {
        do_message_loop_work();
        while let Ok(command) = rx.try_recv() {
            match command {
                Input::Viewport {
                    width,
                    height,
                    scale,
                    y,
                } => {
                    if !(1..=2048).contains(&width)
                        || !(1..=1400).contains(&height)
                        || !scale.is_finite()
                        || !(1.0..=2.0).contains(&scale)
                        || !y.is_finite()
                    {
                        continue;
                    }
                    let previous = *surface.viewport.lock().unwrap();
                    *surface.viewport.lock().unwrap() = (width, height, scale);
                    if previous.2 != scale {
                        host.notify_screen_info_changed();
                    }
                    if previous != (width, height, scale) {
                        let generation =
                            surface.requested_generation.fetch_add(1, Ordering::SeqCst) + 1;
                        host.was_resized();
                        if let Some(frame) = browser.main_frame() {
                            script(
                                &frame,
                                &format!(
                                    "requestAnimationFrame(()=>requestAnimationFrame(()=>console.log('durian-viewport:{generation}')))"
                                ),
                            );
                        }
                    }
                    if let Some(frame) = browser.main_frame() {
                        script(&frame, &format!("scrollTo(0,{})", y.max(0.0)));
                    }
                }
                Input::Mouse {
                    x,
                    y,
                    button,
                    down,
                    count,
                } => {
                    let event = MouseEvent {
                        x,
                        y,
                        modifiers: if button { 16 } else { 0 },
                    };
                    if let Some(down) = down {
                        host.send_mouse_click_event(
                            Some(&event),
                            MouseButtonType::LEFT,
                            (!down) as i32,
                            count.clamp(1, 3),
                        );
                    } else {
                        host.send_mouse_move_event(Some(&event), 0);
                    }
                }
                Input::Key { code, shift } if [9, 13].contains(&code) => {
                    #[cfg(target_os = "macos")]
                    let (key_down, native_key_code) =
                        (KeyEventType::KEYDOWN, if code == 9 { 0x30 } else { 0x24 });
                    #[cfg(not(target_os = "macos"))]
                    let (key_down, native_key_code) = (KeyEventType::RAWKEYDOWN, 0);
                    for type_ in [key_down, KeyEventType::CHAR, KeyEventType::KEYUP] {
                        host.send_key_event(Some(&KeyEvent {
                            type_,
                            windows_key_code: code,
                            native_key_code,
                            character: code as u16,
                            unmodified_character: code as u16,
                            modifiers: if shift { 2 } else { 0 },
                            ..Default::default()
                        }));
                    }
                }
                Input::SelectAll => {
                    if let Some(frame) = browser.main_frame() {
                        frame.select_all();
                    }
                }
                Input::Close if closing.is_none() => {
                    host.close_browser(1);
                    closing = Some(Instant::now());
                }
                _ => {}
            }
        }
        if surface.closed.load(Ordering::SeqCst) {
            break;
        }
        if closing.is_some_and(|time| time.elapsed() > Duration::from_secs(3)) {
            std::process::exit(1);
        }
        std::thread::sleep(Duration::from_millis(8));
    }
    drop(host);
    drop(browser);
    drop(client);
    #[cfg(target_os = "macos")]
    mac::drain_shutdown();
    shutdown();
    Ok(())
}
