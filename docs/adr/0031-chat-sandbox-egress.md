# ADR-0031: Interactive chat honors the fleet-wide sandbox egress mode

- **Status:** Accepted
- **Date:** 2026-07-04
- **Deciders:** fleet maintainers
- **Extends:** [ADR-0012](0012-sandbox-egress-allowlist.md)

## Context

ADR-0012 added a fleet-wide sandbox egress posture
(`FLEET_DEFAULT_NETWORK_MODE`: `open` / `allowlisted` / `lockdown`) but wired it
into the **scheduled-task** sandbox path only. The interactive chat path
(`Manager.takeTurnSandbox`) never consulted the mode, so a non-lockdown chat
turn always got open rootless egress even when an operator set
`allowlisted` or `lockdown` — the value silently applied to half the sandboxes.
ADR-0012 flagged this as a deferred follow-up; this ADR closes it.

The gap mattered: an operator who set `lockdown` to seal the box against
exfiltration still had every chat turn's bash/python able to reach the network,
and an `allowlisted` operator's chat turns bypassed the proxy entirely.

## Decision

`takeTurnSandbox` now applies the same egress posture as `takeTaskSandbox`:

- A per-conversation lockdown **or** `FLEET_DEFAULT_NETWORK_MODE=lockdown`
  seals the turn (`--network=none`, via `TakeContainer`).
- `allowlisted` routes the turn's egress through the host proxy scoped to the
  bundle allowlist (`TakeContainerWithEgress`, fresh per-turn token,
  fail-closed).
- `open`/unset keeps the prior behavior (persistent-REPL borrow, else warm
  take).

**A containing network mode takes precedence over the persistent Python REPL
(#213).** An `allowlisted` turn needs a fresh per-turn proxy token (so it must
cold-start) and a sealed turn must have no network namespace at all — neither
of which a shared long-lived container can provide. Containment is a security
boundary; persistence is a convenience, so the boundary wins. Under `open`
mode (the default) persistence is unaffected.

The routing lives in a free function `takeTurnSandboxFrom` over a small
`turnSandboxTaker` interface, mirroring `scheduledrun.takeTaskSandbox`, so the
posture is unit-tested with a fake rather than a live podman pool.

## Consequences

- `FLEET_DEFAULT_NETWORK_MODE` now genuinely applies fleet-wide — the ADR-0012
  boot logs no longer say "chat turns are unaffected".
- Operators who set `allowlisted`/`lockdown` **and** use the persistent REPL
  lose kernel persistence on chat turns (documented in the code + PII/deployment
  notes) — the safe direction, and only when a containing mode is active.
- No change to `open`-mode deployments (the default): byte-for-byte the prior
  behavior.
- Does not weaken ADR-0002 (the sandbox is still mandatory) or ADR-0012
  (allowlisted remains strictly more restrictive than open); it widens
  ADR-0012's coverage to the chat path it deferred.
