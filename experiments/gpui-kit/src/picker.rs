//! The Swift client's search/tag palettes, with selection separate from typing.
use crate::data::{Attachment, Message, Thread, ThreadPreview};
use gpui_kit::assets::IconName;
use gpui_kit::component::input::{Input, InputEvent, InputState};
use gpui_kit::component::{ActiveTheme as _, Icon, Sizable as _, WindowExt as _, h_flex, v_flex};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;
use std::{collections::BTreeSet, time::Duration};

type Mail = Vec<(ThreadPreview, Thread)>;
pub enum PickerEvent {
    Open {
        query: String,
        mail: Mail,
        selected: usize,
    },
    Tags(Vec<String>),
}
enum Mode {
    Search {
        source: Mail,
        live: bool,
    },
    Tags {
        all: BTreeSet<String>,
        selected: BTreeSet<String>,
        live: bool,
    },
    Attachments {
        message: Message,
        live: bool,
    },
}
pub struct MailPicker {
    mode: Mode,
    input: Entity<InputState>,
    results: Mail,
    tag_rows: Vec<(String, bool)>,
    attachment_rows: Vec<Attachment>,
    selected: usize,
    list: ListState,
    loading: bool,
    error: Option<String>,
    generation: usize,
    pending: Option<Task<()>>,
    _subscription: Subscription,
}
impl EventEmitter<PickerEvent> for MailPicker {}
actions!(mail_picker, [PickNext, PickPrevious, PickAccept, PickClose]);
pub fn init(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("down", PickNext, Some("MailPicker > Input")),
        KeyBinding::new("up", PickPrevious, Some("MailPicker > Input")),
        KeyBinding::new("ctrl-n", PickNext, Some("MailPicker > Input")),
        KeyBinding::new("ctrl-p", PickPrevious, Some("MailPicker > Input")),
        KeyBinding::new("enter", PickAccept, Some("MailPicker > Input")),
        KeyBinding::new("escape", PickClose, Some("MailPicker")),
    ]);
}

/// Demo search covers all folders and full plain-text bodies, not just previews.
fn matches(query: &str, preview: &ThreadPreview, thread: &Thread) -> bool {
    let text = format!(
        "{} {} {} {}",
        preview.subject,
        preview.sender,
        preview.tags.join(" "),
        thread
            .messages
            .iter()
            .map(|m| format!("{} {} {}", m.from, m.to, m.body))
            .collect::<Vec<_>>()
            .join(" ")
    )
    .to_lowercase();
    query
        .to_lowercase()
        .split_whitespace()
        .all(|word| text.contains(word))
}
fn tag_rows(all: &BTreeSet<String>, query: &str) -> Vec<(String, bool)> {
    let query = query.trim();
    let lower = query.to_lowercase();
    let mut rows: Vec<_> = all
        .iter()
        .filter(|tag| tag.to_lowercase().contains(&lower))
        .map(|tag| (tag.clone(), false))
        .collect();
    if !query.is_empty()
        && !query
            .chars()
            .any(|c| c.is_control() || c.is_whitespace() || [',', '+', '-'].contains(&c))
        && query.len() <= 64
        && !all.iter().any(|tag| tag.eq_ignore_ascii_case(query))
    {
        rows.push((query.into(), true));
    }
    rows
}
impl MailPicker {
    pub fn attachments(
        message: Message,
        live: bool,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        Self::new(Mode::Attachments { message, live }, window, cx)
    }

    pub fn search(mail: Mail, live: bool, window: &mut Window, cx: &mut Context<Self>) -> Self {
        Self::new(Mode::Search { source: mail, live }, window, cx)
    }
    pub fn tags(
        all: BTreeSet<String>,
        selected: Vec<String>,
        live: bool,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        Self::new(
            Mode::Tags {
                all,
                selected: selected.into_iter().collect(),
                live,
            },
            window,
            cx,
        )
    }
    fn new(mode: Mode, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let title = match mode {
            Mode::Search { .. } => "Search all mail…",
            Mode::Tags { live: true, .. } => "Filter tags…",
            Mode::Tags { .. } => "Filter or create a tag…",
            Mode::Attachments { .. } => "Filter attachments…",
        };
        let input = cx.new(|cx| InputState::new(window, cx).placeholder(title));
        let subscription = cx.subscribe_in(&input, window, |this, _, event, window, cx| {
            if matches!(this.mode, Mode::Search { .. }) {
                crate::diagnostics::record("search.input", || {
                    let input = this.input.read(cx);
                    serde_json::json!({
                        "event": match event {
                            InputEvent::Change => "change",
                            InputEvent::Focus => "focus",
                            InputEvent::Blur => "blur",
                            _ => "other",
                        },
                        "focused": input.focus_handle(cx).is_focused(window),
                        "chars": input.value().chars().count(),
                        "probe_matches": input.value().as_ref() == "zzdurianprobe",
                    })
                });
            }
            if matches!(event, InputEvent::Change) {
                this.refresh(window, cx);
            }
        });
        let mut this = Self {
            mode,
            input,
            results: Vec::new(),
            tag_rows: Vec::new(),
            attachment_rows: Vec::new(),
            selected: 0,
            list: ListState::new(0, ListAlignment::Top, px(200.)),
            loading: false,
            error: None,
            generation: 0,
            pending: None,
            _subscription: subscription,
        };
        this.refresh(window, cx);
        if matches!(&this.mode, Mode::Tags { live: true, .. }) {
            this.loading = true;
            let task = cx
                .background_executor()
                .spawn(async { crate::data::tags() });
            cx.spawn_in(window, async move |this, cx| {
                let result = task.await;
                this.update(cx, |this, cx| {
                    this.loading = false;
                    match result {
                        Ok(tags) => {
                            if let Mode::Tags { all, .. } = &mut this.mode {
                                all.extend(tags);
                            }
                        }
                        Err(_) => {
                            this.error =
                                Some("Couldn’t load server tags. Showing loaded tags.".into())
                        }
                    }
                    this.refilter_tags(cx);
                    cx.notify();
                })
                .ok();
            })
            .detach();
        }
        this
    }
    pub fn focus(&self, window: &mut Window, cx: &mut App) {
        self.input.update(cx, |input, cx| input.focus(window, cx));
        if matches!(self.mode, Mode::Search { .. }) {
            crate::diagnostics::record("search.focus", || serde_json::json!({
                "focused": self.input.read(cx).focus_handle(cx).is_focused(window),
                "a11y_active": window.is_a11y_active(),
            }));
        }
    }
    fn count(&self) -> usize {
        match self.mode {
            Mode::Search { .. } => self.results.len(),
            Mode::Tags { .. } => self.tag_rows.len(),
            Mode::Attachments { .. } => self.attachment_rows.len(),
        }
    }
    fn refilter_tags(&mut self, cx: &mut Context<Self>) {
        if let Mode::Tags { all, live, .. } = &self.mode {
            self.tag_rows = tag_rows(all, &self.input.read(cx).value());
            if *live {
                self.tag_rows.retain(|(_, create)| !create);
            }
            self.list.reset(self.tag_rows.len());
        }
    }
    fn refresh(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.generation += 1;
        self.pending = None;
        self.selected = 0;
        self.error = None;
        self.loading = false;
        let query = self.input.read(cx).value().trim().to_string();
        match &self.mode {
            Mode::Tags { .. } => self.refilter_tags(cx),
            Mode::Attachments { message, .. } => {
                self.attachment_rows = message
                    .attachments
                    .iter()
                    .filter(|a| a.filename.to_lowercase().contains(&query.to_lowercase()))
                    .cloned()
                    .collect();
                self.list.reset(self.attachment_rows.len());
            }
            Mode::Search {
                source,
                live: false,
            } => {
                self.results = if query.is_empty() {
                    Vec::new()
                } else {
                    source
                        .iter()
                        .filter(|(p, t)| matches(&query, p, t))
                        .take(25)
                        .cloned()
                        .collect()
                };
                self.list.reset(self.results.len());
            }
            Mode::Search { live: true, .. } => {
                self.results.clear();
                self.list.reset(0);
                if !query.is_empty() {
                    self.loading = true;
                    let generation = self.generation;
                    let timer = cx.background_executor().timer(Duration::from_millis(300));
                    let task = cx.background_executor().spawn(async move {
                        timer.await;
                        crate::data::search(&query, 25)
                    });
                    self.pending = Some(cx.spawn_in(window, async move |this, cx| {
                        let result = task.await;
                        this.update(cx, |this, cx| {
                            if this.generation != generation { return; }
                            this.loading = false;
                            match result { Ok(mail) => this.results = mail, Err(_) => this.error = Some("Couldn’t search. Check durian serve, then edit the query to retry.".into()) }
                            this.list.reset(this.results.len()); cx.notify();
                        }).ok();
                    }));
                }
            }
        }
        cx.notify();
    }
    fn navigate(&mut self, delta: isize, cx: &mut Context<Self>) {
        if self.count() == 0 {
            return;
        }
        self.selected = self
            .selected
            .saturating_add_signed(delta)
            .min(self.count() - 1);
        self.list.scroll_to_reveal_item(self.selected);
        cx.notify();
    }
    fn accept(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        match &mut self.mode {
            Mode::Attachments { message, live } => {
                if let Some(attachment) = self.attachment_rows.get(self.selected).cloned() {
                    window.close_dialog(cx);
                    crate::content::open_attachment(
                        message.id.clone(),
                        attachment,
                        *live,
                        window,
                        cx,
                    );
                }
            }
            Mode::Search { .. } if self.selected < self.results.len() => {
                cx.emit(PickerEvent::Open {
                    query: self.input.read(cx).value().to_string(),
                    mail: self.results.clone(),
                    selected: self.selected,
                });
                window.close_dialog(cx);
            }
            Mode::Tags {
                all,
                selected,
                live,
            } => {
                if *live {
                    return;
                }
                if let Some((tag, _)) = self.tag_rows.get(self.selected).cloned() {
                    all.insert(tag.clone());
                    if !selected.remove(&tag) {
                        selected.insert(tag);
                    }
                    cx.emit(PickerEvent::Tags(selected.iter().cloned().collect()));
                    self.refilter_tags(cx);
                    cx.notify();
                }
            }
            _ => {}
        }
    }
    fn row(&self, ix: usize, cx: &mut Context<Self>) -> AnyElement {
        let (title, detail, active) = match &self.mode {
            Mode::Attachments { .. } => {
                let item = &self.attachment_rows[ix];
                (
                    item.filename.clone(),
                    format!(
                        "{} · {}",
                        item.content_type,
                        crate::content::file_size(item.size)
                    ),
                    false,
                )
            }
            Mode::Search { .. } => {
                let (p, _) = &self.results[ix];
                (
                    p.subject.clone(),
                    format!("{} · {}", p.sender, p.date),
                    false,
                )
            }
            Mode::Tags { selected, .. } => {
                let (tag, create) = &self.tag_rows[ix];
                (
                    if *create {
                        format!("Create “{tag}”")
                    } else {
                        tag.clone()
                    },
                    String::new(),
                    selected.contains(tag),
                )
            }
        };
        h_flex()
            .id(("picker-result", ix))
            .role(Role::ListBoxOption)
            .aria_label(title.clone())
            .aria_selected(self.selected == ix)
            .w_full()
            .px_3()
            .py_2()
            .gap_3()
            .rounded(cx.theme().radius)
            .bg(if self.selected == ix {
                cx.theme().accent
            } else {
                cx.theme().popover
            })
            .text_color(cx.theme().popover_foreground)
            .child(
                Icon::new(if active {
                    IconName::Check
                } else {
                    match self.mode {
                        Mode::Search { .. } => IconName::Mail,
                        Mode::Attachments { .. } => IconName::Paperclip,
                        _ => IconName::Tag,
                    }
                })
                .small(),
            )
            .child(
                v_flex()
                    .flex_1()
                    .min_w_0()
                    .gap_1()
                    .child(div().text_sm().truncate().child(title))
                    .when(!detail.is_empty(), |el| {
                        el.child(
                            div()
                                .text_xs()
                                .text_color(cx.theme().muted_foreground)
                                .truncate()
                                .child(detail),
                        )
                    }),
            )
            .on_click(cx.listener(move |this, _, window, cx| {
                this.selected = ix;
                this.accept(window, cx);
            }))
            .into_any_element()
    }
}
impl Render for MailPicker {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let view = cx.entity();
        let search = matches!(self.mode, Mode::Search { .. });
        let tags = matches!(self.mode, Mode::Tags { .. });
        v_flex()
            .key_context("MailPicker")
            .w_full()
            .gap_2()
            .on_action(cx.listener(|this, _: &PickNext, _, cx| this.navigate(1, cx)))
            .on_action(cx.listener(|this, _: &PickPrevious, _, cx| this.navigate(-1, cx)))
            .on_action(cx.listener(|this, _: &PickAccept, w, cx| this.accept(w, cx)))
            .on_action(|_: &PickClose, w, cx| w.close_dialog(cx))
            .child(
                Input::new(&self.input)
                    .cleanable(true)
                    .aria_label(if search {
                        "Search all mail"
                    } else if tags {
                        "Filter tags"
                    } else {
                        "Filter attachments"
                    }),
            )
            .when(self.loading, |el| {
                el.child(div().p_3().text_sm().child("Searching…"))
            })
            .when_some(self.error.clone(), |el, error| {
                el.child(
                    div()
                        .p_3()
                        .text_sm()
                        .text_color(cx.theme().danger)
                        .child(error),
                )
            })
            .when(
                self.count() == 0 && !self.loading && self.error.is_none(),
                |el| {
                    el.child(
                        div()
                            .p_3()
                            .text_sm()
                            .text_color(cx.theme().muted_foreground)
                            .child(if self.input.read(cx).value().is_empty() && search {
                                "Search subject, sender or message text across all folders."
                            } else {
                                "No matches"
                            }),
                    )
                },
            )
            .child(
                div()
                    .h(rems(if search {
                        22.
                    } else if tags {
                        17.
                    } else {
                        self.count().clamp(1, 6) as f32 * 3.5
                    }))
                    .child(
                        list(self.list.clone(), move |ix, _, cx| {
                            view.update(cx, |this, cx| this.row(ix, cx))
                        })
                        .size_full(),
                    ),
            )
            .child(
                h_flex()
                    .pt_2()
                    .border_t_1()
                    .border_color(cx.theme().border)
                    .text_xs()
                    .text_color(cx.theme().muted_foreground)
                    .child(div().flex_1().child(format!(
                        "{} {}",
                        self.count(),
                        if search {
                            "results · max 25"
                        } else if tags {
                            "tags"
                        } else {
                            "attachments"
                        }
                    )))
                    .child(if !tags {
                        "↑↓ Navigate · Enter Open · Esc Close"
                    } else if matches!(self.mode, Mode::Tags { live: true, .. }) {
                        "↑↓ Navigate · Esc Close"
                    } else {
                        "↑↓ Navigate · Enter Toggle · Esc Close"
                    }),
            )
            .when(tags, |el| {
                el.child(
                    div()
                        .text_xs()
                        .text_color(cx.theme().muted_foreground)
                        .child(if matches!(self.mode, Mode::Tags { live: true, .. }) {
                            "Live tags are read-only in this prototype."
                        } else {
                            "Tag changes apply to sample mail for this session."
                        }),
                )
            })
    }
}

#[cfg(test)]
mod tests {
    use super::{matches, tag_rows};
    use std::collections::BTreeSet;
    #[test]
    fn search_finds_body_text_outside_the_preview_and_in_other_folders() {
        let mail = crate::demo::mail();
        let found: Vec<_> = mail
            .iter()
            .filter(|(p, t)| matches("one-way normalization", p, t))
            .map(|(p, _)| p.thread_id.as_str())
            .collect();
        assert_eq!(found, ["api-migration"]);
        assert!(!matches("one-way missingword", &mail[3].0, &mail[3].1));
        let found: Vec<_> = mail
            .iter()
            .filter(|(p, t)| matches("coach 12", p, t))
            .map(|(p, _)| p.thread_id.as_str())
            .collect();
        assert_eq!(found, ["berlin-itinerary"]);
    }
    #[test]
    fn tag_creation_avoids_duplicate_case_and_invalid_tokens() {
        let all = BTreeSet::from(["design".into(), "engineering".into()]);
        assert_eq!(tag_rows(&all, "DESIGN"), vec![("design".into(), false)]);
        assert_eq!(tag_rows(&all, "waiting"), vec![("waiting".into(), true)]);
        assert!(tag_rows(&all, "+inbox, -deleted").is_empty());
    }
}
