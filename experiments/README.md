# GPUI experiments

> **Spike, not a product.** Native Rust experiments for Durian; these do not
> replace the Swift or Qt clients.

`gpui/` is the initial three-pane Linux experiment built directly on Zed's GPUI.
It includes local SVG icons, light/dark themes and a mail API adapter. The
adapter reads the existing Durian HTTP API, never the SQLite database, and uses
sample mail if the local server is unavailable. API access currently requires
`durian serve --no-auth`; authenticated connections are not implemented.

```bash
cd experiments/gpui
cargo run --locked
cargo test --locked
```

The GPUI revision is pinned in Cargo.toml and Cargo.lock. A first build takes
several minutes and multiple GB of disk space. Linux needs Vulkan, Wayland or
X11 development libraries. This is not a packaging or scalability benchmark.

The shared mail model and HTTP client are in `gpui/src/data.rs`. Subsequent
experiments can reuse them without coupling to this prototype's render tree.
