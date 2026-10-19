//! Calendar views on GPUI, built with the `ui` crate to sit next to the mail UI: the same header
//! bar, cards, chips and selection rules. The host hands in [`model::CalendarData`] and shows a
//! [`CalendarView`]; shared motions (j/k, Esc, ⌘K) stay with the host and land here through
//! [`model::Schedule`].

mod agenda;
pub mod detail;
pub mod grid;
pub mod model;
mod month;
pub mod parse;

use chrono::{Datelike, NaiveDateTime};
use gpui::{
    App, Context, Entity, EventEmitter, FocusHandle, Focusable, IntoElement, Pixels, Render, ScrollHandle, Subscription, Task, Window, actions, div, point,
    prelude::*, px,
};
use ui::theme::{ActiveTheme, typography::*};
use ui::{IconButton, IconName, StyledExt, TabPill};

use model::{Schedule, View};

actions!(calendar, [NextPeriod, PrevPeriod, GoToToday, ShowDay, ShowWeek, ShowMonth, ShowAgenda, EditEvent, DeleteEvent, DuplicateEvent]);

/// Key context of the calendar area; bindings for the actions above use it.
pub const CONTEXT: &str = "Calendar";

/// What a right-click landed on.
#[derive(Clone, Debug)]
pub enum MenuTarget {
    Event(gpui::SharedString),
    /// empty time in the grid (snapped to the quarter hour)
    Slot(NaiveDateTime),
    /// empty all-day row cell or month cell
    Day(chrono::NaiveDate),
}

pub enum CalendarViewEvent {
    /// right-click: the host builds the menu (it knows the composer, mail, toasts)
    ContextMenu { target: MenuTarget, position: gpui::Point<Pixels> },
    /// a slot was dragged out in the grid: the host opens its event composer
    CreateRequested { start: NaiveDateTime, end: NaiveDateTime, all_day: bool },
}

pub struct CalendarView {
    schedule: Entity<Schedule>,
    focus: FocusHandle,
    scroll: ScrollHandle,
    drag: Option<grid::Drag>,
    columns: grid::ColumnBounds,
    /// text selection in the details panel (⌘C copies it)
    pub selection: ui::Selection,
    _observe: Subscription,
    _clock: Task<()>,
}

impl EventEmitter<CalendarViewEvent> for CalendarView {}

impl Focusable for CalendarView {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}

impl CalendarView {
    pub fn new(schedule: Entity<Schedule>, cx: &mut Context<Self>) -> Self {
        let scroll = ScrollHandle::new();
        // open on the working day, not at midnight
        scroll.set_offset(point(px(0.), px(-7.5 * grid::HOUR)));
        let observe = cx.observe(&schedule, |_, _, cx| cx.notify());
        // another event, another document
        cx.subscribe(&schedule, |this: &mut Self, _, event, _| {
            if let model::ScheduleEvent::SelectionChanged = event {
                this.selection.clear();
            }
        })
        .detach();
        // Also runs while mail is in front. Wake/sleep and timezone changes must
        // not leave Today or cached wall times at their launch values.
        let clock_schedule = schedule.downgrade();
        let executor = cx.background_executor().clone();
        let clock = cx.spawn(async move |_, cx| loop {
            if clock_schedule.update(cx, |s, cx| s.tick(cx)).is_err() { break; }
            executor.timer(std::time::Duration::from_secs(1)).await;
        });
        Self { schedule, focus: cx.focus_handle(), scroll, drag: None, columns: Default::default(), selection: ui::Selection::new(), _observe: observe, _clock: clock }
    }

    /// Right-click: an event gets selected first, so menu commands act on it.
    pub fn request_menu(&mut self, target: MenuTarget, position: gpui::Point<Pixels>, cx: &mut Context<Self>) {
        if let MenuTarget::Event(id) = &target {
            self.schedule.update(cx, |s, cx| s.select(Some(id.clone()), cx));
        }
        cx.emit(CalendarViewEvent::ContextMenu { target, position });
    }

    /// Right-click on empty grid time: the quarter hour under the cursor.
    pub fn request_slot_menu(&mut self, day: chrono::NaiveDate, position: gpui::Point<Pixels>, cx: &mut Context<Self>) {
        let Some(column) = self.columns.borrow().get(&day).copied() else { return };
        let at = day.and_time(chrono::NaiveTime::MIN) + chrono::Duration::minutes(grid::minute_at(position.y, column));
        self.request_menu(MenuTarget::Slot(at), position, cx);
    }

    pub fn start_drag(&mut self, day: chrono::NaiveDate, y: Pixels, all_day: bool, cx: &mut Context<Self>) {
        let Some(column) = self.columns.borrow().get(&day).copied() else { return };
        let minute = if all_day { 0 } else { grid::minute_at(y, column) };
        self.drag = Some(grid::Drag { anchor_day: day, anchor: minute, day, current: minute, all_day });
        cx.notify();
    }

    /// The mouse moved while dragging: follow it across days (x) and times (y).
    pub fn update_drag(&mut self, position: gpui::Point<Pixels>, cx: &mut Context<Self>) {
        let Some(drag) = self.drag.as_mut() else { return };
        let columns = self.columns.borrow();
        let Some(day) = grid::day_at(position.x, &columns) else { return };
        let minute = if drag.all_day { 0 } else { columns.get(&day).map(|c| grid::minute_at(position.y, *c)).unwrap_or(drag.current) };
        if (day, minute) != (drag.day, drag.current) {
            (drag.day, drag.current) = (day, minute);
            cx.notify();
        }
    }

    /// Mouse up: a real drag asks the host for a new event; a plain click only cleared the selection.
    pub fn finish_drag(&mut self, cx: &mut Context<Self>) {
        let Some(drag) = self.drag.take() else { return };
        // in the all-day row a single day is a real choice too; in the grid a plain click isn't
        if drag.moved() || drag.all_day {
            let (start, end) = drag.range();
            cx.emit(CalendarViewEvent::CreateRequested { start, end, all_day: drag.all_day });
        }
        cx.notify();
    }

    pub fn schedule(&self) -> &Entity<Schedule> {
        &self.schedule
    }

    fn title(&self, cx: &App) -> String {
        let s = self.schedule.read(cx);
        match s.view {
            View::Day => s.anchor.format("%A, %B %-d").to_string(),
            View::Week => {
                let days = s.days();
                let (first, last) = (days[0], days[days.len() - 1]);
                if first.month() == last.month() {
                    first.format("%B %Y").to_string()
                } else {
                    format!("{} – {}", first.format("%b"), last.format("%b %Y"))
                }
            }
            View::Month => s.anchor.format("%B %Y").to_string(),
            View::Agenda => "Upcoming".to_string(),
        }
    }

    fn render_header(&self, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let theme = cx.theme().clone();
        let (view, week) = {
            let s = self.schedule.read(cx);
            (s.view, s.anchor.iso_week().week())
        };
        let schedule = self.schedule.clone();
        let tab = |v: View| {
            let schedule = schedule.clone();
            TabPill::new(v.label(), v.label(), view == v).on_click(move |_, _, cx| schedule.update(cx, |s, cx| s.set_view(v, cx)))
        };
        div()
            .flex()
            .flex_none()
            .items_center()
            .h(px(44.))
            .px(px(8.))
            .gap(px(4.))
            .border_b_hairline()
            .border_color(theme.border_panel)
            .child(div().pl(px(10.)).text_size(px(SIZE_SMALL)).font_weight(WEIGHT_MEDIUM).text_color(theme.text_secondary).child(self.title(cx)))
            .when(view == View::Week, |h| h.child(div().text_size(px(SIZE_MINI)).text_color(theme.text_quaternary).child(format!("W{week}"))))
            .child(div().flex_1())
            .child(div().flex().gap(px(6.)).mr(px(8.)).children([View::Day, View::Week, View::Month, View::Agenda].map(tab)))
            .child(
                div()
                    .id("today")
                    .flex()
                    .items_center()
                    .h(px(26.))
                    .px(px(10.))
                    .rounded(theme.radius.pill)
                    .shadow(theme.raised_shadow(false))
                    .text_size(px(SIZE_MINI))
                    .font_weight(WEIGHT_MEDIUM)
                    .text_color(theme.text_secondary)
                    .hover(|s| s.bg(theme.bg_raised_hover))
                    .child("Today")
                    .on_click(cx.listener(|this, _, _, cx| this.schedule.update(cx, |s, cx| s.go_to_today(cx)))),
            )
            .child(IconButton::new("prev-period", IconName::ArrowLeft).tooltip("Previous period").on_click(cx.listener(|this, _, _, cx| this.schedule.update(cx, |s, cx| s.shift(-1, cx)))))
            .child(IconButton::new("next-period", IconName::ArrowRight).tooltip("Next period").on_click(cx.listener(|this, _, _, cx| this.schedule.update(cx, |s, cx| s.shift(1, cx)))))
    }
}

impl Render for CalendarView {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = cx.theme().clone();
        let now: NaiveDateTime = chrono::Local::now().naive_local();
        let (view, selected) = {
            let s = self.schedule.read(cx);
            (s.view, s.selected_event().cloned())
        };
        let body = match view {
            View::Day | View::Week => {
                let interaction = grid::Interaction { view: cx.entity().downgrade(), drag: self.drag, columns: self.columns.clone() };
                grid::render(&self.schedule, &self.scroll, now, interaction, &theme, cx)
            }
            View::Month => month::render(&self.schedule, cx.entity().downgrade(), &theme, cx),
            View::Agenda => agenda::render(&self.schedule, cx.entity().downgrade(), &theme, cx),
        };

        div()
            .key_context(CONTEXT)
            .track_focus(&self.focus)
            .on_action(cx.listener(|this, _: &NextPeriod, _, cx| this.schedule.update(cx, |s, cx| s.shift(1, cx))))
            .on_action(cx.listener(|this, _: &PrevPeriod, _, cx| this.schedule.update(cx, |s, cx| s.shift(-1, cx))))
            .on_action(cx.listener(|this, _: &GoToToday, _, cx| this.schedule.update(cx, |s, cx| s.go_to_today(cx))))
            .on_action(cx.listener(|this, _: &ShowDay, _, cx| this.schedule.update(cx, |s, cx| s.set_view(View::Day, cx))))
            .on_action(cx.listener(|this, _: &ShowWeek, _, cx| this.schedule.update(cx, |s, cx| s.set_view(View::Week, cx))))
            .on_action(cx.listener(|this, _: &ShowMonth, _, cx| this.schedule.update(cx, |s, cx| s.set_view(View::Month, cx))))
            .on_action(cx.listener(|this, _: &ShowAgenda, _, cx| this.schedule.update(cx, |s, cx| s.set_view(View::Agenda, cx))))
            .size_full()
            .flex()
            .flex_col()
            .child(self.render_header(cx))
            .child(
                div()
                    .flex()
                    .flex_1()
                    .min_h_0()
                    .child(div().flex_1().min_w_0().h_full().child(body))
                    .children(selected.map(|event| detail::render(&self.schedule, &event, &self.selection, &theme, cx))),
            )
    }
}
