//! Text, selection and edit history; no key grammar, focus, layout or platform IO.
//! Adapters choose a range before applying an edit, whether their grammar is
//! selection-first or operator-first. Offsets are UTF-8 bytes at character boundaries.

use std::collections::VecDeque;
use std::ops::Range;

use gpui::SharedString;
use unicode_segmentation::UnicodeSegmentation;

const HISTORY_STEPS: usize = 128;
const HISTORY_BYTES: usize = 8 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Selection {
    pub anchor: usize,
    pub head: usize,
}

impl Selection {
    pub fn caret(at: usize) -> Self {
        Self { anchor: at, head: at }
    }

    pub fn range(self) -> Range<usize> {
        self.anchor.min(self.head)..self.anchor.max(self.head)
    }
}

#[derive(Clone, Default)]
struct State {
    text: SharedString,
    selection: Selection,
}

#[derive(Clone, Copy)]
pub enum Direction {
    Backward,
    Forward,
}

#[derive(Default)]
pub struct Editor {
    state: State,
    undo: VecDeque<State>,
    redo: Vec<State>,
    /// One native composition is one undo step, not one step per preedit update.
    composition: Option<State>,
    marked: Option<Range<usize>>,
}

impl Editor {
    pub fn text(&self) -> &SharedString {
        &self.state.text
    }

    /// Draft autosave must not persist transient IME preedit text.
    pub fn committed_text(&self) -> &SharedString {
        &self.composition.as_ref().unwrap_or(&self.state).text
    }

    pub fn selection(&self) -> Selection {
        self.state.selection
    }

    pub fn marked(&self) -> Option<Range<usize>> {
        self.marked.clone()
    }

    /// Loading/recovering another draft establishes a new baseline, not an undo edit.
    pub fn reset(&mut self, text: SharedString) {
        *self = Self { state: State { selection: Selection::caret(text.len()), text }, ..Self::default() };
    }

    pub fn select(&mut self, selection: Selection) -> bool {
        if !self.text().is_char_boundary(selection.anchor) || !self.text().is_char_boundary(selection.head) {
            return false;
        }
        self.state.selection = selection;
        true
    }

    pub fn boundary(&self, direction: Direction, word: bool) -> usize {
        let at = self.selection().head;
        if word {
            // Unicode word segments; punctuation is a segment too. Skip adjacent
            // whitespace on the approached side, never special-case ASCII bytes.
            let mut segments = self.text().split_word_bound_indices().filter(|(_, s)| !s.trim().is_empty());
            match direction {
                Direction::Backward => segments.rfind(|(i, _)| *i < at).map_or(0, |(i, _)| i),
                Direction::Forward => segments.find(|(i, s)| i + s.len() > at).map_or(self.text().len(), |(i, s)| i + s.len()),
            }
        } else {
            match direction {
                Direction::Backward => self.text().grapheme_indices(true).rev().find_map(|(i, _)| (i < at).then_some(i)).unwrap_or(0),
                Direction::Forward => self.text().grapheme_indices(true).find_map(|(i, _)| (i > at).then_some(i)).unwrap_or(self.text().len()),
            }
        }
    }

    pub fn move_to(&mut self, at: usize, extend: bool) {
        self.select(Selection { anchor: if extend { self.selection().anchor } else { at }, head: at });
    }

    pub fn replace_selection(&mut self, text: &str) -> bool {
        self.replace(self.selection().range(), text)
    }

    pub fn delete(&mut self, direction: Direction, word: bool) -> bool {
        let selection = self.selection();
        let range = if selection.range().is_empty() {
            let to = self.boundary(direction, word);
            selection.head.min(to)..selection.head.max(to)
        } else {
            selection.range()
        };
        self.replace(range, "")
    }

    pub fn replace(&mut self, range: Range<usize>, text: &str) -> bool {
        if !self.valid_range(&range) {
            return false;
        }
        let before = self.composition.take().unwrap_or_else(|| self.state.clone());
        self.replace_unrecorded(range, text);
        self.marked = None;
        self.record(before)
    }

    fn valid_range(&self, range: &Range<usize>) -> bool {
        range.start <= range.end && self.text().is_char_boundary(range.start) && self.text().is_char_boundary(range.end)
    }

    fn replace_unrecorded(&mut self, range: Range<usize>, text: &str) {
        self.state.text = format!("{}{}{}", &self.text()[..range.start], text, &self.text()[range.end..]).into();
        self.state.selection = Selection::caret(range.start + text.len());
    }

    fn record(&mut self, before: State) -> bool {
        if before.text == self.state.text {
            return false;
        }
        self.redo.clear();
        self.undo.push_back(before);
        self.trim_history();
        true
    }

    fn trim_history(&mut self) {
        let mut bytes: usize = self.undo.iter().chain(&self.redo).map(|s| s.text.len()).sum();
        while self.undo.len() + self.redo.len() > HISTORY_STEPS || bytes > HISTORY_BYTES {
            // Oldest undo and furthest redo are least useful. Large past documents
            // may be evicted completely; the current document is never truncated.
            let old = self.undo.pop_front().or_else(|| (!self.redo.is_empty()).then(|| self.redo.remove(0)));
            let Some(old) = old else { break };
            bytes -= old.text.len();
        }
    }

    pub fn undo(&mut self) -> bool {
        if let Some(before) = self.composition.take() {
            self.state = before;
            self.marked = None;
            return false; // cancelled preedit was never a committed change
        }
        let Some(before) = self.undo.pop_back() else { return false };
        self.redo.push(std::mem::replace(&mut self.state, before));
        self.trim_history();
        true
    }

    pub fn redo(&mut self) -> bool {
        if self.composition.is_some() {
            return false;
        }
        let Some(after) = self.redo.pop() else { return false };
        self.undo.push_back(std::mem::replace(&mut self.state, after));
        self.trim_history();
        true
    }

    pub fn range_from_utf16(&self, range: Range<usize>) -> Range<usize> {
        from_utf16(self.text(), range.start)..from_utf16(self.text(), range.end)
    }

    pub fn range_to_utf16(&self, range: Range<usize>) -> Range<usize> {
        self.text()[..range.start].encode_utf16().count()..self.text()[..range.end].encode_utf16().count()
    }

    fn input_range(&self, range_utf16: Option<Range<usize>>) -> Range<usize> {
        range_utf16.map(|r| self.range_from_utf16(r)).or_else(|| self.marked()).unwrap_or_else(|| self.selection().range())
    }

    pub fn input(&mut self, range_utf16: Option<Range<usize>>, text: &str) -> bool {
        self.replace(self.input_range(range_utf16), text)
    }

    pub fn preedit(&mut self, range_utf16: Option<Range<usize>>, text: &str, selection_utf16: Option<Range<usize>>) {
        let range = self.input_range(range_utf16);
        if !self.valid_range(&range) {
            return;
        }
        self.composition.get_or_insert_with(|| self.state.clone());
        let start = range.start;
        self.replace_unrecorded(range, text);
        self.marked = Some(start..start + text.len());
        if let Some(selection) = selection_utf16.filter(|s| s.start <= s.end) {
            // Native preedit selection is relative to the inserted string, not
            // the document prefix (which may contain surrogate pairs).
            self.select(Selection { anchor: start + from_utf16(text, selection.start), head: start + from_utf16(text, selection.end) });
        }
    }

    pub fn finish_composition(&mut self) -> bool {
        self.marked = None;
        self.composition.take().is_some_and(|before| self.record(before))
    }
}

fn from_utf16(text: &str, offset: usize) -> usize {
    let mut units = 0;
    for (at, ch) in text.char_indices() {
        if units >= offset {
            return at;
        }
        units += ch.len_utf16();
    }
    text.len()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn selection_first_and_operator_first_share_edits_not_grammar() {
        let mut select_first = Editor::default();
        let mut operator_first = Editor::default();
        for editor in [&mut select_first, &mut operator_first] {
            editor.reset("écho  café!\n終わり".into());
            editor.move_to("écho  café".len(), false);
        }
        select_first.move_to(select_first.boundary(Direction::Backward, true), true);
        assert_eq!(&select_first.text()[select_first.selection().range()], "café");
        assert!(select_first.replace_selection(""));
        assert!(operator_first.delete(Direction::Backward, true));
        assert_eq!(select_first.text(), "écho  !\n終わり");
        assert_eq!(select_first.text(), operator_first.text());
        assert_eq!(select_first.selection(), operator_first.selection());
        assert!(select_first.undo());
        assert_eq!(select_first.text(), "écho  café!\n終わり");
        assert_eq!(select_first.selection(), Selection { anchor: 12, head: 7 });
        assert!(operator_first.undo());
        assert_eq!(operator_first.selection(), Selection::caret(12));
        assert!(select_first.redo());
        assert_eq!(select_first.text(), "écho  !\n終わり");
        operator_first.move_to(7, false);
        assert!(operator_first.delete(Direction::Forward, true));
        assert_eq!(operator_first.text(), "écho  !\n終わり");
        assert!(operator_first.delete(Direction::Forward, true));
        assert_eq!(operator_first.text(), "écho  \n終わり"); // punctuation, not the next word
        operator_first.move_to(5, false);
        assert_eq!(operator_first.boundary(Direction::Forward, true), 11); // skip spaces and newline, then 終
        assert_eq!(operator_first.boundary(Direction::Backward, true), 0);
    }

    #[test]
    fn edits_preserve_graphemes_and_history_branches() {
        let mut editor = Editor::default();
        editor.reset("a👩🏽‍💻e\u{301}\n終".into());
        editor.move_to("a👩🏽‍💻e\u{301}".len(), false);
        assert!(editor.delete(Direction::Backward, false));
        assert_eq!(editor.text(), "a👩🏽‍💻\n終");
        assert!(editor.delete(Direction::Backward, false));
        assert_eq!(editor.text(), "a\n終");
        assert!(editor.undo());
        assert!(editor.replace_selection("paste\n🦀"));
        assert_eq!(editor.text(), "a👩🏽‍💻paste\n🦀\n終");
        assert!(!editor.redo());
        assert!(!editor.select(Selection::caret(2))); // inside emoji, no panic/mutation
        assert!(!editor.replace(2..3, "invalid"));
        assert!(editor.undo());
        assert_eq!(editor.text(), "a👩🏽‍💻\n終");
        editor.reset("recovered draft".into());
        assert!(!editor.undo());
        assert!(!editor.redo());
    }

    #[test]
    fn native_preedit_is_relative_atomic_and_not_autosaved() {
        let mut editor = Editor::default();
        editor.reset("🦀 prefix old tail".into());
        editor.select(Selection { anchor: 12, head: 15 });
        editor.preedit(None, "é🦀x", Some(1..3));
        assert_eq!(editor.text(), "🦀 prefix é🦀x tail");
        assert_eq!(editor.selection(), Selection { anchor: 14, head: 18 });
        assert_eq!(editor.range_to_utf16(editor.selection().range()), 11..13);
        assert_eq!(editor.committed_text(), "🦀 prefix old tail");
        editor.preedit(None, "日本", Some(2..2));
        assert_eq!(editor.committed_text(), "🦀 prefix old tail");
        assert!(editor.input(None, "日本語"));
        assert_eq!(editor.committed_text(), "🦀 prefix 日本語 tail");
        assert!(editor.undo());
        assert_eq!(editor.text(), "🦀 prefix old tail");
        assert_eq!(editor.selection(), Selection { anchor: 12, head: 15 });
        assert!(!editor.undo());
        assert!(editor.redo());
        editor.preedit(Some(3..9), "changed", None);
        assert!(!editor.undo()); // cancel preedit only
        assert_eq!(editor.text(), "🦀 prefix 日本語 tail");
        editor.preedit(None, "!", None);
        assert!(editor.finish_composition());
        assert!(editor.undo());
        assert_eq!(editor.text(), "🦀 prefix 日本語 tail");
    }

    #[test]
    fn bounded_history_never_truncates_document() {
        let mut editor = Editor::default();
        for _ in 0..150 {
            editor.replace_selection("x");
        }
        for _ in 0..128 {
            assert!(editor.undo());
        }
        assert!(!editor.undo());
        assert_eq!(editor.text().len(), 22);
        editor.reset("x".repeat(HISTORY_BYTES + 1).into());
        assert!(editor.replace_selection("tail"));
        assert!(!editor.undo());
        assert_eq!(editor.text().len(), HISTORY_BYTES + 5);
    }
}
