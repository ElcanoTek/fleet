# ADR-0060: Remove the `fleet-admin` shim, and evict it from the boxes that have it

- **Status:** Accepted
- **Date:** 2026-09-04
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0012](0012-unified-fleet-cli.md) (completes its deprecation)

## Context

[ADR-0012](0012-unified-fleet-cli.md) unified the two binaries into one `fleet`
CLI (#461) and reduced `cmd/fleet-admin` to a 24-line shim: print a deprecation
notice, forward `os.Args[1:]` to the same `admincli.Run` the `fleet` binary
calls. It was meant to be temporary.

Its removal trigger never worked. The original said "for ONE release … removed
next release" — a clock that never started, because fleet had never cut a
release. The 2026-08-22 amendment diagnosed that correctly and then chose "the
first release after 1.0.0", which had the same defect for the same reason.
[ADR-0059](0059-date-based-rolling-releases.md) re-anchored it to a date
(2026-12-01) and made the eight restatements agree, but the shim was still
sitting there, and the reasons to keep waiting had run out:

- **Nothing depends on it.** No systemd unit, timer, or Helm template references
  it — every shipped unit's `ExecStart` names `fleet` or `fleet-web-start.sh`.
  Nothing switches on `argv[0]`: the shim is not a mode of the binary, it is a
  second binary that calls the same function.
- **It is not equivalent to `fleet`.** `fleet task run` is dispatched by
  `cmd/fleet` *before* `admincli`, so the shim could never run it and errored
  with an explanation instead (#722). "Still works" was already qualified.
- **Keeping it cost more than it saved.** It appeared in four scripts, two test
  assertions, and ~40 doc and comment references — including two hard-fail
  guards (`scripts/update.sh`, `scripts/fleet-upgrade.sh`) and one silent-skip
  guard in `scripts/bootstrap.sh` that would have skipped installing `fleet`
  itself if the shim were ever missing.
- **The deprecation warning was doing no work.** It printed on every invocation
  and pointed at a spelling that had worked since #461.

One thing the deprecation checklists had all missed: **deleting the shim from
the repo does not remove it from a box.** `bootstrap` installed a copy at
`$INSTALL_DIR/fleet-admin` and symlinked `/usr/local/bin/fleet-admin` at it;
`update` and `fleet-upgrade` reinstalled it every time. Nothing ever deleted
either. Left alone, an existing box keeps a fully functional `fleet-admin` on
`PATH` that is never rebuilt again — so it dispatches operator verbs against
today's databases using whatever `admincli` was compiled the day it was last
installed, silently and with no warning that it is unsupported. A stale operator
CLI that quietly does the wrong thing is a worse outcome than a missing one.

## Decision

**The `fleet-admin` shim is removed now, and the removal reaches installed
boxes.**

1. **`cmd/fleet-admin/` is deleted.** `fleet <verb>` is the only operator CLI.
   `internal/admincli` is untouched — it is `fleet`'s own dispatch and always
   was; only the second `main` that called it is gone.
2. **The build and install path emits one binary.** `make build` / `make bins` /
   `make install` produce and install `fleet`. The two hard-fail guards and the
   silent-skip guard that required a second artifact are gone with it.
3. **`fleet update` and `fleet doctor` delete a leftover shim** from
   `$INSTALL_DIR/fleet-admin` and `/usr/local/bin/fleet-admin`, reporting what
   they removed. The test for existence is `-e || -L`, because removing the
   target first leaves the bootstrap symlink dangling — for which `-e` is false,
   and a broken link on `PATH` is the one outcome worse than the stale binary.
   Failure to remove warns; it never fails an update. `doctor --check` reports
   without removing, like every other artifact check.
4. **ADR-0012's dated removal window is discharged**, not merely edited: the
   sentence is deleted from all eight operator-facing files, because there is
   nothing left to remove.

## Enforcement

- `scripts/check_release_version_test.go` — `TestTheFleetAdminShimIsGone`
  asserts `cmd/fleet-admin` stays deleted, that the Makefile does not build or
  install it again, and that **both** convergence paths (`scripts/update.sh`,
  `scripts/doctor.sh`) still evict an installed copy. The eviction is the half a
  future cleanup would most plausibly delete as dead code.
- `scripts/check_release_version_test.go` —
  `TestDeprecationWindowsAreNotKeyedToAReleaseNumber` keeps the lesson: no
  operator-facing file may key a window to a release number again.
- `internal/admincli/scripts_dryrun_test.go` — the bootstrap and update dry-run
  plans assert the one-binary install, and the update plan asserts the eviction
  step is in the plan.

## Consequences

**Easier.** One binary to build, install, back up, roll back, and document. The
scripts lose two hard-fail guards and a silent-skip guard whose failure mode was
"bootstrap reports success having installed nothing". Every doc and comment names
one spelling.

**Harder / accepted costs.**

- **`fleet-admin <verb>` stops working.** That is the point, and it is a breaking
  change for any operator script still using it — muscle memory, a cron entry, a
  runbook. The fix is mechanical (`fleet <verb>`, same flags, same behaviour) and
  the spelling has been supported since #461, warned about on every invocation
  since, and documented as deprecated throughout. No shipped unit or timer uses
  it, so nothing on the box breaks on its own; a person or a script has to type
  it.
- **The eviction runs without asking.** `fleet update` deletes a binary the
  operator did not ask it to delete. Accepted, and narrowly scoped: it removes
  only the two paths fleet's own bootstrap created, only on a box already being
  converged, and it says so in the output. Leaving a never-rebuilt operator CLI
  on `PATH` is the larger harm.
- **A rollback needs the checkout, not the binaries.** `fleet-upgrade.sh` no
  longer backs up or restores a second binary. `git checkout <sha> && fleet
  update --no-pull` is the documented path back and is unaffected.

## Alternatives considered

- **Keep waiting for 2026-12-01** (ADR-0059's dated window). Honest, and the
  reason to reject it is that the window's purpose — giving users time to
  migrate — was already served: the spelling changed in #461, the warning has
  printed on every invocation since, and nothing automated depends on it. Three
  more months of carrying it through every script, test, and doc buys nobody
  anything.
- **Delete the package, leave installed copies alone.** The cheap version, and
  the one every previous checklist implied. Rejected: it converts "deprecated
  command that warns" into "unsupported command that silently runs old code",
  which is strictly worse than both keeping it and removing it properly.
- **Replace the shim with a stub that only errors.** Keeps a second binary,
  a second `main`, and a second thing to build and install, to deliver a message
  the shell already delivers ("command not found") — and it would still need the
  eviction path to replace the copies already installed.
- **Have the eviction only warn, never delete.** Considered for the surprise
  factor. Rejected: a warning during `fleet update` is exactly the output an
  operator scrolls past, and the stale binary would survive indefinitely on the
  boxes least likely to be watched. `doctor --check` remains the read-only view
  for anyone who wants to look first.
