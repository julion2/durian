//! Standalone, bounded plain-text composer for the GPUI Kit experiment.
//!
//! This module deliberately has no sending, draft persistence, or backend
//! writes. It owns a separate native window and guards every dirty close path.

use std::collections::HashSet;
use std::ops::Range;
use std::time::Duration;

use gpui_kit::assets::IconName;
use gpui_kit::component::button::{Button, ButtonVariant, ButtonVariants as _};
use gpui_kit::component::dialog::DialogButtonProps;
use gpui_kit::component::input::{Input, InputEvent, InputState, Textarea, TextareaState};
use gpui_kit::component::menu::{DropdownMenu as _, PopupMenuItem};
use gpui_kit::component::separator::Separator;
use gpui_kit::component::{
    ActiveTheme as _, Disableable as _, Root, Sizable as _, ThemeStyled as _, TitleBar,
    WindowExt as _, h_flex, v_flex,
};
use gpui_kit::prelude::FluentBuilder as _;
use gpui_kit::*;

#[cfg(test)]
const SENDER: &str = "Julian <julian@js-lab.org>";
const SENDER_EMAIL: &str = "julian@js-lab.org";
const COMPOSE_CONTEXT: &str = "Compose";
const SUGGESTIONS_CONTEXT: &str = "ContactSuggestions";
const CONTACT_LIMIT: usize = 8;
const CONTACT_DEBOUNCE: Duration = Duration::from_millis(80);

actions!(
    durian_compose,
    [
        CloseCompose,
        FocusNextComposeField,
        FocusPreviousComposeField,
        NextContactSuggestion,
        PreviousContactSuggestion,
        AcceptContactSuggestion,
        DismissContactSuggestions
    ]
);

/// Install composer-only shortcuts. Call once after `gpui_kit::init`.
pub fn init(cx: &mut App) {
    // Textarea normally owns Tab for indentation. A mail composer needs the
    // platform form convention instead, so the more-specific context moves
    // focus through all native fields (including the body and quotation).
    cx.bind_keys([
        KeyBinding::new("tab", FocusNextComposeField, Some("Compose > Input")),
        KeyBinding::new(
            "shift-tab",
            FocusPreviousComposeField,
            Some("Compose > Input"),
        ),
    ]);
    cx.bind_keys([
        KeyBinding::new(
            "down",
            NextContactSuggestion,
            Some("Compose > ContactSuggestions > Input"),
        ),
        KeyBinding::new(
            "up",
            PreviousContactSuggestion,
            Some("Compose > ContactSuggestions > Input"),
        ),
        KeyBinding::new(
            "enter",
            AcceptContactSuggestion,
            Some("Compose > ContactSuggestions > Input"),
        ),
        KeyBinding::new(
            "tab",
            AcceptContactSuggestion,
            Some("Compose > ContactSuggestions > Input"),
        ),
        KeyBinding::new(
            "escape",
            DismissContactSuggestions,
            Some("Compose > ContactSuggestions > Input"),
        ),
    ]);

    #[cfg(target_os = "macos")]
    cx.bind_keys([KeyBinding::new(
        "cmd-w",
        CloseCompose,
        Some(COMPOSE_CONTEXT),
    )]);

    #[cfg(not(target_os = "macos"))]
    cx.bind_keys([KeyBinding::new(
        "ctrl-w",
        CloseCompose,
        Some(COMPOSE_CONTEXT),
    )]);
}

/// Open a separate native composer window.
///
/// `reply` is the thread subject and the exact message being replied to. A
/// reply starts with the original sender as recipient and keeps the source in
/// a separate read-only, selectable plain-text field.
pub fn open(
    reply: Option<(String, crate::data::Message)>,
    parent_window: &mut Window,
    cx: &mut App,
) {
    let is_reply = reply.is_some();
    let model = ComposeModel::from_reply(reply);
    let bounds = Bounds::centered(None, size(px(720.), px(680.)), cx);

    let result = cx.open_window(
        WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            window_decorations: Some(WindowDecorations::Client),
            window_background: WindowBackgroundAppearance::Opaque,
            window_min_size: Some(size(px(600.), px(620.))),
            ..TitleBar::window_options()
        },
        move |window, cx| {
            window.set_window_title(if is_reply {
                "Reply — Durian"
            } else {
                "New message — Durian"
            });
            let composer = cx.new(|cx| Composer::new(model, window, cx));
            let first_focus = if is_reply {
                composer.read(cx).body.read(cx).focus_handle(cx)
            } else {
                composer.read(cx).to.read(cx).focus_handle(cx)
            };
            window.focus(&first_focus, cx);
            cx.new(|cx| Root::new(composer, window, cx))
        },
    );

    if result.is_err() {
        parent_window.push_notification(
            gpui_kit::component::notification::Notification::error(
                "Couldn’t open the composer window.",
            ),
            cx,
        );
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct DraftValues {
    from: String,
    to: String,
    cc: String,
    bcc: String,
    subject: String,
    body: String,
}

impl DraftValues {
    fn is_dirty_from(&self, baseline: &Self) -> bool {
        self != baseline
    }
}

#[derive(Clone, Debug)]
struct ComposeModel {
    initial: DraftValues,
    quotation: Option<String>,
    reply_addresses: Option<(String, String)>,
}

impl ComposeModel {
    fn from_reply(reply: Option<(String, crate::data::Message)>) -> Self {
        let Some((subject, message)) = reply else {
            return Self {
                initial: DraftValues::default(),
                quotation: None,
                reply_addresses: None,
            };
        };

        Self {
            initial: DraftValues {
                to: reply_recipient(&message.from, &message.to, &[SENDER_EMAIL]),
                subject: reply_subject(&subject),
                ..DraftValues::default()
            },
            quotation: Some(reply_quotation(&message)),
            reply_addresses: Some((message.from, message.to)),
        }
    }
}

fn reply_subject(subject: &str) -> String {
    let subject = subject.trim();
    if subject
        .get(..3)
        .is_some_and(|prefix| prefix.eq_ignore_ascii_case("re:"))
    {
        subject.to_string()
    } else if subject.is_empty() {
        "Re:".to_string()
    } else {
        format!("Re: {subject}")
    }
}

fn address_only(mailbox: &str) -> &str {
    let mailbox = mailbox.trim();
    match (mailbox.find('<'), mailbox.rfind('>')) {
        (Some(start), Some(end)) if start < end => mailbox[start + 1..end].trim(),
        _ => mailbox,
    }
}

fn reply_recipient(from: &str, to: &str, own_addresses: &[&str]) -> String {
    if own_addresses
        .iter()
        .any(|email| address_only(from).eq_ignore_ascii_case(email))
    {
        to.trim().to_string()
    } else {
        from.trim().to_string()
    }
}

fn reply_quotation(message: &crate::data::Message) -> String {
    let lead = match (
        message.date.trim().is_empty(),
        message.from.trim().is_empty(),
    ) {
        (false, false) => format!("On {}, {} wrote:", message.date.trim(), message.from.trim()),
        (false, true) => format!("On {}:", message.date.trim()),
        (true, false) => format!("{} wrote:", message.from.trim()),
        (true, true) => "Original message:".to_string(),
    };
    let quoted = message
        .body
        .lines()
        .map(|line| {
            if line.is_empty() {
                ">".to_string()
            } else {
                format!("> {line}")
            }
        })
        .collect::<Vec<_>>()
        .join("\n");

    if quoted.is_empty() {
        lead
    } else {
        format!("{lead}\n{quoted}")
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum RecipientField {
    To,
    Cc,
    Bcc,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ActiveRecipient {
    range: Range<usize>,
    query: String,
}

/// Find the address fragment containing the caret. Commas and semicolons in a
/// quoted display name are content, not recipient separators.
fn active_recipient(value: &str, cursor: usize) -> ActiveRecipient {
    let cursor = cursor.min(value.len());
    let mut segment_start = 0;
    let mut in_quotes = false;
    let mut escaped = false;
    let mut segment = 0..value.len();

    for (offset, ch) in value.char_indices() {
        if escaped {
            escaped = false;
            continue;
        }
        if ch == '\\' && in_quotes {
            escaped = true;
            continue;
        }
        if ch == '"' {
            in_quotes = !in_quotes;
            continue;
        }
        if !in_quotes && matches!(ch, ',' | ';') {
            if cursor <= offset {
                segment = segment_start..offset;
                break;
            }
            segment_start = offset + ch.len_utf8();
            segment = segment_start..value.len();
        }
    }

    let raw = &value[segment.clone()];
    let leading = raw.len() - raw.trim_start().len();
    let trailing = raw.len() - raw.trim_end().len();
    let start = segment.start + leading;
    let end = segment.end.saturating_sub(trailing).max(start);
    ActiveRecipient {
        range: start..end,
        query: value[start..end].to_string(),
    }
}

fn recipient_segments(value: &str) -> Vec<Range<usize>> {
    let mut ranges = Vec::new();
    let mut start = 0;
    let mut in_quotes = false;
    let mut escaped = false;

    for (offset, ch) in value.char_indices() {
        if escaped {
            escaped = false;
            continue;
        }
        if ch == '\\' && in_quotes {
            escaped = true;
        } else if ch == '"' {
            in_quotes = !in_quotes;
        } else if !in_quotes && matches!(ch, ',' | ';') {
            ranges.push(start..offset);
            start = offset + ch.len_utf8();
        }
    }
    ranges.push(start..value.len());
    ranges
}

fn email_from_recipient(recipient: &str) -> Option<String> {
    let recipient = recipient.trim();
    if recipient.is_empty() {
        return None;
    }
    let email = match (recipient.rfind('<'), recipient.rfind('>')) {
        (Some(start), Some(end)) if start < end => &recipient[start + 1..end],
        _ if recipient.contains('@') => recipient,
        _ => return None,
    };
    let email = email.trim().to_lowercase();
    (!email.is_empty()).then_some(email)
}

fn selected_emails(
    values: [&str; 3],
    active_field: RecipientField,
    active_range: Range<usize>,
) -> HashSet<String> {
    let active_field = match active_field {
        RecipientField::To => 0,
        RecipientField::Cc => 1,
        RecipientField::Bcc => 2,
    };
    values
        .into_iter()
        .enumerate()
        .flat_map(|(field, value)| {
            recipient_segments(value)
                .into_iter()
                .filter_map(move |range| {
                    let is_active_fragment = field == active_field
                        && range.start <= active_range.start
                        && range.end >= active_range.end;
                    (!is_active_fragment)
                        .then(|| email_from_recipient(&value[range]))
                        .flatten()
                })
        })
        .collect()
}

fn replace_active_recipient(value: &str, cursor: usize, contact: &crate::data::Contact) -> String {
    let active = active_recipient(value, cursor);
    let display = if contact.name.trim().is_empty() {
        contact.email.trim().to_string()
    } else {
        let name = contact.name.trim();
        let name = if name.chars().any(|c| ",;<>@()[]:\\\"".contains(c)) {
            format!("\"{}\"", name.replace('\\', "\\\\").replace('"', "\\\""))
        } else {
            name.to_string()
        };
        format!("{name} <{}>", contact.email.trim())
    };
    let completes_last_fragment = active.range.end == value.trim_end().len();
    let mut result = String::with_capacity(value.len() + display.len() + 2);
    result.push_str(&value[..active.range.start]);
    result.push_str(&display);
    if completes_last_fragment {
        result.push_str(", ");
        result.push_str(value[active.range.end..].trim_start());
    } else {
        result.push_str(&value[active.range.end..]);
    }
    result
}

fn synthetic_contacts() -> Vec<crate::data::Contact> {
    [
        ("Alex Rivera", "alex.rivera@example.com"),
        ("Lisa Wang", "lisa.wang@example.com"),
        ("Sarah Chen", "sarah.chen@example.com"),
        ("James Park", "james.park@example.com"),
        ("Mara Fischer", "mara.fischer@example.com"),
    ]
    .into_iter()
    .map(|(name, email)| crate::data::Contact {
        name: name.to_string(),
        email: email.to_string(),
    })
    .collect()
}

fn filter_contacts(
    contacts: Vec<crate::data::Contact>,
    query: &str,
    excluded: &HashSet<String>,
) -> Vec<crate::data::Contact> {
    let query = query.trim().to_lowercase();
    if query.is_empty() {
        return Vec::new();
    }
    contacts
        .into_iter()
        .filter(|contact| {
            !excluded.contains(&contact.email.to_lowercase())
                && (contact.name.to_lowercase().contains(&query)
                    || contact.email.to_lowercase().contains(&query))
        })
        .take(CONTACT_LIMIT)
        .collect()
}

struct Composer {
    from: String,
    accounts: Vec<crate::accounts::Account>,
    accounts_loading: bool,
    accounts_error: Option<&'static str>,
    reply_addresses: Option<(String, String)>,
    to: Entity<InputState>,
    cc: Entity<InputState>,
    bcc: Entity<InputState>,
    subject: Entity<InputState>,
    body: Entity<TextareaState>,
    quotation: Option<Entity<TextareaState>>,
    quote_rows: usize,
    baseline: DraftValues,
    show_cc_bcc: bool,
    active_recipient: Option<RecipientField>,
    suggestions: Vec<crate::data::Contact>,
    selected_suggestion: usize,
    suggestion_error: Option<String>,
    suggestion_generation: usize,
    pending_suggestions: Option<Task<()>>,
    live: bool,
    _subscriptions: Vec<Subscription>,
}

impl Composer {
    fn new(model: ComposeModel, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let live = std::env::args().any(|argument| argument == "--live");
        let mut initial = model.initial;
        initial.from = if live {
            String::new()
        } else {
            SENDER_EMAIL.into()
        };
        // Until configured accounts arrive, never classify a live sender using
        // the demo identity. Correct an untouched reply after the async load.
        if live && let Some((from, to)) = &model.reply_addresses {
            initial.to = reply_recipient(from, to, &[]);
        }
        let to = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("Recipients")
                .default_value(initial.to.clone())
        });
        let cc = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("Carbon copy")
                .default_value(initial.cc.clone())
        });
        let bcc = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("Blind carbon copy")
                .default_value(initial.bcc.clone())
        });
        let subject = cx.new(|cx| {
            InputState::new(window, cx)
                .placeholder("Subject")
                .default_value(initial.subject.clone())
        });
        let body = cx.new(|cx| {
            TextareaState::new(window, cx)
                .rows(12)
                .placeholder("Message")
                .default_value(initial.body.clone())
        });

        let quote_rows = model
            .quotation
            .as_deref()
            .map(|quote| quote.lines().count().clamp(3, 4))
            .unwrap_or(0);
        let quotation = model.quotation.map(|quote| {
            cx.new(|cx| {
                TextareaState::new(window, cx)
                    .rows(quote_rows)
                    .default_value(quote)
            })
        });

        let mut subscriptions = Vec::with_capacity(5);
        for (field, input) in [
            (RecipientField::To, &to),
            (RecipientField::Cc, &cc),
            (RecipientField::Bcc, &bcc),
        ] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                move |this, input, event: &InputEvent, window, cx| match event {
                    InputEvent::Change | InputEvent::Focus => {
                        this.update_contact_suggestions(field, input, window, cx);
                        cx.notify();
                    }
                    InputEvent::Blur if this.active_recipient == Some(field) => {
                        this.dismiss_contact_suggestions(cx);
                    }
                    _ => {}
                },
            ));
        }
        subscriptions.push(
            cx.subscribe_in(&subject, window, |_, _, event: &InputEvent, _, cx| {
                if matches!(event, InputEvent::Change) {
                    cx.notify();
                }
            }),
        );
        subscriptions.push(
            cx.subscribe_in(&body, window, |_, _, event: &InputEvent, _, cx| {
                if matches!(event, InputEvent::Change) {
                    cx.notify();
                }
            }),
        );

        let weak = cx.weak_entity();
        window.on_window_should_close(cx, move |window, cx| {
            weak.update(cx, |this, cx| this.intercept_close(window, cx))
                .unwrap_or(true)
        });

        let mut composer = Self {
            from: initial.from.clone(),
            accounts: if live {
                Vec::new()
            } else {
                vec![crate::accounts::Account {
                    name: "Julian".into(),
                    email: SENDER_EMAIL.into(),
                }]
            },
            accounts_loading: false,
            accounts_error: None,
            reply_addresses: model.reply_addresses,
            to,
            cc,
            bcc,
            subject,
            body,
            quotation,
            quote_rows,
            baseline: initial,
            show_cc_bcc: false,
            active_recipient: None,
            suggestions: Vec::new(),
            selected_suggestion: 0,
            suggestion_error: None,
            suggestion_generation: 0,
            pending_suggestions: None,
            live,
            _subscriptions: subscriptions,
        };
        if live {
            composer.load_accounts(window, cx);
        }
        composer
    }

    fn load_accounts(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.accounts_loading || !self.live {
            return;
        }
        self.accounts_loading = true;
        self.accounts_error = None;
        let task = cx
            .background_executor()
            .spawn(async { crate::accounts::load() });
        cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update_in(cx, |this, window, cx| {
                this.accounts_loading = false;
                match result {
                    Ok(accounts) => {
                        this.accounts = accounts;
                        let first = this
                            .accounts
                            .first()
                            .map(|account| account.email.clone())
                            .unwrap_or_default();
                        if this.from.is_empty() && this.baseline.from.is_empty() {
                            this.from = first.clone();
                            this.baseline.from = first;
                        }
                        if let Some((from, to)) = &this.reply_addresses {
                            // Do not overwrite recipient edits made while Pkl ran.
                            if this.to.read(cx).value().as_ref() == this.baseline.to {
                                let emails: Vec<_> =
                                    this.accounts.iter().map(|a| a.email.as_str()).collect();
                                let recipient = reply_recipient(from, to, &emails);
                                this.baseline.to = recipient.clone();
                                // Initializing a reply is not user input: do not
                                // open contact suggestions over a focused body.
                                this.to
                                    .update(cx, |input, cx| input.set_value(recipient, window, cx));
                            }
                        }
                    }
                    Err(error) => this.accounts_error = Some(error),
                }
                cx.notify();
            })
            .ok();
        })
        .detach();
        cx.notify();
    }

    fn recipient_input(&self, field: RecipientField) -> &Entity<InputState> {
        match field {
            RecipientField::To => &self.to,
            RecipientField::Cc => &self.cc,
            RecipientField::Bcc => &self.bcc,
        }
    }

    fn update_contact_suggestions(
        &mut self,
        field: RecipientField,
        input: &Entity<InputState>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let state = input.read(cx);
        let value = state.value().to_string();
        let active = active_recipient(&value, state.selected_range().end);
        let query = active.query.trim().to_string();
        self.suggestion_generation += 1;
        self.pending_suggestions = None;
        let generation = self.suggestion_generation;
        self.active_recipient = Some(field);
        self.suggestions.clear();
        self.selected_suggestion = 0;
        self.suggestion_error = None;

        if query.chars().count() < 2 {
            cx.notify();
            return;
        }

        let values = self.values(cx);
        let excluded = selected_emails([&values.to, &values.cc, &values.bcc], field, active.range);
        if !self.live {
            self.suggestions = filter_contacts(synthetic_contacts(), &query, &excluded);
            cx.notify();
            return;
        }

        let timer = cx.background_executor().timer(CONTACT_DEBOUNCE);
        let task = cx.background_executor().spawn(async move {
            timer.await;
            crate::data::contacts(&query, CONTACT_LIMIT)
                .map(|contacts| filter_contacts(contacts, &query, &excluded))
        });
        self.pending_suggestions = Some(cx.spawn_in(window, async move |this, cx| {
            let result = task.await;
            this.update(cx, |this, cx| {
                if this.suggestion_generation != generation || this.active_recipient != Some(field)
                {
                    return;
                }
                match result {
                    Ok(contacts) => this.suggestions = contacts,
                    Err(_) => {
                        this.suggestions.clear();
                        this.suggestion_error = Some(
                            "Couldn’t load contacts. You can still enter an address manually."
                                .to_string(),
                        );
                    }
                }
                this.selected_suggestion = 0;
                cx.notify();
            })
            .ok();
        }));
        cx.notify();
    }

    fn dismiss_contact_suggestions(&mut self, cx: &mut Context<Self>) {
        self.suggestion_generation += 1;
        self.pending_suggestions = None;
        self.active_recipient = None;
        self.suggestions.clear();
        self.selected_suggestion = 0;
        self.suggestion_error = None;
        cx.notify();
    }

    fn move_contact_selection(&mut self, delta: isize, cx: &mut Context<Self>) {
        if self.suggestions.is_empty() {
            return;
        }
        self.selected_suggestion = self
            .selected_suggestion
            .saturating_add_signed(delta)
            .min(self.suggestions.len() - 1);
        cx.notify();
    }

    fn accept_contact_suggestion(
        &mut self,
        index: usize,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(field) = self.active_recipient else {
            return;
        };
        let Some(contact) = self.suggestions.get(index).cloned() else {
            self.dismiss_contact_suggestions(cx);
            window.focus_next(cx);
            return;
        };
        let input = self.recipient_input(field).clone();
        input.update(cx, |state, cx| {
            let value = state.value().to_string();
            let cursor = state.selected_range().end;
            state.replace_all(
                replace_active_recipient(&value, cursor, &contact),
                window,
                cx,
            );
            state.focus(window, cx);
        });
        self.dismiss_contact_suggestions(cx);
    }

    fn values(&self, cx: &App) -> DraftValues {
        DraftValues {
            from: self.from.clone(),
            to: self.to.read(cx).value().to_string(),
            cc: self.cc.read(cx).value().to_string(),
            bcc: self.bcc.read(cx).value().to_string(),
            subject: self.subject.read(cx).value().to_string(),
            body: self.body.read(cx).value().to_string(),
        }
    }

    fn is_dirty(&self, cx: &App) -> bool {
        self.values(cx).is_dirty_from(&self.baseline)
    }

    fn intercept_close(&mut self, window: &mut Window, cx: &mut Context<Self>) -> bool {
        if !self.is_dirty(cx) {
            return true;
        }

        self.show_discard_confirmation(window, cx);
        false
    }

    fn request_close(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.is_dirty(cx) {
            self.show_discard_confirmation(window, cx);
        } else {
            window.remove_window();
        }
    }

    fn show_discard_confirmation(&self, window: &mut Window, cx: &mut App) {
        if window.has_active_dialog(cx) {
            return;
        }

        window.open_alert_dialog(cx, |dialog, _, _| {
            dialog
                .title("Discard this message?")
                .description(
                    "This plain-text prototype does not save drafts. Your changes will be lost.",
                )
                .button_props(
                    DialogButtonProps::default()
                        .ok_text("Discard changes")
                        .ok_variant(ButtonVariant::Danger)
                        .cancel_text("Keep editing")
                        .show_cancel(true)
                        .on_ok(|_, window, cx| {
                            // Let the dialog complete its own close before removing
                            // the native window it belongs to.
                            window.defer(cx, |window, _| window.remove_window());
                            true
                        }),
                )
        });
    }

    fn render_title_bar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let dirty = self.is_dirty(cx);

        TitleBar::new()
            .on_close_window(cx.listener(|this, _, window, cx| {
                this.request_close(window, cx);
            }))
            .child(
                h_flex()
                    .h_full()
                    .items_center()
                    .text_xs()
                    .text_color(cx.theme().muted_foreground)
                    .child(if dirty {
                        "Message · Edited"
                    } else {
                        "Message"
                    }),
            )
            .child(
                h_flex().h_full().items_center().pr_2().child(
                    Button::new("compose-send")
                        .ghost()
                        .small()
                        .icon(IconName::Send)
                        .label("Send")
                        .disabled(true)
                        .tooltip("Sending is unavailable in this plain-text prototype"),
                ),
            )
    }

    fn render_contact_suggestions(
        &self,
        field: RecipientField,
        cx: &mut Context<Self>,
    ) -> Option<AnyElement> {
        if self.active_recipient != Some(field) {
            return None;
        }

        if let Some(error) = self.suggestion_error.clone() {
            return Some(
                deferred(
                    div()
                        .absolute()
                        .top(rems(2.25))
                        .left_0()
                        .w_full()
                        .max_w(rems(27.))
                        .occlude()
                        .popover_style(cx)
                        .shadow_md()
                        .px_3()
                        .py_2()
                        .text_xs()
                        .text_color(cx.theme().danger)
                        .child(error),
                )
                .into_any_element(),
            );
        }

        if self.suggestions.is_empty() {
            return None;
        }

        let mut list = v_flex()
            .id("contact-suggestions")
            .role(Role::ListBox)
            .aria_label("Contact suggestions")
            .absolute()
            .top(rems(2.25))
            .left_0()
            .w_full()
            .max_w(rems(27.))
            .occlude()
            .popover_style(cx)
            .shadow_md()
            .p_1();
        for (index, contact) in self.suggestions.iter().enumerate() {
            let selected = index == self.selected_suggestion;
            let name = if contact.name.trim().is_empty() {
                "No name".to_string()
            } else {
                contact.name.clone()
            };
            let email = contact.email.clone();
            let label = format!("{name}, {email}");
            list = list.child(
                h_flex()
                    .id(("contact-suggestion", index))
                    .role(Role::ListBoxOption)
                    .aria_label(label)
                    .aria_selected(selected)
                    .w_full()
                    .min_w_0()
                    .items_center()
                    .gap_3()
                    .rounded(cx.theme().radius)
                    .px_3()
                    .py_2()
                    .text_sm()
                    .hover(|row| row.bg(cx.theme().accent.opacity(0.8)))
                    .when(selected, |row| {
                        row.bg(cx.theme().tokens.accent)
                            .text_color(cx.theme().accent_foreground)
                    })
                    .child(crate::avatar(
                        if contact.name.trim().is_empty() {
                            &email
                        } else {
                            &name
                        },
                        false,
                        cx,
                    ))
                    .child(div().min_w_0().flex_1().truncate().child(name))
                    .child(
                        div()
                            .min_w_0()
                            .max_w(relative(0.58))
                            .truncate()
                            .text_xs()
                            .text_color(if selected {
                                cx.theme().accent_foreground.opacity(0.78)
                            } else {
                                cx.theme().muted_foreground
                            })
                            .child(email),
                    )
                    .on_mouse_down(
                        MouseButton::Left,
                        cx.listener(move |this, _, window, cx| {
                            cx.stop_propagation();
                            this.accept_contact_suggestion(index, window, cx);
                        }),
                    ),
            );
        }
        Some(deferred(list).into_any_element())
    }

    fn render_sender_row(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let ready =
            !self.accounts_loading && self.accounts_error.is_none() && !self.accounts.is_empty();
        let label = if self.accounts_loading {
            "Loading senders…".to_string()
        } else if self.accounts_error.is_some() {
            "Senders unavailable".to_string()
        } else if self.accounts.is_empty() {
            "No senders configured".to_string()
        } else {
            self.accounts
                .iter()
                .find(|account| account.email == self.from)
                .map(|account| format!("{} <{}>", account.name, account.email))
                .unwrap_or_else(|| "Select sender".into())
        };
        let trigger = Button::new("compose-from")
            .ghost()
            .small()
            .max_w_full()
            .label(label.clone())
            .tooltip("Choose sending account")
            .dropdown_caret(true)
            .loading(self.accounts_loading)
            .disabled(!ready);
        // A disabled Button must not be wrapped in an interactive Popover.
        let picker = if ready {
            let accounts = self.accounts.clone();
            let selected = self.from.clone();
            let owner = cx.weak_entity();
            trigger
                .dropdown_menu(move |menu, _, _| {
                    accounts.iter().fold(menu, |menu, account| {
                        let email = account.email.clone();
                        let owner = owner.clone();
                        menu.item(
                            PopupMenuItem::new(format!("{} <{}>", account.name, account.email))
                                .checked(email == selected)
                                .on_click(move |_, _, cx| {
                                    owner
                                        .update(cx, |this, cx| {
                                            this.from = email.clone();
                                            cx.notify();
                                        })
                                        .ok();
                                }),
                        )
                    })
                })
                .into_any_element()
        } else {
            trigger.into_any_element()
        };
        let status = self.accounts_error.or_else(|| {
            (self.live && !self.accounts_loading && self.accounts.is_empty())
                .then_some("Add an account to config.pkl, then reload.")
        });
        h_flex()
            .w_full()
            .min_h(rems(2.75))
            .gap_3()
            .items_center()
            .child(
                div()
                    .w(rems(3.125))
                    .flex_shrink_0()
                    .text_sm()
                    .font_weight(FontWeight::MEDIUM)
                    .child("From:"),
            )
            .child(
                v_flex()
                    .flex_1()
                    .min_w_0()
                    .items_start()
                    .gap_1()
                    .text_color(cx.theme().foreground)
                    .child(picker)
                    .when_some(status, |row, status| {
                        row.child(
                            div()
                                .text_xs()
                                .text_color(cx.theme().muted_foreground)
                                .child(status),
                        )
                    }),
            )
            .when(status.is_some(), |row| {
                row.child(
                    Button::new("compose-reload-senders")
                        .ghost()
                        .small()
                        .label("Reload senders")
                        .on_click(cx.listener(|this, _, window, cx| {
                            // This trigger disappears during loading. Keep focus
                            // in the form, without stealing it when Pkl finishes.
                            this.to.update(cx, |input, cx| input.focus(window, cx));
                            this.load_accounts(window, cx);
                        })),
                )
            })
    }

    fn recipient_field_row(
        &self,
        field: RecipientField,
        label: &'static str,
        input: &Entity<InputState>,
        trailing: Option<Button>,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let suggestions_open = self.active_recipient == Some(field)
            && (!self.suggestions.is_empty() || self.suggestion_error.is_some());
        let suggestions = self.render_contact_suggestions(field, cx);

        h_flex()
            .w_full()
            .min_h(rems(2.75))
            .gap_3()
            .items_center()
            .child(
                div()
                    .w(rems(3.125))
                    .flex_shrink_0()
                    .text_sm()
                    .font_weight(FontWeight::MEDIUM)
                    .child(label),
            )
            .child(
                div()
                    .relative()
                    .flex_1()
                    .min_w_0()
                    .when(suggestions_open, |field| {
                        field.key_context(SUGGESTIONS_CONTEXT)
                    })
                    .child(
                        Input::new(input)
                            .small()
                            .text_color(cx.theme().foreground)
                            .appearance(false)
                            .bordered(false)
                            .focus_bordered(false)
                            .aria_label(label),
                    )
                    .children(suggestions),
            )
            .when_some(trailing, |row, button| row.child(button))
    }

    fn render_form(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let label_color = cx.theme().muted_foreground;
        let show_cc_bcc = self.show_cc_bcc;

        v_flex()
            .w_full()
            .flex_shrink_0()
            .px_6()
            .pt_4()
            .pb_2()
            .text_color(label_color)
            .child(
                self.recipient_field_row(
                    RecipientField::To,
                    "To:",
                    &self.to,
                    Some(
                        Button::new("compose-cc-bcc")
                            .ghost()
                            .xsmall()
                            .icon(if show_cc_bcc {
                                IconName::ChevronUp
                            } else {
                                IconName::ChevronDown
                            })
                            .tooltip(if show_cc_bcc {
                                "Hide Cc and Bcc"
                            } else {
                                "Show Cc and Bcc"
                            })
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.show_cc_bcc = !this.show_cc_bcc;
                                cx.notify();
                            })),
                    ),
                    cx,
                ),
            )
            .when(show_cc_bcc, |form| {
                form.child(self.recipient_field_row(RecipientField::Cc, "Cc:", &self.cc, None, cx))
                    .child(self.recipient_field_row(
                        RecipientField::Bcc,
                        "Bcc:",
                        &self.bcc,
                        None,
                        cx,
                    ))
            })
            .child(self.render_sender_row(cx))
            .child(
                div().w_full().py_2().child(
                    Input::new(&self.subject)
                        .text_color(cx.theme().foreground)
                        .appearance(false)
                        .bordered(false)
                        .focus_bordered(false)
                        .font_weight(FontWeight::SEMIBOLD)
                        .aria_label("Subject"),
                ),
            )
            .child(Separator::horizontal())
    }

    fn render_editor(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme();

        v_flex()
            .flex_1()
            .min_h_0()
            .mx_6()
            .mb_4()
            .rounded(theme.radius_lg)
            .border_1()
            .border_color(theme.border)
            .bg(theme.background)
            .overflow_hidden()
            .child(
                div().flex_1().min_h_0().child(
                    Textarea::new(&self.body)
                        .h(relative(1.))
                        .appearance(false)
                        .bordered(false)
                        .aria_label("Message body"),
                ),
            )
            .when_some(self.quotation.as_ref(), |editor, quotation| {
                editor.child(Separator::horizontal()).child(
                    v_flex()
                        .flex_shrink_0()
                        .bg(theme.muted.opacity(0.42))
                        .px_3()
                        .pt_2()
                        .pb_3()
                        .gap_1()
                        .child(
                            div()
                                .text_xs()
                                .font_weight(FontWeight::MEDIUM)
                                .text_color(theme.muted_foreground)
                                .child("Original message · selectable plain text"),
                        )
                        .child(
                            Textarea::new(quotation)
                                .h(rems(self.quote_rows as f32 * 1.25 + 1.0))
                                .appearance(false)
                                .bordered(false)
                                .readonly(true)
                                .aria_label("Original message quotation"),
                        ),
                )
            })
    }
}

impl Render for Composer {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let dialogs = Root::render_dialog_layer(window, cx);
        let background = cx.theme().background;
        let foreground = cx.theme().foreground;
        let muted_foreground = cx.theme().muted_foreground;

        v_flex()
            .relative()
            .key_context(COMPOSE_CONTEXT)
            .on_action(cx.listener(|this, _: &CloseCompose, window, cx| {
                this.request_close(window, cx);
            }))
            .on_action(|_: &FocusNextComposeField, window, cx| {
                window.focus_next(cx);
            })
            .on_action(|_: &FocusPreviousComposeField, window, cx| {
                window.focus_prev(cx);
            })
            .on_action(cx.listener(|this, _: &NextContactSuggestion, _, cx| {
                this.move_contact_selection(1, cx);
            }))
            .on_action(cx.listener(|this, _: &PreviousContactSuggestion, _, cx| {
                this.move_contact_selection(-1, cx);
            }))
            .on_action(
                cx.listener(|this, _: &AcceptContactSuggestion, window, cx| {
                    this.accept_contact_suggestion(this.selected_suggestion, window, cx);
                }),
            )
            .on_action(cx.listener(|this, _: &DismissContactSuggestions, _, cx| {
                this.dismiss_contact_suggestions(cx);
            }))
            .size_full()
            .min_w_0()
            .min_h_0()
            .bg(background)
            .text_color(foreground)
            .child(self.render_title_bar(cx))
            .child(self.render_form(cx))
            .child(
                h_flex()
                    .h(rems(2.5))
                    .px_6()
                    .flex_shrink_0()
                    .items_center()
                    .text_xs()
                    .text_color(muted_foreground)
                    .child("Plain text prototype · sending and draft saving unavailable"),
            )
            .child(self.render_editor(cx))
            .children(dialogs)
    }
}

#[cfg(test)]
mod tests {
    use super::{
        DraftValues, RecipientField, SENDER, active_recipient, filter_contacts,
        replace_active_recipient, reply_quotation, reply_recipient, reply_subject, selected_emails,
        synthetic_contacts,
    };

    fn message(from: &str, to: &str, date: &str, body: &str) -> crate::data::Message {
        crate::data::Message {
            from: from.to_string(),
            to: to.to_string(),
            date: date.to_string(),
            body: body.to_string(),
            ..Default::default()
        }
    }

    #[test]
    fn reply_subject_adds_one_prefix() {
        assert_eq!(reply_subject("Hello"), "Re: Hello");
        assert_eq!(reply_subject("re: Hello"), "re: Hello");
        assert_eq!(reply_subject("  RE: Grüß dich  "), "RE: Grüß dich");
    }

    #[test]
    fn reply_to_own_message_uses_original_recipient() {
        let own = message(SENDER, "Mara <mara@example.com>", "", "");
        assert_eq!(
            reply_recipient(&own.from, &own.to, &["julian@js-lab.org"]),
            "Mara <mara@example.com>"
        );

        let incoming = message("Mara <mara@example.com>", SENDER, "", "");
        assert_eq!(
            reply_recipient(&incoming.from, &incoming.to, &["julian@js-lab.org"]),
            "Mara <mara@example.com>"
        );
        assert_eq!(
            reply_recipient(
                "Work <ME@work.test>",
                "mara@example.test",
                &["me@home.test", "me@work.test"]
            ),
            "mara@example.test"
        );
        assert_eq!(
            reply_recipient(SENDER, "mara@example.test", &["me@work.test"]),
            SENDER
        );
    }

    #[test]
    fn quotation_preserves_unicode_and_blank_lines() {
        let source = message(
            "Mara <mara@example.com>",
            SENDER,
            "4 Oct 2026",
            "Grüße 👋\n\nBis bald",
        );
        assert_eq!(
            reply_quotation(&source),
            "On 4 Oct 2026, Mara <mara@example.com> wrote:\n> Grüße 👋\n>\n> Bis bald"
        );
    }

    #[test]
    fn prefilled_reply_is_clean_until_an_edit() {
        let baseline = DraftValues {
            to: "mara@example.com".into(),
            subject: "Re: Hello".into(),
            ..DraftValues::default()
        };
        assert!(!baseline.is_dirty_from(&baseline));

        let mut edited = baseline.clone();
        edited.body = "Thanks".into();
        assert!(edited.is_dirty_from(&baseline));

        let mut changed_sender = baseline.clone();
        changed_sender.from = "another@example.test".into();
        assert!(changed_sender.is_dirty_from(&baseline));
    }

    #[test]
    fn replacing_active_recipient_preserves_quoted_commas_and_unicode() {
        let value = "\"Dœ, Jane\" <jane@example.com>, sá";
        let contact = crate::data::Contact {
            name: "Sára Černá".into(),
            email: "sara@example.com".into(),
        };

        assert_eq!(active_recipient(value, value.len()).query, "sá");
        assert_eq!(
            replace_active_recipient(value, value.len(), &contact),
            "\"Dœ, Jane\" <jane@example.com>, Sára Černá <sara@example.com>, "
        );
    }

    #[test]
    fn suggested_name_with_comma_remains_one_recipient() {
        let contact = crate::data::Contact {
            name: "Rivera, Alex".into(),
            email: "alex@example.com".into(),
        };
        let result = replace_active_recipient("al", 2, &contact);
        assert_eq!(result, "\"Rivera, Alex\" <alex@example.com>, ");
        let active = active_recipient(&result, result.len());
        let emails = selected_emails([&result, "", ""], RecipientField::To, active.range);
        assert_eq!(
            emails,
            std::collections::HashSet::from(["alex@example.com".into()])
        );
    }

    #[test]
    fn suggestions_deduplicate_selected_email_case_insensitively() {
        let to = "Lisa Wang <LISA.WANG@example.com>, mar";
        let cc = "Alex Rivera <alex.rivera@example.com>";
        let active = active_recipient(to, to.len());
        let excluded = selected_emails([to, cc, ""], RecipientField::To, active.range);
        let suggestions = filter_contacts(synthetic_contacts(), "a", &excluded);
        let emails = suggestions
            .iter()
            .map(|contact| contact.email.as_str())
            .collect::<Vec<_>>();

        assert!(!emails.contains(&"lisa.wang@example.com"));
        assert!(!emails.contains(&"alex.rivera@example.com"));
        assert!(emails.contains(&"mara.fischer@example.com"));
    }

    #[test]
    fn empty_contact_query_has_no_suggestions() {
        assert!(filter_contacts(synthetic_contacts(), "  ", &Default::default()).is_empty());
    }
}
