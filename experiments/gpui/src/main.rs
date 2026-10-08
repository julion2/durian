//! Durian × GPUI — a Linux spike of the mail client UI on Zed's GPUI.
//!
//! Layout: sidebar (folders) | virtualized thread list | detail view.
//! Keys: j/k move, gg/G first/last, J/K next/prev folder, Enter open,
//!       q back to list, r reload, t toggle theme, ctrl-q quit.

mod assets;
mod data;
mod theme;

use gpui::{
    AnyElement, App, Bounds, ClickEvent, Context, FocusHandle, Focusable, KeyBinding, ScrollStrategy,
    SharedString, TitlebarOptions, UniformListScrollHandle, Window, WindowBounds, WindowOptions,
    actions, div, prelude::*, px, size, svg, uniform_list,
};
use gpui_platform::application;

use data::{FOLDERS, Thread, ThreadPreview};
use theme::Theme;

actions!(
    durian,
    [
        NextThread,
        PrevThread,
        FirstThread,
        LastThread,
        NextFolder,
        PrevFolder,
        OpenThread,
        CloseDetail,
        Reload,
        ToggleTheme,
        Quit,
    ]
);

/// Three text lines (sender, subject, preview) at line-height ≈ 1.6 × 14 px
/// plus vertical padding; `uniform_list` needs every row to be this tall.
const ROW_HEIGHT: f32 = 84.;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Pane {
    List,
    Detail,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum Source {
    Loading,
    Server,
    Seed(String),
}

struct MailApp {
    focus_handle: FocusHandle,
    theme: Theme,
    folder_ix: usize,
    threads: Vec<ThreadPreview>,
    contents: Vec<Thread>,
    selected: Option<usize>,
    pane: Pane,
    source: Source,
    list_scroll: UniformListScrollHandle,
}

impl MailApp {
    fn new(cx: &mut Context<Self>) -> Self {
        let mut this = Self {
            focus_handle: cx.focus_handle(),
            theme: Theme::light(),
            folder_ix: 0,
            threads: Vec::new(),
            contents: Vec::new(),
            selected: None,
            pane: Pane::List,
            source: Source::Loading,
            list_scroll: UniformListScrollHandle::new(),
        };
        this.load_folder(cx);
        this
    }

    // MARK: - Data loading

    fn load_folder(&mut self, cx: &mut Context<Self>) {
        let query = FOLDERS[self.folder_ix].query.to_string();
        self.source = Source::Loading;
        let task = cx
            .background_executor()
            .spawn(async move { data::search(&query, 200) });
        cx.spawn(async move |this, cx| {
            let result = task.await;
            this.update(cx, |this, cx| {
                let (rows, source) = match result {
                    Ok(rows) => (rows, Source::Server),
                    Err(err) => (data::seed(), Source::Seed(err)),
                };
                this.source = source;
                this.threads = rows.iter().map(|(p, _)| p.clone()).collect();
                this.contents = rows.into_iter().map(|(_, t)| t).collect();
                this.selected = (!this.threads.is_empty()).then_some(0);
                this.list_scroll.scroll_to_item(0, ScrollStrategy::Top);
                cx.notify();
            })
            .ok();
        })
        .detach();
    }

    // MARK: - Actions

    fn select(&mut self, ix: usize, cx: &mut Context<Self>) {
        if self.threads.is_empty() {
            return;
        }
        let ix = ix.min(self.threads.len() - 1);
        self.selected = Some(ix);
        self.threads[ix].unread = false;
        self.list_scroll.scroll_to_item(ix, ScrollStrategy::Nearest);
        cx.notify();
    }

    fn move_by(&mut self, delta: isize, cx: &mut Context<Self>) {
        let current = self.selected.unwrap_or(0) as isize;
        self.select((current + delta).max(0) as usize, cx);
    }

    fn go_folder(&mut self, delta: isize, cx: &mut Context<Self>) {
        let n = FOLDERS.len() as isize;
        self.folder_ix = ((self.folder_ix as isize + delta).rem_euclid(n)) as usize;
        self.pane = Pane::List;
        self.load_folder(cx);
        cx.notify();
    }

    fn set_folder(&mut self, ix: usize, cx: &mut Context<Self>) {
        if ix != self.folder_ix {
            self.folder_ix = ix;
            self.pane = Pane::List;
            self.load_folder(cx);
            cx.notify();
        }
    }

    fn open_thread(&mut self, cx: &mut Context<Self>) {
        if self.selected.is_some() {
            self.pane = Pane::Detail;
            cx.notify();
        }
    }

    fn close_detail(&mut self, cx: &mut Context<Self>) {
        self.pane = Pane::List;
        cx.notify();
    }

    fn toggle_theme(&mut self, cx: &mut Context<Self>) {
        self.theme = self.theme.toggled();
        cx.notify();
    }

    // MARK: - Rendering

    fn icon(path: &'static str, color: gpui::Hsla, size_px: f32) -> impl IntoElement {
        svg().path(path).size(px(size_px)).text_color(color).flex_shrink_0()
    }

    fn render_sidebar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let t = self.theme;
        div()
            .flex()
            .flex_col()
            .w(px(200.))
            .h_full()
            .flex_shrink_0()
            .bg(t.sidebar_bg)
            .border_r_1()
            .border_color(t.border)
            .child(
                // Account header
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .px_3()
                    .h(px(48.))
                    .border_b_1()
                    .border_color(t.border)
                    .child(
                        div()
                            .size(px(24.))
                            .rounded_md()
                            .bg(t.accent)
                            .flex()
                            .items_center()
                            .justify_center()
                            .text_xs()
                            .font_weight(gpui::FontWeight::BOLD)
                            .text_color(t.accent_fg)
                            .child("D"),
                    )
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .min_w_0()
                            .child(
                                div()
                                    .text_sm()
                                    .font_weight(gpui::FontWeight::SEMIBOLD)
                                    .text_color(t.text)
                                    .child("Durian"),
                            )
                            .child(div().text_xs().text_color(t.text_muted).child("julian@js-lab.org")),
                    ),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap_px()
                    .p_2()
                    .children(FOLDERS.iter().enumerate().map(|(ix, folder)| {
                        let active = ix == self.folder_ix;
                        let count = if active {
                            self.threads.iter().filter(|t| t.unread).count()
                        } else {
                            0
                        };
                        div()
                            .id(("folder", ix))
                            .flex()
                            .items_center()
                            .gap_2()
                            .h(px(30.))
                            .px_2()
                            .rounded_md()
                            .cursor_pointer()
                            .text_sm()
                            .text_color(if active { t.text } else { t.text_muted })
                            .when(active, |el| el.bg(t.selection).font_weight(gpui::FontWeight::MEDIUM))
                            .hover(|el| el.bg(if active { t.selection } else { t.hover }))
                            .on_click(cx.listener(move |this, _: &ClickEvent, _, cx| {
                                this.set_folder(ix, cx);
                            }))
                            .child(Self::icon(
                                folder.icon,
                                if active { t.accent } else { t.text_muted },
                                15.,
                            ))
                            .child(div().flex_1().child(folder.name))
                            .when(count > 0, |el| {
                                el.child(
                                    div()
                                        .text_xs()
                                        .px(px(6.))
                                        .rounded_full()
                                        .bg(t.accent)
                                        .text_color(t.accent_fg)
                                        .child(count.to_string()),
                                )
                            })
                    })),
            )
            .child(div().flex_1())
            .child(
                // Keymap cheat-sheet
                div()
                    .p_3()
                    .border_t_1()
                    .border_color(t.border)
                    .text_xs()
                    .text_color(t.text_faint)
                    .flex()
                    .flex_col()
                    .gap_1()
                    .child("j/k  move   gg/G  first/last")
                    .child("J/K  folder   ⏎  open   q  close")
                    .child("r reload   t theme   ctrl-q quit"),
            )
    }

    /// Returns `AnyElement` so the `uniform_list` closure can hand rows out
    /// without tying them to the `&self` borrow (edition-2024 RPIT captures it).
    fn render_thread_row(&self, ix: usize, cx: &mut Context<Self>) -> AnyElement {
        let t = self.theme;
        let thread = &self.threads[ix];
        let selected = self.selected == Some(ix);
        let sender = thread.sender.clone();
        let avatar_bg = theme::avatar_color(&sender);
        let weight = if thread.unread {
            gpui::FontWeight::SEMIBOLD
        } else {
            gpui::FontWeight::NORMAL
        };

        div()
            .id(("thread", ix))
            .flex()
            .gap_3()
            .px_3()
            .py_2()
            .h(px(ROW_HEIGHT))
            .w_full()
            .border_b_1()
            .border_color(t.border)
            .cursor_pointer()
            .when(selected, |el| el.bg(t.selection))
            .hover(|el| el.bg(if selected { t.selection } else { t.hover }))
            .on_click(cx.listener(move |this, event: &ClickEvent, _, cx| {
                this.select(ix, cx);
                if event.click_count() >= 2 {
                    this.open_thread(cx);
                }
            }))
            .child(
                // Avatar with initials
                div()
                    .size(px(36.))
                    .flex_shrink_0()
                    .rounded_full()
                    .bg(avatar_bg)
                    .flex()
                    .items_center()
                    .justify_center()
                    .text_xs()
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .text_color(gpui::white())
                    .child(data::initials(&sender)),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .flex_1()
                    .min_w_0()
                    .gap_px()
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_2()
                            .child(
                                div()
                                    .flex_1()
                                    .min_w_0()
                                    .truncate()
                                    .text_sm()
                                    .font_weight(weight)
                                    .text_color(t.text)
                                    .child(sender),
                            )
                            .child(div().text_xs().text_color(t.text_faint).child(thread.date.clone())),
                    )
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_1()
                            .when(thread.unread, |el| {
                                el.child(div().size(px(7.)).rounded_full().bg(t.unread_dot).flex_shrink_0())
                            })
                            .child(
                                div()
                                    .flex_1()
                                    .min_w_0()
                                    .truncate()
                                    .text_sm()
                                    .font_weight(weight)
                                    .text_color(t.text)
                                    .child(thread.subject.clone()),
                            )
                            .when(thread.message_count > 1, |el| {
                                el.child(
                                    div()
                                        .text_xs()
                                        .text_color(t.text_faint)
                                        .child(thread.message_count.to_string()),
                                )
                            }),
                    )
                    .child(
                        div()
                            .truncate()
                            .text_xs()
                            .text_color(t.text_muted)
                            .child(thread.preview.clone()),
                    ),
            )
            .into_any_element()
    }

    fn render_thread_list(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let t = self.theme;
        let folder = &FOLDERS[self.folder_ix];
        let status: SharedString = match &self.source {
            Source::Loading => "loading…".into(),
            Source::Server => format!("{} threads · durian serve", self.threads.len()).into(),
            Source::Seed(_) => format!("{} threads · seed data (server offline)", self.threads.len()).into(),
        };

        div()
            .flex()
            .flex_col()
            .w(px(340.))
            .h_full()
            .flex_shrink_0()
            .bg(t.list_bg)
            .border_r_1()
            .border_color(t.border)
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .px_3()
                    .h(px(48.))
                    .border_b_1()
                    .border_color(t.border)
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .child(
                                div()
                                    .text_sm()
                                    .font_weight(gpui::FontWeight::SEMIBOLD)
                                    .text_color(t.text)
                                    .child(folder.name),
                            )
                            .child(div().text_xs().text_color(t.text_faint).child(status)),
                    )
                    .child(
                        div()
                            .id("reload")
                            .cursor_pointer()
                            .p_1()
                            .rounded_md()
                            .hover(|el| el.bg(t.hover))
                            .on_click(cx.listener(|this, _: &ClickEvent, _, cx| this.load_folder(cx)))
                            .child(Self::icon("icons/refresh.svg", t.text_muted, 15.)),
                    ),
            )
            .child(
                uniform_list(
                    "threads",
                    self.threads.len(),
                    cx.processor(|this, range: std::ops::Range<usize>, _window, cx| {
                        range.map(|ix| this.render_thread_row(ix, cx)).collect()
                    }),
                )
                .track_scroll(&self.list_scroll)
                .flex_1(),
            )
    }

    fn render_detail(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let t = self.theme;
        let Some(ix) = self.selected.filter(|_| self.pane == Pane::Detail) else {
            let hint = if self.threads.is_empty() {
                "No threads in this folder"
            } else {
                "Select a thread and press ⏎ to open it"
            };
            return div()
                .flex_1()
                .h_full()
                .bg(t.detail_bg)
                .flex()
                .items_center()
                .justify_center()
                .text_sm()
                .text_color(t.text_faint)
                .child(hint)
                .into_any_element();
        };

        let thread = &self.contents[ix];
        let preview = &self.threads[ix];
        let tags = preview.tags.clone();

        div()
            .flex_1()
            .h_full()
            .min_w_0()
            .bg(t.detail_bg)
            .flex()
            .flex_col()
            .child(
                // Action bar
                div()
                    .flex()
                    .items_center()
                    .gap_1()
                    .px_3()
                    .h(px(48.))
                    .border_b_1()
                    .border_color(t.border)
                    .child(
                        div()
                            .id("close-detail")
                            .cursor_pointer()
                            .p_1()
                            .rounded_md()
                            .hover(|el| el.bg(t.hover))
                            .on_click(cx.listener(|this, _: &ClickEvent, _, cx| this.close_detail(cx)))
                            .child(Self::icon("icons/reply.svg", t.text_muted, 16.)),
                    )
                    .child(
                        div()
                            .id("archive")
                            .cursor_pointer()
                            .p_1()
                            .rounded_md()
                            .hover(|el| el.bg(t.hover))
                            .child(Self::icon("icons/archive.svg", t.text_muted, 16.)),
                    )
                    .child(
                        div()
                            .id("trash")
                            .cursor_pointer()
                            .p_1()
                            .rounded_md()
                            .hover(|el| el.bg(t.hover))
                            .child(Self::icon("icons/trash.svg", t.text_muted, 16.)),
                    )
                    .child(div().flex_1())
                    .child(
                        div()
                            .id("theme-toggle")
                            .cursor_pointer()
                            .p_1()
                            .rounded_md()
                            .hover(|el| el.bg(t.hover))
                            .on_click(cx.listener(|this, _: &ClickEvent, _, cx| this.toggle_theme(cx)))
                            .child(Self::icon(
                                match t.mode {
                                    theme::Mode::Light => "icons/moon.svg",
                                    theme::Mode::Dark => "icons/sun.svg",
                                },
                                t.text_muted,
                                16.,
                            )),
                    ),
            )
            .child(
                div()
                    .id("detail-scroll")
                    .flex_1()
                    .overflow_y_scroll()
                    .p_5()
                    .flex()
                    .flex_col()
                    .gap_4()
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .gap_2()
                            .child(
                                div()
                                    .text_xl()
                                    .font_weight(gpui::FontWeight::SEMIBOLD)
                                    .text_color(t.text)
                                    .child(thread.subject.clone()),
                            )
                            .child(div().flex().gap_1().children(tags.into_iter().map(|tag| {
                                div()
                                    .text_xs()
                                    .px(px(7.))
                                    .py(px(1.))
                                    .rounded_full()
                                    .bg(t.tag_bg)
                                    .text_color(t.tag_fg)
                                    .child(tag)
                            }))),
                    )
                    .children(thread.messages.iter().enumerate().map(|(m_ix, msg)| {
                        let sender = data::display_name(&msg.from);
                        let avatar_bg = theme::avatar_color(&sender);
                        div()
                            .id(("message", m_ix))
                            .flex()
                            .flex_col()
                            .gap_3()
                            .p_4()
                            .rounded_lg()
                            .bg(t.card_bg)
                            .border_1()
                            .border_color(t.border)
                            .child(
                                div()
                                    .flex()
                                    .items_center()
                                    .gap_3()
                                    .child(
                                        div()
                                            .size(px(32.))
                                            .rounded_full()
                                            .bg(avatar_bg)
                                            .flex()
                                            .items_center()
                                            .justify_center()
                                            .text_xs()
                                            .font_weight(gpui::FontWeight::SEMIBOLD)
                                            .text_color(gpui::white())
                                            .child(data::initials(&sender)),
                                    )
                                    .child(
                                        div()
                                            .flex()
                                            .flex_col()
                                            .flex_1()
                                            .min_w_0()
                                            .child(
                                                div()
                                                    .text_sm()
                                                    .font_weight(gpui::FontWeight::SEMIBOLD)
                                                    .text_color(t.text)
                                                    .child(sender),
                                            )
                                            .child(
                                                div()
                                                    .text_xs()
                                                    .text_color(t.text_muted)
                                                    .truncate()
                                                    .child(format!("to {}", msg.to)),
                                            ),
                                    )
                                    .child(div().text_xs().text_color(t.text_faint).child(msg.date.clone())),
                            )
                            .child(
                                div()
                                    .text_sm()
                                    .text_color(t.text)
                                    .line_height(px(21.))
                                    .child(msg.body.clone()),
                            )
                    })),
            )
            .into_any_element()
    }
}

impl Focusable for MailApp {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}

impl Render for MailApp {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = self.theme;
        div()
            .key_context("MailApp")
            .track_focus(&self.focus_handle)
            .on_action(cx.listener(|this, _: &NextThread, _, cx| this.move_by(1, cx)))
            .on_action(cx.listener(|this, _: &PrevThread, _, cx| this.move_by(-1, cx)))
            .on_action(cx.listener(|this, _: &FirstThread, _, cx| this.select(0, cx)))
            .on_action(cx.listener(|this, _: &LastThread, _, cx| this.select(usize::MAX, cx)))
            .on_action(cx.listener(|this, _: &NextFolder, _, cx| this.go_folder(1, cx)))
            .on_action(cx.listener(|this, _: &PrevFolder, _, cx| this.go_folder(-1, cx)))
            .on_action(cx.listener(|this, _: &OpenThread, _, cx| this.open_thread(cx)))
            .on_action(cx.listener(|this, _: &CloseDetail, _, cx| this.close_detail(cx)))
            .on_action(cx.listener(|this, _: &Reload, _, cx| this.load_folder(cx)))
            .on_action(cx.listener(|this, _: &ToggleTheme, _, cx| this.toggle_theme(cx)))
            .size_full()
            .flex()
            .bg(t.window_bg)
            .text_color(t.text)
            .child(self.render_sidebar(cx))
            .child(self.render_thread_list(cx))
            .child(self.render_detail(cx))
    }
}

fn main() {
    env_logger::init();
    application().with_assets(assets::Assets).run(|cx: &mut App| {
        cx.bind_keys([
            KeyBinding::new("j", NextThread, Some("MailApp")),
            KeyBinding::new("down", NextThread, Some("MailApp")),
            KeyBinding::new("k", PrevThread, Some("MailApp")),
            KeyBinding::new("up", PrevThread, Some("MailApp")),
            KeyBinding::new("g g", FirstThread, Some("MailApp")),
            KeyBinding::new("shift-g", LastThread, Some("MailApp")),
            KeyBinding::new("shift-j", NextFolder, Some("MailApp")),
            KeyBinding::new("shift-k", PrevFolder, Some("MailApp")),
            KeyBinding::new("enter", OpenThread, Some("MailApp")),
            KeyBinding::new("q", CloseDetail, Some("MailApp")),
            KeyBinding::new("escape", CloseDetail, Some("MailApp")),
            KeyBinding::new("r", Reload, Some("MailApp")),
            KeyBinding::new("t", ToggleTheme, Some("MailApp")),
            KeyBinding::new("ctrl-q", Quit, None),
        ]);
        cx.on_action(|_: &Quit, cx| cx.quit());

        let bounds = Bounds::centered(None, size(px(1180.), px(720.)), cx);
        let window = cx
            .open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    titlebar: Some(TitlebarOptions {
                        title: Some("Durian (GPUI)".into()),
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                |_, cx| cx.new(MailApp::new),
            )
            .unwrap();

        window
            .update(cx, |view, window, cx| {
                window.focus(&view.focus_handle(cx), cx);
                cx.activate(true);
            })
            .unwrap();
    });
}
