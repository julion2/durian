#!/usr/bin/env bash
# Source-only transfer for isolated verification, including out-of-crate inputs.
set -euo pipefail
if [[ $# != 1 ]]; then
    echo "Usage: $0 /tmp/gpui-source.tgz" >&2
    exit 2
fi
root="$(dirname -- "${BASH_SOURCE[0]}")/../../.."
tar -czf "$1" -C "$root" \
    experiments/README.md \
    experiments/gpui-kit/Cargo.toml experiments/gpui-kit/Cargo.lock \
    experiments/gpui-kit/src experiments/gpui-kit/fixtures \
    experiments/gpui-kit/verify-macos.sh \
    experiments/gpui-kit/verification/keyboard.swift \
    experiments/gpui-kit/verification/verify-keyboard.py \
    experiments/gpui-kit/verification/diagnose-keyboard.py \
    experiments/gpui-kit/verification/test_diagnosis.py \
    experiments/gpui-kit/verification/pack-source.sh \
    experiments/gpui/src/data.rs experiments/mail-cef/src/protocol.rs \
    experiments/mail-webview/src/document.rs \
    macos/durian/Utilities/DarkModeTransform.swift schema/Config.pkl
