# In-housed dependencies

fleet used seven third-party packages for small, stable slices of what they
offer. Each is now either copied and trimmed into the tree with its licence, or
replaced by fleet's own code, and the dependency is gone from `go.mod` or
`web/package.json`. This page records what moved where, what was kept
byte-for-byte, what changed on purpose, and how each move is pinned by tests.
The policy behind it is in [`AGENTS.md`](../AGENTS.md) ("Dependencies and
in-housed code").

Why: every dependency is code we run but do not read, an update stream to
triage, and a CVE surface to assess. These seven gave little in return —
one or two call sites each — and two of them (golang-migrate's pgx driver and
react-syntax-highlighter) pulled in far larger trees than the slice used. After
the move, `go mod tidy` also drops `lib/pq` (GO-2026-6166, GO-2026-6168 to
6173, no upstream fix), `jackc/pgerrcode`, `golang-jwt/jwt`, `goquery`,
`cascadia` and `lumberjack`; `npm` drops refractor, prismjs, highlight.js,
lowlight and their dependencies.

## What moved

| Was | Now | How | Licence |
|-----|-----|-----|---------|
| `github.com/robfig/cron/v3` v3.0.1 | `internal/cron` | parser + spec copied, trimmed (no runner); `SpecSchedule.Next` is fleet's DST-safe walk | MIT, `internal/cron/LICENSE` |
| `github.com/golang-migrate/migrate/v4` v4.20.1 | `internal/sched/db/migrate.go` | re-implemented (Up, Force); a few lines adapted | MIT, `internal/sched/db/LICENSE.golang-migrate` |
| `github.com/SherClockHolmes/webpush-go` v1.4.0 | `internal/webpush/protocol.go` | re-implemented on the standard library | — (own code) |
| `github.com/JohannesKaufmann/html-to-markdown` v1.6.0 | `internal/tools/htmlmd` | CommonMark rules ported from goquery to `x/net/html` | MIT, `internal/tools/htmlmd/LICENSE` |
| `gopkg.in/natefinch/lumberjack.v2` v2.2.1 | `internal/logging/rotate` | copied, trimmed | MIT, `internal/logging/rotate/LICENSE` |
| `github.com/go-chi/chi/v5/middleware` (chi v5.4.0 for ClientIPFromXFF/GetClientIP; v5.3.2 for Recoverer, WrapResponseWriter) | `internal/httpmw` | ClientIPFromXFF/GetClientIP, Recoverer, WrapResponseWriter copied; chi stays as the router | MIT, `internal/httpmw/LICENSE` |
| `react-syntax-highlighter` 16.1.2 (+ refractor 5.0.0, prismjs 1.30.0) | `web/src/app/chat/ui/highlight/` | Prism tokenizer + 5 grammars vendored; renderer ported | MIT, three `LICENSE.*` files there |

Every copied or adapted file starts with a header naming the package,
version, upstream path, that it was modified, and its SPDX id.

## Per package

### cron (`internal/cron`)

- **Kept:** `ParseStandard` and the configurable `Parser`, descriptors
  (`@daily`, `@every 90s`, …), `CRON_TZ=` / `TZ=` prefixes, field semantics,
  error texts. Upstream's parser, spec and constant-delay tests are ported
  unchanged and pass.
- **Changed:** `SpecSchedule.Next` is the zone-period walk that used to live
  in `internal/cronnext` (robfig's wall-clock walk survives only as the
  in-UTC step and as the tests' reference), so there is no library `Next` to
  call by mistake. That mistake was live: `internal/croncount` (the approval
  card's runs-per-month hint) called robfig's `Next`, which skips a day across
  Australia/Lord_Howe's 30-minute offset changes for a `CRON_TZ=` schedule;
  `TestEstimateRunsAcrossNonHourDSTChange` pins it. `CRON_TZ=<zone>` with no
  expression after it used to panic (`spec[eq+1:-1]`); it is now a parse
  error.

### sched migrations (`internal/sched/db/migrate.go`)

- **Kept, byte-compatible:** the `schema_migrations` table
  (`version bigint primary key, dirty boolean`, one row, created if missing);
  the advisory-lock key (`GenerateAdvisoryLockId`: CRC-32 of schema, table and
  database joined by NUL, times 1486364155 — pinned against values the library
  computed); the dirty → run → clean sequence with each `.up.sql` run as one
  multi-statement `Exec` (one implicit transaction, as the pgx driver did); the
  `Dirty database version N. Fix and force version.` text; refusing a
  database whose version this build does not ship. A database migrated by the
  library and one migrated from empty by the new runner dump to the same
  schema, and the library's state continues cleanly (tested against a real
  Postgres).
- **Changed:** the lock is taken by polling `pg_try_advisory_lock` within the
  same 15 s, so a cancelled wait cannot leave a granted lock on a pooled
  connection; the dedicated connection is released after the run (the library
  held it for the life of the process); a failure names the migration file and
  the Postgres message instead of echoing the whole SQL.
- **Operators:** the dirty-row repair was the external `migrate force` CLI. It
  is now `fleet migrate force <version>` ([`MIGRATIONS.md`](MIGRATIONS.md)).
  The status report's `runner` field still reads `golang-migrate`: it names the
  tracking format, and consumers already key on it.

### Web Push (`internal/webpush/protocol.go`)

- **Kept:** the VAPID key format (base64url scalar and uncompressed point,
  padded or not — existing `FLEET_VAPID_*` keep working), subscription key
  decoding (standard or URL base64), one full 4096-octet record per message,
  the request headers, and the VAPID JWT header and claims byte for byte
  (checked against the library before removal). The endpoint allowlist and the
  no-URLs-in-logs rule (#1773) are untouched.
- **How:** `crypto/ecdh`, `crypto/hkdf`, AES-128-GCM and `crypto/ecdsa`; no
  deprecated `crypto/elliptic` arithmetic, no JWT library.
- **Tests:** RFC 8291 §5 / Appendix A's worked example encrypts to the RFC's
  exact body; a real send decrypts as the browser would and its signature
  verifies against `k=`.

### HTML → Markdown (`internal/tools/htmlmd`)

- **Kept:** the output, quirks included — it is the text models have been
  reading from `web_fetch`. 38 golden pages (the library's own test inputs plus
  fleet edge cases) were generated with the library before it was removed; a
  throwaway differential over 200,000 random documents found no difference.
- **Dropped:** plugins, Keep/Remove, referenced links, other option values,
  URL fetching.

### Rotating log file (`internal/logging/rotate`)

- **Dropped:** `LocalTime`, the JSON/YAML tags, the `os.TempDir` default name.
- **Changed:** `Close` stops the compress/cleanup goroutine and waits for a run
  in progress (upstream leaked one per Logger); a later `Write` reopens. The
  ported tests wait for that goroutine instead of sleeping, so they pass under
  `-race`.

### chi middleware (`internal/httpmw`)

- **Kept:** `ClientIPFromXFF` / `GetClientIP` (with upstream's XFF advisory
  regression tests), `NewWrapResponseWriter`, and Recoverer's recover /
  re-panic `http.ErrAbortHandler` / 500 logic.
- **Versions:** `client_ip.go` is re-synced to v5.4.0, which fixed
  GHSA-j6qf-3m9c-vwj3 (`ClientIPFromXFF` now treats `r.RemoteAddr` as the
  rightmost hop and requires at least one trusted prefix). Recoverer and
  WrapResponseWriter remain v5.3.2 copies: v5.4.0 changed `wrap_writer.go`
  (Flush commits the status through a shared `basicWriter.flush`) and only
  spelled `interface{}` as `any` in `recoverer.go`.
- **Dropped:** upstream's `ClientIPFromHeader`, `ClientIPFromXFFTrustedProxies`
  and `ClientIPFromRemoteAddr`, and their tests (plus the halves of advisory
  tests that exercise them).
- **Changed:** Recoverer prints `panic: <value>` and Go's stack, not chi's
  coloured pretty-stack. `ClientIPFromXFF`'s per-request walk is lifted out of
  the handler into `xffClientIP` (same code, same semantics) so fleet's own
  `trusted_proxies.go` — the one client-IP resolver both the orchestrator API
  and the chat server's IP filter use — runs that exact walk.

### Syntax highlighting (`web/src/app/chat/ui/highlight/`)

- **Kept:** the rendered HTML. 52 renders (13 snippets × four layouts) are
  pinned byte for byte against the library's output; a throwaway differential
  over 6,000 random snippets found no difference.
- **Dropped:** every grammar but python, bash/shell, json, yaml and markup;
  markup's `addInlined` / `addAttribute` helpers. An entity's tooltip decodes
  numeric references and the five XML entities only (upstream decoded every
  named entity).

## Deliberately not in-housed

glamour, sentry-go, a2a-go, acp-go-sdk, TanStack Virtual, `google/uuid`, the
Kubernetes client, fantasy and OpenTelemetry stay dependencies: each is large,
protocol-defining or actively maintained enough that owning a copy would cost
more than tracking it.
