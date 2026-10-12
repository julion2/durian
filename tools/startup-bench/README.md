# Native startup benchmark

Benchmark-only tooling: a separate encrypted synthetic seeder, native X11 pixel/OCR measurements, and supplemental read-only HTTP diagnostics. The normal integration fixture is unchanged. No providers are configured, automatic sync/notifications/remote images are disabled, avatars are disabled, and every address is `.invalid`.

## Optimized integrated build, before and after

A separate 40-run matrix uses **Bazel `-c opt`**, the integrated client and the same deterministic 10k/100k fixture generator. This orb has **8 logical CPUs / 16 GB RAM**, unlike the historical baseline below. The runtime still uses Xvfb and Mesa llvmpipe, not a hardware GPU or macOS. Do not attribute differences from the old fastbuild matrix solely to optimization flags.

The comparison changes only two production paths: defer the initial sidebar count fanout until the first list response (success or failure), and verify encrypted search candidates in batches of 512 with an ASCII-only casing fast path. Unicode still uses the existing contextual caser; every candidate still passes the full AST/collision checks. No indexes, schema, search semantics, transport deadlines or dependencies change. Count freshness/retries remain intact; badges can arrive after useful mail. That revision still had duplicate startup list requests; the later comparison below isolates their fix.

Five warm and five targeted-file-cold native launches per corpus **on each revision**; p50 / nearest-rank p95 in milliseconds:

| Corpus / condition | Useful list before | Useful list after | Expanded plaintext before | Expanded plaintext after |
| --- | ---: | ---: | ---: | ---: |
| 10k warm | 398 / 509 | 399 / 467 | 582 / 608 | 510 / 575 |
| 10k targeted cold | 919 / 1344 | 905 / 978 | 1047 / 1451 | 1019 / 1087 |
| 100k warm | 1525 / 1546 | 1080 / 1113 | 1633 / 1654 | 1081 / 1113 |
| 100k targeted cold | 7961 / 10307 | 5248 / 5607 | 8130 / 10399 | 5271 / 5591 |

The 100k useful-list median falls about 29% warm / 34% targeted-cold. The 10k useful-list median is effectively unchanged. All five after-change 10k warm lists arrive within 500 ms, but **no expanded-body run and no 100k list run meets 500 ms**. First frame remains a visibly empty/loading shell. All 40 runs independently passed their mincore residency expectations and owned-CLI cleanup checks; final captures were inspected against their actual thread/body text.

Separate store profiling (`benchseeder --profile`) measures real encrypted Search/SearchCount, not GUI startup. The before CPU profile spends about 70% in Unicode casing. For the broad ASCII-heavy `BenchBody` query over 100k messages, three-run median Search falls **17.870 → 6.848 s**, count **17.613 → 6.308 s**. Process peak RSS over the complete profile run falls **2,196,460 → 47,144 KiB**; allocated bytes per broad query fall about **11.78 → 7.76 GB**. First broad searches include colder data and remain slower (29.838 → 17.023 s), so these are not general cold-query guarantees. Non-ASCII text keeps the original caser and does not get the ASCII speedup. Inbox store timings remain effectively unchanged. All 12 before/after observations return identical counts and ordered IDs, independently checked against SQL aggregation: 16,106 inbox threads, 27,799 broad matches, and 200 exact page IDs.

Evidence is in `.amp/in/artifacts/startup-optimized/`: per-run raw bounds/residency/cleanup, binary hashes, traces, inspected captures, profiles and resource logs. The historical matrix below remains untouched.

```sh
bazel build -c opt //gpui:durian //cli/cmd/benchseeder
# Use the same synthetic-only environment and native run.py command below,
# with separate before/after packaged binaries and fresh output directories.
bazel-bin/cli/cmd/benchseeder/benchseeder_/benchseeder --profile \
  /tmp/startup-corpora/100k /tmp/profile-before
# This requires the same public fixture key as seeding; opens the corpus read-only.
# go tool pprof -top <that-exact-benchseeder-binary> /tmp/profile-before/cpu.pprof
```

## Startup request reuse: later integrated Linux build

The next comparison isolates only `Live` startup request ownership. It starts from the
integrated optimized build with the Linux HTML helper, not the earlier 40-run revision.
The Go CLI and HTML helper hashes are identical before/after. Environment remains 8 logical
CPUs / 16 GB, Xvfb/Openbox/llvmpipe, `-c opt`, the same original 100k encrypted corpus, and
five warm plus five targeted-file-cold launches on each side (20 total). No providers or
benchmark-time concurrent builds/tests. Shared-library/host caches are not evicted.

| 100k condition | Useful list before | Useful list after | Expanded plaintext before | Expanded plaintext after |
| --- | ---: | ---: | ---: | ---: |
| Warm | 1285 / 1466 | 859 / 1040 | 1362 / 1466 | 980 / 1192 |
| Targeted cold | 5767 / 12762 | 5287 / 6366 | 5907 / 12873 | 5389 / 6461 |

Values are p50 / nearest-rank p95 milliseconds (with five samples, p95 is just the maximum).
The visible-list median improves about 33% warm and 8% targeted-cold. The large before-cold
maximum is retained, not discarded or presented as a reliable population-tail reduction.
No list/body sample meets 500 ms. Selected body is actual expanded plaintext in these captures,
not an HTML-completion measure, even though both packages now include the HTML engine.

All 20 runs passed independent checks for the exact selected thread, 200-row list trace,
targeted mincore residency (all pages warm / zero pages cold), and owned CLI kill/reap.
Actual before/after screenshots were inspected for populated rows and expanded message text.
Evidence: `.amp/in/artifacts/startup-dedup/{before,after}-100k/`, `audit.json` and
`binary-hashes.txt`. The older matrices remain unchanged.

The separate `//integration:gpui_startup_ui_test` regression uses a real encrypted Go fixture
behind a loopback response barrier and real native keyboard/OCR. Already-read fixture mail
avoids legitimate mark-read refreshes. On the old binary one additional scoped search completes
while its identical startup request is held; the fixed binary sends none until that first
response is released, then shows the real inbox. Controls exercise a different query without
waiting, returning to Inbox while both answers are pending, manual refresh superseding stale
data after an actual tag removal, failed-search retry, and empty-inbox count release.
SSE/manual/write-driven refreshes remain authoritative; this is not a promise of exactly one
search during every startup regardless of changes.

Two initial harness preflights used an incorrect READY-port assumption (`serve --port 0`
does not report its allocated port); they are excluded from the regression evidence. A
header-held preflight also surfaced a transport `Interrupted system call` error, not a duplicate
request. The final barrier holds response bodies and passes the same old/new binaries through
the same procedure. That comparison did not include the later transport fix below.

## Timed socket interruptions: later transport fix

The pinned ureq 3.4.2 propagates Linux EINTR from timed reads even with SA_RESTART. A private
transport adapter now resumes the same established response read with its remaining deadline;
it never retransmits a write or replays a request. Unbounded reads remain interruptible. This
does not add retries to TLS/proxy connection setup and does not change the 20 s HTTP deadline.

The real-socket regression interrupts the requesting thread during headers, partial headers,
partial JSON bodies and a POST acknowledgment. Before the fix it fails with EINTR in the first
case; afterwards all cases pass, including sustained signals that must still time out at 20 s.
The native startup contract now also holds headers: five separate fresh encrypted-fixture
launches pass, with one search response and a populated native inbox. The complete startup
matrix, real Go contract and all four GPUI test targets pass (six targets total).

Evidence: `.amp/in/artifacts/transport-eintr/`. A discarded repeat attempt reused the same
case name within one fixture root and read stale READY metadata; the five accepted repeats use
separate script invocations/fresh roots. The historical startup timing matrices are unchanged.

## Bounded recent-date probe: later Go-only comparison

The next change is confined to Go Search. Broad SQL-only queries with a required driving tag
first try the existing date index. The candidate window includes only messages **strictly newer
than the 4096th newest date**, excluding the whole boundary tie. It is accepted only when it
contains the requested number of distinct threads; otherwise Search runs its original full
plan. Full matching authors still include older messages; latest global subject/recipients,
matching dates, account-scoped tags and deterministic thread tie ordering are preserved.
The boundary, page and authors share one SQL read snapshot. Small tags, selective indexed
queries, large pages and post-decrypt verification keep their original plans. A sparse broad
folder can pay for the bounded probe before falling back; this is not an optimization of every
query. No new index, schema, persisted cache, dependency or count-query change.

Moving only sender aggregation after LIMIT barely changed cold I/O. A newest-message anti-join
was fast on this corpus but can do repeated work on long conversations; the bounded candidate
window avoids that design. Both experiments are retained as diagnostics, not native timings.

Same original encrypted corpora, optimized packages, 8 CPUs / 16 GB, Xvfb/Openbox/llvmpipe.
Only the CLI binary differs: GUI and HTML helper SHA256s match. No concurrent builds/tests
during timing. Five warm and five targeted-file-cold launches per corpus/side (40 total):

| Corpus / condition | Useful list before | Useful list after | Selected body before | Selected body after |
| --- | ---: | ---: | ---: | ---: |
| 10k warm | 488 / 522 | 497 / 531 | 907 / 968 | 1323 / 1343 |
| 10k targeted cold | 983 / 1049 | 997 / 1111 | 1471 / 1891 | 1463 / 1532 |
| 100k warm | 843 / 957 | 507 / 528 | 957 / 1004 | 631 / 657 |
| 100k targeted cold | 5431 / 5466 | 1017 / 1071 | 5452 / 5585 | 1128 / 1180 |

Numbers are p50 / nearest-rank p95 milliseconds, not population-tail estimates. The 100k cold
list median improves about 81%; its warm median about 40%. The 10k list is effectively unchanged.
Unlike the historical no-helper baseline, **10k selected-body glyphs are in rendered HTML**;
100k's selected message is expanded plaintext. These are neither all-message HTML completion
nor attachment-ready measures. No selected-body sample meets 500 ms; only two of the five
after-change 100k warm lists do. Cold first frame alone still takes about 0.8–0.9 s.

The initial 10k warm body result is worse and is retained, not discarded. A separate interleaved
five+five warm repetition reverses the body medians (1309 → 1006 ms; list 511 → 534 ms). Both
binaries show variable gaps between full-thread arrival and HTML completion. No consistent
10k body benefit or regression is established; do not attribute this variability to SQL alone.
All 50 launches pass targeted residency and owned-CLI cleanup checks. The main 40-run matrix
also passes independent selected-thread/200-row/OCR/sampling/quantile audits and blank-image
rejection for both light HTML and dark plaintext anchors. Inspected images show actual mail.

Ten public CLI comparisons on 100k return identical complete JSON fields before/after, with
IDs/dates/order independently checked against full SQL aggregation: unscoped and scoped inbox,
multi-account scope, negation, old dates, one thread, empty results, 2500/5000-row pages and broad
verified full text. Store regressions cover older cross-account authors, a newer nonmatching
reply, a page-boundary tie, insufficient candidate windows and a tie larger than the budget.

Evidence: `.amp/in/artifacts/cold-search/`, including both matrices, the interleaved control,
binary/corpus hashes, query experiments, CLI comparison script/results, screenshots and audit.
The first full 42-target run passed 41 targets; calendar UI timed out finding its native window
before assertions, despite a running app in its log. An unchanged isolated repeat passed, then
the identical uncached full-suite command passed **42/42 targets** (all CLI, four GPUI targets
and five integration contracts; Rust: 129 passed / 5 ignored). All logs are retained; the
initial window-discovery failure is not a claimed fixed application bug.

## Measured baseline

Exact parent's unpushed `gpui-next-base.tar.gz`, plus its authorized shared-first-name sender fix (`thread_list.rs` SHA256 `cff898ed2191639e14377a0f88381a566ebf878a55007010ef028d071c72f3f1`). **All startup timings precede the later four-file CLI search correctness fix.**

- Linux x86_64 E2B orb, kernel 6.1.158+, 4 logical CPUs, Intel Xeon 2.60GHz, 8,147,608 KiB RAM, no swap.
- Bazel 8.2.1 default **fastbuild**, not an optimized release. Rust 1.99.0; Go SDK 1.26.9; Pkl 0.31.1.
- Xvfb 1600×1000×24, Openbox, 1420×816 native client pixels, scale 1. Mesa llvmpipe LLVM 15.0.6 OpenGL CPU renderer, `VK_ICD_FILENAMES=/dev/null`.
- GUI SHA256 `27d69190762cc580ee67dff44d75635d63278f77e7554bef697e152a828aca75`; original packaged CLI SHA256 `b31bcce26f6c8c59de6c151d400cfe221237d871f62f0804b27a568e6110d560`.
- No real mailbox, SMTP, IMAP, provider requests, Mac runner, production optimization, commit, or push.

**The 500ms useful-mail goal was not met in any of the 20 measured runs.** These results do not establish optimized-release, hardware-GPU, macOS, or machine-wide-cold performance.

## Corpus

| Property | 10k | 100k |
| --- | ---: | ---: |
| Messages | 10,000 | 100,000 |
| Distinct threads | 2,731 | 27,799 |
| Inbox messages / threads | 5,821 / 1,597 | 58,020 / 16,106 |
| Alpha / Beta / Gamma messages | 6,289 / 2,596 / 1,115 | 61,510 / 27,035 / 11,455 |
| Messages with HTML | 4,114 | 41,101 |
| Attachment metadata records | 4,606 | 45,537 |
| Plaintext bytes | 72,805,542 | 722,399,927 |
| HTML bytes | 31,076,944 | 309,651,579 |
| Encrypted email.db bytes | 162,226,176 | 1,591,296,000 |

Content is deterministic, ciphertext is not: production AES-256-GCM uses fresh random nonces. Subjects, bodies, attachment filenames/types/sizes and account/mailbox metadata use the production Keyring; normal production routing headers remain plaintext. No ciphertext rows are cloned.

Thread lengths 1/2/4/8/16 follow asymmetric approximately 32/29/21/13/5% thread distributions (10k ends with a partial 7-message thread). Distinct senders share the first name `Person`; long conversations retain >3 participants. Unique subjects/body markers, three accounts, inbox/sent/archive, unread/flagged, 13 project tags and finance vary deterministically. Bodies mix 512–2048, 2048–8192, 8192–32768 and 32768–131072 bytes at approximately 55/30/12/3%. About 41% also include HTML; about 23% have 1–3 PDF metadata records with varied sizes. There are 240 encrypted synthetic contacts; attachment blobs are intentionally not generated.

The seeder initializes via real store/contact APIs, then generates fresh encrypted rows in transactional batches using the real schema, Keyring and blind-FTS tokenizer. This avoids the public insertion path's quadratic reverse-reference lookup. It validates independent SQL row/thread/index/envelope counts, public inbox searches, broad full-text counts (including failures), a unique marker query, and selected-body decryption. `--verify` checks an existing corpus without regenerating it; it writes only stats.json. Recovered seed duration is recorded as 0 (unknown), not invented.

## Timing definitions

- Clock starts immediately before fresh GUI `Popen`, including exec/loading. The GUI launches its **owned sibling CLI**, not an external server. Normal Ctrl+Q ends every run; all 20 owned CLIs were confirmed reaped.
- First frame: first nonblack dark-theme native client background at a fixed empty 8×8 sidebar patch; a full first-frame image is also captured. This can be a loading shell, not useful mail.
- Useful list: visible OCR-calibrated `BenchThread` glyphs in an actual initial-page result row, never the reading-pane header or toolbar. 10k anchor is row 1 at (306,145); 100k's first reliably OCR-readable anchor is row 3 at (306,255). This measures an initial result page, not a guaranteed first-row timestamp.
- Selected body: visible larger-font `BenchBody` glyphs in **expanded plaintext**, not a collapsed header/snippet. Selection ID is checked independently in the trace. Linux has no compatible packaged HTML helper here; HTML mail falls back to plaintext. This is **not HTML-complete, attachment-ready, or full-viewport body hydration**.
- Captures poll every 15ms, with independent previous-negative / first-positive interval bounds. Final observed intervals were about 16–21ms. Glyph matching rejects empty/loading pixels; each completed run also executes full-window OCR. One full first-frame capture adds observer work; PNG encoding/OCR run after milestone collection.
- Warm filesystem: sequentially read GUI/CLI/database/config/WAL files into cache; mincore confirms every targeted page resident. Keep private app/config cache. Fresh app+CLI still reset in-process caches.
- Targeted-file cold: clear only that benchmark runtime's XDG_CACHE_HOME; fsync then `POSIX_FADV_DONTNEED` GUI/CLI/database/config/WAL files. mincore confirms **zero targeted resident pages before all ten cold launches**. Shared libraries, Pkl executable, filesystem metadata, kernel, WM, driver and host/storage caches remain warm. This combines app-cache clearing and targeted eviction; it is **not machine-wide cold** and uses no global cache drop.
- Five runs per corpus/condition. p50 is median; p95 uses nearest-rank, thus maximum with n=5. It is not a reliable population-tail estimate.

## Results: p50 / p95 milliseconds

| Corpus / condition | First frame | Useful list | Selected plaintext |
| --- | ---: | ---: | ---: |
| 10k warm | 523 / 543 | 964 / 987 | 1472 / 1517 |
| 10k targeted cold | 1285 / 1331 | 1885 / 1998 | 2560 / 2614 |
| 100k warm | 555 / 705 | 1248 / 1686 | 1743 / 2389 |
| 100k targeted cold | 1319 / 2006 | 10470 / 15808 | 11114 / 16869 |

Raw observations in **run order**, rounded here to milliseconds; full decimals and sampling bounds remain in each report.json/result.json:

| Corpus / condition | Frames | Lists | Plaintext bodies |
| --- | --- | --- | --- |
| 10k warm | 543,523,511,526,498 | 987,972,947,964,943 | 1517,1485,1472,1289,1455 |
| 10k targeted cold | 1331,1285,1324,1227,1212 | 1998,1885,1839,1709,1921 | 2560,2571,2387,2265,2614 |
| 100k warm | 558,705,555,530,536 | 1686,1416,1248,1223,1241 | 2389,2170,1685,1743,1737 |
| 100k targeted cold | 1317,2006,1210,1386,1319 | 15808,14381,10470,7191,9053 | 16869,15068,11114,7704,9818 |

## Observed bottlenecks and correctness findings

1. **100k targeted-cold first searches dominate.** Trace examples: first frame 1.317s; search reports 200 rows at 15.320s with a 14s request; visible list 15.808s; plaintext 16.869s. Across cold runs initial data waits are several seconds, despite the early loading shell. Search aggregates/group-orders matching messages before taking the page; this is a source-supported investigation target, not a CPU/I/O profile or an optimization claim.
2. **Main-thread/render availability is distinct from API response.** A separate native warm diagnostic got 10k profiles in 0.8/1.2ms and the initial search response in 286ms, while native result/body pixels arrived later. 100k diagnostic sent a second identical startup search before the first finished (794ms and 1324ms). Do not label UI trace “profiles in 500ms” as 500ms of Pkl evaluation: it records when the UI applies the report. Software rendering/scheduling and duplicate initial work are observed investigation targets. API latency alone cannot establish GUI startup.
3. **Sidebar count fanout encounters rate limiting.** Timed 10k warm runs all report 36 counts / 5 failures; targeted-cold reports 1,1,1,1,0 failures. All ten 100k timed runs report 36 / 0 failures, with final counts sometimes later than the body. Separate native packet diagnostics have zero dropped packets and no unanswered counts. 10k returns HTTP 429 `rate limited` for these exact queries:
   - `(tag:project-09) AND tag:unread`
   - `(tag:sent) AND tag:unread`
   - `(tag:trash) AND tag:unread`
   - `(tag:spam) AND tag:unread`
   - `(tag:unread) AND tag:unread`
   The server shares a burst-30, refill-10/s limiter between search and count. 100k diagnostic returns all 36 counts successfully. Exact codes/queries are from separately labeled diagnostic boots, not retrospectively assigned to each uncaptured timed run. Raw packets/auth headers/tokens are never written.
4. **Original sender-name crash preserved.** Original 10k boot panicked at thread_list.rs:464 (`recent[1]`, length 1) with many distinct `Person NNN` senders. Full-name versus shortened-label dedup was the cause; an earlier lazy-stub inference was incorrect. Parent supplied the sole GUI production fix, verified by hash; original corpus participant variety was retained. Its native path then rendered successfully.
5. **Original broad-search failure and later fix are separate.** With unchanged 100k data, original CLI `count BenchBody` and `search BenchBody --limit 7 --json` both exit 1: `post-decrypt fetch: query: SQL logic error: too many SQL variables (1)`. HTTP count returns 500 `internal error`; bounded search returns HTTP 200 with an error envelope and null results. After importing exactly the parent's four search-fix files and rebuilding CLI/seeder, CLI count returns **27,799**, and search returns **exactly 7 independently verified newest threads in order**. HTTP count is 200 / 27,799 (18.907s); HTTP bounded page is 200 / 7 (19.720s). Unique `BenchBody 099999` remains 200 / count 1. CLI observations were 33.105s count and 24.395s page; these are expensive full-candidate checks, not faster-startup evidence. The API did succeed with the unchanged 20s diagnostic deadline, close to Live's 20s boundary; timeout risk remains, but a GUI timeout was not demonstrated. The entire email.db SHA256 before/after is unchanged: `50223e8b7ac8f6f73cc7c46baed04b35253bf63b951137aefada898fc231542f`.

Invalid preflights (wrong OCR region, wrong first-frame patch, original crash) were discarded, not mixed into the 20-run matrix. Single-run 561/1179/1789ms warm and 1418/2171/2683ms cold were preliminary only. A seeder stats-pointer alias was corrected after validation: the broad-count value had been overwritten by the unique-marker count. The actual store assertion already checked the correct thread total; original CLI independently confirms 10k broad count 2731. Metadata correction does not alter fixtures, captures, timestamps or distributions.

## Reproduction

Requires existing Bazel native build dependencies, Pkl, Xvfb/Openbox, xdotool, tesseract, `/usr/bin/python3` and python3-pil. Start Xvfb/Openbox using the orb's supervised services (or an existing display); do not use real provider configuration.

```sh
bazel build //gpui:durian //cli/cmd/benchseeder
# Public fixture key, NOT a real mailbox key.
export DURIAN_MASTER_KEY_HEX_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
SEEDER=bazel-bin/cli/cmd/benchseeder/benchseeder_/benchseeder
mkdir -p /tmp/startup-corpora
"$SEEDER" /tmp/startup-corpora/10k 10000
"$SEEDER" /tmp/startup-corpora/100k 100000
"$SEEDER" --verify /tmp/startup-corpora/100k 100000
for corpus in 10k 100k; do
  DISPLAY=:99 /usr/bin/python3 tools/startup-bench/run.py \
    --app bazel-bin/gpui/durian --corpus /tmp/startup-corpora/$corpus \
    --work /tmp/startup-runtime/$corpus \
    --output .amp/in/artifacts/startup-benchmark/$corpus --runs 5
done
```

Do not reuse an output evidence directory. Seeder refuses to overwrite a corpus. Exact original failure reproduction needs the original CLI/store, not the later correctness fix. Direct CLI reproduction requires the synthetic runtime environment; use `environment()` from run.py (public fixture key, XDG paths, data/durian symlink, no configured providers). Then run:

```sh
PYTHONPATH=tools/startup-bench /usr/bin/python3 - <<'PY'
from pathlib import Path
import subprocess
from run import environment
env = environment(Path('/tmp/startup-runtime/cli'), Path('/tmp/startup-corpora/100k'))
cli = str(Path('bazel-bin/gpui/durian-cli').resolve())
for args in [('count', 'BenchBody'), ('search', 'BenchBody', '--limit', '7', '--json')]:
    result = subprocess.run([cli, *args], env=env)
    print('command:', args, 'exit:', result.returncode)
PY
# Equivalent full HTTP public-interface checks; launches a diagnostic owned CLI,
# consumes READY/token only in memory, and performs GET requests only.
/usr/bin/python3 tools/startup-bench/api-diagnostic.py \
  --cli bazel-bin/gpui/durian-cli --corpus /tmp/startup-corpora/100k \
  --work /tmp/startup-runtime/api --output /tmp/api.json
```

For sanitized HTTP observation, run `sudo /usr/bin/python3 tools/startup-bench/capture.py --seconds 20 --output /tmp/native-http.json`, wait for its ready line, and launch a separate native `run.py --calibrate-only` boot. Observer needs Linux CAP_NET_RAW and CAP_NET_ADMIN for its own larger receive buffer; it changes no global sysctl. Check packet_stats.dropped=0 and unanswered_counts empty. Do not treat a lost/unanswered response as an HTTP failure.

Evidence: `10k/` and `100k/` contain report.json, per-run result.json, timestamped logs/traces, first-frame/ready PNGs and OCR TSV; `diagnostics/*-verified/` has sanitized native HTTP observations and traces; `correctness/` has original crash, CLI/API before/after, seeder validation and exact imported source hashes. Large database binaries are intentionally not exported; the seeder reproduces them.
