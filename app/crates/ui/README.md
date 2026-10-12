# ui

Design system for the mail client, built on [GPUI](https://www.gpui.rs): theme tokens (light and
dark) and the components every view is built from.

```rust
fn main() {
    gpui_platform::application().with_assets(MyAssets).run(|cx| {
        ui::init(cx).expect("fonts");         // Geist + component key bindings
        ui::theme::apply_appearance(cx);      // follows macOS light/dark
        // open windows…
    });
}
```

Serve `ui::Assets` behind your own `AssetSource` (it provides `fonts/…` and `icons/lucide/…`).

## Contents

| Module | What |
|---|---|
| `theme` | `Theme` tokens (defined in LCH), `Appearance` (system / light / dark), type ramp |
| `icon` | `Icon`, `IconName` (Lucide), `IconSource` for app-provided SVGs |
| `button`, `chip`, `kbd`, `avatar`, `nav` | small building blocks |
| `picker` | command-menu style picker over a `PickerDelegate` |
| `context_menu`, `dialog`, `empty_state` | overlays and placeholders |
| `text_input`, `text_area` | single-line field, multi-line editor with live Markdown |
| `markdown` | parser, live editor rendering, message rendering, HTML export |

## Icons

`tools/icons/map.tsv` maps each `IconName` file to a Lucide icon; `tools/icons/build.py` copies
them into `assets/icons/lucide/`. A test fails if an `IconName` has no SVG.

## Licenses of bundled assets

- Geist / Geist Mono: SIL Open Font License 1.1, `assets/fonts/geist/OFL.txt`
- Lucide: ISC, `assets/icons/lucide/CREDITS.md`
