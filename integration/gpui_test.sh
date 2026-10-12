#!/usr/bin/env bash
# Real GPUI Rust client -> authenticated Go server, using only throwaway encrypted data.
set -euo pipefail

CLIENT="$1"
SEEDER="$2"
DURIAN="$3"
CONFIG="$4"
ROOT=$(mktemp -d "${TEST_TMPDIR:-/tmp}/durian-gpui-contract-XXXXXX")
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$ROOT"
}
trap cleanup EXIT

export HOME="$ROOT/home"
export XDG_DATA_HOME="$ROOT/data"
export XDG_STATE_HOME="$ROOT/state"
mkdir -p "$HOME/config"
cp "$CONFIG" "$HOME/config/config.pkl"
cp "$(dirname "$CONFIG")/profiles.pkl" "$HOME/config/profiles.pkl"
# The write contract uses only a local calendar, never a provider-backed sync.
cat >> "$HOME/config/config.pkl" <<EOF

calendar: C.CalendarConfig = new {
  vdir_path = "$XDG_DATA_HOME/durian/calendars"
  local_calendars = new Listing {
    new C.LocalCalendar { name = "Scratch"; path = "$ROOT/local-calendar" }
  }
}
EOF
# Deliberately public, throwaway fixture key, identical for seeder and server.
export DURIAN_MASTER_KEY_HEX_SECRET="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
"$SEEDER" "$ROOT/email.db" "$ROOT/contacts.db"
CALENDAR="$XDG_DATA_HOME/durian/calendars/test/Work"
mkdir -p "$CALENDAR"
printf 'Work\n' > "$CALENDAR/displayname"
printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//durian//gpui-test//EN\r\nBEGIN:VEVENT\r\nUID:gpui-contract-event\r\nSUMMARY:GPUI contract meeting\r\nDTSTART:20260801T120000Z\r\nDTEND:20260801T130000Z\r\nORGANIZER:mailto:organizer@example.invalid\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:test@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n' > "$CALENDAR/gpui-contract-event.ics"

# Different from the existing integration test's 19723; Bazel runs each target once.
PORT=19724
"$DURIAN" serve --port "$PORT" --exit-when-orphaned --db "$ROOT/email.db" --contacts-db "$ROOT/contacts.db" -c "$HOME/config/config.pkl" > "$ROOT/ready.log" 2> "$ROOT/server.log" &
SERVER_PID=$!
for _ in {1..200}; do
  if grep -q '^READY ' "$ROOT/ready.log"; then break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "FAIL backend exited before READY" >&2
    exit 1
  fi
  sleep 0.05
done
READY=$(grep '^READY ' "$ROOT/ready.log" | head -1)
[[ "$READY" == *" api=1" ]]
export DURIAN_TEST_TOKEN
DURIAN_TEST_TOKEN=$(printf '%s' "$READY" | sed -n 's/.*token=\([^ ]*\).*/\1/p')
[[ -n "$DURIAN_TEST_TOKEN" ]]
export DURIAN_TEST_URL="http://127.0.0.1:$PORT"
"$CLIENT"
echo 'PASS GPUI real-backend contract'
