#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "build-macos.sh requires macOS" >&2
    exit 1
fi

script_dir="$(cd "$(dirname "$0")" && pwd -P)"
target_dir="${CARGO_TARGET_DIR:-$script_dir/target}"
mkdir -p "$target_dir"
target_dir="$(cd "$target_dir" && pwd -P)"

# A stable CEF_PATH makes the downloaded framework available to both Cargo and
# the bundling step. cef-dll-sys validates and downloads CEF 154.0.33 here.
cef_root="${CEF_PATH:-$target_dir/cef-cache}"
mkdir -p "$cef_root"
cef_root="$(cd "$cef_root" && pwd -P)"

export CARGO_TARGET_DIR="$target_dir"
export CEF_PATH="$cef_root"

echo "Building sandboxed CEF browser and subprocess helper..." >&2
(
    cd "$script_dir"
    cargo build --locked --bin durian-mail-cef
    cargo build --locked --bin durian-mail-cef-helper
) >&2

framework="$(find "$cef_root" -type d -name 'Chromium Embedded Framework.framework' -print -quit)"
if [[ -z "$framework" ]]; then
    echo "Could not locate the downloaded Chromium Embedded Framework.framework under $cef_root" >&2
    exit 1
fi

bundle_root="$target_dir/macos-bundle"
app_name="durian-mail-cef"
app="$bundle_root/$app_name.app"
contents="$app/Contents"
frameworks="$contents/Frameworks"
main_executable="$contents/MacOS/$app_name"
helper_binary="$target_dir/debug/durian-mail-cef-helper"

echo "Bundling CEF framework and helper applications..." >&2
rm -rf "$app"
mkdir -p "$contents/MacOS" "$contents/Resources" "$frameworks"
cp "$target_dir/debug/$app_name" "$main_executable"
ditto "$framework" "$frameworks/Chromium Embedded Framework.framework"

write_plist() {
    local destination="$1"
    local executable="$2"
    local helper="$3"
    local ui_element=""
    if [[ "$helper" == "true" ]]; then
        ui_element='<key>LSUIElement</key><string>1</string>'
    fi
    cat >"$destination" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleDevelopmentRegion</key><string>English</string>
    <key>CFBundleDisplayName</key><string>Durian Mail CEF</string>
    <key>CFBundleExecutable</key><string>$executable</string>
    <key>CFBundleIdentifier</key><string>org.js-lab.durian.mail-cef</string>
    <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
    <key>CFBundleName</key><string>Durian Mail CEF</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>0.1.0</string>
    <key>CFBundleVersion</key><string>0.1.0</string>
    <key>LSEnvironment</key><dict><key>MallocNanoZone</key><string>0</string></dict>
    <key>LSFileQuarantineEnabled</key><true/>
    <key>LSMinimumSystemVersion</key><string>11.0</string>
    $ui_element
    <key>NSSupportsAutomaticGraphicsSwitching</key><true/>
</dict>
</plist>
EOF
}

# This process only paints into GPUI; it must not add a second Dock application.
write_plist "$contents/Info.plist" "$app_name" true

for suffix in 'Helper (GPU)' 'Helper (Renderer)' 'Helper (Plugin)' 'Helper (Alerts)' 'Helper'; do
    helper_name="$app_name $suffix"
    helper_app="$frameworks/$helper_name.app"
    mkdir -p "$helper_app/Contents/MacOS" "$helper_app/Contents/Resources" "$helper_app/Contents/Frameworks"
    cp "$helper_binary" "$helper_app/Contents/MacOS/$helper_name"
    write_plist "$helper_app/Contents/Info.plist" "$helper_name" true
done

chmod +x "$main_executable"
find "$frameworks" -path '*/Contents/MacOS/*' -type f -exec chmod +x {} +
plutil -lint "$contents/Info.plist" "$frameworks"/*.app/Contents/Info.plist >&2

# stdout is an API consumed by run-desktop.sh. Keep it to this one path.
printf '%s\n' "$main_executable"
