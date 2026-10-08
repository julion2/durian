//! Native WKWebView-backed HTML mail body for macOS.
//!
//! WebKit is a real AppKit child of the GPUI window. The child is restricted to
//! the intersection of this element and GPUI's current content mask; JavaScript
//! scrolls the document by the clipped-away amount so list scrolling remains
//! continuous without bitmap capture.
#[path = "../../mail-webview/src/document.rs"]
mod document;

use block2::RcBlock;
use gpui_kit::{
    component::{ActiveTheme as _, WindowExt as _},
    *,
};
use objc2_app_kit::{NSEvent, NSEventMask, NSEventModifierFlags, NSEventType, NSImage, NSView};
use objc2_foundation::{MainThreadMarker, NSError};
use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    ptr::NonNull,
    rc::{Rc, Weak},
    sync::Arc,
    sync::mpsc,
    time::Duration,
};
use wry::{
    NewWindowResponse, PermissionResponse, Rect, WebView, WebViewBuilder,
    WebViewBuilderExtDarwin as _, WebViewExtMacOS as _, dpi::LogicalPosition, dpi::LogicalSize,
    http::Response,
};

pub struct Changed;
impl EventEmitter<Changed> for BrowserBody {}

actions!(mail_browser, [CopySelection, SelectAll]);

const SELECT_ALL_SCRIPT: &str = "const r=document.createRange();r.selectNodeContents(document.body);const s=getSelection();s.removeAllRanges();s.addRange(r);window.ipc.postMessage('selection:'+s.toString());";

thread_local! {
    static WEBVIEWS: RefCell<Vec<Weak<RefCell<NativeView>>>> = const { RefCell::new(Vec::new()) };
    static NATIVE_OVERLAY_ACTIVE: Cell<bool> = const { Cell::new(false) };
    static KEY_MONITOR_INSTALLED: Cell<bool> = const { Cell::new(false) };
    static FRAME_GENERATION: Cell<usize> = const { Cell::new(0) };
}

/// Native AppKit children always composite above GPUI's Metal surface. The root
/// must call this with its modal-overlay state so dialogs remain interactive.
/// This is temporary occlusion only; normal list clipping never blanks the body.
pub fn set_native_overlay_active(window: &mut Window, active: bool) {
    let generation = FRAME_GENERATION.get().wrapping_add(1);
    FRAME_GENERATION.set(generation);
    update_native_overlay(active);
    window.on_next_frame(move |_, _| {
        WEBVIEWS.with_borrow_mut(|views| {
            views.retain(|weak| {
                let Some(view) = weak.upgrade() else {
                    return false;
                };
                let mut view = view.borrow_mut();
                if view.painted_generation != generation {
                    view.in_view = false;
                    view.sync_visibility();
                }
                true
            });
        });
    });
}

fn update_native_overlay(active: bool) {
    if NATIVE_OVERLAY_ACTIVE.replace(active) == active {
        return;
    }
    WEBVIEWS.with_borrow_mut(|views| {
        views.retain(|weak| {
            let Some(view) = weak.upgrade() else {
                return false;
            };
            if active {
                NativeView::request_overlay_snapshot(&view);
            } else {
                let mut view = view.borrow_mut();
                view.overlay_generation = view.overlay_generation.wrapping_add(1);
                view.overlay_requested = false;
                view.overlay_active = false;
                if let Some(snapshot) = view.snapshot.take() {
                    view.retired_images.push(snapshot.image);
                    view.snapshot_generation = view.snapshot_generation.wrapping_add(1);
                }
                view.sync_visibility();
            }
            true
        });
    });
}

fn routes_to_gpui(key: &str, flags: NSEventModifierFlags) -> bool {
    if flags.contains(NSEventModifierFlags::Option) {
        return false;
    }
    let shift = flags.contains(NSEventModifierFlags::Shift);
    let control = flags.contains(NSEventModifierFlags::Control);
    if flags.contains(NSEventModifierFlags::Command) {
        return !control && !shift && key == "n";
    }
    if control {
        return if shift {
            key == "t"
        } else {
            matches!(key, "r" | "n" | "q")
        };
    }
    // charactersIgnoringModifiers preserves Shift: / is Shift+7 on German keyboards.
    if key == "/" {
        return true;
    }
    if shift {
        return matches!(key, "n" | "g" | "v" | "j" | "k");
    }
    matches!(
        key,
        "h" | "l"
            | "j"
            | "k"
            | "n"
            | "g"
            | "v"
            | "r"
            | "c"
            | "a"
            | "u"
            | "s"
            | "o"
            | "t"
            | "\u{1b}"
            | "\u{f700}"
            | "\u{f701}"
    )
}

pub fn init(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("ctrl-c", CopySelection, Some("MailBrowser")),
        KeyBinding::new("cmd-c", CopySelection, Some("MailBrowser")),
        KeyBinding::new("ctrl-a", SelectAll, Some("MailBrowser")),
        KeyBinding::new("cmd-a", SelectAll, Some("MailBrowser")),
    ]);
    KEY_MONITOR_INSTALLED.with(|installed| {
        if installed.replace(true) {
            return;
        }
        let monitor = RcBlock::new(|event: NonNull<NSEvent>| {
            let event = unsafe { event.as_ref() };
            if event.r#type() == NSEventType::ScrollWheel {
                let Some(event_window) =
                    event.window(MainThreadMarker::new().expect("main thread"))
                else {
                    return event as *const NSEvent as *mut NSEvent;
                };
                let window_point = event.locationInWindow();
                let handled = WEBVIEWS.with_borrow(|views| {
                    views.iter().any(|weak| {
                        let Some(native) = weak.upgrade() else {
                            return false;
                        };
                        let native = native.borrow();
                        let webview = native.webview.webview();
                        let webview: &NSView = &webview;
                        let webview_window = native.webview.ns_window();
                        if !std::ptr::eq(&*event_window, &*webview_window)
                            || !native.in_view
                            || native.overlay_active
                            || webview.isHidden()
                        {
                            return false;
                        }
                        let point = webview.convertPoint_fromView(window_point, None);
                        let bounds = webview.bounds();
                        if point.x < bounds.origin.x
                            || point.y < bounds.origin.y
                            || point.x > bounds.origin.x + bounds.size.width
                            || point.y > bounds.origin.y + bounds.size.height
                        {
                            return false;
                        }
                        let y = if webview.isFlipped() {
                            point.y - bounds.origin.y
                        } else {
                            bounds.size.height - (point.y - bounds.origin.y)
                        };
                        let _ = native.events.send(BrowserEvent::Scroll {
                            x: event.scrollingDeltaX() as f32,
                            y: event.scrollingDeltaY() as f32,
                            precise: event.hasPreciseScrollingDeltas(),
                            position_x: f32::from(native.visible_bounds.origin.x)
                                + (point.x - bounds.origin.x) as f32,
                            position_y: f32::from(native.visible_bounds.origin.y) + y as f32,
                        });
                        true
                    })
                });
                return if handled {
                    std::ptr::null_mut()
                } else {
                    event as *const NSEvent as *mut NSEvent
                };
            }
            let flags = event.modifierFlags();
            let character = event
                .charactersIgnoringModifiers()
                .map(|value| value.to_string().to_lowercase());
            let route = routes_to_gpui(character.as_deref().unwrap_or(""), flags);
            let copy_or_select = flags.contains(NSEventModifierFlags::Command)
                && !flags.intersects(
                    NSEventModifierFlags::Control
                        | NSEventModifierFlags::Option
                        | NSEventModifierFlags::Shift,
                )
                && matches!(character.as_deref(), Some("a" | "c"));
            if !route && !copy_or_select {
                return event as *const NSEvent as *mut NSEvent;
            }
            let Some(window) = event.window(MainThreadMarker::new().expect("main thread")) else {
                return event as *const NSEvent as *mut NSEvent;
            };
            let Some(first) = window.firstResponder() else {
                return event as *const NSEvent as *mut NSEvent;
            };
            let Some(first) = first.downcast_ref::<NSView>() else {
                return event as *const NSEvent as *mut NSEvent;
            };
            let handled = WEBVIEWS.with_borrow(|views| {
                views.iter().any(|weak| {
                    let Some(native) = weak.upgrade() else {
                        return false;
                    };
                    let native = native.borrow();
                    let webview = native.webview.webview();
                    let webview: &NSView = &webview;
                    if !native.in_view
                        || native.overlay_active
                        || webview.isHidden()
                        || (!std::ptr::eq(first, webview) && !first.isDescendantOf(webview))
                    {
                        return false;
                    }
                    if route {
                        // Change AppKit responder synchronously, then let it deliver
                        // this original event to GPUI. Queuing a JS shortcut can
                        // otherwise steal the first characters typed into Search.
                        return native.webview.focus_parent().is_ok();
                    } else if character.as_deref() == Some("a") {
                        let _ = native.webview.evaluate_script(SELECT_ALL_SCRIPT);
                    } else {
                        let _ = native.events.send(BrowserEvent::Copy);
                    }
                    true
                })
            });
            if handled && !route {
                std::ptr::null_mut()
            } else {
                event as *const NSEvent as *mut NSEvent
            }
        });
        let monitor = unsafe {
            NSEvent::addLocalMonitorForEventsMatchingMask_handler(
                NSEventMask::KeyDown | NSEventMask::ScrollWheel,
                &monitor,
            )
        };
        // AppKit owns the monitor registration; retain its token for process lifetime.
        if let Some(monitor) = monitor {
            std::mem::forget(monitor);
        }
    });
}

enum BrowserEvent {
    Height(f32),
    Link(String),
    Selection(String),
    Copy,
    Scroll {
        x: f32,
        y: f32,
        precise: bool,
        position_x: f32,
        position_y: f32,
    },
}

struct NativeView {
    webview: WebView,
    events: mpsc::Sender<BrowserEvent>,
    painted_generation: usize,
    in_view: bool,
    overlay_requested: bool,
    overlay_active: bool,
    scroll_y: f32,
    visible_bounds: Bounds<Pixels>,
    snapshot: Option<Snapshot>,
    snapshot_generation: usize,
    overlay_generation: usize,
    retired_images: Vec<Arc<RenderImage>>,
}

struct Snapshot {
    image: Arc<RenderImage>,
    bounds: Bounds<Pixels>,
}

impl NativeView {
    fn sync_visibility(&self) {
        let visible = self.in_view && !self.overlay_active;
        if !visible {
            // GPUI's logical focus and AppKit's first responder are separate.
            // A hidden native child must not retain keys intended for a dialog.
            let webview = self.webview.webview();
            let webview: &NSView = &webview;
            if let Some(window) = webview.window()
                && let Some(first) = window.firstResponder()
                && let Some(first) = first.downcast_ref::<NSView>()
                && (std::ptr::eq(first, webview) || first.isDescendantOf(webview))
            {
                let _ = self.webview.focus_parent();
            }
        }
        let _ = self.webview.set_visible(visible);
    }

    fn request_overlay_snapshot(view: &Rc<RefCell<Self>>) {
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
        let callback = RcBlock::new(move |image: *mut NSImage, _: *mut NSError| {
            let Some(view) = weak.upgrade() else {
                return;
            };
            let snapshot = NonNull::new(image).and_then(|image| {
                // The completion handler owns the image for this call. Copy its
                // TIFF bytes before returning, then let image-rs bound decoding.
                let data = unsafe { image.as_ref().TIFFRepresentation() }?;
                let mut bytes = vec![0; data.length()];
                if !bytes.is_empty() {
                    unsafe {
                        data.getBytes_length(
                            NonNull::new_unchecked(bytes.as_mut_ptr().cast()),
                            bytes.len(),
                        );
                    }
                }
                let mut rgba = image::load_from_memory(&bytes).ok()?.into_rgba8();
                for pixel in rgba.pixels_mut() {
                    pixel.0.swap(0, 2);
                }
                Some(Arc::new(RenderImage::new([image::Frame::new(rgba)])))
            });
            let mut state = view.borrow_mut();
            if state.overlay_requested && state.overlay_generation == generation {
                if let Some(image) = snapshot {
                    let bounds = state.visible_bounds;
                    if let Some(previous) = state.snapshot.replace(Snapshot { image, bounds }) {
                        state.retired_images.push(previous.image);
                    }
                    state.overlay_active = true;
                } else {
                    // Keep live WebKit visible rather than presenting a blank body.
                    state.overlay_active = false;
                }
                state.snapshot_generation += 1;
                state.sync_visibility();
            }
        });
        let webview = view.borrow().webview.webview();
        unsafe {
            webview.takeSnapshotWithConfiguration_completionHandler(None, &callback);
        }
    }
}

pub struct BrowserBody {
    focus: FocusHandle,
    native: Option<Rc<RefCell<NativeView>>>,
    height: f32,
    selection: String,
    error: Option<String>,
}

impl BrowserBody {
    pub fn focus(&self, window: &mut Window, cx: &mut App) {
        window.focus(&self.focus, cx);
        if let Some(native) = &self.native {
            let _ = native.borrow().webview.focus();
        }
    }

    pub fn new(
        html: String,
        images: HashMap<String, String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let dark = cx.theme().is_dark();
        let document = match document::prepare(&document::Mail { html, dark, images }) {
            Ok(document) => document,
            Err(error) => {
                return Self {
                    focus: cx.focus_handle(),
                    native: None,
                    height: 180.,
                    selection: String::new(),
                    error: Some(error),
                };
            }
        };
        let (events, incoming) = mpsc::channel();
        let callback = events.clone();
        let dark_transform = document::dark_script();
        let host = format!(
            r#"addEventListener('DOMContentLoaded', () => {{
                if ({dark}) {{
                    {dark_transform}
                    document.documentElement.style.setProperty('background', '#2a2a2c', 'important');
                    document.body.style.setProperty('background-color', '#2a2a2c', 'important');
                    document.body.style.setProperty('color', '#e5e5e5', 'important');
                    document.documentElement.style.colorScheme = 'dark';
                }}
                const height = () => window.ipc.postMessage('height:' + Math.max(
                    document.documentElement.scrollHeight, document.body.scrollHeight));
                new ResizeObserver(height).observe(document.body);
                height();
            }}, {{once:true}});
            addEventListener('click', event => {{
                const link = event.target.closest('a');
                if (link && !link.getAttribute('href')?.startsWith('#')) {{
                    event.preventDefault();
                    window.ipc.postMessage('link:' + (link.getAttribute('href') || ''));
                }}
            }}, true);
            addEventListener('selectionchange', () => {{
                window.ipc.postMessage('selection:' + getSelection().toString());
            }});"#
        );
        let images = document.images;
        let builder = WebViewBuilder::new()
            .with_bounds(Rect {
                position: LogicalPosition::new(0., 0.).into(),
                size: LogicalSize::new(1., 1.).into(),
            })
            .with_focused(false)
            .with_visible(false)
            .with_html(document.html)
            .with_initialization_script(&host)
            .with_custom_protocol("durianmail".into(), move |_, request| {
                match images.get(&request.uri().to_string()) {
                    Some(png) => Response::builder()
                        .header("Content-Type", "image/png")
                        .header("Cache-Control", "no-store")
                        .body(std::borrow::Cow::Owned(png.clone()))
                        .unwrap(),
                    None => Response::builder()
                        .status(404)
                        .body(std::borrow::Cow::Borrowed(&[][..]))
                        .unwrap(),
                }
            })
            .with_navigation_handler(|url| url == "about:blank" || url.starts_with("about:blank#"))
            .with_new_window_req_handler(|_, _| NewWindowResponse::Deny)
            .with_download_started_handler(|_, _| false)
            .with_permission_handler(|_| PermissionResponse::Deny)
            .with_ipc_handler(move |request| {
                let body = request.body();
                let event = if let Some(value) = body.strip_prefix("height:") {
                    value.parse().ok().map(BrowserEvent::Height)
                } else if let Some(url) = body.strip_prefix("link:") {
                    Some(BrowserEvent::Link(url.into()))
                } else if let Some(selection) = body.strip_prefix("selection:") {
                    Some(BrowserEvent::Selection(selection.into()))
                } else {
                    None
                };
                if let Some(event) = event {
                    let _ = callback.send(event);
                }
            })
            .with_incognito(true)
            .with_devtools(false)
            .with_allow_link_preview(false);
        let webview = match builder.build_as_child(window) {
            Ok(webview) => webview,
            Err(error) => {
                return Self {
                    focus: cx.focus_handle(),
                    native: None,
                    height: 180.,
                    selection: String::new(),
                    error: Some(format!("Couldn’t create the embedded WebKit view: {error}")),
                };
            }
        };
        // Wry may make a newly attached child first responder even when built
        // unfocused. Keep Durian's list focus until Alt+V explicitly enters HTML.
        let _ = webview.focus_parent();
        let native = Rc::new(RefCell::new(NativeView {
            webview,
            events,
            painted_generation: 0,
            in_view: false,
            overlay_requested: false,
            overlay_active: NATIVE_OVERLAY_ACTIVE.get(),
            scroll_y: -1.,
            visible_bounds: Bounds::default(),
            snapshot: None,
            snapshot_generation: 0,
            overlay_generation: 0,
            retired_images: Vec::new(),
        }));
        if NATIVE_OVERLAY_ACTIVE.get() {
            NativeView::request_overlay_snapshot(&native);
        }
        WEBVIEWS.with_borrow_mut(|views| views.push(Rc::downgrade(&native)));
        let observed = Rc::downgrade(&native);
        let mut snapshot_generation = 0;
        cx.spawn_in(window, async move |this, cx| {
            loop {
                cx.background_executor()
                    .timer(Duration::from_millis(33))
                    .await;
                let messages: Vec<_> = incoming.try_iter().collect();
                let alive = cx
                    .update(|window, cx| {
                        if this.update(cx, |_, _| ()).is_err() {
                            return false;
                        }
                        let active = window.has_active_dialog(cx)
                            || window.has_active_sheet(cx)
                            || !window.notifications(cx).is_empty()
                            || gpui_kit::base::GlobalState::is_in_deferred_context(cx);
                        update_native_overlay(active);
                        let Some(observed) = observed.upgrade() else {
                            return false;
                        };
                        let generation = observed.borrow().snapshot_generation;
                        if generation != snapshot_generation {
                            snapshot_generation = generation;
                            if this.update(cx, |_, cx| cx.notify()).is_err() {
                                return false;
                            }
                        }
                        let retired = std::mem::take(&mut observed.borrow_mut().retired_images);
                        for image in retired {
                            cx.drop_image(image, Some(window));
                        }
                        for event in messages {
                            if this
                                .update(cx, |this, cx| match event {
                                    BrowserEvent::Height(height)
                                        if height.is_finite()
                                            && (1.0..=1_000_000.0).contains(&height) =>
                                    {
                                        if (this.height - height).abs() >= 0.5 {
                                            this.height = height;
                                            cx.emit(Changed);
                                            cx.notify();
                                        }
                                    }
                                    BrowserEvent::Link(url) if document::external_link(&url) => {
                                        crate::content::confirm_link(url, window, cx);
                                    }
                                    BrowserEvent::Selection(selection) => {
                                        this.selection = selection;
                                    }
                                    BrowserEvent::Copy => {
                                        if !this.selection.is_empty() {
                                            cx.write_to_clipboard(ClipboardItem::new_string(
                                                this.selection.clone(),
                                            ));
                                        }
                                    }
                                    BrowserEvent::Scroll {
                                        x,
                                        y,
                                        precise,
                                        position_x,
                                        position_y,
                                    } => {
                                        let delta = if precise {
                                            ScrollDelta::Pixels(point(px(x), px(y)))
                                        } else {
                                            ScrollDelta::Lines(point(x, y))
                                        };
                                        window.dispatch_event(
                                            PlatformInput::ScrollWheel(ScrollWheelEvent {
                                                position: point(px(position_x), px(position_y)),
                                                delta,
                                                ..Default::default()
                                            }),
                                            cx,
                                        );
                                    }
                                    _ => {}
                                })
                                .is_err()
                            {
                                return false;
                            }
                        }
                        true
                    })
                    .unwrap_or(false);
                if !alive {
                    break;
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
                if let Some(snapshot) = native.snapshot.take() {
                    cx.drop_image(snapshot.image, None);
                }
                for image in native.retired_images.drain(..) {
                    cx.drop_image(image, None);
                }
            }
        })
        .detach();
        Self {
            focus: cx.focus_handle(),
            native: Some(native),
            height: 180.,
            selection: String::new(),
            error: None,
        }
    }
}

impl Render for BrowserBody {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if let Some(error) = &self.error {
            return div()
                .track_focus(&self.focus)
                .text_sm()
                .text_color(cx.theme().danger)
                .child(error.clone())
                .into_any_element();
        }
        let native = self.native.as_ref().expect("native view or error").clone();
        let snapshot = native
            .borrow()
            .snapshot
            .as_ref()
            .map(|snapshot| (snapshot.image.clone(), snapshot.bounds));
        div()
            .key_context("MailBrowser")
            .track_focus(&self.focus)
            .w_full()
            .h(px(self.height))
            .flex_shrink_0()
            .on_action(cx.listener(|this, _: &SelectAll, _, _| {
                if let Some(native) = &this.native {
                    let _ = native.borrow().webview.evaluate_script(SELECT_ALL_SCRIPT);
                }
            }))
            .on_action(cx.listener(|this, _: &CopySelection, _, cx| {
                if !this.selection.is_empty() {
                    cx.write_to_clipboard(ClipboardItem::new_string(this.selection.clone()));
                }
            }))
            .child(
                canvas(
                    move |bounds, window, _| {
                        let visible = bounds.intersect(&window.content_mask().bounds);
                        let mut native = native.borrow_mut();
                        native.painted_generation = FRAME_GENERATION.get();
                        native.in_view =
                            visible.size.width > px(0.) && visible.size.height > px(0.);
                        native.visible_bounds = visible;
                        if native.in_view {
                            let x = f32::from(visible.origin.x);
                            let y = f32::from(visible.origin.y);
                            let width = f32::from(visible.size.width).max(1.);
                            let height = f32::from(visible.size.height).max(1.);
                            let _ = native.webview.set_bounds(Rect {
                                position: LogicalPosition::new(x, y).into(),
                                size: LogicalSize::new(width, height).into(),
                            });
                            let scroll_y = f32::from(visible.origin.y - bounds.origin.y).max(0.);
                            if (scroll_y - native.scroll_y).abs() >= 0.5 {
                                native.scroll_y = scroll_y;
                                let _ = native.webview.evaluate_script(&format!(
                                    "window.scrollTo({{top:{scroll_y},behavior:'instant'}})"
                                ));
                            }
                        }
                        native.sync_visibility();
                    },
                    move |_, _, window, _| {
                        if let Some((image, bounds)) = snapshot {
                            let _ = window.paint_image(
                                bounds,
                                bounds,
                                Corners::default(),
                                image,
                                0,
                                false,
                            );
                        }
                    },
                )
                .size_full(),
            )
            .into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::{document, routes_to_gpui};
    use objc2_app_kit::NSEventModifierFlags as Flags;
    use std::collections::HashMap;

    #[test]
    fn shortcuts_return_to_gpui_without_stealing_native_text_keys() {
        assert!(routes_to_gpui("/", Flags::empty()));
        assert!(routes_to_gpui("/", Flags::Shift));
        assert!(routes_to_gpui("j", Flags::empty()));
        assert!(routes_to_gpui("j", Flags::Shift));
        assert!(routes_to_gpui("t", Flags::Control | Flags::Shift));
        assert!(!routes_to_gpui("t", Flags::Control));
        assert!(routes_to_gpui("n", Flags::Command));
        assert!(!routes_to_gpui("n", Flags::Command | Flags::Shift));
        for key in ["a", "c"] {
            assert!(routes_to_gpui(key, Flags::empty()));
            assert!(!routes_to_gpui(key, Flags::Command));
        }
        for key in ["\t", "\r", "d", ""] {
            assert!(!routes_to_gpui(key, Flags::empty()));
        }
        assert!(!routes_to_gpui("g", Flags::Option));
    }

    #[test]
    fn native_body_uses_the_shared_mail_policy() {
        let document = document::prepare(&document::Mail {
            html: "<script>bad()</script><img src='https://canary.invalid/x'><a href='https://example.com'>ok</a>".into(),
            dark: false,
            images: HashMap::new(),
        })
        .unwrap();
        assert!(!document.html.contains("<script"));
        assert!(!document.html.contains("canary.invalid"));
        assert!(document.html.contains("https://example.com"));
    }
}
