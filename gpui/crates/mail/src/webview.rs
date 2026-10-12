//! HTML mail in a live WKWebView (macOS): a real AppKit child of the GPUI window instead of a
//! snapshot – the mail at the card's real width, selectable text, dark mode, no height limit.
//! Based on durian's GPUI experiment (PR #429, `browser_mac.rs`).
//!
//! - **Geometry.** The native view covers exactly the visible part of its card (the card's
//!   bounds ∩ GPUI's content mask) and the document scrolls itself by the clipped-away amount,
//!   so the thread scrolls continuously. The card's height is the document's, reported by a
//!   ResizeObserver.
//! - **Above Metal.** AppKit children always composite over GPUI. While an overlay (⌘K, a
//!   menu, a dialog, a toast) is open, the view shows a snapshot of itself and hides; the
//!   workspace calls [`frame`] every render with whether one is open, and whether the thread
//!   is on screen at all (another mode, the list layout) – then every view hides. Not "was it
//!   painted this frame": the thread view is a cached view, and a frame that reuses its last
//!   paint runs none of our code (hiding on that blinked the mail away and back).
//! - **Input.** Wheel events over the view go to GPUI (the thread scrolls, not the mail), and
//!   so do mouse moves (WebKit gets them too): GPUI owns the cursor and resets it every frame
//!   from its own idea of the pointer – the page says what's under it (link, text) and the
//!   card's hitbox shows that. Keys while the view has focus go back to GPUI – a mail has no
//!   inputs – except ⌘C / ⌘A, which copy / select within the mail. Right-click opens the
//!   app's menu, not WebKit's; hovering a link names it in the thread's status line.
//! - **Content.** The document is `html_doc::prepare`d (allowlist, CSP); the WebView is
//!   incognito, navigates nowhere, opens no windows, downloads nothing, grants no
//!   permissions. Link clicks come back over IPC and open through `ui::link`.
//! - **Theme.** Dark mode runs durian's CIELAB transform (`dark_mode.js`, from the Swift app)
//!   over a fresh load: a theme change reloads the pristine document (durian #422).

use std::cell::{Cell, RefCell};
use std::ptr::NonNull;
use std::rc::{Rc, Weak};
use std::sync::Arc;

use block2::RcBlock;
use futures::StreamExt;
use futures::channel::mpsc;
use gpui::{
    Bounds, ClipboardItem, Context, Corners, CursorStyle, EventEmitter, HitboxBehavior, Hsla, IntoElement, Modifiers, MouseMoveEvent, ParentElement,
    Pixels, PlatformInput, Point, Render, RenderImage, ScrollDelta, ScrollWheelEvent, SharedString, Styled, Window, canvas, div, point, px,
};
use objc2_app_kit::{NSEvent, NSEventMask, NSEventModifierFlags, NSEventType, NSImage, NSView};
use objc2_foundation::{MainThreadMarker, NSError};
use wry::dpi::{LogicalPosition, LogicalSize};
use wry::{NewWindowResponse, PermissionResponse, Rect, WebView, WebViewBuilder, WebViewBuilderExtDarwin as _, WebViewExtMacOS as _};

use crate::html_doc::host::SELECT_ALL;

/// Height before the document reports its own (a short mail's; avoids a big jump).
const INITIAL_HEIGHT: f32 = 120.;

thread_local! {
    static VIEWS: RefCell<Vec<Weak<RefCell<Native>>>> = const { RefCell::new(Vec::new()) };
    static OVERLAY: Cell<bool> = const { Cell::new(false) };
    static SHOWN: Cell<bool> = const { Cell::new(true) };
    static MONITOR: Cell<bool> = const { Cell::new(false) };
}

/// Called by the workspace on every render: whether an overlay covers the window, and whether
/// the thread (with its mail views) is on screen.
pub fn frame(_window: &mut Window, overlay: bool, shown: bool) {
    set_overlay(overlay);
    if SHOWN.replace(shown) != shown {
        each_view(|view| view.borrow().sync_visibility());
    }
}

fn each_view(mut f: impl FnMut(&Rc<RefCell<Native>>)) {
    VIEWS.with_borrow_mut(|views| {
        views.retain(|weak| match weak.upgrade() {
            Some(view) => {
                f(&view);
                true
            }
            None => false,
        })
    });
}

fn set_overlay(active: bool) {
    if OVERLAY.replace(active) == active {
        return;
    }
    each_view(|view| {
        if active {
            Native::snapshot_for_overlay(view);
        } else {
            let mut view = view.borrow_mut();
            view.overlay_generation = view.overlay_generation.wrapping_add(1);
            view.overlay_requested = false;
            view.overlay_shown = false;
            if let Some(snapshot) = view.snapshot.take() {
                view.retired.push(snapshot.0);
                view.snapshots = view.snapshots.wrapping_add(1);
            }
            view.sync_visibility();
        }
    });
}

/// Keys and wheel events for the native views (installed once).
fn install_monitor() {
    if MONITOR.replace(true) {
        return;
    }
    let monitor = RcBlock::new(|event: NonNull<NSEvent>| -> *mut NSEvent {
        let event = unsafe { event.as_ref() };
        let pass = event as *const NSEvent as *mut NSEvent;
        let Some(mtm) = MainThreadMarker::new() else { return pass };
        let kind = event.r#type();
        if kind == NSEventType::ScrollWheel || kind == NSEventType::MouseMoved {
            let Some(event_window) = event.window(mtm) else { return pass };
            let at = event.locationInWindow();
            let mut handled = false;
            each_view(|native| {
                if handled {
                    return;
                }
                let native = native.borrow();
                let view = native.webview.webview();
                let view: &NSView = &view;
                if !std::ptr::eq(&*event_window, &*native.webview.ns_window()) || !native.in_view || native.overlay_shown || view.isHidden() {
                    return;
                }
                let p = view.convertPoint_fromView(at, None);
                let b = view.bounds();
                if p.x < b.origin.x || p.y < b.origin.y || p.x > b.origin.x + b.size.width || p.y > b.origin.y + b.size.height {
                    return;
                }
                let y = if view.isFlipped() { p.y - b.origin.y } else { b.size.height - (p.y - b.origin.y) };
                let position = (f32::from(native.visible.origin.x) + (p.x - b.origin.x) as f32, f32::from(native.visible.origin.y) + y as f32);
                let _ = native.events.unbounded_send(if kind == NSEventType::ScrollWheel {
                    // the thread scrolls, not the mail inside it
                    Message::Scroll { x: event.scrollingDeltaX() as f32, y: event.scrollingDeltaY() as f32, precise: event.hasPreciseScrollingDeltas(), at: position }
                } else {
                    // GPUI learns where the pointer is (its cursor follows the page's wish)
                    Message::Move { at: position }
                });
                handled = true;
            });
            // a wheel event is the thread's; a move is WebKit's too (hover, selection)
            return if handled && kind == NSEventType::ScrollWheel { std::ptr::null_mut() } else { pass };
        }
        // keys: only while a mail view has focus
        let Some(window) = event.window(mtm) else { return pass };
        let Some(first) = window.firstResponder() else { return pass };
        let Some(first) = first.downcast_ref::<NSView>() else { return pass };
        let flags = event.modifierFlags();
        let key = event.charactersIgnoringModifiers().map(|k| k.to_string().to_lowercase()).unwrap_or_default();
        let command_only = flags.contains(NSEventModifierFlags::Command)
            && !flags.intersects(NSEventModifierFlags::Control | NSEventModifierFlags::Option | NSEventModifierFlags::Shift);
        let mut consumed = false;
        each_view(|native| {
            let native = native.borrow();
            let view = native.webview.webview();
            let view: &NSView = &view;
            if consumed || !native.in_view || (!std::ptr::eq(first, view) && !first.isDescendantOf(view)) {
                return;
            }
            match (command_only, key.as_str()) {
                (true, "a") => {
                    let _ = native.webview.evaluate_script(SELECT_ALL);
                    consumed = true;
                }
                (true, "c") => {
                    let _ = native.events.unbounded_send(Message::Copy);
                    consumed = true;
                }
                // everything else is the app's: hand the responder back, then let AppKit
                // deliver this very event to GPUI (a queued one would lose the first keys
                // typed into search)
                _ => {
                    let _ = native.webview.focus_parent();
                }
            }
        });
        if consumed { std::ptr::null_mut() } else { pass }
    });
    let token = unsafe { NSEvent::addLocalMonitorForEventsMatchingMask_handler(NSEventMask::KeyDown | NSEventMask::ScrollWheel | NSEventMask::MouseMoved, &monitor) };
    // AppKit keeps the registration for the process's lifetime
    std::mem::forget(token);
}

enum Message {
    Height(f32),
    Link(String),
    Selection(String),
    Copy,
    Scroll { x: f32, y: f32, precise: bool, at: (f32, f32) },
    Move { at: (f32, f32) },
    Cursor(CursorStyle),
    Hover(String),
    Menu(MenuRequest),
}

/// A right-click in the page: where (in the view's coordinates) and on what.
#[derive(serde::Deserialize)]
struct MenuRequest {
    x: f32,
    y: f32,
    #[serde(default)]
    link: String,
    #[serde(default)]
    selection: String,
}

pub enum MailWebViewEvent {
    /// the link under the pointer (only ones that may be opened), for the status line
    Hover(Option<SharedString>),
    /// right-click: the app's menu at this window position
    ContextMenu { position: Point<Pixels>, link: Option<SharedString>, selection: String },
}

impl EventEmitter<MailWebViewEvent> for MailWebView {}

struct Native {
    webview: WebView,
    events: mpsc::UnboundedSender<Message>,
    in_view: bool,
    visible: Bounds<Pixels>,
    scroll_y: f32,
    overlay_requested: bool,
    overlay_shown: bool,
    overlay_generation: usize,
    snapshot: Option<(Arc<RenderImage>, Bounds<Pixels>)>,
    snapshots: usize,
    retired: Vec<Arc<RenderImage>>,
}

impl Native {
    fn sync_visibility(&self) {
        let visible = self.in_view && !self.overlay_shown && SHOWN.get();
        if !visible {
            // a hidden view must not keep keys meant for a dialog
            let view = self.webview.webview();
            let view: &NSView = &view;
            if let Some(window) = view.window()
                && let Some(first) = window.firstResponder()
                && let Some(first) = first.downcast_ref::<NSView>()
                && (std::ptr::eq(first, view) || first.isDescendantOf(view))
            {
                let _ = self.webview.focus_parent();
            }
        }
        let _ = self.webview.set_visible(visible);
    }

    /// An overlay opened: show a picture of the mail instead of the live view (which would
    /// cover the overlay).
    fn snapshot_for_overlay(view: &Rc<RefCell<Self>>) {
        {
            let mut state = view.borrow_mut();
            if state.overlay_requested {
                return;
            }
            state.overlay_requested = true;
            state.overlay_generation = state.overlay_generation.wrapping_add(1);
        }
        let generation = view.borrow().overlay_generation;
        let weak = Rc::downgrade(view);
        let done = RcBlock::new(move |image: *mut NSImage, _: *mut NSError| {
            let Some(view) = weak.upgrade() else { return };
            let picture = NonNull::new(image).and_then(|image| {
                // the image is ours for this call only: copy its bytes before returning
                let data = unsafe { image.as_ref().TIFFRepresentation() }?;
                let mut bytes = vec![0; data.length()];
                if !bytes.is_empty() {
                    unsafe { data.getBytes_length(NonNull::new_unchecked(bytes.as_mut_ptr().cast()), bytes.len()) };
                }
                let mut rgba = image::load_from_memory(&bytes).ok()?.into_rgba8();
                // GPUI's images are BGRA
                for pixel in rgba.pixels_mut() {
                    pixel.0.swap(0, 2);
                }
                Some(Arc::new(RenderImage::new([image::Frame::new(rgba)])))
            });
            let mut state = view.borrow_mut();
            if state.overlay_requested && state.overlay_generation == generation {
                match picture {
                    Some(picture) => {
                        let bounds = state.visible;
                        if let Some((previous, _)) = state.snapshot.replace((picture, bounds)) {
                            state.retired.push(previous);
                        }
                        state.overlay_shown = true;
                    }
                    // no picture: better the live view over the overlay than a blank card
                    None => state.overlay_shown = false,
                }
                state.snapshots = state.snapshots.wrapping_add(1);
                state.sync_visibility();
            }
        });
        let webview = view.borrow().webview.webview();
        unsafe { webview.takeSnapshotWithConfiguration_completionHandler(None, &done) };
    }
}

/// One HTML message's body.
pub struct MailWebView {
    native: Option<Rc<RefCell<Native>>>,
    height: f32,
    selection: String,
    /// what the page wants under the pointer
    cursor: CursorStyle,
    /// why there's no live view (the thread view falls back to the snapshot renderer)
    pub error: Option<SharedString>,
}

impl MailWebView {
    /// `html`: a document from `html_doc::prepare`. `dark`: run the dark transform onto
    /// `background` (the card's color).
    pub fn new(html: SharedString, dark: bool, background: Hsla, window: &mut Window, cx: &mut Context<Self>) -> Self {
        install_monitor();
        let (events, mut incoming) = mpsc::unbounded();
        let ipc = events.clone();
        let host = host_script(dark, background);
        let builder = WebViewBuilder::new()
            .with_bounds(Rect { position: LogicalPosition::new(0., 0.).into(), size: LogicalSize::new(1., 1.).into() })
            .with_focused(false)
            .with_visible(false)
            .with_transparent(true)
            .with_html(html.to_string())
            .with_initialization_script(&host)
            // the document we load, and in-page anchors – nothing else
            .with_navigation_handler(|url| crate::html_doc::allows_navigation(&url))
            .with_new_window_req_handler(|_, _| NewWindowResponse::Deny)
            .with_download_started_handler(|_, _| false)
            .with_permission_handler(|_| PermissionResponse::Deny)
            .with_ipc_handler(move |request| {
                let body = request.body();
                let message = if let Some(value) = body.strip_prefix("height:") {
                    value.parse().ok().map(Message::Height)
                } else if let Some(url) = body.strip_prefix("link:") {
                    Some(Message::Link(url.to_string()))
                } else if let Some(selection) = body.strip_prefix("selection:") {
                    Some(Message::Selection(selection.to_string()))
                } else if let Some(kind) = body.strip_prefix("cursor:") {
                    Some(Message::Cursor(match kind {
                        "pointer" => CursorStyle::PointingHand,
                        "text" => CursorStyle::IBeam,
                        _ => CursorStyle::Arrow,
                    }))
                } else if let Some(url) = body.strip_prefix("hover:") {
                    Some(Message::Hover(url.to_string()))
                } else {
                    body.strip_prefix("menu:").and_then(|json| serde_json::from_str(json).ok()).map(Message::Menu)
                };
                if let Some(message) = message {
                    let _ = ipc.unbounded_send(message);
                }
            })
            .with_incognito(true)
            .with_devtools(false)
            .with_allow_link_preview(false);
        let webview = match builder.build_as_child(window) {
            Ok(webview) => webview,
            Err(error) => return Self { native: None, height: INITIAL_HEIGHT, selection: String::new(), cursor: CursorStyle::Arrow, error: Some(format!("WebKit view unavailable: {error}").into()) },
        };
        // a new child may become first responder even when built unfocused: the list keeps it
        let _ = webview.focus_parent();
        let native = Rc::new(RefCell::new(Native {
            webview,
            events,
            in_view: false,
            visible: Bounds::default(),
            scroll_y: -1.,
            overlay_requested: false,
            overlay_shown: false,
            overlay_generation: 0,
            snapshot: None,
            snapshots: 0,
            retired: Vec::new(),
        }));
        if OVERLAY.get() {
            Native::snapshot_for_overlay(&native);
        }
        VIEWS.with_borrow_mut(|views| views.push(Rc::downgrade(&native)));

        // messages from the page (and the wheel monitor), applied on the UI thread
        let observed = Rc::downgrade(&native);
        cx.spawn_in(window, async move |this, cx| {
            while let Some(message) = incoming.next().await {
                let alive = this.update_in(cx, |this, window, cx| {
                    match message {
                        Message::Height(height) if height.is_finite() && (1.0..=1_000_000.0).contains(&height) => {
                            if (this.height - height).abs() >= 0.5 {
                                this.height = height;
                                cx.notify();
                            }
                        }
                        Message::Height(_) => {}
                        Message::Link(url) => {
                            ui::link::open(&url, cx);
                        }
                        Message::Selection(selection) => this.selection = selection,
                        Message::Copy => {
                            if !this.selection.is_empty() {
                                cx.write_to_clipboard(ClipboardItem::new_string(this.selection.clone()));
                            }
                        }
                        Message::Scroll { x, y, precise, at } => {
                            let delta = if precise { ScrollDelta::Pixels(point(px(x), px(y))) } else { ScrollDelta::Lines(point(x, y)) };
                            window.dispatch_event(PlatformInput::ScrollWheel(ScrollWheelEvent { position: point(px(at.0), px(at.1)), delta, ..Default::default() }), cx);
                        }
                        Message::Move { at } => {
                            window.dispatch_event(PlatformInput::MouseMove(MouseMoveEvent { position: point(px(at.0), px(at.1)), pressed_button: None, modifiers: Modifiers::default() }), cx);
                        }
                        Message::Cursor(cursor) => {
                            if this.cursor != cursor {
                                this.cursor = cursor;
                                cx.notify();
                            }
                        }
                        Message::Hover(url) => cx.emit(MailWebViewEvent::Hover(Some(url).filter(|u| ui::link::allowed(u)).map(Into::into))),
                        Message::Menu(request) => {
                            // the page's coordinates are the visible part's
                            let origin = observed.upgrade().map_or(Bounds::default(), |n| n.borrow().visible).origin;
                            let position = origin + point(px(request.x), px(request.y));
                            let link = Some(request.link).filter(|l| !l.is_empty()).map(Into::into);
                            cx.emit(MailWebViewEvent::ContextMenu { position, link, selection: request.selection });
                        }
                    }
                    // pictures replaced by newer ones leave the GPU
                    if let Some(native) = observed.upgrade() {
                        for image in std::mem::take(&mut native.borrow_mut().retired) {
                            cx.drop_image(image, Some(window));
                        }
                    }
                });
                if alive.is_err() {
                    break;
                }
            }
        })
        .detach();
        // a snapshot taken or dropped repaints the card
        let watched = Rc::downgrade(&native);
        cx.spawn_in(window, async move |this, cx| {
            let mut seen = 0;
            loop {
                cx.background_executor().timer(std::time::Duration::from_millis(50)).await;
                let Some(native) = watched.upgrade() else { break };
                let now = native.borrow().snapshots;
                if now != seen {
                    seen = now;
                    if this.update(cx, |_, cx| cx.notify()).is_err() {
                        break;
                    }
                }
            }
        })
        .detach();
        cx.on_release(|this, cx| {
            if let Some(native) = this.native.take() {
                let mut native = native.borrow_mut();
                native.in_view = false;
                native.sync_visibility();
                native.webview.webview().removeFromSuperview();
                if let Some((image, _)) = native.snapshot.take() {
                    cx.drop_image(image, None);
                }
                for image in native.retired.drain(..) {
                    cx.drop_image(image, None);
                }
            }
        })
        .detach();
        Self { native: Some(native), height: INITIAL_HEIGHT, selection: String::new(), cursor: CursorStyle::Arrow, error: None }
    }

    /// Put the keyboard into the mail (text selection with the keyboard).
    pub fn focus(&self) {
        if let Some(native) = &self.native {
            let _ = native.borrow().webview.focus();
        }
    }
}

/// Adapt GPUI's theme color to the renderer-independent trusted page script.
fn host_script(dark: bool, background: Hsla) -> String {
    let rgb = background.to_rgb();
    let (r, g, b) = ((rgb.r * 255.).round() as u8, (rgb.g * 255.).round() as u8, (rgb.b * 255.).round() as u8);
    crate::html_doc::host::script(dark, [r, g, b])
}

impl Render for MailWebView {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let Some(native) = self.native.clone() else { return div().into_any_element() };
        let snapshot = native.borrow().snapshot.clone();
        let cursor = self.cursor;
        div()
            .w_full()
            .h(px(self.height))
            .flex_shrink_0()
            .child(
                canvas(
                    move |bounds, window, _| {
                        let visible = bounds.intersect(&window.content_mask().bounds);
                        let mut native = native.borrow_mut();
                        native.in_view = visible.size.width > px(0.) && visible.size.height > px(0.);
                        native.visible = visible;
                        if native.in_view {
                            let (x, y) = (f32::from(visible.origin.x), f32::from(visible.origin.y));
                            let (w, h) = (f32::from(visible.size.width).max(1.), f32::from(visible.size.height).max(1.));
                            let _ = native.webview.set_bounds(Rect { position: LogicalPosition::new(x, y).into(), size: LogicalSize::new(w, h).into() });
                            // the part of the card above the visible area: scrolled away inside
                            let scroll_y = f32::from(visible.origin.y - bounds.origin.y).max(0.);
                            if (scroll_y - native.scroll_y).abs() >= 0.5 {
                                native.scroll_y = scroll_y;
                                let _ = native.webview.evaluate_script(&format!("window.scrollTo({{top:{scroll_y},behavior:'instant'}})"));
                            }
                        }
                        native.sync_visibility();
                        window.insert_hitbox(visible, HitboxBehavior::Normal)
                    },
                    move |_, hitbox, window, _| {
                        window.set_cursor_style(cursor, &hitbox);
                        if let Some((image, bounds)) = snapshot {
                            let _ = window.paint_image(bounds, bounds, Corners::default(), image, 0, false);
                        }
                    },
                )
                .size_full(),
            )
            .into_any_element()
    }
}

/// Whether live mail views can be used here (the snapshot renderer otherwise).
pub fn available() -> bool {
    std::env::var("MAIL_HTML").as_deref() != Ok("snapshot")
}

#[cfg(test)]
mod dump {
    /// `cargo test -p mail dump_host_script -- --ignored --nocapture | node --check`-style check
    #[test]
    #[ignore]
    fn dump_host_script() {
        println!("{}", super::host_script(true, gpui::rgb(0x1e1e20).into()));
    }
}
