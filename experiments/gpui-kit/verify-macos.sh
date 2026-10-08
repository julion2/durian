#!/usr/bin/env bash
# Run only in an agreed Mac UI-test window. Supply a privately built demo binary.
set -euo pipefail
mode=accept
if [[ "${1:-}" == --diagnose ]]; then
    mode=diagnose
    shift
fi
if [[ "$(uname -s)" != Darwin || $# != 1 ]]; then
    echo "Usage on macOS: $0 [--diagnose] /tmp/gpui-.../target/debug/durian-gpui-kit" >&2
    exit 2
fi
binary="$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve(strict=True))' "$1")"
case "$binary" in
    /tmp/gpui-*/*|/private/tmp/gpui-*/*) ;;
    *) echo "The test binary must be in an isolated /tmp/gpui-* directory." >&2; exit 2 ;;
esac
[[ -x "$binary" ]] || { echo "Test binary is not executable." >&2; exit 2; }
source_dir="$(dirname -- "${BASH_SOURCE[0]}")/verification"
umask 077
work="$(mktemp -d /tmp/gpui-keyboard.XXXXXX)"
cleanup() {
    status=$?
    # Cache is disposable. On failure preserve logs and any unrestored private
    # clipboard backup; never delete that backup just to make cleanup green.
    rm -rf -- "$work/module-cache"
    if [[ "$status" == 0 && "$mode" == accept ]]; then
        rm -rf -- "$work"
    else
        echo "Logs/diagnosis and any private recovery files retained in $work" >&2
    fi
}
trap cleanup EXIT
cp -- "$source_dir/keyboard.swift" "$source_dir/verify-keyboard.py" "$source_dir/diagnose-keyboard.py" "$work/"
swiftc -module-cache-path "$work/module-cache" "$work/keyboard.swift" -o "$work/keyboard"
if [[ "$mode" == diagnose ]]; then
    python3 "$work/diagnose-keyboard.py" "$binary" | tee "$work/collection.log"
else
    python3 "$work/verify-keyboard.py" "$binary"
fi
