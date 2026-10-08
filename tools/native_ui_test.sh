#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
    echo "Native UI tests require macOS 26 or newer and Xcode." >&2
    exit 1
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$(mktemp -d "${TMPDIR:-/tmp}/stability-ui.XXXXXX")"
trap 'rm -rf "$OUT"' EXIT
mkdir -p "$OUT/stability-tmp" "$OUT/module-cache"
export TMPDIR="$OUT/stability-tmp/"
export CLANG_MODULE_CACHE_PATH="$OUT/module-cache"
export SWIFT_MODULECACHE_PATH="$OUT/module-cache"
TARGET="$(uname -m)-apple-macosx26.0"
ARTIFACT_DIR="${NATIVE_UI_ARTIFACT_DIR:-$ROOT/.amp/in/artifacts}"
SCREENSHOT="$ARTIFACT_DIR/native-ui-email-detail-loaded.png"

mkdir -p "$OUT/home/config" "$OUT/home/data" "$OUT/home/state" "$ARTIFACT_DIR"
export HOME="$OUT/home"
export CFFIXED_USER_HOME="$OUT/home"
export XDG_CACHE_HOME="$OUT/cache"
cd "$ROOT"

sources="$(rg --files macos/durian -g '*.swift' | rg -v 'macos/durian/DurianApp.swift')"
HARNESS=macos/tests/native/UITestHarness.swift
FLAGS=""
if [[ "${NATIVE_UI_SCENE:-0}" == 1 ]]; then
    HARNESS="$OUT/SceneHarness.swift"
    cat macos/tests/native/UITestHarness.swift > "$HARNESS"
    cat >> "$HARNESS" <<'EOF'

@main
private struct NativeSceneApp: App {
    private let runner: NativeUITestRunner
    init() {
        runner = NativeUITestRunner()
        runner.configure()
        MockHTTP.contentFixtureEnabled = true
        NSApplication.shared.appearance = NSAppearance(named: ProcessInfo.processInfo.environment["NATIVE_UI_APPEARANCE"] == "dark" ? .darkAqua : .aqua)
    }
    var body: some Scene {
        WindowGroup {
            ContentView().onAppear {
                Timer.scheduledTimer(withTimeInterval: 1, repeats: false) { _ in
                    MainActor.assumeIsolated {
                        do { try runner.run(); exit(0) }
                        catch { fputs("SCENE HARNESS ERROR: \(error)\n", stderr); exit(1) }
                    }
                }
            }
        }
EOF
    # Execute the exact production compose scene without DurianApp.init's
    # notification authorization and external CLI startup side effects.
    sed -n '/        \/\/ Compose Window - supports multiple windows via UUID/,/        \.defaultSize(width: 650, height: 550)/p' macos/durian/DurianApp.swift >> "$HARNESS"
    printf '\n    }\n}\n' >> "$HARNESS"
    FLAGS="-D UI_WINDOW_SCENE"
fi
xcrun swiftc \
    -target "$TARGET" \
    -module-cache-path "$OUT/module-cache" \
    -whole-module-optimization \
    -emit-executable \
    -suppress-warnings \
    $FLAGS \
    $sources \
    "$HARNESS" \
    -o "$OUT/native-ui-test"

APPEARANCES="${NATIVE_UI_APPEARANCE:-light}"
if [[ "$APPEARANCES" == both ]]; then APPEARANCES="light dark"; fi
for appearance in $APPEARANCES; do
    RUN_SCREENSHOT="$SCREENSHOT"
    if [[ "${NATIVE_UI_APPEARANCE:-light}" == both ]]; then
        mkdir -p "$ARTIFACT_DIR/$appearance"
        RUN_SCREENSHOT="$ARTIFACT_DIR/$appearance/native-ui-email-detail-loaded.png"
    fi
    echo "NATIVE_UI_RUN appearance=$appearance scene=${NATIVE_UI_SCENE:-0}"
    HOME="$OUT/home" \
    CFFIXED_USER_HOME="$OUT/home" \
    XDG_CONFIG_HOME="$OUT/home/config" \
    XDG_DATA_HOME="$OUT/home/data" \
    XDG_STATE_HOME="$OUT/home/state" \
    NATIVE_UI_APPEARANCE="$appearance" \
    NATIVE_UI_SCREENSHOT="$RUN_SCREENSHOT" \
        /usr/bin/sandbox-exec -p '(version 1)(allow default)(deny network*)' "$OUT/native-ui-test"
done

echo "NATIVE_UI_TEST_EXIT=0"
echo "NATIVE_UI_SCREENSHOT=$SCREENSHOT"
