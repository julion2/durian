#!/usr/bin/env bash
# Native calendar writes through a fault-injecting proxy into the real Go server.
set -euo pipefail
if [[ "${DURIAN_UI_DISPLAY:-}" != 1 ]]; then
  exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' env DURIAN_UI_DISPLAY=1 "$0" "$@"
fi
APP=$(realpath "$1")
SEEDER=$(realpath "$2")
CONFIG=$(realpath "$3")
CLI="$(dirname "$APP")/durian-cli"
# Keep WebKit's runtime Unix socket paths below sockaddr_un's limit.
ROOT=$(mktemp -d /tmp/durian-calendar-ui-XXXXXX)
OUT="${DURIAN_UI_ARTIFACT_DIR:-${TEST_UNDECLARED_OUTPUTS_DIR:-$ROOT/evidence}}"
mkdir -p "$OUT"
PIDS=()
cleanup() {
  cp "$ROOT/requests.jsonl" "$OUT/requests.jsonl" 2>/dev/null || true
  cp "$ROOT/app.log" "$OUT/app.log" 2>/dev/null || true
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$ROOT"
}
trap cleanup EXIT
export HOME="$ROOT/home" XDG_DATA_HOME="$ROOT/data" XDG_STATE_HOME="$ROOT/state"
export XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_RUNTIME_DIR="$ROOT/runtime"
export TMPDIR="$ROOT/tmp" MAIL_APPEARANCE=dark MAIL_AVATARS=0 MAIL_TRACE=1
export VK_ICD_FILENAMES=/dev/null
unset MAIL_BACKEND MAIL_TOKEN DURIAN_BIN MAIL_FOLDER MAIL_BACKGROUND MAILRENDER_BIN
mkdir -p "$HOME" "$XDG_CONFIG_HOME/durian" "$XDG_DATA_HOME/durian" "$TMPDIR" "$XDG_RUNTIME_DIR" "$ROOT/calendar"
chmod 700 "$TMPDIR" "$XDG_RUNTIME_DIR"
sed -e '/    smtp {/d' -e '/    imap {/d' -e '/    auth {/d' "$CONFIG" > "$XDG_CONFIG_HOME/durian/config.pkl"
cp "$(dirname "$CONFIG")/profiles.pkl" "$XDG_CONFIG_HOME/durian/profiles.pkl"
cat >> "$XDG_CONFIG_HOME/durian/config.pkl" <<EOF

calendar: C.CalendarConfig = new {
  vdir_path = "$ROOT/vdir"
  local_calendars = new Listing {
    new C.LocalCalendar { name = "Scratch"; path = "$ROOT/calendar" }
  }
}
EOF
export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
"$SEEDER" "$XDG_DATA_HOME/durian/email.db" "$XDG_DATA_HOME/durian/contacts.db"
for uid in lost lost-neighbor; do
  title=LostTarget
  if [[ "$uid" == lost-neighbor ]]; then title=KeepNeighbor; fi
  printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//durian//test//EN\r\nBEGIN:VEVENT\r\nUID:%s\r\nSUMMARY:%s\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n' \
    "$uid" "$title" "$(date +%Y%m%d)" "$(date -d tomorrow +%Y%m%d)" > "$ROOT/calendar/$uid.ics"
done
sha256sum "$ROOT/calendar/lost-neighbor.ics" | cut -d' ' -f1 > "$ROOT/neighbor.sha"
"$CLI" serve --port 19725 --exit-when-orphaned > "$ROOT/ready.log" 2> "$ROOT/server.log" &
SERVER_PID=$!; PIDS+=("$SERVER_PID")
for _ in {1..200}; do
  if grep -q '^READY ' "$ROOT/ready.log"; then break; fi
  kill -0 "$SERVER_PID"; sleep 0.05
done
export MAIL_TOKEN
MAIL_TOKEN=$(sed -n 's/^READY .*token=\([^ ]*\).*/\1/p' "$ROOT/ready.log")
[[ -n "$MAIL_TOKEN" ]]
python3 "$(dirname "$CONFIG")/fault_proxy.py" 19725 "$ROOT/fault" "$ROOT/requests.jsonl" "$ROOT/proxy.port" &
PIDS+=("$!")
for _ in {1..100}; do [[ -s "$ROOT/proxy.port" ]] && break; sleep 0.05; done
export MAIL_BACKEND="http://127.0.0.1:$(cat "$ROOT/proxy.port")"
openbox --sm-disable > "$ROOT/wm.log" 2>&1 &
PIDS+=("$!")
"$APP" > "$ROOT/app.log" 2>&1 &
APP_PID=$!; PIDS+=("$APP_PID")
WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
xdotool windowactivate --sync "$WINDOW"

screen() {
  import -window "$WINDOW" "$OUT/$1.png"
  convert "$OUT/$1.png" -resize 300% -colorspace Gray -negate "$ROOT/ocr.png"
  tesseract "$ROOT/ocr.png" stdout --psm 11 2>/dev/null > "$ROOT/screen.txt"
  # White labels on the accent button need a different threshold than dim text.
  convert "$OUT/$1.png" -resize 300% -colorspace Gray -threshold 60% -negate "$ROOT/buttons.png"
  tesseract "$ROOT/buttons.png" stdout --psm 11 2>/dev/null >> "$ROOT/screen.txt"
  if [[ "$1" == create-* || "$1" == edit-* ]]; then
    # Preserve red warnings and remove the modal's filled background, which
    # sparse-page OCR otherwise sometimes treats as a non-text illustration.
    convert "$OUT/$1.png" -channel R -separate +channel -resize 300% -threshold 50% -negate "$ROOT/warnings.png"
    tesseract "$ROOT/warnings.png" stdout --psm 11 2>/dev/null >> "$ROOT/screen.txt"
  fi
}
expect_screen() {
  for _ in {1..30}; do
    screen "$1"
    if grep -Eqi "$2" "$ROOT/screen.txt" && { [[ -z "${3:-}" ]] || ! grep -Eqi "$3" "$ROOT/screen.txt"; }; then
      echo "PASS native calendar $1"; return
    fi
    kill -0 "$APP_PID"; sleep 0.2
  done
  cat "$ROOT/screen.txt" >&2
  echo "FAIL native calendar $1: expected $2, absent ${3:-}" >&2
  return 1
}
click_word() {
  screen control
  local position
  position=$(tesseract "$ROOT/ocr.png" stdout --psm 11 tsv 2>/dev/null |
    awk -F '\t' -v word="$1" '$12 == word && (word != "Recover" || $7/3 < 300) { print int(($7+$9/2)/3), int(($8+$10/2)/3); exit }')
  if [[ -z "$position" ]]; then
    position=$(tesseract "$ROOT/buttons.png" stdout --psm 11 tsv 2>/dev/null |
      awk -F '\t' -v word="$1" '$12 == word && (word != "Recover" || $7/3 < 300) { print int(($7+$9/2)/3), int(($8+$10/2)/3); exit }')
  fi
  [[ -n "$position" ]] || { echo "FAIL missing control $1" >&2; return 1; }
  read -r x y <<< "$position"
  xdotool mousemove --window "$WINDOW" "$x" "$y" click 1
}

expect_screen mail-ready 'Reply body'
xdotool key --clearmodifiers ctrl+0
xdotool key --clearmodifiers g c
xdotool key a
expect_screen calendar-ready 'LostTarget'
click_word LostTarget
printf 'DELETE /api/v1/calendars/event after\n' > "$ROOT/fault"
xdotool key --clearmodifiers Delete
expect_screen delete-confirm 'Delete this event'
xdotool key Return
for _ in {1..100}; do [[ ! -e "$ROOT/calendar/lost.ics" ]] && break; sleep 0.05; done
[[ ! -e "$ROOT/calendar/lost.ics" ]]
[[ "$(sha256sum "$ROOT/calendar/lost-neighbor.ics" | cut -d' ' -f1)" == "$(cat "$ROOT/neighbor.sha")" ]]
echo 'PASS real Go deletion committed, prefix neighbor bytes unchanged, ACK dropped'
expect_screen delete-uncertain 'Recover calendar changes' 'LostTarget'
click_word Recover
expect_screen delete-reconciled 'KeepNeighbor' 'Recover calendar changes'
[[ "$(jq -s '[.[] | select(.method == "DELETE")] | length' "$ROOT/requests.jsonl")" == 1 ]]
[[ "$(jq -s '[.[] | select(.path == "/api/v1/calendars/sync/event")] | length' "$ROOT/requests.jsonl")" == 0 ]]
[[ "$(sha256sum "$ROOT/calendar/lost-neighbor.ics" | cut -d' ' -f1)" == "$(cat "$ROOT/neighbor.sha")" ]]
echo 'PASS recovery reconciles exact local identity without another DELETE or provider request'

# Control: a dropped request never reached Go. A failed reconciliation must keep
# the action visible; a later successful read must not delete the still-present event.
click_word KeepNeighbor
printf 'DELETE /api/v1/calendars/event before\n' > "$ROOT/fault"
xdotool key --clearmodifiers Delete
expect_screen unsubmitted-confirm 'Delete this event'
xdotool key Return
expect_screen unsubmitted-uncertain 'Recover calendar changes'
printf 'GET /api/v1/calendars reject\n' > "$ROOT/fault"
click_word Recover
expect_screen recovery-failed 'Calendar recovery failed'
expect_screen recovery-retained 'Recover calendar changes'
click_word Recover
expect_screen unsubmitted-reconciled 'KeepNeighbor' 'Recover calendar changes'
[[ "$(jq -s '[.[] | select(.method == "DELETE")] | length' "$ROOT/requests.jsonl")" == 2 ]]
[[ "$(sha256sum "$ROOT/calendar/lost-neighbor.ics" | cut -d' ' -f1)" == "$(cat "$ROOT/neighbor.sha")" ]]
echo 'PASS failed recovery retains scope; present event is not deleted or automatically replayed'

# A lost create ACK keeps the complete editor and stable UID. Retrying the
# explicit Save updates that same file rather than producing another event.
printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//durian//test//EN\r\nBEGIN:VEVENT\r\nUID:other-conflict\r\nSUMMARY:OtherConflict\r\nDTSTART:%sT213000Z\r\nDTEND:%sT220000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n' \
  "$(date -d tomorrow +%Y%m%d)" "$(date -d tomorrow +%Y%m%d)" > "$ROOT/calendar/other-conflict.ics"
xdotool key --clearmodifiers ctrl+r
expect_screen conflict-loaded 'OtherConflict'
xdotool key --clearmodifiers Escape c
expect_screen create-empty 'New event'
xdotool type --clearmodifiers --delay 15 'NativeCreated'
xdotool key Tab ctrl+a
xdotool type --clearmodifiers --delay 15 "$(date -d tomorrow +%d.%m.%Y) 21:00 - $(date -d '2 days' +%d.%m.%Y) 00:00"
xdotool key Tab Tab
xdotool type --clearmodifiers --delay 15 'RoomNine'
xdotool key Tab
xdotool type --clearmodifiers --delay 15 'Preserve calendar notes'
printf 'PUT /api/v1/calendars/event after\n' > "$ROOT/fault"
click_word Save
expect_screen create-ack-lost 'Calendar request failed'
expect_screen create-editor-kept 'Preserve calendar notes' 'Overlaps NativeCreated'
expect_screen create-other-conflict 'Overlaps OtherConflict' 'Overlaps NativeCreated'
[[ "$(find "$ROOT/calendar" -maxdepth 1 -name '*.ics' | wc -l)" == 3 ]]
CREATED=$(grep -l 'SUMMARY:NativeCreated' "$ROOT/calendar/"*.ics)
[[ -n "$CREATED" ]]
grep -q "DTSTART:$(date -d tomorrow +%Y%m%d)T210000Z" "$CREATED"
grep -q "DTEND:$(date -d '2 days' +%Y%m%d)T000000Z" "$CREATED"
click_word Save
expect_screen create-retried 'NativeCreated' 'New event'
[[ "$(find "$ROOT/calendar" -maxdepth 1 -name '*.ics' | wc -l)" == 3 ]]
grep -q 'Preserve calendar notes' "$CREATED"
echo 'PASS native create retry preserves stable UID, description and next-day midnight end'

click_word NativeCreated
xdotool key --clearmodifiers e
expect_screen edit-open 'Edit event'
xdotool key ctrl+a
xdotool type --clearmodifiers --delay 15 'NativeEdited'
# Native Control clipboard shortcuts must preserve the actual title, not append
# to it. Right collapses a selection if Cut failed, exposing an accidental copy.
xdotool key --clearmodifiers ctrl+a ctrl+c
xdotool type --clearmodifiers --delay 15 'ShouldNotPersist'
xdotool key --clearmodifiers ctrl+a ctrl+v ctrl+a ctrl+x Right ctrl+v
printf 'PUT /api/v1/calendars/event reject\n' > "$ROOT/fault"
click_word Save
expect_screen edit-rejected 'Calendar request failed'
grep -q 'SUMMARY:NativeCreated' "$CREATED"
click_word Save
expect_screen edit-saved 'NativeEdited' 'Edit event'
[[ "$(sed -n 's/\r$//;s/^SUMMARY://p' "$CREATED")" == NativeEdited ]]
grep -q "DTEND:$(date -d '2 days' +%Y%m%d)T000000Z" "$CREATED"
[[ "$(find "$ROOT/calendar" -maxdepth 1 -name '*.ics' | wc -l)" == 3 ]]
xdotool key --clearmodifiers Delete
expect_screen acknowledged-delete-confirm 'Delete this event'
xdotool key Return
xdotool key t
expect_screen acknowledged-delete 'KeepNeighbor' 'NativeEdited'
[[ ! -e "$CREATED" ]]
echo 'PASS native edit rejection keeps editor; explicit retry and acknowledged deletion persist'

# The invite is read/written by Go. The proxy refuses the subsequent provider
# sync so no provider can be contacted and failed scoped sync stays recoverable.
mkdir -p "$ROOT/vdir/test/Work"
printf 'Work\n' > "$ROOT/vdir/test/Work/displayname"
INVITE="$ROOT/vdir/test/Work/invitation.ics"
printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//durian//test//EN\r\nBEGIN:VEVENT\r\nUID:invitation\r\nSUMMARY:TeamReview\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\nORGANIZER:mailto:organizer@example.invalid\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:test@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n' \
  "$(date +%Y%m%d)" "$(date -d tomorrow +%Y%m%d)" > "$INVITE"
xdotool key --clearmodifiers ctrl+r
expect_screen invitation-loaded 'TeamReview'
click_word TeamReview
expect_screen invitation-details 'Going'
printf 'POST /api/v1/calendars/rsvp reject\n' > "$ROOT/fault"
click_word Going
expect_screen rsvp-rejected 'Calendar request failed'
grep -q 'PARTSTAT=NEEDS-ACTION' "$INVITE"
click_word Going
expect_screen rsvp-sync-pending 'Recover calendar changes'
grep -q 'PARTSTAT=ACCEPTED' "$INVITE"
click_word Recover
expect_screen rsvp-sync-still-pending 'Recover calendar changes'
[[ "$(jq -s '[.[] | select(.path == "/api/v1/calendars/rsvp")] | length' "$ROOT/requests.jsonl")" == 2 ]]
jq -se '[.[] | select(.path == "/api/v1/calendars/sync/event")] | length >= 2 and all(.[]; .status == 503 and .scope == {"account":"test","calendar":"Work","uid":"invitation","operation":"rsvp"})' "$ROOT/requests.jsonl"
echo 'PASS native RSVP rejection and acceptance persist; sync retry is scoped and never replays RSVP'

xdotool key --clearmodifiers ctrl+q
for _ in {1..100}; do ! kill -0 "$APP_PID" 2>/dev/null && break; sleep 0.1; done
! kill -0 "$APP_PID" 2>/dev/null
wait "$APP_PID"
echo 'PASS GPUI native calendar write contract'
