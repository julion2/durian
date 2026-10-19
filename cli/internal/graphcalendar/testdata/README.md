# Real Microsoft Graph calendar contract

`graph-timezones.json` is the **unchanged output of calendartest.Recorder**
from a real, consented personal Microsoft demo account, not synthesized Graph
responses. The fixture is byte-for-byte identical to
`.amp/in/artifacts/graph-calendar-live/conditional-replan-20261019/recording.json`:

```
SHA-256 df3551bc96dea1767a377bda34c45ffc9b77e9ff3666359b79e1f309a755c66d
```

Recorder sanitizes JSON before saving: credentials, addresses, organizer and
attendee objects, body content, headers, and arbitrary fields are not retained.
Thus this is byte-identical **sanitized evidence**, not verbatim sensitive
HTTP bodies. Temporal fields and provider failures are not hand-edited.
This recording has no status-zero transport/guard failures. The prior recording
under `run-TkX2kjov` is preserved unchanged: it proves Graph accepted a stale
If-Match DELETE with 204 and the subsequent read-back was refused by the guard.
The latest recording passed email, credential-field, bearer/JWT and loaded-secret
audits; Recorder.Save also checked in-memory refreshed credentials before saving.

## Run

Offline (no OAuth, opt-in, or network required):

```
bazel test //cli/internal/graphcalendar:graphcalendar_test \
  --test_filter=TestGraphRecordedTimezones --test_arg=-test.v
```

Live, only with approved demo credentials already loaded into the orb:

```
bazel build //cli/internal/graphcalendar:graphcalendar_live_integration_test
```

Then run that built test binary with `-test.run '^TestGraphLiveTimezones$'
-test.v`, `DURIAN_CALENDAR_LIVE_TEST=1`, and `DURIAN_CALENDAR_RECORDING` pointing
at a new writable evidence filename. The test reserves it with O_EXCL before
bootstrap, uses a fresh 128-bit subject nonce, and pins the unique owned,
non-default calendar named exactly `Durian Test`. Do not reuse a prefix.
Microsoft refresh rotations are persisted by the existing token source via
stdin-only Amp secret updates. Never pass credentials as command arguments.

Live and replay use the same contract. Replay is the sole HTTP transport; an
unexpected request, exhausted recording, or mismatch returns an error. There
is **no network fallback**. Validation controls use a rejecting transport and
assert zero outbound calls, even if production validation were to regress.

## Harness additions at the actual outbound boundary

- POST adds `isReminderOn: false` and `isOnlineMeeting: false`; caller-supplied
  versions of these fields and all attendee fields are forbidden.
- Every request sends `Prefer: outlook.timezone="UTC", IdType="ImmutableId"`.
- Collection `$select` adds safety fields, series identity, and UTC endpoints.
- Global event reads/writes recheck safety and actual pinned-calendar
  membership; mutations preserve an explicit caller `If-Match` unchanged.
- Personal Graph rejected `$select=isSharedWithMe` with HTTP 400. The guard
  uses creator-only `canShare`, ownership identity, editability and non-default
  metadata instead; an explicitly true shared flag still rejects the calendar.
- This account's event/calendar navigation returned the primary calendar for
  events present in the pinned calendar. A mismatching relationship is NOT
  accepted: the guard instead requires the exact already-known immutable ID
  in the pinned `/events` collection, or a bounded pinned `calendarView` for
  occurrences. This does not enroll masters or authorize prefix-sweep cleanup.
- Recorder wraps the guard: auxiliary safety/membership GETs, injected flags,
  preferences and augmented selects are not separate recorded exchanges.
  Offline replay verifies the provider request seam, not fresh remote safety.

## Provider contract passes; application occurrence rendering remains open

Latest live and offline: **16/16 leaf cases pass**, **117/117 exchanges**.
The fixture contains 16 POST, 75 GET, 11 PATCH and 15 DELETE exchanges;
statuses: 15×201, 82×200, 4×412, 15×204, 1×400. The live run took 41.27s.
This is the Go provider + local/ICS contract, not an HTTP/GUI or full-sync proof.
Provider-specific DST characterization is deliberately separate from RFC5545
expectations; a passing characterization does not make those models equivalent.

| Cases | Count | Outcome |
| --- | ---: | --- |
| Gap-crossing single, travel/start+end zones, all-day DST | 3 | Pass: idempotent create, current-version edit, read-back, then stale PATCH rejection and unchanged read-back |
| Stale DELETE/read-back | 1 | Pass: preflight sees changed key, sends no DELETE; edited event remains readable |
| Berlin spring/autumn and New York spring weekly move/cancel | 3 | Pass: Graph, local expansion and ICS retain moved and cancelled dates; matching-key occurrence DELETEs succeed |
| Berlin 02:30 spring-gap weekly | 1 | Pass with separate expectations: Graph consumes COUNT on nonexistent March 29 and returns a zero-duration occurrence; local/ICS skip that date and extend COUNT |
| Berlin 02:30 autumn-fold weekly | 1 | Pass with separate expectations: start chooses first fold, but Graph occurrence lasts 105 minutes versus local/ICS 45 |
| Raw first fold / second fold / fold crossing | 3 | Characterization passes: independently expected UTC instants correct, original zones lost to UTC; production still rejects these writes |
| Raw nonexistent spring endpoint | 1 | Pass: Graph rejects with 400 |
| Production ambiguous and floating validation | 2 | Pass, before any I/O |
| Cleanup read-back | 1 | Pass: zero owned instances remain |

The prior 8/16 run is retained under `run-delete-preflight-20261019`; later
`provider-semantics-20261019` and `read-before-edit-20261019` each passed 15/16,
with setup PATCH rejected by an asynchronously advancing changeKey. No provider
responses were edited. The scenario now reads the current key after POST, and
on setup-only 412 permits at most two re-planned attempts, each after read-back
and comparison to the independently expected pre-edit content. A real content
change aborts; no unconditional PATCH or production retry was introduced. The
latest stale-delete setup used one such re-plan. The actual stale PATCH/DELETE
assertions never retry and must preserve the edited remote content.

Graph's gap/fold expectations now reflect the earlier genuine recordings, while
the original RFC/local expectations remain unchanged and are also asserted.
Raw fold probes use explicit-offset dateTime strings plus Europe/Berlin only
in test-only HTTP through guard+recorder. Production ambiguous-time rejection
remains unchanged because this representation loses zone identity. All events
have no attendees, conferencing or reminders. The HTTP/UI still expands local
masters: it does NOT yet substitute provider instances for DST divergences.

`DeleteEvent` now reads the same event with `$select=id,changeKey` and immutable
ID preference when etag is nonempty. Missing/foreign identity, missing key or
key mismatch fails closed with ErrPrecondition before DELETE; 404/410 remain
ErrNotFound. Matching keys permit DELETE, retaining caller If-Match. Empty etag
remains the explicit unconditional cleanup path. This adds one GET per conditional
delete, NOT atomic concurrency safety: a remote edit between GET and DELETE can
still be lost. [event-delete](https://learn.microsoft.com/en-us/graph/api/event-delete)
documents no conditional guarantee. No claim is made that formatting changeKey
differently fixes Graph DELETE.

Cleanup removes only POST-returned, guard-verified IDs (plus explicitly chosen
owned occurrence cancellations), never an event chosen by prefix/list sweep.
The latest fresh run created 12 distinct master/single IDs and cleaned all 12;
3 occurrence cancellations were also exercised. The guard enrolled 30 IDs
(including occurrences). The final read-only view is empty; zero unknown POST
transactions remain. Refresh rotation was persisted by the existing token source.

Earlier guard-interoperability failures are retained under the artifact root.
Twelve initially unverified POST outcomes were recovered with exact same-body,
same-transaction retries in the same logical run after fixing membership
verification, then all twelve returned IDs were cleaned. The temporary recovery
entry point was removed; the retained fixture is from a separate fresh nonce.
At that earlier evidence-capture stage no production provider/timezone/write code
was changed. The current change preserves the expanded-master exception/cancellation
fix and adds the DELETE preflight. Still open: provider-faithful HTTP/UI occurrence
rendering, Graph fold zone identity (writes remain rejected), and the non-atomic
DELETE race. Graph may legitimately return 412 without a user content edit; the
sync engine keeps such operations pending and re-plans rather than overwriting.
