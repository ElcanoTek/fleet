# ADR-0012: One `fleet` binary — `serve` plus the operator CLI (back-compat preserved)

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

fleet shipped two binaries: `fleet` (the long-running server — systemd's
`ExecStart=/usr/local/bin/fleet`) and `fleet-admin` (the operator CLI:
bootstrap/update/status/diagnose/restart/stop/logs + chat-user/sched-user/apikey/
notes/task/mcp/backup verbs). Operators found this confusing ("`fleet-admin
update` isn't installed / doesn't work as advertised", #461) and asked for a
single `fleet` command. But `fleet` was already the server's name, so a naive
rename collides with the daemon, and changing the daemon's invocation out from
under a running systemd unit risks bricking a box on the next restart.

## Decision

There is **one `fleet` binary** (`cmd/fleet`) with subcommand dispatch
(`internal/admincli` holds the operator verbs):

- `fleet serve` runs the server. **Bare `fleet` (no subcommand) also runs the
  server** — this back-compat is load-bearing: a historical unit with
  `ExecStart=/usr/local/bin/fleet` keeps starting the daemon, so a unit file can
  migrate to `fleet serve` on its own schedule and **no restart mid-upgrade can
  ever brick the box** (the binary understands both forms).
- Server-family verbs handled in `cmd/fleet`: `version`, `mcp-broker`,
  `validate-config`.
- Every other verb (`update`, `status`, `bootstrap`, `chat`, `sched`, `task`,
  `mcp`, `notes`, `worktree`, `backup`, `restore`, `motd`, …) routes to
  `internal/admincli.Run`.
- `cmd/fleet-admin` is reduced to a **deprecation shim** for ONE release: it
  prints a one-line notice and forwards to the same `admincli.Run`, so existing
  scripts and the in-place upgrade path keep working. It is removed next release.

`make install` puts `fleet` (and the shim) on `PATH` — the actual fix for "isn't
installed" on a dev box. The systemd unit is **not** force-migrated to
`fleet serve`; bare `fleet` serving means it can stay as-is.

## Enforcement

- `cmd/fleet/classifyInvocation` is the single routing decision; `cmd/fleet/route_test.go`
  (`TestClassifyInvocation`) asserts bare `fleet` AND `fleet serve` BOTH classify
  as `invokeServe`, locking the no-brick invariant, and that admin verbs route to
  `admincli`.
- `internal/admincli` is the one operator dispatch both `fleet <verb>` and the
  `fleet-admin` shim call; `internal/admincli/service_test.go` asserts the verbs
  are wired.

## Consequences

- Operators get the unified `fleet` they asked for; muscle memory (`fleet-admin
  <verb>`) still works for one release with a deprecation warning.
- The daemon artifact stays named `fleet`, so the highest-blast-radius references
  (systemd unit + bootstrap on a *running* box) barely move.
- Two binaries still build for one release (the shim), so the existing
  build/upgrade scripts that expect both `fleet` and `fleet-admin` are unchanged.
- A future release deletes the shim and may flip bare `fleet` to print help
  (requiring explicit `serve`); by then every deployed unit says `fleet serve`.

## Alternatives considered

- **CLI = `fleet`, daemon renamed to `fleetd`.** Rejected: strictly more ripple
  (renames the *server* artifact across systemd/bootstrap/update/CI/docs AND
  still builds two binaries) for no benefit the subcommand split doesn't give.
- **Make bare `fleet` print help immediately (require `fleet serve`).** Rejected
  for this release: a restart landing between the binary swap and the unit-file
  update would brick the daemon. The bare-`fleet`-serves bridge eliminates that
  window.
- **GitHub-release binary self-update as the primary `fleet update`.** Rejected:
  the canonical deployment is a git checkout + systemd, and the established
  upgrade path also rebuilds the Next web app + sandbox image and gates on
  `/readyz` — a binary-only swap would be a weaker, parallel path (and signing
  infra / RCE-on-update surface). `fleet update` keeps wrapping the script;
  `fleet update --check` adds a read-only "commits behind" report with no new
  infrastructure.
