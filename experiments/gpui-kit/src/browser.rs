//! An interactive, offscreen browser inside the normal GPUI message layout.
//! The subprocess has no native child window to cover menus or steal focus.
#[path = "../../mail-cef/src/protocol.rs"]
mod protocol;

use gpui_kit::{component::ActiveTheme as _, *};
use protocol::{Command, Event};
use std::{
    cell::RefCell,
    collections::HashMap,
    io::{Read, Write},
    process::{Command as Process, Stdio},
    rc::Rc,
    sync::{Arc, Mutex, mpsc},
    time::Duration,
};

pub struct Changed;
impl EventEmitter<Changed> for BrowserBody {}

actions!(
    mail_browser,
    [CopySelection, SelectAll, NextLink, PreviousLink, OpenLink]
);

pub fn init(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("ctrl-c", CopySelection, Some("MailBrowser")),
        KeyBinding::new("cmd-c", CopySelection, Some("MailBrowser")),
        KeyBinding::new("ctrl-a", SelectAll, Some("MailBrowser")),
        KeyBinding::new("cmd-a", SelectAll, Some("MailBrowser")),
        KeyBinding::new("tab", NextLink, Some("MailBrowser")),
        KeyBinding::new("shift-tab", PreviousLink, Some("MailBrowser")),
        KeyBinding::new("enter", OpenLink, Some("MailBrowser")),
    ]);
}

struct Frame {
    image: Arc<RenderImage>,
    width: f32,
    height: f32,
    y: f32,
}

#[derive(Default)]
struct Geometry {
    bounds: Bounds<Pixels>,
    viewport: Option<(u32, u32, f32, f64)>,
    painted_y: f32,
}

pub struct BrowserBody {
    focus: FocusHandle,
    commands: mpsc::Sender<Command>,
    frame: Option<Frame>,
    height: f32,
    selection: String,
    error: Option<String>,
    geometry: Rc<RefCell<Geometry>>,
}

fn run(
    receiver: mpsc::Receiver<Command>,
    events: mpsc::Sender<Event>,
    latest: Arc<Mutex<Option<Frame>>>,
) -> Result<(), Box<dyn std::error::Error>> {
    let executable = std::env::var_os("DURIAN_CEF_HELPER")
        .map(std::path::PathBuf::from)
        .unwrap_or(std::env::current_exe()?.with_file_name("durian-mail-cef"));
    let mut command = Process::new(&executable);
    // Linux loader lookup is local to the helper; never alter GPUI's libraries.
    #[cfg(target_os = "linux")]
    command.env(
        "LD_LIBRARY_PATH",
        executable.parent().ok_or("Missing helper directory")?,
    );
    let mut child = command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()?;
    let mut input = child.stdin.take().expect("piped stdin");
    let mut output = child.stdout.take().expect("piped stdout");
    std::thread::spawn(move || {
        for command in receiver {
            let Ok(mut bytes) = serde_json::to_vec(&command) else {
                break;
            };
            if bytes.len() > 32 * 1024 * 1024 {
                break;
            }
            bytes.push(b'\n');
            if input.write_all(&bytes).is_err() {
                break;
            }
        }
        // EOF asks CEF to close all of its subprocesses when the view drops.
    });
    let result = (|| -> Result<(), Box<dyn std::error::Error>> {
        loop {
            let mut length = [0; 4];
            if let Err(error) = output.read_exact(&mut length) {
                if error.kind() == std::io::ErrorKind::UnexpectedEof {
                    return Ok(());
                }
                return Err(error.into());
            }
            let length = u32::from_le_bytes(length) as usize;
            if length > 1024 * 1024 {
                return Err("Oversized browser event".into());
            }
            let mut header = vec![0; length];
            output.read_exact(&mut header)?;
            match serde_json::from_slice(&header)? {
                Event::Frame {
                    width,
                    height,
                    scale,
                    y,
                } => {
                    let len = protocol::frame_len(width, height).ok_or("Invalid browser frame")?;
                    if !scale.is_finite()
                        || !(1.0..=2.0).contains(&scale)
                        || !y.is_finite()
                        || y < 0.0
                    {
                        return Err("Invalid browser geometry".into());
                    }
                    let mut pixels = vec![0; len];
                    output.read_exact(&mut pixels)?;
                    let buffer = image::RgbaImage::from_raw(width, height, pixels)
                        .ok_or("Invalid pixels")?;
                    // GPUI's RenderImage contract is BGRA, just like CEF.
                    let image = Arc::new(RenderImage::new([image::Frame::new(buffer)]));
                    *latest.lock().unwrap() = Some(Frame {
                        image,
                        width: width as f32 / scale,
                        height: height as f32 / scale,
                        y: y as f32,
                    });
                }
                event => {
                    if events.send(event).is_err() {
                        return Ok(());
                    }
                }
            }
        }
    })();
    if result.is_err() {
        let _ = child.kill();
    }
    let status = child.wait()?;
    result?;
    if !status.success() {
        return Err("Browser process stopped".into());
    }
    Ok(())
}

impl BrowserBody {
    pub fn focus(&self, window: &mut Window, cx: &mut App) {
        window.focus(&self.focus, cx);
    }

    pub fn new(
        html: String,
        images: HashMap<String, String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let (commands, receiver) = mpsc::channel();
        let (events, incoming) = mpsc::channel();
        let latest = Arc::new(Mutex::new(None));
        let output = latest.clone();
        let _ = commands.send(Command::Open {
            html,
            dark: cx.theme().is_dark(),
            images,
        });
        std::thread::spawn(move || {
            let _ = run(receiver, events.clone(), output);
            let _ = events.send(Event::Error {
                message:
                    "The embedded browser stopped. Use plain text or the separate WebKit preview."
                        .into(),
            });
        });
        cx.spawn_in(window, async move |this, cx| {
            loop {
                cx.background_executor()
                    .timer(Duration::from_millis(33))
                    .await;
                let frame = latest.lock().unwrap().take();
                let messages: Vec<_> = incoming.try_iter().collect();
                if cx
                    .update(|window, cx| {
                        this.update(cx, |this, cx| {
                            if let Some(frame) = frame {
                                if let Some(old) = this.frame.replace(frame) {
                                    cx.drop_image(old.image, Some(window));
                                }
                                cx.notify();
                            }
                            for event in messages {
                                match event {
                                    Event::Height { height }
                                        if height.is_finite()
                                            && (1.0..=1_000_000.0).contains(&height) =>
                                    {
                                        this.height = height as f32;
                                        cx.emit(Changed);
                                        cx.notify();
                                    }
                                    Event::Selection { text } => this.selection = text,
                                    Event::Link { url } => {
                                        crate::content::confirm_link(url, window, cx)
                                    }
                                    Event::Error { message } => {
                                        this.error = Some(message);
                                        cx.emit(Changed);
                                        cx.notify();
                                    }
                                    _ => {}
                                }
                            }
                        })
                    })
                    .ok()
                    .is_none_or(|result| result.is_err())
                {
                    break;
                }
            }
        })
        .detach();
        cx.on_release(|this, cx| {
            if let Some(frame) = this.frame.take() {
                cx.drop_image(frame.image, None);
            }
        })
        .detach();
        Self {
            focus: cx.focus_handle(),
            commands,
            frame: None,
            height: 180.,
            selection: String::new(),
            error: None,
            geometry: Rc::new(RefCell::new(Geometry::default())),
        }
    }

    fn mouse(&self, position: Point<Pixels>, button: bool, down: Option<bool>, count: usize) {
        let geometry = self.geometry.borrow();
        let _ = self.commands.send(Command::Mouse {
            x: f32::from(position.x - geometry.bounds.origin.x) as i32,
            y: (f32::from(position.y - geometry.bounds.origin.y) - geometry.painted_y) as i32,
            button,
            down,
            count: count as i32,
        });
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
        if self.frame.is_none() {
            return div()
                .track_focus(&self.focus)
                .w_full()
                .h(px(self.height))
                .text_sm()
                .text_color(cx.theme().muted_foreground)
                .child("Rendering HTML…")
                .into_any_element();
        }
        let geometry = self.geometry.clone();
        let commands = self.commands.clone();
        let image = self
            .frame
            .as_ref()
            .map(|f| (f.image.clone(), f.width, f.height, f.y));
        div()
            .id("embedded-mail-browser")
            .w_full()
            .h(px(self.height))
            .flex_shrink_0()
            .key_context("MailBrowser")
            .track_focus(&self.focus)
            .cursor_text()
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, event: &MouseDownEvent, w, cx| {
                    this.mouse(event.position, true, Some(true), event.click_count);
                    // Let the message card select itself, then give text/link keys
                    // to this surface without intercepting Durian's j/k, /, t, etc.
                    cx.defer_in(w, |this, w, cx| w.focus(&this.focus, cx));
                }),
            )
            .on_mouse_move(cx.listener(|this, event: &MouseMoveEvent, _, _| {
                this.mouse(
                    event.position,
                    event.pressed_button == Some(MouseButton::Left),
                    None,
                    1,
                )
            }))
            .on_mouse_up(
                MouseButton::Left,
                cx.listener(|this, event: &MouseUpEvent, _, _| {
                    this.mouse(event.position, false, Some(false), event.click_count)
                }),
            )
            .on_mouse_up_out(
                MouseButton::Left,
                cx.listener(|this, event: &MouseUpEvent, _, _| {
                    this.mouse(event.position, false, Some(false), event.click_count)
                }),
            )
            .on_action(cx.listener(|this, _: &CopySelection, _, cx| {
                if !this.selection.is_empty() {
                    cx.write_to_clipboard(ClipboardItem::new_string(this.selection.clone()));
                }
            }))
            .on_action(cx.listener(|this, _: &SelectAll, _, _| {
                let _ = this.commands.send(Command::SelectAll);
            }))
            .on_action(cx.listener(|this, _: &NextLink, _, _| {
                let _ = this.commands.send(Command::Key {
                    code: 9,
                    shift: false,
                });
            }))
            .on_action(cx.listener(|this, _: &PreviousLink, _, _| {
                let _ = this.commands.send(Command::Key {
                    code: 9,
                    shift: true,
                });
            }))
            .on_action(cx.listener(|this, _: &OpenLink, _, _| {
                let _ = this.commands.send(Command::Key {
                    code: 13,
                    shift: false,
                });
            }))
            .child(
                canvas(
                    move |bounds, window, _| {
                        let mut geometry = geometry.borrow_mut();
                        geometry.bounds = bounds;
                        let mask = window.content_mask().bounds;
                        let visible = bounds.intersect(&mask);
                        if visible.size.height > px(0.) {
                            let viewport = (
                                f32::from(bounds.size.width).ceil().clamp(1., 2048.) as u32,
                                f32::from(mask.size.height).ceil().clamp(1., 1400.) as u32,
                                window.scale_factor().clamp(1., 2.),
                                f32::from(visible.origin.y - bounds.origin.y).max(0.) as f64,
                            );
                            if geometry.viewport != Some(viewport) {
                                geometry.viewport = Some(viewport);
                                let (width, height, scale, y) = viewport;
                                let _ = commands.send(Command::Viewport {
                                    width,
                                    height,
                                    scale,
                                    y,
                                });
                            }
                        }
                        geometry.painted_y = image.as_ref().map_or(0., |f| f.3);
                        image
                    },
                    |bounds, frame, window, _| {
                        if let Some((image, width, height, y)) = frame {
                            let image_bounds = Bounds::new(
                                bounds.origin + point(px(0.), px(y)),
                                size(px(width), px(height)),
                            );
                            let _ = window.paint_image(
                                bounds,
                                image_bounds,
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
