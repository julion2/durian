#!/bin/bash
# Integration test: starts Go durian server with seeded test data,
# validates API contract via curl + jq, then cleans up.
#
# Each assertion validates a JSON field type/path that maps 1:1
# to a Swift Decodable struct field. If it breaks here, Swift breaks too.
set -euo pipefail

SEEDER="$1"
DURIAN="$2"
TEST_CONFIG="$3"
PORT=19723
TMPDIR=$(mktemp -d /tmp/durian-inttest-XXXXXX)
export HOME="${HOME:-$TMPDIR}"
EMAIL_DB="$TMPDIR/email.db"
CONTACTS_DB="$TMPDIR/contacts.db"
FAILURES=0
PASSED=0

cleanup() {
    if [ -n "${SERVER_PID:-}" ]; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

fail() {
    echo "FAIL: $1"
    FAILURES=$((FAILURES + 1))
}

pass() {
    echo "PASS: $1"
    PASSED=$((PASSED + 1))
}

assert_jq() {
    local desc="$1" json="$2" expr="$3"
    if echo "$json" | jq -e "$expr" > /dev/null 2>&1; then
        pass "$desc"
    else
        fail "$desc (expression: $expr)"
        echo "  Response: $(echo "$json" | head -c 200)"
    fi
}

assert_http_code() {
    local desc="$1" url="$2" method="${3:-GET}" expected="$4" body="${5:-}"
    local code
    if [ "$method" = "GET" ]; then
        code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" "$url")
    else
        code=$(curl -s -o /dev/null -w "%{http_code}" "${AUTH[@]}" -X "$method" -H "Content-Type: application/json" -d "$body" "$url")
    fi
    if [ "$code" = "$expected" ]; then
        pass "$desc"
    else
        fail "$desc (got $code, want $expected)"
    fi
}

# --- Setup ---

# ADR-0001 step 4/5: seeder and serve must agree on the master encryption
# key, otherwise serve cannot decrypt the subjects the seeder wrote.
# DURIAN_MASTER_KEY_HEX is read by both binaries; the keychain is bypassed
# in this test environment (headless CI has no Secret Service). The value
# is throwaway test data — no production secret.
export DURIAN_MASTER_KEY_HEX="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

echo "==> Seeding test databases"
"$SEEDER" "$EMAIL_DB" "$CONTACTS_DB"

# --- Validate config ---
echo "==> Validating Pkl config"
if "$DURIAN" validate config -c "$TEST_CONFIG"; then
    pass "durian validate config"
else
    fail "durian validate config"
fi

echo "==> Starting durian serve on port $PORT"
# Capture stdout to read the READY line with auth token
READY_PIPE=$(mktemp -u)
mkfifo "$READY_PIPE"
"$DURIAN" serve --port "$PORT" --db "$EMAIL_DB" --contacts-db "$CONTACTS_DB" -c "$TEST_CONFIG" > "$READY_PIPE" &
SERVER_PID=$!

echo "==> Waiting for server..."
TOKEN=""
if read -r -t 10 READY_LINE < "$READY_PIPE"; then
    echo "$READY_LINE"
    TOKEN=$(echo "$READY_LINE" | grep -o 'token=[^ ]*' | cut -d= -f2)
fi
rm -f "$READY_PIPE"

if [ -z "$TOKEN" ]; then
    echo "ERROR: Server not responding after 10s"; exit 1
fi
echo "==> Server ready (token received)"

BASE="http://localhost:$PORT/api/v1"
AUTH=(-H "Authorization: Bearer $TOKEN")

echo ""
echo "=== API Contract Tests ==="
echo ""

# ─────────────────────────────────────────────
# 1. Version
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/version")
assert_jq "GET /version .version is string" "$RESP" '.version | type == "string"'
assert_jq "GET /version .commit is string" "$RESP" '.commit | type == "string"'

# ─────────────────────────────────────────────
# 2. Search (DurianResponse + MailSearchResult)
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/search?query=tag:inbox&limit=10")
assert_jq "GET /search .ok is true" "$RESP" '.ok == true'
assert_jq "GET /search .results is array" "$RESP" '.results | type == "array"'
assert_jq "GET /search .results[0].thread_id is string" "$RESP" '.results[0].thread_id | type == "string"'
assert_jq "GET /search .results[0].subject is string" "$RESP" '.results[0].subject | type == "string"'
assert_jq "GET /search .results[0].from is string" "$RESP" '.results[0].from | type == "string"'
assert_jq "GET /search .results[0].date is string" "$RESP" '.results[0].date | type == "string"'
assert_jq "GET /search .results[0].timestamp is number" "$RESP" '.results[0].timestamp | type == "number"'
assert_jq "GET /search .results[0].tags is string" "$RESP" '.results[0].tags | type == "string"'

# 3. Search count
RESP=$(curl -sf "${AUTH[@]}" "$BASE/search/count?query=tag:inbox")
assert_jq "GET /search/count .count is number" "$RESP" '.count | type == "number"'
assert_jq "GET /search/count .count > 0" "$RESP" '.count > 0'

# Error: missing query
assert_http_code "GET /search without query → 400" "$BASE/search" "GET" "400"

# ─────────────────────────────────────────────
# 4. Tags (DurianResponse + tags array)
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/tags")
assert_jq "GET /tags .ok is true" "$RESP" '.ok == true'
assert_jq "GET /tags .tags is array" "$RESP" '.tags | type == "array"'
assert_jq "GET /tags .tags contains inbox" "$RESP" '.tags | index("inbox") != null'

# ─────────────────────────────────────────────
# 5. Show thread (DurianResponse + ThreadContent + ThreadMessage)
# ─────────────────────────────────────────────
THREAD_ID=$(curl -sf "${AUTH[@]}" "$BASE/search?query=tag:inbox&limit=1" | jq -r '.results[0].thread_id')
RESP=$(curl -sf "${AUTH[@]}" "$BASE/threads/$THREAD_ID")
assert_jq "GET /threads/{id} .ok is true" "$RESP" '.ok == true'
assert_jq "GET /threads/{id} .thread.thread_id is string" "$RESP" '.thread.thread_id | type == "string"'
assert_jq "GET /threads/{id} .thread.subject is string" "$RESP" '.thread.subject | type == "string"'
assert_jq "GET /threads/{id} .thread.messages is array" "$RESP" '.thread.messages | type == "array"'
assert_jq "GET /threads/{id} message.id is string" "$RESP" '.thread.messages[0].id | type == "string"'
assert_jq "GET /threads/{id} message.from is string" "$RESP" '.thread.messages[0].from | type == "string"'
assert_jq "GET /threads/{id} message.date is string" "$RESP" '.thread.messages[0].date | type == "string"'
assert_jq "GET /threads/{id} message.timestamp is number" "$RESP" '.thread.messages[0].timestamp | type == "number"'
assert_jq "GET /threads/{id} message.body is string" "$RESP" '.thread.messages[0].body | type == "string"'
assert_jq "GET /threads/{id} message.tags is array" "$RESP" '.thread.messages[0].tags | type == "array"'

# ─────────────────────────────────────────────
# 6. Tag thread (POST, write + verify)
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" -X POST -H "Content-Type: application/json" \
    -d '{"tags":"+starred"}' "$BASE/threads/$THREAD_ID/tags")
assert_jq "POST /threads/{id}/tags .ok is true" "$RESP" '.ok == true'

# Verify tag was applied
RESP=$(curl -sf "${AUTH[@]}" "$BASE/tags")
assert_jq "POST /tags verified: starred exists" "$RESP" '.tags | index("starred") != null'

# Error: invalid body
assert_http_code "POST /threads/{id}/tags invalid body → 400" \
    "$BASE/threads/$THREAD_ID/tags" "POST" "400" "not json"

# ─────────────────────────────────────────────
# 7. Message body (DurianResponse + MessageBody)
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/message/body?id=msg1@test")
assert_jq "GET /message/body .ok is true" "$RESP" '.ok == true'
assert_jq "GET /message/body .message_body.body is string" "$RESP" '.message_body.body | type == "string"'
assert_jq "GET /message/body .message_body.html is string" "$RESP" '.message_body.html | type == "string"'
assert_jq "GET /message/body body not empty" "$RESP" '.message_body.body | length > 0'

# Error: missing id
assert_http_code "GET /message/body without id → 400" "$BASE/message/body" "GET" "400"

# ─────────────────────────────────────────────
# 8. Contacts
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/contacts?limit=10")
assert_jq "GET /contacts is array" "$RESP" 'type == "array"'
assert_jq "GET /contacts[0].email is string" "$RESP" '.[0].email | type == "string"'
assert_jq "GET /contacts[0].name is string" "$RESP" '.[0].name | type == "string"'
assert_jq "GET /contacts[0].usage_count is number" "$RESP" '.[0].usage_count | type == "number"'
assert_jq "GET /contacts[0].source is string" "$RESP" '.[0].source | type == "string"'

# Search contacts
RESP=$(curl -sf "${AUTH[@]}" "$BASE/contacts/search?query=alice&limit=5")
assert_jq "GET /contacts/search returns array" "$RESP" 'type == "array"'
assert_jq "GET /contacts/search finds alice" "$RESP" '.[0].email | contains("alice")'

# Increment usage
RESP=$(curl -sf "${AUTH[@]}" -X POST -H "Content-Type: application/json" \
    -d '{"emails":["alice@example.com"]}' "$BASE/contacts/usage")
assert_jq "POST /contacts/usage returns ok" "$RESP" '. == {}'

# ─────────────────────────────────────────────
# 9. Local drafts (CRUD roundtrip)
# ─────────────────────────────────────────────
# Save a draft
RESP=$(curl -sf "${AUTH[@]}" -X PUT -H "Content-Type: application/json" \
    -d '{"draft_json":{"to":"test@example.com","subject":"Draft"}}' \
    "$BASE/local-drafts/test-draft-1")
assert_jq "PUT /local-drafts/{id} .ok is true" "$RESP" '.ok == true'

# Get the draft back
RESP=$(curl -sf "${AUTH[@]}" "$BASE/local-drafts/test-draft-1")
assert_jq "GET /local-drafts/{id} .id is string" "$RESP" '.id | type == "string"'
assert_jq "GET /local-drafts/{id} .draft_json exists" "$RESP" '.draft_json != null'

# List drafts
RESP=$(curl -sf "${AUTH[@]}" "$BASE/local-drafts")
assert_jq "GET /local-drafts is array" "$RESP" 'type == "array"'
assert_jq "GET /local-drafts has 1 entry" "$RESP" 'length == 1'
assert_jq "GET /local-drafts[0].id is string" "$RESP" '.[0].id | type == "string"'
assert_jq "GET /local-drafts[0].draft_json exists" "$RESP" '.[0].draft_json != null'

# Delete the draft
RESP=$(curl -sf "${AUTH[@]}" -X DELETE "$BASE/local-drafts/test-draft-1")
assert_jq "DELETE /local-drafts/{id} .ok is true" "$RESP" '.ok == true'

# Verify deleted
RESP=$(curl -sf "${AUTH[@]}" "$BASE/local-drafts")
assert_jq "GET /local-drafts empty after delete" "$RESP" 'length == 0'

# ─────────────────────────────────────────────
# 10. Outbox
# ─────────────────────────────────────────────
RESP=$(curl -sf "${AUTH[@]}" "$BASE/outbox")
assert_jq "GET /outbox is array" "$RESP" 'type == "array"'

# ─────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────
echo ""
echo "=== Results ==="
echo "$PASSED passed, $FAILURES failed"
if [ "$FAILURES" -eq 0 ]; then
    echo "All contract tests passed!"
    exit 0
else
    exit 1
fi
