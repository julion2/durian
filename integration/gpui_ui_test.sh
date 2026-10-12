#!/usr/bin/env bash
# Native Linux GPUI -> packaged Go backend -> encrypted throwaway SQLite.
# Uses real X11 keyboard input and OCR of rendered pixels, not a replacement web UI.
set -euo pipefail

if [[ "${DURIAN_UI_DISPLAY:-}" != 1 ]]; then
  exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' env DURIAN_UI_DISPLAY=1 "$0" "$@"
fi

APP=$(realpath "$1")
SEEDER=$(realpath "$2")
CONFIG=$(realpath "$3")
CLI="$(dirname "$APP")/durian-cli"
[[ -x "$CLI" ]]
# Keep WebKit's runtime Unix socket paths below sockaddr_un's limit.
ROOT=$(mktemp -d /tmp/durian-gpui-ui-XXXXXX)
OUT="${DURIAN_UI_ARTIFACT_DIR:-${TEST_UNDECLARED_OUTPUTS_DIR:-$ROOT/evidence}}"
mkdir -p "$OUT"
APP_PID=""
BACKEND_PID=""
WM_PID=""
HELPER_PID=""
cleanup() {
  cp "$ROOT/app.log" "$OUT/app.log" 2>/dev/null || true
  for pid in "$APP_PID" "$BACKEND_PID" "$HELPER_PID" "$WM_PID"; do
    if [[ -n "$pid" ]]; then
      kill -CONT "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$ROOT"
}
trap cleanup EXIT

export HOME="$ROOT/home" XDG_DATA_HOME="$ROOT/data" XDG_STATE_HOME="$ROOT/state"
export XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_RUNTIME_DIR="$ROOT/runtime"
export TMPDIR="$ROOT/tmp" MAIL_APPEARANCE=dark MAIL_AVATARS=0 MAIL_TRACE=1
# Mesa's Orb software Vulkan renderer paints black; the same app renders with OpenGL.
export VK_ICD_FILENAMES=/dev/null
unset MAIL_BACKEND MAIL_TOKEN DURIAN_BIN MAIL_FOLDER MAIL_BACKGROUND
mkdir -p "$HOME" "$XDG_CONFIG_HOME/durian" "$XDG_DATA_HOME/durian" "$TMPDIR" "$XDG_RUNTIME_DIR"
chmod 700 "$TMPDIR" "$XDG_RUNTIME_DIR"
# Keep the fixture's configured sending identity, but remove all provider
# transports. An accidental expired Undo window cannot deliver a real message.
sed -e '/    smtp {/d' -e '/    imap {/d' -e '/    auth {/d' "$CONFIG" > "$XDG_CONFIG_HOME/durian/config.pkl"
cp "$(dirname "$CONFIG")/profiles.pkl" "$XDG_CONFIG_HOME/durian/profiles.pkl"
cat >> "$XDG_CONFIG_HOME/durian/config.pkl" <<EOF

calendar: C.CalendarConfig = new {
  vdir_path = "$XDG_DATA_HOME/durian/calendars"
  local_calendars = new Listing {
    new C.LocalCalendar { name = "Scratch"; path = "$ROOT/local-calendar" }
  }
}
EOF
# Public test key, not a user's mailbox key.
export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
"$SEEDER" "$XDG_DATA_HOME/durian/email.db" "$XDG_DATA_HOME/durian/contacts.db"
# This contact is deliberately absent from every loaded message.
"$CLI" contacts add server@example.invalid 'Server Only'

openbox --sm-disable > "$ROOT/wm.log" 2>&1 &
WM_PID=$!
"$APP" > "$ROOT/app.log" 2>&1 &
APP_PID=$!
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"

screen() {
  import -window "$WINDOW" "$OUT/$1.png"
  OCR_SCALE=2
  case "$1" in
    reconnecting)
      # The short-lived status pill is behind the dimmed composer. Isolate it
      # before OCR so white HTML and the modal do not hide its lower contrast.
      OCR_SCALE=3
      convert "$OUT/$1.png" -crop 390x50+250+750 -resize 300% -colorspace Gray -normalize -negate "$ROOT/ocr.png"
      ;;
    draft-*)
      # Committed address chips need a higher threshold than the dimmer controls.
      OCR_SCALE=3
      convert "$OUT/$1.png" -resize 300% -colorspace Gray -threshold 50% -negate "$ROOT/ocr.png"
      ;;
    compose-*|drafts-*|control)
      # The dimmed backdrop defeats sparse-page OCR; isolate brighter dialog text.
      OCR_SCALE=3
      convert "$OUT/$1.png" -resize 300% -colorspace Gray -threshold 40% -negate "$ROOT/ocr.png"
      ;;
    *) convert "$OUT/$1.png" -resize 200% -colorspace Gray -negate "$ROOT/ocr.png" ;;
  esac
  tesseract "$ROOT/ocr.png" stdout --psm 11 2>/dev/null > "$ROOT/screen.txt"
  case "$1" in
    compose-*|draft-*|drafts-*|control|reconnecting) ;;
    *)
      # Mixed white HTML/dark shell defeats whole-page sparse OCR. Read the
      # actual sidebar/list separately too, without changing any assertions.
      convert "$OUT/$1.png" -crop 645x816+0+0 -resize 200% -colorspace Gray -negate "$ROOT/list-ocr.png"
      tesseract "$ROOT/list-ocr.png" stdout --psm 11 2>/dev/null >> "$ROOT/screen.txt"
      ;;
  esac
}

expect_screen() {
  local name="$1" expected="$2" absent="${3:-}" attempts="${4:-40}"
  for ((i = 0; i < attempts; i++)); do
    screen "$name"
    if grep -Eqi -- "$expected" "$ROOT/screen.txt" && { [[ -z "$absent" ]] || ! grep -Eqi -- "$absent" "$ROOT/screen.txt"; }; then
      echo "PASS native rendered $name"
      return
    fi
    kill -0 "$APP_PID"
    sleep 0.25
  done
  echo "FAIL native rendered $name: expected '$expected', absent '$absent'" >&2
  cat "$ROOT/screen.txt" >&2
  return 1
}

search() {
  xdotool key --clearmodifiers slash
  xdotool type --clearmodifiers --delay 25 "$1 "
  xdotool key Return
}

expect_inbox_count() {
  local name="$1" expected="$2"
  for _ in {1..40}; do
    screen "$name"
    # Full-page OCR drops the low-contrast badge. Normalize its isolated region
    # in the fixed-size native window before reading the digits.
    convert "$OUT/$name.png" -crop 25x25+204+96 -resize 600% -colorspace Gray -normalize -negate "$ROOT/badge.png"
    local digits
    digits=$(tesseract "$ROOT/badge.png" stdout --psm 10 -c tessedit_char_whitelist=0123456789 2>/dev/null)
    # Zero intentionally has no badge; unknown is a dash, never a real zero.
    if [[ "$digits" == "$expected" ]] || { [[ "$expected" == 0 && -z "$digits" ]] && ! grep -Eq '^[-–—]$' "$ROOT/screen.txt"; }; then
      echo "PASS native Inbox badge $expected in $name"
      return
    fi
    kill -0 "$APP_PID"
    sleep 0.25
  done
  echo "FAIL native Inbox badge: expected $expected" >&2
  return 1
}

clear_search() {
  xdotool key --clearmodifiers slash Escape Escape Escape
}

# Resolve controls from rendered text rather than assuming dialog coordinates.
click_word() {
  if [[ "$1" == Undo ]]; then
    # The real Undo deadline keeps running during OCR. Read its small toast
    # once instead of spending the remaining window scanning the whole UI twice.
    import -window "$WINDOW" "$OUT/control.png"
    convert "$OUT/control.png" -crop 450x100+970+700 -resize 200% -colorspace Gray -negate "$ROOT/undo-ocr.png"
    local undo
    undo=$(tesseract "$ROOT/undo-ocr.png" stdout --psm 11 tsv 2>/dev/null |
      awk -F '\t' '$12 == "Undo" { print 970+int(($7+$9/2)/2), 700+int(($8+$10/2)/2); exit }')
    if [[ -z "$undo" ]]; then echo 'FAIL native Undo control not found'; return 1; fi
    read -r x y <<< "$undo"
    xdotool mousemove --window "$WINDOW" "$x" "$y" click 1
    return
  fi
  screen control
  local position
  position=$(tesseract "$ROOT/ocr.png" stdout --psm 11 tsv 2>/dev/null |
    awk -F '\t' -v word="$1" -v scale="$OCR_SCALE" '$12 == word { print int(($7+$9/2)/scale), int(($8+$10/2)/scale); exit }')
  if [[ -z "$position" ]]; then
    # Thresholding separates dim controls from the modal backdrop, but can merge
    # white labels with colored primary buttons. Use their higher contrast boundary.
    convert "$OUT/control.png" -resize 300% -colorspace Gray -threshold 60% -negate "$ROOT/ocr.png"
    position=$(tesseract "$ROOT/ocr.png" stdout --psm 11 tsv 2>/dev/null |
      awk -F '\t' -v word="$1" '$12 == word { print int(($7+$9/2)/3), int(($8+$10/2)/3); exit }')
  fi
  if [[ -z "$position" ]]; then
    echo "FAIL native control not found: $1" >&2
    return 1
  fi
  read -r x y <<< "$position"
  xdotool mousemove --window "$WINDOW" "$x" "$y" click 1
}

assert_gone() {
  if kill -0 "$1" 2>/dev/null; then
    echo "FAIL child process $1 survived" >&2
    return 1
  fi
}

expect_screen live 'Reply body'
BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)
[[ "$("$CLI" count 'tag:inbox')" == 2 ]]
[[ "$("$CLI" count 'tag:unread')" == 0 ]]
expect_inbox_count live 0
echo 'PASS packaged sibling backend and opening a thread marks it read'

xdotool key --clearmodifiers a
expect_screen archived 'Different thread' 'Reply body'
[[ "$("$CLI" count 'tag:inbox')" == 1 ]]
[[ "$("$CLI" count 'subject:Hello AND tag:archive')" == 1 ]]
"$CLI" tag 'subject:Hello' +inbox -archive > "$ROOT/tag.log"
xdotool key --clearmodifiers ctrl+r
# Refresh preserves the selected Other thread; explicitly reopen the first row.
expect_screen unarchived-list 'Hello World'
xdotool key --clearmodifiers k
expect_screen unarchived 'Reply body'
expect_inbox_count unarchived 0
echo 'PASS archive key writes global thread tags and refresh reads the restored fixture'

xdotool key --clearmodifiers u
expect_inbox_count marked-unread 1
[[ "$("$CLI" count 'tag:unread')" == 1 ]]
search 'subject:Other'
expect_screen search 'Different thread' 'Hello World'
expect_inbox_count search 1
clear_search
# Clearing a filter preserves Other; only actually reopening Hello reads it.
expect_screen cleared-search 'Hello World'
expect_inbox_count cleared-search 1
xdotool key --clearmodifiers k
expect_screen reopened 'Reply body'
expect_inbox_count reopened 0
search 'subject:missing-fixture'
expect_screen empty 'No threads match' 'Reply body'
expect_inbox_count empty 0
clear_search
expect_screen restored 'Reply body'

# Go flushes its first heartbeat after 30 seconds. Wait for that initial
# reconciliation before testing manual refresh, so it cannot mask an unbound key.
for _ in {1..400}; do
  if grep -q 'event stream connected' "$ROOT/app.log"; then break; fi
  sleep 0.1
done
grep -q 'event stream connected' "$ROOT/app.log"
sleep 1
echo 'PASS authenticated real Go SSE stream connected'

# Direct CLI writes deliberately bypass SSE; manual authoritative refresh must catch them.
"$CLI" tag 'subject:Other' +unread > "$ROOT/tag.log"
xdotool key --clearmodifiers ctrl+r
expect_inbox_count counts-refreshed 1
[[ "$("$CLI" count 'tag:inbox AND tag:unread')" == 1 ]]
"$CLI" tag 'subject:Other' -inbox > "$ROOT/tag.log"
xdotool key --clearmodifiers ctrl+r
expect_screen refreshed 'Reply body' 'Other Thread' 5
expect_inbox_count refreshed 0
[[ "$("$CLI" count 'tag:inbox')" == 1 ]]

# A later external change receives no keypress and no mail SSE event: the fixed
# 60-second reconciliation must discover it. Heartbeats are not change hints.
"$CLI" tag 'subject:Other' +inbox > "$ROOT/tag.log"
expect_screen reconciled 'Other Thread' '' 140
expect_inbox_count reconciled 1
"$CLI" tag 'subject:Other' -inbox > "$ROOT/tag.log"
xdotool key --clearmodifiers ctrl+r
expect_screen refreshed 'Reply body' 'Other Thread' 5
expect_inbox_count refreshed 0
echo 'PASS periodic reconciliation catches external writes without SSE or a keypress'

# Kill only the owned fixture child. The UI must reconnect with the replacement's new token.
"$CLI" tag 'subject:Other' +inbox > "$ROOT/tag.log"
OLD_BACKEND="$BACKEND_PID"
kill -KILL "$OLD_BACKEND"
BACKEND_PID=""
for _ in {1..200}; do
  BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli || true)
  if [[ -n "$BACKEND_PID" && "$BACKEND_PID" != "$OLD_BACKEND" ]]; then break; fi
  sleep 0.1
done
[[ -n "$BACKEND_PID" && "$BACKEND_PID" != "$OLD_BACKEND" ]]
expect_screen recovered 'Other Thread'
assert_gone "$OLD_BACKEND"
"$CLI" tag 'subject:Other' -inbox > "$ROOT/tag.log"
xdotool key --clearmodifiers ctrl+r
expect_screen recovered-refresh 'Reply body' 'Other Thread'
echo 'PASS owned backend replacement and authenticated workspace recovery'

# A crash loop must stop, not repeatedly spawn servers forever. The budget is
# lifetime-wide, even though each previous replacement successfully became ready.
for crash in 2 3 4; do
  OLD_BACKEND="$BACKEND_PID"
  kill -KILL "$OLD_BACKEND"
  BACKEND_PID=""
  if [[ "$crash" == 4 ]]; then
    expect_screen restart-error 'restart limit reached'
  else
    for _ in {1..200}; do
      BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli || true)
      if [[ -n "$BACKEND_PID" && "$BACKEND_PID" != "$OLD_BACKEND" ]]; then break; fi
      sleep 0.1
    done
    [[ -n "$BACKEND_PID" && "$BACKEND_PID" != "$OLD_BACKEND" ]]
    expect_screen recovered 'Reply body' 'Other Thread'
  fi
  assert_gone "$OLD_BACKEND"
done
if pgrep -P "$APP_PID" -x durian-cli >/dev/null; then
  echo 'FAIL restart budget spawned an extra backend' >&2
  exit 1
fi
echo 'PASS bounded crash loop shows a terminal failure'

xdotool key --clearmodifiers ctrl+q
for _ in {1..100}; do
  if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
  sleep 0.1
done
assert_gone "$APP_PID"
wait "$APP_PID"
APP_PID=""
BACKEND_PID=""
[[ -z "$(find "$TMPDIR" -type d -name 'session-*' -print -quit)" ]]
echo 'PASS quit removes private decrypted session'

# A prefetched HTML helper must not leak when startup never constructs a workspace.
cat > "$ROOT/renderer" <<'HELPER'
#!/bin/sh
echo $$ > "$DURIAN_TEST_HELPER_PID_PATH"
exec sleep 120
HELPER
chmod +x "$ROOT/renderer"
DURIAN_TEST_HELPER_PID_PATH="$ROOT/helper.pid" MAILRENDER_BIN="$ROOT/renderer" DURIAN_BIN="$ROOT/missing-durian" "$APP" >> "$ROOT/app.log" 2>&1 &
APP_PID=$!
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"
expect_screen startup-error 'Could not start durian' 'Hello World'
HELPER_PID=$(cat "$ROOT/helper.pid")
kill -0 "$HELPER_PID"
xdotool key --clearmodifiers ctrl+q
for _ in {1..100}; do
  if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
  sleep 0.1
done
assert_gone "$APP_PID"
wait "$APP_PID"
APP_PID=""
assert_gone "$HELPER_PID"
HELPER_PID=""
echo 'PASS explicit startup failure never substitutes demo mail'
echo 'PASS unused early renderer is reaped after failed startup'

# Exercise the client-owned deadlines with actual helper processes. Neither a
# silent startup nor a ready helper that never replies may occupy slots forever.
cat > "$ROOT/stalled-renderer" <<'HELPER'
#!/bin/sh
echo $$ > "$DURIAN_TEST_HELPER_PID_PATH"
if [ "$DURIAN_TEST_RENDER_MODE" = ready ]; then
  printf '%s\n' '{"ready":true}'
  IFS= read -r request
  touch "$DURIAN_TEST_REQUEST_SEEN"
fi
exec sleep 120
HELPER
chmod +x "$ROOT/stalled-renderer"
for mode in silent ready; do
  deadlines=$(grep -c 'HTML renderer deadline exceeded' "$ROOT/app.log" || true)
  DURIAN_TEST_HELPER_PID_PATH="$ROOT/helper-$mode.pid" DURIAN_TEST_RENDER_MODE="$mode" \
    DURIAN_TEST_REQUEST_SEEN="$ROOT/request-$mode" MAILRENDER_BIN="$ROOT/stalled-renderer" \
    "$APP" >> "$ROOT/app.log" 2>&1 &
  APP_PID=$!
  WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
  xdotool windowactivate --sync "$WINDOW"
  expect_screen "helper-$mode-pending" 'Reply body'
  BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)
  HELPER_PID=$(cat "$ROOT/helper-$mode.pid")
  kill -0 "$HELPER_PID"
  for _ in {1..120}; do
    if ! kill -0 "$HELPER_PID" 2>/dev/null; then break; fi
    kill -0 "$APP_PID"
    sleep 0.25
  done
  assert_gone "$HELPER_PID"
  HELPER_PID=""
  [[ "$(grep -c 'HTML renderer deadline exceeded' "$ROOT/app.log")" == "$((deadlines + 1))" ]]
  if [[ "$mode" == ready ]]; then [[ -e "$ROOT/request-$mode" ]]; fi
  expect_screen "helper-$mode-fallback" 'Reply body'
  search 'subject:missing-fixture'
  expect_screen "helper-$mode-responsive" 'No threads match'
  xdotool key --clearmodifiers ctrl+q
  for _ in {1..100}; do
    if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
    sleep 0.1
  done
  assert_gone "$APP_PID"
  wait "$APP_PID"
  APP_PID=""
  assert_gone "$BACKEND_PID"
  BACKEND_PID=""
  echo "PASS $mode HTML helper deadline reaps process and keeps plaintext/search working"
done

# Slow only a replacement's handshake to inspect the brief reconnecting screen.
# Initial connection and replacement still execute the packaged Go CLI.
cat > "$ROOT/slow-replacement" <<'BACKEND'
#!/bin/sh
if [ -e "$DURIAN_TEST_STARTED" ]; then sleep 3; else touch "$DURIAN_TEST_STARTED"; fi
exec "$DURIAN_TEST_CLI" "$@"
BACKEND
chmod +x "$ROOT/slow-replacement"
DURIAN_TEST_CLI="$CLI" DURIAN_TEST_STARTED="$ROOT/started" DURIAN_BIN="$ROOT/slow-replacement" "$APP" >> "$ROOT/app.log" 2>&1 &
APP_PID=$!
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"
expect_screen quit-ready 'Reply body'
BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)
# Keep an edited composer open across the owned-server replacement.
xdotool key --clearmodifiers c
expect_screen compose-empty 'New message'
kill -STOP "$BACKEND_PID"
xdotool type --clearmodifiers --delay 15 'alice'
expect_screen compose-contacts-loading 'Searching contacts'
expect_screen compose-contacts-error 'Contacts unavailable' '' 80
# The same field recovers on new input. Change query while the older lookup is
# blocked too: its eventual Alice result must never replace the newest query.
xdotool key --clearmodifiers ctrl+a
xdotool type --clearmodifiers --delay 15 'alice@'
expect_screen compose-contacts-retry 'Searching contacts'
xdotool key --clearmodifiers ctrl+a
xdotool type --clearmodifiers --delay 15 'server@'
kill -CONT "$BACKEND_PID"
expect_screen compose-contacts-found 'Server Only' "Alice Smith|durian isn't reachable"
# Entering a prefix containing @ must select the canonical result, not commit
# the incomplete prefix. Tab again advances to the subject as before.
xdotool key Tab Tab
expect_screen draft-contact-canonical 'Server Only <server@example.invalid>' 'Searching contacts'
xdotool type --clearmodifiers --delay 15 'NativeRecovery'
xdotool key Tab
xdotool type --clearmodifiers --delay 15 'Durable compose body'
click_word Bcc
xdotool type --clearmodifiers --delay 15 'hidden@example.invalid'
expect_screen compose-bcc 'hidden@example.invalid'
OLD_BACKEND="$BACKEND_PID"
kill -KILL "$OLD_BACKEND"
BACKEND_PID=""
expect_screen reconnecting 'Reconnecting'
expect_screen compose-recovered 'Durable compose body'
for _ in {1..100}; do
  BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli || true)
  if [[ -n "$BACKEND_PID" && "$BACKEND_PID" != "$OLD_BACKEND" ]]; then break; fi
  sleep 0.1
done
BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)
[[ "$BACKEND_PID" != "$OLD_BACKEND" ]]
assert_gone "$OLD_BACKEND"
# Escape/Save commits pending Bcc text and closes only after the Go save ACK.
xdotool key --clearmodifiers Escape
expect_screen compose-save-confirm 'Save to drafts'
kill -STOP "$BACKEND_PID"
xdotool key Return
expect_screen compose-save-pending 'Saving draft'
xdotool type --clearmodifiers --delay 15 'UNACCEPTED-LATE-TEXT'
expect_screen compose-save-locked 'Saving draft' 'UNACCEPTED-LATE-TEXT'
kill -CONT "$BACKEND_PID"
expect_screen compose-saved 'Draft saved' 'Save to drafts'
# Normal quit must also reap a still-running child, not only an already-crashed one.
xdotool key --clearmodifiers ctrl+q
for _ in {1..100}; do
  if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
  sleep 0.1
done
assert_gone "$APP_PID"
wait "$APP_PID"
APP_PID=""
assert_gone "$BACKEND_PID"
BACKEND_PID=""
[[ -z "$(find "$TMPDIR" -type d -name 'session-*' -print -quit)" ]]
echo 'PASS normal quit reaps the live owned backend and removes session files'

# A completely new app process must recover the same encrypted server draft.
"$APP" >> "$ROOT/app.log" 2>&1 &
APP_PID=$!
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"
expect_screen draft-restart-ready 'Reply body'
BACKEND_PID=$(pgrep -P "$APP_PID" -x durian-cli)
xdotool key --clearmodifiers g d
expect_screen drafts-recovered 'NativeRecovery'
click_word NativeRecovery
expect_screen draft-reopened 'Durable compose body' 'UNACCEPTED-LATE-TEXT'
expect_screen draft-bcc-restored 'hidden@example.invalid'
expect_screen draft-contact-restored 'server@example.invalid'
click_word Send
expect_screen compose-queued 'Queued for delivery'
[[ "$("$CLI" outbox list --json | jq length)" == 1 ]]
click_word Undo
expect_screen compose-undone 'Delivery cancelled'
[[ "$("$CLI" outbox list --json | jq length)" == 0 ]]
echo 'PASS native composer save/restart/Bcc/enqueue/confirmed Undo against encrypted Go state'

# A sent inline reply keeps its recovery receipt, not ownership of the next editor.
xdotool key --clearmodifiers g i
expect_screen inline-ready 'Reply body'
xdotool key --clearmodifiers r
xdotool type --clearmodifiers --delay 15 'InlineFirst'
expect_screen inline-edited 'InlineFirst'
kill -STOP "$BACKEND_PID"
click_word Send
expect_screen inline-pending 'Queuing reply'
xdotool type --clearmodifiers --delay 15 'UNACCEPTED-LATE-TEXT'
expect_screen inline-locked 'InlineFirst' 'UNACCEPTED-LATE-TEXT'
kill -CONT "$BACKEND_PID"
expect_screen inline-queued 'Queued for delivery' 'Queuing reply'
[[ "$("$CLI" outbox list --json | jq length)" == 1 ]]
xdotool key --clearmodifiers r
xdotool type --clearmodifiers --delay 15 'InlineSecond'
expect_screen inline-second 'InlineSecond' 'InlineFirst'
click_word Undo
expect_screen inline-undone 'Delivery cancelled'
[[ "$("$CLI" outbox list --json | jq length)" == 0 ]]
echo 'PASS inline ACK freezes input, releases the next reply, and confirmed Undo cancels only the first'
xdotool key --clearmodifiers ctrl+q
for _ in {1..100}; do
  if ! kill -0 "$APP_PID" 2>/dev/null; then break; fi
  sleep 0.1
done
assert_gone "$APP_PID"
wait "$APP_PID"
APP_PID=""
assert_gone "$BACKEND_PID"
BACKEND_PID=""
echo 'PASS GPUI native end-to-end'
