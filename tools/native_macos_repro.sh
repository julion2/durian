#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
    echo "Native checks require macOS 26 and Xcode." >&2
    exit 1
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$(mktemp -d "${TMPDIR:-/tmp}/stability-subset.XXXXXX")"
trap 'rm -rf "$OUT"' EXIT
mkdir -p "$OUT/stability-tmp" "$OUT/module-cache" "$OUT/home"
export TMPDIR="$OUT/stability-tmp/"
export HOME="$OUT/home"
export CFFIXED_USER_HOME="$OUT/home"
export CLANG_MODULE_CACHE_PATH="$OUT/module-cache"
export SWIFT_MODULECACHE_PATH="$OUT/module-cache"
export XDG_CACHE_HOME="$OUT/cache"
TARGET="$(uname -m)-apple-macosx26.0"
cd "$ROOT"
sw_vers
xcodebuild -version

package="$OUT/stability-package"
mkdir -p "$package/Sources/durian_lib" "$package/Tests/durian_libTests"
cat > "$package/Package.swift" <<'EOF'
// swift-tools-version: 6.2
import PackageDescription
let package = Package(
    name: "NativeSubset",
    platforms: [.macOS(.v26)],
    products: [.library(name: "durian_lib", targets: ["durian_lib"])],
    targets: [
        .target(name: "durian_lib"),
        .testTarget(name: "durian_libTests", dependencies: ["durian_lib"]),
    ]
)
EOF
cp macos/durian/Models/{AttachmentModels,Mail,EmailComposition,EmailDraftFactory}.swift \
    macos/durian/Managers/AttachmentCacheManager.swift \
    macos/durian/Utilities/NightlyDiagnostics.swift \
    macos/tests/native/FactorySubsetSupport.swift "$package/Sources/durian_lib/"
cp macos/tests/{EmailDraftFactoryTests,EmailDraftFactoryDefectReproductionTests,NightlyDiagnosticsTests,AttachmentCacheManagerTests}.swift \
    "$package/Tests/durian_libTests/"
swift test --package-path "$package" --disable-sandbox \
    --cache-path "$OUT/swiftpm-cache" --config-path "$OUT/swiftpm-config" \
    --security-path "$OUT/swiftpm-security" --scratch-path "$OUT/build" \
    -Xswiftc -module-cache-path -Xswiftc "$OUT/module-cache"

echo "== Full app lifecycle typecheck (no execution) =="
sources="$(rg --files macos/durian -g '*.swift')"
xcrun swiftc -target "$TARGET" -module-cache-path "$OUT/module-cache" \
    -typecheck -suppress-warnings $sources
echo "FULL_APP_TYPECHECK=PASS"

echo "== Async cache write/read and main-actor progress =="
xcrun swiftc -target "$TARGET" -module-cache-path "$OUT/module-cache" \
    macos/durian/Models/{AttachmentModels,Mail}.swift \
    macos/durian/Managers/AttachmentCacheManager.swift \
    macos/tests/native/AttachmentCacheHarness.swift -o "$OUT/cache-harness"
"$OUT/cache-harness"
