# Google Calendar live contract and offline replay

These tests exercise the real Go provider, then the iCalendar round-trip and
local recurrence expansion. They do not launch the GUI or run a complete sync
engine round. The production CLI does not read the test credential variables.

## Current result: 14/14 cases pass (previously 8/14)

Captured on 2026-10-19 against a dedicated secondary **Durian Test** calendar.
The current fixture is copied byte-for-byte from the genuine sanitized live
recording `preflight-fixed-run-20261019.json` in
`.amp/in/artifacts/google-calendar-live/`. It contains **111 scoped exchanges**
for **11 created masters**. All eleven were deleted with HTTP 204; the final
expanded-events read returned no items. The calendar itself remains available.

The unchanged 14-case scenario now passes both live and offline, consuming
**111/111 exchanges** without network during replay. The previous recording,
`fold-fixed-run-20261019.json`, and its logs remain intact: **8/14 passed**,
with six real HTTP 400 failures on
`single-gap-crossing`, `single-travel-zones`, `all-day-over-DST`, and the three
Berlin/New York `EXDATE-move-cancel` series. Those nonfold PATCH requests
null-cleared private keys that the events never had, producing two `required`
reasons. The latest run has **zero HTTP 400 responses**: ordinary edits omit
absent keys, while clearing the existing fold pair still succeeds. All three
series now reach and pass their move, cancellation, provider read-back, and
local/iCalendar assertions. No scenario assertions or test bodies were changed
for this verification.

### Preflight GET and ETag tradeoff

Every full edit now reads the current Google event before PATCH to discover
which fold keys actually exist and whether the target is a recurring instance.
The cost is **one extra GET per full edit attempt**, including stale attempts,
plus its latency and quota usage; a failed read or missing remote ETag prevents
the write. The recording contains 16 such immediately preceding GETs for 16
PATCH attempts. A caller-supplied ETag is preserved, not refreshed: all six
deliberately stale single-event edits still receive HTTP 412. When the caller
omits an ETag, the production path binds PATCH to the preflight ETag so the
metadata decision cannot silently race a concurrent edit. Unit lifecycle tests
exercise omitted-ETag edits, but do not assert the resulting If-Match or inject
a concurrent edit. The unchanged live scenario always supplies ETags and does
not attempt a fold update on a remote `recurringEventId` with caller Type absent.
That rejection is present in production code but has no direct write-path test
in this verification. Missing-remote-ETag rejection is likewise not directly
tested here. Attendee-only edits do not gain this full-edit preflight.

Passing cases now include both single-fold branches, the new fold crossing
and metadata-clear case, the spring-gap and autumn-fold series, ambiguous
series rejection, floating-write rejection, and cleanup read-back. The crossing
starts at Berlin 02:30 +02:00 and ends at 02:00 +01:00: **30 real minutes**.
After a successful fold edit, it moves outside the fold with different endpoint
zones (New York/London), proving the old Berlin metadata is no longer restored.
The recording shows UTC wire endpoints with exactly
`extendedProperties.private.durian.fold.v1.startZone` and `endZone`, then paired
null deletion and a response/read-back without the private pair. Individual
writes check idempotent creation and successful edits check stale ETags.

Evidence from this run uses the `preflight-fixed-` filename prefix: build,
live, suites and audit logs, the sanitized JSON, and binary/source SHA256s.
The fixture/recording SHA256 is
`80d0fd1157ef737d32c1ff7ef8238ac3c097c30b9dfa82004f9cb6f388449673`.
The uncached full suites passed: Google **56 top-level tests / 74 subtests**,
shared calendartest **20 / 79**, and local calendar **61 / 13** (zero failures).
HTTP counts were GET 200: 64, POST 200: 11, expected idempotency POST 409: 6,
PATCH 200: 10, expected stale PATCH 412: 6, and DELETE 204: 14 (eleven masters
plus three canceled occurrences). All 47 writes used `sendUpdates=none`;
the boundary guard excluded attendees and conference data.

### Explicit provider versus RFC spring-gap semantics

For weekly Berlin 02:30 starting March 22 with COUNT=3, two independently
specified expectations are deliberately asserted:

- **Google:** March 22 01:30Z, March 29 **01:30Z**, April 5 00:30Z. Google
  normalizes nonexistent March 29 02:30 to **03:30 Berlin** and consumes COUNT.
- **Local expansion and iCalendar round-trip:** March 22 01:30Z, April 5
  00:30Z, April 12 00:30Z. RFC 5545 skips the nonexistent wall time without
  consuming COUNT. This expectation and the local engine are unchanged.

This records a genuine interoperability discrepancy; a green spring-gap case
does not mean Google and the RFC engine enumerate the same occurrences.

### Retained baseline evidence

The prior fixture remains intact as `final-run.json` (82 exchanges, ten
masters), alongside `first-run.json`, `second-run.json`, their logs and the
original replay/safety/suite logs in the artifact directory. The old strict
scenario passed 9/12 cases, failing first-fold crossing (HTTP 400
`timeRangeEmpty`), second-fold offset/duration preservation, and the then-shared
spring-gap expectation. The UTC/private-metadata fix resolves the two fold
failures; the gap expectation is now explicitly provider-specific, not hidden.

The initial scenario incorrectly reused a sibling instance's ETag after
moving another occurrence; Google returned 412. Reading that instance again
before cancellation resolves those three failures without dropping If-Match.
This is a test-sequencing correction, not a production sync fix.

## Offline replay (no credentials or network)

```sh
bazel test //cli/internal/googlecalendar:googlecalendar_test \
  //cli/internal/calendartest:calendartest_test \
  //cli/internal/calendar:calendar_test \
  --nocache_test_results --test_output=errors
```

All three targets pass with the current fixture. The replay runs the same
public-method scenario as the live test, including writes and cleanup; it
compares requests and consumes every recorded response. It has no network
fallback. This verifies the scoped provider contract, not the GUI or complete
sync engine, and does not erase the Google/RFC spring-gap discrepancy above.

## Live run (requires explicit authorization to write)

Credentials must already be injected into the process environment:
`DURIAN_GOOGLE_TEST_CLIENT_ID`, `DURIAN_GOOGLE_TEST_CLIENT_SECRET`, and
`DURIAN_GOOGLE_TEST_REFRESH_TOKEN`. Never paste their values into commands,
logs, Bazel `--test_env=NAME=value`, or repository files.

```sh
bazel test //cli/internal/calendartest:calendartest_test --test_output=errors
bazel build //cli/internal/googlecalendar:googlecalendar_live_integration_test
binary="$(bazel cquery //cli/internal/googlecalendar:googlecalendar_live_integration_test --output=files | grep '/googlecalendar_live_integration_test$')"
mkdir -p .amp/in/artifacts/google-calendar-live
DURIAN_CALENDAR_LIVE_TEST=1 \
DURIAN_CALENDAR_RECORDING="$PWD/.amp/in/artifacts/google-calendar-live/new-run.json" \
  "$binary" \
  -test.run='^TestGoogleLiveTimezones$' -test.v -test.timeout=10m
```

Use a new recording filename for every run. Build the test binary without
credentials in Bazel flags, then run it with inherited environment variables.
Without the build tag/manual target and explicit runtime opt-in, no live test
runs. Only the offline replay participates in the ordinary Google test suite.

The test-only adapter refreshes OAuth entirely in memory. It never reads or
writes keychain/config.pkl. A dedicated calendar with an ownership description
is reused; an ambiguous, primary, non-owned, or unmarked same-name calendar
aborts the run. Calendar creation is never automatically retried after an
unknown answer. All subsequent requests hard-check the pinned calendar ID,
HTTPS host, allowed operations and fields. Writes require `sendUpdates=none`,
exclude attendees/conference data, and use deterministic IDs derived from
the unique run-prefixed title. Cleanup addresses only those IDs/instances,
never every item returned by a listing. A process killed before cleanup can
leave its prefixed events behind; later runs deliberately do not delete them.
The only allowed extended properties are the exact two private fold zone keys:
both valid zones, or both null on PATCH. Shared/arbitrary/missing/mixed pairs
are rejected before adding credentials. The recorder retains only these keys.

The recorder retains allowlisted temporal/event JSON, HTTP method/status,
normalized paths and If-Match versions. It discards identity objects, auth
headers, OAuth exchanges, provider free-text errors and unknown fields;
addresses/iCalendar UIDs and cursors are pseudonymized. Saving also rejects
the current credential values if they somehow appear in retained data.
ETags and generated event IDs remain for exact replay and are not credentials.

The shared recorder also retains allowlisted Graph event times, zone fields,
series/instance IDs, versions, date-only flags and recurrence pattern/range
fields, plus location display text (not location email addresses). Organizer,
attendee, token and unknown fields remain excluded. Trusted Graph nextLinks
retain only supported event-list/instance paths and allowlisted query fields;
the pinned calendar ID is normalized and opaque `$skiptoken` cursors use the
same stable placeholder in responses and follow-up requests. Untrusted hosts,
mailbox identity paths, other calendars and arbitrary query parameters are
dropped. This is replay infrastructure, **not** authorization: Graph still
requires its separate calendar/event-membership guard. Graph body HTML is
deliberately not recorded. No Microsoft live behavior is verified here.
