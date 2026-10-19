//! Multi-line, soft-wrapping text editor for the composer. Shares editing actions and key
//! bindings with [`TextInput`](crate::TextInput) (its key context includes `TextInput`) and adds
//! vertical movement, newlines and a submit action. Height follows the wrapped content, so the
//! composer grows as you type.

use std::cell::{Cell, RefCell};
use std::ops::Range;
use std::rc::Rc;

use gpui::{
    App, Bounds, ClipboardItem, Context, CursorStyle, ElementId, ElementInputHandler, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable,
    GlobalElementId, KeyBinding, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, PaintQuad, Pixels, Point, ScrollHandle, SharedString, Style, TextRun,
    UTF16Selection, Window, WrappedLine, actions, div, fill, point, prelude::*, px, relative, size,
};

use crate::editing::{Direction, Editor, Selection};
use crate::theme::ActiveTheme;
use crate::{Backspace, Copy, Cut, Delete, End, Home, Left, Paste, Right, SelectAll, SelectLeft, SelectRight};

actions!(
    text_area,
    [
        Up,
        Down,
        SelectUp,
        SelectDown,
        Newline,
        Submit,
        Cancel,
        Bold,
        Italic,
        InlineCode,
        Undo,
        Redo,
        WordLeft,
        WordRight,
        SelectWordLeft,
        SelectWordRight,
        DeleteWordLeft,
        DeleteWordRight
    ]
);

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
        KeyBinding::new("cmd-z", Undo, Some(CONTEXT)),
        KeyBinding::new("cmd-shift-z", Redo, Some(CONTEXT)),
    ]);
    #[cfg(target_os = "macos")]
    let word = "alt";
    #[cfg(not(target_os = "macos"))]
    let word = "ctrl";
    cx.bind_keys([
        KeyBinding::new(&format!("{word}-left"), WordLeft, Some(CONTEXT)),
        KeyBinding::new(&format!("{word}-right"), WordRight, Some(CONTEXT)),
        KeyBinding::new(&format!("{word}-shift-left"), SelectWordLeft, Some(CONTEXT)),
        KeyBinding::new(&format!("{word}-shift-right"), SelectWordRight, Some(CONTEXT)),
        KeyBinding::new(&format!("{word}-backspace"), DeleteWordLeft, Some(CONTEXT)),
        KeyBinding::new(&format!("{word}-delete"), DeleteWordRight, Some(CONTEXT)),
    ]);
    #[cfg(not(target_os = "macos"))]
    cx.bind_keys([
        KeyBinding::new("ctrl-z", Undo, Some(CONTEXT)),
        KeyBinding::new("ctrl-shift-z", Redo, Some(CONTEXT)),
        KeyBinding::new("ctrl-y", Redo, Some(CONTEXT)),
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
    editor: Editor,
    placeholder: SharedString,
    min_lines: usize,
    layout: Rc<RefCell<Layout>>,
    is_selecting: bool,
    /// Column to return to when moving up/down through shorter lines.
    goal_x: Option<Pixels>,
    /// Live Markdown styling + formatting shortcuts.
    markdown: bool,
    /// Optional enclosing scroll region; reveal edits/navigation, never ordinary wheel scrolls.
    scroll: Option<ScrollHandle>,
    reveal_caret: Cell<bool>,
    was_focused: Cell<bool>,
}

impl EventEmitter<TextAreaEvent> for TextArea {}

impl TextArea {
    pub fn new(placeholder: impl Into<SharedString>, min_lines: usize, cx: &mut Context<Self>) -> Self {
        Self {
            focus_handle: cx.focus_handle(),
            editor: Editor::default(),
            placeholder: placeholder.into(),
            min_lines,
            layout: Rc::default(),
            is_selecting: false,
            goal_x: None,
            markdown: false,
            scroll: None,
            reveal_caret: Cell::new(true),
            was_focused: Cell::new(false),
        }
    }

    pub fn scroll_in(mut self, scroll: ScrollHandle) -> Self {
        self.scroll = Some(scroll);
        self
    }

    pub fn markdown(mut self, enabled: bool) -> Self {
        self.markdown = enabled;
        self
    }

    /// Cmd-B / Cmd-I / Cmd-E: wrap the selection in `marker`, or unwrap if it's already wrapped.
    /// With no selection, insert a pair and put the cursor between.
    pub fn toggle_wrap(&mut self, marker: &str, cx: &mut Context<Self>) {
        let range = self.editor.selection().range();
        let reversed = self.editor.selection().head < self.editor.selection().anchor;
        let (m, text) = (marker.len(), self.editor.text().to_string());
        let wrapped = range.start >= m && text[range.end..].starts_with(marker) && text[..range.start].ends_with(marker);
        let (new, selection) = if wrapped {
            let new = format!("{}{}{}", &text[..range.start - m], &text[range.clone()], &text[range.end + m..]);
            (new, range.start - m..range.end - m)
        } else {
            let new = format!("{}{marker}{}{marker}{}", &text[..range.start], &text[range.clone()], &text[range.end..]);
            (new, range.start + m..range.end + m)
        };
        let changed = self.editor.replace(0..text.len(), &new);
        let (anchor, head) = if reversed { (selection.end, selection.start) } else { (selection.start, selection.end) };
        self.editor.select(Selection { anchor, head });
        self.edited(changed, cx);
    }

    /// Toolbar: toggle a block prefix ("- ", "> ", "1. ") on the cursor's line.
    pub fn toggle_line_prefix(&mut self, prefix: &str, cx: &mut Context<Self>) {
        let cursor = self.cursor_offset();
        let start = self.editor.text()[..cursor].rfind('\n').map_or(0, |i| i + 1);
        let text = self.editor.text().to_string();
        let (new, delta): (String, isize) = if text[start..].starts_with(prefix) {
            (format!("{}{}", &text[..start], &text[start + prefix.len()..]), -(prefix.len() as isize))
        } else {
            (format!("{}{prefix}{}", &text[..start], &text[start..]), prefix.len() as isize)
        };
        let changed = self.editor.replace(0..text.len(), &new);
        let at = (cursor as isize + delta).max(start as isize) as usize;
        self.editor.move_to(at, false);
        self.edited(changed, cx);
    }

    pub fn text(&self) -> &SharedString {
        self.editor.committed_text()
    }

    pub fn set_text(&mut self, text: impl Into<SharedString>, cx: &mut Context<Self>) {
        self.editor.reset(text.into());
        self.goal_x = None;
        self.reveal_caret.set(true);
        cx.emit(TextAreaEvent::Changed);
        cx.notify();
    }

    pub fn is_empty(&self) -> bool {
        self.text().trim().is_empty()
    }

    fn cursor_offset(&self) -> usize {
        self.editor.selection().head
    }

    fn move_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        self.editor.move_to(offset, false);
        self.reveal_caret.set(true);
        cx.notify();
    }

    fn select_to(&mut self, offset: usize, cx: &mut Context<Self>) {
        self.editor.move_to(offset, true);
        self.reveal_caret.set(true);
        cx.notify();
    }

    fn edited(&mut self, changed: bool, cx: &mut Context<Self>) {
        self.goal_x = None;
        self.reveal_caret.set(true);
        if changed {
            cx.emit(TextAreaEvent::Changed);
        }
        cx.notify();
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
        layout.source(para.start + ix).min(self.editor.text().len())
    }

    fn vertical(&mut self, lines: f32, select: bool, cx: &mut Context<Self>) {
        let line_height = self.layout.borrow().line_height;
        let Some(pos) = self.position_for(self.cursor_offset()) else { return };
        let x = *self.goal_x.get_or_insert(pos.x);
        let target = point(x, pos.y + line_height * lines + line_height / 2.);
        let offset = if target.y < px(0.) { 0 } else { self.offset_for(target) };
        let goal = self.goal_x;
        if select {
            self.select_to(offset, cx)
        } else {
            self.move_to(offset, cx)
        }
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
        let range = self.editor.selection().range();
        if range.is_empty() {
            self.move_to(self.editor.boundary(Direction::Backward, false), cx);
        } else {
            self.move_to(range.start, cx)
        }
    }
    fn right(&mut self, _: &Right, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        let range = self.editor.selection().range();
        if range.is_empty() {
            self.move_to(self.editor.boundary(Direction::Forward, false), cx);
        } else {
            self.move_to(range.end, cx)
        }
    }
    fn select_left(&mut self, _: &SelectLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.editor.boundary(Direction::Backward, false), cx);
    }
    fn select_right(&mut self, _: &SelectRight, _: &mut Window, cx: &mut Context<Self>) {
        self.select_to(self.editor.boundary(Direction::Forward, false), cx);
    }
    fn select_all(&mut self, _: &SelectAll, _: &mut Window, cx: &mut Context<Self>) {
        self.move_to(0, cx);
        self.select_to(self.editor.text().len(), cx)
    }
    /// Home/End work on the current paragraph.
    fn home(&mut self, _: &Home, _: &mut Window, cx: &mut Context<Self>) {
        let start = self.editor.text()[..self.cursor_offset()].rfind('\n').map_or(0, |i| i + 1);
        self.move_to(start, cx);
    }
    fn end(&mut self, _: &End, _: &mut Window, cx: &mut Context<Self>) {
        let cursor = self.cursor_offset();
        let end = self.editor.text()[cursor..].find('\n').map_or(self.editor.text().len(), |i| cursor + i);
        self.move_to(end, cx);
    }
    fn backspace(&mut self, _: &Backspace, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.delete(Direction::Backward, false);
        self.edited(changed, cx);
    }
    fn delete(&mut self, _: &Delete, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.delete(Direction::Forward, false);
        self.edited(changed, cx);
    }
    fn undo(&mut self, _: &Undo, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.undo();
        self.edited(changed, cx);
    }
    fn redo(&mut self, _: &Redo, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.redo();
        self.edited(changed, cx);
    }
    fn word_left(&mut self, _: &WordLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        self.move_to(self.editor.boundary(Direction::Backward, true), cx);
    }
    fn word_right(&mut self, _: &WordRight, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        self.move_to(self.editor.boundary(Direction::Forward, true), cx);
    }
    fn select_word_left(&mut self, _: &SelectWordLeft, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        self.select_to(self.editor.boundary(Direction::Backward, true), cx);
    }
    fn select_word_right(&mut self, _: &SelectWordRight, _: &mut Window, cx: &mut Context<Self>) {
        self.goal_x = None;
        self.select_to(self.editor.boundary(Direction::Forward, true), cx);
    }
    fn delete_word_left(&mut self, _: &DeleteWordLeft, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.delete(Direction::Backward, true);
        self.edited(changed, cx);
    }
    fn delete_word_right(&mut self, _: &DeleteWordRight, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.delete(Direction::Forward, true);
        self.edited(changed, cx);
    }
    fn newline(&mut self, _: &Newline, window: &mut Window, cx: &mut Context<Self>) {
        if self.markdown && self.editor.selection().range().is_empty() {
            // lists and quotes continue; Enter on an empty item ends the list
            let cursor = self.cursor_offset();
            let start = self.editor.text()[..cursor].rfind('\n').map_or(0, |i| i + 1);
            let line = self.editor.text()[start..cursor].to_string();
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
                    let changed = self.editor.replace(start..cursor, "");
                    return self.edited(changed, cx);
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
        let changed = self.editor.finish_composition();
        self.edited(changed, cx);
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
        let range = self.editor.selection().range();
        if !range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.editor.text()[range].to_string()));
        }
    }
    fn cut(&mut self, _: &Cut, window: &mut Window, cx: &mut Context<Self>) {
        let range = self.editor.selection().range();
        if !range.is_empty() {
            cx.write_to_clipboard(ClipboardItem::new_string(self.editor.text()[range].to_string()));
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
}

impl EntityInputHandler for TextArea {
    fn text_for_range(&mut self, range_utf16: Range<usize>, actual: &mut Option<Range<usize>>, _: &mut Window, _: &mut Context<Self>) -> Option<String> {
        let range = self.editor.range_from_utf16(range_utf16);
        let text = self.editor.text().get(range.clone())?.to_string();
        actual.replace(self.editor.range_to_utf16(range));
        Some(text)
    }

    fn selected_text_range(&mut self, _: bool, _: &mut Window, _: &mut Context<Self>) -> Option<UTF16Selection> {
        let selection = self.editor.selection();
        Some(UTF16Selection { range: self.editor.range_to_utf16(selection.range()), reversed: selection.head < selection.anchor })
    }

    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        self.editor.marked().map(|r| self.editor.range_to_utf16(r))
    }

    fn unmark_text(&mut self, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.finish_composition();
        self.edited(changed, cx);
    }

    fn replace_text_in_range(&mut self, range_utf16: Option<Range<usize>>, new_text: &str, _: &mut Window, cx: &mut Context<Self>) {
        let changed = self.editor.input(range_utf16, new_text);
        self.edited(changed, cx);
    }

    fn replace_and_mark_text_in_range(
        &mut self,
        range_utf16: Option<Range<usize>>,
        new_text: &str,
        new_selected_range_utf16: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.editor.preedit(range_utf16, new_text, new_selected_range_utf16);
        self.goal_x = None;
        self.reveal_caret.set(true);
        cx.notify();
    }

    fn bounds_for_range(&mut self, range_utf16: Range<usize>, bounds: Bounds<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<Bounds<Pixels>> {
        let range = self.editor.range_from_utf16(range_utf16);
        let line_height = self.layout.borrow().line_height;
        let start = self.position_for(range.start)?;
        let end = self.position_for(range.end)?;
        Some(Bounds::from_corners(bounds.origin + start, bounds.origin + point(end.x, end.y + line_height)))
    }

    fn character_index_for_point(&mut self, point: Point<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<usize> {
        let origin = self.text_origin()?;
        let at = self.offset_for(point - origin);
        self.editor.text().is_char_boundary(at).then(|| self.editor.range_to_utf16(at..at).start)
    }
}

/// `runs: None` = one run in `color`; otherwise the given styled runs (live Markdown).
fn shape(text: &SharedString, width: Pixels, window: &Window, style: &gpui::TextStyle, line_height: Pixels, color: gpui::Hsla, runs: Option<Vec<TextRun>>) -> (Vec<Paragraph>, Pixels) {
    let font_size = style.font_size.to_pixels(window.rem_size());
    let runs =
        runs.unwrap_or_else(|| vec![TextRun { len: text.len(), font: style.font(), color, background_color: None, underline: None, strikethrough: None }]);
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
        let text = if input.editor.text().is_empty() {
            input.placeholder.clone()
        } else if input.markdown {
            let family = window.text_style().font_family.clone();
            crate::markdown::live(input.editor.text(), Some(input.cursor_offset()), &family, cx.theme()).display
        } else {
            input.editor.text().clone()
        };
        // Measured-layout callbacks run outside the element's inherited text-style scope.
        // Capture it now, or the measured height differs from what prepaint actually draws.
        let text_style = window.text_style();
        let line_height = window.line_height();
        let min_height = line_height * input.min_lines as f32;
        let mut style = Style::default();
        style.size.width = relative(1.).into();
        // height follows the wrapped text at whatever width we get
        let layout_id = window.request_measured_layout(style, move |known, available, window, _cx| {
            let width = known.width.or(match available.width {
                gpui::AvailableSpace::Definite(w) => Some(w),
                _ => None,
            });
            let height = width.map_or(min_height, |w| shape(&text, w, window, &text_style, line_height, gpui::black(), None).1.max(min_height));
            size(width.unwrap_or(px(0.)), height)
        });
        (layout_id, ())
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) -> Self::PrepaintState {
        let theme = cx.theme().clone();
        let input = self.input.read(cx);
        let empty = input.editor.text().is_empty();
        let text = if empty { input.placeholder.clone() } else { input.editor.text().clone() };
        let color = if empty { theme.text_placeholder } else { window.text_style().color };
        let selected = input.editor.selection().range();
        let cursor = input.cursor_offset();
        // live Markdown: shape what the user sees, keep maps back to the source
        let (text, runs, to_display, to_source) = if input.markdown && !empty {
            let family = window.text_style().font_family.clone();
            let live = crate::markdown::live(&text, Some(cursor), &family, &theme);
            (live.display, Some(live.runs), live.to_display, live.to_source)
        } else {
            (text, None, Vec::new(), Vec::new())
        };
        let line_height = window.line_height();
        let (paragraphs, _) = shape(&text, bounds.size.width, window, &window.text_style(), line_height, color, runs);
        let layout = input.layout.clone();
        *layout.borrow_mut() = Layout { paragraphs, line_height, bounds: Some(bounds), to_display, to_source };

        let input = self.input.read(cx);
        let pos = |offset: usize| input.position_for(offset).unwrap_or_default();
        let focused = input.focus_handle.is_focused(window);
        let was_focused = input.was_focused.replace(focused);
        if focused && (input.reveal_caret.replace(false) || !was_focused)
            && let Some(scroll) = &input.scroll
        {
            let caret = Bounds::new(bounds.origin + pos(cursor), size(px(1.5), line_height));
            let viewport = scroll.bounds();
            let mut offset = scroll.offset();
            if caret.top() < viewport.top() {
                offset.y += viewport.top() - caret.top();
            } else if caret.bottom() > viewport.bottom() {
                offset.y += viewport.bottom() - caret.bottom();
            }
            offset.y = offset.y.clamp(-scroll.max_offset().y, px(0.));
            if offset != scroll.offset() {
                // Div has already prepainted its translation this frame. Refresh once with
                // the new offset; plain scrolling Divs do not consume request_autoscroll.
                let scroll = scroll.clone();
                window.on_next_frame(move |window, _| {
                    scroll.set_offset(offset);
                    window.refresh();
                });
            }
        }
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

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        prepaint: &mut Self::PrepaintState,
        window: &mut Window,
        cx: &mut App,
    ) {
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
            .on_action(cx.listener(Self::undo))
            .on_action(cx.listener(Self::redo))
            .on_action(cx.listener(Self::word_left))
            .on_action(cx.listener(Self::word_right))
            .on_action(cx.listener(Self::select_word_left))
            .on_action(cx.listener(Self::select_word_right))
            .on_action(cx.listener(Self::delete_word_left))
            .on_action(cx.listener(Self::delete_word_right))
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
