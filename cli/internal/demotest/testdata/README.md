# Graph MIME replay provenance

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
