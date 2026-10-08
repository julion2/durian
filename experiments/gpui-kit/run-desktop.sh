#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
# Keep the optional standalone WebKit fallback beside GPUI.
cargo build --locked --manifest-path ../mail-webview/Cargo.toml --target-dir "${CARGO_TARGET_DIR:-target}"
# macOS embeds the system WKWebView directly. Linux retains the CEF prototype
# until GPUI can host WebKitGTK in its Wayland window; only that path downloads
# the pinned Chromium runtime on first build (~300 MiB).
if [[ "$(uname -s)" == Linux && -z "${DURIAN_CEF_HELPER:-}" ]]; then
    cargo build --locked --manifest-path ../mail-cef/Cargo.toml --target-dir ../mail-cef/target
    export DURIAN_CEF_HELPER="$(pwd)/../mail-cef/target/debug/durian-mail-cef"
fi
# A minimal Linux desktop may provide Wayland but no session bus. The native
# Save dialog needs one; inherit a real desktop session whenever available.
if [[ "$(uname -s)" == Linux && -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
    exec dbus-run-session -- cargo run --locked -- "$@"
fi
exec cargo run --locked -- "$@"
