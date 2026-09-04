# ADR-0012: One `fleet` binary — `serve` plus the operator CLI (back-compat preserved)

- **Status:** Accepted; the shim removed by ADR-0060 (trigger re-anchored by ADR-0059)
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
- `cmd/fleet-admin` is reduced to a **deprecation shim**: it prints a one-line
  notice and forwards to the same `admincli.Run`, so existing scripts and the
  in-place upgrade path keep working.

  **Amended 2026-08-22 (enterprise security audit).** This originally said "for
  ONE release ... removed next release". That clock never started: `git tag`
  returns nothing, `VERSION` is `0.0.0`, and `CHANGELOG.md` has only an
  `[Unreleased]` heading — there has never been a release, so "next release" is
  not a date and "one release" is not a window. The shim also turns out to be
  load-bearing rather than vestigial: `Makefile` (`bins`, `install`),
  `scripts/bootstrap.sh`, `scripts/update.sh` and `scripts/fleet-upgrade.sh` all
  build or install it, the last two *hard-fail* if the binary is missing, and
  `internal/admincli/scripts_dryrun_test.go` asserts the "would install fleet +
  fleet-admin" string. So removal is a coordinated change across four scripts and
  two test assertions, not a deletion.

  The concrete trigger, replacing the unanchored one: **the shim is removed in
  the first release after 1.0.0.** Until then it stays, and it is 20 lines that
  fork no logic — it shares `internal/admincli.Run` with `fleet`, so it adds no
  second governance path.

  **Discharged 2026-09-04 by [ADR-0060](0060-remove-the-fleet-admin-shim.md).**
  The shim is gone: `cmd/fleet-admin` is deleted, the build emits one binary, and
  `fleet update` / `fleet doctor` delete a copy already installed on a box —
  the step every version of this checklist missed, and without which "removed"
  would have meant "removed from the repo, still on your PATH running old code".
  `fleet <verb>` is the only operator CLI. Everything below is the record of how
  the window was reasoned about; the window itself no longer exists.

  **Re-anchored 2026-09-04 by [ADR-0059](0059-date-based-rolling-releases.md).**
  The 2026-08-22 amendment above diagnosed the problem correctly and then picked
  a trigger with the same defect. There is no 1.0.0 and there will not be one:
  releases are now date-based (`vYYYY.MM.DD.N`) and tagged automatically on every
  green push to `main`, so "the first release after 1.0.0" is a condition that
  can never be met — the second unanchored clock in a row. **The shim is removed
  in the first release on or after 2026-12-01**, which is a real date a real tag
  passes. Everything else above stands: removal is still the coordinated change
  across four scripts and two test assertions that the amendment describes, and
  `scripts/check_release_version_test.go` now asserts every file carrying this
  window states the dated form, so the eight restatements cannot drift apart
  again.

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
  <verb>`) still works, with a deprecation warning, until the removal trigger
  above.
- The daemon artifact stays named `fleet`, so the highest-blast-radius references
  (systemd unit + bootstrap on a *running* box) barely move.
- Two binaries still build (the shim), so the existing build/upgrade scripts
  that expect both `fleet` and `fleet-admin` are unchanged.
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
