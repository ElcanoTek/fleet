# Test databases: a cloned database per test

Issue #2049, milestone M2. Builds on phase 1 ([ONE-DATABASE.md](ONE-DATABASE.md),
ADR-0098): both stores in one database, schemas `chat` and `sched`.

Before M2 the Postgres-backed tests shared databases — one for the chat store,
one for the scheduler, a third for `internal/httpapi` in CI — and paid for it
three ways: `scripts/go-test.sh` sorted packages into DSN groups and ran each
with `-p 1`; every chat fixture `TRUNCATE`d ~80 tables through a helper that
needed an advisory lock, `LOCK … NOWAIT` escalation and 14 retries to survive
goroutines leaking into the next test; and every sched helper serialized on
`pg_advisory_lock(1)` and `DELETE FROM`'d its tables before and after. Within
a package nothing could run in parallel.

Now every DB test gets **a database of its own**, cloned from a migrated
template and dropped when the test ends, and the whole tree runs in one
`go test`.

## What shipped

**`internal/testdb`** (imports neither store):

- `New(t, Spec)` clones the template a `Spec` describes (`Name`,
  `Fingerprint`, `Migrate`) — `CREATE DATABASE fleet_t_<owner>_<n> TEMPLATE
  fleet_tmpl_<name>_<sha12>` — and drops it `WITH (FORCE)` at cleanup, so a
  goroutine that outlived its test cannot keep it alive.
- `Empty(t)`, `LegacyPair(t)` (two empty databases — the legacy two-database
  layout's stores, `LEGACY-DB-LAYOUT`), `Reserve(t)` (a name the code under
  test creates; `fleet db consolidate`'s target), `MaintenanceDSN(t)`.
- **One knob:** `FLEET_TEST_DATABASE_URL`, redefined as the maintenance DSN —
  any database on a server whose role can `CREATE DATABASE`; testdb never
  writes to it. Unset: skip (#2019's rule, unchanged). Set: every connect,
  privilege, clone or migration error fails, and a role without `CREATEDB`
  fails with `ALTER ROLE … CREATEDB`. `FLEET_TEST_DB_STRATEGY` (`wal_log` /
  `file_copy`) overrides `CREATE DATABASE`'s strategy; the default
  (`WAL_LOG`) measured faster (below).
- **Templates** are keyed by a hash of the embedded migration files
  (`store.MigrationFingerprint`, `scheddb.MigrationFingerprint`, both
  `dbschema.Fingerprint`) plus a recipe version. A template is built once, under
  an advisory lock in the maintenance database: migrate a
  `fleet_tmpl_build_<key>_<owner>` database, disallow connections, rename it,
  mark it a template. A per-process map skips the lock afterwards. Worktrees
  on different migration sets get different templates, so they share a server
  safely; the newest three of each kind are kept.
- **Garbage collection.** Each test process holds an owner lock (a session
  advisory lock in the maintenance database) whose random id is part of every
  database it creates. At startup, under the build lock, a process drops
  every `fleet_t_*` / `fleet_tmpl_build_*` database whose owner holds no lock —
  read from `pg_locks`, which spans the whole server — so a killed run is
  cleaned up by the next one and a live run (even one using another
  maintenance database) is never touched. Nothing outside those prefixes is
  ever dropped. `scripts/test-db-setup.sh --gc` does the same by hand
  (`--templates` drops the templates too).
- **`testdb.Run`** (from a package's `TestMain`): background mode. One filler
  per template keeps clones ready (up to `GOMAXPROCS`, never more than the
  package has asked for), finished tests' databases are dropped by background
  droppers behind a short queue (the backpressure that bounds disk use), and
  `Run` returns only after every drop — so a finished run leaves no `fleet_t_`
  database. Every DB package has the `TestMain`; a scripts check keeps it so.

**`internal/testdb/fleetdb`** clones the one-database template — both stores
migrated through their own runners, `chat` and `sched` schemas, the
production layout — and opens them: `fleetdb.New(t)` (`.DSN` is what
`DATABASE_URL` holds; `.ChatDSN()`, `.SchedDSN()`, `.Chat(t)`, `.SchedDB(t)`),
`fleetdb.Chat(t)`, `fleetdb.SchedDB(t)`, `DeleteRows`. Pools are small
(`testdb.PoolMaxOpen` = 4). `internal/store` and `internal/sched/db` cannot
import it, so their internal tests clone single-store templates (`chat`,
`sched`) through `testdb.New`; `fleetdb` deliberately does not import
`internal/sched/storage`, so storage's own tests can use it.

**Every helper migrated**: `newTestStore`, the httpapi fixtures, sched/db's
`setupTestDB` and `migrationSchemaDB`, the twelve `sched/handlers` setups,
`testdb.SchedDB` callers in sched/scheduler/storage/runner/admincli/cmd/fleet,
and the hand-made scratch databases (backup/restore round trips, the override
probe, the external-privilege check, `TestBothStoresInOneDatabase`, the
consolidate end-to-end tests). **Deleted:** `TruncateAllForTest` and its
retries, lock key and test; `testdb.ChatDSN` / `SchedDSN` / `SchedDB` /
`SchedLockKey`; `acquireTestLock`, `cleanDB`, `isDatabaseUnavailable` and
every per-setup `DELETE FROM`; the admincli `TestMain` that pinned its suites
to the legacy layout (`LEGACY-DB-LAYOUT`). A verb under test that resolves
the deployment's DSNs now finds its fixture through `DATABASE_URL` alone —
the one-database layout.

**`t.Parallel()`** went into every top-level DB-package test that a
conservative check allowed: no `Setenv`/`Chdir`, no write to a package-level
variable, no call to a package-global setter (`pkg.Set*`, `Configure*`,
`Install*`, `Reset*`, `Enable*`, `Disable*`, `*ForTest*`, `log.SetOutput`, …),
directly or through a package-local helper. Go runs every serial test before
the parallel ones, so a serial test's override never leaks into a parallel
one. The check was a throwaway tool, not a linter; the `-race` runs and full
runs are what proved the result.

| package | parallel / top-level tests |
|---|---|
| internal/store | 347 / 353 |
| internal/httpapi | 632 / 861 |
| internal/sched/handlers | 256 / 259 |
| internal/sched/storage | 121 / 121 |
| internal/sched/db | 93 / 94 |
| internal/runner | 92 / 94 |
| internal/sched/scheduler | 24 / 26 |
| internal/sched | 13 / 13 |
| internal/admincli | 208 / 277 |
| cmd/fleet | 111 / 156 |
| internal/dbschema | 2 / 2 |

What stays serial, and why: `t.Setenv` (httpapi 45, admincli and cmd/fleet
dozens — Go forbids it in a parallel test; a CLI verb reads the process
environment), and package-global overrides — httpapi's timing knobs
(`approvalResumeDebounce`, `directReleaseBackoff`, `sseHeartbeatInterval`, …)
and the 59 approval tests that install an `agentcore.ConfigureAgentPolicy`
(found by a full run: describer and stop tests failed when a parallel test's
policy replaced theirs). `TestApprovalResume_RunningTurnQueuesTheResume`
(18 s, serial: it shortens `approvalResumeDebounce`) is the single slowest
test on httpapi's critical path.

**Scripts and CI.** `scripts/go-test.sh` is one `go test` (`-p` and `-parallel` bounded by the server's free connections, below)
(tags, `--race`, `--count`, `--coverprofile` kept; no groups, no prebuild,
no `--print-groups`). `scripts/go_test_script_test.go` fails a test file
outside `internal/testdb` that reads `DATABASE_URL` /
`"FLEET_TEST_DATABASE_URL"`, takes `pg_advisory_lock(1)`, truncates, or uses
`HTTPAPI_TEST_DATABASE_URL`, and a DB package without `testdb.Run`. CI sets one
DSN and starts Postgres 18 by a step with `max_connections=300`,
`fsync=off`, `synchronous_commit=off`, `full_page_writes=off` (a service
container cannot pass flags); the pg_dump-18 installs stay; the race job keeps
its own server only because it is a separate runner.
`scripts/test-db-setup.sh` is a preflight (role, `CREATEDB`, the export
line) plus `--gc`.

**A CREATEDB-only role is enough** (verified against Postgres 16 with a
`CREATEDB NOSUPERUSER` role: template build, clone, write, drop; pinned by
`TestCreateDBOnlyRole`). The owner of a database may mark it a template and
close it to connections on Postgres 15+, and may clone its own database
whether or not it is marked, so the marks are best effort: a server that
refuses them (`insufficient_privilege`) still gets a working template.
Templates are keyed by the test role as well as the migrations — a clone's
objects belong to the template's owner, so another role's template would hand
a non-superuser a database it cannot write to. Garbage collection skips
another role's databases (`insufficient_privilege`) instead of failing. Tests
that create server-wide roles (the external-privilege and hardened-public
tests) skip when the test role lacks CREATEROLE, which is not part of the
contract.

**A failed background drop fails the run.** `testdb.Run` records the first
background drop that fails (the database is left behind) and returns a nonzero
code even when every test passed; synchronous mode reports the same failure as
a `t.Errorf` on the owning test.

**Connection budget.** `go test` runs `-p` binaries at once and each runs up to
`-parallel` tests; both default to GOMAXPROCS, so a many-core box could open
more connections than a default `max_connections=100` server allows.
`scripts/go-test.sh` reads the server's free connections through
`FLEET_TEST_DATABASE_URL` (`max_connections` − `superuser_reserved_connections`
− current sessions) and keeps the suite within three quarters of them,
counting 8 connections per test process (owner lock, background clone/drop)
and 3 per running DB test (the stores' pools are `testdb.PoolMaxOpen` = 4
each but rarely all busy). It lowers `-parallel` first and `-p` only when
`-parallel 1` does not fit, never leaving 1..CPUs; without `psql` or a
reachable server Go's defaults apply. Examples (pinned by
`TestGoTestScriptBoundsConnections`): CI (300 connections, 4 CPUs) keeps the
defaults, `-p 4 -parallel 4`; a default-100 server on 4 cores gets
`-p 4 -parallel 2`; on 16 cores `-p 6 -parallel 1`. It is an estimate of how
many connections a test holds, not a hard limit — a test that opens wider
pools on purpose is not accounted for.

## Measured

On the development box: 4 vCPU, shared with other agents' test runs (load
average 5–11 throughout), Postgres 16. "Private server" is a cluster with CI's
flags (`fsync=off`, `synchronous_commit=off`, `full_page_writes=off`,
`max_connections=300`); before and after ran in the same checkout (so the
build cache was shared) with a warm cache, the baseline in CI's shape (three
databases, `HTTPAPI_TEST_DATABASE_URL` set). Wall time of `scripts/go-test.sh
--count=1`.

Runs were interleaved (before, after, before, after) because the box's load
moved a lot; each cell lists every run, in order.

| run | before (4c850839) | after |
|---|---|---|
| `make test`, private server | 157 s, 148 s, 165 s (median 157) | 114 s, 126 s, 143 s, 109 s (median 120) |
| `make test -race`, private server | 276 s, 257 s | 324 s, 215 s |
| `make test`, the box's shared server (`fsync=on`), first run | 252 s | — (see below) |
| `make test -race`, shared server, first run | 563 s | — |

(A fifth "after" run, 154 s, came straight after a code change and
recompiled; it is left out of the median.)

Per package (private server, before → after): `internal/store` 39–49 s →
17–19 s; `internal/httpapi` 75–82 s → 56–59 s; `internal/sched/handlers`
10–15 s → 5–7 s; `internal/runner` 9–11 s → 3 s; `internal/admincli`
27–36 s → 21–31 s. Under `-race`: store 74–88 s → 18–25 s, httpapi
154–167 s → 91–144 s.

Honest reading:

- **`make test` is ~25 % faster** (median 157 s → 120 s), and the DB packages
  are 1.5–3× faster. The wall time is now bounded by compile plus the slowest
  packages, several of which (`internal/sandbox`, `internal/tools`) use no
  database.
- **`-race`: no clear win on this box.** Its critical path is
  `internal/agentcore` (170–240 s under `-race`, no database), which M2 does
  not touch; the DB packages finish well inside it now, but they also run at
  the same time as agentcore instead of in their own serial lane, so on a
  4-core box loaded by other runs agentcore can get less CPU (the 324 s run).
  CI runners are 4-core too: expect the race lane to be bounded by
  agentcore, not by the database — that package is the next lever.
- **Clone cost is real.** `CREATE DATABASE … TEMPLATE` of the ~11 MB
  one-database template took ~170 ms alone and ~60 ms each at 4 in parallel
  (`WAL_LOG`; `FILE_COPY` ~90 ms); a drop ~50 ms. On a server with `fsync=on`
  it is ~300 ms and a drop up to 600 ms, which is why CI turns fsync off and
  why `testdb.Run` moves both off the tests' critical path (it took the full
  run from ~165 s to ~115–125 s).
- The shared-server "after" run was abandoned: the box's disk filled (other
  agents' runs plus this run's peak), Postgres crashed mid-run and recovered.
  An earlier version of background mode with an unbounded drop queue had
  filled it too (330 databases waiting); the drop queue is now bounded at
  `GOMAXPROCS`. Peak database count per test process is about
  running tests + `GOMAXPROCS` ready + `GOMAXPROCS` queued.

## What deviated from the plan

- **`testdb.Run` and background mode** are additions: synchronous clone/drop
  per test made the serial part of httpapi slower than the old TRUNCATE.
- **GC is liveness-based** (owner locks, `pg_locks`) instead of "older than
  6 h": the next run cleans a killed run immediately, and never a live one.
- **`Reserve` and `MaintenanceDSN`** were added for the consolidate end-to-end
  and pg_dump tests; `fleetdb` has no `Storage` helper (storage's own tests
  must be able to import fleetdb), and `PoolMaxOpen` is 4, not 6, because
  dozens of processes' pools share one server.
- **`--print-groups`** is deleted, not kept for a release: nothing outside the
  script's own test used it.
- **No `tparallel` linter** in `.golangci.yml`: the rule that matters (do not
  call `t.Parallel` in a test that overrides globals) is not what it checks.

## Not done / follow-ups

- The legacy two-database layout's tests (`LegacyPair`, the consolidate
  tests) go with that layout ([ONE-DATABASE.md](ONE-DATABASE.md)).
- The serial httpapi tests that only shorten a timing knob could become
  parallel if the knob moved onto the `Server`; that is a refactor of
  production code, out of scope here.
