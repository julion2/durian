#!/usr/bin/env bash
# Packaged native app + owned Go backend + actual WebKitGTK, provider-free fixtures.
set -euo pipefail
if [[ "${DURIAN_UI_DISPLAY:-}" != 1 ]]; then
  exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' env DURIAN_UI_DISPLAY=1 "$0" "$@"
fi
APP=$(realpath "$1")
SEEDER=$(realpath "$2")
CONFIG=$(realpath "$3")
FIXTURE=$(realpath "$(dirname "$0")/testdata/render-mail.html")
# WebKit's accessibility proxy uses Unix sockets: Bazel TEST_TMPDIR exceeds
# sockaddr_un's path limit. Keep the private runtime path short, with trap cleanup.
ROOT=$(mktemp -d /tmp/durian-html-ui-XXXXXX)
OUT="${DURIAN_UI_ARTIFACT_DIR:-${TEST_UNDECLARED_OUTPUTS_DIR:-$ROOT/evidence}}"
mkdir -p "$OUT"
APP_PID=""; WM_PID=""; GROUP=""
cleanup() {
  for pid in "$APP_PID" "$WM_PID"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  [[ -z "$GROUP" ]] || kill -- "-$GROUP" 2>/dev/null || true
  rm -rf "$ROOT"
}
trap cleanup EXIT
export HOME="$ROOT/home" XDG_DATA_HOME="$ROOT/data" XDG_STATE_HOME="$ROOT/state"
export XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_RUNTIME_DIR="$ROOT/runtime"
export TMPDIR="$ROOT/tmp" MAIL_AVATARS=0 MAIL_TRACE=1 VK_ICD_FILENAMES=/dev/null
unset MAIL_BACKEND MAIL_TOKEN DURIAN_BIN MAIL_FOLDER MAIL_BACKGROUND MAILRENDER_BIN
mkdir -p "$HOME" "$XDG_CONFIG_HOME/durian" "$XDG_DATA_HOME/durian" "$TMPDIR" "$XDG_RUNTIME_DIR" "$ROOT/bin"
chmod 700 "$TMPDIR" "$XDG_RUNTIME_DIR"
sed -e '/    smtp {/d' -e '/    imap {/d' -e '/    auth {/d' "$CONFIG" > "$XDG_CONFIG_HOME/durian/config.pkl"
cp "$(dirname "$CONFIG")/profiles.pkl" "$XDG_CONFIG_HOME/durian/profiles.pkl"
export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
"$SEEDER" "$XDG_DATA_HOME/durian/email.db" "$XDG_DATA_HOME/durian/contacts.db" "$FIXTURE"
# Observe the desktop-open boundary without launching a browser or contacting a provider.
printf '#!/bin/sh\nprintf "%%s\\n" "$1" >> "%s/opened"\n' "$ROOT" > "$ROOT/bin/xdg-open"
chmod +x "$ROOT/bin/xdg-open"
export PATH="$ROOT/bin:$PATH"
openbox --sm-disable > "$ROOT/wm.log" 2>&1 &
WM_PID=$!

screen() {
  import -window "$WINDOW" "$OUT/$1.png"
  # Isolate the message pane: sparse OCR across mixed dark shell/white HTML
  # otherwise drops perfectly visible cells and links.
  convert "$OUT/$1.png" -crop 760x816+650+0 -resize 200% "$ROOT/ocr.png"
  tesseract "$ROOT/ocr.png" stdout --psm 11 2>/dev/null > "$ROOT/screen.txt"
}
expect() {
  for _ in {1..40}; do
    screen "$1"
    if grep -Eqi "$2" "$ROOT/screen.txt" && { [[ -z "${3:-}" ]] || ! grep -Eqi "$3" "$ROOT/screen.txt"; }; then
      echo "PASS native $1: $2"; return
    fi
    kill -0 "$APP_PID"; sleep .2
  done
  cat "$ROOT/screen.txt"; return 1
}
point() {
  tesseract "$ROOT/ocr.png" stdout --psm 11 tsv 2>/dev/null |
    awk -F '\t' -v word="$1" '$12 == word { print 650+int(($7+$9/2)/2), int(($8+$10/2)/2); exit }'
}
click() {
  local p; p=$(point "$1"); [[ -n "$p" ]]
  read -r x y <<< "$p"; xdotool mousemove --window "$WINDOW" "$x" "$y" click 1
}
start() {
  MAIL_APPEARANCE="$1" "$APP" > "$OUT/$2.log" 2>&1 &
  APP_PID=$!
  WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
  xdotool windowactivate --sync "$WINDOW"
}
quit() {
  local backend; backend=$(pgrep -P "$APP_PID" -x durian-cli)
  xdotool key --clearmodifiers ctrl+q
  for _ in {1..50}; do kill -0 "$APP_PID" 2>/dev/null || break; sleep .1; done
  if kill -0 "$APP_PID" 2>/dev/null; then echo 'FAIL app survived quit'; return 1; fi
  wait "$APP_PID"; APP_PID=""
  if kill -0 "$backend" 2>/dev/null; then echo 'FAIL owned CLI survived quit'; return 1; fi
  if [[ -n "$GROUP" ]]; then
    if ps -eo pgid=,stat= | awk -v group="$GROUP" '$1 == group && $2 !~ /^Z/ { found=1 } END { exit !found }'; then
      echo 'FAIL renderer process group survived quit'; return 1
    fi
  fi
  GROUP=""
}

# A missing helper is an observable plaintext control, not a successful HTML render.
export MAILRENDER_BIN="$ROOT/missing"
start dark unavailable
expect fallback 'Reply body' 'Preparing original|October invoice'
quit
unset MAILRENDER_BIN
for theme in dark light; do
  if [[ "$theme" == dark ]]; then
    printf '#!/bin/sh\nsleep 4\nexec "%s/mailrender"\n' "$(dirname "$APP")" > "$ROOT/slow-renderer"
    chmod +x "$ROOT/slow-renderer"
    export MAILRENDER_BIN="$ROOT/slow-renderer"
  fi
  start "$theme" "$theme"
  if [[ "$theme" == dark ]]; then expect loading 'Preparing original layout'; fi
  expect "$theme-html" '42.00 EUR' 'UNSAFE SCRIPT'
  unset MAILRENDER_BIN
  GROUP=$(pgrep -P "$APP_PID" -x mailrender)
  [[ -n "$GROUP" ]]
  # Actual link hitbox -> GPUI desktop opener, never a URL inferred from JSON.
  : > "$ROOT/opened"
  click Open
  for _ in {1..20}; do [[ -s "$ROOT/opened" ]] && break; sleep .1; done
  grep -qx 'https://example.invalid/invoice' "$ROOT/opened"
  echo "PASS native $theme link activation"
  click Show
  expect "$theme-text" 'Reply body' '42.00 EUR'
  # Select the plaintext paragraph, copy, and paste into the real search field.
  read -r x y <<< "$(point Reply)"
  xdotool mousemove --window "$WINDOW" "$x" "$y" click --repeat 3 --delay 100 1
  screen "$theme-selected"
  xdotool key --clearmodifiers ctrl+c
  xdotool mousemove --window "$WINDOW" 370 108 click 1
  xdotool key --clearmodifiers ctrl+v
  sleep .3
  screen "$theme-copy"
  convert "$OUT/$theme-copy.png" -crop 385x35+250+90 -resize 300% -colorspace Gray -negate "$ROOT/search.png"
  tesseract "$ROOT/search.png" stdout --psm 7 2>/dev/null | grep -qi 'Reply body'
  xdotool key --clearmodifiers Escape Escape
  expect "$theme-text" 'Show original layout'
  click Show
  expect "$theme-restored" '42.00 EUR'
  quit
  echo "PASS native $theme text selection/clipboard/layout restore and owned helper cleanup"
done
echo 'PASS native Linux HTML contract'
