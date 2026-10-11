//! Multi-line, soft-wrapping text editor for the composer. Shares editing actions and key
//! bindings with [`TextInput`](crate::TextInput) (its key context includes `TextInput`) and adds
//! vertical movement, newlines and a submit action. Height follows the wrapped content, so the
//! composer grows as you type.

use std::cell::RefCell;
use std::ops::Range;
use std::rc::Rc;

use gpui::{
    App, Bounds, ClipboardItem, Context, CursorStyle, ElementId, ElementInputHandler, Entity, EntityInputHandler,
    EventEmitter, FocusHandle, Focusable, GlobalElementId, KeyBinding, LayoutId, MouseButton, MouseDownEvent,
    MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point, SharedString, Style, TextRun, UTF16Selection, Window,
    WrappedLine, actions, div, fill, point, prelude::*, px, relative, size,
};
use unicode_segmentation::*;

use crate::theme::ActiveTheme;
use crate::{Backspace, Copy, Cut, Delete, End, Home, Left, Paste, Right, SelectAll, SelectLeft, SelectRight};

actions!(text_area, [Up, Down, SelectUp, SelectDown, Newline, Submit, Cancel, Bold, Italic, InlineCode]);

const CONTEXT: &str = "TextArea";

pub fn bind_text_area_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("up", Up, Some(CONTEXT)),
        KeyBinding::new("down", Down, Some(CONTEXT)),
        KeyBinding::new("shift-up", SelectUp, Some(CONTEXT)),
        KeyBinding::new("shift-down", SelectDown, Some(CONTEXT)),
        KeyBinding::new("enter", Newline, Some(CONTEXT)),
        KeyBinding::new("shift-enter", Newline, Some(CONTEXT)),
        KeyBinding::new("cmd-enter", Submit, Some(CONTEXT)),
        KeyBinding::new("escape", Cancel, Some(CONTEXT)),
        KeyBinding::new("cmd-b", Bold, Some(CONTEXT)),
        KeyBinding::new("cmd-i", Italic, Some(CONTEXT)),
        KeyBinding::new("cmd-e", InlineCode, Some(CONTEXT)),
    ]);
}

pub enum TextAreaEvent {
    Changed,
    Submit,
    Cancel,
}

/// One shaped paragraph (text between newlines) and where it sits.
struct Paragraph {
    line: WrappedLine,
    start: usize,
    top: Pixels,
}

#[derive(Default)]
struct Layout {
    paragraphs: Vec<Paragraph>,
    line_height: Pixels,
    bounds: Option<Bounds<Pixels>>,
    /// Live Markdown: the shown text differs from the source; these translate offsets.
    /// Empty = identity.
    to_display: Vec<usize>,
    to_source: Vec<usize>,
}

impl Layout {
    fn display(&self, source: usize) -> usize {
        self.to_display.get(source).copied().unwrap_or(source)
    }
    fn source(&self, display: usize) -> usize {
        self.to_source.get(display).copied().unwrap_or(display)
    }
}

pub struct TextArea {
    focus_handle: FocusHandle,
    content: SharedString,
    placeholder: SharedString,
    selected_range: Range<usize>,
    selection_reversed: bool,
    marked_range: Option<Range<usize>>,
    min_lines: usize,
    layout: Rc<RefCell<Layout>>,
    is_selecting: bool,
    /// Column to return to when moving up/down through shorter lines.
    goal_x: Option<Pixels>,
    /// Live Markdown styling + formatting shortcuts.
    markdown: bool,
}

impl EventEmitter<TextAreaEvent> for TextArea {}

impl TextArea {
    pub fn new(placeholder: impl Into<SharedString>, min_lines: usize, cx: &mut Context<Self>) -> Self {
        Self {
            focus_handle: cx.focus_handle(),
            content: SharedString::default(),
            placeholder: placeholder.into(),
            selected_range: 0..0,
            selection_reversed: false,
            marked_range: None,
            min_lines,
            layout: Rc::default(),
            is_selecting: false,
            goal_x: None,
            markdown: false,
        }
    }

    pub fn markdown(mut self, enabled: bool) -> Self {
        self.markdown = enabled;
        self
    }

    /// Cmd-B / Cmd-I / Cmd-E: wrap the selection in `marker`, or unwrap if it's already wrapped.
    /// With no selection, insert a pair and put the cursor between.
    pub fn toggle_wrap(&mut self, marker: &str, cx: &mut Context<Self>) {
        let range = self.selected_range.clone();
        let (m, text) = (marker.len(), self.content.to_string());
        let wrapped = range.start >= m && text[range.end..].starts_with(marker) && text[..range.start].ends_with(marker);
        let (new, selection) = if wrapped {
            let new = format!("{}{}{}", &text[..range.start - m], &text[range.clone()], &text[range.end + m..]);
            (new, range.start - m..range.end - m)
        } else {
            let new = format!("{}{marker}{}{marker}{}", &text[..range.start], &text[range.clone()], &text[range.end..]);
            (new, range.start + m..range.end + m)
        };
        self.content = new.into();
        self.selected_range = selection;
        cx.emit(TextAreaEvent::Changed);
        cx.notify();
    }

    /// Toolbar: toggle a block prefix ("- ", "> ", "1. ") on the cursor's line.
    pub fn toggle_line_prefix(&mut self, prefix: &str, cx: &mut Context<Self>) {
        let cursor = self.cursor_offset();
        let start = self.content[..cursor].rfind('\n').map_or(0, |i| i + 1);
        let text = self.content.to_string();
        let (new, delta): (String, isize) = if text[start..].starts_with(prefix) {
            (format!("{}{}", &text[..start], &text[start + prefix.len()..]), -(prefix.len() as isize))
        } else {
            (format!("{}{prefix}{}", &text[..start], &text[start..]), prefix.len() as isize)
        };
        self.content = new.into();
        let at = (cursor as isize + delta).max(start as isize) as usize;
        self.selected_range = at..at;
        cx.emit(TextAreaEvent::Changed);
        cx.notify();
    }

    pub fn text(&self) -> &SharedString {
        &self.content
    }

    pub fn set_text(&mut self, text: impl Into<SharedString>, cx: &mut Context<Self>) {
        self.content = text.into();
        self.selected_range = self.content.len()..self.content.len();
        cx.emit(TextAreaEvent::Changed);
        cx.notify();
    }

    pub fn is_empty(&self) -> bool {
        self.content.trim().is_empty()
    }

    fn cursor_offset(&self) -> usize {
        if self.selection_reversed { self.selected_range.start } else { self.selected_range.end }
    }

    fn move_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        self.selected_range = offset..offset;
        self.selection_reversed = false;
        cx.notify();
    }

    fn select_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        if self.selection_reversed {
            self.selected_range.start = offset
        } else {
            self.selected_range.end = offset
        }
        if self.selected_range.end < self.selected_range.start {
            self.selection_reversed = !self.selection_reversed;
            self.selected_range = self.selected_range.end..self.selected_range.start;
        }
        cx.notify();
    }

    fn previous_boundary(&self, offset: usize) -> usize {
        self.content.grapheme_indices(true).rev().find_map(|(ix, _)| (ix < offset).then_some(ix)).unwrap_or(0)
    }

    fn next_boundary(&self, offset: usize) -> usize {
        self.content.grapheme_indices(true).find_map(|(ix, _)| (ix > offset).then_some(ix)).unwrap_or(self.content.len())
    }

    /// Position of a byte offset relative to the text origin.
    fn position_for(&self, offset: usize) -> Option<Point<Pixels>> {
        let layout = self.layout.borrow();
        let offset = layout.display(offset);
        let para = layout.paragraphs.iter().rev().find(|p| p.start <= offset)?;
        let local = para.line.position_for_index(offset - para.start, layout.line_height)?;
        Some(point(local.x, local.y + para.top))
    }

    /// Byte offset closest to a point relative to the text origin.
    fn offset_for(&self, position: Point<Pixels>) -> usize {
        let layout = self.layout.borrow();
        let Some(para) = layout.paragraphs.iter().rev().find(|p| p.top <= position.y).or(layout.paragraphs.first()) else {
            return 0;
        };
        let local = point(position.x, (position.y - para.top).max(px(0.)));
        let ix = para.line.closest_index_for_position(local, layout.line_height).unwrap_or_else(|ix| ix);
        layout.source(para.start + ix).min(self.content.len())
    }

    fn vertical(&mut self, lines: f32, select: bool, cx: &mut Context<Self>) {
        let line_height = self.layout.borrow().line_height;
        let Some(pos) = self.position_for(self.cursor_offset()) else { return };
        let x = *self.goal_x.get_or_insert(pos.x);
        let target = point(x, pos.y + line_height * lines + line_height / 2.);
        let offset = if target.y < px(0.) {
            0
        } else {
            self.offset_for(target)
        };
        let goal = self.goal_x;
        if select { self.select_to(offset, cx) } else { self.move_to(offset, cx) }
        self.goal_x = goal;
    }

    fn up(&mut self, _: &Up, _: &mut Window, cx: &mut Context<Self>) {
        self.vertical(-1., false, cx);
    }
    fn down(&mut self, _: &Down, _: &mut Window, cx: &mut Context<Self>) {
        self.vertical(1., false, cx);
    }
    fn select_up(&mut self, _: &SelectUp, _: &mut Window, cx: &mut Context<Self>) {
        self.vertical(-1., true, cx);
    }
    fn select_down(&mut self, _: &SelectDown, _: &mut Window, cx: &mut Context<Self>) {
        self.vertical(1., true, cx);
    }

    fn left(&mut self, _: &Left, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        if self.selected_range.is_empty() {
            self.move_to(self.previous_boundary(self.cursor_offset()), cx);
        } else {
            self.move_to(self.selected_range.start, cx)
        }
    }
    fn right(&mut self, _: &Right, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        if self.selected_range.is_empty() {
            self.move_to(self.next_boundary(self.selected_range.end), cx);
        } else {
            self.move_to(self.selected_range.end, cx)
        }
    }
    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.previous_boundary(self.cursor_offset()), cx);
    }
    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.next_boundary(self.cursor_offset()), cx);
    }
    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx);
        self.select_to(self.content.len(), cx)
    }
    /// Home/End work on the current paragraph.
    fn home(&mut self, _: &Home, _: &mut Window, cx: &mut Context<Self>) {
        let start = self.content[..self.cursor_offset()].rfind('\n').map_or(0, |i| i + 1);
        self.move_to(start, cx);
    }
    fn end(&mut self, _: &End, _: &mut Window, cx: &mut Context<Self>) {
        let cursor = self.cursor_offset();
        let end = self.content[cursor..].find('\n').map_or(self.content.len(), |i| cursor + i);
        self.move_to(end, cx);
    }
    fn backspace(&mut self, _: &Backspace, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            self.select_to(self.previous_boundary(self.cursor_offset()), cx)
        }
        self.replace_text_in_range(None, "", window, cx)
    }
    fn delete(&mut self, _: &Delete, window: &mut Window, cx: &mut Context<Self>) {
        if self.selected_range.is_empty() {
            self.select_to(self.next_boundary(self.cursor_offset()), cx)
        }
        self.replace_text_in_range(None, "", window, cx)
    }
    fn newline(&mut self, _: &Newline, window: &mut Window, cx: &mut Context<Self>) {
        if self.markdown && self.selected_range.is_empty() {
            // lists and quotes continue; Enter on an empty item ends the list
            let cursor = self.cursor_offset();
            let start = self.content[..cursor].rfind('\n').map_or(0, |i| i + 1);
            let line = self.content[start..cursor].to_string();
            let digits = line.find(|c: char| !c.is_ascii_digit()).unwrap_or(0);
            let prefix = if let Some(p) = ["- ", "* ", "> "].into_iter().find(|p| line.starts_with(p)) {
                Some((p.to_string(), p.to_string()))
            } else if digits > 0 && line[digits..].starts_with(". ") {
                let n: usize = line[..digits].parse().unwrap_or(0);
                Some((format!("{n}. "), format!("{}. ", n + 1)))
            } else {
                None
            };
            if let Some((current, next)) = prefix {
                if line == current {
                    self.selected_range = start..cursor;
                    return self.replace_text_in_range(None, "", window, cx);
                }
                return self.replace_text_in_range(None, &format!("\n{next}"), window, cx);
            }
        }
        self.replace_text_in_range(None, "\n", window, cx)
    }
    fn bold(&mut self, _: &Bold, _: &mut Window, cx: &mut Context<Self>) {
        if self.markdown {
            self.toggle_wrap("**", cx);
        }
    }
    fn italic(&mut self, _: &Italic, _: &mut Window, cx: &mut Context<Self>) {
        if self.markdown {
            self.toggle_wrap("_", cx);
        }
    }
    fn inline_code(&mut self, _: &InlineCode, _: &mut Window, cx: &mut Context<Self>) {
        if self.markdown {
            self.toggle_wrap("`", cx);
        }
    }
    fn submit(&mut self, _: &Submit, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(TextAreaEvent::Submit);
    }
    fn cancel(&mut self, _: &Cancel, _: &mut Window, cx: &mut Context<Self>) {
        cx.emit(TextAreaEvent::Cancel);
    }
    fn paste(&mut self, _: &Paste, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(text) = cx.read_from_clipboard().and_then(|item| item.text()) {
            self.replace_text_in_range(None, &text, window, cx);
        }
    }
    fn copy(&mut self, _: &Copy, _: &mut Window, cx: &mut Context<Self>) {
        if !self.selected_range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[self.selected_range.clone()].to_string()));
        }
    }
    fn cut(&mut self, _: &Cut, window: &mut Window, cx: &mut Context<Self>) {
        if !self.selected_range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.content[self.selected_range.clone()].to_string()));
            self.replace_text_in_range(None, "", window, cx)
        }
    }

    fn text_origin(&self) -> Option<Point<Pixels>> {
        self.layout.borrow().bounds.map(|b| b.origin)
    }

    fn on_mouse_down(&mut self, event: &MouseDownEvent, window: &mut Window, cx: &mut Context<Self>) {
        window.focus(&self.focus_handle, cx);
        self.is_selecting = true;
        self.goal_x = None;
        let Some(origin) = self.text_origin() else { return };
        let offset = self.offset_for(event.position - origin);
        if event.modifiers.shift { self.select_to(offset, cx) } else { self.move_to(offset, cx) }
    }
    fn on_mouse_up(&mut self, _: &MouseUpEvent, _: &mut Window, _: &mut Context<Self>) {
        self.is_selecting = false;
    }
    fn on_mouse_move(&mut self, event: &MouseMoveEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.is_selecting
            && let Some(origin) = self.text_origin()
        {
            self.select_to(self.offset_for(event.position - origin), cx);
        }
    }

    // UTF-16 <-> UTF-8 conversions for the platform input handler (IME)
    fn offset_from_utf16(&self, offset: usize) -> usize {
        let (mut utf8, mut utf16) = (0, 0);
        for ch in self.content.chars() {
            if utf16 >= offset {
                break;
            }
            utf16 += ch.len_utf16();
            utf8 += ch.len_utf8();
        }
        utf8
    }
    fn offset_to_utf16(&self, offset: usize) -> usize {
        let (mut utf16, mut utf8) = (0, 0);
        for ch in self.content.chars() {
            if utf8 >= offset {
                break;
            }
            utf8 += ch.len_utf8();
            utf16 += ch.len_utf16();
        }
        utf16
    }
    fn range_to_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_to_utf16(range.start)..self.offset_to_utf16(range.end)
    }
    fn range_from_utf16(&self, range: &Range<usize>) -> Range<usize> {
        self.offset_from_utf16(range.start)..self.offset_from_utf16(range.end)
    }
}

impl EntityInputHandler for TextArea {
    fn text_for_range(&mut self, range_utf16: Range<usize>, actual: &mut Option<Range<usize>>, _: &mut Window, _: &mut Context<Self>) -> Option<String> {
        let range = self.range_from_utf16(&range_utf16);
        actual.replace(self.range_to_utf16(&range));
        Some(self.content[range].to_string())
    }

    fn selected_text_range(&mut self, _: bool, _: &mut Window, _: &mut Context<Self>) -> Option<UTF16Selection> {
        Some(UTF16Selection { range: self.range_to_utf16(&self.selected_range), reversed: self.selection_reversed })
    }

    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.marked_range.as_ref().map(|r| self.range_to_utf16(r))
    }

    fn unmark_text(&mut self, _: &mut Window, _: &mut Context<Self>) {
        self.marked_range = None;
    }

    fn replace_text_in_range(&mut self, range_utf16: Option<Range<usize>>, new_text: &str, _: &mut Window, cx: &mut Context<Self>) {
        let range = range_utf16
            .as_ref()
            .map(|r| self.range_from_utf16(r))
            .or(self.marked_range.clone())
            .unwrap_or(self.selected_range.clone());
        self.content = (self.content[..range.start].to_owned() + new_text + &self.content[range.end..]).into();
        self.selected_range = range.start + new_text.len()..range.start + new_text.len();
        self.selection_reversed = false;
        self.marked_range = None;
        self.goal_x = None;
        cx.emit(TextAreaEvent::Changed);
        cx.notify();
    }

    fn replace_and_mark_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        new_selected_range_utf16: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let range = range_utf16
            .as_ref()
            .map(|r| self.range_from_utf16(r))
            .or(self.marked_range.clone())
            .unwrap_or(self.selected_range.clone());
        self.content = (self.content[..range.start].to_owned() + new_text + &self.content[range.end..]).into();
        self.marked_range = (!new_text.is_empty()).then(|| range.start..range.start + new_text.len());
        self.selected_range = new_selected_range_utf16
            .as_ref()
            .map(|r| self.range_from_utf16(r))
            .map(|r| r.start + range.start..r.end + range.start)
            .unwrap_or_else(|| range.start + new_text.len()..range.start + new_text.len());
        cx.notify();
    }

    fn bounds_for_range(&mut self, range_utf16: Range<usize>, bounds: Bounds<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<Bounds<Pixels>> {
        let range = self.range_from_utf16(&range_utf16);
        let line_height = self.layout.borrow().line_height;
        let start = self.position_for(range.start)?;
        let end = self.position_for(range.end)?;
        Some(Bounds::from_corners(bounds.origin + start, bounds.origin + point(end.x, end.y + line_height)))
    }

    fn character_index_for_point(&mut self, point: Point<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<usize> {
        let origin = self.text_origin()?;
        Some(self.offset_to_utf16(self.offset_for(point - origin)))
    }
}

/// `runs: None` = one run in `color`; otherwise the given styled runs (live Markdown).
fn shape(text: &SharedString, width: Pixels, window: &Window, color: gpui::Hsla, runs: Option<Vec<TextRun>>) -> (Vec<Paragraph>, Pixels) {
    let style = window.text_style();
    let font_size = style.font_size.to_pixels(window.rem_size());
    let line_height = window.line_height();
    let runs = runs.unwrap_or_else(|| {
        vec![TextRun { len: text.len(), font: style.font(), color, background_color: None, underline: None, strikethrough: None }]
    });
    let lines = window.text_system().shape_text(text.clone(), font_size, &runs, Some(width), None).unwrap_or_default();
    let (mut start, mut top) = (0, px(0.));
    let paragraphs = lines
        .into_iter()
        .map(|line| {
            let para = Paragraph { start, top, line };
            start += para.line.len() + 1;
            top += para.line.size(line_height).height;
            para
        })
        .collect();
    (paragraphs, top.max(line_height))
}

struct TextAreaElement {
    input: Entity<TextArea>,
}

impl IntoElement for TextAreaElement {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}

impl Element for TextAreaElement {
    type RequestLayoutState = ();
    type PrepaintState = (Vec<PaintQuad>, Option<PaintQuad>);

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(&mut self, _: Option<&GlobalElementId>, _: Option<&gpui::InspectorElementId>, window: &mut Window, cx: &mut App) -> (LayoutId, ()) {
        let input = self.input.read(cx);
        let text = if input.content.is_empty() {
            input.placeholder.clone()
        } else if input.markdown {
            let family = window.text_style().font_family.clone();
            crate::markdown::live(&input.content, Some(input.cursor_offset()), &family, cx.theme()).display
        } else {
            input.content.clone()
        };
        let min_height = window.line_height() * input.min_lines as f32;
        let mut style = Style::default();
        style.size.width = relative(1.).into();
        // height follows the wrapped text at whatever width we get
        let layout_id = window.request_measured_layout(style, move |known, available, window, _cx| {
            let width = known.width.or(match available.width {
                gpui::AvailableSpace::Definite(w) => Some(w),
                _ => None,
            });
            let height = width.map_or(min_height, |w| shape(&text, w, window, gpui::black(), None).1.max(min_height));
            size(width.unwrap_or(px(0.)), height)
        });
        (layout_id, ())
    }

    fn prepaint(&mut self, _: Option<&GlobalElementId>, _: Option<&gpui::InspectorElementId>, bounds: Bounds<Pixels>, _: &mut (), window: &mut Window, cx: &mut App) -> Self::PrepaintState {
        let theme = cx.theme().clone();
        let input = self.input.read(cx);
        let empty = input.content.is_empty();
        let text = if empty { input.placeholder.clone() } else { input.content.clone() };
        let color = if empty { theme.text_placeholder } else { window.text_style().color };
        let selected = input.selected_range.clone();
        let cursor = input.cursor_offset();
        // live Markdown: shape what the user sees, keep maps back to the source
        let (text, runs, to_display, to_source) = if input.markdown && !empty {
            let family = window.text_style().font_family.clone();
            let live = crate::markdown::live(&text, Some(cursor), &family, &theme);
            (live.display, Some(live.runs), live.to_display, live.to_source)
        } else {
            (text, None, Vec::new(), Vec::new())
        };
        let (paragraphs, _) = shape(&text, bounds.size.width, window, color, runs);
        let line_height = window.line_height();
        let layout = input.layout.clone();
        *layout.borrow_mut() = Layout { paragraphs, line_height, bounds: Some(bounds), to_display, to_source };

        let input = self.input.read(cx);
        let pos = |offset: usize| input.position_for(offset).unwrap_or_default();
        if selected.is_empty() || empty {
            let at = if empty { point(px(0.), px(0.)) } else { pos(cursor) };
            let caret = fill(Bounds::new(bounds.origin + at, size(px(1.5), line_height)), theme.accent);
            return (Vec::new(), Some(caret));
        }
        // selection: one rect per visual row between start and end
        let (start, end) = (pos(selected.start), pos(selected.end));
        let color = theme.accent.opacity(0.22);
        let mut rects = Vec::new();
        let mut y = start.y;
        while y <= end.y {
            let x0 = if y == start.y { start.x } else { px(0.) };
            let x1 = if y == end.y { end.x } else { bounds.size.width };
            rects.push(fill(Bounds::from_corners(bounds.origin + point(x0, y), bounds.origin + point(x1, y + line_height)), color));
            y += line_height;
        }
        (rects, None)
    }

    fn paint(&mut self, _: Option<&GlobalElementId>, _: Option<&gpui::InspectorElementId>, bounds: Bounds<Pixels>, _: &mut (), prepaint: &mut Self::PrepaintState, window: &mut Window, cx: &mut App) {
        let focus_handle = self.input.read(cx).focus_handle.clone();
        window.handle_input(&focus_handle, ElementInputHandler::new(bounds, self.input.clone()), cx);
        let layout = self.input.read(cx).layout.clone();
        let layout = layout.borrow();
        // run backgrounds (inline code) are a separate pass in GPUI, under the selection
        for para in &layout.paragraphs {
            let _ = para.line.paint_background(bounds.origin + point(px(0.), para.top), layout.line_height, gpui::TextAlign::Left, None, window, cx);
        }
        for rect in prepaint.0.drain(..) {
            window.paint_quad(rect);
        }
        for para in &layout.paragraphs {
            let _ = para.line.paint(bounds.origin + point(px(0.), para.top), layout.line_height, gpui::TextAlign::Left, None, window, cx);
        }
        if focus_handle.is_focused(window)
            && let Some(caret) = prepaint.1.take()
        {
            window.paint_quad(caret);
        }
    }
}

impl Render for TextArea {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .w_full()
            .key_context("TextInput TextArea")
            .track_focus(&self.focus_handle)
            .cursor(CursorStyle::IBeam)
            .on_action(cx.listener(Self::backspace))
            .on_action(cx.listener(Self::delete))
            .on_action(cx.listener(Self::left))
            .on_action(cx.listener(Self::right))
            .on_action(cx.listener(Self::up))
            .on_action(cx.listener(Self::down))
            .on_action(cx.listener(Self::select_left))
            .on_action(cx.listener(Self::select_right))
            .on_action(cx.listener(Self::select_up))
            .on_action(cx.listener(Self::select_down))
            .on_action(cx.listener(Self::select_all))
            .on_action(cx.listener(Self::home))
            .on_action(cx.listener(Self::end))
            .on_action(cx.listener(Self::newline))
            .on_action(cx.listener(Self::submit))
            .on_action(cx.listener(Self::cancel))
            .on_action(cx.listener(Self::bold))
            .on_action(cx.listener(Self::italic))
            .on_action(cx.listener(Self::inline_code))
            .on_action(cx.listener(Self::paste))
            .on_action(cx.listener(Self::cut))
            .on_action(cx.listener(Self::copy))
            .on_mouse_down(MouseButton::Left, cx.listener(Self::on_mouse_down))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_mouse_up))
            .on_mouse_move(cx.listener(Self::on_mouse_move))
            .child(TextAreaElement { input: cx.entity() })
    }
}

impl Focusable for TextArea {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus_handle.clone()
    }
}
