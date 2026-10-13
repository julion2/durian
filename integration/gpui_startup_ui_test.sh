#!/usr/bin/env bash
# Hold real Go search responses while driving the packaged native client's input.
set -euo pipefail
if [[ "${DURIAN_UI_DISPLAY:-}" != 1 ]]; then
  exec xvfb-run -a -s '-screen 0 1600x1000x24 -nolisten tcp' env DURIAN_UI_DISPLAY=1 bash "$0" "$@"
fi
APP=$(realpath "$1")
SEEDER=$(realpath "$2")
CONFIG=$(realpath "$3")
CLI="$(dirname "$APP")/durian-cli"
BASE=$(mktemp -d /tmp/durian-startup-ui-XXXXXX)
OUT="${DURIAN_UI_ARTIFACT_DIR:-${TEST_UNDECLARED_OUTPUTS_DIR:-$BASE/evidence}}"
mkdir -p "$OUT"
PIDS=()
stop_case() {
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
  PIDS=()
}
cleanup() { stop_case; rm -rf "$BASE"; }
trap cleanup EXIT

wait_file() {
  for _ in {1..200}; do [[ -e "$1" ]] && return; sleep 0.025; done
  echo "FAIL missing barrier $1" >&2; return 1
}
search_count() {
  local n
  n=$(jq -s '[.[] | select(.path == "/api/v1/search")] | length' "$CASE_OUT/requests.jsonl")
  [[ "$n" == "$1" ]] || { echo "FAIL expected $1 completed searches, got $n"; cat "$CASE_OUT/requests.jsonl"; return 1; }
}
screen() {
  import -window "$WINDOW" "$CASE_OUT/$1.png"
  # Only actual list glyphs, except the explicit full-width thread-opening case.
  local crop=390x660+252+105
  [[ "$1" == thread-* ]] && crop=1168x760+244+45
  convert "$CASE_OUT/$1.png" -crop "$crop" -resize 300% -colorspace Gray -negate "$ROOT/ocr.png"
  tesseract "$ROOT/ocr.png" stdout --psm 11 2>/dev/null > "$ROOT/list.txt"
}
expect_list() {
  for _ in {1..20}; do
    screen "$1"
    if grep -Eqi "$2" "$ROOT/list.txt" && { [[ -z "${3:-}" ]] || ! grep -Eqi "$3" "$ROOT/list.txt"; }; then
      echo "PASS native startup $CASE/$1"; return
    fi
    kill -0 "$APP_PID"; sleep 0.05
  done
  cat "$ROOT/list.txt"; echo "FAIL $CASE/$1" >&2; return 1
}
search_charlie() {
  xdotool key --clearmodifiers slash
  xdotool type --clearmodifiers --delay 15 'from:charlie '
  xdotool key Return
  expect_list searched 'Other Thread' 'Hello World'
  search_count 1
}

# Scroll, never select: opening a row fetches its full thread and hides missing
# search previews. Find the row number in the actual list's glyphs, not an API.
scroll_preview() {
  local target="$1" row
  xdotool mousemove --window "$WINDOW" 420 450
  for _ in {1..90}; do
    screen scrolling
    # Use a fully visible row, and its snippet from the SAME frame. The first
    # row can be clipped, and wheel momentum can move it out before another OCR.
    row=$(sed -nE 's/.*Preview row ([0-9]{3}).*/\1/p' "$ROOT/list.txt" | sed -n '3p')
    if [[ -n "$row" ]] && ((10#$row >= target)); then
      if grep -qi "Snippetnumber $row" "$ROOT/list.txt"; then
        cp "$CASE_OUT/scrolling.png" "$CASE_OUT/row-$row.png"
        echo "PASS native startup $CASE/row-$row"; return
      fi
    else
      xdotool click --repeat 5 --delay 15 5
    fi
    sleep 0.1
  done
  echo "FAIL did not scroll to preview row $target" >&2; return 1
}

for CASE in ${DURIAN_STARTUP_CASES:-initial headers navigation return return-pending refresh failure empty rate-limit previews previews-page previews-cancel thread-open}; do
  ROOT="$BASE/$CASE"; CASE_OUT="$OUT/$CASE"
  mkdir -p "$ROOT" "$CASE_OUT"
  export HOME="$ROOT/home" XDG_DATA_HOME="$ROOT/data" XDG_STATE_HOME="$ROOT/state"
  export XDG_CONFIG_HOME="$ROOT/config" XDG_CACHE_HOME="$ROOT/cache" XDG_RUNTIME_DIR="$ROOT/runtime"
  export TMPDIR="$ROOT/tmp" MAIL_APPEARANCE=dark MAIL_AVATARS=0 MAIL_TRACE=1 VK_ICD_FILENAMES=/dev/null
  unset MAIL_BACKEND MAIL_TOKEN DURIAN_BIN MAIL_FOLDER MAIL_BACKGROUND
  # HTML engine startup is unrelated to request ownership; use the supported text fallback.
  export MAILRENDER_BIN="$ROOT/no-renderer"
  export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  mkdir -p "$HOME" "$XDG_CONFIG_HOME/durian" "$XDG_DATA_HOME/durian" "$TMPDIR" "$XDG_RUNTIME_DIR"
  chmod 700 "$TMPDIR" "$XDG_RUNTIME_DIR"
  sed -e '/    smtp {/d' -e '/    imap {/d' -e '/    auth {/d' "$CONFIG" > "$XDG_CONFIG_HOME/durian/config.pkl"
  cp "$(dirname "$CONFIG")/profiles.pkl" "$XDG_CONFIG_HOME/durian/profiles.pkl"
  SEED_ARGS=()
  [[ "$CASE" == previews* ]] && SEED_ARGS=(-list-rows 300)
  # The growing enriched prefix exceeds ureq's 10 MiB response bound at 200 rows.
  # A 50-row batch fits; paging must not redownload all preceding message bodies.
  [[ "$CASE" == previews-page ]] && SEED_ARGS+=(-list-body-bytes 65536)
  "$SEEDER" "${SEED_ARGS[@]}" "$XDG_DATA_HOME/durian/email.db" "$XDG_DATA_HOME/durian/contacts.db"
  # Opening unread mail legitimately writes tags and refreshes. Isolate startup
  # reads here; the full native mail suite covers mark-read freshness separately.
  "$CLI" tag --all '*' -unread > "$CASE_OUT/read-fixture.log"
  if [[ "$CASE" == empty ]]; then
    "$CLI" tag --all '*' -inbox > "$CASE_OUT/empty-fixture.log"
  fi
  if [[ "$CASE" == rate-limit ]]; then
    # Many configured folder badges without changing the visible message rows.
    {
      cat <<'EOF'
import "modulepath:/Profiles.pkl" as P
profiles: Listing<P.ProfileConfig> = new {
  new { name = "All"; accounts = new { "*" }; folders = new {
EOF
      for ((i = 0; i < 80; i++)); do
        printf '    new { name = "Budget %s"; query = "tag:inbox AND NOT tag:missing-%s" }\n' "$i" "$i"
      done
      cat <<'EOF'
  } }
  new { name = "Test"; accounts = new { "test" }; default = true; folders = new { new { name = "Inbox"; query = "tag:inbox" } } }
}
EOF
    } > "$XDG_CONFIG_HOME/durian/profiles.pkl"
  fi
  # Separate from the backend/calendar contracts' 19724/19725 ports.
  PORT=19726
  "$CLI" serve --port "$PORT" --exit-when-orphaned > "$ROOT/ready.log" 2> "$CASE_OUT/server.log" &
  SERVER_PID=$!; PIDS+=("$SERVER_PID")
  for _ in {1..200}; do
    grep -q '^READY ' "$ROOT/ready.log" && break
    kill -0 "$SERVER_PID"; sleep 0.025
  done
  export MAIL_TOKEN
  MAIL_TOKEN=$(sed -n 's/^READY .*token=\([^ ]*\).*/\1/p' "$ROOT/ready.log")
  PORT=$(sed -n 's/^READY .*addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$ROOT/ready.log")
  [[ -n "$MAIL_TOKEN" && -n "$PORT" ]]
  MODE=hold; [[ "$CASE" == failure ]] && MODE=hold-reject
  [[ "$CASE" == headers ]] && MODE=hold-headers
  echo "GET /api/v1/search $MODE" > "$ROOT/fault"
  if [[ "$CASE" == thread-open ]]; then
    thread=$("$CLI" search from:bob --json | jq -er '.[0].thread_id')
    echo "GET /api/v1/threads/$thread hold" > "$ROOT/fault"
  fi
  : > "$CASE_OUT/requests.jsonl"
  python3 "$(dirname "$CONFIG")/fault_proxy.py" "$PORT" "$ROOT/fault" "$CASE_OUT/requests.jsonl" "$ROOT/proxy.port" > "$CASE_OUT/proxy.log" 2>&1 &
  PIDS+=("$!")
  wait_file "$ROOT/proxy.port"
  export MAIL_BACKEND="http://127.0.0.1:$(cat "$ROOT/proxy.port")"
  openbox --sm-disable > "$ROOT/wm.log" 2>&1 &
  PIDS+=("$!")
  "$APP" > "$CASE_OUT/app.log" 2>&1 &
  APP_PID=$!; PIDS+=("$APP_PID")
  WINDOW=$(timeout 20 xdotool search --sync --onlyvisible --name '^Durian$' | head -1)
  xdotool windowactivate --sync "$WINDOW"
  wait_file "$ROOT/fault.held"
  for _ in {1..100}; do
    grep -q 'accounts, .*addresses, .*tags' "$CASE_OUT/app.log" && break
    sleep 0.025
  done
  grep -q 'accounts, .*addresses, .*tags' "$CASE_OUT/app.log"
  sleep 0.5
  # Identity/profile completion must not dispatch another copy of the held request.
  if [[ "$CASE" == thread-open ]]; then search_count 1; else search_count 0; fi
  jq -se '[.[] | select(.path == "/api/v1/profiles")] | length == 1' "$CASE_OUT/requests.jsonl" > /dev/null
  case "$CASE" in
    thread-open)
      expect_list ready 'Hello World'
      xdotool windowsize --sync "$WINDOW" 1420 500
      xdotool key --clearmodifiers l
      expect_list thread-pending 'Loading conversation'
      touch "$ROOT/fault.release"
      expect_list thread-latest 'Message[[:space:]]*3[[:space:]]*[o0]f[[:space:]]*3'
      grep -qi 'Reply body' "$ROOT/list.txt"
      # A later refresh must not jump back to newest after explicit navigation.
      xdotool key --clearmodifiers g g
      expect_list thread-first 'Message[[:space:]]*[1lI][[:space:]]*[o0]f[[:space:]]*3'
      completed=$(grep -c "durian: thread $thread in" "$CASE_OUT/app.log")
      xdotool key --clearmodifiers ctrl+r
      for _ in {1..400}; do
        [[ $(grep -c "durian: thread $thread in" "$CASE_OUT/app.log") -gt "$completed" ]] && break
        kill -0 "$APP_PID"; sleep 0.025
      done
      [[ $(grep -c "durian: thread $thread in" "$CASE_OUT/app.log") -gt "$completed" ]]
      expect_list thread-refreshed 'First message body'
      grep -Eqi 'Message[[:space:]]*[1lI][[:space:]]*[o0]f[[:space:]]*3' "$ROOT/list.txt"
      ;;
    initial|headers)
      touch "$ROOT/fault.release"
      expect_list ready 'Hello World'
      search_count 1
      ;;
    navigation)
      search_charlie
      touch "$ROOT/fault.release"
      sleep 0.4
      expect_list late-inbox 'Other Thread' 'Hello World'
      search_count 2
      ;;
    return)
      search_charlie
      xdotool key --clearmodifiers slash ctrl+a BackSpace Return
      sleep 0.4
      search_count 1
      touch "$ROOT/fault.release"
      expect_list returned 'Hello World'
      search_count 2
      ;;
    return-pending)
      # Keep both answers pending, then return to Inbox. The other query's
      # in-flight response must not replace the reused startup page.
      rm "$ROOT/fault.held"
      echo 'GET /api/v1/search hold' > "$ROOT/fault"
      xdotool key --clearmodifiers slash
      xdotool type --clearmodifiers --delay 15 'from:charlie '
      xdotool key Return
      wait_file "$ROOT/fault.held"
      xdotool key --clearmodifiers slash ctrl+a BackSpace Return
      sleep 0.4
      search_count 0
      touch "$ROOT/fault.release"
      sleep 0.4
      expect_list returned-pending 'Hello World'
      search_count 2
      ;;
    refresh)
      # The held page contains Other Thread. Change real SQLite state, then
      # refresh: its late response must not resurrect the removed inbox row.
      "$CLI" tag 'from:charlie' -inbox > "$CASE_OUT/tag.log"
      xdotool key --clearmodifiers ctrl+r
      expect_list refreshed 'Hello World' 'Other Thread'
      search_count 1
      touch "$ROOT/fault.release"
      sleep 0.4
      expect_list late-refresh 'Hello World' 'Other Thread'
      search_count 2
      ;;
    failure)
      touch "$ROOT/fault.release"
      expect_list failed 'Try again'
      search_count 1
      xdotool key --clearmodifiers ctrl+r
      expect_list retried 'Hello World'
      search_count 2
      ;;
    empty)
      touch "$ROOT/fault.release"
      expect_list empty 'Inbox zero' 'Hello World|Other Thread|Loading'
      search_count 1
      # Empty success still releases the queued sidebar count batch.
      jq -se 'any(.[]; .path == "/api/v1/search/count" and .status == 200)' "$CASE_OUT/requests.jsonl" > /dev/null
      ;;
    rate-limit)
      touch "$ROOT/fault.release"
      expect_list ready 'Hello World'
      # Navigate while the large sidebar is still filling, not after it settles.
      xdotool key --clearmodifiers slash
      xdotool type --clearmodifiers --delay 15 'from:charlie '
      xdotool key Return
      expect_list foreground 'Other Thread' 'Hello World'
      # Frequent same-scope refreshes must queue a follow-up, not restart the
      # long batch forever and leave its tail permanently unknown.
      for _ in {1..20}; do
        grep -q 'sidebar counts, 0 failed' "$CASE_OUT/app.log" && break
        kill -0 "$APP_PID"
        xdotool key --clearmodifiers ctrl+r
        sleep 1
      done
      grep -q 'sidebar counts, 0 failed' "$CASE_OUT/app.log"
      jq -se '[.[] | select(.path == "/api/v1/search/count" and .status == 200) | .query] | unique | length >= 80' "$CASE_OUT/requests.jsonl" > /dev/null
      jq -se 'all(.[]; .status != 429)' "$CASE_OUT/requests.jsonl" > /dev/null
      echo 'PASS native large sidebar and foreground search without any HTTP 429'
      ;;
    previews|previews-page)
      touch "$ROOT/fault.release"
      expect_list ready 'Hello World'
      target=60; [[ "$CASE" == previews-page ]] && target=220
      scroll_preview "$target"
      # A refresh must not replace the scrolled page with blank summaries either.
      completed=$(grep -cE 'durian: search #[0-9]+: .* rows' "$CASE_OUT/app.log")
      xdotool key --clearmodifiers ctrl+r
      for _ in {1..400}; do
        [[ $(grep -cE 'durian: search #[0-9]+: .* rows' "$CASE_OUT/app.log") -gt "$completed" ]] && break
        kill -0 "$APP_PID"; sleep 0.025
      done
      [[ $(grep -cE 'durian: search #[0-9]+: .* rows' "$CASE_OUT/app.log") -gt "$completed" ]]
      scroll_preview "$target"
      if [[ "$CASE" == previews-page ]]; then
        # Earlier unselected rows must retain their previews after more pages arrive.
        xdotool click --repeat 600 --delay 2 4
        expect_list retained-top 'Hello World'
        scroll_preview 60
      fi
      jq -se 'all(.[]; .status != 429)' "$CASE_OUT/requests.jsonl" > /dev/null
      ;;
    previews-cancel)
      touch "$ROOT/fault.release"
      expect_list ready 'Hello World'
      rm "$ROOT/fault.held" "$ROOT/fault.release"
      echo 'GET /api/v1/search hold' > "$ROOT/fault"
      xdotool mousemove --window "$WINDOW" 420 450
      xdotool click --repeat 60 --delay 15 5
      wait_file "$ROOT/fault.held"
      xdotool key --clearmodifiers slash
      xdotool type --clearmodifiers --delay 15 'from:charlie '
      xdotool key Return
      expect_list navigated 'Other Thread' 'Preview row'
      search_count 2
      touch "$ROOT/fault.release"
      sleep 0.5
      expect_list late-page 'Other Thread' 'Preview row'
      # Superseded paging must not start its remaining preview GETs.
      search_count 3
      ;;
  esac
  xdotool key --clearmodifiers ctrl+q
  wait "$APP_PID"
  stop_case
done
echo 'PASS GPUI native startup ownership, navigation, refresh and retry'
