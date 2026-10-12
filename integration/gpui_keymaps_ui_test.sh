#!/usr/bin/env bash
# Real Pkl -> authenticated Go API -> native GPUI keyboard input/rendered output.
set -euo pipefail
if [[ "${DURIAN_UI_DISPLAY:-}" != 1 ]]; then
  exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' env DURIAN_UI_DISPLAY=1 "$0" "$@"
fi
APP=$(realpath "$1"); SEEDER=$(realpath "$2"); CONFIG=$(realpath "$3")
CLI="$(dirname "$APP")/durian-cli"
ROOT=$(mktemp -d /tmp/durian-keys-XXXXXX)
OUT="${DURIAN_UI_ARTIFACT_DIR:-${TEST_UNDECLARED_OUTPUTS_DIR:-$ROOT/evidence}}"
mkdir -p "$OUT"
APP_PID=""; WM_PID=""; BACKEND_PID=""
cleanup() {
  cp "$ROOT/app.log" "$OUT/app.log" 2>/dev/null || true
  for pid in "$APP_PID" "$BACKEND_PID" "$WM_PID"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  rm -rf "$ROOT"
}
trap cleanup EXIT
export HOME="$ROOT/home" XDG_DATA_HOME="$ROOT/data" XDG_STATE_HOME="$ROOT/state"
export XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_RUNTIME_DIR="$ROOT/runtime"
export TMPDIR="$ROOT/tmp" MAIL_APPEARANCE="${MAIL_APPEARANCE:-dark}" MAIL_AVATARS=0 MAIL_TRACE=1 VK_ICD_FILENAMES=/dev/null
export MAILRENDER_BIN="$ROOT/no-renderer"
unset MAIL_BACKEND MAIL_TOKEN DURIAN_BIN MAIL_FOLDER MAIL_BACKGROUND
mkdir -p "$HOME" "$XDG_CONFIG_HOME/durian" "$XDG_DATA_HOME/durian" "$TMPDIR" "$XDG_RUNTIME_DIR"
chmod 700 "$TMPDIR" "$XDG_RUNTIME_DIR"
sed -e '/    smtp {/d' -e '/    imap {/d' -e '/    auth {/d' "$CONFIG" > "$XDG_CONFIG_HOME/durian/config.pkl"
cp "$(dirname "$CONFIG")/profiles.pkl" "$XDG_CONFIG_HOME/durian/profiles.pkl"
# Public fixture key. Provider transports are absent; this cannot send mail.
export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
"$SEEDER" -list-rows 20 "$XDG_DATA_HOME/durian/email.db" "$XDG_DATA_HOME/durian/contacts.db"
KEYMAPS="$XDG_CONFIG_HOME/durian/keymaps.pkl"
cat > "$KEYMAPS" <<'PKL'
global_settings { sequence_timeout = 0.35 }
keymaps {
  new { action = "next_email"; key = "j"; enabled = false }
  new { action = "next_email"; key = "x"; supports_count = true }
  new { action = "archive"; key = "a"; enabled = false }
  new { action = "tag_op"; key = "zz"; tags = "+keymap-proof" }
  new { action = "confirm_selection"; key = "y"; modifiers { "ctrl" }; context = "tag_picker" }
  new { action = "calendar_today"; key = "b"; context = "calendar" }
}
PKL
openbox --sm-disable > "$ROOT/wm.log" 2>&1 & WM_PID=$!
"$APP" > "$ROOT/app.log" 2>&1 & APP_PID=$!
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"
BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)

screen() {
  import -window "$WINDOW" "$OUT/$1.png"
  # Inspect the detail pane, not duplicate subject/snippet glyphs in the list.
  local crop=770x760+650+45
  [[ "$1" == thread-context ]] && crop=1168x760+244+45
  [[ "$1" == message-* ]] && crop=1168x760+244+45
  [[ "$1" == selection-* ]] && crop=1440x850+0+0
  [[ "$1" == picker-* ]] && crop=700x600+350+70
  convert "$OUT/$1.png" -crop "$crop" -resize 200% -colorspace Gray -normalize "$ROOT/ocr.png"
  tesseract "$ROOT/ocr.png" stdout --psm 11 2>/dev/null > "$ROOT/screen.txt"
}
expect() {
  local name="$1" text="$2" absent="${3:-}" detail="${4:-}"
  for _ in {1..35}; do
    screen "$name"
    if [[ -n "$detail" ]]; then
      convert "$OUT/$name.png" -crop 770x760+650+45 -resize 200% -colorspace Gray -normalize "$ROOT/detail.png"
      tesseract "$ROOT/detail.png" stdout --psm 11 2>/dev/null > "$ROOT/detail.txt"
    fi
    if grep -Eqi -- "$text" "$ROOT/screen.txt" &&
       { [[ -z "$absent" ]] || ! grep -Eqi -- "$absent" "$ROOT/screen.txt"; } &&
       { [[ -z "$detail" ]] || grep -Eqi -- "$detail" "$ROOT/detail.txt"; }; then echo "PASS native $name"; return; fi
    kill -0 "$APP_PID"; sleep 0.2
  done
  echo "FAIL native $name: expected $text" >&2; cat "$ROOT/screen.txt" >&2; return 1
}
keys() { xdotool key --clearmodifiers --delay 55 "$@"; }
filter_help() {
  local name="$1" query="$2"
  keys ctrl+a
  xdotool type --clearmodifiers --delay 30 "$query"
  # A result label can match before all keystrokes have been rendered. Observe
  # the input field independently so captures never show a half-typed filter.
  for _ in {1..35}; do
    screen "$name"
    convert "$OUT/$name.png" -crop 348x30+1040+99 -resize 300% -colorspace Gray -normalize "$ROOT/filter.png"
    if tesseract "$ROOT/filter.png" stdout --psm 7 2>/dev/null | grep -Fqi "$query"; then return; fi
    kill -0 "$APP_PID"; sleep 0.2
  done
  echo "FAIL native filter: expected $query" >&2; return 1
}
reload() {
  local before
  before=$(grep -c 'keymaps loaded;' "$ROOT/app.log" || true)
  keys ctrl+alt+r
  for _ in {1..80}; do
    [[ $(grep -c 'keymaps loaded;' "$ROOT/app.log" || true) -gt $before ]] && return
    sleep 0.1
  done
  echo 'FAIL keymaps reload did not complete' >&2; return 1
}
count() {
  for _ in {1..40}; do
    [[ "$("$CLI" count "$1")" == "$2" ]] && { echo "PASS durable $1 = $2"; return; }
    sleep 0.1
  done
  echo "FAIL durable $1 expected $2" >&2; return 1
}

expect initial 'Reply body'
for _ in {1..80}; do grep -q 'keymaps loaded;' "$ROOT/app.log" && break; sleep 0.1; done
grep -q 'keymaps loaded;' "$ROOT/app.log"
keys j a
expect disabled-defaults 'Reply body'
count 'tag:inbox' 22
keys 3 x
expect counted-custom 'Snippetnumber 002'
keys g g
expect sequence-first 'Reply body'
keys z; sleep 0.5; keys z; sleep 0.5
count 'tag:keymap-proof' 0
keys z z
count 'tag:keymap-proof' 1
keys x
expect custom-next 'Different thread'
keys l j
expect thread-context 'Different thread'
keys h
keys question
expect custom-help 'Keyboard shortcuts'
filter_help custom-help-filtered 'next thread'
expect custom-help-filtered 'Next thread' 'next folder'
filter_help folder-shortcuts 'go folder'
expect folder-shortcuts 'go folder'
keys Escape

# A contextual shortcut acts inside the tag picker while plain text stays text.
keys t
expect picker-open 'Add tags'
xdotool type --clearmodifiers --delay 30 'keymap-popup'
expect picker-typed 'keymap-popup'
keys ctrl+y Escape
count 'tag:keymap-popup' 1
keys slash
xdotool type --clearmodifiers --delay 30 'zz'
keys ctrl+a BackSpace Escape
count 'tag:keymap-proof' 1

# Atomically install an edited file, retain it after a broken edit, then delete it.
cat > "$KEYMAPS" <<'PKL'
keymaps { new { action = "prev_email"; key = "x" } }
PKL
reload
keys x
expect reloaded-override 'Reply body'
printf 'keymaps { new { action =' > "$KEYMAPS"
keys ctrl+alt+r
expect invalid-file 'Keymaps not loaded'
keys j x
expect retained-last-good 'Reply body'
rm "$KEYMAPS"
reload
keys j
expect deleted-file-defaults 'Different thread'
keys question
expect default-help 'Keyboard shortcuts'
filter_help unavailable-actions 'unavailable'
expect unavailable-actions 'Unavailable'
keys Escape

cat > "$KEYMAPS" <<'PKL'
global_settings { keymaps_enabled = false }
PKL
reload
keys k g g a
expect globally-disabled 'Different thread'
count 'tag:inbox' 22
rm "$KEYMAPS"
reload

# Message navigation uses local message identities, including two messages with
# the same RFC Message-ID. Counts clamp instead of wrapping. OCR can conflate
# the toolbar's lowercase o with zero; the position and total remain exact.
# A short viewport exercises scrolling as the message cards expand.
xdotool windowsize --sync "$WINDOW" 1420 500
keys g g l
expect message-first 'Message[[:space:]]*[1lI][[:space:]]*[o0]f[[:space:]]*3'
[[ "$(identify -format '%h' "$OUT/message-first.png")" == 500 ]]
grep -qi 'First message body' "$ROOT/screen.txt"
keys n
expect message-second 'Message[[:space:]]*2[[:space:]]*[o0]f[[:space:]]*3'
grep -qi 'Duplicate provider body' "$ROOT/screen.txt"
keys 9 n
expect message-last 'Message[[:space:]]*3[[:space:]]*[o0]f[[:space:]]*3'
grep -qi 'Reply body' "$ROOT/screen.txt"
keys 9 shift+n
expect message-clamp-first 'Message[[:space:]]*[1lI][[:space:]]*[o0]f[[:space:]]*3'
keys shift+g
expect message-edge-last 'Message[[:space:]]*3[[:space:]]*[o0]f[[:space:]]*3'
keys g g
expect message-edge-first 'Message[[:space:]]*[1lI][[:space:]]*[o0]f[[:space:]]*3'
grep -qi 'First message body' "$ROOT/screen.txt"
xdotool windowsize --sync "$WINDOW" 1420 816
keys h

# Selection actions remain remappable; the model does not interpret Vim keys.
cat > "$KEYMAPS" <<'PKL'
keymaps {
  new { action = "enter_visual_mode"; key = "e" }
  new { action = "toggle_list_layout"; key = "z"; modifiers { "ctrl" } }
}
PKL
reload
# Range marks, not the cursor alone, are the stable targets of durable actions.
keys g g e 2 j
expect selection-range 'Visual.*3'
keys ctrl+z
expect selection-wide 'Visual.*3'
keys s
count 'tag:flagged' 3
keys s
count 'tag:flagged' 0
keys ctrl+z
expect selection-split-restored 'Visual.*3'
keys t
expect picker-range '3 selected threads'
xdotool type --clearmodifiers --delay 30 'keymap-proof'
keys Return
count 'tag:keymap-proof' 3
keys Return
count 'tag:keymap-proof' 0
keys ctrl+a
xdotool type --clearmodifiers --delay 30 'range-proof'
keys Return Escape
count 'tag:range-proof' 3
keys u
count 'tag:unread' 3
keys ctrl+r
expect selection-refreshed 'Visual.*3'
count 'tag:unread' 3
keys u
count 'tag:unread' 0
keys k
expect selection-shrink 'Visual.*2'
keys Escape g g 2 j v 2 k
expect selection-reversed 'Visual.*3'
keys Escape

# Toggle marks are noncontiguous; Space advances without selecting its new cursor.
keys g g shift+v 2 j space space 2 k space j
# The count already equals two before the final j; observe its new cursor too.
expect selection-toggle 'Toggle.*2' '' 'Snippetnumber 003'
keys s
count 'tag:flagged' 2
count 'tag:flagged AND tag:range-proof' 1
keys Escape s
count 'tag:flagged' 3

keys g g a
count 'subject:Hello AND tag:archive' 1
# This profile has only Inbox. Archive fallback must replace its base query,
# not intersect tag:archive with tag:inbox, and remain scoped after refresh.
keys g a
expect archive-destination 'Reply body'
keys ctrl+r
expect archive-refresh 'Reply body'
keys g i
expect inbox-destination 'Different thread' 'Reply body'
keys g f
expect picker-folders 'Go to folder'
keys Return
expect picked-inbox 'Different thread'
keys 3 d d
count 'tag:trash' 3
count 'tag:inbox' 18
# A committed write is not yet a rendered search response. Start the next
# selection only after the removed rows have actually left the list.
expect selection-after-delete 'Preview row 003' 'Other Thread|Preview row 001|Preview row 002'
keys g g shift+v 2 j space a
count 'tag:archive' 3
count 'tag:inbox' 16
expect selection-after-archive 'Preview row 004' 'Preview row 003|Preview row 005'
keys g g v j d d
count 'tag:trash' 5
count 'tag:inbox' 14
keys g c question
expect calendar-help 'Keyboard shortcuts'
filter_help calendar-help-filtered 'Next event'
expect calendar-help-filtered 'Next event' 'Next thread'
keys Escape
keys ctrl+q
wait "$APP_PID"; APP_PID=""
for _ in {1..30}; do kill -0 "$BACKEND_PID" 2>/dev/null || break; sleep 0.1; done
! kill -0 "$BACKEND_PID" 2>/dev/null
BACKEND_PID=""
echo 'PASS GPUI user keymaps: overrides/counts/sequences/contexts/reload/errors/disable/defaults/multi-selection/message navigation/tag intents/cleanup'
