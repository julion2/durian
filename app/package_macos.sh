#!/usr/bin/env bash
# Assemble a fresh output; the Bazel rule signs it on macOS after assembly.
set -euo pipefail
[[ $# == 9 ]] || { echo 'Expected GUI, CLI, renderer, icon, license, version, plist, output, minimum OS' >&2; exit 2; }
GUI=$1 CLI=$2 RENDERER=$3 ICON=$4 LICENSE=$5 METADATA=$6 PLIST=$7 OUT=$8 MINIMUM_OS=$9
VERSION=$(sed -n 's/^DURIAN_VERSION=//p' "$METADATA")
BUILD=$(sed -n 's/^DURIAN_BUILD_NUMBER=//p' "$METADATA")
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "$BUILD" =~ ^[0-9]+$ ]] || {
  echo 'Invalid bundle version metadata' >&2; exit 1;
}
[[ "$MINIMUM_OS" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo 'Invalid macOS deployment target' >&2; exit 1; }
for binary in "$GUI" "$CLI" "$RENDERER"; do
  [[ -f "$binary" && -x "$binary" ]] || { echo 'Missing bundle executable' >&2; exit 1; }
done
# Bazel precreates an empty tree-artifact directory. Never overwrite a populated
# application or follow a symlink, including a dangling one.
[[ ! -L "$OUT" && ( ! -e "$OUT" || ( -d "$OUT" && -z $(ls -A "$OUT") ) ) ]] || {
  echo 'Bundle output is not an empty directory' >&2; exit 1;
}
mkdir -p "$OUT/Contents/MacOS" "$OUT/Contents/Resources"
cp -L "$GUI" "$OUT/Contents/MacOS/durian"
cp -L "$CLI" "$OUT/Contents/MacOS/durian-cli"
cp -L "$RENDERER" "$OUT/Contents/MacOS/mailrender"
chmod 755 "$OUT/Contents/MacOS/"*
cp -L "$ICON" "$OUT/Contents/Resources/Durian.icns"
cp -L "$LICENSE" "$OUT/Contents/Resources/LICENSE"
sed -e "s/@VERSION@/$VERSION/g" -e "s/@BUILD@/$BUILD/g" -e "s/@MINIMUM_OS@/$MINIMUM_OS/g" "$PLIST" > "$OUT/Contents/Info.plist"
printf 'APPL????' > "$OUT/Contents/PkgInfo"
