#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${1:-quick}"

usage() {
    cat <<'EOF'
Usage: tools/test_quality.sh [quick|stress]

  quick   Run the CLI, sync server, and API contract Bazel gates.
  stress  Run quick, then the CLI race suite, repeated TLS IMAP/outbox
          regressions, and bounded calendar, sync-engine, and MIME fuzzing.

The runner is limited to four CPUs, two concurrent Bazel tests, and 6 GiB of
Bazel local RAM. It keeps running independent gates after a failure and exits
nonzero if any gate fails or times out.
EOF
}

if (($# > 1)); then
    usage >&2
    exit 2
fi

case "$MODE" in
    -h | --help)
        usage
        exit 0
        ;;
    quick | stress) ;;
    *)
        echo "Unknown mode: $MODE" >&2
        usage >&2
        exit 2
        ;;
esac

for command in bazel go timeout; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "Required command not found: $command" >&2
        exit 2
    fi
done

if [[ ! -d "$ROOT/cli/internal/config/schema" ]]; then
    echo "Missing staged Pkl schemas. Run .agents/setup before this script." >&2
    exit 2
fi

readonly BAZEL_JOBS=4
readonly BAZEL_TEST_JOBS=2
readonly BAZEL_RAM_MB=6144
readonly REPEAT_COUNT=10
readonly FUZZ_TIME=15s

BAZEL_LIMITS=(
    "--jobs=$BAZEL_JOBS"
    "--local_resources=cpu=$BAZEL_JOBS"
    "--local_resources=memory=$BAZEL_RAM_MB"
    "--local_test_jobs=$BAZEL_TEST_JOBS"
)
BAZEL_TEST_FLAGS=(
    --test_output=errors
    --nocache_test_results
    --test_env=DURIAN_JMAP_TEST_SESSION_URL=
    --test_env=DURIAN_JMAP_TEST_USERNAME=
    --test_env=DURIAN_JMAP_TEST_PASSWORD=
)
failures=()

run_gate() {
    local label="$1"
    local limit="$2"
    shift 2

    printf '\n== %s (timeout %s) ==\n' "$label" "$limit"
    timeout --foreground "$limit" "$@"
    local status=$?
    if ((status != 0)); then
        failures+=("$label (exit $status)")
        echo "FAILED: $label (exit $status)" >&2
    fi
}

run_go_gate() {
    local label="$1"
    local limit="$2"
    shift 2

    run_gate "$label" "$limit" bash -c \
        'cd "$1/cli" && shift && unset DURIAN_JMAP_TEST_SESSION_URL DURIAN_JMAP_TEST_USERNAME DURIAN_JMAP_TEST_PASSWORD && GOMAXPROCS=4 exec go "$@"' \
        _ "$ROOT" "$@"
}

run_quick() {
    run_gate "CLI Bazel tests" 20m \
        bazel test "${BAZEL_LIMITS[@]}" "${BAZEL_TEST_FLAGS[@]}" --keep_going //cli/...
    run_gate "Sync server Bazel tests" 10m \
        bazel test "${BAZEL_LIMITS[@]}" "${BAZEL_TEST_FLAGS[@]}" --keep_going //sync:sync_test
    run_gate "API contract Bazel test" 10m \
        bazel test "${BAZEL_LIMITS[@]}" "${BAZEL_TEST_FLAGS[@]}" //integration:integration_test
    run_gate "CLI Bazel build" 10m \
        bazel build "${BAZEL_LIMITS[@]}" //cli/cmd/durian
    run_gate "Sync server Bazel build" 10m \
        bazel build "${BAZEL_LIMITS[@]}" //sync:durian-sync
}

run_stress() {
    run_go_gate "Go race tests (all CLI packages)" 25m \
        test -race -count=1 -p=2 -parallel=4 -timeout=20m ./...

    run_go_gate "Repeated real-TLS IMAP protocol tests" 10m \
        test -p=2 -parallel=4 -timeout=8m -count="$REPEAT_COUNT" \
        -run '^TestProtocol' ./internal/imapbackend

    run_go_gate "Repeated outbox concurrency regressions" 10m \
        test -p=2 -parallel=4 -timeout=8m -count="$REPEAT_COUNT" \
        -run '^(TestAdversarial|TestOutboxLifecycle)' ./internal/store ./internal/handler

    run_go_gate "Fuzz ICS exception round trip" 3m \
        test -parallel=4 -timeout=2m -run '^$' \
        -fuzz '^FuzzAdversarialICSRoundTripExceptionInstants$' -fuzztime="$FUZZ_TIME" \
        ./internal/calendar

    run_go_gate "Fuzz sync-engine flag sequences" 3m \
        test -parallel=4 -timeout=2m -run '^$' \
        -fuzz '^FuzzAdversarialFlagSequenceProperties$' -fuzztime="$FUZZ_TIME" \
        ./internal/syncengine

    run_go_gate "Fuzz nested MIME round trip" 3m \
        test -parallel=4 -timeout=2m -run '^$' \
        -fuzz '^FuzzAdversarialNestedMIMERoundTrip$' -fuzztime="$FUZZ_TIME" \
        ./internal/mail
}

cd "$ROOT" || exit 1
run_quick
if [[ "$MODE" == stress ]]; then
    run_stress
fi

printf '\n== Quality summary ==\n'
if ((${#failures[@]} == 0)); then
    echo "All $MODE gates passed."
    exit 0
fi

printf 'Failed gates (%d):\n' "${#failures[@]}" >&2
printf '  - %s\n' "${failures[@]}" >&2
exit 1
