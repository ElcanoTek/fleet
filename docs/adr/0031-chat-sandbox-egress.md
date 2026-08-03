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

**The approved-bash path is the third take and was initially missed.** An
approval is a deferred tool call from a conversation, executed out-of-band by
`runStagedBash` rather than by the turn loop, so it acquires its own sandbox
via `takeStagedBashSandbox`. That function honored the *per-conversation*
lockdown seal (#562) but not `FLEET_DEFAULT_NETWORK_MODE`, so this ADR's
"fleet-wide" claim was false for it: under `lockdown` an approved command from
a non-lockdown conversation still ran with open slirp4netns egress, and under
`allowlisted` it bypassed the proxy entirely — reachable by staging a risky
command (the interactive gates stage `git push`, package installs, …) and
having a human click Approve. It now mirrors `takeTurnSandboxFrom` exactly,
including the `ErrContainerUnavailable` degrade to the host take, while
keeping the per-conversation seal's stricter no-degrade rule. With that, all
three sandbox takes — interactive turn, scheduled task, approved bash — honor
the setting.

## Consequences

- `FLEET_DEFAULT_NETWORK_MODE` genuinely applies fleet-wide — the ADR-0012
  boot logs no longer say "chat turns are unaffected".
- Operators who set `allowlisted`/`lockdown` **and** use the persistent REPL
  lose kernel persistence on chat turns (documented in the code + PII/deployment
  notes) — the safe direction, and only when a containing mode is active.
- No change to `open`-mode deployments (the default): byte-for-byte the prior
  behavior.
- Does not weaken ADR-0002 (the sandbox is still mandatory) or ADR-0012
  (allowlisted remains strictly more restrictive than open); it widens
  ADR-0012's coverage to the chat path it deferred.
