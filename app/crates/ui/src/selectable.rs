//! Selectable read-only text, across paragraphs: a [`Selection`] scope collects the text blocks
//! of one document (a message, an event's details) in reading order; [`SelectableText`] is
//! one block. Dragging selects from block to block, double-click selects a word, triple-click
//! the block, a click elsewhere clears; links still open on a plain click. The owner copies
//! with [`Selection::selected_text`] (bound to ⌘C by the app).
//!
//! The view that renders the blocks calls [`Selection::begin`] at the top of its render; each
//! block takes the next index ([`Selection::block`]) and registers where it was laid out.

use std::cell::RefCell;
use std::ops::Range;
use std::rc::Rc;

use gpui::{
    App, Bounds, CursorStyle, DispatchPhase, Element, ElementId, EntityId, GlobalElementId, Hitbox, HitboxBehavior, InspectorElementId,
    IntoElement, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, Pixels, Point, SharedString, StyledText, TextLayout,
    TextRun, Window, fill, point, px, size,
};

use crate::theme::ActiveTheme;

/// A position in the document: block index, byte offset in that block.
type Spot = (usize, usize);

#[derive(Default)]
struct State {
    /// per block, in reading order: its text and where it was laid out this frame
    blocks: Vec<(SharedString, Option<TextLayout>)>,
    anchor: Option<Spot>,
    head: Option<Spot>,
    dragging: bool,
    /// the view that renders the blocks (repainted when the selection changes)
    view: Option<EntityId>,
}

#[derive(Clone, Default)]
pub struct Selection(Rc<RefCell<State>>);

impl Selection {
    pub fn new() -> Self {
        Self::default()
    }

    /// Start of a render pass: blocks register again, in order. The selection survives as long
    /// as the content does – call [`Selection::clear`] when it changes (another thread).
    pub fn begin(&self) {
        self.0.borrow_mut().blocks.clear();
    }

    pub fn clear(&self) {
        let mut s = self.0.borrow_mut();
        (s.anchor, s.head, s.dragging) = (None, None, false);
    }

    /// The next block of the document.
    pub fn block(&self, text: SharedString) -> usize {
        let mut s = self.0.borrow_mut();
        s.blocks.push((text, None));
        s.blocks.len() - 1
    }

    pub fn has_selection(&self) -> bool {
        let s = self.0.borrow();
        matches!((s.anchor, s.head), (Some(a), Some(h)) if a != h)
    }

    /// Everything selected, blocks separated by a line break.
    pub fn selected_text(&self) -> Option<String> {
        let s = self.0.borrow();
        let (start, end) = ordered(s.anchor?, s.head?);
        if start == end {
            return None;
        }
        let parts: Vec<&str> = (start.0..=end.0.min(s.blocks.len().saturating_sub(1)))
            .filter_map(|b| {
                let text = s.blocks.get(b)?.0.as_ref();
                let from = if b == start.0 { start.1 } else { 0 };
                let to = if b == end.0 { end.1 } else { text.len() };
                text.get(from.min(to)..to)
            })
            .collect();
        Some(parts.join("\n"))
    }

    /// ⌘A: the whole document.
    pub fn select_all(&self, cx: &mut App) {
        let view = {
            let mut s = self.0.borrow_mut();
            let Some(last) = s.blocks.len().checked_sub(1) else { return };
            let end = s.blocks[last].0.len();
            (s.anchor, s.head) = (Some((0, 0)), Some((last, end)));
            s.view
        };
        if let Some(view) = view {
            cx.notify(view);
        }
    }

    fn range_in(&self, block: usize, len: usize) -> Option<Range<usize>> {
        let s = self.0.borrow();
        let (start, end) = ordered(s.anchor?, s.head?);
        if start == end || block < start.0 || block > end.0 {
            return None;
        }
        let from = if block == start.0 { start.1 } else { 0 };
        let to = if block == end.0 { end.1 } else { len };
        (from < to).then_some(from..to)
    }

    /// The document position under a window point: the block it's in, else the end of the
    /// block above it (between paragraphs), else the start.
    fn spot_at(&self, position: Point<Pixels>) -> Option<Spot> {
        let s = self.0.borrow();
        let mut above: Option<Spot> = None;
        for (ix, (text, layout)) in s.blocks.iter().enumerate() {
            let Some(layout) = layout else { continue };
            let bounds = layout.bounds();
            if position.y >= bounds.top() && position.y <= bounds.bottom() {
                return Some((ix, index_in(layout, position)));
            }
            if position.y > bounds.bottom() {
                above = Some((ix, text.len()));
            }
        }
        above.or_else(|| s.blocks.iter().position(|(_, l)| l.is_some()).map(|ix| (ix, 0)))
    }

    fn contains(&self, position: Point<Pixels>) -> bool {
        self.0.borrow().blocks.iter().any(|(_, l)| l.as_ref().is_some_and(|l| l.bounds().contains(&position)))
    }

    fn notify(&self, cx: &mut App) {
        if let Some(view) = self.0.borrow().view {
            cx.notify(view);
        }
    }
}

fn ordered(a: Spot, b: Spot) -> (Spot, Spot) {
    if a <= b { (a, b) } else { (b, a) }
}

fn index_in(layout: &TextLayout, position: Point<Pixels>) -> usize {
    match layout.index_for_position(position) {
        Ok(ix) | Err(ix) => ix.min(layout.len()),
    }
}

/// The word around `ix` (letters, digits, `_`, plus `.`/`@`/`-` inside, so addresses and
/// versions select whole).
fn word_at(text: &str, ix: usize) -> Range<usize> {
    let is_word = |c: char| c.is_alphanumeric() || matches!(c, '_' | '.' | '@' | '-' | '/' | ':');
    let start = text[..ix.min(text.len())].char_indices().rev().take_while(|(_, c)| is_word(*c)).last().map_or(ix, |(i, _)| i);
    let end = text[ix.min(text.len())..].char_indices().find(|(_, c)| !is_word(*c)).map_or(text.len(), |(i, _)| ix + i);
    // trailing punctuation isn't part of the word ("example.com.")
    let trimmed = text[start..end].trim_end_matches(['.', ':', '-', '/']).len();
    start..start + trimmed
}

/// One selectable block: styled text (runs as for [`StyledText`]) plus optional links.
pub struct SelectableText {
    text: StyledText,
    raw: SharedString,
    selection: Selection,
    block: usize,
    links: Rc<Vec<(Range<usize>, SharedString)>>,
}

impl SelectableText {
    pub fn new(selection: &Selection, text: impl Into<SharedString>, runs: Vec<TextRun>) -> Self {
        let raw: SharedString = text.into();
        let block = selection.block(raw.clone());
        Self { text: StyledText::new(raw.clone()).with_runs(runs), raw, selection: selection.clone(), block, links: Rc::new(Vec::new()) }
    }

    /// One font, one color (titles, labels).
    pub fn plain(selection: &Selection, text: impl Into<SharedString>, family: impl Into<SharedString>, weight: gpui::FontWeight, color: gpui::Hsla) -> Self {
        let text: SharedString = text.into();
        let font = gpui::Font { family: family.into(), features: Default::default(), fallbacks: None, weight, style: gpui::FontStyle::Normal };
        let run = TextRun { len: text.len(), font, color, background_color: None, underline: None, strikethrough: None };
        Self::new(selection, text, vec![run])
    }

    /// Byte ranges that open a URL on a plain click (not after a drag).
    pub fn links(mut self, links: Vec<(Range<usize>, SharedString)>) -> Self {
        self.links = Rc::new(links);
        self
    }
}

impl IntoElement for SelectableText {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for SelectableText {
    type RequestLayoutState = ();
    type PrepaintState = Hitbox;

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(&mut self, id: Option<&GlobalElementId>, inspector_id: Option<&InspectorElementId>, window: &mut Window, cx: &mut App) -> (LayoutId, ()) {
        self.text.request_layout(id, inspector_id, window, cx)
    }

    fn prepaint(&mut self, id: Option<&GlobalElementId>, inspector_id: Option<&InspectorElementId>, bounds: Bounds<Pixels>, state: &mut (), window: &mut Window, cx: &mut App) -> Hitbox {
        self.text.prepaint(id, inspector_id, bounds, state, window, cx);
        if let Some(slot) = self.selection.0.borrow_mut().blocks.get_mut(self.block) {
            slot.1 = Some(self.text.layout().clone());
        }
        window.insert_hitbox(bounds, HitboxBehavior::Normal)
    }

    fn paint(&mut self, id: Option<&GlobalElementId>, inspector_id: Option<&InspectorElementId>, bounds: Bounds<Pixels>, state: &mut (), hitbox: &mut Hitbox, window: &mut Window, cx: &mut App) {
        self.selection.0.borrow_mut().view = Some(window.current_view());
        let layout = self.text.layout().clone();

        // the highlight goes under the glyphs
        if let Some(range) = self.selection.range_in(self.block, self.raw.len()) {
            let color = cx.theme().accent.opacity(0.22);
            let line_height = layout.line_height();
            // at a soft wrap the start index reports the end of the line above; a selection
            // starts where its first character is drawn, at the head of the next line
            let start = layout.position_for_index(range.start).map(|a| {
                let next = self.raw[range.start..].chars().next().map_or(range.start, |c| range.start + c.len_utf8());
                match layout.position_for_index(next) {
                    Some(n) if n.y > a.y => point(bounds.left(), n.y),
                    _ => a,
                }
            });
            if let (Some(a), Some(b)) = (start, layout.position_for_index(range.end)) {
                let (left, right) = (bounds.left(), bounds.right());
                let rect = |x0: Pixels, x1: Pixels, y: Pixels| fill(Bounds::new(point(x0, y), size((x1 - x0).max(px(0.)), line_height)), color);
                if a.y == b.y {
                    window.paint_quad(rect(a.x, b.x, a.y));
                } else {
                    window.paint_quad(rect(a.x, right, a.y));
                    let mut y = a.y + line_height;
                    while y < b.y {
                        window.paint_quad(rect(left, right, y));
                        y += line_height;
                    }
                    window.paint_quad(rect(left, b.x, b.y));
                }
            }
        }
        self.text.paint(id, inspector_id, bounds, state, &mut (), window, cx);
        window.set_cursor_style(CursorStyle::IBeam, hitbox);

        // press in this block: anchor (word / whole block on double / triple click)
        let (selection, block, raw, hit) = (self.selection.clone(), self.block, self.raw.clone(), hitbox.clone());
        window.on_mouse_event(move |event: &MouseDownEvent, phase, window, cx| {
            if phase != DispatchPhase::Bubble || event.button != MouseButton::Left || !hit.is_hovered(window) {
                return;
            }
            let ix = index_in(&layout, event.position);
            let (from, to) = match event.click_count {
                1 => (ix, ix),
                2 => {
                    let w = word_at(&raw, ix);
                    (w.start, w.end)
                }
                _ => (0, raw.len()),
            };
            {
                let mut s = selection.0.borrow_mut();
                (s.anchor, s.head, s.dragging) = (Some((block, from)), Some((block, to)), event.click_count == 1);
            }
            selection.notify(cx);
        });

        // the first block also handles what concerns the whole document: dragging across
        // blocks, release (and link clicks), and clearing on a press outside the text
        if self.block == 0 {
            let selection = self.selection.clone();
            window.on_mouse_event(move |event: &MouseMoveEvent, phase, _, cx| {
                if phase != DispatchPhase::Bubble || !selection.0.borrow().dragging {
                    return;
                }
                if let Some(spot) = selection.spot_at(event.position) {
                    let changed = {
                        let mut s = selection.0.borrow_mut();
                        let changed = s.head != Some(spot);
                        s.head = Some(spot);
                        changed
                    };
                    if changed {
                        selection.notify(cx);
                    }
                }
            });
            let selection = self.selection.clone();
            window.on_mouse_event(move |event: &MouseUpEvent, phase, _, _| {
                if phase == DispatchPhase::Bubble && event.button == MouseButton::Left {
                    selection.0.borrow_mut().dragging = false;
                }
            });
            let selection = self.selection.clone();
            window.on_mouse_event(move |event: &MouseDownEvent, phase, _, cx| {
                if phase == DispatchPhase::Capture && event.button == MouseButton::Left && !selection.contains(event.position) && selection.has_selection() {
                    selection.clear();
                    selection.notify(cx);
                }
            });
        }

        // a plain click on a link opens it (a drag that started on it doesn't)
        if !self.links.is_empty() {
            let (selection, links, layout, hit) = (self.selection.clone(), self.links.clone(), self.text.layout().clone(), hitbox.clone());
            window.on_mouse_event(move |event: &MouseUpEvent, phase, window, cx| {
                if phase != DispatchPhase::Bubble || event.button != MouseButton::Left || !hit.is_hovered(window) || selection.has_selection() {
                    return;
                }
                let ix = index_in(&layout, event.position);
                if let Some((_, url)) = links.iter().find(|(r, _)| r.contains(&ix)) {
                    crate::link::open(url, cx);
                }
            });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn words_include_addresses_but_not_trailing_punctuation() {
        let text = "Mail julian@habric.com, see example.com. Done";
        let at = |needle: &str| text.find(needle).unwrap() + 2;
        assert_eq!(&text[word_at(text, at("julian"))], "julian@habric.com");
        assert_eq!(&text[word_at(text, at("example"))], "example.com");
        assert_eq!(&text[word_at(text, at("Done"))], "Done");
    }

    #[test]
    fn selection_spans_blocks_in_order() {
        let selection = Selection::new();
        selection.begin();
        selection.block("First paragraph".into());
        selection.block("Second one".into());
        selection.block("Third".into());
        {
            let mut s = selection.0.borrow_mut();
            // dragged backwards: from the middle of the third into the first
            (s.anchor, s.head) = (Some((2, 3)), Some((0, 6)));
        }
        assert_eq!(selection.selected_text().as_deref(), Some("paragraph\nSecond one\nThi"));
        assert_eq!(selection.range_in(1, 10), Some(0..10));
        assert_eq!(selection.range_in(0, 15), Some(6..15));
    }
}
