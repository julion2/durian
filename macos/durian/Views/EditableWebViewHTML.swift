//
//  EditableWebViewHTML.swift
//  Durian
//
//  HTML/JS template generation for EditableWebView.
//  Split out of EditableWebView.swift to keep that file focused on
//  NSViewRepresentable/WKWebView glue.
//

import AppKit

enum EditableWebViewHTML {
        static func resolveHex(_ color: NSColor, dark: Bool) -> String
    {
        let name: NSAppearance.Name = dark ? .darkAqua : .aqua
        guard let appearance = NSAppearance(named: name) else {
            let resolved = color.usingColorSpace(.sRGB) ?? color
            return String(format: "#%02x%02x%02x",
                Int(resolved.redComponent * 255),
                Int(resolved.greenComponent * 255),
                Int(resolved.blueComponent * 255))
        }
        var hex = "#000000"
        appearance.performAsCurrentDrawingAppearance {
            let resolved = color.usingColorSpace(.sRGB) ?? color
            hex = String(format: "#%02x%02x%02x",
                Int(resolved.redComponent * 255),
                Int(resolved.greenComponent * 255),
                Int(resolved.blueComponent * 255))
        }
        return hex
    }

    static func buildHTML(plainText: String, signature: String?, font: NSFont, textColor: NSColor, backgroundColor: NSColor, placeholder: String, vimInsertExitKeys: [String]) -> String {
        let exitKeysJS = "[" + vimInsertExitKeys.map { "\"\($0)\"" }.joined(separator: ",") + "]"
        let escapedText = plainText
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\n", with: "<br>")

        // Build content: user text + signature in one editable area
        let hasSignature = signature != nil && !signature!.isEmpty
        var content = escapedText.isEmpty && !hasSignature ? "" : (escapedText.isEmpty ? "<br>" : escapedText)
        if let sig = signature {
            content += "<br><span id=\"sig\">\(sig)</span>"
        }

        let lightTextHex = resolveHex(textColor, dark: false)
        let darkTextHex = resolveHex(textColor, dark: true)
        let lightBgHex = resolveHex(backgroundColor, dark: false)
        let darkBgHex = resolveHex(backgroundColor, dark: true)

        return """
        <!DOCTYPE html>
        <html>
        <head>
            <meta charset="UTF-8">
            <style>
                * { margin: 0; padding: 0; box-sizing: border-box; }
                ul, ol { padding-left: 1.5em; margin: 0.3em 0; }
                li > ul, li > ol { margin: 0; }
                html, body {
                    overflow: hidden;
                    height: auto;
                }
                body {
                    font-family: -apple-system, BlinkMacSystemFont, sans-serif;
                    font-size: \(font.pointSize)px;
                    line-height: 1.47;
                    color-scheme: light dark;
                    padding: 0;
                }
                @media (prefers-color-scheme: light) {
                    html, body { background-color: \(lightBgHex); color: \(lightTextHex); }
                }
                @media (prefers-color-scheme: dark) {
                    html, body { background-color: \(darkBgHex); color: \(darkTextHex); }
                }
                #editor {
                    outline: none;
                    min-height: 100px;
                    word-wrap: break-word;
                    padding: 8px 5px;
                }
            </style>
        </head>
        <body>
            <div id="editor" contenteditable="true">\(content)</div>
            <script>
                const editor = document.getElementById('editor');

                function notifyHeight() {
                    const h = document.body.scrollHeight;
                    window.webkit.messageHandlers.heightChanged.postMessage(h);
                }

                function getPlainText() {
                    // Get only the text before the signature marker
                    const sig = document.getElementById('sig');
                    let range;
                    if (sig) {
                        // Get text content before the sig element
                        range = document.createRange();
                        range.setStart(editor, 0);
                        range.setEndBefore(sig);
                    }

                    // Extract from the relevant portion
                    const container = document.createElement('div');
                    if (range) {
                        container.appendChild(range.cloneContents());
                    } else {
                        container.innerHTML = editor.innerHTML;
                    }

                    let html = container.innerHTML;
                    // Empty-line divs: <div><br></div> = single newline
                    html = html.replace(/<div><br\\s*\\/?><\\/div>/gi, '\\n');
                    // Block elements: opening tag = line break
                    html = html.replace(/<div[^>]*>/gi, '\\n');
                    html = html.replace(/<\\/div>/gi, '');
                    // Inline line breaks
                    html = html.replace(/<br\\s*\\/?>/gi, '\\n');
                    // Paragraphs
                    html = html.replace(/<p[^>]*>/gi, '\\n');
                    html = html.replace(/<\\/p>/gi, '');
                    // Strip remaining tags
                    html = html.replace(/<[^>]+>/g, '');
                    // Decode HTML entities
                    const ta = document.createElement('textarea');
                    ta.innerHTML = html;
                    let text = ta.value;
                    // Trim leading and trailing newlines
                    text = text.replace(/^\\n+/, '');
                    text = text.replace(/\\n+$/, '');
                    return text;
                }

                function getEditorHTML() {
                    // Get innerHTML of user content (excluding signature)
                    const sig = document.getElementById('sig');
                    if (sig) {
                        const range = document.createRange();
                        range.setStart(editor, 0);
                        range.setEndBefore(sig);
                        const container = document.createElement('div');
                        container.appendChild(range.cloneContents());
                        // Strip trailing empty blocks and <br> (visual separator before signature)
                        let html = container.innerHTML;
                        while (html !== (html = html.replace(/(<div><br\\s*\\/?><\\/div>|<div><\\/div>|<br\\s*\\/?>)\\s*$/i, ''))) {}
                        return html;
                    }
                    return editor.innerHTML;
                }

                // Save selection on every change so toolbar clicks can restore it
                var savedRange = null;
                document.addEventListener('selectionchange', function() {
                    const sel = window.getSelection();
                    if (sel.rangeCount > 0 && editor.contains(sel.anchorNode)) {
                        savedRange = sel.getRangeAt(0).cloneRange();
                    }
                });
                function restoreSelection() {
                    if (!savedRange) return;
                    editor.focus();
                    const sel = window.getSelection();
                    sel.removeAllRanges();
                    sel.addRange(savedRange);
                }

                function toggleList(tag) {
                    const sel = window.getSelection();
                    if (!sel.rangeCount) return;
                    // Check if cursor is already inside a list of this type
                    let node = sel.anchorNode;
                    while (node && node !== editor) {
                        if (node.nodeName === tag.toUpperCase()) {
                            // Unwrap: replace each <li> with a <div>
                            const parent = node.parentNode;
                            Array.from(node.children).forEach(function(li) {
                                const div = document.createElement('div');
                                div.innerHTML = li.innerHTML;
                                parent.insertBefore(div, node);
                            });
                            parent.removeChild(node);
                            notifyFormatState();
                            return;
                        }
                        node = node.parentNode;
                    }
                    // Create list from selection or current line
                    const range = sel.getRangeAt(0);
                    const text = range.toString();
                    const list = document.createElement(tag);
                    if (text.trim()) {
                        const lines = text.split('\\n');
                        lines.forEach(function(line) {
                            const li = document.createElement('li');
                            li.textContent = line || '';
                            list.appendChild(li);
                        });
                    } else {
                        const li = document.createElement('li');
                        li.innerHTML = '<br>';
                        list.appendChild(li);
                    }
                    range.deleteContents();
                    range.insertNode(list);
                    // Place cursor in first li
                    const firstLi = list.querySelector('li');
                    const nr = document.createRange();
                    nr.setStart(firstLi, firstLi.childNodes.length);
                    nr.collapse(true);
                    sel.removeAllRanges();
                    sel.addRange(nr);
                    notifyFormatState();
                }

                function notifyFormatState() {
                    const b = document.queryCommandState('bold');
                    const i = document.queryCommandState('italic');
                    const u = document.queryCommandState('underline');
                    const s = document.queryCommandState('strikeThrough');
                    let fs = 13;
                    let ff = 'Helvetica';
                    const sel = window.getSelection();
                    if (sel && sel.rangeCount > 0) {
                        let node = sel.anchorNode;
                        if (node && node.nodeType === 3) node = node.parentNode;
                        if (node && node.nodeType === 1) {
                            const cs = window.getComputedStyle(node);
                            fs = Math.round(parseFloat(cs.fontSize)) || 13;
                            const raw = cs.fontFamily;
                            const known = ['Helvetica', 'Arial', 'Times New Roman', 'Georgia', 'Courier'];
                            for (const k of known) {
                                if (raw.indexOf(k) !== -1) { ff = k; break; }
                            }
                        }
                    }
                    let align = 'left';
                    if (document.queryCommandState('justifyCenter')) align = 'center';
                    else if (document.queryCommandState('justifyRight')) align = 'right';
                    else if (document.queryCommandState('justifyFull')) align = 'justify';
                    window.webkit.messageHandlers.formatState.postMessage({bold: b, italic: i, underline: u, strikethrough: s, fontSize: fs, fontFamily: ff, alignment: align});
                }

                editor.addEventListener('input', function(e) {
                    // Auto-list: "- " → bullet list, "1. " → numbered list
                    if (e.inputType === 'insertText' && /^\\s$/.test(e.data)) {
                        const sel = window.getSelection();
                        if (sel.rangeCount > 0 && sel.anchorNode && sel.anchorNode.nodeType === 3) {
                            const node = sel.anchorNode;
                            const text = node.textContent;
                            const offset = sel.anchorOffset;
                            const before = text.substring(0, offset);
                            let listTag = null;
                            let prefixLen = 0;
                            if (/^-\\s$/.test(before)) {
                                listTag = 'ul';
                                prefixLen = 2;
                            } else if (/^\\d+\\.\\s$/.test(before)) {
                                listTag = 'ol';
                                prefixLen = before.length;
                            }
                            if (listTag) {
                                // Remove the prefix text, then create the list
                                const rest = text.substring(prefixLen);
                                node.textContent = rest;
                                // Place cursor at start
                                const r = document.createRange();
                                r.setStart(node, 0);
                                r.collapse(true);
                                sel.removeAllRanges();
                                sel.addRange(r);
                                toggleList(listTag);
                                return;
                            }
                        }
                    }
                    const text = getPlainText();
                    window.webkit.messageHandlers.textChanged.postMessage(text);
                    window.webkit.messageHandlers.htmlChanged.postMessage(getEditorHTML());
                    setTimeout(notifyHeight, 10);
                    notifyFormatState();
                });

                // Vim modal editing engine
                const vim = {
                    mode: 'insert',
                    register: '',
                    registerIsLine: false,
                    visual: '',
                    pending: '',
                    pendingCount: 0,
                    count: '',
                    exitSeqs: \(exitKeysJS),
                    insertPending: '',
                    insertTimer: null,

                    notifyMode() {
                        let m = this.mode;
                        if (this.visual === 'char') m = 'visual';
                        else if (this.visual === 'line') m = 'visual_line';
                        window.webkit.messageHandlers.vimModeChanged.postMessage(m);
                    },

                    setMode(m) {
                        this.mode = m;
                        this.visual = '';
                        this.pending = '';
                        this.count = '';
                        editor.classList.toggle('vim-normal', m === 'normal');
                        this.notifyMode();
                    },

                    getCurrentBlock() {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return null;
                        let node = sel.anchorNode;
                        if (node === editor) return editor.firstChild;
                        while (node && node.parentNode !== editor) node = node.parentNode;
                        return node;
                    },

                    // Select a text object (iw, aw, i", a", i(, a(, etc.)
                    // inner=true for 'i' objects, false for 'a' objects
                    // Get block text and cursor offset within it
                    getBlockContext() {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return null;
                        const block = this.getCurrentBlock();
                        if (!block) return null;
                        const text = block.textContent;
                        // Calculate cursor offset within block
                        const range = document.createRange();
                        range.setStart(block, 0);
                        range.setEnd(sel.focusNode, sel.focusOffset);
                        const pos = range.toString().length;
                        return { block, text, pos };
                    },

                    // Set selection by offset within a block element
                    setBlockSelection(block, start, end) {
                        const sel = window.getSelection();
                        const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
                        let offset = 0;
                        let startNode = null, startOff = 0, endNode = null, endOff = 0;
                        while (walker.nextNode()) {
                            const node = walker.currentNode;
                            const len = node.textContent.length;
                            if (!startNode && offset + len > start) {
                                startNode = node;
                                startOff = start - offset;
                            }
                            if (!endNode && offset + len >= end) {
                                endNode = node;
                                endOff = end - offset;
                            }
                            offset += len;
                            if (startNode && endNode) break;
                        }
                        if (!startNode || !endNode) return false;
                        const range = document.createRange();
                        range.setStart(startNode, startOff);
                        range.setEnd(endNode, endOff);
                        sel.removeAllRanges();
                        sel.addRange(range);
                        return true;
                    },

                    selectTextObject(obj, inner) {
                        const ctx = this.getBlockContext();
                        if (!ctx) return false;
                        const { block, text, pos } = ctx;

                        if (obj === 'w' || obj === 'W') {
                            const isWord = obj === 'w'
                                ? (c) => /\\w/.test(c)
                                : (c) => c !== ' ' && c !== '\\t' && c !== '\\n';
                            let start = pos, end = pos;
                            if (pos < text.length && isWord(text[pos])) {
                                while (start > 0 && isWord(text[start - 1])) start--;
                                while (end < text.length && isWord(text[end])) end++;
                            } else {
                                while (start > 0 && !isWord(text[start - 1])) start--;
                                while (end < text.length && !isWord(text[end])) end++;
                            }
                            if (!inner) {
                                while (end < text.length && text[end] === ' ') end++;
                                if (end === pos) { while (start > 0 && text[start - 1] === ' ') start--; }
                            }
                            return this.setBlockSelection(block, start, end);
                        }

                        // Paired delimiters
                        const pairs = { '(': ')', ')': ')', '{': '}', '}': '}', '[': ']', ']': ']', '<': '>', '>': '>' };
                        const quotes = ['"', "'", '`'];

                        if (quotes.includes(obj)) {
                            let start = text.lastIndexOf(obj, pos - 1);
                            let end = text.indexOf(obj, Math.max(pos, start + 1));
                            if (start === -1 && pos < text.length && text[pos] === obj) { start = pos; end = text.indexOf(obj, start + 1); }
                            if (start === -1 || end === -1) return false;
                            return this.setBlockSelection(block, inner ? start + 1 : start, inner ? end : end + 1);
                        }

                        if (pairs[obj]) {
                            const open = [')', '}', ']', '>'].includes(obj) ? Object.keys(pairs).find(k => pairs[k] === obj && k !== obj) : obj;
                            const close = pairs[open];
                            let depth = 0, start = -1;
                            for (let i = pos; i >= 0; i--) {
                                if (text[i] === close && i !== pos) depth++;
                                if (text[i] === open) { if (depth === 0) { start = i; break; } depth--; }
                            }
                            if (start === -1) return false;
                            depth = 0;
                            let endPos = -1;
                            for (let i = start + 1; i < text.length; i++) {
                                if (text[i] === open) depth++;
                                if (text[i] === close) { if (depth === 0) { endPos = i; break; } depth--; }
                            }
                            if (endPos === -1) return false;
                            return this.setBlockSelection(block, inner ? start + 1 : start, inner ? endPos : endPos + 1);
                        }

                        return false;
                    },

                    // Execute a motion in 'move' or 'extend' mode
                    execMotion(key, n, mode) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return false;
                        switch(key) {
                            case 'h': for(let i=0;i<n;i++) sel.modify(mode,'backward','character'); return true;
                            case 'l': for(let i=0;i<n;i++) sel.modify(mode,'forward','character'); return true;
                            case 'j': for(let i=0;i<n;i++) sel.modify(mode,'forward','line'); return true;
                            case 'k': for(let i=0;i<n;i++) sel.modify(mode,'backward','line'); return true;
                            case 'w': for(let i=0;i<n;i++) sel.modify(mode,'forward','word'); return true;
                            case 'b': for(let i=0;i<n;i++) sel.modify(mode,'backward','word'); return true;
                            case 'e': for(let i=0;i<n;i++) sel.modify(mode,'forward','word'); return true;
                            case '0': sel.modify(mode,'backward','lineboundary'); return true;
                            case '$': sel.modify(mode,'forward','lineboundary'); return true;
                            default: return false;
                        }
                    },

                    // Apply operator (d/c/y) on current selection
                    applyOperator(op) {
                        const sel = window.getSelection();
                        if (sel.isCollapsed) return;
                        const text = sel.toString();
                        this.register = text;
                        this.registerIsLine = false;
                        window.webkit.messageHandlers.vimYank.postMessage(text);
                        if (op === 'd' || op === 'c') {
                            document.execCommand('delete');
                            if (op === 'c') this.setMode('insert');
                        } else {
                            sel.collapseToStart();
                        }
                    },

                    // cc: change entire line content
                    changeLine(n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        sel.modify('move','backward','lineboundary');
                        for (let i = 0; i < n - 1; i++) sel.modify('extend','forward','line');
                        sel.modify('extend','forward','lineboundary');
                        if (!sel.isCollapsed) {
                            this.register = sel.toString();
                            this.registerIsLine = true;
                            window.webkit.messageHandlers.vimYank.postMessage(this.register);
                            document.execCommand('delete');
                        }
                        this.setMode('insert');
                    },

                    // Custom w motion for move mode (handles punctuation)
                    moveW(n) {
                        const sel = window.getSelection();
                        for(let i=0;i<n;i++) {
                            const wn = sel.focusNode;
                            if (wn && wn.nodeType === 3) {
                                const wt = wn.textContent;
                                let wp = sel.focusOffset;
                                if (wp < wt.length && /\\w/.test(wt[wp])) {
                                    while (wp < wt.length && /\\w/.test(wt[wp])) wp++;
                                    while (wp < wt.length && !/\\w/.test(wt[wp])) wp++;
                                } else {
                                    while (wp < wt.length && !/\\w/.test(wt[wp])) wp++;
                                }
                                if (wp > sel.focusOffset && wp <= wt.length) {
                                    const wr = document.createRange();
                                    wr.setStart(wn, wp);
                                    wr.collapse(true);
                                    sel.removeAllRanges();
                                    sel.addRange(wr);
                                } else {
                                    sel.modify('move','forward','word');
                                }
                            } else {
                                sel.modify('move','forward','word');
                            }
                        }
                    },

                    handleNormal(e) {
                        const key = e.key;
                        // Ignore bare modifier keys (Shift, Control, etc.)
                        if (['Shift', 'Control', 'Alt', 'Meta', 'CapsLock'].includes(key)) return;
                        if (e.ctrlKey && key === 'r') { document.execCommand('redo'); return; }

                        if (/^[1-9]$/.test(key) || (this.count && /^[0-9]$/.test(key))) {
                            this.count += key;
                            return;
                        }
                        const n = parseInt(this.count) || 1;
                        this.count = '';
                        const sel = window.getSelection();

                        // Pending operator handling
                        if (this.pending) {
                            const op = this.pending;
                            const combo = op + key;
                            const pn = this.pendingCount;
                            this.pending = '';
                            this.pendingCount = 0;

                            // r: replace char with next typed char
                            if (op === 'r' && key.length === 1 && key !== 'Escape') {
                                this.replaceChar(key, pn);
                                this.recordAction(['r', key]);
                                return;
                            }
                            if (op === 'r') return; // Escape cancels r

                            // f/t/F/T: find char on line
                            if ((op === 'f' || op === 't' || op === 'F' || op === 'T') && key.length === 1 && key !== 'Escape') {
                                const dir = (op === 'f' || op === 't') ? 'forward' : 'backward';
                                const till = (op === 't' || op === 'T');
                                for (let i = 0; i < pn; i++) this.findChar(key, dir, till);
                                return;
                            }
                            if (op === 'f' || op === 't' || op === 'F' || op === 'T') return;

                            // Visual mode text objects: viw, vi", etc.
                            if (op === 'visual_obj') {
                                const inner = this.textObjInner;
                                delete this.textObjInner;
                                this.selectTextObject(key, inner);
                                return;
                            }

                            // Line ops: dd, cc, yy, gg, >>, <<
                            if (combo === 'dd') { this.deleteLine(pn); this.recordAction(['dd']); return; }
                            if (combo === 'cc') { this.changeLine(pn); return; }
                            if (combo === 'yy') { this.yankLine(pn); return; }
                            if (combo === 'gg') { this.goToTop(); return; }
                            if (combo === '>>') { this.indentLine(pn); this.recordAction(['>>']); return; }
                            if (combo === '<<') { this.outdentLine(pn); this.recordAction(['<<']); return; }

                            // Text objects: di, ci, yi + next key; da, ca, ya + next key
                            if ((op === 'd' || op === 'c' || op === 'y') && (key === 'i' || key === 'a')) {
                                this.pending = op;
                                this.pendingCount = pn;
                                this.textObjInner = (key === 'i');
                                return;
                            }

                            // Resolve text object (e.g. diw, ci", ya()
                            if ((op === 'd' || op === 'c' || op === 'y') && this.textObjInner !== undefined) {
                                const inner = this.textObjInner;
                                delete this.textObjInner;
                                if (this.selectTextObject(key, inner)) {
                                    this.applyOperator(op);
                                    if (op === 'd') this.recordAction([op, inner ? 'i' : 'a', key]);
                                    return;
                                }
                                return;
                            }

                            // Operator + motion (dw, cw, yw, d$, etc.)
                            if ((op === 'd' || op === 'c' || op === 'y') && this.execMotion(key, pn, 'extend')) {
                                this.applyOperator(op);
                                if (op === 'd') this.recordAction([op, key]);
                                return;
                            }
                            return;
                        }

                        // Visual mode: operators apply on selection directly
                        if (this.visual && (key === 'd' || key === 'c' || key === 'y')) {
                            this.applyOperator(key);
                            this.visual = '';
                            this.notifyMode();
                            return;
                        }

                        // Visual mode: motions extend selection
                        const mot = this.visual ? 'extend' : 'move';

                        switch(key) {
                            // Visual mode toggles
                            case 'v':
                                if (this.visual === 'char') {
                                    this.visual = '';
                                    sel.collapseToEnd();
                                } else {
                                    this.visual = 'char';
                                    sel.modify('extend','forward','character');
                                }
                                this.notifyMode();
                                break;
                            case 'V':
                                if (this.visual === 'line') {
                                    this.visual = '';
                                    sel.collapseToEnd();
                                } else {
                                    this.visual = 'line';
                                    if (sel.rangeCount) {
                                        sel.modify('move','backward','lineboundary');
                                        sel.modify('extend','forward','lineboundary');
                                    }
                                }
                                this.notifyMode();
                                break;
                            // Insert mode entries (not in visual)
                            case 'i':
                                if (this.visual) { this.pending = 'visual_obj'; this.textObjInner = true; }
                                else this.setMode('insert');
                                break;
                            case 'a':
                                if (this.visual) { this.pending = 'visual_obj'; this.textObjInner = false; }
                                else { if (sel.rangeCount) sel.modify('move','forward','character'); this.setMode('insert'); }
                                break;
                            case 'I': if (!this.visual) { if (sel.rangeCount) sel.modify('move','backward','lineboundary'); this.setMode('insert'); } break;
                            case 'A': if (!this.visual) { if (sel.rangeCount) sel.modify('move','forward','lineboundary'); this.setMode('insert'); } break;
                            case 'o': if (!this.visual) { this.openLineBelow(); this.setMode('insert'); } break;
                            case 'O': if (!this.visual) { this.openLineAbove(); this.setMode('insert'); } break;
                            // Navigation (extend in visual, move in normal)
                            case 'h': for(let i=0;i<n;i++) sel.modify(mot,'backward','character'); break;
                            case 'l': for(let i=0;i<n;i++) sel.modify(mot,'forward','character'); break;
                            case 'j': for(let i=0;i<n;i++) sel.modify(mot,'forward','line'); break;
                            case 'k': for(let i=0;i<n;i++) sel.modify(mot,'backward','line'); break;
                            case 'w': if (this.visual) { for(let i=0;i<n;i++) sel.modify('extend','forward','word'); } else { this.moveW(n); } break;
                            case 'b': for(let i=0;i<n;i++) sel.modify(mot,'backward','word'); break;
                            case 'e': for(let i=0;i<n;i++) sel.modify(mot,'forward','word'); break;
                            case '0': if (sel.rangeCount) sel.modify(mot,'backward','lineboundary'); break;
                            case '$': if (sel.rangeCount) sel.modify(mot,'forward','lineboundary'); break;
                            case 'G': if (this.visual) { sel.modify('extend','forward','documentboundary'); } else { this.goToBottom(); } break;
                            // Editing
                            case 'x': this.deleteForward(n); this.recordAction(['x']); if (this.visual) { this.visual = ''; this.notifyMode(); } break;
                            case 'X': this.deleteBackward(n); if (this.visual) { this.visual = ''; this.notifyMode(); } break;
                            case 'u': document.execCommand('undo'); break;
                            case 'p': this.pasteAfter(); break;
                            case 'P': this.pasteBefore(); break;
                            // New: r (replace), ~ (toggle case), J (join)
                            case 'r': this.pending = 'r'; this.pendingCount = n; break;
                            case '~': this.toggleCase(n); this.recordAction(['~']); break;
                            case 'J': this.joinLines(n); this.recordAction(['J']); break;
                            // New: f/t/F/T (find char), ;/, (repeat find)
                            case 'f': this.pending = 'f'; this.pendingCount = n; break;
                            case 't': this.pending = 't'; this.pendingCount = n; break;
                            case 'F': this.pending = 'F'; this.pendingCount = n; break;
                            case 'T': this.pending = 'T'; this.pendingCount = n; break;
                            case ';': this.repeatFind(false); break;
                            case ',': this.repeatFind(true); break;
                            // New: . (repeat last action)
                            case '.': this.repeatLastAction(n); break;
                            // Shortcuts: C = c$, D = d$
                            case 'C': if (sel.rangeCount) { sel.modify('extend','forward','lineboundary'); this.applyOperator('c'); } break;
                            case 'D': if (sel.rangeCount) { sel.modify('extend','forward','lineboundary'); this.applyOperator('d'); this.recordAction(['D']); } break;
                            // Operators (pending, not in visual — visual handled above)
                            case 'd': this.pending = 'd'; this.pendingCount = n; break;
                            case 'c': this.pending = 'c'; this.pendingCount = n; break;
                            case 'y': this.pending = 'y'; this.pendingCount = n; break;
                            case 'g': this.pending = 'g'; this.pendingCount = n; break;
                            case '>': this.pending = '>'; this.pendingCount = n; break;
                            case '<': this.pending = '<'; this.pendingCount = n; break;
                            case '%': this.matchBracket(); break;
                            case '/': this.openSearch(); break;
                            case 'n': this.searchNext(false); break;
                            case 'N': this.searchNext(true); break;
                            case 'Escape':
                                if (this.visual) { this.visual = ''; sel.collapseToEnd(); this.notifyMode(); }
                                this.pending = '';
                                break;
                        }
                    },

                    deleteLine(n) {
                        let block = this.getCurrentBlock();
                        if (!block || block.id === 'sig') return;
                        const sel = window.getSelection();
                        let lines = [];
                        let lastBlock = block;
                        lines.push(block.textContent);
                        for (let i = 1; i < n; i++) {
                            const next = lastBlock.nextElementSibling || lastBlock.nextSibling;
                            if (!next || next.id === 'sig') break;
                            lines.push(next.textContent);
                            lastBlock = next;
                        }
                        this.register = lines.join('\\n');
                        this.registerIsLine = true;
                        window.webkit.messageHandlers.vimYank.postMessage(this.register);
                        const range = document.createRange();
                        range.setStartBefore(block);
                        range.setEndAfter(lastBlock);
                        sel.removeAllRanges();
                        sel.addRange(range);
                        document.execCommand('delete');
                        if (!editor.textContent.trim() && !editor.querySelector('#sig')) {
                            editor.innerHTML = '<br>';
                            this.placeCursorAt(editor);
                        }
                        this.notifyTextChange();
                    },

                    yankLine(n) {
                        let block = this.getCurrentBlock();
                        if (!block) return;
                        let lines = [];
                        for (let i = 0; i < n && block; i++) {
                            lines.push(block.textContent);
                            block = block.nextElementSibling || block.nextSibling;
                        }
                        this.register = lines.join('\\n');
                        this.registerIsLine = true;
                        window.webkit.messageHandlers.vimYank.postMessage(this.register);
                    },

                    indentLine(n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        const savedOffset = sel.focusOffset;
                        for (let i = 0; i < n; i++) {
                            sel.modify('move', 'backward', 'lineboundary');
                            document.execCommand('insertText', false, '\\t');
                        }
                        this.notifyTextChange();
                    },

                    outdentLine(n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        for (let i = 0; i < n; i++) {
                            sel.modify('move', 'backward', 'lineboundary');
                            // Check leading whitespace via selection
                            let removed = 0;
                            while (removed < 4) {
                                sel.modify('extend', 'forward', 'character');
                                const ch = sel.toString();
                                if (ch === ' ') {
                                    document.execCommand('delete');
                                    removed++;
                                } else if (ch === '\\t' && removed === 0) {
                                    document.execCommand('delete');
                                    break;
                                } else {
                                    sel.collapseToStart();
                                    break;
                                }
                            }
                        }
                        this.notifyTextChange();
                    },

                    matchBracket() {
                        const ctx = this.getBlockContext();
                        if (!ctx) return;
                        const { block, text, pos } = ctx;
                        const pairs = { '(': ')', ')': '(', '{': '}', '}': '{', '[': ']', ']': '[', '<': '>', '>': '<' };
                        const opens = new Set(['(', '{', '[', '<']);

                        // Find a bracket at or after cursor on current line
                        let bracketPos = -1;
                        for (let i = pos; i < text.length; i++) {
                            if (text[i] === '\\n') break;
                            if (pairs[text[i]]) { bracketPos = i; break; }
                        }
                        // Also check at cursor
                        if (bracketPos === -1 && pos < text.length && pairs[text[pos]]) bracketPos = pos;
                        if (bracketPos === -1) return;

                        const ch = text[bracketPos];
                        const match = pairs[ch];
                        const forward = opens.has(ch);
                        let depth = 0;
                        let targetPos = -1;

                        if (forward) {
                            for (let i = bracketPos + 1; i < text.length; i++) {
                                if (text[i] === ch) depth++;
                                if (text[i] === match) { if (depth === 0) { targetPos = i; break; } depth--; }
                            }
                        } else {
                            for (let i = bracketPos - 1; i >= 0; i--) {
                                if (text[i] === ch) depth++;
                                if (text[i] === match) { if (depth === 0) { targetPos = i; break; } depth--; }
                            }
                        }
                        if (targetPos === -1) return;

                        // Move cursor to matched bracket
                        const sel = window.getSelection();
                        const walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT);
                        let offset = 0;
                        while (walker.nextNode()) {
                            const node = walker.currentNode;
                            const len = node.textContent.length;
                            if (offset + len > targetPos) {
                                const range = document.createRange();
                                range.setStart(node, targetPos - offset);
                                range.collapse(true);
                                sel.removeAllRanges();
                                sel.addRange(range);
                                return;
                            }
                            offset += len;
                        }
                    },

                    // / search
                    lastSearch: '',

                    openSearch() {
                        window.webkit.messageHandlers.vimSearch.postMessage({});
                    },

                    doSearch(query) {
                        console.log('[vimsearch] doSearch called:', query);
                        if (query) {
                            this.lastSearch = query;
                            this.searchNext(false);
                        }
                        editor.focus();
                    },

                    searchNext(reverse) {
                        console.log('[vimsearch] searchNext reverse=' + reverse, 'lastSearch=' + this.lastSearch);
                        if (!this.lastSearch) return;
                        const sel = window.getSelection();
                        // Collapse to end of current selection to search forward
                        if (!reverse && sel.rangeCount && !sel.isCollapsed) {
                            sel.collapseToEnd();
                        } else if (reverse && sel.rangeCount && !sel.isCollapsed) {
                            sel.collapseToStart();
                        }
                        const found = window.find(this.lastSearch, false, reverse, true);
                        if (!found) {
                            // Wrap around: move to start/end and try again
                            const range = document.createRange();
                            if (reverse) {
                                range.selectNodeContents(editor);
                                range.collapse(false);
                            } else {
                                range.setStart(editor, 0);
                                range.collapse(true);
                            }
                            sel.removeAllRanges();
                            sel.addRange(range);
                            window.find(this.lastSearch, false, reverse, true);
                        }
                    },

                    openLineBelow() {
                        const block = this.getCurrentBlock();
                        const div = document.createElement('div');
                        div.innerHTML = '<br>';
                        if (block && block.nextSibling) editor.insertBefore(div, block.nextSibling);
                        else editor.appendChild(div);
                        this.placeCursorAt(div);
                        this.notifyTextChange();
                    },

                    openLineAbove() {
                        const block = this.getCurrentBlock();
                        const div = document.createElement('div');
                        div.innerHTML = '<br>';
                        if (block) editor.insertBefore(div, block);
                        else editor.appendChild(div);
                        this.placeCursorAt(div);
                        this.notifyTextChange();
                    },

                    doPaste(text, before) {
                        if (!text) return;
                        editor.focus();
                        const sel = window.getSelection();
                        if (this.registerIsLine && text === this.register) {
                            if (before) {
                                if (sel.rangeCount) sel.modify('move','backward','lineboundary');
                                document.execCommand('insertParagraph');
                                sel.modify('move','backward','line');
                            } else {
                                if (sel.rangeCount) sel.modify('move','forward','lineboundary');
                                document.execCommand('insertParagraph');
                            }
                            document.execCommand('insertText', false, text);
                        } else {
                            document.execCommand('insertText', false, text);
                        }
                        this.notifyTextChange();
                    },

                    pasteAfter() {
                        window.webkit.messageHandlers.vimPaste.postMessage({before: false});
                    },

                    pasteBefore() {
                        window.webkit.messageHandlers.vimPaste.postMessage({before: true});
                    },

                    deleteForward(n) {
                        for (let i = 0; i < n; i++) document.execCommand('forwardDelete');
                    },

                    deleteBackward(n) {
                        for (let i = 0; i < n; i++) document.execCommand('delete');
                    },

                    goToTop() {
                        const range = document.createRange();
                        range.setStart(editor, 0);
                        range.collapse(true);
                        const sel = window.getSelection();
                        sel.removeAllRanges();
                        sel.addRange(range);
                    },

                    goToBottom() {
                        const sel = window.getSelection();
                        const range = document.createRange();
                        range.selectNodeContents(editor);
                        range.collapse(false);
                        sel.removeAllRanges();
                        sel.addRange(range);
                    },

                    placeCursorAt(node) {
                        const sel = window.getSelection();
                        const range = document.createRange();
                        if (node.childNodes.length > 0 && node.childNodes[0].nodeType === 3) {
                            range.setStart(node.childNodes[0], 0);
                        } else {
                            range.setStart(node, 0);
                        }
                        range.collapse(true);
                        sel.removeAllRanges();
                        sel.addRange(range);
                    },

                    notifyTextChange() {
                        window.webkit.messageHandlers.textChanged.postMessage(getPlainText());
                        setTimeout(notifyHeight, 10);
                    },

                    // r: replace char under cursor with next typed char
                    replaceChar(ch, n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        for (let i = 0; i < n; i++) {
                            sel.modify('extend', 'forward', 'character');
                            document.execCommand('insertText', false, ch);
                        }
                        // Move cursor back to last replaced char
                        sel.modify('move', 'backward', 'character');
                        this.notifyTextChange();
                    },

                    // ~: toggle case of char under cursor
                    toggleCase(n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        for (let i = 0; i < n; i++) {
                            sel.modify('extend', 'forward', 'character');
                            const ch = sel.toString();
                            if (!ch) break;
                            const toggled = ch === ch.toUpperCase() ? ch.toLowerCase() : ch.toUpperCase();
                            document.execCommand('insertText', false, toggled);
                        }
                        this.notifyTextChange();
                    },

                    // J: join current line with next line
                    joinLines(n) {
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        for (let i = 0; i < n; i++) {
                            sel.modify('move', 'forward', 'lineboundary');
                            // Delete the line break and any leading whitespace on next line
                            sel.modify('extend', 'forward', 'character');
                            const br = sel.toString();
                            if (!br || br === '') break;
                            document.execCommand('insertText', false, ' ');
                            // Trim leading whitespace of the joined line
                            while (true) {
                                sel.modify('extend', 'forward', 'character');
                                const c = sel.toString();
                                if (c === ' ' || c === '\\t') {
                                    document.execCommand('delete');
                                } else {
                                    sel.collapseToStart();
                                    break;
                                }
                            }
                        }
                        this.notifyTextChange();
                    },

                    // f/t/F/T: find char on current line
                    lastFind: null,
                    findChar(ch, direction, till) {
                        this.lastFind = { ch, direction, till };
                        const sel = window.getSelection();
                        if (!sel.rangeCount) return;
                        const node = sel.focusNode;
                        if (!node || node.nodeType !== 3) return;
                        const text = node.textContent;
                        let pos = sel.focusOffset;
                        if (direction === 'forward') {
                            const idx = text.indexOf(ch, pos + 1);
                            if (idx === -1) return;
                            const target = till ? idx - 1 : idx;
                            if (target <= pos) return;
                            const mot = this.visual ? 'extend' : 'move';
                            for (let i = 0; i < target - pos; i++) sel.modify(mot, 'forward', 'character');
                        } else {
                            const idx = text.lastIndexOf(ch, pos - 1);
                            if (idx === -1) return;
                            const target = till ? idx + 1 : idx;
                            if (target >= pos) return;
                            const mot = this.visual ? 'extend' : 'move';
                            for (let i = 0; i < pos - target; i++) sel.modify(mot, 'backward', 'character');
                        }
                    },

                    // ;/,: repeat last f/t/F/T
                    repeatFind(reverse) {
                        if (!this.lastFind) return;
                        const { ch, direction, till } = this.lastFind;
                        const dir = reverse ? (direction === 'forward' ? 'backward' : 'forward') : direction;
                        this.findChar(ch, dir, till);
                    },

                    // . repeat: last editing action
                    lastAction: null,

                    recordAction(keys) {
                        this.lastAction = keys;
                    },

                    repeatLastAction(n) {
                        if (!this.lastAction) return;
                        const saved = this.lastAction;
                        this.lastAction = null; // prevent overwrite during replay
                        if (n > 1) this.count = String(n);
                        for (const k of saved) {
                            this.handleNormal({ key: k, ctrlKey: false, preventDefault() {} });
                        }
                        this.lastAction = saved; // restore
                    }
                };

                // Unified keydown handler (vim + tab)
                editor.addEventListener('keydown', function(e) {
                    // Normal mode: vim handles everything
                    if (vim.mode === 'normal') {
                        if (e.metaKey) return;
                        e.preventDefault();
                        vim.handleNormal(e);
                        return;
                    }

                    // Insert mode: Escape enters normal mode
                    if (e.key === 'Escape') {
                        e.preventDefault();
                        vim.insertPending = '';
                        clearTimeout(vim.insertTimer);
                        vim.setMode('normal');
                        const sel = window.getSelection();
                        if (sel.rangeCount) sel.modify('move', 'backward', 'character');
                        return;
                    }

                    // Insert mode: configurable exit sequences (e.g. jk)
                    if (vim.exitSeqs.length > 0 && e.key.length === 1) {
                        const pending = vim.insertPending + e.key;
                        if (vim.exitSeqs.indexOf(pending) !== -1) {
                            e.preventDefault();
                            clearTimeout(vim.insertTimer);
                            vim.insertPending = '';
                            for (let i = 0; i < pending.length - 1; i++) document.execCommand('delete');
                            vim.setMode('normal');
                            const esel = window.getSelection();
                            if (esel.rangeCount) esel.modify('move', 'backward', 'character');
                            return;
                        }
                        const hasPartial = vim.exitSeqs.some(function(s) { return s.startsWith(pending) && s !== pending; });
                        if (hasPartial) {
                            vim.insertPending = pending;
                            clearTimeout(vim.insertTimer);
                            vim.insertTimer = setTimeout(function() { vim.insertPending = ''; }, 200);
                            return;
                        }
                        vim.insertPending = '';
                        clearTimeout(vim.insertTimer);
                    }

                    // Insert mode: Tab handling
                    if (e.key === 'Tab') {
                        e.preventDefault();
                        let node = window.getSelection().anchorNode;
                        let inList = false;
                        while (node && node !== editor) {
                            if (node.nodeName === 'UL' || node.nodeName === 'OL') { inList = true; break; }
                            node = node.parentNode;
                        }
                        if (inList) {
                            document.execCommand(e.shiftKey ? 'outdent' : 'indent', false, null);
                        } else {
                            document.execCommand('insertText', false, '\\t');
                        }
                    }
                });

                // Track bold/italic/underline state on selection/cursor change
                document.addEventListener('selectionchange', notifyFormatState);

                // MutationObserver: catch all DOM changes (formatting, Cmd+B, etc.)
                new MutationObserver(function() {
                    window.webkit.messageHandlers.htmlChanged.postMessage(getEditorHTML());
                    notifyFormatState();
                }).observe(editor, { childList: true, subtree: true, characterData: true, attributes: true });

                // Update signature without reloading (preserves user formatting)
                function updateSignature(html) {
                    const sig = document.getElementById('sig');
                    if (html && html.length > 0) {
                        if (sig) {
                            sig.innerHTML = html;
                        } else {
                            const br = document.createElement('br');
                            const span = document.createElement('span');
                            span.id = 'sig';
                            span.innerHTML = html;
                            editor.appendChild(br);
                            editor.appendChild(span);
                        }
                    } else {
                        if (sig) {
                            // Remove the <br> before sig too
                            const prev = sig.previousSibling;
                            if (prev && prev.nodeName === 'BR') prev.remove();
                            sig.remove();
                        }
                    }
                    setTimeout(notifyHeight, 10);
                }

                // Initial height
                setTimeout(notifyHeight, 50);
            </script>
        </body>
        </html>
        """
    }

}
