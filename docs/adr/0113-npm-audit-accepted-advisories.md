# ADR-0113: A reviewed, expiring register of accepted npm advisories

- **Status:** Accepted
- **Date:** 2026-11-17
- **Deciders:** fleet maintainers

## Context

The web tier's CVE gate is `npm audit` over `web/package-lock.json`, failing
on any advisory of any severity ([SCANNING.md](../SCANNING.md)). It had no
accepted-advisory register on purpose. The last time an advisory arrived with
no patched release (adm-zip GHSA-vwc7-r8mq-g2x9, in the reference Rampart
service), the dependency was removed instead
([ADR-0063](0063-remove-the-rampart-pii-engine.md)).

On 2026-11-17 GHSA-fc5q-39xc-vjpf (picomatch ReDoS through single-branch
repeated extglobs, high, CVSS 7.5) began covering `picomatch <=4.0.7`. 4.0.7
is the latest release and upstream has no fix. Every open PR turned red.
Removal is not available this time:

- picomatch arrives through `vite`, `vitest` and `tinyglobby`/`fdir`.
- Every release of `vite` and `vitest` depends on picomatch directly, and
  the Elcano frontend standard requires Vite (Tier B, stack-check).
- `npm audit fix --force` proposes only a vitest downgrade that still
  resolves picomatch 4.0.7.

The finding is not reachable from anything we ship:

- picomatch is a dev dependency in the lockfile.
- It is not in the browser bundle or the Go binary.
- It only compiles glob patterns written in our own build and test config.

A ReDoS needs an attacker-chosen pattern, and no request, user input or
network data ever becomes one.

The options were:

1. **Leave the gate red until upstream ships a fix.** Every PR is blocked
   for an unbounded time on a finding that cannot be exploited here.
2. **Fork picomatch in-house with our own fix.** This makes the package local,
   so `npm audit` stops seeing it at all. Future picomatch advisories would
   then be invisible, which is a worse security posture than the waiver it
   replaces.
3. **Drop the gate to `npm audit --omit=dev` or a high-only level.** This
   blinds the gate to every future dev-tree or low-severity advisory, not just
   this one.
4. **A register that accepts one advisory, on one package, with conditions
   the gate checks.** This is the model `.github/codeql-accepted-findings.json`
   already uses for CodeQL ([ADR-0048](0048-codeql-severity-gating.md)).

## Decision

**Option 4.** CI's audit step (and `make ci-web`) runs
`web/tools/npm-audit-gate.cjs` instead of `npm audit --audit-level=low`. The
script runs `npm audit --json --package-lock-only` and fails on every
advisory, of any severity, except one accepted by
`web/npm-audit-accepted.json`. An entry is honoured only while **all** of
these hold, and the script checks each one:

- It names the advisory (GHSA id) and the package the advisory is filed
  against.
- It carries a written reason saying why the code cannot be exploited here.
- It has not expired. An expired entry fails CI, so the decision is made
  again. Expiries are short: the first entry expires on 2026-12-31.
- Every copy of the package in `package-lock.json` is a dev dependency. A
  package that would ship can never be accepted, whatever the reason says.
- The advisory is still reported. An entry that matches nothing fails CI, so
  the PR that bumps the package past the advisory must also remove the entry.

The register's first and only entry is GHSA-fc5q-39xc-vjpf on picomatch.

**Update 2026-11-18.** GitHub withdrew GHSA-fc5q-39xc-vjpf that day, so
`npm audit` stopped reporting it and the stale-entry check failed CI. The
entry was removed and the register is empty; the register and its checks stay
for the next advisory of this kind.

## Consequences

- PRs are unblocked. Any other advisory, including a new one on picomatch or
  one on a production dependency, still fails CI exactly as before.
- Widening the register is a reviewed security decision in a PR diff, like
  the CodeQL register. "Fix the dependency" remains the first answer:
  SCANNING.md and the register's own note say so.
- The gate depends on `npm audit`'s JSON output (`vulnerabilities[*].via[*]`
  with `url`, `name`, `severity`, `range`). If npm changes that format, the
  script fails loudly rather than passing.
- When picomatch publishes a fixed release, its lockfile bump goes in its own
  PR (AGENTS.md: a CVE bump is its own change) and removes the entry. The
  stale-entry check enforces that.
