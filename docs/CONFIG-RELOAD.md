# Config hot-reload (#286)

A running fleet can re-read a **small, explicitly safe subset** of its settings
without a restart, so an operator can adjust a cost/token/iteration ceiling or
the sampling temperature without interrupting active chat conversations or
in-flight scheduled tasks.

Everything else — bind addresses, database DSNs, auth secrets, the
admission-semaphore size, TLS — is bound into a listener, connection pool, or
signing context **at startup** and cannot be swapped mid-run. A reload reports
any attempt to change one of those rather than silently ignoring it.

## How to trigger a reload

Two mechanisms, both equivalent:

1. **Signal** — `kill -USR2 <fleet-pid>`. (We use `SIGUSR2`, not `SIGHUP`, so it
   never clashes with log-rotation tooling that conventionally sends `SIGHUP`.)
   The outcome is logged.
2. **HTTP** — `POST /admin/reload-config` (admin-API-key gated). Returns a JSON
   diff:

   ```json
   {
     "reloaded_at": "2026-06-30T08:00:00Z",
     "changed": [
       { "key": "FLEET_MAX_COST_USD", "old": "50", "new": "25" }
     ],
     "skipped": [],
     "errors": []
   }
   ```

Both re-read values from the env file (`FLEET_ENV_FILE`) and the process
environment. To change a value, edit the env file and trigger a reload.

## Precedence

Reload reproduces boot's **process-env-over-file** precedence for values that are
set: a value the operator pinned in the **process environment** at startup (e.g.
systemd `Environment=`) still wins over the env file. The env file drives
everything else. So in practice you change a reloadable setting by editing the
env file — unless it was pinned in the process environment at boot, in which case
it is fixed until restart (and a reload attempt to change it is reported under
`skipped`).

Two behaviors deliberately **differ from a fresh boot**, both in the safe
direction (a reload never silently snaps a running value to a default):

- A reloadable var that is **unset / removed** from the file keeps the current
  running value (it does NOT revert to the built-in default a fresh boot would
  use) — see below.
- A value that **fails to parse or validate** keeps the current value (a fresh
  boot would fall back to the default) — see below.

A value that fails to parse or falls outside its allowed range is reported under
`errors` and the **previous value is kept** — a bad value never poisons a running
process.

A reloadable var that is **unset** (or removed from the file) is treated as "no
change": the current running value is kept, not reverted to a built-in default.
So a reload only ever raises or lowers a value you explicitly set; deleting a
line never snaps a ceiling back to its default mid-run.

## Reloadable settings

These are read per-turn / per-task from the shared config, so a new value takes
effect on the next turn or task:

| Env var (FLEET_ / CHAT_ / CUTLASS_) | Field | Bound |
|---|---|---|
| `FLEET_MAX_COST_USD` | per-run cost ceiling (USD) | `>= 0` |
| `FLEET_MAX_TOTAL_TOKENS` | per-run token ceiling | `>= 0` |
| `FLEET_MAX_ITERATIONS` | per-turn iteration ceiling | `1`–`10000` |
| `FLEET_TEMPERATURE` | interactive sampling temperature | `>= 0` |
| `CUTLASS_TEMPERATURE` | scheduled-task sampling temperature | `>= 0` |

## Non-reloadable settings (require a restart)

These are bound at startup and cannot change without a restart. A change to one
is reported under `skipped` (with a reason) so you know a restart is needed. The
list below is the curated set the reload explicitly watches; other startup-only
settings (e.g. `DATA_DIR`, the sandbox image, listener TLS files) likewise
require a restart even though they are not individually echoed back.

| Env var | Why a restart is required |
|---|---|
| `FLEET_SERVER_ADDR` | the TCP listener is bound at startup |
| `DATABASE_URL` / `DB_*` | the Postgres connection pool is opened once at startup |
| `FLEET_SERVER_TOKEN` | shared-secret auth — rotating it mid-run would invalidate in-flight sessions |
| `ADMIN_API_KEY` | admin auth secret |
| `FLEET_MAX_CONCURRENT_AGENTS` | sizes the admission semaphore + sandbox warm pool at startup |
| `FLEET_TLS_MODE` | the TLS listener context is built at startup |

## What is intentionally NOT reloadable here

- **Auth secrets and DB DSNs** are deliberately excluded — hot-swapping a signing
  secret or a connection pool mid-run is unsafe, and these are reported under
  `skipped` so the operator gets clear feedback.
- **`FLEET_LOG_LEVEL` / sampling intervals** are not reloadable: fleet has no live
  log-level config field today (log verbosity is consumed ad-hoc), so a reload
  knob with no sink would be dishonest. Reloadable log-level is a follow-on once
  the slog migration (#178) lands a real level sink.
- **`.env` file watching (`FLEET_ENV_FILE_WATCH`)** — auto-reloading on a file
  write — is a follow-on. It would add a filesystem-watch dependency; the two
  triggers above (signal + HTTP) fully deliver hot-reload and integrate cleanly
  with a config-management tool or a deploy hook.
