//! Safe, deliberately non-browser mail rendering and bounded attachment previews.
use crate::data::{Attachment, Message};
use base64::Engine as _;
use gpui_kit::assets::IconName;
use gpui_kit::base::SelectableText;
use gpui_kit::component::button::{Button, ButtonVariants as _};
use gpui_kit::component::notification::Notification;
use gpui_kit::component::scroll::ScrollableElement as _;
use gpui_kit::component::text::TextView;
use gpui_kit::component::{
    ActiveTheme as _, Disableable as _, Root, Sizable as _, TitleBar, WindowExt as _, h_flex,
    v_flex,
};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;
use html5ever::tendril::TendrilSink as _;
use markup5ever_rcdom::{Handle, NodeData, RcDom};
use std::{
    collections::HashMap,
    io::{Cursor, Write},
    sync::Arc,
    time::{Duration, Instant},
};

pub struct ContentChanged;
pub struct MailContent {
    message: Message,
    live: bool,
    plain: bool,
    html: String,
    blocked_images: usize,
    inline_status: Option<String>,
    inline_images: HashMap<String, String>,
    images_loading: bool,
    browser_open: bool,
    embedded: Option<Entity<crate::browser::BrowserBody>>,
    embedded_subscription: Option<Subscription>,
    embedded_dark: bool,
}
impl EventEmitter<ContentChanged> for MailContent {}

fn escape(value: &str) -> String {
    value
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

pub fn safe_link(url: &str) -> bool {
    !url.chars().any(|c| c.is_control())
        && ["https://", "http://", "mailto:"]
            .iter()
            .any(|s| url.starts_with(s))
}

/// Emit only the native renderer's inert subset. No source attributes, CSS,
/// arbitrary data URLs, local paths or remote image URLs reach GPUI's loader.
fn sanitize(html: &str, inline: &HashMap<String, String>) -> (String, usize) {
    if html.len() > 1024 * 1024 {
        return ("<p>HTML is too large. Use plain text.</p>".into(), 0);
    }
    let dom = html5ever::parse_document(RcDom::default(), Default::default()).one(html);
    fn walk(
        node: &Handle,
        out: &mut String,
        images: &HashMap<String, String>,
        blocked: &mut usize,
        depth: usize,
    ) {
        if depth > 100 {
            return;
        }
        match &node.data {
            NodeData::Text { contents } => out.push_str(&escape(&contents.borrow())),
            NodeData::Element { name, attrs, .. } => {
                let tag = name.local.as_ref();
                if [
                    "head", "script", "style", "iframe", "object", "embed", "svg", "math", "form",
                    "input", "button", "video", "audio",
                ]
                .contains(&tag)
                {
                    return;
                }
                let attrs = attrs.borrow();
                let attr = |key: &str| {
                    attrs
                        .iter()
                        .find(|a| a.name.local.as_ref() == key)
                        .map(|a| a.value.to_string())
                        .unwrap_or_default()
                };
                if tag == "img" {
                    let src = attr("src");
                    if let Some(data) = src
                        .strip_prefix("cid:")
                        .and_then(|id| images.get(id.trim_matches(['<', '>'])))
                    {
                        out.push_str(&format!(
                            "<img src=\"{}\" width=\"420\" alt=\"{}\">",
                            data,
                            escape(&attr("alt"))
                        ));
                    } else {
                        *blocked += 1;
                    }
                    return;
                }
                let keep = [
                    "p",
                    "br",
                    "div",
                    "strong",
                    "b",
                    "em",
                    "i",
                    "u",
                    "del",
                    "s",
                    "code",
                    "pre",
                    "blockquote",
                    "ul",
                    "ol",
                    "li",
                    "h1",
                    "h2",
                    "h3",
                    "h4",
                    "h5",
                    "h6",
                    "table",
                    "thead",
                    "tbody",
                    "tr",
                    "th",
                    "td",
                    "a",
                ]
                .contains(&tag);
                if keep {
                    out.push('<');
                    out.push_str(tag);
                    if tag == "a" && safe_link(&attr("href")) {
                        out.push_str(&format!(" href=\"{}\"", escape(&attr("href"))));
                    }
                    out.push('>');
                }
                for child in node.children.borrow().iter() {
                    walk(child, out, images, blocked, depth + 1);
                }
                if keep && tag != "br" {
                    out.push_str(&format!("</{tag}>"));
                }
            }
            _ => {
                for child in node.children.borrow().iter() {
                    walk(child, out, images, blocked, depth + 1);
                }
            }
        }
    }
    let mut out = String::new();
    let mut blocked = 0;
    walk(&dom.document, &mut out, inline, &mut blocked, 0);
    (out, blocked)
}

fn load(message: &str, part: u32, live: bool) -> Result<Vec<u8>, String> {
    if live {
        crate::data::attachment(message, part)
    } else {
        crate::demo::attachment(message, part)
    }
}

/// Decode with a strict raster budget, then normalize to PNG. Never pass SVG,
/// animated media or an unbounded decompression target to the native image loader.
fn raster_png(bytes: &[u8]) -> Result<Vec<u8>, String> {
    let mut reader = image::ImageReader::new(Cursor::new(bytes))
        .with_guessed_format()
        .map_err(|_| "Unknown image format")?;
    let mut limits = image::Limits::default();
    limits.max_image_width = Some(8192);
    limits.max_image_height = Some(8192);
    limits.max_alloc = Some(64 * 1024 * 1024);
    reader.limits(limits);
    let image = reader
        .decode()
        .map_err(|_| "Image could not be decoded within the preview limits")?;
    let mut png = Cursor::new(Vec::new());
    image
        .thumbnail(1600, 1600)
        .write_to(&mut png, image::ImageFormat::Png)
        .map_err(|_| "Could not prepare image")?;
    Ok(png.into_inner())
}

impl MailContent {
    pub fn focus_browser(&self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(browser) = &self.embedded {
            browser.update(cx, |browser, cx| browser.focus(window, cx));
        }
    }

    pub fn toggle_format(&mut self, cx: &mut Context<Self>) {
        self.plain = !self.plain;
        cx.emit(ContentChanged);
        cx.notify();
    }

    pub fn open_browser(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.browser_open || self.images_loading || self.message.html.trim().is_empty() {
            return;
        }
        let images: HashMap<_, _> = self
            .inline_images
            .iter()
            .filter_map(|(id, data)| {
                data.strip_prefix("data:image/png;base64,")
                    .map(|png| (id.clone(), png.to_string()))
            })
            .collect();
        let payload = serde_json::to_vec(&serde_json::json!({
            "html": self.message.html,
            "dark": cx.theme().is_dark(),
            "images": images,
        }))
        .expect("serializable mail preview");
        if payload.len() > 32 * 1024 * 1024 {
            window.push_notification(
                Notification::error(
                    "HTML and inline images exceed the 32 MiB browser preview limit.",
                ),
                cx,
            );
            return;
        }
        self.browser_open = true;
        cx.notify();
        let task = cx.background_executor().spawn(async move {
            use std::process::{Command, Stdio};
            let helper = std::env::current_exe().map_err(|_| "Couldn’t locate the HTML viewer.")?.with_file_name("durian-mail-webview");
            let mut child = Command::new(helper).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::null()).spawn()
                .map_err(|_| "Couldn’t start the HTML viewer. Build it with run-desktop.sh; Linux needs WebKitGTK 4.1.")?;
            let write = child.stdin.take().expect("piped stdin").write_all(&payload);
            if write.is_err() {
                let _ = child.kill();
                let _ = child.wait();
                return Err("Couldn’t pass the message to the HTML viewer.");
            }
            let output = child.wait_with_output().map_err(|_| "Couldn’t wait for the HTML viewer.")?;
            if !output.status.success() {
                return Err("The HTML viewer stopped unexpectedly. The native message view is still available.");
            }
            let response: Option<serde_json::Value> = serde_json::from_slice(&output.stdout).ok();
            Ok(response.and_then(|v| v["external_link"].as_str().map(String::from)))
        });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            cx.update(|window, cx| {
                this.update(cx, |this, cx| {
                    this.browser_open = false;
                    cx.notify();
                })
                .ok();
                match result {
                    Ok(Some(url)) if safe_link(&url) => confirm_link(url, window, cx),
                    Ok(_) => {}
                    Err(error) => window.push_notification(Notification::error(error), cx),
                }
            })
            .ok();
        })
        .detach();
    }

    pub fn new(message: Message, live: bool, cx: &mut Context<Self>) -> Self {
        let (html, blocked_images) = sanitize(&message.html, &HashMap::new());
        // CID parts belong to this message and are obtained only from our local API.
        let parts: Vec<_> = message
            .attachments
            .iter()
            .filter(|a| !a.content_id.is_empty() && a.content_type.starts_with("image/"))
            .take(8)
            .cloned()
            .collect();
        let images_loading = !parts.is_empty();
        if images_loading {
            let id = message.id.clone();
            let task = cx.background_executor().spawn(async move {
                let mut images = HashMap::new();
                let mut failed = false;
                for part in parts {
                    match load(&id, part.part_id, live).and_then(|bytes| raster_png(&bytes)) {
                        Ok(png) => {
                            images.insert(
                                part.content_id.trim_matches(['<', '>']).to_string(),
                                format!(
                                    "data:image/png;base64,{}",
                                    base64::engine::general_purpose::STANDARD.encode(png)
                                ),
                            );
                        }
                        Err(_) => failed = true,
                    }
                }
                (images, failed)
            });
            cx.spawn(async move |this, cx| {
                let (images, failed) = task.await;
                this.update(cx, |this, cx| {
                    (this.html, this.blocked_images) = sanitize(&this.message.html, &images);
                    this.inline_images = images;
                    this.images_loading = false;
                    this.inline_status = failed.then(|| {
                        "Some inline images couldn’t load. Open the attachment to retry.".into()
                    });
                    cx.emit(ContentChanged);
                    cx.notify();
                })
                .ok();
            })
            .detach();
        }
        Self {
            message,
            live,
            plain: false,
            html,
            blocked_images,
            inline_status: None,
            inline_images: HashMap::new(),
            images_loading,
            browser_open: false,
            embedded: None,
            embedded_subscription: None,
            embedded_dark: false,
        }
    }
}

impl Render for MailContent {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let has_html = !self.message.html.trim().is_empty();
        if self.plain || self.embedded_dark != cx.theme().is_dark() {
            self.embedded = None;
            self.embedded_subscription = None;
        }
        if has_html && !self.plain && !self.images_loading && self.embedded.is_none() {
            let images = self.inline_images.iter().filter_map(|(id, data)| {
                data.strip_prefix("data:image/png;base64,").map(|png| (id.clone(), png.to_string()))
            }).collect();
            let browser = cx.new(|cx| crate::browser::BrowserBody::new(self.message.html.clone(), images, window, cx));
            self.embedded_subscription = Some(cx.subscribe(&browser, |_, _, _: &crate::browser::Changed, cx| {
                cx.emit(ContentChanged);
                cx.notify();
            }));
            self.embedded = Some(browser);
            self.embedded_dark = cx.theme().is_dark();
        }
        v_flex()
            .w_full()
            .gap_3()
            .when(has_html, |el| {
                el.child(
                    h_flex()
                        .gap_2()
                        .flex_wrap()
                        .child(
                            Button::new("body-format")
                                .ghost()
                                .xsmall()
                                .label(if self.plain {
                                    "Show HTML"
                                } else {
                                    "Show plain text"
                                })
                                .tooltip("Toggle HTML / plain text (v). Focus browser: Alt+V; Tab links; Esc returns.")
                                .on_click(cx.listener(|this, _, _, cx| {
                                    this.toggle_format(cx);
                                })),
                        )
                        .child(
                            Button::new("browser-preview")
                                .ghost()
                                .xsmall()
                                .label(if self.browser_open { "Preview open" } else { "Browser preview…" })
                                .tooltip("Full HTML in a separate WebKit window (Shift+V). Remote content stays blocked.")
                                .disabled(self.browser_open || self.images_loading)
                                .on_click(cx.listener(|this, _, window, cx| this.open_browser(window, cx))),
                        )
                        .child(
                            div()
                                .text_xs()
                                .text_color(cx.theme().muted_foreground)
                                .child(if self.plain {
                                    ""
                                } else {
                                    "Browser HTML · remote content blocked"
                                }),
                        ),
                )
            })
            .when(self.blocked_images > 0 && has_html && !self.plain, |el| {
                el.child(
                    div()
                        .text_xs()
                        .text_color(cx.theme().muted_foreground)
                        .child("External or unavailable images blocked"),
                )
            })
            .when_some(self.inline_status.clone(), |el, status| {
                el.child(div().text_xs().text_color(cx.theme().warning).child(status))
            })
            .child(if has_html && !self.plain {
                if let Some(browser) = &self.embedded {
                    browser.clone().into_any_element()
                } else {
                    TextView::html("mail-html", self.html.clone())
                    .selectable(true)
                    .scrollable(false)
                    .on_link_click(|url, _, window, cx| {
                        confirm_link(url.to_string(), window, cx);
                    })
                    .into_any_element()
                }
            } else {
                div()
                    .text_sm()
                    .line_height(rems(1.375))
                    .child(SelectableText::new("mail-plain", self.message.body.clone()))
                    .into_any_element()
            })
            .when(!self.message.attachments.is_empty(), |el| {
                el.child(
                    v_flex()
                        .gap_1()
                        .pt_2()
                        .border_t_1()
                        .border_color(cx.theme().border)
                        .child(
                            div()
                                .text_xs()
                                .text_color(cx.theme().muted_foreground)
                                .child(format!("{} attachments", self.message.attachments.len())),
                        )
                        .children(self.message.attachments.iter().map(|attachment| {
                            let id = self.message.id.clone();
                            let live = self.live;
                            let attachment = attachment.clone();
                            let label =
                                format!("{} · {}", attachment.filename, file_size(attachment.size));
                            Button::new(("attachment", attachment.part_id as usize))
                                .ghost()
                                .small()
                                .w_full()
                                .accessibility_label(format!("Preview {}", attachment.filename))
                                .child(
                                    h_flex()
                                        .w_full()
                                        .gap_2()
                                        .child(
                                            gpui_kit::component::Icon::new(IconName::Paperclip)
                                                .small(),
                                        )
                                        .child(div().flex_1().truncate().child(label))
                                        .child(
                                            gpui_kit::component::Icon::new(IconName::ChevronRight)
                                                .xsmall(),
                                        ),
                                )
                                .on_click(move |_, window, cx| {
                                    open_attachment(
                                        id.clone(),
                                        attachment.clone(),
                                        live,
                                        window,
                                        cx,
                                    )
                                })
                        })),
                )
            })
    }
}

pub(crate) fn confirm_link(url: String, window: &mut Window, cx: &mut App) {
    if !safe_link(&url) {
        return;
    }
    window.open_alert_dialog(cx, move |dialog, _, _| {
        let target = url.clone();
        dialog
            .title("Open external link?")
            .description(url.clone())
            .button_props(
                gpui_kit::component::dialog::DialogButtonProps::default()
                    .ok_text("Open link")
                    .cancel_text("Cancel")
                    .show_cancel(true)
                    .on_ok(move |_, _, cx| {
                        cx.open_url(&target);
                        true
                    }),
            )
    });
}

pub fn file_size(size: u64) -> String {
    if size < 1024 {
        format!("{size} B")
    } else if size < 1024 * 1024 {
        format!("{:.1} KB", size as f64 / 1024.)
    } else {
        format!("{:.1} MB", size as f64 / 1048576.)
    }
}
fn safe_filename(name: &str) -> String {
    let name: String = name
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or("")
        .trim()
        .chars()
        .filter(|c| !c.is_control())
        .take(200)
        .collect();
    if name.is_empty() || name == "." || name == ".." {
        "attachment".into()
    } else {
        name
    }
}

enum Preview {
    Image(Arc<Image>),
    Pdf { image: Arc<Image>, pages: usize },
    Text(String),
    Unsupported,
}
struct AttachmentView {
    focus: FocusHandle,
    scroll: ScrollHandle,
    message_id: String,
    attachment: Attachment,
    live: bool,
    bytes: Option<Arc<Vec<u8>>>,
    preview: Option<Preview>,
    error: Option<String>,
    page: usize,
    loading: bool,
    saving: bool,
    saved: bool,
    save_error: Option<String>,
}
actions!(
    attachments,
    [
        ClosePreview,
        NextPage,
        PreviousPage,
        SaveAttachment,
        ScrollDown,
        ScrollUp,
        ScrollPageDown,
        ScrollPageUp
    ]
);
pub fn init(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("escape", ClosePreview, Some("AttachmentPreview")),
        KeyBinding::new("right", NextPage, Some("AttachmentPreview")),
        KeyBinding::new("left", PreviousPage, Some("AttachmentPreview")),
        KeyBinding::new("down", ScrollDown, Some("AttachmentPreview")),
        KeyBinding::new("up", ScrollUp, Some("AttachmentPreview")),
        KeyBinding::new("j", ScrollDown, Some("AttachmentPreview")),
        KeyBinding::new("k", ScrollUp, Some("AttachmentPreview")),
        KeyBinding::new("pagedown", ScrollPageDown, Some("AttachmentPreview")),
        KeyBinding::new("pageup", ScrollPageUp, Some("AttachmentPreview")),
        KeyBinding::new("ctrl-s", SaveAttachment, Some("AttachmentPreview")),
        KeyBinding::new("cmd-s", SaveAttachment, Some("AttachmentPreview")),
    ]);
}

pub fn open_attachment(
    message_id: String,
    attachment: Attachment,
    live: bool,
    window: &mut Window,
    cx: &mut App,
) {
    let bounds = Bounds::centered(None, size(px(900.), px(720.)), cx);
    let result = cx.open_window(
        WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            window_min_size: Some(size(px(560.), px(400.))),
            ..TitleBar::window_options()
        },
        move |window, cx| {
            window.set_window_title(&format!("{} — Durian", attachment.filename));
            let view = cx.new(|cx| {
                let mut this = AttachmentView {
                    focus: cx.focus_handle(),
                    scroll: ScrollHandle::new(),
                    message_id,
                    attachment,
                    live,
                    bytes: None,
                    preview: None,
                    error: None,
                    page: 1,
                    loading: false,
                    saving: false,
                    saved: false,
                    save_error: None,
                };
                this.load_page(1, window, cx);
                this
            });
            window.focus(&view.read(cx).focus.clone(), cx);
            cx.new(|cx| Root::new(view, window, cx))
        },
    );
    if result.is_err() {
        window.push_notification(Notification::error("Couldn’t open attachment preview."), cx);
    }
}

fn pdf_page(bytes: &[u8], page: usize) -> Result<(Vec<u8>, usize), String> {
    let dir = tempfile::tempdir().map_err(|_| "Couldn’t create preview directory")?;
    let input = dir.path().join("attachment.pdf");
    let output = dir.path().join("page");
    std::fs::write(&input, bytes).map_err(|_| "Couldn’t prepare PDF")?;
    let metadata = dir.path().join("info");
    let mut info = std::process::Command::new("pdfinfo");
    info.arg(&input)
        .env("LC_ALL", "C")
        .stdout(std::fs::File::create(&metadata).map_err(|_| "Couldn’t read PDF metadata")?);
    run_pdf_tool(&mut info)?;
    let info = std::fs::read_to_string(metadata).map_err(|_| "Couldn’t read PDF metadata")?;
    let pages = info
        .lines()
        .find_map(|line| line.strip_prefix("Pages:")?.trim().parse::<usize>().ok())
        .ok_or("Couldn’t determine the PDF page count")?;
    if page == 0 || page > pages {
        return Err("PDF page is out of range".into());
    }
    let mut render = std::process::Command::new("pdftoppm");
    render
        .args([
            "-f",
            &page.to_string(),
            "-l",
            &page.to_string(),
            "-singlefile",
            "-scale-to",
            "1600",
            "-png",
        ])
        .arg(&input)
        .arg(&output)
        .stdout(std::process::Stdio::null());
    run_pdf_tool(&mut render)?;
    let png =
        std::fs::read(output.with_extension("png")).map_err(|_| "PDF renderer produced no page")?;
    Ok((png, pages))
}

fn run_pdf_tool(command: &mut std::process::Command) -> Result<(), String> {
    let mut child = command.stdin(std::process::Stdio::null()).stderr(std::process::Stdio::null())
        .spawn().map_err(|_| "PDF preview needs Poppler (poppler-utils on Linux; brew install poppler on macOS). You can still save the file.")?;
    let start = Instant::now();
    loop {
        if let Some(status) = child.try_wait().map_err(|_| "PDF renderer failed")? {
            if !status.success() {
                return Err("This PDF couldn’t be rendered. It may be encrypted or invalid. You can still save the file.".into());
            }
            return Ok(());
        }
        if start.elapsed() > Duration::from_secs(20) {
            let _ = child.kill();
            let _ = child.wait();
            return Err("PDF preview timed out. You can still save the file.".into());
        }
        std::thread::sleep(Duration::from_millis(30));
    }
}

fn preview(bytes: &[u8], mime: &str, page: usize) -> Result<Preview, String> {
    if mime == "application/pdf" {
        return pdf_page(bytes, page).map(|(png, pages)| Preview::Pdf {
            image: Arc::new(Image::from_bytes(ImageFormat::Png, png)),
            pages,
        });
    }
    if ["image/png", "image/jpeg", "image/gif", "image/webp"].contains(&mime) {
        return raster_png(bytes)
            .map(|png| Preview::Image(Arc::new(Image::from_bytes(ImageFormat::Png, png))));
    }
    if mime.starts_with("text/") || ["application/json", "application/xml"].contains(&mime) {
        let text = std::str::from_utf8(bytes).map_err(
            |_| "This attachment is not UTF-8 text. Save it to open with another application.",
        )?;
        let mut shown: String = text.chars().take(100_000).collect();
        if shown.len() < text.len() {
            shown.push_str("\n\n[Preview truncated. Save to read the complete file.]");
        }
        return Ok(Preview::Text(shown));
    }
    Ok(Preview::Unsupported)
}

impl AttachmentView {
    fn scroll_by(&self, amount: f32, window: &Window, cx: &mut Context<Self>) {
        let mut offset = self.scroll.offset();
        offset.y -= window.rem_size() * amount;
        self.scroll.set_offset(offset);
        cx.notify();
    }

    fn has_next_page(&self) -> bool {
        matches!(self.preview, Some(Preview::Pdf { pages, .. }) if self.page < pages)
    }

    fn load_page(&mut self, page: usize, window: &mut Window, cx: &mut Context<Self>) {
        if self.loading {
            return;
        }
        self.loading = true;
        self.error = None;
        let id = self.message_id.clone();
        let attachment = self.attachment.clone();
        let live = self.live;
        let bytes = self.bytes.clone();
        let task = cx.background_executor().spawn(async move {
            let bytes = bytes
                .map(Ok)
                .unwrap_or_else(|| load(&id, attachment.part_id, live).map(Arc::new));
            bytes.map(|bytes| {
                let result = preview(&bytes, &attachment.content_type, page);
                (bytes, result)
            })
        });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update(cx, |this, cx| {
                this.loading = false;
                match result {
                    Ok((bytes, result)) => {
                        this.bytes = Some(bytes);
                        match result {
                            Ok(preview) => {
                                this.preview = Some(preview);
                                this.page = page;
                                this.scroll.set_offset(Point::default());
                            }
                            Err(error) => this.error = Some(error),
                        }
                    }
                    Err(error) => this.error = Some(error),
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn save(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(bytes) = self.bytes.clone() else {
            return;
        };
        if self.saving {
            return;
        }
        self.saving = true;
        self.saved = false;
        self.save_error = None;
        let name = safe_filename(&self.attachment.filename);
        let home = std::env::var_os("HOME")
            .map(std::path::PathBuf::from)
            .unwrap_or_default();
        let prompt = cx.prompt_for_new_path(&home, Some(&name));
        cx.spawn_in(window, async move |this, cx| {
            let result = match prompt.await {
                Ok(Ok(Some(path))) => {
                    Some(cx.background_executor().spawn(async move {
                        let mut file = std::fs::OpenOptions::new().write(true).create_new(true).open(&path).map_err(|_| "Couldn’t save. Choose a new filename; existing files are never overwritten.")?;
                        file.write_all(&bytes).map_err(|_| "Couldn’t write attachment")
                    }).await)
                }
                Ok(Ok(None)) => None,
                _ => Some(Err("Couldn’t open the system file picker.")),
            };
            this.update(cx, |this, cx| {
                this.saving = false;
                this.saved = matches!(result, Some(Ok(())));
                if let Some(Err(error)) = result { this.save_error = Some(error.into()); }
                cx.notify();
            }).ok();
        }).detach();
        cx.notify();
    }
}
impl Render for AttachmentView {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        v_flex().size_full().key_context("AttachmentPreview").track_focus(&self.focus)
            .on_action(|_: &ClosePreview, w, _| w.remove_window())
            .on_action(cx.listener(|this, _: &NextPage, w, cx| { if this.has_next_page() { this.load_page(this.page + 1, w, cx); } }))
            .on_action(cx.listener(|this, _: &PreviousPage, w, cx| { if this.page > 1 { this.load_page(this.page - 1, w, cx); } }))
            .on_action(cx.listener(|this, _: &SaveAttachment, w, cx| this.save(w, cx)))
            .on_action(cx.listener(|this, _: &ScrollDown, w, cx| this.scroll_by(3., w, cx)))
            .on_action(cx.listener(|this, _: &ScrollUp, w, cx| this.scroll_by(-3., w, cx)))
            .on_action(cx.listener(|this, _: &ScrollPageDown, w, cx| this.scroll_by(24., w, cx)))
            .on_action(cx.listener(|this, _: &ScrollPageUp, w, cx| this.scroll_by(-24., w, cx)))
            .bg(cx.theme().background).text_color(cx.theme().foreground)
            .child(TitleBar::new().child(self.attachment.filename.clone()))
            .child(h_flex().p_3().gap_2().flex_shrink_0()
                .child(div().flex_1().text_xs().text_color(cx.theme().muted_foreground).child(format!("{} · {}", self.attachment.content_type, file_size(self.attachment.size))))
                .when(self.attachment.content_type == "application/pdf", |el| el
                    .child(Button::new("previous-page").ghost().small().label("Previous").disabled(self.page == 1 || self.loading).on_click(cx.listener(|this, _, w, cx| this.load_page(this.page - 1, w, cx))))
                    .child(div().text_sm().child(match &self.preview { Some(Preview::Pdf { pages, .. }) => format!("Page {} of {pages}", self.page), _ => format!("Page {}", self.page) }))
                    .child(Button::new("next-page").ghost().small().label("Next").disabled(self.loading || !self.has_next_page()).on_click(cx.listener(|this, _, w, cx| this.load_page(this.page + 1, w, cx)))))
                .child(Button::new("save-file").small().icon(IconName::Download).label("Save as…").disabled(self.bytes.is_none() || self.saving).on_click(cx.listener(|this, _, w, cx| this.save(w, cx)))))
            .when(self.saved, |el| el.child(div().px_4().text_sm().child("Attachment saved")))
            .when_some(self.save_error.clone(), |el, error| el.child(div().px_4().text_sm().text_color(cx.theme().danger).child(error)))
            .when(self.loading, |el| el.child(div().p_4().child("Loading preview…")))
            .when_some(self.error.clone(), |el, error| el.child(v_flex().p_4().gap_2().child(error).child(Button::new("retry-preview").small().label("Retry").on_click(cx.listener(|this, _, w, cx| this.load_page(this.page, w, cx))))))
            .child(div().relative().flex_1().min_h_0().child(v_flex().id("attachment-scroll").size_full().p_4().overflow_y_scroll().track_scroll(&self.scroll).when(!self.loading, |el| match &self.preview {
                Some(Preview::Image(image) | Preview::Pdf { image, .. }) => el.child(img(image.clone()).w_full().flex_shrink_0().object_fit(ObjectFit::Contain)),
                Some(Preview::Text(text)) => el.child(SelectableText::new("attachment-text", text.clone())),
                Some(Preview::Unsupported) => el.child("No preview for this file type. Save the attachment to open it in another application."),
                None => el,
            })).vertical_scrollbar(&self.scroll))
    }
}

#[cfg(test)]
mod tests {
    use super::{pdf_page, raster_png, safe_filename, safe_link, sanitize};
    use std::collections::HashMap;
    #[test]
    fn untrusted_html_never_passes_fetch_targets_or_active_content() {
        let (html, blocked) = sanitize(
            r#"<style>bad css</style><script>bad js</script><svg><image href="file:///etc/passwd"/></svg><p onclick="bad()">Hi &amp; Grüße <strong>team</strong><img src="https://tracker.invalid/p"><img src="data:image/svg+xml,evil"><img src="file:///etc/passwd"><a href="javascript:bad()">bad</a><a href="https://example.com?a=1&amp;b=2">safe</a></p>"#,
            &HashMap::new(),
        );
        assert_eq!(blocked, 3);
        assert!(html.contains("<strong>team</strong>"));
        for forbidden in [
            "onclick",
            "tracker",
            "file:",
            "javascript",
            "<img",
            "bad js",
            "bad css",
            "<svg",
        ] {
            assert!(!html.contains(forbidden), "{html}");
        }
        assert!(html.contains("href=\"https://example.com?a=1&amp;b=2\""));
    }
    #[test]
    fn cid_images_only_use_verified_message_parts() {
        let images =
            HashMap::from([("part@local".into(), "data:image/png;base64,verified".into())]);
        let (html, blocked) = sanitize("<img src='cid:part@local'><img src='cid:other'>", &images);
        assert!(html.contains("base64,verified"));
        assert_eq!(blocked, 1);
        assert!(!safe_link("file:///tmp/a"));
        assert!(!safe_link("https://a\n"));
        assert_eq!(safe_filename("../../a\\report.pdf"), "report.pdf");
        assert_eq!(safe_filename("\0\u{7}"), "attachment");
    }
    #[test]
    fn image_preview_decodes_real_bytes_and_rejects_garbage() {
        assert!(
            raster_png(include_bytes!("../fixtures/review.png"))
                .unwrap()
                .starts_with(b"\x89PNG")
        );
        assert!(raster_png(b"not an image").is_err());
    }
    #[test]
    #[ignore = "requires Poppler; run explicitly for the native renderer"]
    fn pdf_pages_are_distinct_and_invalid_page_fails() {
        let bytes = include_bytes!("../fixtures/review.pdf");
        let (first, pages) = pdf_page(bytes, 1).unwrap();
        let (second, _) = pdf_page(bytes, 2).unwrap();
        assert_eq!(pages, 2);
        assert!(first.starts_with(b"\x89PNG"));
        assert_ne!(first, second);
        assert!(pdf_page(bytes, 3).is_err());
    }
}
