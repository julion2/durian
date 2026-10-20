# Live replay provenance

`graph-mime-edges.eml.gz` is a gzip-compressed, redacted Graph `messages/$value`
response received on 2026-10-20 after sending one independently authored Gmail
message to the Outlook demo account via Durian's public outbox and real sender.
The receiving side used Durian's Graph backend and sync engine, not a mock.
Uncompressed SHA-256:
`0f631bbddf8de1d11b69729c3333d8e8f906df054b59555ed51ec168de36bd3b`.

The case takes ideas, not code or fixtures, from Thunderbird's
[test_bcc.js](https://github.com/mozilla/releases-comm-central/blob/master/mailnews/compose/test/unit/test_bcc.js)
and [test_attachment.js](https://github.com/mozilla/releases-comm-central/blob/master/mailnews/compose/test/unit/test_attachment.js):
blind-copy delivery, a literal `Bcc:` in body text, long quoted Unicode filename,
binary bytes after a long ASCII prefix, and an attached non-UTF-8 email. No MPL
code or Thunderbird fixtures were copied. This is not a Thunderbird test runner.

Before export, Python's independent email parser checked every decoded part
against the authored data. The nested email's received bytes differ only by
MIME header reserialization (charset quoting and header order); its decoded
Shift_JIS text is still `会議の資料です`.

Redactions: discard transport/authentication/anti-spam headers, provider message
identity and every `X-Attachment-Id`; replace outer From/To/Bcc, Message-ID and
Date with synthetic fixtures. Keep the known synthetic subject/body and MIME
boundary/encoding/disposition structure. Nested addresses were synthetic from
the start. No tokens, real addresses, third-party content or OAuth responses are
included. Compression avoids committing thousands of repetitive base64 lines.

The replay transports this MIME through the real backend, sync engine, encrypted
store and HTTP APIs, including reopening and resyncing. Folder/delta/flag and
userinfo envelopes are synthetic scaffolding, not captured provider exchanges.
Only the MIME response is an actual redacted recording; do not describe this as
a complete wire-session replay.

## Two-party iTIP REQUEST

`graph-invitation.eml.gz` is an actual received Graph `$value` response from one
Gmail → Outlook REQUEST sent through the same Durian outbox on 2026-10-20.
Uncompressed SHA-256:
`67c384a5b03b06f9980517a1a092b5b4e8633a397699cc260714bd1202c8ab10`.

The independently authored event starts at 09:00 Europe/Berlin on 2026-11-03,
includes VTIMEZONE, and requests a response from the other demo account. The
received MIME has a multipart/alternative with plain text and inline calendar,
plus the original text/ICS attachments. Live sync/HTTP readback checked exact
attachment bytes and the parsed invitation's UID, method, participants and time.
The replay additionally closes/reopens the encrypted store and syncs again.

The same transport-header redactions apply. The two real addresses inside both
calendar copies are replaced by `google@example.test` and
`microsoft@example.test`; the attached calendar is re-encoded in base64. Its
redacted decoded size is 841 bytes and SHA-256 is
`dcd184e0d0e756006ab9bdd7de452d06f04d2e3e0a020134828878a99e165b5a`.
The calendar content/structure is otherwise unchanged. This records invitation
delivery, not RSVP or provider-calendar creation.

## Monthly recurrence through the production planner

`google-monthly-sync.json.gz` and `graph-monthly-sync.json.gz` contain 11 and 29
sanitized actual exchanges respectively. Authored ICS with
`FREQ=MONTHLY;INTERVAL=1;COUNT=4;BYDAY=FR;BYSETPOS=-1` goes through Durian's planner,
create/read-back, persisted sync and provider-occurrence cache. Expected dates
were calculated independently: Oct 30, Nov 27, Dec 25, Jan 29 at 10:00 UTC.

The case adapts ideas from Thunderbird's
[negative-ordinal/BYSETPOS cases](https://github.com/mozilla/releases-comm-central/blob/d7588aa16db3b685f9ffb17c8f7b4e5062dde08b/calendar/test/unit/test_recur.js#L532-L555)
and [moved-occurrence cases](https://github.com/mozilla/releases-comm-central/blob/d7588aa16db3b685f9ffb17c8f7b4e5062dde08b/calendar/test/unit/test_recurrence_utils.js#L112-L161),
without copying code or fixtures. Unlike the upstream unit harness, the original
runs used the real Google/Graph servers. Unsupported authored moved occurrences
and Graph EXDATE uploads must preserve the local file and create no partial
series; normal series creation must still succeed. Initial live failures are
documented in the parent README rather than converted into supported behavior.

Calendar recordings use `calendartest.Recorder`'s field allowlist: no OAuth,
headers, actual calendar address or email participants; calendar identity is
`test-calendar`, cursors are non-reusable placeholders. Collections also contain
other intentional synthetic demo events. Safety preflight GETs made inside the
guard are not recorded; provider-facing requests/responses are. Each recording
ends with successful exact-ID cleanup of its one created master.
