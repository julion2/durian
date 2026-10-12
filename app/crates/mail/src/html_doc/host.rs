//! Trusted page integration for a live renderer, independent of native windowing and GPUI.
//! This script belongs to the host, not the mail: sender scripts remain forbidden by CSP.

const DARK_MODE: &str = include_str!("../webview/dark_mode.js");

#[cfg(target_os = "macos")]
pub(crate) const SELECT_ALL: &str = "const r=document.createRange();r.selectNodeContents(document.body);const s=getSelection();s.removeAllRanges();s.addRange(r);window.ipc.postMessage('selection:'+s.toString());";

/// The page's side: report the height, hand link clicks to the app, track the selection, and
/// in dark mode transform the mail's colors onto the card's. A live engine must provide the
/// `window.ipc.postMessage` bridge and execute this as a privileged initialization script.
pub(crate) fn script(dark: bool, background: [u8; 3]) -> String {
    let [r, g, b] = background;
    let dark_mode = if dark {
        format!(
            "{}\ndocument.documentElement.style.setProperty('background','rgb({r},{g},{b})','important');\
             document.body.style.setProperty('background-color','rgb({r},{g},{b})','important');\
             document.documentElement.style.colorScheme='dark';",
            DARK_MODE.replace("const DARK_BG = [42, 42, 44];", &format!("const DARK_BG = [{r}, {g}, {b}];"))
        )
    } else {
        String::new()
    };
    format!(
        r#"addEventListener('DOMContentLoaded', () => {{
            {dark_mode}
            const height = () => window.ipc.postMessage('height:' + Math.max(document.documentElement.scrollHeight, document.body.scrollHeight));
            new ResizeObserver(height).observe(document.body);
            height();
        }}, {{once: true}});
        addEventListener('click', event => {{
            const link = event.target.closest('a');
            if (link && !(link.getAttribute('href') || '').startsWith('#')) {{
                event.preventDefault();
                window.ipc.postMessage('link:' + (link.getAttribute('href') || ''));
            }}
        }}, true);
        addEventListener('selectionchange', () => window.ipc.postMessage('selection:' + getSelection().toString()));
        // what's under the pointer: the app draws the cursor and names the link
        let cursor = '', hover = '';
        addEventListener('mousemove', event => {{
            const link = event.target.closest ? event.target.closest('a[href]') : null;
            let kind = 'default';
            if (link) {{
                kind = 'pointer';
            }} else {{
                const range = document.caretRangeFromPoint(event.clientX, event.clientY);
                if (range && range.startContainer.nodeType === 3 && /\S/.test(range.startContainer.textContent)) kind = 'text';
            }}
            if (kind !== cursor) {{ cursor = kind; window.ipc.postMessage('cursor:' + kind); }}
            const url = link ? link.href : '';
            if (url !== hover) {{ hover = url; window.ipc.postMessage('hover:' + url); }}
        }}, {{passive: true}});
        document.addEventListener('mouseleave', () => {{
            if (hover) {{ hover = ''; window.ipc.postMessage('hover:'); }}
        }});
        // the app's menu, not WebKit's
        addEventListener('contextmenu', event => {{
            event.preventDefault();
            const link = event.target.closest ? event.target.closest('a[href]') : null;
            window.ipc.postMessage('menu:' + JSON.stringify({{x: event.clientX, y: event.clientY, link: link ? link.href : '', selection: getSelection().toString()}}));
        }}, true);"#
    )
}

#[cfg(test)]
mod tests {
    #[test]
    fn dark_mode_takes_the_cards_color() {
        let script = super::script(true, [30, 30, 32]);
        assert!(script.contains("const DARK_BG = [30, 30, 32];"), "the transform's background is the card's");
        assert!(script.contains("MIN_CONTRAST = 4.5"));
        assert!(script.contains(r"/rgba?\((\d+),\s*(\d+),\s*(\d+)/"), "the regex survived extraction");
        let light = super::script(false, [255, 255, 255]);
        assert!(!light.contains("DARK_BG"));
        // the page reports cursor, hovered link and right-clicks; WebKit's own menu is suppressed
        for part in ["'cursor:'", "'hover:'", "'menu:'", "event.preventDefault()", r"/\S/.test"] {
            assert!(light.contains(part), "missing {part}");
        }
    }
}
