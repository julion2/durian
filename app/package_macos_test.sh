#!/usr/bin/env bash
# Portable bundle-layout contract, not a substitute for codesign/Finder acceptance.
set -euo pipefail
PACKAGER=$1 PLIST=$2 ICON=$3 LICENSE=$4
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
mkdir "$ROOT/input"
for name in gui cli renderer; do
  printf '#!/bin/sh\nprintf "%%s\\n" "%s"\n' "$name" > "$ROOT/input/$name"
  chmod +x "$ROOT/input/$name"
done
ln -s "$ROOT/input/gui" "$ROOT/gui-link"
printf 'DURIAN_VERSION=0.4.0\nDURIAN_BUILD_NUMBER=123\n' > "$ROOT/version"
OUT="$ROOT/Bundle path with spaces/Durian Preview.app"
# Bazel precreates tree-artifact roots before invoking the packaging action.
mkdir -p "$OUT"
args=("$ROOT/gui-link" "$ROOT/input/cli" "$ROOT/input/renderer" "$ICON" "$LICENSE" "$ROOT/version" "$PLIST" "$OUT" "26.1")
bash "$PACKAGER" "${args[@]}"
[[ -z $(find "$OUT" -type l -print) ]]
[[ $("$OUT/Contents/MacOS/durian") == gui ]]
[[ $("$OUT/Contents/MacOS/durian-cli") == cli ]]
[[ $("$OUT/Contents/MacOS/mailrender") == renderer ]]
cmp "$ICON" "$OUT/Contents/Resources/Durian.icns"
cmp "$LICENSE" "$OUT/Contents/Resources/LICENSE"
python3 - "$OUT" <<'PY'
import pathlib, plistlib, sys
root = pathlib.Path(sys.argv[1]) / 'Contents'
p = plistlib.loads((root / 'Info.plist').read_bytes())
assert p['CFBundleIdentifier'] == 'org.js-lab.durian.preview'
assert p['CFBundleName'] == p['CFBundleDisplayName'] == 'Durian Preview'
assert p['CFBundlePackageType'] == 'APPL'
assert p['CFBundleShortVersionString'] == '0.4.0'
assert p['CFBundleVersion'] == '123'
assert p['LSMinimumSystemVersion'] == '26.1'
assert (root / 'MacOS' / p['CFBundleExecutable']).is_file()
assert (root / 'Resources' / p['CFBundleIconFile']).read_bytes()[:4] == b'icns'
assert (root / 'PkgInfo').read_bytes() == b'APPL????'
PY
if bash "$PACKAGER" "${args[@]}" 2>/dev/null; then echo 'Overwrote existing app' >&2; exit 1; fi
[[ $("$OUT/Contents/MacOS/durian") == gui ]]
args[7]="$ROOT/invalid-minimum.app"
args[8]='26&invalid'
if bash "$PACKAGER" "${args[@]}" 2>/dev/null; then echo 'Accepted invalid minimum OS' >&2; exit 1; fi
[[ ! -e "$ROOT/invalid-minimum.app" ]]
args[8]='26.1'
printf 'DURIAN_VERSION=invalid\nDURIAN_BUILD_NUMBER=123\n' > "$ROOT/version"
args[7]="$ROOT/invalid.app"
if bash "$PACKAGER" "${args[@]}" 2>/dev/null; then echo 'Accepted invalid metadata' >&2; exit 1; fi
[[ ! -e "$ROOT/invalid.app" ]]
echo 'PASS bundle: identity, plist, minimum OS, icon, copied executables, spaces, no symlinks/overwrite, invalid metadata'
